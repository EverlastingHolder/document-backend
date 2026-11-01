//go:build integration

package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"passport-backend/internal/jobs"
)

func TestRetryFloorCoversOutboxTransactionTimestamp(t *testing.T) {
	dsnFile := strings.TrimSpace(os.Getenv("PASSPORT_POSTGRES_TEST_DSN_FILE"))
	if dsnFile == "" {
		t.Fatal("integration tests require PASSPORT_POSTGRES_TEST_DSN_FILE")
	}
	dsnBytes, err := os.ReadFile(dsnFile)
	if err != nil {
		t.Fatalf("read private test DSN file: %v", err)
	}
	dsn := strings.TrimSpace(string(dsnBytes))
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("private PostgreSQL test DSN did not parse")
	}
	if config.ConnConfig.Host != "postgres" || !strings.HasPrefix(config.ConnConfig.Database, "passport_pg_it_") || config.ConnConfig.User != "passport_test" {
		t.Fatal("integration test requires the isolated Compose database")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("open isolated PostgreSQL test pool")
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Fatal("ping isolated PostgreSQL test pool")
	}
	store := &Store{pool: pool}

	jobID, err := randomUUIDv4()
	if err != nil {
		t.Fatal("generate test job ID")
	}
	eventID, err := randomUUIDv4()
	if err != nil {
		t.Fatal("generate test event ID")
	}
	imageKey, err := randomUUIDv4()
	if err != nil {
		t.Fatal("generate test image key")
	}
	digest := sha256.Sum256([]byte("retry transaction timestamp fixture " + jobID))
	submission := jobs.Submission{
		JobID: jobID, EventID: eventID, OwnerID: "retry-transaction-clock-" + jobID,
		IdempotencyKey: "retry-transaction-clock-" + jobID, RequestDigest: digest,
		ImageKey: imageKey, ImageSHA256: hex.EncodeToString(digest[:]), MediaType: "image/jpeg", Size: 1,
	}
	if _, outcome, err := store.CreateQueued(ctx, submission); err != nil || outcome != jobs.OutcomeCreated {
		t.Fatalf("CreateQueued outcome=%v err=%v", outcome, err)
	}
	policy := jobs.RetryPolicy{MaxAttempts: 2, InitialBackoff: time.Second, MaxBackoff: time.Second}
	envelope := jobs.Envelope{SchemaVersion: 1, EventID: eventID, JobID: jobID, AttemptNo: 1}
	claim, err := store.ClaimDelivery(ctx, envelope, 2*time.Hour, policy)
	if err != nil || claim.Execution == nil {
		t.Fatalf("ClaimDelivery = (%+v, %v)", claim, err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal("begin retry transaction")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var transactionAt time.Time
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&transactionAt); err != nil {
		t.Fatal("read transaction timestamp")
	}
	var created, started time.Time
	if err := tx.QueryRow(ctx, `
		UPDATE jobs SET created_at=$2::timestamptz - interval '3 hours',
		    available_at=$2::timestamptz - interval '3 hours',
		    started_at=$2::timestamptz - interval '2 hours',
		    updated_at=$2::timestamptz - interval '2 hours'
		WHERE job_id=$1 AND state='processing' RETURNING created_at, started_at`, jobID, transactionAt).
		Scan(&created, &started); err != nil {
		t.Fatal("set valid past job timestamps")
	}
	if _, err := tx.Exec(ctx, `
		UPDATE job_attempts SET started_at=$2::timestamptz,
		    lease_expires_at=$2::timestamptz + interval '2 hours'
		WHERE job_id=$1 AND attempt_no=1 AND state='processing'`, jobID, started); err != nil {
		t.Fatal("set valid past attempt timestamps")
	}
	rollbackNow := transactionAt.Add(-time.Hour)
	rawAvailableAt := rollbackNow.Add(policy.InitialBackoff)
	if !rawAvailableAt.After(created) || !rawAvailableAt.Before(transactionAt) {
		t.Fatal("fixture must place raw retry availability between job creation and transaction start")
	}
	if err := retryOrFinishAttempt(ctx, tx, jobID, 1, 1, "processing_failed", policy, rollbackNow); err != nil {
		t.Fatalf("retry after clock rollback: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal("commit retry after clock rollback")
	}
	var state string
	var jobAvailable, outboxAvailable, outboxCreated time.Time
	if err := pool.QueryRow(ctx, `SELECT state, available_at FROM jobs WHERE job_id=$1`, jobID).
		Scan(&state, &jobAvailable); err != nil {
		t.Fatal("read retry job schedule")
	}
	if err := pool.QueryRow(ctx, `SELECT available_at, created_at FROM outbox WHERE job_id=$1 AND attempt_no=2`, jobID).
		Scan(&outboxAvailable, &outboxCreated); err != nil {
		t.Fatal("read retry outbox schedule")
	}
	if state != string(jobs.StatusQueued) || jobAvailable != transactionAt || outboxAvailable != transactionAt || outboxCreated != transactionAt {
		t.Fatalf("retry state=%s job schedule equals tx=%t outbox schedule equals tx=%t outbox creation equals tx=%t",
			state, jobAvailable == transactionAt, outboxAvailable == transactionAt, outboxCreated == transactionAt)
	}
}
