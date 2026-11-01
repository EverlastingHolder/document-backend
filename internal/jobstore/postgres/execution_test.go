package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"passport-backend/internal/jobs"
)

func TestRetryBackoffIsExponentialAndCapped(t *testing.T) {
	policy := jobs.RetryPolicy{
		MaxAttempts: 5, InitialBackoff: 3 * time.Second, MaxBackoff: 10 * time.Second,
	}
	for attempt, want := range map[int]time.Duration{
		1: 3 * time.Second,
		2: 6 * time.Second,
		3: 10 * time.Second,
		4: 10 * time.Second,
	} {
		if got := retryBackoff(policy, attempt); got != want {
			t.Errorf("retryBackoff(attempt %d) = %s, want %s", attempt, got, want)
		}
	}
}

func TestRetryBackoffDoesNotOverflowDuration(t *testing.T) {
	policy := jobs.RetryPolicy{
		MaxAttempts: 100, InitialBackoff: time.Duration(1 << 62), MaxBackoff: time.Duration(1<<63 - 1),
	}
	if got := retryBackoff(policy, 2); got != policy.MaxBackoff {
		t.Fatalf("retryBackoff(2) = %s, want cap %s", got, policy.MaxBackoff)
	}
}

func TestEnvelopeAndCanonicalJSONValidation(t *testing.T) {
	valid := jobs.Envelope{
		SchemaVersion: 1, EventID: "a4f3706b-c1a9-49ae-9bf4-917ec20d80c8",
		JobID: "f2d5a4c8-4735-4242-8a70-ef42fb6db936", AttemptNo: 1,
	}
	if !validEnvelope(valid) || !validJSONObject([]byte(`{"status":"completed"}`)) {
		t.Fatal("valid envelope or canonical JSON object rejected")
	}
	invalid := valid
	invalid.SchemaVersion = 2
	if validEnvelope(invalid) {
		t.Fatal("unsupported message schema version accepted")
	}
	for _, value := range [][]byte{nil, []byte(`null`), []byte(`[]`), []byte(`"object"`), []byte(`{`)} {
		if validJSONObject(value) {
			t.Errorf("validJSONObject(%q) = true", value)
		}
	}
}

func TestRandomUUIDv4(t *testing.T) {
	value, err := randomUUIDv4()
	if err != nil {
		t.Fatal(err)
	}
	if !canonicalUUIDv4.MatchString(value) {
		t.Fatalf("randomUUIDv4() = %q, not canonical UUIDv4", value)
	}
}

func TestSafeStorageFailureRedactsDriverDetails(t *testing.T) {
	cause := &pgconn.PgError{
		Code: "23514", ConstraintName: "jobs_state_check",
		Message: "private driver message", Detail: "private OCR payload detail",
	}
	err := safeStorageFailure(context.Background(), "update_job_completed", cause)
	if !errors.Is(err, jobs.ErrUnavailable) {
		t.Fatalf("safe failure = %v, want ErrUnavailable", err)
	}
	if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "23514") || strings.Contains(err.Error(), "jobs_state_check") {
		t.Fatalf("safe error text leaked diagnostic details: %q", err.Error())
	}
	var diagnostic *SafeStorageFailure
	if !errors.As(err, &diagnostic) {
		t.Fatalf("safe failure type = %T, want *SafeStorageFailure", err)
	}
	var retainedDriverError *pgconn.PgError
	if errors.As(err, &retainedDriverError) {
		t.Fatal("safe failure retained the raw PostgreSQL driver error")
	}
	if diagnostic.Operation != "update_job_completed" || diagnostic.SQLState != "23514" || diagnostic.ConstraintName != "jobs_state_check" {
		t.Fatalf("safe diagnostic = %+v, want bounded stage/state/constraint", diagnostic)
	}
	if strings.Contains(diagnostic.Error(), "private") || strings.Contains(diagnostic.Error(), "payload") {
		t.Fatalf("diagnostic error exposed driver cause: %q", diagnostic.Error())
	}
}
