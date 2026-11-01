package workerapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"passport-backend/internal/clientauth"
	"passport-backend/internal/jobs"
)

type authStub struct {
	err   error
	calls int
}

func (a *authStub) AuthenticateWorker([]string) (clientauth.WorkerPrincipal, error) {
	a.calls++
	if a.err != nil {
		return clientauth.WorkerPrincipal{}, a.err
	}
	return clientauth.WorkerPrincipal{WorkerID: "8d2fae9c-efb7-4e98-8a90-f10b00edbdf2"}, nil
}

type executorStub struct {
	claimResult jobs.ClaimResult
	claimErr    error
	decision    jobs.DeliveryDecision
	callErr     error
	claimed     jobs.Envelope
	completed   jobs.Completion
	failed      jobs.Failure
	rejected    jobs.Rejection
}

func (e *executorStub) Claim(_ context.Context, envelope jobs.Envelope) (jobs.ClaimResult, error) {
	e.claimed = envelope
	return e.claimResult, e.claimErr
}

func (e *executorStub) CompleteRaw(_ context.Context, completion jobs.Completion) (jobs.DeliveryDecision, error) {
	e.completed = completion
	return e.decision, e.callErr
}

func (e *executorStub) Fail(_ context.Context, failure jobs.Failure) (jobs.DeliveryDecision, error) {
	e.failed = failure
	return e.decision, e.callErr
}

func (e *executorStub) Reject(_ context.Context, rejection jobs.Rejection) (jobs.DeliveryDecision, error) {
	e.rejected = rejection
	return e.decision, e.callErr
}

func newTestHandler(t *testing.T, auth Authenticator, executor Executor) http.Handler {
	t.Helper()
	handler, err := NewHandler(Config{Authenticator: auth, Executor: executor, WaitPollInterval: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewHandler() error=%v", err)
	}
	return handler
}

func workerRequest(path, body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer pwt1_fake")
	return request
}

func TestWorkerClaimIsAuthenticatedAndReturnsGoDecision(t *testing.T) {
	auth := &authStub{}
	executor := &executorStub{claimResult: jobs.ClaimResult{Decision: jobs.DecisionWait, RetryAfterMS: 250}}
	handler := newTestHandler(t, auth, executor)
	request := workerRequest(claimPath, `{"schema_version":1,"event_id":"9cfe2e2f-135d-42f4-8db5-ddabc5a2f831","job_id":"8d2fae9c-efb7-4e98-8a90-f10b00edbdf2","attempt_no":1}`)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || auth.calls != 1 || executor.claimed.AttemptNo != 1 {
		t.Fatalf("status=%d auth_calls=%d envelope=%+v body=%s", recorder.Code, auth.calls, executor.claimed, recorder.Body.String())
	}
	var result jobs.ClaimResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Decision != jobs.DecisionWait || result.RetryAfterMS != 250 {
		t.Fatalf("claim result=%+v", result)
	}
}

func TestWorkerAuthenticationHappensBeforeBodyRead(t *testing.T) {
	auth := &authStub{err: clientauth.ErrUnauthorized}
	executor := &executorStub{}
	handler := newTestHandler(t, auth, executor)
	request := httptest.NewRequest(http.MethodPost, claimPath, strings.NewReader("not json"))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized || recorder.Header().Get("WWW-Authenticate") == "" || executor.claimed != (jobs.Envelope{}) {
		t.Fatalf("status=%d auth=%v claimed=%+v body=%s", recorder.Code, recorder.Header(), executor.claimed, recorder.Body.String())
	}
}

func TestWorkerStrictJSONRejectsDuplicateAndUnknownKeys(t *testing.T) {
	for _, body := range []string{
		`{"schema_version":1,"schema_version":1,"event_id":"9cfe2e2f-135d-42f4-8db5-ddabc5a2f831","job_id":"8d2fae9c-efb7-4e98-8a90-f10b00edbdf2","attempt_no":1}`,
		`{"schema_version":1,"event_id":"9cfe2e2f-135d-42f4-8db5-ddabc5a2f831","job_id":"8d2fae9c-efb7-4e98-8a90-f10b00edbdf2","attempt_no":1,"owner_id":"private"}`,
	} {
		executor := &executorStub{}
		handler := newTestHandler(t, &authStub{}, executor)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, workerRequest(claimPath, body))
		if recorder.Code != http.StatusBadRequest || executor.claimed != (jobs.Envelope{}) {
			t.Fatalf("body accepted: status=%d claimed=%+v body=%s", recorder.Code, executor.claimed, recorder.Body.String())
		}
	}
}

func TestWorkerCompleteOnlyACKsDurableDecisionAndSuppressesConflictingPayload(t *testing.T) {
	auth := &authStub{}
	executor := &executorStub{decision: jobs.DecisionAck}
	handler := newTestHandler(t, auth, executor)
	body := `{"schema_version":1,"event_id":"9cfe2e2f-135d-42f4-8db5-ddabc5a2f831","job_id":"8d2fae9c-efb7-4e98-8a90-f10b00edbdf2","attempt_no":1,"attempt_token":"22222222-2222-4222-8222-222222222222","fields":{"surname":"PRIVATE-DOCUMENT-SENTINEL"}}`
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, workerRequest(completePath, body))
	if recorder.Code != http.StatusOK || executor.completed.Envelope.AttemptNo != 1 || executor.completed.Fields["surname"] == nil {
		t.Fatalf("status=%d completion=%+v body=%s", recorder.Code, executor.completed, recorder.Body.String())
	}

	executor.callErr = jobs.ErrConflictingCompletion
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, workerRequest(completePath, body))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"decision":"ack"`) || strings.Contains(recorder.Body.String(), "PRIVATE-DOCUMENT-SENTINEL") {
		t.Fatalf("conflicting completion response leaked or failed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestWorkerUnavailableDoesNotReturnACK(t *testing.T) {
	executor := &executorStub{decision: jobs.DecisionAck, callErr: jobs.ErrUnavailable}
	handler := newTestHandler(t, &authStub{}, executor)
	body := `{"schema_version":1,"event_id":"9cfe2e2f-135d-42f4-8db5-ddabc5a2f831","job_id":"8d2fae9c-efb7-4e98-8a90-f10b00edbdf2","attempt_no":1,"attempt_token":"22222222-2222-4222-8222-222222222222","error_code":"processing_failed"}`
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, workerRequest(failPath, body))
	if recorder.Code != http.StatusServiceUnavailable || strings.Contains(recorder.Body.String(), `"decision":"ack"`) {
		t.Fatalf("unavailable callback response status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestWorkerCallbackWaitKeepsDeliveryUnacknowledged(t *testing.T) {
	executor := &executorStub{decision: jobs.DecisionWait}
	handler := newTestHandler(t, &authStub{}, executor)
	body := `{"schema_version":1,"event_id":"9cfe2e2f-135d-42f4-8db5-ddabc5a2f831","job_id":"8d2fae9c-efb7-4e98-8a90-f10b00edbdf2","attempt_no":1}`
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, workerRequest(rejectPath, body))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"decision":"wait"`) || !strings.Contains(recorder.Body.String(), `"retry_after_ms":100`) {
		t.Fatalf("wait callback response status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestWorkerRejectAcceptsKnownPoisonSchemaAndUsesFixedServiceOperation(t *testing.T) {
	executor := &executorStub{decision: jobs.DecisionAck}
	handler := newTestHandler(t, &authStub{}, executor)
	body := `{"schema_version":2,"event_id":"9cfe2e2f-135d-42f4-8db5-ddabc5a2f831","job_id":"8d2fae9c-efb7-4e98-8a90-f10b00edbdf2","attempt_no":1}`
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, workerRequest(rejectPath, body))
	if recorder.Code != http.StatusOK || executor.rejected.Envelope.SchemaVersion != 2 || !strings.Contains(recorder.Body.String(), `"decision":"ack"`) {
		t.Fatalf("reject response status=%d rejected=%+v body=%s", recorder.Code, executor.rejected, recorder.Body.String())
	}
}

func TestWorkerClaimExecuteRequiresFencedDeadlineMetadata(t *testing.T) {
	now := time.Now().UTC()
	execution := &jobs.Execution{
		AttemptToken: "22222222-2222-4222-8222-222222222222", ImageKey: "33333333-3333-4333-8333-333333333333",
		MediaType: "image/webp", Size: 10, AttemptNo: 1, DeadlineAt: now.Add(time.Minute),
		LeaseExpiresAt: now.Add(2 * time.Minute), PipelineVersion: "pipeline-v1",
		HardProcessingTimeoutMS: 30_000, CompletionBudgetMS: 30_000,
	}
	executor := &executorStub{claimResult: jobs.ClaimResult{Decision: jobs.DecisionExecute, Execution: execution}}
	handler := newTestHandler(t, &authStub{}, executor)
	body := `{"schema_version":1,"event_id":"9cfe2e2f-135d-42f4-8db5-ddabc5a2f831","job_id":"8d2fae9c-efb7-4e98-8a90-f10b00edbdf2","attempt_no":1}`
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, workerRequest(claimPath, body))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"hard_processing_timeout_ms":30000`) || !strings.Contains(recorder.Body.String(), `"completion_budget_ms":30000`) {
		t.Fatalf("execute claim status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	executor.claimResult.Execution.LeaseExpiresAt = now.Add(time.Minute + 30*time.Second)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, workerRequest(claimPath, body))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("invalid lease claim status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
