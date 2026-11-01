//go:build integration

package postgres_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"passport-backend/internal/jobexecution"
	"passport-backend/internal/jobs"
	"passport-backend/internal/jobstore/postgres"
)

func TestPostgresExecutionLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	dsn := integrationDSN(t)
	store := openIntegrationStore(t, ctx, dsn)
	pool := openTestPool(t, ctx, dsn)

	t.Run("claim and completion idempotence with conflict fencing", func(t *testing.T) {
		service := newLifecycleService(t, store, 3)
		submission := createLifecycleJob(t, ctx, store)
		envelope := lifecycleEnvelope(submission, 1)

		first, err := service.Claim(ctx, envelope)
		if err != nil || first.Decision != jobs.DecisionExecute || first.Execution == nil {
			t.Fatalf("first claim = (%+v, %v), want execute", first, err)
		}
		duplicate, err := service.Claim(ctx, envelope)
		if err != nil || duplicate.Decision != jobs.DecisionWait || duplicate.RetryAfterMS <= 0 {
			t.Fatalf("duplicate active claim = (%+v, %v), want delayed wait", duplicate, err)
		}
		assertLifecycleCounts(t, ctx, pool, submission.JobID, 1, 1)

		fields := lifecycleFields("ИВАНОВ", "АЛЕКСЕЙ")
		completion := jobs.Completion{Envelope: envelope, AttemptToken: first.Execution.AttemptToken, Fields: fields}
		decision, err := service.CompleteRaw(ctx, completion)
		if err != nil || decision != jobs.DecisionAck {
			t.Fatalf("first completion = (%q, %v), want durable ACK", decision, err)
		}
		decision, err = service.CompleteRaw(ctx, completion)
		if err != nil || decision != jobs.DecisionAck {
			t.Fatalf("same completion replay = (%q, %v), want idempotent ACK", decision, err)
		}
		conflictingFields := lifecycleFields("ПЕТРОВ", "АЛЕКСЕЙ")
		decision, err = service.CompleteRaw(ctx, jobs.Completion{
			Envelope: envelope, AttemptToken: first.Execution.AttemptToken, Fields: conflictingFields,
		})
		if decision != jobs.DecisionAck || !errors.Is(err, jobs.ErrConflictingCompletion) {
			t.Fatalf("conflicting duplicate completion = (%q, %v), want ACK plus conflict", decision, err)
		}
		assertLifecycleJob(t, ctx, pool, submission.JobID, string(jobs.StatusCompleted), 1, nil, "ИВАНОВ")
		assertLifecycleCounts(t, ctx, pool, submission.JobID, 1, 1)
	})

	t.Run("retry is atomic, delayed, and capped at max attempts", func(t *testing.T) {
		service := newLifecycleService(t, store, 2)
		submission := createLifecycleJob(t, ctx, store)
		firstEnvelope := lifecycleEnvelope(submission, 1)
		first, err := service.Claim(ctx, firstEnvelope)
		if err != nil || first.Execution == nil {
			t.Fatalf("claim attempt 1 = (%+v, %v)", first, err)
		}
		decision, err := service.Fail(ctx, jobs.Failure{
			Envelope: firstEnvelope, AttemptToken: first.Execution.AttemptToken, ErrorCode: "processing_failed",
		})
		if err != nil || decision != jobs.DecisionAck {
			t.Fatalf("fail attempt 1 = (%q, %v), want durable ACK", decision, err)
		}
		assertLifecycleJob(t, ctx, pool, submission.JobID, string(jobs.StatusQueued), 1, nil, "")
		assertLifecycleCounts(t, ctx, pool, submission.JobID, 1, 2)
		assertLifecycleAttempt(t, ctx, pool, submission.JobID, 1, "failed", "processing_failed")

		secondEnvelope := lifecycleRetryEnvelope(t, ctx, pool, submission, 2)
		waiting, err := service.Claim(ctx, secondEnvelope)
		if err != nil || waiting.Decision != jobs.DecisionWait || waiting.RetryAfterMS <= 0 {
			t.Fatalf("claim before retry available_at = (%+v, %v), want wait", waiting, err)
		}
		var attempts int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM job_attempts WHERE job_id=$1`, submission.JobID).Scan(&attempts); err != nil || attempts != 1 {
			t.Fatalf("attempt rows while retry delayed = %d, %v; want 1", attempts, err)
		}
		makeLifecycleRetryDue(t, ctx, pool, submission.JobID, 2)
		second, err := service.Claim(ctx, secondEnvelope)
		if err != nil || second.Decision != jobs.DecisionExecute || second.Execution == nil {
			t.Fatalf("claim attempt 2 = (%+v, %v), want execute", second, err)
		}
		decision, err = service.Fail(ctx, jobs.Failure{
			Envelope: secondEnvelope, AttemptToken: second.Execution.AttemptToken, ErrorCode: "processing_failed",
		})
		if err != nil || decision != jobs.DecisionAck {
			t.Fatalf("fail final attempt = (%q, %v), want durable ACK", decision, err)
		}
		assertLifecycleJob(t, ctx, pool, submission.JobID, string(jobs.StatusFailed), 2, lifecyclePtr("processing_failed"), "")
		assertLifecycleCounts(t, ctx, pool, submission.JobID, 2, 2)
		decision, err = service.Fail(ctx, jobs.Failure{
			Envelope: secondEnvelope, AttemptToken: second.Execution.AttemptToken, ErrorCode: "processing_failed",
		})
		if err != nil || decision != jobs.DecisionAck {
			t.Fatalf("duplicate terminal failure = (%q, %v), want idempotent ACK", decision, err)
		}
		assertLifecycleCounts(t, ctx, pool, submission.JobID, 2, 2)
	})

	t.Run("permanent image failure is terminal without retry", func(t *testing.T) {
		service := newLifecycleService(t, store, 4)
		submission := createLifecycleJob(t, ctx, store)
		envelope := lifecycleEnvelope(submission, 1)
		claim, err := service.Claim(ctx, envelope)
		if err != nil || claim.Execution == nil {
			t.Fatalf("claim = (%+v, %v)", claim, err)
		}
		decision, err := service.Fail(ctx, jobs.Failure{
			Envelope: envelope, AttemptToken: claim.Execution.AttemptToken, ErrorCode: "image_unprocessable",
		})
		if err != nil || decision != jobs.DecisionAck {
			t.Fatalf("permanent failure = (%q, %v), want durable ACK", decision, err)
		}
		assertLifecycleJob(t, ctx, pool, submission.JobID, string(jobs.StatusFailed), 1, lifecyclePtr("image_unprocessable"), "")
		assertLifecycleCounts(t, ctx, pool, submission.JobID, 1, 1)
	})

	t.Run("expired attempt recovery and late completion fencing", func(t *testing.T) {
		service := newLifecycleService(t, store, 3)
		submission := createLifecycleJob(t, ctx, store)
		firstEnvelope := lifecycleEnvelope(submission, 1)
		first, err := service.Claim(ctx, firstEnvelope)
		if err != nil || first.Execution == nil {
			t.Fatalf("claim attempt 1 = (%+v, %v)", first, err)
		}
		expireLifecycleAttempt(t, ctx, pool, first.Execution.AttemptToken)
		recovered, err := service.RecoverExpired(ctx, 10)
		if err != nil || recovered != 1 {
			t.Fatalf("recover expired attempt = (%d, %v), want one", recovered, err)
		}
		if recovered, err = service.RecoverExpired(ctx, 10); err != nil || recovered != 0 {
			t.Fatalf("repeat recovery = (%d, %v), want no duplicate retry", recovered, err)
		}
		assertLifecycleJob(t, ctx, pool, submission.JobID, string(jobs.StatusQueued), 1, nil, "")
		assertLifecycleAttempt(t, ctx, pool, submission.JobID, 1, "failed", "processing_timeout")
		assertLifecycleCounts(t, ctx, pool, submission.JobID, 1, 2)

		makeLifecycleRetryDue(t, ctx, pool, submission.JobID, 2)
		secondEnvelope := lifecycleRetryEnvelope(t, ctx, pool, submission, 2)
		second, err := service.Claim(ctx, secondEnvelope)
		if err != nil || second.Decision != jobs.DecisionExecute || second.Execution == nil {
			t.Fatalf("claim attempt 2 = (%+v, %v), want execute", second, err)
		}
		staleDecision, staleErr := service.CompleteRaw(ctx, jobs.Completion{
			Envelope: firstEnvelope, AttemptToken: first.Execution.AttemptToken,
			Fields: lifecycleFields("СТАРЫЙ", "РЕЗУЛЬТАТ"),
		})
		if staleErr != nil || staleDecision != jobs.DecisionAck {
			t.Fatalf("late attempt 1 completion = (%q, %v), want superseded ACK", staleDecision, staleErr)
		}
		assertLifecycleJob(t, ctx, pool, submission.JobID, string(jobs.StatusProcessing), 2, nil, "")
		assertLifecycleAttempt(t, ctx, pool, submission.JobID, 2, "processing", "")

		decision, err := service.CompleteRaw(ctx, jobs.Completion{
			Envelope: secondEnvelope, AttemptToken: second.Execution.AttemptToken,
			Fields: lifecycleFields("НОВЫЙ", "РЕЗУЛЬТАТ"),
		})
		if err != nil || decision != jobs.DecisionAck {
			t.Fatalf("attempt 2 completion = (%q, %v); safe storage failure: %s; safe database state: %s", decision, err,
				safeStorageFailure(err),
				safeLifecycleState(ctx, pool, submission.JobID, 2))
		}
		assertLifecycleJob(t, ctx, pool, submission.JobID, string(jobs.StatusCompleted), 2, nil, "НОВЫЙ")
		assertLifecycleCounts(t, ctx, pool, submission.JobID, 2, 2)
	})

	t.Run("expired callback durably schedules retry before ACK", func(t *testing.T) {
		service := newLifecycleService(t, store, 3)
		submission := createLifecycleJob(t, ctx, store)
		envelope := lifecycleEnvelope(submission, 1)
		claim, err := service.Claim(ctx, envelope)
		if err != nil || claim.Execution == nil {
			t.Fatalf("claim = (%+v, %v)", claim, err)
		}
		expireLifecycleAttempt(t, ctx, pool, claim.Execution.AttemptToken)
		decision, err := service.CompleteRaw(ctx, jobs.Completion{
			Envelope: envelope, AttemptToken: claim.Execution.AttemptToken,
			Fields: lifecycleFields("LATE", "RESULT"),
		})
		if err != nil || decision != jobs.DecisionAck {
			t.Fatalf("expired completion = (%q, %v), want ACK after durable retry", decision, err)
		}
		assertLifecycleJob(t, ctx, pool, submission.JobID, string(jobs.StatusQueued), 1, nil, "")
		assertLifecycleAttempt(t, ctx, pool, submission.JobID, 1, "failed", "processing_timeout")
		assertLifecycleCounts(t, ctx, pool, submission.JobID, 1, 2)
	})

	t.Run("known poison is terminal while unknown event stays invalid", func(t *testing.T) {
		service := newLifecycleService(t, store, 3)
		poison := createLifecycleJob(t, ctx, store)
		poisonEnvelope := lifecycleEnvelope(poison, 1)
		poisonEnvelope.SchemaVersion = 9
		decision, err := service.Reject(ctx, jobs.Rejection{Envelope: poisonEnvelope})
		if err != nil || decision != jobs.DecisionAck {
			t.Fatalf("known poison reject = (%q, %v), want durable ACK", decision, err)
		}
		assertLifecycleJob(t, ctx, pool, poison.JobID, string(jobs.StatusFailed), 0, lifecyclePtr("processing_failed"), "")
		assertLifecycleCounts(t, ctx, pool, poison.JobID, 0, 1)

		unknown := createLifecycleJob(t, ctx, store)
		unknownEnvelope := lifecycleEnvelope(unknown, 1)
		unknownEnvelope.EventID = randomTestUUID()
		if decision, err := service.Reject(ctx, jobs.Rejection{Envelope: unknownEnvelope}); decision != "" || !errors.Is(err, jobs.ErrInvalidDelivery) {
			t.Fatalf("unknown event reject = (%q, %v), want invalid delivery", decision, err)
		}
		assertLifecycleJob(t, ctx, pool, unknown.JobID, string(jobs.StatusQueued), 0, nil, "")
		assertLifecycleCounts(t, ctx, pool, unknown.JobID, 0, 1)
	})

	t.Run("canceled database operation cannot ACK", func(t *testing.T) {
		service := newLifecycleService(t, store, 3)
		submission := createLifecycleJob(t, ctx, store)
		canceled, stop := context.WithCancel(ctx)
		stop()
		result, err := service.Claim(canceled, lifecycleEnvelope(submission, 1))
		if !errors.Is(err, context.Canceled) || result.Decision != "" {
			t.Fatalf("claim with canceled database context = (%+v, %v), want no decision", result, err)
		}
		assertLifecycleJob(t, ctx, pool, submission.JobID, string(jobs.StatusQueued), 0, nil, "")
		assertLifecycleCounts(t, ctx, pool, submission.JobID, 0, 1)
	})
}

func safeStorageFailure(err error) string {
	var failure *postgres.SafeStorageFailure
	if !errors.As(err, &failure) {
		return "unclassified"
	}
	return fmt.Sprintf("operation=%s sqlstate=%s constraint=%s", failure.Operation, failure.SQLState, failure.ConstraintName)
}

func safeLifecycleState(ctx context.Context, pool *pgxpool.Pool, jobID string, attempt int) string {
	ctxErr := ctx.Err()
	if ctxErr == nil {
		ctxErr = errors.New("none")
	}
	var jobState, attemptState string
	var attemptCount int
	var leaseRemainingMS int64
	var published, due, claimed bool
	err := pool.QueryRow(ctx, `
		SELECT j.state, j.attempt_count, a.state,
		       GREATEST(0, CEIL(EXTRACT(EPOCH FROM (a.lease_expires_at-clock_timestamp()))*1000))::bigint,
		       o.published_at IS NOT NULL,
		       o.available_at <= clock_timestamp(),
	       COALESCE(o.claim_until > clock_timestamp(), false)
		FROM jobs j
		JOIN job_attempts a ON a.job_id=j.job_id AND a.attempt_no=$2
		JOIN outbox o ON o.job_id=j.job_id AND o.attempt_no=$2
		WHERE j.job_id=$1`, jobID, attempt).Scan(
		&jobState, &attemptCount, &attemptState, &leaseRemainingMS,
		&published, &due, &claimed,
	)
	if err != nil {
		return fmt.Sprintf("context=%v diagnostic=unavailable", ctxErr)
	}
	return fmt.Sprintf("context=%v job=%s/%d attempt=%s lease_remaining_ms=%d outbox_published=%t due=%t claimed=%t",
		ctxErr, jobState, attemptCount, attemptState, leaseRemainingMS, published, due, claimed)
}

func newLifecycleService(t *testing.T, store *postgres.Store, maxAttempts int) *jobexecution.Service {
	t.Helper()
	service, err := jobexecution.New(lifecycleDiagnosticRepository{ExecutionRepository: store}, jobexecution.Config{
		PipelineVersion: "integration-lifecycle-v1", LeaseDuration: time.Hour,
		ProcessingTimeout: time.Minute, CompletionBudget: time.Minute,
		RetryPolicy: jobs.RetryPolicy{MaxAttempts: maxAttempts, InitialBackoff: time.Second, MaxBackoff: 4 * time.Second},
	})
	if err != nil {
		t.Fatalf("construct job execution service: %v", err)
	}
	return service
}

// lifecycleDiagnosticRepository preserves generic errors.Is behavior while
// appending only bounded storage metadata to integration-test failures.
type lifecycleDiagnosticRepository struct {
	jobs.ExecutionRepository
}

func (r lifecycleDiagnosticRepository) ClaimDelivery(ctx context.Context, envelope jobs.Envelope, lease time.Duration, policy jobs.RetryPolicy) (jobs.ClaimResult, error) {
	result, err := r.ExecutionRepository.ClaimDelivery(ctx, envelope, lease, policy)
	return result, annotateLifecycleStorageError(err)
}

func (r lifecycleDiagnosticRepository) PersistCompletion(ctx context.Context, completion jobs.PersistedCompletion, policy jobs.RetryPolicy) (jobs.DeliveryDecision, error) {
	decision, err := r.ExecutionRepository.PersistCompletion(ctx, completion, policy)
	return decision, annotateLifecycleStorageError(err)
}

func (r lifecycleDiagnosticRepository) FailAttempt(ctx context.Context, failure jobs.Failure, policy jobs.RetryPolicy) (jobs.DeliveryDecision, error) {
	decision, err := r.ExecutionRepository.FailAttempt(ctx, failure, policy)
	return decision, annotateLifecycleStorageError(err)
}

func (r lifecycleDiagnosticRepository) RejectDelivery(ctx context.Context, rejection jobs.Rejection) (jobs.DeliveryDecision, error) {
	decision, err := r.ExecutionRepository.RejectDelivery(ctx, rejection)
	return decision, annotateLifecycleStorageError(err)
}

func (r lifecycleDiagnosticRepository) RecoverExpired(ctx context.Context, limit int, policy jobs.RetryPolicy) (int, error) {
	count, err := r.ExecutionRepository.RecoverExpired(ctx, limit, policy)
	return count, annotateLifecycleStorageError(err)
}

func annotateLifecycleStorageError(err error) error {
	if err == nil {
		return nil
	}
	var failure *postgres.SafeStorageFailure
	if errors.As(err, &failure) {
		return fmt.Errorf("%w [storage operation=%s sqlstate=%s constraint=%s]", err, failure.Operation, failure.SQLState, failure.ConstraintName)
	}
	return err
}

func createLifecycleJob(t *testing.T, ctx context.Context, store *postgres.Store) jobs.Submission {
	t.Helper()
	digest := sha256.Sum256([]byte("isolated lifecycle fixture " + randomTestUUID()))
	submission := newSubmission("owner-lifecycle-"+randomTestUUID(), "lifecycle-"+randomTestUUID(), digest)
	if _, outcome, err := store.CreateQueued(ctx, submission); err != nil || outcome != jobs.OutcomeCreated {
		t.Fatalf("create lifecycle job outcome=%v err=%v", outcome, err)
	}
	return submission
}

func lifecycleEnvelope(submission jobs.Submission, attempt int) jobs.Envelope {
	return jobs.Envelope{SchemaVersion: 1, EventID: submission.EventID, JobID: submission.JobID, AttemptNo: attempt}
}

func lifecycleRetryEnvelope(t *testing.T, ctx context.Context, pool *pgxpool.Pool, submission jobs.Submission, attempt int) jobs.Envelope {
	t.Helper()
	var eventID string
	if err := pool.QueryRow(ctx, `SELECT event_id::text FROM outbox WHERE job_id=$1 AND attempt_no=$2`, submission.JobID, attempt).Scan(&eventID); err != nil {
		t.Fatalf("read retry outbox event: %v", err)
	}
	return jobs.Envelope{SchemaVersion: 1, EventID: eventID, JobID: submission.JobID, AttemptNo: attempt}
}

func lifecycleFields(surname, givenName string) map[string]*string {
	return map[string]*string{
		"surname": lifecyclePtr(surname), "given_name": lifecyclePtr(givenName), "patronymic": nil,
		"gender": lifecyclePtr("M"), "birth_date": lifecyclePtr("1990-04-12"), "birth_place": lifecyclePtr("МОСКВА"),
		"series": lifecyclePtr("0012"), "number": lifecyclePtr("000345"), "issue_date": lifecyclePtr("2010-06-18"),
		"issuer": lifecyclePtr("ОТДЕЛ ПРИМЕРА"), "department_code": lifecyclePtr("123-456"),
	}
}

func assertLifecycleJob(t *testing.T, ctx context.Context, pool *pgxpool.Pool, jobID, state string, attemptCount int, errorCode *string, surname string) {
	t.Helper()
	var actualState string
	var actualCount int
	var actualError *string
	var result []byte
	if err := pool.QueryRow(ctx, `SELECT state, attempt_count, error_code, result FROM jobs WHERE job_id=$1`, jobID).
		Scan(&actualState, &actualCount, &actualError, &result); err != nil {
		t.Fatalf("read lifecycle job state: %v", err)
	}
	if actualState != state || actualCount != attemptCount || !sameOptionalString(actualError, errorCode) {
		t.Fatalf("job state/count/error = %s/%d/%v, want %s/%d/%v", actualState, actualCount, actualError, state, attemptCount, errorCode)
	}
	if surname == "" {
		if state == string(jobs.StatusCompleted) || len(result) != 0 {
			t.Fatalf("unexpected completed result for state=%s: %s", state, result)
		}
		return
	}
	var response struct {
		Fields map[string]struct {
			Value *string `json:"value"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		t.Fatalf("decode canonical result: %v", err)
	}
	field, ok := response.Fields["surname"]
	if !ok || field.Value == nil || *field.Value != surname {
		t.Fatalf("stored canonical surname = %+v, want %q", field, surname)
	}
}

func assertLifecycleCounts(t *testing.T, ctx context.Context, pool *pgxpool.Pool, jobID string, attempts, outbox int) {
	t.Helper()
	var actualAttempts, actualOutbox int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM job_attempts WHERE job_id=$1`, jobID).Scan(&actualAttempts); err != nil {
		t.Fatalf("count lifecycle attempts: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE job_id=$1`, jobID).Scan(&actualOutbox); err != nil {
		t.Fatalf("count lifecycle outbox events: %v", err)
	}
	if actualAttempts != attempts || actualOutbox != outbox {
		t.Fatalf("lifecycle counts attempts/outbox=%d/%d, want %d/%d", actualAttempts, actualOutbox, attempts, outbox)
	}
}

func assertLifecycleAttempt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, jobID string, attempt int, state, errorCode string) {
	t.Helper()
	var actualState string
	var actualError *string
	if err := pool.QueryRow(ctx, `SELECT state, error_code FROM job_attempts WHERE job_id=$1 AND attempt_no=$2`, jobID, attempt).
		Scan(&actualState, &actualError); err != nil {
		t.Fatalf("read lifecycle attempt: %v", err)
	}
	if actualState != state || !sameOptionalString(actualError, optionalString(errorCode)) {
		t.Fatalf("attempt state/error=%s/%v, want %s/%q", actualState, actualError, state, errorCode)
	}
}

func makeLifecycleRetryDue(t *testing.T, ctx context.Context, pool *pgxpool.Pool, jobID string, attempt int) {
	t.Helper()
	// The created_at timestamps are already in the past; using them keeps schema
	// constraints valid while letting this test avoid sleeping for backoff.
	if _, err := pool.Exec(ctx, `UPDATE jobs SET available_at=created_at WHERE job_id=$1`, jobID); err != nil {
		t.Fatalf("make retry job available in test fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE outbox SET available_at=created_at WHERE job_id=$1 AND attempt_no=$2`, jobID, attempt); err != nil {
		t.Fatalf("make retry event available in test fixture: %v", err)
	}
}

func expireLifecycleAttempt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, token string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE job_attempts SET lease_expires_at=started_at WHERE attempt_token=$1`, token); err != nil {
		t.Fatalf("expire lifecycle attempt: %v", err)
	}
}

func sameOptionalString(actual, expected *string) bool {
	if actual == nil || expected == nil {
		return actual == nil && expected == nil
	}
	return *actual == *expected
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func lifecyclePtr(value string) *string { return &value }
