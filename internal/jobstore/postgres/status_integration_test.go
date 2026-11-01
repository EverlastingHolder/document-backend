//go:build integration

package postgres_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"passport-backend/internal/api"
	"passport-backend/internal/clientauth"
	"passport-backend/internal/httpapi"
	"passport-backend/internal/jobexecution"
	"passport-backend/internal/jobs"
	"passport-backend/internal/jobstatus"
	"passport-backend/internal/jobstore/postgres"
)

const (
	statusAPIURLEnv       = "PASSPORT_POSTGRES_TEST_API_URL"
	statusOwnerIDsEnv     = "PASSPORT_POSTGRES_TEST_CLIENT_OWNER_IDS_FILE"
	statusSecondTokenEnv  = "PASSPORT_POSTGRES_TEST_SECOND_CLIENT_TOKEN_FILE"
	statusWorkerTokenEnv  = "PASSPORT_POSTGRES_TEST_WORKER_TOKEN_FILE"
	statusResponseFileEnv = "PASSPORT_POSTGRES_TEST_STATUS_RESPONSE_FILE"
)

func TestPostgresHTTPJobStatusOwnerIsolationAndFourStoredStates(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	dsn := integrationDSN(t)
	store := openIntegrationStore(t, ctx, dsn)
	pool := openTestPool(t, ctx, dsn)
	owners := readTwoClientOwners(t)
	firstToken := readPrivateToken(t, testClientTokenFileEnv, "pbt1_")
	secondToken := readPrivateToken(t, statusSecondTokenEnv, "pbt1_")
	workerToken := readPrivateToken(t, statusWorkerTokenEnv, "pwt1_")

	execution, err := jobexecution.New(store, jobexecution.Config{
		PipelineVersion: "status-fixture-old-pipeline-v1", LeaseDuration: 2 * time.Hour,
		ProcessingTimeout: time.Hour, CompletionBudget: 30 * time.Minute,
		RetryPolicy: jobs.RetryPolicy{MaxAttempts: 2, InitialBackoff: time.Second, MaxBackoff: time.Second},
	})
	if err != nil {
		t.Fatalf("construct status fixture lifecycle service: %v", err)
	}

	fixtures := make(map[string]jobs.Submission, 5)
	create := func(state string) jobs.Submission {
		t.Helper()
		digest := sha256.Sum256([]byte("stage5 status synthetic fixture " + state + " " + randomTestUUID()))
		submission := newSubmission(owners[0], "stage5-status-"+state+"-"+randomTestUUID(), digest)
		if _, outcome, err := store.CreateQueued(ctx, submission); err != nil || outcome != jobs.OutcomeCreated {
			t.Fatalf("create %s status fixture: outcome=%v err=%v", state, outcome, err)
		}
		fixtures[state] = submission
		return submission
	}

	queued := create("queued")
	queuedRetry := create("queued-retry")
	queuedRetryEnvelope := envelopeFor(queuedRetry)
	queuedRetryClaim, err := execution.Claim(ctx, queuedRetryEnvelope)
	if err != nil || queuedRetryClaim.Execution == nil {
		t.Fatalf("claim retry-scheduled queued status fixture: execution=%t err=%v", queuedRetryClaim.Execution != nil, err)
	}
	if decision, err := execution.Fail(ctx, jobs.Failure{
		Envelope: queuedRetryEnvelope, AttemptToken: queuedRetryClaim.Execution.AttemptToken, ErrorCode: "processing_failed",
	}); err != nil || decision != jobs.DecisionAck {
		t.Fatalf("schedule retry-scheduled queued status fixture: decision=%q err=%v", decision, err)
	}
	processing := create("processing")
	processingEnvelope := envelopeFor(processing)
	if claim, err := execution.Claim(ctx, processingEnvelope); err != nil || claim.Execution == nil {
		t.Fatalf("claim processing status fixture: execution=%t err=%v", claim.Execution != nil, err)
	}

	completed := create("completed")
	completedEnvelope := envelopeFor(completed)
	completedClaim, err := execution.Claim(ctx, completedEnvelope)
	if err != nil || completedClaim.Execution == nil {
		t.Fatalf("claim completed status fixture: execution=%t err=%v", completedClaim.Execution != nil, err)
	}
	fields := map[string]*string{
		"surname": nil, "given_name": nil, "patronymic": nil, "gender": nil,
		"birth_date": nil, "birth_place": nil, "series": nil, "number": nil,
		"issue_date": nil, "issuer": nil, "department_code": nil,
	}
	if decision, err := execution.CompleteRaw(ctx, jobs.Completion{
		Envelope: completedEnvelope, AttemptToken: completedClaim.Execution.AttemptToken, Fields: fields,
	}); err != nil || decision != jobs.DecisionAck {
		t.Fatalf("persist completed status fixture: decision=%q err=%v", decision, err)
	}

	failed := create("failed")
	failedEnvelope := envelopeFor(failed)
	failedClaim, err := execution.Claim(ctx, failedEnvelope)
	if err != nil || failedClaim.Execution == nil {
		t.Fatalf("claim failed status fixture: execution=%t err=%v", failedClaim.Execution != nil, err)
	}
	if decision, err := execution.Fail(ctx, jobs.Failure{
		Envelope: failedEnvelope, AttemptToken: failedClaim.Execution.AttemptToken, ErrorCode: "image_unprocessable",
	}); err != nil || decision != jobs.DecisionAck {
		t.Fatalf("persist failed status fixture: decision=%q err=%v", decision, err)
	}

	baseURL := os.Getenv(statusAPIURLEnv)
	if baseURL != "http://api:8080" {
		t.Fatal("status integration must call the actual Compose API service")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	responseCapture := make(map[string]json.RawMessage, 4)
	requestID := "1c7b7782-e4f5-47a8-a2b6-2d264b792086"

	before := make(map[string]statusPersistenceSnapshot, len(fixtures))
	for state, submission := range fixtures {
		before[state] = readStatusPersistenceSnapshot(t, ctx, pool, submission.JobID)
	}

	for _, state := range []string{"queued", "queued-retry", "processing", "completed", "failed"} {
		submission := fixtures[state]
		pathJobID := submission.JobID
		if state == "queued" {
			pathJobID = strings.ToUpper(pathJobID)
		}
		status, header, body := doStatusRequest(t, ctx, client, baseURL, http.MethodGet, pathJobID, firstToken, requestID)
		if status != http.StatusOK {
			t.Fatalf("GET %s job status=%d, want 200", state, status)
		}
		assertNoStoreAndRequestID(t, header, requestID)
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("decode %s job status JSON: %v", state, err)
		}
		wantKeys := []string{"job_id", "status", "created_at", "updated_at", "started_at", "finished_at"}
		if state == "completed" || state == "failed" {
			wantKeys = append(wantKeys, map[string]string{"completed": "result", "failed": "error"}[state])
		}
		if !sameJSONKeys(payload, wantKeys) {
			t.Fatalf("%s job status exposed unexpected or missing fields", state)
		}
		var gotJobID, gotState string
		if err := json.Unmarshal(payload["job_id"], &gotJobID); err != nil {
			t.Fatalf("decode %s job_id: %v", state, err)
		}
		wantState := state
		if state == "queued-retry" {
			wantState = "queued"
		}
		if err := json.Unmarshal(payload["status"], &gotState); err != nil || gotJobID != submission.JobID || gotState != wantState {
			t.Fatalf("%s status identity/state mismatch", state)
		}
		assertStatusTimesMatchRecord(t, ctx, store, owners[0], submission.JobID, payload)
		if state == "queued-retry" {
			var startedAt time.Time
			if json.Unmarshal(payload["started_at"], &startedAt) != nil || payload["finished_at"] == nil || string(payload["finished_at"]) != "null" {
				t.Fatal("retry-scheduled queued status did not preserve started_at with null finished_at")
			}
		}
		if state == "completed" {
			var result api.RecognitionResponse
			if err := json.Unmarshal(payload["result"], &result); err != nil {
				t.Fatalf("decode completed canonical response: %v", err)
			}
			if !strings.EqualFold(result.RequestID, submission.JobID) || result.Status != "completed" ||
				result.PipelineVersion != "status-fixture-old-pipeline-v1" {
				t.Fatal("completed GET remapped or changed the persisted canonical result/pipeline version")
			}
			stored, err := store.GetOwnedStatus(ctx, owners[0], submission.JobID)
			if err != nil || !sameJSONValue(t, stored.ResultJSON, payload["result"]) {
				t.Fatal("completed GET result differs from the persisted canonical JSON")
			}
		}
		if state == "failed" {
			var publicError struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(payload["error"], &publicError); err != nil ||
				publicError.Code != "image_unprocessable" || strings.TrimSpace(publicError.Message) == "" {
				t.Fatal("failed GET did not return a stable safe public error")
			}
		}
		responseCapture[state] = append(json.RawMessage(nil), body...)
	}

	// A public client token must not cross over to the worker-only credential
	// purpose. Missing, invalid, and worker tokens all fail before status reads.
	for name, token := range map[string]string{
		"missing": "", "invalid": "pbt1_invalid-stage5-token", "worker": workerToken,
	} {
		status, header, _ := doStatusRequest(t, ctx, client, baseURL, http.MethodGet, queued.JobID, token, requestID)
		if status != http.StatusUnauthorized {
			t.Fatalf("%s credential GET status=%d, want 401", name, status)
		}
		assertNoStoreAndRequestID(t, header, requestID)
	}

	foreignStatus, foreignHeader, foreignBody := doStatusRequest(t, ctx, client, baseURL, http.MethodGet, completed.JobID, secondToken, requestID)
	missingID := randomTestUUID()
	missingStatus, missingHeader, missingBody := doStatusRequest(t, ctx, client, baseURL, http.MethodGet, missingID, firstToken, requestID)
	malformedStatus, malformedHeader, malformedBody := doStatusRequest(t, ctx, client, baseURL, http.MethodGet, "not-a-uuid", firstToken, requestID)
	if foreignStatus != http.StatusNotFound || missingStatus != http.StatusNotFound || malformedStatus != http.StatusNotFound ||
		!bytes.Equal(foreignBody, missingBody) || !bytes.Equal(malformedBody, missingBody) {
		t.Fatal("foreign, missing, and malformed IDs did not return the same 404 response")
	}
	assertNoStoreAndRequestID(t, foreignHeader, requestID)
	assertNoStoreAndRequestID(t, missingHeader, requestID)
	assertNoStoreAndRequestID(t, malformedHeader, requestID)

	methodStatus, methodHeader, _ := doStatusRequest(t, ctx, client, baseURL, http.MethodDelete, completed.JobID, firstToken, requestID)
	if methodStatus != http.StatusMethodNotAllowed || methodHeader.Get("Allow") != http.MethodGet {
		t.Fatal("unsupported method on a single-job route did not return 405 Allow: GET")
	}
	assertNoStoreAndRequestID(t, methodHeader, requestID)
	partialStatus, _, _ := doStatusRequest(t, ctx, client, baseURL, http.MethodGet, completed.JobID+"/nested", firstToken, requestID)
	if partialStatus != http.StatusNotFound {
		t.Fatalf("nested/trailing status path=%d, want 404", partialStatus)
	}
	collectionStatus, collectionHeader, _ := doStatusRequest(t, ctx, client, baseURL, http.MethodGet, "", firstToken, requestID)
	if collectionStatus != http.StatusMethodNotAllowed || collectionHeader.Get("Allow") != http.MethodPost {
		t.Fatal("jobs collection GET changed the existing POST-only collection behavior")
	}

	// A syntactically valid JSON object in a completed row can still be corrupt
	// as a canonical result. It must fail closed without echoing stored bytes.
	var originalResult []byte
	if err := pool.QueryRow(ctx, `SELECT result::text FROM jobs WHERE job_id=$1`, completed.JobID).Scan(&originalResult); err != nil {
		t.Fatal("read canonical result before corruption probe")
	}
	const corruptSentinel = "stage5-corrupt-result-private-sentinel"
	corruptJSON, _ := json.Marshal(map[string]string{"private_marker": corruptSentinel})
	if _, err := pool.Exec(ctx, `UPDATE jobs SET result=$2::jsonb WHERE job_id=$1`, completed.JobID, string(corruptJSON)); err != nil {
		t.Fatalf("install syntactically valid corrupt result fixture: %v", err)
	}
	status, header, body := doStatusRequest(t, ctx, client, baseURL, http.MethodGet, completed.JobID, firstToken, requestID)
	if _, err := pool.Exec(ctx, `UPDATE jobs SET result=$2::jsonb WHERE job_id=$1`, completed.JobID, string(originalResult)); err != nil {
		t.Fatalf("restore canonical result fixture: %v", err)
	}
	if status != http.StatusServiceUnavailable || bytes.Contains(body, []byte(corruptSentinel)) {
		t.Fatal("corrupt canonical result was exposed instead of a safe 503")
	}
	assertNoStoreAndRequestID(t, header, requestID)
	var errorPayload struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(body, &errorPayload) != nil || errorPayload.Code != "job_status_unavailable" {
		t.Fatal("corrupt canonical result did not map to the fixed status-unavailable response")
	}

	for state, submission := range fixtures {
		after := readStatusPersistenceSnapshot(t, ctx, pool, submission.JobID)
		if !reflect.DeepEqual(before[state], after) {
			t.Fatalf("repeated GET changed persisted %s state, attempts, result, or outbox row count", state)
		}
	}
	responseFile := os.Getenv(statusResponseFileEnv)
	if responseFile == "" {
		t.Fatal("status schema validation output path unavailable")
	}
	encoded, err := json.Marshal(responseCapture)
	if err != nil || os.WriteFile(responseFile, encoded, 0o600) != nil {
		t.Fatal("write synthetic GET JSON for OpenAPI oneOf validation")
	}
}

func TestPostgresHTTPJobStatusRejectsAuthBeforeRepositoryRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	dsn := integrationDSN(t)
	intake, store, _, _, _ := newIntegrationIntake(t, ctx, dsn, 1<<20)
	credential := newCredentialFixture(t)
	registryPath := filepath.Join(t.TempDir(), "client-credentials.json")
	if err := os.Chmod(filepath.Dir(registryPath), 0o700); err != nil {
		t.Fatal("protect status auth test registry directory")
	}
	registryJSON, err := json.Marshal(struct {
		SchemaVersion string           `json:"schema_version"`
		Credentials   []credentialJSON `json:"credentials"`
	}{"passport-client-credentials/v1", []credentialJSON{{OwnerID: credential.Owner, TokenSHA256: tokenHash(t, credential.Token)}}})
	if err != nil || os.WriteFile(registryPath, registryJSON, 0o600) != nil || os.Chmod(registryPath, 0o600) != nil {
		t.Fatal("write private status auth test registry")
	}
	registry, err := clientauth.Load(registryPath)
	if err != nil {
		t.Fatal("load private status auth test registry")
	}
	counted := &countingStatusRepository{StatusRepository: store}
	handler, err := httpapi.NewHandler(httpapi.Config{
		MaxUploadBytes: 1 << 20, OCRTimeout: time.Second, MaxConcurrent: 1,
		PipelineVersion: "status-auth-test", AllowedClientNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		AsyncJobs: &httpapi.AsyncJobsConfig{
			Authenticator: registry, Acceptor: intake, StatusReader: jobstatus.New(counted),
			StatusReadTimeout: time.Second, MaxConcurrentUploads: 1,
		},
	}, integrationRecognizer{})
	if err != nil {
		t.Fatalf("construct status auth-before-read handler: %v", err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	for _, token := range []string{"", "pbt1_invalid-stage5-token", "pwt1_wrong-purpose-token"} {
		request, err := http.NewRequest(http.MethodGet, server.URL+"/v1/passport/jobs/not-a-uuid", nil)
		if err != nil {
			t.Fatal("construct status auth-before-read request")
		}
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		request.Header.Set("X-Request-ID", "934c7930-e6c6-4fdb-85ae-64a347e95274")
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal("send local status auth probe")
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unauthorized status request returned %d, want 401", response.StatusCode)
		}
	}
	if counted.calls.Load() != 0 {
		t.Fatal("status repository was consulted before client authentication")
	}
}

func TestPostgresHTTPJobStatusUnavailableWithoutReader(t *testing.T) {
	credential := newCredentialFixture(t)
	registryPath := filepath.Join(t.TempDir(), "client-credentials.json")
	if err := os.Chmod(filepath.Dir(registryPath), 0o700); err != nil {
		t.Fatal("protect disabled status registry directory")
	}
	registryJSON, err := json.Marshal(struct {
		SchemaVersion string           `json:"schema_version"`
		Credentials   []credentialJSON `json:"credentials"`
	}{"passport-client-credentials/v1", []credentialJSON{{OwnerID: credential.Owner, TokenSHA256: tokenHash(t, credential.Token)}}})
	if err != nil || os.WriteFile(registryPath, registryJSON, 0o600) != nil || os.Chmod(registryPath, 0o600) != nil {
		t.Fatal("write private disabled status registry")
	}
	registry, err := clientauth.Load(registryPath)
	if err != nil {
		t.Fatalf("load private disabled status registry: %v", err)
	}
	handler, err := httpapi.NewHandler(httpapi.Config{
		MaxUploadBytes: 1 << 20, OCRTimeout: time.Second, MaxConcurrent: 1,
		PipelineVersion: "status-disabled-test", AllowedClientNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		AsyncJobs: &httpapi.AsyncJobsConfig{Authenticator: registry, Acceptor: statusNoopAcceptor{}, MaxConcurrentUploads: 1},
	}, integrationRecognizer{})
	if err != nil {
		t.Fatalf("construct disabled status handler: %v", err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	request, err := http.NewRequest(http.MethodGet, server.URL+"/v1/passport/jobs/"+randomTestUUID(), nil)
	if err != nil {
		t.Fatal("construct disabled status request")
	}
	request.Header.Set("Authorization", "Bearer "+credential.Token)
	request.Header.Set("X-Request-ID", "1fe9c042-541b-41f6-87d3-d4f7a3d85ff6")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal("send disabled status request")
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	assertNoStoreAndRequestID(t, response.Header, "1fe9c042-541b-41f6-87d3-d4f7a3d85ff6")
	var errorPayload struct {
		Code string `json:"code"`
	}
	if response.StatusCode != http.StatusServiceUnavailable || json.Unmarshal(body, &errorPayload) != nil || errorPayload.Code != "job_status_unavailable" {
		t.Fatal("GET without a configured status reader did not fail closed with 503")
	}
}

type statusNoopAcceptor struct{}

func (statusNoopAcceptor) Accept(context.Context, string, string, []byte, string) (jobs.Accepted, error) {
	return jobs.Accepted{}, jobs.ErrUnavailable
}

type countingStatusRepository struct {
	jobs.StatusRepository
	calls atomic.Int64
}

func (r *countingStatusRepository) GetOwnedStatus(ctx context.Context, ownerID, jobID string) (jobs.StatusRecord, error) {
	r.calls.Add(1)
	return r.StatusRepository.GetOwnedStatus(ctx, ownerID, jobID)
}

type statusPersistenceSnapshot struct {
	State       string
	UpdatedAt   time.Time
	AttemptRows int
	OutboxRows  int
	ResultJSON  string
}

func readStatusPersistenceSnapshot(t *testing.T, ctx context.Context, pool *pgxpool.Pool, jobID string) statusPersistenceSnapshot {
	t.Helper()
	var snapshot statusPersistenceSnapshot
	var result []byte
	if err := pool.QueryRow(ctx, `SELECT state, updated_at, result FROM jobs WHERE job_id=$1`, jobID).
		Scan(&snapshot.State, &snapshot.UpdatedAt, &result); err != nil {
		t.Fatal("read status persistence snapshot")
	}
	snapshot.ResultJSON = string(result)
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM job_attempts WHERE job_id=$1`, jobID).Scan(&snapshot.AttemptRows); err != nil {
		t.Fatal("count status fixture attempts")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE job_id=$1`, jobID).Scan(&snapshot.OutboxRows); err != nil {
		t.Fatal("count status fixture outbox rows")
	}
	return snapshot
}

func assertStatusTimesMatchRecord(t *testing.T, ctx context.Context, store *postgres.Store, ownerID, jobID string, payload map[string]json.RawMessage) {
	t.Helper()
	stored, err := store.GetOwnedStatus(ctx, ownerID, jobID)
	if err != nil {
		t.Fatal("read status fixture timestamp record")
	}
	var createdAt, updatedAt time.Time
	if json.Unmarshal(payload["created_at"], &createdAt) != nil || json.Unmarshal(payload["updated_at"], &updatedAt) != nil ||
		!createdAt.Equal(stored.CreatedAt) || !updatedAt.Equal(stored.UpdatedAt) {
		t.Fatal("GET status timestamps differ from PostgreSQL")
	}
	var startedAt, finishedAt *time.Time
	if json.Unmarshal(payload["started_at"], &startedAt) != nil || json.Unmarshal(payload["finished_at"], &finishedAt) != nil ||
		!equalOptionalTime(startedAt, stored.StartedAt) || !equalOptionalTime(finishedAt, stored.FinishedAt) {
		t.Fatal("GET start/finish timestamps differ from PostgreSQL")
	}
}

func equalOptionalTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func sameJSONKeys(value map[string]json.RawMessage, keys []string) bool {
	if len(value) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, ok := value[key]; !ok {
			return false
		}
	}
	return true
}

func sameJSONValue(t *testing.T, left, right []byte) bool {
	t.Helper()
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

func assertNoStoreAndRequestID(t *testing.T, headers http.Header, requestID string) {
	t.Helper()
	if headers.Get("Cache-Control") != "no-store" || headers.Get("X-Request-ID") != requestID {
		t.Fatal("job status response did not include no-store and the request ID")
	}
}

func readTwoClientOwners(t *testing.T) []string {
	t.Helper()
	path := os.Getenv(statusOwnerIDsEnv)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("private integration owner fixture unavailable")
	}
	var owners []string
	if json.Unmarshal(data, &owners) != nil || len(owners) != 2 || owners[0] == "" || owners[1] == "" || owners[0] == owners[1] {
		t.Fatal("private integration owner fixture invalid")
	}
	return owners
}

func readPrivateToken(t *testing.T, envName, prefix string) string {
	t.Helper()
	path := os.Getenv(envName)
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		t.Fatal("private integration credential unavailable")
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) < len(prefix)+32 || !strings.HasPrefix(string(data), prefix) {
		t.Fatal("private integration credential unavailable")
	}
	return string(data)
}

func doStatusRequest(t *testing.T, ctx context.Context, client *http.Client, baseURL, method, jobID, token, requestID string) (int, http.Header, []byte) {
	t.Helper()
	path := "/v1/passport/jobs"
	if jobID != "" {
		path += "/" + jobID
	}
	request, err := http.NewRequestWithContext(ctx, method, baseURL+path, nil)
	if err != nil {
		t.Fatalf("construct status request: %v", err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	request.Header.Set("X-Request-ID", requestID)
	var response *http.Response
	for attempt := 0; attempt < 30; attempt++ {
		response, err = client.Do(request)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("status request transport failed: %T", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err != nil || response == nil {
		t.Fatalf("status API remained unavailable after bounded retry (%T)", err)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	_ = response.Body.Close()
	if err != nil {
		t.Fatalf("read status response body: %v", err)
	}
	return response.StatusCode, response.Header, body
}

func envelopeFor(submission jobs.Submission) jobs.Envelope {
	return jobs.Envelope{SchemaVersion: 1, EventID: submission.EventID, JobID: submission.JobID, AttemptNo: 1}
}
