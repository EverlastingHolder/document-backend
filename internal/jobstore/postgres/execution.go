package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"passport-backend/internal/jobs"
)

const requestedEventType = "recognition.requested"

var (
	postgresSQLStatePattern   = regexp.MustCompile(`^[0-9A-Z]{5}$`)
	postgresIdentifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)
)

// SafeStorageFailure provides bounded diagnostics for internal tests and
// operators without retaining the driver error, SQL, parameters, or details.
// Its public error text and unwrap target remain the generic unavailable error.
type SafeStorageFailure struct {
	Operation      string
	SQLState       string
	ConstraintName string
	CauseType      string
}

func (e *SafeStorageFailure) Error() string { return jobs.ErrUnavailable.Error() }

func (e *SafeStorageFailure) Unwrap() error { return jobs.ErrUnavailable }

func safeStorageFailure(ctx context.Context, operation string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	failure := &SafeStorageFailure{Operation: operation}
	if causeType := reflect.TypeOf(err); causeType != nil {
		failure.CauseType = causeType.String()
	}
	var postgresErr *pgconn.PgError
	if errors.As(err, &postgresErr) {
		if postgresSQLStatePattern.MatchString(postgresErr.Code) {
			failure.SQLState = postgresErr.Code
		}
		if postgresIdentifierPattern.MatchString(postgresErr.ConstraintName) {
			failure.ConstraintName = postgresErr.ConstraintName
		}
	}
	return failure
}

// LeaseOutbox atomically leases due events and returns their privacy-minimal
// envelopes. It never holds a database transaction across broker I/O.
func (s *Store) LeaseOutbox(ctx context.Context, limit int, leaseFor time.Duration) ([]jobs.LeasedOutbox, error) {
	if s == nil || s.pool == nil || ctx == nil || limit <= 0 || leaseFor.Microseconds() <= 0 {
		return nil, jobs.ErrInvalidTransition
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, safeStorageFailure(ctx, "lease_outbox_begin", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	rows, err := tx.Query(ctx, `
		SELECT event_id::text, job_id::text, event_type, schema_version, attempt_no
		FROM outbox
		WHERE published_at IS NULL
		  AND event_type = 'recognition.requested'
		  AND schema_version = 'v1'
		  AND available_at <= clock_timestamp()
		  AND (claim_until IS NULL OR claim_until <= clock_timestamp())
		ORDER BY available_at, created_at, event_id
		LIMIT $1
		FOR UPDATE SKIP LOCKED
		`, limit)
	if err != nil {
		return nil, safeStorageFailure(ctx, "lease_outbox_select", err)
	}
	type candidate struct {
		eventID, jobID, eventType, schemaVersion string
		attemptNo                                int
	}
	candidates := make([]candidate, 0, limit)
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.eventID, &item.jobID, &item.eventType, &item.schemaVersion, &item.attemptNo); err != nil {
			rows.Close()
			return nil, safeStorageFailure(ctx, "lease_outbox_scan", err)
		}
		candidates = append(candidates, item)
	}
	rowsErr := rows.Err()
	rows.Close()
	if rowsErr != nil {
		return nil, safeStorageFailure(ctx, "lease_outbox_rows", rowsErr)
	}

	leased := make([]jobs.LeasedOutbox, 0, len(candidates))
	for _, item := range candidates {
		token, err := randomUUIDv4()
		if err != nil {
			return nil, jobs.ErrUnavailable
		}
		var claimUntil time.Time
		err = tx.QueryRow(ctx, `
			UPDATE outbox
			SET claim_token = $2,
			    claim_until = clock_timestamp() + ($3 * interval '1 microsecond'),
			    publish_attempts = publish_attempts + 1
			WHERE event_id = $1
			RETURNING claim_until`, item.eventID, token, leaseFor.Microseconds()).Scan(&claimUntil)
		if err != nil {
			return nil, safeStorageFailure(ctx, "lease_outbox_update", err)
		}
		leased = append(leased, jobs.LeasedOutbox{
			Envelope: jobs.Envelope{
				SchemaVersion: 1, EventID: item.eventID, JobID: item.jobID, AttemptNo: item.attemptNo,
			},
			EventType: item.eventType, StoredSchemaVersion: item.schemaVersion,
			ClaimToken: token, ClaimUntil: claimUntil,
		})
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, safeStorageFailure(ctx, "lease_outbox_commit", err)
	}
	return leased, nil
}

// MarkOutboxPublished acknowledges only the still-current lease and only after
// the caller received a broker publisher confirm and mandatory routing result.
func (s *Store) MarkOutboxPublished(ctx context.Context, eventID, claimToken string) error {
	if s == nil || s.pool == nil || ctx == nil || !canonicalUUIDv4.MatchString(eventID) || !canonicalUUIDv4.MatchString(claimToken) {
		return jobs.ErrInvalidDelivery
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE outbox
		SET published_at = clock_timestamp(), claim_token = NULL, claim_until = NULL,
		    last_error_code = NULL
		WHERE event_id = $1 AND claim_token = $2 AND claim_until > clock_timestamp()
		  AND published_at IS NULL`, eventID, claimToken)
	if err != nil {
		return safeStorageFailure(ctx, "mark_outbox_published", err)
	}
	if tag.RowsAffected() != 1 {
		return jobs.ErrInvalidTransition
	}
	return nil
}

// ReleaseOutbox records a safe failure category and schedules another
// publication attempt. A stale relay cannot release a newer lease.
func (s *Store) ReleaseOutbox(ctx context.Context, eventID, claimToken, safeErrorCode string, retryAfter time.Duration) error {
	if s == nil || s.pool == nil || ctx == nil || !canonicalUUIDv4.MatchString(eventID) ||
		!canonicalUUIDv4.MatchString(claimToken) || safeErrorCode != "publish_failed" || retryAfter.Microseconds() <= 0 {
		return jobs.ErrInvalidTransition
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE outbox
		SET available_at = clock_timestamp() + ($3 * interval '1 microsecond'),
		    claim_token = NULL, claim_until = NULL, last_error_code = $4
		WHERE event_id = $1 AND claim_token = $2 AND claim_until > clock_timestamp()
		  AND published_at IS NULL`, eventID, claimToken, retryAfter.Microseconds(), safeErrorCode)
	if err != nil {
		return safeStorageFailure(ctx, "release_outbox", err)
	}
	if tag.RowsAffected() != 1 {
		return jobs.ErrInvalidTransition
	}
	return nil
}

// ClaimDelivery validates a message against the committed outbox row and
// starts its matching attempt in one transaction. An unexpired active lease
// returns WAIT so the consumer does not acknowledge the only durable message.
func (s *Store) ClaimDelivery(ctx context.Context, envelope jobs.Envelope, leaseFor time.Duration, policy jobs.RetryPolicy) (jobs.ClaimResult, error) {
	if s == nil || s.pool == nil || ctx == nil || !validEnvelope(envelope) || leaseFor <= 0 || !validRetryPolicy(policy) {
		return jobs.ClaimResult{}, jobs.ErrInvalidDelivery
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return jobs.ClaimResult{}, safeStorageFailure(ctx, "claim_begin", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var eventType, schemaVersion, state string
	var attemptNo, attemptCount int
	var eventAvailable, jobAvailable time.Time
	var imageKey, mediaType string
	var imageSize int64
	var startedAt, leaseExpires *time.Time
	err = tx.QueryRow(ctx, `
		SELECT o.event_type, o.schema_version, o.attempt_no, o.available_at,
		       j.state, j.attempt_count, j.available_at, j.image_key::text,
		       j.image_media_type, j.image_size_bytes, j.started_at, a.lease_expires_at
		FROM outbox o
	JOIN jobs j ON j.job_id = o.job_id
	LEFT JOIN job_attempts a ON a.job_id = j.job_id AND a.attempt_no = j.attempt_count
		WHERE o.event_id = $1 AND o.job_id = $2
		FOR UPDATE OF o, j`, envelope.EventID, envelope.JobID).Scan(
		&eventType, &schemaVersion, &attemptNo, &eventAvailable,
		&state, &attemptCount, &jobAvailable, &imageKey, &mediaType, &imageSize, &startedAt, &leaseExpires,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return jobs.ClaimResult{}, jobs.ErrInvalidDelivery
	}
	if err != nil {
		return jobs.ClaimResult{}, safeStorageFailure(ctx, "claim_select_delivery", err)
	}
	if eventType != requestedEventType || schemaVersion != "v1" || attemptNo != envelope.AttemptNo {
		return jobs.ClaimResult{}, jobs.ErrInvalidDelivery
	}
	if state == string(jobs.StatusCompleted) || state == string(jobs.StatusFailed) {
		if err := tx.Commit(ctx); err != nil {
			return jobs.ClaimResult{}, safeStorageFailure(ctx, "claim_commit_terminal_ack", err)
		}
		return jobs.ClaimResult{Decision: jobs.DecisionAck}, nil
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return jobs.ClaimResult{}, err
	}
	if state == string(jobs.StatusProcessing) {
		if envelope.AttemptNo < attemptCount {
			if err := tx.Commit(ctx); err != nil {
				return jobs.ClaimResult{}, safeStorageFailure(ctx, "claim_commit_superseded_ack", err)
			}
			return jobs.ClaimResult{Decision: jobs.DecisionAck}, nil
		}
		if envelope.AttemptNo != attemptCount {
			return jobs.ClaimResult{}, jobs.ErrInvalidDelivery
		}
		if leaseExpires == nil {
			return jobs.ClaimResult{}, jobs.ErrInvalidTransition
		}
		if leaseExpires.After(now) {
			if err := tx.Commit(ctx); err != nil {
				return jobs.ClaimResult{}, safeStorageFailure(ctx, "claim_commit_wait", err)
			}
			return jobs.ClaimResult{Decision: jobs.DecisionWait, RetryAfter: leaseExpires.Sub(now)}, nil
		}
		if err := retryOrFinishAttempt(ctx, tx, envelope.JobID, attemptCount, attemptNo, "processing_timeout", policy, now); err != nil {
			return jobs.ClaimResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return jobs.ClaimResult{}, safeStorageFailure(ctx, "claim_commit_expired_retry", err)
		}
		return jobs.ClaimResult{Decision: jobs.DecisionAck}, nil
	}
	if state != string(jobs.StatusQueued) {
		return jobs.ClaimResult{}, jobs.ErrInvalidTransition
	}
	expectedAttempt := attemptCount + 1
	if envelope.AttemptNo < expectedAttempt {
		if err := tx.Commit(ctx); err != nil {
			return jobs.ClaimResult{}, safeStorageFailure(ctx, "claim_commit_stale_ack", err)
		}
		return jobs.ClaimResult{Decision: jobs.DecisionAck}, nil
	}
	if envelope.AttemptNo != expectedAttempt {
		return jobs.ClaimResult{}, jobs.ErrInvalidDelivery
	}
	if jobAvailable.After(now) || eventAvailable.After(now) {
		waitUntil := jobAvailable
		if eventAvailable.After(waitUntil) {
			waitUntil = eventAvailable
		}
		if err := tx.Commit(ctx); err != nil {
			return jobs.ClaimResult{}, safeStorageFailure(ctx, "claim_commit_delayed_wait", err)
		}
		return jobs.ClaimResult{Decision: jobs.DecisionWait, RetryAfter: waitUntil.Sub(now)}, nil
	}
	attemptID, err := randomUUIDv4()
	if err != nil {
		return jobs.ClaimResult{}, jobs.ErrUnavailable
	}
	token, err := randomUUIDv4()
	if err != nil {
		return jobs.ClaimResult{}, jobs.ErrUnavailable
	}
	var started, leaseUntil time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO job_attempts (attempt_id, job_id, attempt_no, attempt_token, state, started_at, lease_expires_at)
		VALUES ($1, $2, $3, $4, 'processing', clock_timestamp(), clock_timestamp() + ($5 * interval '1 microsecond'))
		RETURNING started_at, lease_expires_at`, attemptID, envelope.JobID, envelope.AttemptNo, token, leaseFor.Microseconds()).Scan(&started, &leaseUntil)
	if err != nil {
		return jobs.ClaimResult{}, safeStorageFailure(ctx, "claim_insert_attempt", err)
	}
	if err = execExactlyOne(ctx, tx, `
		UPDATE jobs
		SET state='processing', attempt_count=$2, started_at=$3, finished_at=NULL,
		    result=NULL, error_code=NULL, updated_at=GREATEST(clock_timestamp(), created_at, $3)
		WHERE job_id=$1 AND state='queued'`, envelope.JobID, envelope.AttemptNo, started); err != nil {
		return jobs.ClaimResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return jobs.ClaimResult{}, safeStorageFailure(ctx, "claim_commit_execute", err)
	}
	return jobs.ClaimResult{
		Decision: jobs.DecisionExecute,
		Execution: &jobs.Execution{
			AttemptToken: token, ImageKey: imageKey, MediaType: mediaType,
			Size: imageSize, AttemptNo: attemptNo, StartedAt: started, LeaseExpiresAt: leaseUntil,
		},
	}, nil
}

// PersistCompletion commits the canonical response, terminal job state, and
// attempt state together. The application service owns canonical mapping.
func (s *Store) PersistCompletion(ctx context.Context, completion jobs.PersistedCompletion, policy jobs.RetryPolicy) (jobs.DeliveryDecision, error) {
	if s == nil || s.pool == nil || ctx == nil || !validEnvelope(completion.Envelope) ||
		!canonicalUUIDv4.MatchString(completion.AttemptToken) || strings.TrimSpace(completion.PipelineVersion) == "" || !validJSONObject(completion.CanonicalJSON) || !validRetryPolicy(policy) {
		return "", jobs.ErrInvalidDelivery
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", safeStorageFailure(ctx, "begin", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var state, attemptState string
	var attemptNo, currentAttempt int
	var attemptToken string
	var resultPurged bool
	err = tx.QueryRow(ctx, `
		SELECT j.state, j.attempt_count, a.state, a.attempt_no, a.attempt_token::text,
		       j.result_purged_at IS NOT NULL
		FROM jobs j
	JOIN job_attempts a ON a.job_id=j.job_id
	JOIN outbox o ON o.event_id=$2 AND o.job_id=j.job_id
		WHERE j.job_id=$1 AND a.attempt_token=$3
		  AND o.event_type=$4 AND o.schema_version='v1' AND o.attempt_no=$5
		FOR UPDATE OF j,a`, completion.Envelope.JobID, completion.Envelope.EventID,
		completion.AttemptToken, requestedEventType, completion.Envelope.AttemptNo).Scan(&state, &currentAttempt, &attemptState, &attemptNo, &attemptToken, &resultPurged)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", jobs.ErrInvalidDelivery
	}
	if err != nil {
		return "", safeStorageFailure(ctx, "select_delivery", err)
	}
	if attemptNo != completion.Envelope.AttemptNo || attemptToken != completion.AttemptToken {
		return "", jobs.ErrInvalidDelivery
	}
	if attemptNo < currentAttempt {
		if err := tx.Commit(ctx); err != nil {
			return "", safeStorageFailure(ctx, "commit_superseded_attempt_ack", err)
		}
		return jobs.DecisionAck, nil
	}
	if state == string(jobs.StatusCompleted) && attemptState == "completed" {
		if resultPurged {
			if err := tx.Commit(ctx); err != nil {
				return "", safeStorageFailure(ctx, "commit_purged_completion_replay", err)
			}
			return jobs.DecisionAck, nil
		}
		var same bool
		if err := tx.QueryRow(ctx, `SELECT result IS NOT DISTINCT FROM $2::jsonb FROM jobs WHERE job_id=$1`, completion.Envelope.JobID, completion.CanonicalJSON).Scan(&same); err != nil {
			return "", safeStorageFailure(ctx, "select_completed_result", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return "", safeStorageFailure(ctx, "commit_completed_replay", err)
		}
		if !same {
			return jobs.DecisionAck, jobs.ErrConflictingCompletion
		}
		return jobs.DecisionAck, nil
	}
	if state != string(jobs.StatusProcessing) || attemptState != "processing" {
		if state == string(jobs.StatusQueued) || state == string(jobs.StatusFailed) || state == string(jobs.StatusCompleted) {
			if err := tx.Commit(ctx); err != nil {
				return "", safeStorageFailure(ctx, "commit_terminal_ack", err)
			}
			return jobs.DecisionAck, nil
		}
		return "", jobs.ErrInvalidTransition
	}
	if currentAttempt != attemptNo {
		if err := tx.Commit(ctx); err != nil {
			return "", safeStorageFailure(ctx, "commit_noncurrent_attempt_ack", err)
		}
		return jobs.DecisionAck, nil
	}
	var attemptLease time.Time
	if err := tx.QueryRow(ctx, `SELECT lease_expires_at FROM job_attempts WHERE attempt_token=$1`, completion.AttemptToken).Scan(&attemptLease); err != nil {
		return "", safeStorageFailure(ctx, "select_attempt_lease", err)
	}
	var now time.Time
	if err := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		return "", safeStorageFailure(ctx, "read_database_clock", err)
	}
	if !attemptLease.After(now) {
		if err := retryOrFinishAttempt(ctx, tx, completion.Envelope.JobID, currentAttempt, attemptNo, "processing_timeout", policy, now); err != nil {
			return "", err
		}
		if err := tx.Commit(ctx); err != nil {
			return "", safeStorageFailure(ctx, "commit_expired_attempt_retry", err)
		}
		return jobs.DecisionAck, nil
	}
	if err = execCompletionExactlyOne(ctx, tx, "update_attempt_completed", `
		UPDATE job_attempts SET state='completed', finished_at=GREATEST(clock_timestamp(), started_at)
		WHERE attempt_token=$1 AND state='processing'`, completion.AttemptToken); err != nil {
		return "", err
	}
	if err = execCompletionExactlyOne(ctx, tx, "update_job_completed", `
		WITH terminal_clock AS MATERIALIZED (
			SELECT GREATEST(clock_timestamp(), created_at, updated_at, started_at) AS at
			FROM jobs WHERE job_id=$1 AND state='processing' AND attempt_count=$4
		)
		UPDATE jobs SET state='completed', result=$2::jsonb, pipeline_version=$3,
		    error_code=NULL, finished_at=terminal_clock.at, updated_at=terminal_clock.at
		FROM terminal_clock
		WHERE job_id=$1 AND state='processing' AND attempt_count=$4`,
		completion.Envelope.JobID, completion.CanonicalJSON, completion.PipelineVersion, attemptNo); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", safeStorageFailure(ctx, "commit_completion", err)
	}
	return jobs.DecisionAck, nil
}

// FailAttempt persists a terminal failure or schedules the next attempt and
// its outbox event atomically. RabbitMQ is never asked to spin in a requeue loop.
func (s *Store) FailAttempt(ctx context.Context, failure jobs.Failure, policy jobs.RetryPolicy) (jobs.DeliveryDecision, error) {
	if s == nil || s.pool == nil || ctx == nil || !validEnvelope(failure.Envelope) ||
		!canonicalUUIDv4.MatchString(failure.AttemptToken) || !validFailureCode(failure.ErrorCode) || !validRetryPolicy(policy) {
		return "", jobs.ErrInvalidDelivery
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", safeStorageFailure(ctx, "fail_begin", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var state, attemptState string
	var attemptCount, attemptNo int
	var leaseExpires time.Time
	err = tx.QueryRow(ctx, `
		SELECT j.state, j.attempt_count, a.state, a.attempt_no, a.lease_expires_at
		FROM jobs j JOIN job_attempts a ON a.job_id=j.job_id
		JOIN outbox o ON o.event_id=$2 AND o.job_id=j.job_id
		WHERE j.job_id=$1 AND a.attempt_token=$3
		  AND o.event_type=$4 AND o.schema_version='v1' AND o.attempt_no=$5
		FOR UPDATE OF j,a`, failure.Envelope.JobID, failure.Envelope.EventID,
		failure.AttemptToken, requestedEventType, failure.Envelope.AttemptNo).Scan(&state, &attemptCount, &attemptState, &attemptNo, &leaseExpires)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", jobs.ErrInvalidDelivery
	}
	if err != nil {
		return "", safeStorageFailure(ctx, "fail_select_delivery", err)
	}
	if attemptNo != failure.Envelope.AttemptNo {
		return "", jobs.ErrInvalidDelivery
	}
	if state == string(jobs.StatusCompleted) || state == string(jobs.StatusFailed) || (state == string(jobs.StatusQueued) && attemptState == "failed") || attemptCount > attemptNo {
		if err := tx.Commit(ctx); err != nil {
			return "", safeStorageFailure(ctx, "fail_commit_terminal_ack", err)
		}
		return jobs.DecisionAck, nil
	}
	if state != string(jobs.StatusProcessing) || attemptState != "processing" || attemptCount != attemptNo {
		return "", jobs.ErrInvalidTransition
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return "", err
	}
	errorCode := failure.ErrorCode
	if !leaseExpires.After(now) {
		errorCode = "processing_timeout"
	}
	if err := retryOrFinishAttempt(ctx, tx, failure.Envelope.JobID, attemptCount, attemptNo, errorCode, policy, now); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", safeStorageFailure(ctx, "fail_commit_decision", err)
	}
	return jobs.DecisionAck, nil
}

// RejectDelivery terminally resolves a known queued event that cannot be
// decoded or processed. Unknown messages fail closed for DLQ handling.
func (s *Store) RejectDelivery(ctx context.Context, rejection jobs.Rejection) (jobs.DeliveryDecision, error) {
	if s == nil || s.pool == nil || ctx == nil || !validDeliveryIdentity(rejection.Envelope) {
		return "", jobs.ErrInvalidDelivery
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", safeStorageFailure(ctx, "reject_begin", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var state, eventType, schemaVersion string
	var attemptNo, attemptCount int
	var leaseExpires *time.Time
	err = tx.QueryRow(ctx, `
		SELECT j.state, j.attempt_count, o.event_type, o.schema_version, o.attempt_no, a.lease_expires_at
		FROM outbox o JOIN jobs j ON j.job_id=o.job_id
		LEFT JOIN job_attempts a ON a.job_id=j.job_id AND a.attempt_no=j.attempt_count
		WHERE o.event_id=$1 AND o.job_id=$2 FOR UPDATE OF o,j`,
		rejection.Envelope.EventID, rejection.Envelope.JobID).Scan(&state, &attemptCount, &eventType, &schemaVersion, &attemptNo, &leaseExpires)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", jobs.ErrInvalidDelivery
	}
	if err != nil {
		return "", safeStorageFailure(ctx, "reject_select_delivery", err)
	}
	if eventType != requestedEventType || schemaVersion != "v1" || attemptNo != rejection.Envelope.AttemptNo {
		return "", jobs.ErrInvalidDelivery
	}
	if state == string(jobs.StatusCompleted) || state == string(jobs.StatusFailed) || attemptCount > attemptNo {
		if err := tx.Commit(ctx); err != nil {
			return "", safeStorageFailure(ctx, "reject_commit_terminal_ack", err)
		}
		return jobs.DecisionAck, nil
	}
	if state == string(jobs.StatusProcessing) {
		if leaseExpires != nil {
			if err := tx.Commit(ctx); err != nil {
				return "", safeStorageFailure(ctx, "reject_commit_wait", err)
			}
			return jobs.DecisionWait, nil
		}
		return "", jobs.ErrInvalidTransition
	}
	if state != string(jobs.StatusQueued) || attemptNo != attemptCount+1 {
		return "", jobs.ErrInvalidTransition
	}
	if err = execExactlyOne(ctx, tx, `
		WITH terminal_clock AS MATERIALIZED (
			SELECT GREATEST(clock_timestamp(), created_at, updated_at, started_at) AS at
			FROM jobs WHERE job_id=$1 AND state='queued'
		)
		UPDATE jobs SET state='failed', error_code=$2, result=NULL,
		    finished_at=terminal_clock.at, updated_at=terminal_clock.at
		FROM terminal_clock
		WHERE job_id=$1 AND state='queued'`, rejection.Envelope.JobID, "processing_failed"); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", safeStorageFailure(ctx, "reject_commit_terminal", err)
	}
	return jobs.DecisionAck, nil
}

// RecoverExpired requeues or terminally fails stale processing attempts. Each
// job and its follow-up outbox event are changed in one transaction.
func (s *Store) RecoverExpired(ctx context.Context, limit int, policy jobs.RetryPolicy) (int, error) {
	if s == nil || s.pool == nil || ctx == nil || limit <= 0 || !validRetryPolicy(policy) {
		return 0, jobs.ErrInvalidTransition
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, safeStorageFailure(ctx, "recover_begin", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	rows, err := tx.Query(ctx, `
		SELECT j.job_id::text, j.attempt_count
		FROM jobs j
	JOIN job_attempts a ON a.job_id=j.job_id AND a.attempt_no=j.attempt_count
		WHERE j.state='processing' AND a.state='processing' AND a.lease_expires_at <= clock_timestamp()
		ORDER BY a.lease_expires_at, j.job_id
		LIMIT $1 FOR UPDATE OF j,a SKIP LOCKED`, limit)
	if err != nil {
		return 0, safeStorageFailure(ctx, "recover_select_expired", err)
	}
	type stale struct {
		jobID   string
		attempt int
	}
	items := make([]stale, 0, limit)
	for rows.Next() {
		var item stale
		if err := rows.Scan(&item.jobID, &item.attempt); err != nil {
			rows.Close()
			return 0, safeStorageFailure(ctx, "recover_scan_expired", err)
		}
		items = append(items, item)
	}
	rowsErr := rows.Err()
	rows.Close()
	if rowsErr != nil {
		return 0, safeStorageFailure(ctx, "recover_rows_expired", rowsErr)
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return 0, err
	}
	for _, item := range items {
		if err := retryOrFinishAttempt(ctx, tx, item.jobID, item.attempt, item.attempt, "processing_timeout", policy, now); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, safeStorageFailure(ctx, "recover_commit", err)
	}
	return len(items), nil
}

func retryOrFinishAttempt(ctx context.Context, tx pgx.Tx, jobID string, attemptCount, attemptNo int, errorCode string, policy jobs.RetryPolicy, now time.Time) error {
	if !validFailureCode(errorCode) || !validRetryPolicy(policy) || attemptNo <= 0 || attemptCount != attemptNo {
		return jobs.ErrInvalidTransition
	}
	if err := execExactlyOne(ctx, tx, `
		UPDATE job_attempts SET state='failed', finished_at=GREATEST(clock_timestamp(), started_at), error_code=$2
		WHERE job_id=$1 AND attempt_no=$3 AND state='processing'`, jobID, errorCode, attemptNo); err != nil {
		return err
	}
	if errorCode == "image_unprocessable" || attemptNo >= policy.MaxAttempts {
		if err := execExactlyOne(ctx, tx, `
			WITH terminal_clock AS MATERIALIZED (
				SELECT GREATEST(clock_timestamp(), created_at, updated_at, started_at) AS at
				FROM jobs WHERE job_id=$1 AND state='processing' AND attempt_count=$3
			)
			UPDATE jobs SET state='failed', error_code=$2, result=NULL,
				finished_at=terminal_clock.at, updated_at=terminal_clock.at
			FROM terminal_clock
			WHERE job_id=$1 AND state='processing' AND attempt_count=$3`, jobID, errorCode, attemptNo); err != nil {
			return err
		}
		return nil
	}
	nextAttempt := attemptNo + 1
	backoff := retryBackoff(policy, attemptNo)
	availableAt := now.Add(backoff)
	if err := execExactlyOne(ctx, tx, `
		UPDATE jobs SET state='queued', error_code=NULL, result=NULL, finished_at=NULL,
			available_at=$2, updated_at=clock_timestamp()
		WHERE job_id=$1 AND state='processing' AND attempt_count=$3`, jobID, availableAt, attemptNo); err != nil {
		return err
	}
	eventID, err := randomUUIDv4()
	if err != nil {
		return jobs.ErrUnavailable
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO outbox (event_id, job_id, event_type, schema_version, attempt_no, available_at)
		VALUES ($1, $2, $3, 'v1', $4, $5)`, eventID, jobID, requestedEventType, nextAttempt, availableAt)
	if err != nil {
		return safeStorageFailure(ctx, "retry_schedule_outbox_insert", err)
	}
	return nil
}

func databaseNow(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var now time.Time
	if err := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		return time.Time{}, safeStorageFailure(ctx, "database_clock", err)
	}
	return now, nil
}

func execExactlyOne(ctx context.Context, tx pgx.Tx, sql string, arguments ...any) error {
	tag, err := tx.Exec(ctx, sql, arguments...)
	if err != nil {
		return safeStorageFailure(ctx, "execution_sql", err)
	}
	if tag.RowsAffected() != 1 {
		return jobs.ErrInvalidTransition
	}
	return nil
}

func execCompletionExactlyOne(ctx context.Context, tx pgx.Tx, operation, sql string, arguments ...any) error {
	tag, err := tx.Exec(ctx, sql, arguments...)
	if err != nil {
		return safeStorageFailure(ctx, operation, err)
	}
	if tag.RowsAffected() != 1 {
		return jobs.ErrInvalidTransition
	}
	return nil
}

func validEnvelope(envelope jobs.Envelope) bool {
	return envelope.SchemaVersion == 1 && validDeliveryIdentity(envelope)
}

func validDeliveryIdentity(envelope jobs.Envelope) bool {
	return envelope.AttemptNo > 0 && canonicalUUIDv4.MatchString(envelope.EventID) &&
		canonicalUUIDv4.MatchString(envelope.JobID)
}

func validFailureCode(code string) bool {
	switch code {
	case "image_unprocessable", "processing_failed", "ocr_unavailable", "processing_timeout":
		return true
	default:
		return false
	}
}

func validRetryPolicy(policy jobs.RetryPolicy) bool {
	return policy.MaxAttempts > 0 && policy.InitialBackoff > 0 && policy.MaxBackoff >= policy.InitialBackoff
}

func retryBackoff(policy jobs.RetryPolicy, attemptNo int) time.Duration {
	backoff := policy.InitialBackoff
	for n := 1; n < attemptNo && backoff < policy.MaxBackoff; n++ {
		if backoff > policy.MaxBackoff/2 {
			return policy.MaxBackoff
		}
		backoff *= 2
	}
	if backoff > policy.MaxBackoff {
		return policy.MaxBackoff
	}
	return backoff
}

func validJSONObject(data []byte) bool {
	if !json.Valid(data) {
		return false
	}
	var value map[string]json.RawMessage
	return json.Unmarshal(data, &value) == nil && value != nil
}

func randomUUIDv4() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := make([]byte, 36)
	hex.Encode(encoded[0:8], raw[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], raw[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], raw[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], raw[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], raw[10:16])
	return string(encoded), nil
}
