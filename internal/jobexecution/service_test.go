package jobexecution

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"passport-backend/internal/jobs"
)

type repositoryStub struct {
	claimResult  jobs.ClaimResult
	claimErr     error
	completion   jobs.PersistedCompletion
	completeErr  error
	failure      jobs.Failure
	failErr      error
	rejection    jobs.Rejection
	rejectErr    error
	recoverCount int
}

type terminalImagePurgerStub struct {
	jobIDs []string
	err    error
}

func (p *terminalImagePurgerStub) PurgeJobImage(_ context.Context, jobID string) error {
	p.jobIDs = append(p.jobIDs, jobID)
	return p.err
}
func (p *terminalImagePurgerStub) PurgeTerminalImages(context.Context, int) (int, error) {
	return 1, nil
}

func (r *repositoryStub) LeaseOutbox(context.Context, int, time.Duration) ([]jobs.LeasedOutbox, error) {
	return nil, nil
}
func (r *repositoryStub) MarkOutboxPublished(context.Context, string, string) error { return nil }
func (r *repositoryStub) ReleaseOutbox(context.Context, string, string, string, time.Duration) error {
	return nil
}
func (r *repositoryStub) ClaimDelivery(_ context.Context, _ jobs.Envelope, _ time.Duration, _ jobs.RetryPolicy) (jobs.ClaimResult, error) {
	return r.claimResult, r.claimErr
}
func (r *repositoryStub) PersistCompletion(_ context.Context, completion jobs.PersistedCompletion, _ jobs.RetryPolicy) (jobs.DeliveryDecision, error) {
	r.completion = completion
	return jobs.DecisionAck, r.completeErr
}
func (r *repositoryStub) FailAttempt(_ context.Context, failure jobs.Failure, _ jobs.RetryPolicy) (jobs.DeliveryDecision, error) {
	r.failure = failure
	return jobs.DecisionAck, r.failErr
}
func (r *repositoryStub) RejectDelivery(_ context.Context, rejection jobs.Rejection) (jobs.DeliveryDecision, error) {
	r.rejection = rejection
	return jobs.DecisionAck, r.rejectErr
}
func (r *repositoryStub) RecoverExpired(context.Context, int, jobs.RetryPolicy) (int, error) {
	return r.recoverCount, nil
}

func testConfig() Config {
	return Config{
		PipelineVersion: "worker-pipeline-v1", LeaseDuration: 11 * time.Minute,
		ProcessingTimeout: 8 * time.Minute, CompletionBudget: 2 * time.Minute,
		RetryPolicy: jobs.RetryPolicy{MaxAttempts: 3, InitialBackoff: time.Second, MaxBackoff: time.Minute},
	}
}

func TestNewRequiresLeaseToCoverProcessingAndCompletion(t *testing.T) {
	config := testConfig()
	if _, err := New(&repositoryStub{}, config); err != nil {
		t.Fatalf("New(valid config) error = %v", err)
	}
	config.LeaseDuration = 10 * time.Minute
	if _, err := New(&repositoryStub{}, config); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New(lease equal to timeout plus budget) error = %v, want ErrInvalidConfig", err)
	}
}

func TestWorkerCompletionDTOUsesFlatSnakeCaseEnvelope(t *testing.T) {
	completion := jobs.Completion{
		Envelope:     jobs.Envelope{SchemaVersion: 1, EventID: "a4f3706b-c1a9-49ae-9bf4-917ec20d80c8", JobID: "f2d5a4c8-4735-4242-8a70-ef42fb6db936", AttemptNo: 1},
		AttemptToken: "54219d20-9099-4a3f-a429-4b2bd5a8e581",
		Fields:       map[string]*string{"surname": ptr("ИВАНОВ")},
	}
	encoded, err := json.Marshal(completion)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"schema_version", "event_id", "job_id", "attempt_no", "attempt_token", "fields"} {
		if _, ok := value[key]; !ok {
			t.Errorf("serialized completion has no top-level %q: %s", key, encoded)
		}
	}
	if _, ok := value["Envelope"]; ok {
		t.Fatalf("serialized completion nests envelope unexpectedly: %s", encoded)
	}
}

func TestClaimAddsGoOwnedExecutionSettingsAndWaitDelay(t *testing.T) {
	repository := &repositoryStub{claimResult: jobs.ClaimResult{
		Decision: jobs.DecisionExecute,
		Execution: &jobs.Execution{
			AttemptToken: "opaque", ImageKey: "opaque-image", MediaType: "image/jpeg", Size: 10, AttemptNo: 1,
			StartedAt: time.Now(), LeaseExpiresAt: time.Now().Add(11 * time.Minute),
		},
	}}
	service, err := New(repository, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Claim(context.Background(), jobs.Envelope{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Execution.PipelineVersion != "worker-pipeline-v1" || result.Execution.HardProcessingTimeoutMS != 8*60*1000 ||
		result.Execution.CompletionBudgetMS != 2*60*1000 || !result.Execution.DeadlineAt.After(result.Execution.StartedAt) {
		t.Fatalf("execution settings = %#v", result.Execution)
	}

	repository.claimResult = jobs.ClaimResult{Decision: jobs.DecisionWait, RetryAfter: 1500 * time.Microsecond}
	result, err = service.Claim(context.Background(), jobs.Envelope{})
	if err != nil || result.RetryAfterMS != 2 {
		t.Fatalf("wait result = %#v, error = %v; want retry_after_ms 2", result, err)
	}
}

func TestCompleteRawUsesCanonicalResponseAndFailClosesMalformedFields(t *testing.T) {
	repository := &repositoryStub{}
	service, err := New(repository, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]*string{
		"surname": ptr("ИВАНОВ"), "given_name": ptr("АЛЕКСЕЙ"), "patronymic": nil,
		"gender": ptr("M"), "birth_date": ptr("1990-04-12"), "birth_place": ptr("МОСКВА"),
		"series": ptr("0012"), "number": ptr("000345"), "issue_date": ptr("2010-06-18"),
		"issuer": ptr("ОТДЕЛ ПРИМЕРА"), "department_code": ptr("123-456"),
	}
	envelope := jobs.Envelope{
		SchemaVersion: 1, EventID: "a4f3706b-c1a9-49ae-9bf4-917ec20d80c8",
		JobID: "f2d5a4c8-4735-4242-8a70-ef42fb6db936", AttemptNo: 1,
	}
	decision, err := service.CompleteRaw(context.Background(), jobs.Completion{
		Envelope: envelope, AttemptToken: "54219d20-9099-4a3f-a429-4b2bd5a8e581", Fields: fields,
	})
	if err != nil || decision != jobs.DecisionAck {
		t.Fatalf("CompleteRaw(valid) = %q, %v", decision, err)
	}
	var response struct {
		RequestID       string                     `json:"request_id"`
		Status          string                     `json:"status"`
		PipelineVersion string                     `json:"pipeline_version"`
		Fields          map[string]json.RawMessage `json:"fields"`
	}
	if err := json.Unmarshal(repository.completion.CanonicalJSON, &response); err != nil {
		t.Fatalf("persisted canonical JSON: %v", err)
	}
	if response.RequestID != envelope.JobID || response.Status != "completed" || response.PipelineVersion != "worker-pipeline-v1" || len(response.Fields) != 11 {
		t.Fatalf("persisted canonical response = %#v", response)
	}

	delete(fields, "issuer")
	decision, err = service.CompleteRaw(context.Background(), jobs.Completion{
		Envelope: envelope, AttemptToken: "54219d20-9099-4a3f-a429-4b2bd5a8e581", Fields: fields,
	})
	if err != nil || decision != jobs.DecisionAck || repository.failure.ErrorCode != "processing_failed" {
		t.Fatalf("CompleteRaw(invalid fields) = %q, %v; failure = %#v", decision, err, repository.failure)
	}
}

func TestTerminalCallbacksTriggerBestEffortImagePurgeAfterRepositoryCommit(t *testing.T) {
	repository := &repositoryStub{}
	purger := &terminalImagePurgerStub{}
	config := testConfig()
	config.TerminalImagePurger = purger
	service, err := New(repository, config)
	if err != nil {
		t.Fatal(err)
	}
	envelope := jobs.Envelope{SchemaVersion: 1, EventID: "event", JobID: "job", AttemptNo: 1}
	if decision, err := service.Fail(context.Background(), jobs.Failure{Envelope: envelope}); err != nil || decision != jobs.DecisionAck {
		t.Fatalf("Fail()=(%q,%v), want ACK", decision, err)
	}
	if len(purger.jobIDs) != 1 || purger.jobIDs[0] != "job" {
		t.Fatalf("purge calls = %v, want one job purge", purger.jobIDs)
	}
	repository.failErr = jobs.ErrUnavailable
	if _, err := service.Fail(context.Background(), jobs.Failure{Envelope: envelope}); err == nil {
		t.Fatal("Fail() hid repository failure")
	}
	if len(purger.jobIDs) != 1 {
		t.Fatalf("purge ran before durable terminal decision: %v", purger.jobIDs)
	}
}

func TestCompleteRawPreservesSafeConflictAcknowledgement(t *testing.T) {
	repository := &repositoryStub{completeErr: jobs.ErrConflictingCompletion}
	service, err := New(repository, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]*string{
		"surname": nil, "given_name": nil, "patronymic": nil, "gender": nil,
		"birth_date": nil, "birth_place": nil, "series": nil, "number": nil,
		"issue_date": nil, "issuer": nil, "department_code": nil,
	}
	decision, err := service.CompleteRaw(context.Background(), jobs.Completion{
		Envelope:     jobs.Envelope{SchemaVersion: 1, EventID: "a4f3706b-c1a9-49ae-9bf4-917ec20d80c8", JobID: "f2d5a4c8-4735-4242-8a70-ef42fb6db936", AttemptNo: 1},
		AttemptToken: "54219d20-9099-4a3f-a429-4b2bd5a8e581", Fields: fields,
	})
	if decision != jobs.DecisionAck || !errors.Is(err, jobs.ErrConflictingCompletion) {
		t.Fatalf("CompleteRaw(conflicting duplicate) = %q, %v", decision, err)
	}
}

func ptr(value string) *string { return &value }
