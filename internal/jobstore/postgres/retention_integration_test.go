//go:build integration

package postgres_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"passport-backend/internal/imagestore"
	"passport-backend/internal/jobexecution"
	"passport-backend/internal/jobs"
	"passport-backend/internal/jobstatus"
	"passport-backend/internal/jobstore/postgres"
	"passport-backend/internal/retention"
)

func TestPostgresRetentionPurgesTerminalDataAndPreservesIdempotency(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	dsn := integrationDSN(t)
	intake, repository, images, imageRoot, pool := newIntegrationIntake(t, ctx, dsn, 1<<20)
	maintenance, err := repository.ReadMaintenanceSnapshot(ctx)
	if err != nil {
		t.Fatalf("read aggregate maintenance snapshot: %s", safeRetentionStorageFailure(err))
	}
	if maintenance.QueuedJobs < 0 || maintenance.OldestQueuedAge < 0 || maintenance.ExpiredLeases < 0 ||
		maintenance.TerminalImagesPending < 0 || maintenance.TerminalResultsPending < 0 {
		t.Fatal("maintenance snapshot returned a negative aggregate")
	}
	retainer, err := retention.New(repository, images, retention.Config{
		ClaimLease: 30 * time.Second, Interval: time.Second, OperationTimeout: 20 * time.Second, BatchSize: 32,
	})
	if err != nil {
		t.Fatal("construct retention service")
	}
	data := []byte("synthetic retention image")
	accepted, err := intake.Accept(ctx, "owner-retention-primary", "retention-same-key", data, "image/jpeg")
	if err != nil {
		t.Fatal("create queued retention fixture")
	}
	var storedImageDigest []byte
	if err := pool.QueryRow(ctx, `SELECT image_sha256 FROM jobs WHERE job_id=$1::uuid`, accepted.JobID).Scan(&storedImageDigest); err != nil {
		t.Fatal("read persisted guarded image digest")
	}
	wantImageDigest := sha256.Sum256(data)
	if !bytes.Equal(storedImageDigest, wantImageDigest[:]) {
		t.Fatal("persisted image digest does not match stored bytes")
	}
	if outcome, err := repository.AcknowledgeOwnedResult(ctx, "owner-retention-primary", accepted.JobID); err != nil || outcome != jobs.ReceiptActive {
		t.Fatalf("receipt for queued fixture = (%v,%v), want active", outcome, err)
	}
	if _, err := repository.AcknowledgeOwnedResult(ctx, "owner-retention-other", accepted.JobID); !errors.Is(err, jobs.ErrNotFound) {
		t.Fatalf("foreign receipt error = %v, want not found", err)
	}
	var eventID string
	if err := pool.QueryRow(ctx, `SELECT event_id::text FROM outbox WHERE job_id=$1::uuid AND attempt_no=1`, accepted.JobID).Scan(&eventID); err != nil {
		t.Fatal("read outbox event for completion fixture")
	}
	envelope := jobs.Envelope{SchemaVersion: 1, EventID: eventID, JobID: accepted.JobID, AttemptNo: 1}
	execution, err := jobexecution.New(repository, jobexecution.Config{
		PipelineVersion: "retention-test-pipeline", LeaseDuration: 10 * time.Minute,
		ProcessingTimeout: 5 * time.Minute, CompletionBudget: 2 * time.Minute,
		RetryPolicy:         jobs.RetryPolicy{MaxAttempts: 1, InitialBackoff: time.Second, MaxBackoff: time.Second},
		TerminalImagePurger: retainer,
	})
	if err != nil {
		t.Fatal("construct completion service")
	}
	claim, err := execution.Claim(ctx, envelope)
	if err != nil || claim.Execution == nil {
		t.Fatal("claim completion fixture")
	}
	fields := map[string]*string{
		"surname": nil, "given_name": nil, "patronymic": nil, "gender": nil, "birth_date": nil,
		"birth_place": nil, "series": nil, "number": nil, "issue_date": nil, "issuer": nil, "department_code": nil,
	}
	if decision, err := execution.CompleteRaw(ctx, jobs.Completion{Envelope: envelope, AttemptToken: claim.Execution.AttemptToken, Fields: fields}); err != nil || decision != jobs.DecisionAck {
		t.Fatalf("terminal completion = (%q,%v), want durable ACK", decision, err)
	}
	assertImageObjectCount(t, imageRoot, 0)
	job, err := repository.Lookup(ctx, "owner-retention-primary", "retention-same-key")
	if err != nil || job.ID != accepted.JobID || job.ImageKey != "" {
		t.Fatalf("technical idempotency row after image purge = (%+v,%v)", job, err)
	}
	reader := jobstatus.New(repository)
	if snapshot, err := reader.ReadOwned(ctx, "owner-retention-primary", accepted.JobID); err != nil || snapshot.Status != jobs.StatusCompleted || len(snapshot.Result) == 0 {
		t.Fatalf("completed status before receipt = (%#v,%v), want canonical result", snapshot, err)
	}
	if outcome, err := repository.AcknowledgeOwnedResult(ctx, "owner-retention-primary", accepted.JobID); err != nil || outcome != jobs.ReceiptReceived {
		t.Fatalf("first receipt = (%v,%v), want received", outcome, err)
	}
	if outcome, err := repository.AcknowledgeOwnedResult(ctx, "owner-retention-primary", accepted.JobID); err != nil || outcome != jobs.ReceiptAlreadyPurged {
		t.Fatalf("repeated receipt = (%v,%v), want already purged", outcome, err)
	}
	if snapshot, err := reader.ReadOwned(ctx, "owner-retention-primary", accepted.JobID); !errors.Is(err, jobs.ErrResultGone) || snapshot.JobID != "" {
		t.Fatalf("status after receipt = (%#v,%v), want result gone", snapshot, err)
	}
	replay, err := intake.Accept(ctx, "owner-retention-primary", "retention-same-key", data, "image/jpeg")
	if err != nil || replay.JobID != accepted.JobID {
		t.Fatalf("same idempotency request after purge = (%+v,%v), want original ID", replay, err)
	}
	if _, err := intake.Accept(ctx, "owner-retention-primary", "retention-same-key", []byte("changed image"), "image/jpeg"); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("changed replay after purge = %v, want conflict", err)
	}
	if decision, err := execution.CompleteRaw(ctx, jobs.Completion{Envelope: envelope, AttemptToken: claim.Execution.AttemptToken, Fields: fields}); err != nil || decision != jobs.DecisionAck {
		t.Fatalf("late duplicate completion after receipt = (%q,%v), want ACK", decision, err)
	}
	if decision, err := execution.Fail(ctx, jobs.Failure{Envelope: envelope, AttemptToken: claim.Execution.AttemptToken, ErrorCode: "processing_failed"}); err != nil || decision != jobs.DecisionAck {
		t.Fatalf("late duplicate failure after receipt = (%q,%v), want ACK", decision, err)
	}
	var resultIsNull, markerIsSet bool
	if err := pool.QueryRow(ctx, `SELECT result IS NULL, result_purged_at IS NOT NULL FROM jobs WHERE job_id=$1::uuid`, accepted.JobID).Scan(&resultIsNull, &markerIsSet); err != nil || !resultIsNull || !markerIsSet {
		t.Fatalf("late callback changed purged terminal payload: null=%t purged=%t err=%v", resultIsNull, markerIsSet, err)
	}
	if snapshot, err := reader.ReadOwned(ctx, "owner-retention-primary", accepted.JobID); !errors.Is(err, jobs.ErrResultGone) || snapshot.JobID != "" {
		t.Fatalf("status after late completion = (%#v,%v), want result gone", snapshot, err)
	}
	assertImageObjectCount(t, imageRoot, 0)

	// The unclaimed terminal result expires from finished_at using the database
	// clock. Expiry preserves the idempotency/job identity row and is race-safe.
	second, err := intake.Accept(ctx, "owner-retention-primary", "retention-expiry-key", data, "image/jpeg")
	if err != nil {
		t.Fatal("create unclaimed-result fixture")
	}
	if _, err := pool.Exec(ctx, `
		UPDATE jobs SET created_at=clock_timestamp()-interval '17 minutes',
		    state='completed', started_at=clock_timestamp()-interval '17 minutes',
		    finished_at=clock_timestamp()-interval '16 minutes', updated_at=clock_timestamp(),
		    pipeline_version='test-pipeline', result='{}'::jsonb
		WHERE job_id=$1::uuid AND state='queued'`, second.JobID); err != nil {
		t.Fatal("make expired-result fixture terminal")
	}
	var purgeWait sync.WaitGroup
	concurrentPurgeErrors := make(chan error, 2)
	for range 2 {
		purgeWait.Add(1)
		go func() {
			defer purgeWait.Done()
			concurrentPurgeErrors <- retainer.PurgeJobImage(ctx, second.JobID)
		}()
	}
	purgeWait.Wait()
	close(concurrentPurgeErrors)
	for err := range concurrentPurgeErrors {
		if err != nil {
			t.Fatal("concurrent image purge claim failed")
		}
	}
	if snapshot, err := reader.ReadOwned(ctx, "owner-retention-primary", second.JobID); !errors.Is(err, jobs.ErrResultGone) || snapshot.JobID != "" {
		t.Fatalf("expired result before maintenance sweep = (%#v,%v), want immediate result-gone", snapshot, err)
	}
	type receiptRaceResult struct {
		outcome jobs.ReceiptOutcome
		err     error
	}
	receiptResult := make(chan receiptRaceResult, 1)
	purgeResult := make(chan struct {
		count int
		err   error
	}, 1)
	go func() {
		outcome, err := repository.AcknowledgeOwnedResult(ctx, "owner-retention-primary", second.JobID)
		receiptResult <- receiptRaceResult{outcome: outcome, err: err}
	}()
	go func() {
		count, err := retainer.PurgeExpiredResults(ctx, 10)
		purgeResult <- struct {
			count int
			err   error
		}{count: count, err: err}
	}()
	receipt := <-receiptResult
	purge := <-purgeResult
	if receipt.err != nil || (receipt.outcome != jobs.ReceiptReceived && receipt.outcome != jobs.ReceiptAlreadyPurged) {
		t.Fatalf("receipt/expiry race outcome = (%v,%v)", receipt.outcome, receipt.err)
	}
	if purge.err != nil || purge.count < 0 || purge.count > 1 {
		t.Fatalf("result-expiry race = (%d,%v)", purge.count, purge.err)
	}
	if snapshot, err := reader.ReadOwned(ctx, "owner-retention-primary", second.JobID); !errors.Is(err, jobs.ErrResultGone) || snapshot.JobID != "" {
		t.Fatalf("expired result status = (%#v,%v), want result gone", snapshot, err)
	}
	if outcome, err := repository.AcknowledgeOwnedResult(ctx, "owner-retention-primary", second.JobID); err != nil || outcome != jobs.ReceiptAlreadyPurged {
		t.Fatalf("receipt after expiry = (%v,%v), want already purged", outcome, err)
	}
	if result, err := repository.Lookup(ctx, "owner-retention-primary", "retention-expiry-key"); err != nil || result.ID != second.JobID {
		t.Fatalf("idempotency row after expiry = (%+v,%v), want original row", result, err)
	}
	if _, err := retainer.PurgeTerminalImages(ctx, 10); err != nil {
		t.Fatal("purge second terminal image")
	}
	postPurgeSnapshot, err := repository.ReadMaintenanceSnapshot(ctx)
	if err != nil {
		t.Fatal("read aggregate snapshot after terminal purge")
	}
	if postPurgeSnapshot.QueuedJobs != 0 || postPurgeSnapshot.OldestQueuedAge != 0 ||
		postPurgeSnapshot.ExpiredLeases != 0 || postPurgeSnapshot.TerminalImagesPending != 0 ||
		postPurgeSnapshot.TerminalResultsPending != 0 {
		t.Fatal("maintenance snapshot retained terminal or active counts after purge")
	}
	queued, err := intake.Accept(ctx, "owner-retention-primary", "retention-queued-preserved", []byte("still active"), "image/png")
	if err != nil {
		t.Fatal("create queued image before offline orphan sweep")
	}
	orphan, err := images.Put(ctx, []byte("unreferenced synthetic image"), "image/jpeg")
	if err != nil {
		t.Fatal("create synthetic orphan object")
	}
	orphanCount, err := retainer.PurgeOrphansOffline(ctx)
	if err != nil || orphanCount != 1 {
		t.Fatalf("offline orphan sweep = (%d,%v), want one unreferenced object", orphanCount, err)
	}
	activeJob, err := repository.Lookup(ctx, "owner-retention-primary", "retention-queued-preserved")
	if err != nil || activeJob.ID != queued.JobID || activeJob.ImageKey == "" {
		t.Fatalf("offline cleanup lost queued job image reference: (%+v,%v)", activeJob, err)
	}
	if _, err := images.Open(ctx, activeJob.ImageKey); err != nil {
		t.Fatal("offline orphan sweep removed a referenced queued image")
	}
	if _, err := images.Open(ctx, orphan.Key); !errors.Is(err, imagestore.ErrNotFound) {
		t.Fatalf("offline orphan remains: %v", err)
	}
	assertImageObjectCount(t, imageRoot, 1)
}

func safeRetentionStorageFailure(err error) string {
	var failure *postgres.SafeStorageFailure
	if !errors.As(err, &failure) {
		return "unclassified"
	}
	return fmt.Sprintf("operation=%s type=%s sqlstate=%s constraint=%s", failure.Operation, failure.CauseType, failure.SQLState, failure.ConstraintName)
}
