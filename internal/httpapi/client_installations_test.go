package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"passport-backend/internal/clientenrollment"
)

type installationRegistrarStub struct {
	installation clientenrollment.Installation
	replay       bool
	err          error
	hash         [32]byte
	calls        int
	wait         bool
	deadline     bool
}

func (s *installationRegistrarStub) Register(ctx context.Context, hash [32]byte) (clientenrollment.Installation, bool, error) {
	s.hash, s.calls = hash, s.calls+1
	_, s.deadline = ctx.Deadline()
	if s.wait {
		<-ctx.Done()
		return clientenrollment.Installation{}, false, ctx.Err()
	}
	return s.installation, s.replay, s.err
}

func TestClientInstallationReturnsCreatedAndReplayWithoutAuthHeader(t *testing.T) {
	created := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	registrar := &installationRegistrarStub{installation: clientenrollment.Installation{ClientID: "123e4567-e89b-42d3-a456-426614174000", CreatedAt: created}}
	handler := clientInstallationHandler(t, registrar)
	request := installationRequest(`{"token_sha256":"` + strings.Repeat("a", 64) + `"}`)
	request.Header.Del("Authorization")
	createdResponse := httptest.NewRecorder()
	handler.ServeHTTP(createdResponse, request)
	if createdResponse.Code != http.StatusCreated || createdResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("created status=%d headers=%v body=%s", createdResponse.Code, createdResponse.Header(), createdResponse.Body.String())
	}
	if registrar.calls != 1 || registrar.hash[0] == 0 {
		t.Fatalf("registrar calls=%d hash=%x", registrar.calls, registrar.hash)
	}
	var response struct {
		ClientID  string    `json:"client_id"`
		CreatedAt time.Time `json:"created_at"`
	}
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &response); err != nil || response.ClientID != registrar.installation.ClientID || !response.CreatedAt.Equal(created) {
		t.Fatalf("registration body=%#v err=%v", response, err)
	}
	registrar.replay = true
	replay := httptest.NewRecorder()
	handler.ServeHTTP(replay, installationRequest(`{"token_sha256":"`+strings.Repeat("a", 64)+`"}`))
	if replay.Code != http.StatusOK || registrar.calls != 2 {
		t.Fatalf("replay status=%d calls=%d body=%s", replay.Code, registrar.calls, replay.Body.String())
	}
}

func TestClientInstallationValidatesExactHashOnlyInput(t *testing.T) {
	registrar := &installationRegistrarStub{}
	handler := clientInstallationHandler(t, registrar)
	for _, body := range []string{
		`{}`, `{"token_sha256":"` + strings.Repeat("A", 64) + `"}`,
		`{"token_sha256":"` + strings.Repeat("a", 64) + `","owner_id":"9cfe2e2f-135d-42f4-8db5-ddabc5a2f831"}`,
		`{"token_sha256":"` + strings.Repeat("a", 64) + `","token_sha256":"` + strings.Repeat("b", 64) + `"}`,
		`{"token_sha256":"` + strings.Repeat("a", 64) + `"} {}`,
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, installationRequest(body))
		if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), strings.Repeat("a", 64)) {
			t.Fatalf("body %q status=%d response=%s", body, response.Code, response.Body.String())
		}
	}
	if registrar.calls != 0 {
		t.Fatalf("invalid inputs reached store %d times", registrar.calls)
	}
}

func TestClientInstallationAccessErrorsAreBoundedAndACLPrecedesBody(t *testing.T) {
	registrar := &installationRegistrarStub{err: clientenrollment.ErrRateLimited}
	handler := clientInstallationHandler(t, registrar)
	limited := httptest.NewRecorder()
	handler.ServeHTTP(limited, installationRequest(`{"token_sha256":"`+strings.Repeat("a", 64)+`"}`))
	if limited.Code != http.StatusTooManyRequests || strings.Contains(limited.Body.String(), strings.Repeat("a", 64)) || limited.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("rate-limit response=%d %s headers=%v", limited.Code, limited.Body.String(), limited.Header())
	}
	blockedRequest := installationRequest("not-json")
	blockedRequest.RemoteAddr = "203.0.113.4:1000"
	blocked := httptest.NewRecorder()
	handler.ServeHTTP(blocked, blockedRequest)
	if blocked.Code != http.StatusForbidden || registrar.calls != 1 {
		t.Fatalf("ACL status=%d registrar calls=%d body=%s", blocked.Code, registrar.calls, blocked.Body.String())
	}
	wrongMethod := installationRequest(`{}`)
	wrongMethod.Method = http.MethodGet
	methodResponse := httptest.NewRecorder()
	handler.ServeHTTP(methodResponse, wrongMethod)
	if methodResponse.Code != http.StatusMethodNotAllowed || methodResponse.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("method status=%d allow=%q", methodResponse.Code, methodResponse.Header().Get("Allow"))
	}
}

func TestClientInstallationDisabledAndStorageErrorsAre503(t *testing.T) {
	config := testConfig()
	handler := newTestHandler(t, config, &fakeRecognizer{})
	request := installationRequest(`{"token_sha256":"` + strings.Repeat("a", 64) + `"}`)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "client_registration_unavailable") {
		t.Fatalf("disabled status=%d body=%s", response.Code, response.Body.String())
	}
	registrar := &installationRegistrarStub{err: errors.New("private postgres url")}
	handler = clientInstallationHandler(t, registrar)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, installationRequest(`{"token_sha256":"`+strings.Repeat("a", 64)+`"}`))
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "private postgres") {
		t.Fatalf("storage status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestClientInstallationBoundsDatabaseWaitAndReturns503(t *testing.T) {
	registrar := &installationRegistrarStub{wait: true}
	handler := clientInstallationHandler(t, registrar)
	response := httptest.NewRecorder()
	request := installationRequest(`{"token_sha256":"` + strings.Repeat("a", 64) + `"}`)
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || !registrar.deadline || !strings.Contains(response.Body.String(), "client_registration_unavailable") {
		t.Fatalf("bounded registration status=%d deadline=%t body=%s", response.Code, registrar.deadline, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "context deadline") {
		t.Fatalf("registration timeout details leaked: %s", response.Body.String())
	}
}

func installationRequest(body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, clientInstallationsPath, strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Request-ID", "94b86c5c-2520-4c0f-b91e-6d4a948fc88a")
	return request
}

func clientInstallationHandler(t *testing.T, registrar ClientInstallationRegistrar) http.Handler {
	t.Helper()
	config := testConfig()
	config.AllowedClientNetworks = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	config.AsyncJobs = &AsyncJobsConfig{
		Authenticator: &asyncAuthStub{}, Acceptor: &asyncAcceptorStub{},
		ClientEnrollment: registrar, MaxConcurrentUploads: 1, StatusReadTimeout: time.Second,
	}
	return newTestHandler(t, config, &fakeRecognizer{})
}
