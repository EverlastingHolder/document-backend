//go:build integration

package postgres_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"passport-backend/internal/clientauth"
	"passport-backend/internal/httpapi"
	"passport-backend/internal/jobintake"
	"passport-backend/internal/jobs"
	"passport-backend/internal/jobstore/postgres"
	"passport-backend/internal/passport"
)

type integrationRecognizer struct{}

func (integrationRecognizer) Recognize(context.Context, httpapi.Image) (passport.Extraction, error) {
	return passport.Extraction{SchemaVersion: passport.ExtractionSchema, Fields: map[string]passport.FieldResult{}}, nil
}

type credentialFixture struct {
	Owner string
	Token string
}

type credentialJSON struct {
	OwnerID     string `json:"owner_id"`
	TokenSHA256 string `json:"token_sha256"`
}

type integrationHTTPFixture struct {
	handler http.Handler
	server  *httptest.Server
	pool    *pgxpool.Pool
	store   *postgres.Store
	images  *jobintake.Service
	root    string
	first   credentialFixture
	second  credentialFixture
	logs    *bytes.Buffer
}

func newIntegrationHTTPFixture(t *testing.T, ctx context.Context, dsn string, maxTotalBytes, maxUploadBytes int64) *integrationHTTPFixture {
	t.Helper()
	service, store, _, imageRoot, pool := newIntegrationIntake(t, ctx, dsn, maxTotalBytes)
	registryDir, err := os.MkdirTemp("/integration-images", "credentials-")
	if err != nil {
		t.Fatalf("create private integration credential directory: %v", err)
	}
	if err := os.Chmod(registryDir, 0o700); err != nil {
		t.Fatalf("protect private integration credential directory: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(registryDir); err != nil {
			t.Errorf("remove this test's private credential directory: %v", err)
		}
	})
	first := newCredentialFixture(t)
	second := newCredentialFixture(t)
	registryPath := filepath.Join(registryDir, "credentials.json")
	registryJSON, err := json.Marshal(struct {
		SchemaVersion string           `json:"schema_version"`
		Credentials   []credentialJSON `json:"credentials"`
	}{
		SchemaVersion: "passport-client-credentials/v1",
		Credentials: []credentialJSON{
			{OwnerID: first.Owner, TokenSHA256: tokenHash(t, first.Token)},
			{OwnerID: second.Owner, TokenSHA256: tokenHash(t, second.Token)},
		},
	})
	if err != nil {
		t.Fatalf("encode private integration credential registry: %v", err)
	}
	if err := os.WriteFile(registryPath, registryJSON, 0o600); err != nil {
		t.Fatalf("write private integration credential registry: %v", err)
	}
	registry, err := clientauth.Load(registryPath)
	if err != nil {
		t.Fatalf("load real private hashed credential registry: %v", err)
	}
	logs := &bytes.Buffer{}
	config := httpapi.Config{
		MaxUploadBytes: maxUploadBytes, OCRTimeout: time.Second, MaxConcurrent: 4,
		PipelineVersion: "integration-only", AllowedClientNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		Logger:    slog.New(slog.NewTextHandler(logs, nil)),
		AsyncJobs: &httpapi.AsyncJobsConfig{Authenticator: registry, Acceptor: service, MaxConcurrentUploads: 128},
	}
	handler, err := httpapi.NewHandler(config, integrationRecognizer{})
	if err != nil {
		t.Fatalf("construct actual HTTP handler around real auth, intake, PostgreSQL, and filesystem: %v", err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &integrationHTTPFixture{
		handler: handler, server: server, pool: pool, store: store, images: service, root: imageRoot,
		first: first, second: second, logs: logs,
	}
}

func newCredentialFixture(t *testing.T) credentialFixture {
	t.Helper()
	token, _, err := clientauth.NewToken()
	if err != nil {
		t.Fatalf("generate in-memory integration Bearer token: %v", err)
	}
	owner, err := clientauth.NewOwnerID()
	if err != nil {
		t.Fatalf("generate integration owner ID: %v", err)
	}
	return credentialFixture{Owner: owner, Token: token}
}

func tokenHash(t *testing.T, token string) string {
	t.Helper()
	return hashForToken(token)
}

func hashForToken(token string) string {
	// Keep token hashing in the test setup only; it is written to a 0600 file.
	digest := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x", digest[:])
}

func multipartRequest(t *testing.T, url, token, key, mediaType string, data []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="scan.bin"`)
	header.Set("Content-Type", mediaType)
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatalf("create multipart test image: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("write multipart test image: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart test body: %v", err)
	}
	request, err := http.NewRequest(http.MethodPost, url, &body)
	if err != nil {
		t.Fatalf("construct integration HTTP request: %v", err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	return request
}

type acceptedPayload struct {
	JobID     string      `json:"job_id"`
	Status    jobs.Status `json:"status"`
	StatusURL string      `json:"status_url"`
	CreatedAt time.Time   `json:"created_at"`
}

func (f *integrationHTTPFixture) post(t *testing.T, token, key, mediaType string, data []byte) (*http.Response, acceptedPayload, []byte) {
	t.Helper()
	request := multipartRequest(t, f.server.URL+"/v1/passport/jobs", token, key, mediaType, data)
	response, err := f.server.Client().Do(request)
	if err != nil {
		t.Fatalf("POST asynchronous intake: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read asynchronous intake response: %v", err)
	}
	var payload acceptedPayload
	if response.StatusCode == http.StatusAccepted {
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("decode accepted response: %v", err)
		}
	}
	return response, payload, body
}

func TestPostgresHTTPAsyncIntakeIsDurableOwnerScopedAndIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()
	dsn := integrationDSN(t)
	data := []byte("synthetic PRIVATE-PII-SENTINEL image payload")
	f := newIntegrationHTTPFixture(t, ctx, dsn, 1<<20, 1<<20)

	request := multipartRequest(t, f.server.URL+"/v1/passport/jobs", f.first.Token, "http-idempotency", "image/png", data)
	request.Header.Set("X-Owner-ID", f.second.Owner)
	request.Header.Set("X-Request-ID", "94b86c5c-2520-4c0f-b91e-6d4a948fc88a")
	response, err := f.server.Client().Do(request)
	if err != nil {
		t.Fatalf("POST real composed handler: %v", err)
	}
	responseBody, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusAccepted {
		t.Fatalf("first POST status=%d readErr=%v body=%s", response.StatusCode, err, responseBody)
	}
	var accepted acceptedPayload
	if err := json.Unmarshal(responseBody, &accepted); err != nil {
		t.Fatalf("decode 202 response: %v", err)
	}
	var acceptedKeys map[string]json.RawMessage
	if err := json.Unmarshal(responseBody, &acceptedKeys); err != nil {
		t.Fatalf("decode 202 response keys: %v", err)
	}
	wantLocation := "/v1/passport/jobs/" + accepted.JobID
	if len(acceptedKeys) != 4 || acceptedKeys["job_id"] == nil || acceptedKeys["status"] == nil || acceptedKeys["status_url"] == nil || acceptedKeys["created_at"] == nil ||
		accepted.JobID == "" || accepted.Status != jobs.StatusQueued || accepted.CreatedAt.IsZero() || accepted.StatusURL != wantLocation ||
		response.Header.Get("Location") != wantLocation || response.Header.Get("Cache-Control") != "no-store" ||
		response.Header.Get("X-Request-ID") != request.Header.Get("X-Request-ID") {
		t.Fatalf("202 response metadata/headers mismatch: payload=%+v keys=%v headers=%v", accepted, acceptedKeys, response.Header)
	}
	assertRepositoryRowCounts(t, ctx, f.pool, f.first.Owner, "http-idempotency", 1, 1, 0)
	var storedOwner, storedMedia string
	var storedSize int64
	var storedDigest []byte
	var storedJobID string
	if err := f.pool.QueryRow(ctx, `SELECT job_id::text, owner_id, image_media_type, image_size_bytes, request_digest
		FROM jobs WHERE owner_id=$1 AND idempotency_key=$2`, f.first.Owner, "http-idempotency").Scan(&storedJobID, &storedOwner, &storedMedia, &storedSize, &storedDigest); err != nil {
		t.Fatalf("read durable HTTP-created job row: %v", err)
	}
	wantDigest := expectedIntakeDigest(data, "image/png")
	if storedJobID != accepted.JobID || storedOwner != f.first.Owner || storedOwner == f.second.Owner || storedMedia != "image/png" || storedSize != int64(len(data)) ||
		!bytes.Equal(storedDigest, wantDigest[:]) {
		t.Fatalf("durable HTTP job metadata does not match authenticated upload: job=%q owner=%q media=%q size=%d", storedJobID, storedOwner, storedMedia, storedSize)
	}
	assertInitialOutbox(t, ctx, f.pool, accepted.JobID)
	assertImageObjectCount(t, f.root, 1)

	replay, replayPayload, _ := f.post(t, f.first.Token, "http-idempotency", "image/png", data)
	if replay.StatusCode != http.StatusAccepted || replayPayload.JobID != accepted.JobID || replayPayload.Status != accepted.Status || !replayPayload.CreatedAt.Equal(accepted.CreatedAt) {
		t.Fatalf("same owner/key/MIME/bytes replay=%+v status=%d, want original job", replayPayload, replay.StatusCode)
	}
	changedMedia, _, _ := f.post(t, f.first.Token, "http-idempotency", "image/jpeg", data)
	changedBody, _, _ := f.post(t, f.first.Token, "http-idempotency", "image/png", []byte("different image bytes"))
	if changedMedia.StatusCode != http.StatusConflict || changedBody.StatusCode != http.StatusConflict {
		t.Fatalf("changed MIME/body idempotency statuses = %d/%d, want 409/409", changedMedia.StatusCode, changedBody.StatusCode)
	}
	otherOwner, otherPayload, _ := f.post(t, f.second.Token, "http-idempotency", "image/png", data)
	if otherOwner.StatusCode != http.StatusAccepted || otherPayload.JobID == accepted.JobID {
		t.Fatalf("same key under independent authenticated owner = %d/%+v, want separate 202 job", otherOwner.StatusCode, otherPayload)
	}
	assertRepositoryRowCounts(t, ctx, f.pool, f.first.Owner, "http-idempotency", 1, 1, 0)
	assertRepositoryRowCounts(t, ctx, f.pool, f.second.Owner, "http-idempotency", 1, 1, 0)
	assertImageObjectCount(t, f.root, 2)

	// The historical synchronous route remains available and returns its 200.
	syncResponse := sendSyncRecognition(t, f.server, data)
	if syncResponse != http.StatusOK {
		t.Fatalf("existing synchronous /v1/passport/recognize status=%d, want 200", syncResponse)
	}

	logs := f.logs.String()
	for _, privateValue := range []string{f.first.Token, f.second.Token, f.first.Owner, f.second.Owner, "http-idempotency", "PRIVATE-PII-SENTINEL", f.root} {
		if strings.Contains(logs, privateValue) {
			t.Fatalf("HTTP request logs contain private token, owner, key, body marker, or filesystem path")
		}
	}
}

func TestPostgresHTTPAsyncIntakeRejectsInvalidRequestsWithoutPersistence(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	f := newIntegrationHTTPFixture(t, ctx, integrationDSN(t), 1<<20, 32)
	valid := []byte("small synthetic image")
	tests := []struct {
		name      string
		token     string
		key       string
		mediaType string
		data      []byte
		want      int
	}{
		{name: "missing credentials", key: "no-auth", mediaType: "image/png", data: valid, want: http.StatusUnauthorized},
		{name: "invalid credentials", token: "not-a-token", key: "bad-auth", mediaType: "image/png", data: valid, want: http.StatusUnauthorized},
		{name: "missing idempotency key", token: f.first.Token, mediaType: "image/png", data: valid, want: http.StatusBadRequest},
		{name: "unsupported media", token: f.first.Token, key: "unsupported", mediaType: "image/gif", data: valid, want: http.StatusUnsupportedMediaType},
		{name: "oversized upload", token: f.first.Token, key: "too-large", mediaType: "image/png", data: bytes.Repeat([]byte("x"), 64), want: http.StatusRequestEntityTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, _, body := f.post(t, test.token, test.key, test.mediaType, test.data)
			if response.StatusCode != test.want {
				t.Fatalf("status=%d want=%d body=%s", response.StatusCode, test.want, body)
			}
		})
	}
	// Directly set a disallowed peer on the real handler to exercise its ACL.
	request := multipartRequest(t, "http://integration.invalid/v1/passport/jobs", f.first.Token, "acl-denied", "image/png", valid)
	request.RemoteAddr = "203.0.113.7:4567"
	aclResponse := httptest.NewRecorder()
	f.handler.ServeHTTP(aclResponse, request)
	if aclResponse.Code != http.StatusForbidden {
		t.Fatalf("disallowed peer status=%d, want 403", aclResponse.Code)
	}
	if _, err := f.store.Lookup(ctx, f.first.Owner, "acl-denied"); !errors.Is(err, jobs.ErrNotFound) {
		t.Fatalf("ACL rejection persisted a job: %v", err)
	}
	var jobCount, outboxCount int64
	if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM jobs WHERE owner_id=$1", f.first.Owner).Scan(&jobCount); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM outbox o JOIN jobs j USING(job_id) WHERE j.owner_id=$1", f.first.Owner).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if jobCount != 0 || outboxCount != 0 {
		t.Fatalf("invalid/unauthorized HTTP requests persisted jobs/outbox=%d/%d", jobCount, outboxCount)
	}
	assertImageObjectCount(t, f.root, 0)
}

func TestPostgresHTTPAsyncIntakeConcurrentUploadsDoNotUseOCRLimiter(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Second)
	defer cancel()
	f := newIntegrationHTTPFixture(t, ctx, integrationDSN(t), 4<<20, 1<<20)

	const sameRequestCount = 50
	sameRequests := make([]*http.Request, sameRequestCount)
	for i := range sameRequests {
		sameRequests[i] = multipartRequest(t, f.server.URL+"/v1/passport/jobs", f.first.Token, "http-concurrent-same", "image/png", []byte("one concurrent HTTP body"))
	}
	sameStatuses, sameIDs, sameErrors := doConcurrentHTTP(sameRequests)
	sameWinner := ""
	for i := range sameRequests {
		if sameErrors[i] != nil || sameStatuses[i] != http.StatusAccepted || sameIDs[i] == "" {
			t.Fatalf("concurrent same-key HTTP request %d status=%d job=%q error=%v", i, sameStatuses[i], sameIDs[i], sameErrors[i])
		}
		if sameWinner == "" {
			sameWinner = sameIDs[i]
		}
		if sameIDs[i] != sameWinner {
			t.Fatalf("same-key HTTP requests returned jobs %q and %q", sameWinner, sameIDs[i])
		}
	}
	assertRepositoryRowCounts(t, ctx, f.pool, f.first.Owner, "http-concurrent-same", 1, 1, 0)
	assertImageObjectCount(t, f.root, 1)

	const changedRequestCount = 50
	changedRequests := make([]*http.Request, changedRequestCount)
	for i := range changedRequests {
		body := []byte("concurrent HTTP content A")
		if i%2 != 0 {
			body = []byte("concurrent HTTP content B")
		}
		changedRequests[i] = multipartRequest(t, f.server.URL+"/v1/passport/jobs", f.first.Token, "http-concurrent-changed", "image/png", body)
	}
	changedStatuses, changedIDs, changedErrors := doConcurrentHTTP(changedRequests)
	acceptedCount, conflictCount := 0, 0
	changedWinner := ""
	for i := range changedRequests {
		if changedErrors[i] != nil {
			t.Fatalf("concurrent changed-payload HTTP request %d error=%v", i, changedErrors[i])
		}
		switch changedStatuses[i] {
		case http.StatusAccepted:
			acceptedCount++
			if changedWinner == "" {
				changedWinner = changedIDs[i]
			}
			if changedIDs[i] != changedWinner {
				t.Fatalf("changed-payload HTTP requests returned multiple jobs %q and %q", changedWinner, changedIDs[i])
			}
		case http.StatusConflict:
			conflictCount++
		default:
			t.Fatalf("concurrent changed-payload HTTP request %d returned %d, want 202 or 409", i, changedStatuses[i])
		}
	}
	if acceptedCount == 0 || conflictCount == 0 || acceptedCount+conflictCount != changedRequestCount {
		t.Fatalf("same-key HTTP race accepted=%d conflicted=%d, want both outcomes over %d requests", acceptedCount, conflictCount, changedRequestCount)
	}
	assertRepositoryRowCounts(t, ctx, f.pool, f.first.Owner, "http-concurrent-changed", 1, 1, 0)
	assertImageObjectCount(t, f.root, 2)

	const count = 100
	var wg sync.WaitGroup
	statuses := make([]int, count)
	ids := make([]string, count)
	errs := make([]error, count)
	for i := range count {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			data := []byte(fmt.Sprintf("small unique integration image %03d", index))
			request := multipartRequest(t, f.server.URL+"/v1/passport/jobs", f.first.Token, fmt.Sprintf("bulk-%03d", index), "image/webp", data)
			response, err := f.server.Client().Do(request)
			if err != nil {
				errs[index] = err
				return
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				errs[index] = err
				return
			}
			statuses[index] = response.StatusCode
			if response.StatusCode == http.StatusAccepted {
				var payload acceptedPayload
				if err := json.Unmarshal(body, &payload); err != nil {
					errs[index] = err
					return
				}
				ids[index] = payload.JobID
			}
		}(i)
	}
	wg.Wait()
	unique := make(map[string]struct{}, count)
	for i := range count {
		if errs[i] != nil || statuses[i] != http.StatusAccepted || ids[i] == "" {
			t.Fatalf("upload %d returned status=%d job=%q error=%v; all bounded uploads should get 202", i, statuses[i], ids[i], errs[i])
		}
		if _, exists := unique[ids[i]]; exists {
			t.Fatalf("distinct concurrent upload reused job ID %q", ids[i])
		}
		unique[ids[i]] = struct{}{}
	}
	var jobCount, outboxCount, attemptCount int64
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE owner_id=$1 AND idempotency_key LIKE 'bulk-%'`, f.first.Owner).Scan(&jobCount); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM outbox o JOIN jobs j USING(job_id) WHERE j.owner_id=$1 AND j.idempotency_key LIKE 'bulk-%'`, f.first.Owner).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM job_attempts a JOIN jobs j USING(job_id) WHERE j.owner_id=$1 AND j.idempotency_key LIKE 'bulk-%'`, f.first.Owner).Scan(&attemptCount); err != nil {
		t.Fatal(err)
	}
	if jobCount != count || outboxCount != count || attemptCount != 0 {
		t.Fatalf("100 bounded POSTs persisted jobs/outbox/attempts=%d/%d/%d, want 100/100/0", jobCount, outboxCount, attemptCount)
	}
	assertImageObjectCount(t, f.root, count+2)
	assertRepositoryRowCounts(t, ctx, f.pool, f.first.Owner, "http-concurrent-same", 1, 1, 0)
	assertRepositoryRowCounts(t, ctx, f.pool, f.first.Owner, "http-concurrent-changed", 1, 1, 0)
	// This proves bounded intake responses are not rejected by the OCR semaphore;
	// it does not measure image-size ceilings, OCR throughput, or production load.
}

func doConcurrentHTTP(requests []*http.Request) ([]int, []string, []error) {
	statuses := make([]int, len(requests))
	jobIDs := make([]string, len(requests))
	errs := make([]error, len(requests))
	var wait sync.WaitGroup
	for i := range requests {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			response, err := http.DefaultClient.Do(requests[index])
			if err != nil {
				errs[index] = err
				return
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				errs[index] = err
				return
			}
			statuses[index] = response.StatusCode
			if response.StatusCode == http.StatusAccepted {
				var payload acceptedPayload
				if err := json.Unmarshal(body, &payload); err != nil {
					errs[index] = err
					return
				}
				jobIDs[index] = payload.JobID
			}
		}(i)
	}
	wait.Wait()
	return statuses, jobIDs, errs
}

func TestPostgresHTTPAsyncIntakeQuotaAndCommitAckRetry(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	dsn := integrationDSN(t)
	data := []byte("exact capacity synthetic image")
	f := newIntegrationHTTPFixture(t, ctx, dsn, int64(len(data)), 1<<20)
	created, createdPayload, _ := f.post(t, f.first.Token, "quota-key", "image/jpeg", data)
	if created.StatusCode != http.StatusAccepted {
		t.Fatalf("first exact-capacity POST status=%d, want 202", created.StatusCode)
	}
	replay, replayPayload, _ := f.post(t, f.first.Token, "quota-key", "image/jpeg", data)
	if replay.StatusCode != http.StatusAccepted || replayPayload.JobID != createdPayload.JobID {
		t.Fatalf("same-request replay while image volume is full status=%d job=%q, want original 202", replay.StatusCode, replayPayload.JobID)
	}
	changedMIME, _, _ := f.post(t, f.first.Token, "quota-key", "image/png", data)
	if changedMIME.StatusCode != http.StatusConflict {
		t.Fatalf("same bytes with changed MIME at full volume status=%d, want idempotency 409", changedMIME.StatusCode)
	}
	newUpload, _, _ := f.post(t, f.first.Token, "new-at-capacity", "image/jpeg", []byte("another image"))
	if newUpload.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("new upload when real volume quota is full status=%d, want 503", newUpload.StatusCode)
	}
	assertRepositoryRowCounts(t, ctx, f.pool, f.first.Owner, "quota-key", 1, 1, 0)
	assertRepositoryRowCounts(t, ctx, f.pool, f.first.Owner, "new-at-capacity", 0, 0, 0)
	assertImageObjectCount(t, f.root, 1)

	ackFixture := newIntegrationHTTPFixture(t, ctx, dsn, 1<<20, 1<<20)
	// Simulate losing the HTTP service acknowledgment after the actual service
	// committed its PostgreSQL job/outbox transaction. Retrying through the
	// ordinary handler must return that original durable job without another file.
	commitLoss := &httpCommitAckLoss{service: ackFixture.images}
	configuredHandler, err := httpapi.NewHandler(httpapi.Config{
		MaxUploadBytes: 1 << 20, OCRTimeout: time.Second, MaxConcurrent: 4, PipelineVersion: "integration-only",
		AllowedClientNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		AsyncJobs:             &httpapi.AsyncJobsConfig{Authenticator: mustRegistry(t, ackFixture.first), Acceptor: commitLoss, MaxConcurrentUploads: 8},
	}, integrationRecognizer{})
	if err != nil {
		t.Fatal(err)
	}
	lossServer := httptest.NewServer(configuredHandler)
	t.Cleanup(lossServer.Close)
	firstFailure, _, _ := postUsingHandler(t, lossServer, ackFixture.first.Token, "ack-loss", "image/png", []byte("commit then lose app acknowledgment"))
	if firstFailure != http.StatusServiceUnavailable {
		t.Fatalf("first HTTP result after committed accept but dropped app acknowledgment=%d, want 503", firstFailure)
	}
	retry, retryPayload, _ := ackFixture.post(t, ackFixture.first.Token, "ack-loss", "image/png", []byte("commit then lose app acknowledgment"))
	if retry.StatusCode != http.StatusAccepted || retryPayload.JobID == "" {
		t.Fatalf("HTTP retry after committed acknowledgment loss status=%d payload=%+v, want original 202", retry.StatusCode, retryPayload)
	}
	assertRepositoryRowCounts(t, ctx, ackFixture.pool, ackFixture.first.Owner, "ack-loss", 1, 1, 0)
	assertImageObjectCount(t, ackFixture.root, 1)
}

func TestPostgresHTTPAsyncIntakeResponseLossCanReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	f := newIntegrationHTTPFixture(t, ctx, integrationDSN(t), 1<<20, 1<<20)
	const key = "response-loss"
	const image = "synthetic image accepted before response writer abort"
	dropper := httptest.NewServer(abortAcceptedResponse{next: f.handler})
	t.Cleanup(dropper.Close)
	request := multipartRequest(t, dropper.URL+"/v1/passport/jobs", f.first.Token, key, "image/png", []byte(image))
	response, err := dropper.Client().Do(request)
	if err == nil {
		body, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode == http.StatusAccepted && readErr == nil && len(body) > 0 {
			t.Fatalf("response-loss fault unexpectedly delivered complete 202 response")
		}
	}
	assertRepositoryRowCounts(t, ctx, f.pool, f.first.Owner, key, 1, 1, 0)
	var committedID string
	if err := f.pool.QueryRow(ctx, "SELECT job_id::text FROM jobs WHERE owner_id=$1 AND idempotency_key=$2", f.first.Owner, key).Scan(&committedID); err != nil {
		t.Fatalf("read job committed before response loss: %v", err)
	}
	assertImageObjectCount(t, f.root, 1)
	retry, payload, _ := f.post(t, f.first.Token, key, "image/png", []byte(image))
	if retry.StatusCode != http.StatusAccepted || payload.JobID != committedID {
		t.Fatalf("retry after lost HTTP response status=%d job=%q, want original job %q", retry.StatusCode, payload.JobID, committedID)
	}
	assertRepositoryRowCounts(t, ctx, f.pool, f.first.Owner, key, 1, 1, 0)
	assertImageObjectCount(t, f.root, 1)
}

type abortAcceptedResponse struct {
	next http.Handler
}

func (w abortAcceptedResponse) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	w.next.ServeHTTP(abortAcceptedWriter{ResponseWriter: response}, request)
}

type abortAcceptedWriter struct {
	http.ResponseWriter
}

func (w abortAcceptedWriter) WriteHeader(status int) {
	w.ResponseWriter.WriteHeader(status)
	if status == http.StatusAccepted {
		panic(http.ErrAbortHandler)
	}
}

type httpCommitAckLoss struct {
	service interface {
		Accept(context.Context, string, string, []byte, string) (jobs.Accepted, error)
	}
	mu   sync.Mutex
	used bool
}

func (a *httpCommitAckLoss) Accept(ctx context.Context, owner, key string, data []byte, media string) (jobs.Accepted, error) {
	accepted, err := a.service.Accept(ctx, owner, key, data, media)
	if err != nil {
		return jobs.Accepted{}, err
	}
	a.mu.Lock()
	lose := !a.used
	a.used = true
	a.mu.Unlock()
	if lose {
		return jobs.Accepted{}, jobs.ErrUnavailable
	}
	return accepted, nil
}

func mustRegistry(t *testing.T, credentials ...credentialFixture) *clientauth.Registry {
	t.Helper()
	path, err := os.MkdirTemp("/integration-images", "retry-credentials-")
	if err != nil {
		t.Fatalf("create private credentials for retry test: %v", err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(path) })
	items := make([]credentialJSON, 0, len(credentials))
	for _, item := range credentials {
		items = append(items, credentialJSON{item.Owner, hashForToken(item.Token)})
	}
	encoded, err := json.Marshal(struct {
		SchemaVersion string `json:"schema_version"`
		Credentials   any    `json:"credentials"`
	}{"passport-client-credentials/v1", items})
	if err != nil {
		t.Fatal(err)
	}
	registryFile := filepath.Join(path, "credentials.json")
	if err := os.WriteFile(registryFile, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := clientauth.Load(registryFile)
	if err != nil {
		t.Fatalf("load retry credential registry: %v", err)
	}
	return registry
}

func sendSyncRecognition(t *testing.T, server *httptest.Server, data []byte) int {
	t.Helper()
	request := multipartRequest(t, server.URL+"/v1/passport/recognize", "", "", "image/png", data)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("POST existing synchronous route: %v", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}

func postUsingHandler(t *testing.T, server *httptest.Server, token, key, media string, data []byte) (int, acceptedPayload, []byte) {
	t.Helper()
	request := multipartRequest(t, server.URL+"/v1/passport/jobs", token, key, media, data)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("POST through acknowledgment-loss handler: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read acknowledgment-loss HTTP response: %v", err)
	}
	var payload acceptedPayload
	if response.StatusCode == http.StatusAccepted {
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
	}
	return response.StatusCode, payload, body
}
