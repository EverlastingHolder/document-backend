package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"testing"
	"time"

	"passport-backend/internal/clientauth"
	"passport-backend/internal/jobs"
	"passport-backend/internal/jobstatus"
)

type asyncAuthStub struct {
	principal clientauth.Principal
	err       error
	seen      []string
	wait      bool
	deadline  bool
}

func (a *asyncAuthStub) AuthenticateContext(ctx context.Context, values []string) (clientauth.Principal, error) {
	a.seen = append([]string(nil), values...)
	_, a.deadline = ctx.Deadline()
	if a.wait {
		<-ctx.Done()
		return clientauth.Principal{}, ctx.Err()
	}
	if a.err != nil {
		return clientauth.Principal{}, a.err
	}
	return a.principal, nil
}

type asyncAcceptorStub struct {
	accepted jobs.Accepted
	err      error
	owner    string
	key      string
	data     []byte
	media    string
	calls    int
}

type asyncStatusReaderStub struct {
	snapshot jobstatus.Snapshot
	err      error
	owner    string
	jobID    string
	calls    int
	deadline bool
	wait     bool
}

type asyncReceiptStub struct {
	outcome jobs.ReceiptOutcome
	err     error
	owner   string
	jobID   string
	calls   int
}

func (s *asyncReceiptStub) AcknowledgeOwnedResult(_ context.Context, owner, jobID string) (jobs.ReceiptOutcome, error) {
	s.calls++
	s.owner, s.jobID = owner, jobID
	return s.outcome, s.err
}

func (s *asyncStatusReaderStub) ReadOwned(ctx context.Context, owner, jobID string) (jobstatus.Snapshot, error) {
	s.calls++
	s.owner, s.jobID = owner, jobID
	_, s.deadline = ctx.Deadline()
	if s.wait {
		<-ctx.Done()
		return jobstatus.Snapshot{}, ctx.Err()
	}
	return s.snapshot, s.err
}

func (a *asyncAcceptorStub) Accept(_ context.Context, owner, key string, data []byte, media string) (jobs.Accepted, error) {
	a.calls++
	a.owner, a.key, a.data, a.media = owner, key, append([]byte(nil), data...), media
	return a.accepted, a.err
}

func asyncTestHandler(t *testing.T, auth AsyncAuthenticator, acceptor AsyncAcceptor) http.Handler {
	t.Helper()
	config := testConfig()
	config.AsyncJobs = &AsyncJobsConfig{Authenticator: auth, Acceptor: acceptor, MaxConcurrentUploads: 2, StatusReadTimeout: time.Second}
	return newTestHandler(t, config, &fakeRecognizer{})
}

func asyncStatusTestHandler(t *testing.T, auth AsyncAuthenticator, reader AsyncStatusReader, readTimeout time.Duration) http.Handler {
	t.Helper()
	config := testConfig()
	config.AsyncJobs = &AsyncJobsConfig{
		Authenticator: auth, Acceptor: &asyncAcceptorStub{}, StatusReader: reader,
		StatusReadTimeout: readTimeout, MaxConcurrentUploads: 2,
	}
	return newTestHandler(t, config, &fakeRecognizer{})
}

func asyncStatusRequest(auth, path string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.RemoteAddr = "127.0.0.1:1234"
	if auth != "" {
		request.Header.Set("Authorization", auth)
	}
	return request
}

func asyncUploadRequest(t *testing.T, auth, key string, body []byte) *http.Request {
	t.Helper()
	var multipartBody bytes.Buffer
	writer := multipart.NewWriter(&multipartBody)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="scan.png"`)
	header.Set("Content-Type", "image/png")
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, jobsPath, &multipartBody)
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Content-Type", writer.FormDataContentType())
	if auth != "" {
		request.Header.Set("Authorization", auth)
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	return request
}

func TestAsyncSubmissionReturnsAcceptedOnlyAfterAcceptorSuccess(t *testing.T) {
	auth := &asyncAuthStub{principal: clientauth.Principal{OwnerID: "owner-from-token"}}
	created := time.Date(2026, 10, 31, 12, 34, 56, 0, time.UTC)
	acceptor := &asyncAcceptorStub{accepted: jobs.Accepted{JobID: "9cfe2e2f-135d-42f4-8db5-ddabc5a2f831", Status: jobs.StatusQueued, CreatedAt: created}}
	handler := asyncTestHandler(t, auth, acceptor)
	body := syntheticPNG()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, asyncUploadRequest(t, "Bearer pbt1_test", "key-1", body))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Location") != jobsPath+"/"+acceptor.accepted.JobID || recorder.Header().Get("X-Request-ID") == "" {
		t.Fatalf("unexpected response headers: %v", recorder.Header())
	}
	var response jobAcceptedResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.JobID != acceptor.accepted.JobID || response.Status != jobs.StatusQueued || !response.CreatedAt.Equal(created) {
		t.Fatalf("accepted response=%+v", response)
	}
	if acceptor.calls != 1 || acceptor.owner != "owner-from-token" || acceptor.key != "key-1" || acceptor.media != "image/png" || !bytes.Equal(acceptor.data, body) {
		t.Fatalf("acceptor args: calls=%d owner=%q key=%q media=%q data_match=%v", acceptor.calls, acceptor.owner, acceptor.key, acceptor.media, bytes.Equal(acceptor.data, body))
	}
}

func TestAsyncSubmissionChecksAuthenticationBeforeReadingBody(t *testing.T) {
	auth := &asyncAuthStub{err: clientauth.ErrUnauthorized}
	acceptor := &asyncAcceptorStub{}
	handler := asyncTestHandler(t, auth, acceptor)
	request := httptest.NewRequest(http.MethodPost, jobsPath, bytes.NewBufferString("not multipart"))
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Idempotency-Key", "key-1")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized || recorder.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("status=%d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
	}
	if acceptor.calls != 0 {
		t.Fatalf("acceptor called %d times for unauthenticated request", acceptor.calls)
	}
}

func TestAsyncSubmissionBoundsDynamicAuthenticationAndMapsTimeoutTo503(t *testing.T) {
	auth := &asyncAuthStub{wait: true}
	config := testConfig()
	config.AsyncJobs = &AsyncJobsConfig{
		Authenticator: auth, Acceptor: &asyncAcceptorStub{}, MaxConcurrentUploads: 1, StatusReadTimeout: 5 * time.Millisecond,
	}
	handler := newTestHandler(t, config, &fakeRecognizer{})
	request := httptest.NewRequest(http.MethodPost, jobsPath, bytes.NewBufferString("body-not-read"))
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Authorization", "Bearer pbt1_test")
	request.Header.Set("Idempotency-Key", "stable-key")
	request.Header.Set("X-Request-ID", "94b86c5c-2520-4c0f-b91e-6d4a948fc88a")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var body errorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusServiceUnavailable || body.Code != "job_acceptance_unavailable" || !auth.deadline {
		t.Fatalf("auth timeout status=%d deadline=%t body=%#v", response.Code, auth.deadline, body)
	}
}

func TestAsyncSubmissionIsUnavailableWhenAsyncStorageIsNotConfigured(t *testing.T) {
	handler := newTestHandler(t, testConfig(), &fakeRecognizer{})
	request := asyncUploadRequest(t, "Bearer unused", "key-1", syntheticPNG())
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want=%d body=%s", recorder.Code, http.StatusServiceUnavailable, recorder.Body.String())
	}
	var response errorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Code != "job_acceptance_unavailable" {
		t.Fatalf("error response=%+v", response)
	}
}

func TestAsyncSubmissionMapsConflictAndUnavailableWithoutAccepting(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want int
	}{
		{name: "conflict", err: jobs.ErrConflict, want: http.StatusConflict},
		{name: "unknown storage outcome", err: jobs.ErrUnavailable, want: http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			auth := &asyncAuthStub{principal: clientauth.Principal{OwnerID: "owner"}}
			acceptor := &asyncAcceptorStub{err: test.err}
			handler := asyncTestHandler(t, auth, acceptor)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, asyncUploadRequest(t, "Bearer pbt1_test", "key-1", syntheticPNG()))
			if recorder.Code != test.want {
				t.Fatalf("status=%d want=%d body=%s", recorder.Code, test.want, recorder.Body.String())
			}
			if acceptor.calls != 1 {
				t.Fatalf("acceptor calls=%d, want 1", acceptor.calls)
			}
		})
	}
}

func TestAsyncStatusReturnsOnlyStateAppropriateFieldsAndNeverCaches(t *testing.T) {
	started := time.Date(2026, 10, 31, 12, 35, 0, 0, time.UTC)
	finished := started.Add(time.Minute)
	created := started.Add(-time.Minute)
	jobID := "9cfe2e2f-135d-42f4-8db5-ddabc5a2f831"
	for _, test := range []struct {
		name     string
		snapshot jobstatus.Snapshot
		result   bool
		error    bool
	}{
		{name: "queued", snapshot: jobstatus.Snapshot{JobID: jobID, Status: jobs.StatusQueued, CreatedAt: created, UpdatedAt: created}},
		{name: "processing", snapshot: jobstatus.Snapshot{JobID: jobID, Status: jobs.StatusProcessing, CreatedAt: created, UpdatedAt: started, StartedAt: &started}},
		{name: "completed", snapshot: jobstatus.Snapshot{JobID: jobID, Status: jobs.StatusCompleted, CreatedAt: created, UpdatedAt: finished, StartedAt: &started, FinishedAt: &finished, Result: json.RawMessage(`{"request_id":"9cfe2e2f-135d-42f4-8db5-ddabc5a2f831","status":"completed"}`)}, result: true},
		{name: "failed", snapshot: jobstatus.Snapshot{JobID: jobID, Status: jobs.StatusFailed, CreatedAt: created, UpdatedAt: finished, StartedAt: &started, FinishedAt: &finished, Error: &jobstatus.PublicError{Code: "processing_failed", Message: "safe"}}, error: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			auth := &asyncAuthStub{principal: clientauth.Principal{OwnerID: "owner-from-token"}}
			reader := &asyncStatusReaderStub{snapshot: test.snapshot}
			handler := asyncStatusTestHandler(t, auth, reader, time.Second)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, asyncStatusRequest("Bearer pbt1_test", jobsPath+"/"+jobID))
			if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			_, hasResult := body["result"]
			_, hasError := body["error"]
			if hasResult != test.result || hasError != test.error {
				t.Fatalf("state response has result=%v error=%v: %s", hasResult, hasError, recorder.Body.String())
			}
			if reader.calls != 1 || reader.owner != "owner-from-token" || reader.jobID != jobID || !reader.deadline {
				t.Fatalf("status reader calls=%d owner=%q job=%q deadline=%v", reader.calls, reader.owner, reader.jobID, reader.deadline)
			}
		})
	}
}

func TestAsyncStatusAuthenticatesBeforeReadingAndDoesNotUseOCRSemaphore(t *testing.T) {
	reader := &asyncStatusReaderStub{snapshot: jobstatus.Snapshot{JobID: "9cfe2e2f-135d-42f4-8db5-ddabc5a2f831", Status: jobs.StatusQueued, CreatedAt: time.Now(), UpdatedAt: time.Now()}}
	badAuth := &asyncAuthStub{err: clientauth.ErrUnauthorized}
	h := asyncStatusTestHandler(t, badAuth, reader, time.Second).(*handler)
	// Occupy the synchronous OCR semaphore. A status read must remain independent.
	h.semaphore <- struct{}{}
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, asyncStatusRequest("Bearer pwt1_worker", jobsPath+"/9cfe2e2f-135d-42f4-8db5-ddabc5a2f831"))
	if recorder.Code != http.StatusUnauthorized || recorder.Header().Get("Cache-Control") != "no-store" || reader.calls != 0 {
		t.Fatalf("status=%d reader calls=%d headers=%v body=%s", recorder.Code, reader.calls, recorder.Header(), recorder.Body.String())
	}

	goodAuth := &asyncAuthStub{principal: clientauth.Principal{OwnerID: "owner"}}
	reader.snapshot = jobstatus.Snapshot{JobID: "9cfe2e2f-135d-42f4-8db5-ddabc5a2f831", Status: jobs.StatusQueued, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	secondHandler := asyncStatusTestHandler(t, goodAuth, reader, time.Second).(*handler)
	secondHandler.semaphore <- struct{}{}
	secondRecorder := httptest.NewRecorder()
	secondHandler.ServeHTTP(secondRecorder, asyncStatusRequest("Bearer pbt1_test", jobsPath+"/9cfe2e2f-135d-42f4-8db5-ddabc5a2f831"))
	if secondRecorder.Code != http.StatusOK || reader.calls != 1 {
		t.Fatalf("status read was blocked by OCR semaphore: status=%d calls=%d body=%s", secondRecorder.Code, reader.calls, secondRecorder.Body.String())
	}
}

func TestAsyncStatusAuthenticationStoreFailureMapsTo503(t *testing.T) {
	auth := &asyncAuthStub{err: clientauth.ErrAuthenticationUnavailable}
	reader := &asyncStatusReaderStub{}
	handler := asyncStatusTestHandler(t, auth, reader, time.Second)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, asyncStatusRequest("Bearer pbt1_test", jobsPath+"/9cfe2e2f-135d-42f4-8db5-ddabc5a2f831"))
	var body errorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusServiceUnavailable || body.Code != "job_status_unavailable" || reader.calls != 0 {
		t.Fatalf("auth store failure status=%d body=%#v reads=%d", response.Code, body, reader.calls)
	}
}

func TestAsyncStatusMapsMissingForeignAndMalformedToSameNotFound(t *testing.T) {
	for _, id := range []string{"9cfe2e2f-135d-42f4-8db5-ddabc5a2f831", "11111111-1111-4111-8111-111111111111", "not-a-uuid"} {
		t.Run(id, func(t *testing.T) {
			auth := &asyncAuthStub{principal: clientauth.Principal{OwnerID: "owner"}}
			reader := &asyncStatusReaderStub{err: jobs.ErrNotFound}
			handler := asyncStatusTestHandler(t, auth, reader, time.Second)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, asyncStatusRequest("Bearer pbt1_test", jobsPath+"/"+id))
			var response errorResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != http.StatusNotFound || response.Code != "not_found" || recorder.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d response=%+v headers=%v", recorder.Code, response, recorder.Header())
			}
		})
	}
}

func TestAsyncStatusUnavailableAndMethodRouting(t *testing.T) {
	auth := &asyncAuthStub{principal: clientauth.Principal{OwnerID: "owner"}}
	reader := &asyncStatusReaderStub{err: context.DeadlineExceeded}
	handler := asyncStatusTestHandler(t, auth, reader, time.Second)
	request := asyncStatusRequest("Bearer pbt1_test", jobsPath+"/9cfe2e2f-135d-42f4-8db5-ddabc5a2f831")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	var response errorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusServiceUnavailable || response.Code != "job_status_unavailable" || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d response=%+v headers=%v", recorder.Code, response, recorder.Header())
	}

	for _, test := range []struct {
		path, method, allow string
		status              int
	}{
		{path: jobsPath + "/id", method: http.MethodPost, allow: http.MethodGet, status: http.StatusMethodNotAllowed},
		{path: jobsPath, method: http.MethodGet, allow: http.MethodPost, status: http.StatusMethodNotAllowed},
		{path: jobsPath + "/id/nested", method: http.MethodGet, status: http.StatusNotFound},
	} {
		req := httptest.NewRequest(test.method, test.path, nil)
		req.RemoteAddr = "127.0.0.1:1234"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != test.status || (test.allow != "" && rec.Header().Get("Allow") != test.allow) {
			t.Errorf("%s %s: status=%d Allow=%q body=%s", test.method, test.path, rec.Code, rec.Header().Get("Allow"), rec.Body.String())
		}
	}
}

func TestAsyncStatusAppliesReadTimeout(t *testing.T) {
	auth := &asyncAuthStub{principal: clientauth.Principal{OwnerID: "owner"}}
	reader := &asyncStatusReaderStub{wait: true}
	handler := asyncStatusTestHandler(t, auth, reader, 5*time.Millisecond)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, asyncStatusRequest("Bearer pbt1_test", jobsPath+"/9cfe2e2f-135d-42f4-8db5-ddabc5a2f831"))
	var response errorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusServiceUnavailable || response.Code != "job_status_unavailable" || reader.calls != 1 || !reader.deadline {
		t.Fatalf("status=%d response=%+v reader=%+v", recorder.Code, response, reader)
	}
}

func TestAsyncStatusUnavailableBeforeAuthenticationWhenReaderMissing(t *testing.T) {
	auth := &asyncAuthStub{err: errors.New("must not authenticate")}
	config := testConfig()
	config.AsyncJobs = &AsyncJobsConfig{Authenticator: auth, Acceptor: &asyncAcceptorStub{}, MaxConcurrentUploads: 1, StatusReadTimeout: time.Second}
	handler := newTestHandler(t, config, &fakeRecognizer{})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, asyncStatusRequest("Bearer pbt1_test", jobsPath+"/9cfe2e2f-135d-42f4-8db5-ddabc5a2f831"))
	var response errorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusServiceUnavailable || response.Code != "job_status_unavailable" || len(auth.seen) != 0 || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d response=%+v auth calls=%v headers=%v", recorder.Code, response, auth.seen, recorder.Header())
	}
}

func TestAsyncStatusInvalidRequestIDStillSetsNoStore(t *testing.T) {
	config := testConfig()
	config.AsyncJobs = &AsyncJobsConfig{
		Authenticator: &asyncAuthStub{principal: clientauth.Principal{OwnerID: "owner"}},
		Acceptor:      &asyncAcceptorStub{}, MaxConcurrentUploads: 1,
		StatusReader: &asyncStatusReaderStub{}, StatusReadTimeout: time.Second,
	}
	handler := newTestHandler(t, config, &fakeRecognizer{})
	request := asyncStatusRequest("Bearer pbt1_test", jobsPath+"/not-a-uuid")
	request.Header.Set("X-Request-ID", "bad")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
	}
}

func TestAsyncReceiptIsOwnerScopedAndIdempotent(t *testing.T) {
	owner := "2c4f8b29-f1cc-4fe8-ae4a-8a14c118df45"
	jobID := "39f1f384-08a1-4d7b-8e86-88aeced08d12"
	for _, outcome := range []jobs.ReceiptOutcome{jobs.ReceiptReceived, jobs.ReceiptAlreadyPurged} {
		receipts := &asyncReceiptStub{outcome: outcome}
		config := testConfig()
		config.AsyncJobs = &AsyncJobsConfig{
			Authenticator: &asyncAuthStub{principal: clientauth.Principal{OwnerID: owner}},
			Acceptor:      &asyncAcceptorStub{}, ReceiptAcknowledger: receipts,
			StatusReadTimeout: time.Second, MaxConcurrentUploads: 1,
		}
		handler := newTestHandler(t, config, &fakeRecognizer{})
		request := httptest.NewRequest(http.MethodPost, jobsPath+"/"+jobID+"/receipt", nil)
		request.RemoteAddr = "127.0.0.1:1234"
		request.Header.Set("Authorization", "Bearer pbt1_placeholder")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent || response.Body.Len() != 0 || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("receipt outcome %d = %d %q", outcome, response.Code, response.Body.String())
		}
		if receipts.calls != 1 || receipts.owner != owner || receipts.jobID != jobID {
			t.Fatalf("receipt called with calls=%d owner=%q job=%q", receipts.calls, receipts.owner, receipts.jobID)
		}
	}
}

func TestAsyncReceiptActiveAndHiddenJobResponses(t *testing.T) {
	owner := "2c4f8b29-f1cc-4fe8-ae4a-8a14c118df45"
	jobID := "39f1f384-08a1-4d7b-8e86-88aeced08d12"
	for _, test := range []struct {
		name     string
		stub     *asyncReceiptStub
		want     int
		wantCode string
	}{
		{name: "active", stub: &asyncReceiptStub{outcome: jobs.ReceiptActive}, want: http.StatusConflict, wantCode: "job_not_terminal"},
		{name: "foreign or missing", stub: &asyncReceiptStub{err: jobs.ErrNotFound}, want: http.StatusNotFound, wantCode: "not_found"},
		{name: "storage unavailable", stub: &asyncReceiptStub{err: jobs.ErrUnavailable}, want: http.StatusServiceUnavailable, wantCode: "job_status_unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig()
			config.AsyncJobs = &AsyncJobsConfig{
				Authenticator: &asyncAuthStub{principal: clientauth.Principal{OwnerID: owner}},
				Acceptor:      &asyncAcceptorStub{}, ReceiptAcknowledger: test.stub,
				StatusReadTimeout: time.Second, MaxConcurrentUploads: 1,
			}
			handler := newTestHandler(t, config, &fakeRecognizer{})
			request := httptest.NewRequest(http.MethodPost, jobsPath+"/"+jobID+"/receipt", nil)
			request.RemoteAddr = "127.0.0.1:1234"
			request.Header.Set("Authorization", "Bearer pbt1_placeholder")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			var body errorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("error body is not JSON: %v", err)
			}
			if response.Code != test.want || body.Code != test.wantCode || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("receipt = %d %+v headers=%v", response.Code, body, response.Header())
			}
		})
	}
}

func TestAsyncStatusResultGoneReturns410(t *testing.T) {
	reader := &asyncStatusReaderStub{err: jobs.ErrResultGone}
	handler := asyncStatusTestHandler(t, &asyncAuthStub{principal: clientauth.Principal{OwnerID: "2c4f8b29-f1cc-4fe8-ae4a-8a14c118df45"}}, reader, time.Second)
	request := asyncStatusRequest("Bearer pbt1_placeholder", jobsPath+"/39f1f384-08a1-4d7b-8e86-88aeced08d12")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var body errorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not JSON: %v", err)
	}
	if response.Code != http.StatusGone || body.Code != "job_result_gone" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status = %d %+v headers=%v", response.Code, body, response.Header())
	}
}

func TestNewHandlerRequiresStatusReadBudgetWhenReaderIsConfigured(t *testing.T) {
	config := testConfig()
	config.AsyncJobs = &AsyncJobsConfig{
		Authenticator: &asyncAuthStub{principal: clientauth.Principal{OwnerID: "owner"}},
		Acceptor:      &asyncAcceptorStub{}, MaxConcurrentUploads: 1,
		StatusReader: &asyncStatusReaderStub{},
	}
	if _, err := NewHandler(config, &fakeRecognizer{}); err == nil {
		t.Fatal("NewHandler accepted status reader without a positive read budget")
	}
}
