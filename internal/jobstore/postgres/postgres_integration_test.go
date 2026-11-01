//go:build integration

package postgres_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"passport-backend/internal/imagestore"
	"passport-backend/internal/jobintake"
	"passport-backend/internal/jobs"
	"passport-backend/internal/jobstore/postgres"
)

const (
	testDSNFileEnv   = "PASSPORT_POSTGRES_TEST_DSN_FILE"
	testImageRootEnv = "PASSPORT_POSTGRES_TEST_IMAGE_ROOT"
	persistJobID     = "4dd683a3-9efa-4b31-9b5b-8ae58c0e2ca1"
	persistImage     = "a57f3945-08cd-441b-85d5-7962281034be"
	persistEvent     = "591d1d58-d38e-4ef8-8cf0-68f9b35d0c31"
)

func TestPostgresMigrateConcurrentAndSchemaConstraints(t *testing.T) {
	dsn := integrationDSN(t)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()

	stores := make([]*postgres.Store, 2)
	for i := range stores {
		store, err := postgres.Open(ctx, postgres.Config{
			DSN:                  dsn,
			ConnectTimeout:       10 * time.Second,
			MigrationLockTimeout: 20 * time.Second,
		})
		if err != nil {
			t.Fatalf("open PostgreSQL store %d: %v", i, err)
		}
		stores[i] = store
		t.Cleanup(store.Close)
	}
	pool := openTestPool(t, ctx, dsn)
	if err := stores[0].CheckSchema(ctx); !errors.Is(err, postgres.ErrSchemaNotReady) {
		t.Fatalf("CheckSchema() on an empty database = %v, want ErrSchemaNotReady", err)
	}
	var historyExists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('public.schema_migrations') IS NOT NULL").Scan(&historyExists); err != nil {
		t.Fatalf("verify CheckSchema() did not create migration history: %v", err)
	}
	if historyExists {
		t.Fatal("CheckSchema() created schema_migrations on an empty database")
	}

	errCh := make(chan error, len(stores))
	for _, store := range stores {
		go func(store *postgres.Store) {
			errCh <- store.Migrate(ctx)
		}(store)
	}
	for range stores {
		if err := <-errCh; err != nil {
			t.Fatalf("concurrent PostgreSQL migration: %v", err)
		}
	}
	if err := stores[0].Migrate(ctx); err != nil {
		t.Fatalf("reapplying PostgreSQL migrations: %v", err)
	}
	if err := stores[0].CheckSchema(ctx); err != nil {
		t.Fatalf("CheckSchema() after migrations: %v", err)
	}

	var migrationCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&migrationCount); err != nil {
		t.Fatalf("read migration history: %v", err)
	}
	if migrationCount != 9 {
		t.Fatalf("migration history rows = %d, want 9", migrationCount)
	}
	for _, table := range []string{"jobs", "job_attempts", "outbox", "client_installations", "schema_migrations"} {
		var exists bool
		if err := pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", "public."+table).Scan(&exists); err != nil {
			t.Fatalf("check table %q: %v", table, err)
		}
		if !exists {
			t.Fatalf("required table %q is missing", table)
		}
	}
	for _, index := range []string{"jobs_recovery_idx", "job_attempts_expired_lease_idx", "outbox_pending_idx", "outbox_expired_claim_idx"} {
		var exists bool
		if err := pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", "public."+index).Scan(&exists); err != nil {
			t.Fatalf("check index %q: %v", index, err)
		}
		if !exists {
			t.Fatalf("required index %q is missing", index)
		}
	}
	for _, column := range []string{"claim_token", "claim_until"} {
		var exists bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema='public' AND table_name='outbox' AND column_name=$1
			)`, column).Scan(&exists); err != nil {
			t.Fatalf("check outbox column %q: %v", column, err)
		}
		if !exists {
			t.Fatalf("required outbox column %q is missing", column)
		}
	}

	checksum := make([]byte, 32)
	if err := pool.QueryRow(ctx, "SELECT checksum FROM schema_migrations WHERE version = 1").Scan(&checksum); err != nil {
		t.Fatalf("read migration checksum: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE schema_migrations SET checksum = $1 WHERE version = 1", make([]byte, 32)); err != nil {
		t.Fatalf("prepare migration-drift check: %v", err)
	}
	if err := stores[0].Migrate(ctx); !errors.Is(err, postgres.ErrMigrationDrift) {
		t.Fatalf("Migrate() after changed checksum = %v, want ErrMigrationDrift", err)
	}
	if err := stores[0].CheckSchema(ctx); !errors.Is(err, postgres.ErrMigrationDrift) {
		t.Fatalf("CheckSchema() after changed checksum = %v, want ErrMigrationDrift", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE schema_migrations SET checksum = $1 WHERE version = 1", checksum); err != nil {
		t.Fatalf("restore migration checksum: %v", err)
	}

	var latestVersion int64
	var latestName string
	var latestChecksum []byte
	var latestApplied time.Time
	if err := pool.QueryRow(ctx, `
		SELECT version, name, checksum, applied_at
		FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&latestVersion, &latestName, &latestChecksum, &latestApplied); err != nil {
		t.Fatalf("read latest migration history row: %v", err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM schema_migrations WHERE version = $1", latestVersion); err != nil {
		t.Fatalf("prepare pending schema check: %v", err)
	}
	if err := stores[0].CheckSchema(ctx); !errors.Is(err, postgres.ErrSchemaNotReady) {
		t.Fatalf("CheckSchema() with a pending migration = %v, want ErrSchemaNotReady", err)
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES ($1, $2, $3, $4)",
		latestVersion, latestName, latestChecksum, latestApplied); err != nil {
		t.Fatalf("restore pending migration history row: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO schema_migrations (version, name, checksum) VALUES (9999, '9999_unknown', $1)", make([]byte, 32)); err != nil {
		t.Fatalf("prepare unknown migration check: %v", err)
	}
	if err := stores[0].CheckSchema(ctx); !errors.Is(err, postgres.ErrMigrationDrift) {
		t.Fatalf("CheckSchema() with an unknown migration = %v, want ErrMigrationDrift", err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM schema_migrations WHERE version = 9999"); err != nil {
		t.Fatalf("restore unknown migration history: %v", err)
	}
	if _, err := pool.Exec(ctx, "ALTER TABLE schema_migrations RENAME TO schema_migrations_stage3_backup"); err != nil {
		t.Fatalf("prepare missing schema history check: %v", err)
	}
	var historyRestored bool
	defer func() {
		if historyRestored {
			return
		}
		restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer restoreCancel()
		if _, err := pool.Exec(restoreCtx, "ALTER TABLE schema_migrations_stage3_backup RENAME TO schema_migrations"); err != nil {
			t.Errorf("restore migration history table during cleanup: %v", err)
		}
	}()
	if err := stores[0].CheckSchema(ctx); !errors.Is(err, postgres.ErrSchemaNotReady) {
		t.Fatalf("CheckSchema() without migration history table = %v, want ErrSchemaNotReady", err)
	}
	if _, err := pool.Exec(ctx, "ALTER TABLE schema_migrations_stage3_backup RENAME TO schema_migrations"); err != nil {
		t.Fatalf("restore migration history table: %v", err)
	}
	historyRestored = true
	if err := stores[0].CheckSchema(ctx); err != nil {
		t.Fatalf("CheckSchema() after restoring migration history: %v", err)
	}

	insertQueuedJob(t, ctx, pool, persistJobID, "owner-one", "same-key", persistImage, bytes32())
	insertOutbox(t, ctx, pool, persistEvent, persistJobID)

	// A key is unique within an owner; another owner may use the same key.
	insertQueuedJob(t, ctx, pool, "fef3be5c-1c7a-4269-8dc9-9bc97a6dbd9d", "owner-two", "same-key", "8c7a7efb-6f15-4cd3-b166-7cac513cb585", bytes32())
	assertPostgresError(t, insertQueuedJobErr(ctx, pool, "b564c1f2-8c82-439e-84c2-8ecff1b81598", "owner-one", "same-key", "fe252437-863d-4265-bfe9-ebea6d348c76", bytes32()), "23505")
	assertPostgresError(t, insertQueuedJobErr(ctx, pool, "c2487484-9ea8-4cb2-8ea5-a800600422ae", "owner-three", "bad key", "2f930986-2542-4229-a646-1b777d57f54b", bytes32()), "23514")
	assertPostgresError(t, insertQueuedJobErr(ctx, pool, "00760454-1646-44a0-ae7e-c85f78ff3f49", "owner-three", "ключ", "f0140b8e-424b-42b7-9f1e-2c120cab8c63", bytes32()), "23514")
	assertPostgresError(t, insertQueuedJobErr(ctx, pool, "28101a62-a71d-403f-85b9-482dce1891ba", "owner-three", "bad-digest", "acb3a333-04f9-4b7f-b0ef-d051c1d7fbc3", make([]byte, 31)), "23514")
	assertPostgresError(t, insertQueuedJobDetailsErr(ctx, pool, "781c9b7b-132f-4158-965a-d6481af3e430", "owner-three", "bad-media", "bd46742b-a48e-49e8-85b6-00a7c3127bd2", bytes32(), "image/gif", 12), "23514")
	assertPostgresError(t, insertQueuedJobDetailsErr(ctx, pool, "aa2e3138-68cc-4c29-bc66-0844e737778a", "owner-three", "bad-size", "140e2ad0-ae24-466b-b167-42a46f3f18eb", bytes32(), "image/jpeg", 0), "23514")
	assertPostgresError(t, insertQueuedJobErr(ctx, pool, "793fc819-bc29-4cf3-a22f-c6870784c658", "owner-three", "bad-image-uuid", "not-a-uuid", bytes32()), "22P02")

	err := insertLifecycleJobErr(ctx, pool, "947508ee-f73b-47de-850c-dcfcfc40a2e5", "invalid-state", "not-a-state", nil, nil, "", "")
	assertPostgresError(t, err, "23514")

	started := time.Now().UTC().Add(-time.Minute)
	finished := started.Add(30 * time.Second)
	for _, invalid := range []struct {
		name     string
		state    string
		started  *time.Time
		finished *time.Time
		result   string
		error    string
	}{
		{name: "processing-missing-start", state: "processing"},
		{name: "queued-with-result", state: "queued", result: `{}`},
		{name: "completed-missing-result", state: "completed", started: &started, finished: &finished},
		{name: "completed-missing-start", state: "completed", finished: &finished, result: `{}`},
		{name: "completed-missing-finish", state: "completed", started: &started, result: `{}`},
		{name: "failed-missing-error", state: "failed", started: &started, finished: &finished},
		{name: "failed-with-result", state: "failed", started: &started, finished: &finished, result: `{}`, error: "processing_failed"},
	} {
		err := insertLifecycleJobErr(ctx, pool, randomTestUUID(), invalid.name, invalid.state, invalid.started, invalid.finished, invalid.result, invalid.error)
		assertPostgresError(t, err, "23514")
	}
	if err := insertLifecycleJobErr(ctx, pool, randomTestUUID(), "valid-processing", "processing", &started, nil, "", ""); err != nil {
		t.Fatalf("insert valid processing job: %v", err)
	}
	if err := insertLifecycleJobErr(ctx, pool, randomTestUUID(), "valid-completed", "completed", &started, &finished, `{}`, ""); err != nil {
		t.Fatalf("insert valid completed job: %v", err)
	}
	if err := insertLifecycleJobErr(ctx, pool, randomTestUUID(), "valid-failed", "failed", &started, &finished, "", "processing_failed"); err != nil {
		t.Fatalf("insert valid failed job: %v", err)
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO jobs (job_id, owner_id, idempotency_key, request_digest, image_key,
		                  image_media_type, image_size_bytes, state, created_at, updated_at, available_at, started_at)
		VALUES ($1, 'owner-order', 'started-before-created', $2, $3, 'image/jpeg', 1,
		        'processing', now(), now(), now(), now() - interval '1 second')`,
		"6919de13-bcb4-4fe9-a7ae-10c8477e6d62", bytes32(), "991a0ce3-8139-4f79-8ccd-827791ac3816")
	assertPostgresError(t, err, "23514")

	_, err = pool.Exec(ctx, `
		INSERT INTO jobs (job_id, owner_id, idempotency_key, request_digest, image_key,
		                  image_media_type, image_size_bytes, state, created_at, updated_at,
		                  available_at, started_at, finished_at)
		VALUES ($1, 'owner-order', 'finished-before-started', $2, $3, 'image/jpeg', 1,
		        'failed', now() - interval '2 minutes', now(), now() - interval '2 minutes',
		        now(), now() - interval '1 second')`,
		"fe1ac72f-393d-4b13-817b-62e540e956ee", bytes32(), "05035b96-a60f-4f70-8f18-2f6825abecdb")
	assertPostgresError(t, err, "23514")

	_, err = pool.Exec(ctx, `
		INSERT INTO outbox (event_id, job_id, event_type, schema_version, attempt_no)
		VALUES ($1, $2, 'recognition.requested', 'v1', 1)`,
		"06108061-df99-4a28-9513-d192687e297f", "091634f7-d3f4-4d30-a44f-bfd662a19d0a")
	assertPostgresError(t, err, "23503")

	_, err = pool.Exec(ctx, `
		INSERT INTO outbox (event_id, job_id, event_type, schema_version, attempt_no)
		VALUES ($1, $2, 'recognition.requested', 'v1', 0)`,
		"e7345a1b-13eb-4a6c-a56e-c04408887622", persistJobID)
	assertPostgresError(t, err, "23514")
}

func TestPostgresRowsSurviveRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	pool := openTestPool(t, ctx, integrationDSN(t))

	var state string
	if err := pool.QueryRow(ctx, "SELECT state FROM jobs WHERE job_id = $1", persistJobID).Scan(&state); err != nil {
		t.Fatalf("read persisted job: %v", err)
	}
	if state != "queued" {
		t.Fatalf("persisted job state = %q, want queued", state)
	}
	var jobID string
	if err := pool.QueryRow(ctx, "SELECT job_id::text FROM outbox WHERE event_id = $1", persistEvent).Scan(&jobID); err != nil {
		t.Fatalf("read persisted outbox row: %v", err)
	}
	if jobID != persistJobID {
		t.Fatalf("persisted outbox job_id = %s, want %s", jobID, persistJobID)
	}
}

func TestPostgresRepositoryCreateQueued(t *testing.T) {
	dsn := integrationDSN(t)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	store, err := postgres.Open(ctx, postgres.Config{
		DSN:                  dsn,
		ConnectTimeout:       10 * time.Second,
		MigrationLockTimeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("open PostgreSQL repository: %v", err)
	}
	t.Cleanup(store.Close)
	if err := store.CheckSchema(ctx); err != nil {
		t.Fatalf("CheckSchema() before repository operations: %v", err)
	}
	pool := openTestPool(t, ctx, dsn)

	digest := sha256.Sum256([]byte("integration image bytes one"))
	submission := newSubmission("owner-repository-one", "repo-idempotency-key", digest)
	created, outcome, err := store.CreateQueued(ctx, submission)
	if err != nil || outcome != jobs.OutcomeCreated {
		t.Fatalf("CreateQueued() = outcome %v, err %v; want created", outcome, err)
	}
	if created.ID != submission.JobID || created.OwnerID != submission.OwnerID ||
		created.IdempotencyKey != submission.IdempotencyKey || created.RequestDigest != digest ||
		created.ImageKey != submission.ImageKey || created.MediaType != submission.MediaType ||
		created.Size != submission.Size || created.Status != jobs.StatusQueued || created.CreatedAt.IsZero() {
		t.Fatalf("CreateQueued() returned unexpected job metadata: %+v", created)
	}
	assertRepositoryRowCounts(t, ctx, pool, submission.OwnerID, submission.IdempotencyKey, 1, 1, 0)
	var eventType, schemaVersion string
	var attemptNo int
	if err := pool.QueryRow(ctx, `
		SELECT event_type, schema_version, attempt_no
		FROM outbox WHERE job_id = $1`, submission.JobID).Scan(&eventType, &schemaVersion, &attemptNo); err != nil {
		t.Fatalf("read initial outbox event: %v", err)
	}
	if eventType != "recognition.requested" || schemaVersion != "v1" || attemptNo != 1 {
		t.Fatalf("initial outbox tuple = (%q, %q, %d), want (recognition.requested, v1, 1)", eventType, schemaVersion, attemptNo)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE jobs SET state = 'processing', started_at = now(), updated_at = now()
		WHERE job_id = $1`, submission.JobID); err != nil {
		t.Fatalf("advance replay fixture state: %v", err)
	}
	replaySubmission := newSubmission(submission.OwnerID, submission.IdempotencyKey, digest)
	replayed, outcome, err := store.CreateQueued(ctx, replaySubmission)
	if err != nil || outcome != jobs.OutcomeReplay {
		t.Fatalf("same-key/same-digest CreateQueued() = outcome %v, err %v; want replay", outcome, err)
	}
	if replayed.ID != created.ID || replayed.Status != jobs.StatusProcessing || !replayed.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("replay returned changed job identity/state/time: first=%+v replay=%+v", created, replayed)
	}
	assertRepositoryRowCounts(t, ctx, pool, submission.OwnerID, submission.IdempotencyKey, 1, 1, 0)

	changedDigest := sha256.Sum256([]byte("different image bytes"))
	conflictSubmission := newSubmission(submission.OwnerID, submission.IdempotencyKey, changedDigest)
	_, outcome, err = store.CreateQueued(ctx, conflictSubmission)
	if err != nil || outcome != jobs.OutcomeConflict {
		t.Fatalf("same-key/different-digest CreateQueued() = outcome %v, err %v; want conflict", outcome, err)
	}
	assertRepositoryRowCounts(t, ctx, pool, submission.OwnerID, submission.IdempotencyKey, 1, 1, 0)

	otherOwnerSubmission := newSubmission("owner-repository-two", submission.IdempotencyKey, digest)
	otherOwnerJob, outcome, err := store.CreateQueued(ctx, otherOwnerSubmission)
	if err != nil || outcome != jobs.OutcomeCreated || otherOwnerJob.ID != otherOwnerSubmission.JobID {
		t.Fatalf("same key for a different owner = job %+v, outcome %v, err %v; want independent creation", otherOwnerJob, outcome, err)
	}
	assertRepositoryRowCounts(t, ctx, pool, otherOwnerSubmission.OwnerID, otherOwnerSubmission.IdempotencyKey, 1, 1, 0)

	// A duplicate event ID fails after the job INSERT. The transaction must
	// roll the new job back instead of leaving a job without its outbox row.
	seed := newSubmission("owner-atomic-seed", "atomic-seed-key", digest)
	if _, outcome, err := store.CreateQueued(ctx, seed); err != nil || outcome != jobs.OutcomeCreated {
		t.Fatalf("CreateQueued() atomic failure seed = outcome %v, err %v; want created", outcome, err)
	}
	atomicFailure := newSubmission("owner-atomic-rollback", "atomic-rollback-key", digest)
	atomicFailure.EventID = seed.EventID
	_, _, err = store.CreateQueued(ctx, atomicFailure)
	if !errors.Is(err, jobs.ErrUnavailable) {
		t.Fatalf("CreateQueued() with duplicate event ID = %v, want ErrUnavailable", err)
	}
	if _, err := store.Lookup(ctx, atomicFailure.OwnerID, atomicFailure.IdempotencyKey); !errors.Is(err, jobs.ErrNotFound) {
		t.Fatalf("Lookup() after outbox insert rollback = %v, want ErrNotFound", err)
	}
	assertRepositoryRowCounts(t, ctx, pool, atomicFailure.OwnerID, atomicFailure.IdempotencyKey, 0, 0, 0)

	stores := make([]*postgres.Store, 8)
	for i := range stores {
		concurrentStore, err := postgres.Open(ctx, postgres.Config{
			DSN:                  dsn,
			ConnectTimeout:       10 * time.Second,
			MigrationLockTimeout: 20 * time.Second,
		})
		if err != nil {
			t.Fatalf("open concurrent repository %d: %v", i, err)
		}
		stores[i] = concurrentStore
		t.Cleanup(concurrentStore.Close)
	}

	const sameRequestCount = 50
	sameKey := "concurrent-same-digest"
	sameDigest := sha256.Sum256([]byte("concurrent image bytes"))
	sameRequests := make([]jobs.Submission, sameRequestCount)
	for i := range sameRequests {
		sameRequests[i] = newSubmission("owner-concurrent-same", sameKey, sameDigest)
	}
	sameResults := runConcurrentSubmissions(ctx, stores, sameRequests)
	createdCount, replayCount := 0, 0
	winnerID := ""
	for _, result := range sameResults {
		if result.err != nil {
			t.Fatalf("concurrent same-digest CreateQueued(): %v", result.err)
		}
		switch result.outcome {
		case jobs.OutcomeCreated:
			createdCount++
		case jobs.OutcomeReplay:
			replayCount++
		default:
			t.Fatalf("concurrent same-digest outcome = %v, want created or replay", result.outcome)
		}
		if winnerID == "" {
			winnerID = result.job.ID
		}
		if result.job.ID != winnerID {
			t.Fatalf("concurrent same-digest requests returned different job IDs: %q and %q", winnerID, result.job.ID)
		}
	}
	if createdCount != 1 || replayCount != sameRequestCount-1 {
		t.Fatalf("concurrent same-digest outcomes: created=%d replay=%d, want 1 and %d", createdCount, replayCount, sameRequestCount-1)
	}
	assertRepositoryRowCounts(t, ctx, pool, "owner-concurrent-same", sameKey, 1, 1, 0)

	const changedRequestCount = 50
	changedKey := "concurrent-different-digests"
	digestA := sha256.Sum256([]byte("concurrent content A"))
	digestB := sha256.Sum256([]byte("concurrent content B"))
	changedRequests := make([]jobs.Submission, changedRequestCount)
	for i := range changedRequests {
		selectedDigest := digestA
		if i%2 != 0 {
			selectedDigest = digestB
		}
		changedRequests[i] = newSubmission("owner-concurrent-different", changedKey, selectedDigest)
	}
	changedResults := runConcurrentSubmissions(ctx, stores, changedRequests)
	createdCount, replayCount = 0, 0
	conflictCount := 0
	winnerDigest := [sha256.Size]byte{}
	winnerID = ""
	for _, result := range changedResults {
		if result.err != nil {
			t.Fatalf("concurrent different-digest CreateQueued(): %v", result.err)
		}
		switch result.outcome {
		case jobs.OutcomeCreated:
			createdCount++
			winnerDigest = result.job.RequestDigest
			winnerID = result.job.ID
		case jobs.OutcomeReplay:
			replayCount++
			if winnerID == "" {
				winnerID = result.job.ID
				winnerDigest = result.job.RequestDigest
			} else if result.job.ID != winnerID {
				t.Fatalf("concurrent replay returned job %q, want %q", result.job.ID, winnerID)
			}
		case jobs.OutcomeConflict:
			conflictCount++
		default:
			t.Fatalf("concurrent different-digest outcome = %v, want created, replay or conflict", result.outcome)
		}
	}
	if createdCount != 1 || conflictCount == 0 || createdCount+replayCount+conflictCount != changedRequestCount {
		t.Fatalf("concurrent different-digest outcomes: created=%d replay=%d conflict=%d, want one creator, conflicts and %d total", createdCount, replayCount, conflictCount, changedRequestCount)
	}
	var winnerDigestHex string
	if err := pool.QueryRow(ctx, `
		SELECT encode(request_digest, 'hex') FROM jobs
		WHERE owner_id = $1 AND idempotency_key = $2`, "owner-concurrent-different", changedKey).Scan(&winnerDigestHex); err != nil {
		t.Fatalf("read concurrent digest winner: %v", err)
	}
	if winnerID == "" || winnerDigest == ([sha256.Size]byte{}) || winnerDigestHex != fmt.Sprintf("%x", winnerDigest) {
		t.Fatalf("concurrent digest winner was inconsistent: job=%q digest=%s", winnerID, winnerDigestHex)
	}
	assertRepositoryRowCounts(t, ctx, pool, "owner-concurrent-different", changedKey, 1, 1, 0)
}

func TestPostgresJobIntakeUsesRealFilesystemAndDatabase(t *testing.T) {
	dsn := integrationDSN(t)
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()
	service, repository, images, imageRoot, pool := newIntegrationIntake(t, ctx, dsn, 1<<20)

	data := []byte("synthetic durable image bytes for PostgreSQL and image storage")
	accepted, err := service.Accept(ctx, "owner-intake-one", "intake-replay-key", data, "image/png")
	if err != nil || accepted.Status != jobs.StatusQueued || accepted.JobID == "" || accepted.CreatedAt.IsZero() {
		t.Fatalf("first Accept() = %+v, %v; want durable queued acceptance", accepted, err)
	}
	job, err := repository.Lookup(ctx, "owner-intake-one", "intake-replay-key")
	if err != nil {
		t.Fatalf("Lookup() after acceptance: %v", err)
	}
	if job.ID != accepted.JobID || job.OwnerID != "owner-intake-one" || job.Status != jobs.StatusQueued ||
		job.Size != int64(len(data)) || job.MediaType != "image/png" || job.RequestDigest != expectedIntakeDigest(data, "image/png") {
		t.Fatalf("persisted job metadata does not match accepted image: %+v", job)
	}
	assertRepositoryRowCounts(t, ctx, pool, job.OwnerID, job.IdempotencyKey, 1, 1, 0)
	assertInitialOutbox(t, ctx, pool, job.ID)
	assertImageObjectCount(t, imageRoot, 1)
	storedFile, err := images.Open(ctx, job.ImageKey)
	if err != nil {
		t.Fatalf("open accepted image: %v", err)
	}
	storedBytes, readErr := io.ReadAll(storedFile)
	closeErr := storedFile.Close()
	if readErr != nil || closeErr != nil || string(storedBytes) != string(data) {
		t.Fatalf("accepted image bytes did not survive storage: read=%v close=%v", readErr, closeErr)
	}

	replay, err := service.Accept(ctx, "owner-intake-one", "intake-replay-key", data, "image/png")
	if err != nil || replay.JobID != accepted.JobID || replay.Status != accepted.Status || !replay.CreatedAt.Equal(accepted.CreatedAt) {
		t.Fatalf("same-request replay = %+v, %v; want original job/state/time", replay, err)
	}
	if _, err := service.Accept(ctx, "owner-intake-one", "intake-replay-key", []byte("changed content"), "image/png"); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("same-key/different-content error = %v, want conflict", err)
	}
	if _, err := service.Accept(ctx, "owner-intake-one", "intake-replay-key", data, "image/jpeg"); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("same-key/same-bytes/different-MIME error = %v, want conflict", err)
	}
	assertRepositoryRowCounts(t, ctx, pool, job.OwnerID, job.IdempotencyKey, 1, 1, 0)
	assertImageObjectCount(t, imageRoot, 1)

	otherOwner, err := service.Accept(ctx, "owner-intake-two", "intake-replay-key", data, "image/png")
	if err != nil || otherOwner.JobID == accepted.JobID {
		t.Fatalf("same idempotency key under another owner = %+v, %v; want independent job", otherOwner, err)
	}
	assertRepositoryRowCounts(t, ctx, pool, "owner-intake-two", "intake-replay-key", 1, 1, 0)
	assertImageObjectCount(t, imageRoot, 2)

	const sameKeyRequestCount = 50
	sameKeyRequests := make([]intakeRequest, sameKeyRequestCount)
	for i := range sameKeyRequests {
		sameKeyRequests[i] = intakeRequest{owner: "owner-intake-concurrent", key: "same-request", data: []byte("one concurrent request body"), mediaType: "image/jpeg"}
	}
	sameKeyResults := runConcurrentAccepts(ctx, service, sameKeyRequests)
	sameJobID := ""
	for _, result := range sameKeyResults {
		if result.err != nil {
			t.Fatalf("concurrent same-key Accept(): %v", result.err)
		}
		if sameJobID == "" {
			sameJobID = result.accepted.JobID
		}
		if result.accepted.JobID != sameJobID || result.accepted.Status != jobs.StatusQueued {
			t.Fatalf("concurrent same-key requests diverged: %+v, want job %s queued", result.accepted, sameJobID)
		}
	}
	assertRepositoryRowCounts(t, ctx, pool, "owner-intake-concurrent", "same-request", 1, 1, 0)
	assertImageObjectCount(t, imageRoot, 3)

	const changedKeyRequestCount = 50
	changedKeyRequests := make([]intakeRequest, changedKeyRequestCount)
	for i := range changedKeyRequests {
		body := []byte("concurrent request content A")
		if i%2 != 0 {
			body = []byte("concurrent request content B")
		}
		changedKeyRequests[i] = intakeRequest{owner: "owner-intake-race", key: "same-key-different-payload", data: body, mediaType: "image/png"}
	}
	changedKeyResults := runConcurrentAccepts(ctx, service, changedKeyRequests)
	acceptedCount, conflictCount := 0, 0
	changedWinnerID := ""
	for _, result := range changedKeyResults {
		switch {
		case result.err == nil:
			acceptedCount++
			if changedWinnerID == "" {
				changedWinnerID = result.accepted.JobID
			}
			if result.accepted.JobID != changedWinnerID {
				t.Fatalf("same-key race accepted different jobs: %q and %q", changedWinnerID, result.accepted.JobID)
			}
		case errors.Is(result.err, jobs.ErrConflict):
			conflictCount++
		default:
			t.Fatalf("same-key/different-content race returned unexpected result: %+v", result)
		}
	}
	if acceptedCount == 0 || conflictCount == 0 || acceptedCount+conflictCount != changedKeyRequestCount {
		t.Fatalf("same-key/different-content results accepted=%d conflict=%d, want both outcomes across %d requests", acceptedCount, conflictCount, changedKeyRequestCount)
	}
	assertRepositoryRowCounts(t, ctx, pool, "owner-intake-race", "same-key-different-payload", 1, 1, 0)
	assertImageObjectCount(t, imageRoot, 4)

	const distinctUploadCount = 100
	distinctRequests := make([]intakeRequest, distinctUploadCount)
	for i := range distinctRequests {
		distinctRequests[i] = intakeRequest{
			owner: "owner-intake-bulk", key: fmt.Sprintf("bulk-%03d", i),
			data: []byte(fmt.Sprintf("small synthetic image %03d", i)), mediaType: "image/webp",
		}
	}
	distinctResults := runConcurrentAccepts(ctx, service, distinctRequests)
	distinctIDs := make(map[string]struct{}, distinctUploadCount)
	for _, result := range distinctResults {
		if result.err != nil || result.accepted.JobID == "" || result.accepted.Status != jobs.StatusQueued {
			t.Fatalf("distinct small upload = %+v, %v; want queued acceptance", result.accepted, result.err)
		}
		if _, exists := distinctIDs[result.accepted.JobID]; exists {
			t.Fatalf("distinct uploads reused job ID %q", result.accepted.JobID)
		}
		distinctIDs[result.accepted.JobID] = struct{}{}
	}
	assertOwnerPrefixRowCounts(t, ctx, pool, "owner-intake-bulk", "bulk-%", distinctUploadCount, distinctUploadCount, 0)
	assertImageObjectCount(t, imageRoot, 4+distinctUploadCount)
}

func TestPostgresJobIntakeConservativeErrorsAndCommitAckLoss(t *testing.T) {
	dsn := integrationDSN(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	t.Run("storage failure does not create database rows", func(t *testing.T) {
		service, repository, _, imageRoot, pool := newIntegrationIntake(t, ctx, dsn, 1)
		_, err := service.Accept(ctx, "owner-intake-storage-failure", "storage-failure", []byte("too large for quota"), "image/jpeg")
		if !errors.Is(err, imagestore.ErrCapacityExceeded) {
			t.Fatalf("Accept() at full real volume = %v, want ErrCapacityExceeded", err)
		}
		if _, err := repository.Lookup(ctx, "owner-intake-storage-failure", "storage-failure"); !errors.Is(err, jobs.ErrNotFound) {
			t.Fatalf("Lookup() after storage failure = %v, want ErrNotFound", err)
		}
		assertRepositoryRowCounts(t, ctx, pool, "owner-intake-storage-failure", "storage-failure", 0, 0, 0)
		assertImageObjectCount(t, imageRoot, 0)
	})

	t.Run("same-owner replay succeeds when the image volume is full", func(t *testing.T) {
		service, repository, _, imageRoot, pool := newIntegrationIntake(t, ctx, dsn, 128)
		data := bytesOfSize(128)
		first, err := service.Accept(ctx, "owner-intake-full-replay", "full-replay", data, "image/jpeg")
		if err != nil {
			t.Fatalf("initial acceptance at exact volume capacity: %v", err)
		}
		replay, err := service.Accept(ctx, "owner-intake-full-replay", "full-replay", data, "image/jpeg")
		if err != nil || replay.JobID != first.JobID || !replay.CreatedAt.Equal(first.CreatedAt) {
			t.Fatalf("same-key replay at full volume = %+v, %v; want original job", replay, err)
		}
		if _, err := service.Accept(ctx, "owner-intake-full-replay", "new-key-at-capacity", []byte("x"), "image/jpeg"); !errors.Is(err, imagestore.ErrCapacityExceeded) {
			t.Fatalf("new upload at full volume = %v, want ErrCapacityExceeded", err)
		}
		assertRepositoryRowCounts(t, ctx, pool, "owner-intake-full-replay", "full-replay", 1, 1, 0)
		assertRepositoryRowCounts(t, ctx, pool, "owner-intake-full-replay", "new-key-at-capacity", 0, 0, 0)
		if _, err := repository.Lookup(ctx, "owner-intake-full-replay", "new-key-at-capacity"); !errors.Is(err, jobs.ErrNotFound) {
			t.Fatalf("Lookup() for rejected new upload = %v, want ErrNotFound", err)
		}
		assertImageObjectCount(t, imageRoot, 1)
	})

	t.Run("repository error after upload leaves only a conservative orphan", func(t *testing.T) {
		_, repository, images, imageRoot, pool := newIntegrationIntake(t, ctx, dsn, 1<<20)
		faultyRepository := &repositoryFailure{base: repository, failBeforeCreate: true}
		faultyService, err := jobintake.New(faultyRepository, images)
		if err != nil {
			t.Fatalf("construct fault-injected intake service: %v", err)
		}
		_, err = faultyService.Accept(ctx, "owner-intake-repository-failure", "repository-failure", []byte("saved image, failed repository"), "image/png")
		if !errors.Is(err, jobs.ErrUnavailable) {
			t.Fatalf("Accept() after repository failure = %v, want ErrUnavailable", err)
		}
		if _, err := repository.Lookup(ctx, "owner-intake-repository-failure", "repository-failure"); !errors.Is(err, jobs.ErrNotFound) {
			t.Fatalf("Lookup() after known precommit failure = %v, want ErrNotFound", err)
		}
		assertRepositoryRowCounts(t, ctx, pool, "owner-intake-repository-failure", "repository-failure", 0, 0, 0)
		assertImageObjectCount(t, imageRoot, 1)
	})

	t.Run("commit acknowledgment loss retries to the single committed job", func(t *testing.T) {
		_, repository, images, imageRoot, pool := newIntegrationIntake(t, ctx, dsn, 1<<20)
		faultyRepository := &repositoryFailure{base: repository}
		faultyRepository.loseCommitAck.Store(true)
		faultyService, err := jobintake.New(faultyRepository, images)
		if err != nil {
			t.Fatalf("construct ack-loss intake service: %v", err)
		}
		owner, key, data := "owner-intake-ack-loss", "commit-ack-loss", []byte("committed despite lost acknowledgment")
		if _, err := faultyService.Accept(ctx, owner, key, data, "image/webp"); !errors.Is(err, jobs.ErrUnavailable) {
			t.Fatalf("first Accept() after simulated commit-ack loss = %v, want ErrUnavailable", err)
		}
		committed, err := repository.Lookup(ctx, owner, key)
		if err != nil {
			t.Fatalf("actual committed row not found after acknowledgment loss: %v", err)
		}
		assertRepositoryRowCounts(t, ctx, pool, owner, key, 1, 1, 0)
		assertImageObjectCount(t, imageRoot, 1)
		retry, err := faultyService.Accept(ctx, owner, key, data, "image/webp")
		if err != nil || retry.JobID != committed.ID || retry.Status != committed.Status || !retry.CreatedAt.Equal(committed.CreatedAt) {
			t.Fatalf("retry after actual commit = %+v, %v; want original committed job", retry, err)
		}
		assertRepositoryRowCounts(t, ctx, pool, owner, key, 1, 1, 0)
		assertImageObjectCount(t, imageRoot, 1)
	})
}

type createResult struct {
	job     jobs.Job
	outcome jobs.Outcome
	err     error
}

type intakeRequest struct {
	owner     string
	key       string
	data      []byte
	mediaType string
}

type acceptResult struct {
	accepted jobs.Accepted
	err      error
}

type repositoryFailure struct {
	base             jobs.Repository
	failBeforeCreate bool
	loseCommitAck    atomic.Bool
}

func (r *repositoryFailure) Lookup(ctx context.Context, ownerID, key string) (jobs.Job, error) {
	return r.base.Lookup(ctx, ownerID, key)
}

func (r *repositoryFailure) CreateQueued(ctx context.Context, submission jobs.Submission) (jobs.Job, jobs.Outcome, error) {
	if r.failBeforeCreate {
		return jobs.Job{}, 0, jobs.ErrUnavailable
	}
	job, outcome, err := r.base.CreateQueued(ctx, submission)
	if err == nil && outcome == jobs.OutcomeCreated && r.loseCommitAck.CompareAndSwap(true, false) {
		// This fault is injected only after the real database transaction returned
		// success; it models a lost application acknowledgment, not a PG fault.
		return jobs.Job{}, 0, jobs.ErrUnavailable
	}
	return job, outcome, err
}

func newIntegrationIntake(t *testing.T, ctx context.Context, dsn string, maxTotalBytes int64) (*jobintake.Service, *postgres.Store, *imagestore.Store, string, *pgxpool.Pool) {
	t.Helper()
	store, err := postgres.Open(ctx, postgres.Config{
		DSN:                  dsn,
		ConnectTimeout:       10 * time.Second,
		MigrationLockTimeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("open PostgreSQL intake repository: %v", err)
	}
	t.Cleanup(store.Close)
	if err := store.CheckSchema(ctx); err != nil {
		t.Fatalf("CheckSchema() before intake integration: %v", err)
	}
	rootParent := strings.TrimSpace(os.Getenv(testImageRootEnv))
	if rootParent != "/integration-images" {
		t.Fatalf("integration image root must use the private named volume, got %q", rootParent)
	}
	imageRoot, err := os.MkdirTemp(rootParent, "intake-")
	if err != nil {
		t.Fatalf("create private image test root: %v", err)
	}
	images, err := imagestore.New(imagestore.Config{Root: imageRoot, MaxBytes: 1 << 20, MaxTotalBytes: maxTotalBytes})
	if err != nil {
		_ = os.RemoveAll(imageRoot)
		t.Fatalf("create real image store: %v", err)
	}
	t.Cleanup(func() {
		if err := images.Close(); err != nil {
			t.Errorf("close real image store: %v", err)
		}
		if err := os.RemoveAll(imageRoot); err != nil {
			t.Errorf("remove only this test's private image root: %v", err)
		}
	})
	service, err := jobintake.New(store, images)
	if err != nil {
		t.Fatalf("construct real PostgreSQL+filesystem intake service: %v", err)
	}
	pool := openTestPool(t, ctx, dsn)
	return service, store, images, imageRoot, pool
}

func newSubmission(ownerID, idempotencyKey string, digest [sha256.Size]byte) jobs.Submission {
	return jobs.Submission{
		JobID:          randomTestUUID(),
		EventID:        randomTestUUID(),
		OwnerID:        ownerID,
		IdempotencyKey: idempotencyKey,
		RequestDigest:  digest,
		ImageKey:       randomTestUUID(),
		ImageSHA256:    strings.Repeat("a", 64),
		MediaType:      "image/jpeg",
		Size:           int64(len(digest)),
	}
}

func runConcurrentSubmissions(ctx context.Context, stores []*postgres.Store, submissions []jobs.Submission) []createResult {
	start := make(chan struct{})
	results := make(chan createResult, len(submissions))
	var workers sync.WaitGroup
	workers.Add(len(submissions))
	for i, submission := range submissions {
		store := stores[i%len(stores)]
		go func(submission jobs.Submission) {
			defer workers.Done()
			<-start
			job, outcome, err := store.CreateQueued(ctx, submission)
			results <- createResult{job: job, outcome: outcome, err: err}
		}(submission)
	}
	close(start)
	workers.Wait()
	close(results)
	out := make([]createResult, 0, len(submissions))
	for result := range results {
		out = append(out, result)
	}
	return out
}

func runConcurrentAccepts(ctx context.Context, service *jobintake.Service, requests []intakeRequest) []acceptResult {
	start := make(chan struct{})
	results := make(chan acceptResult, len(requests))
	var workers sync.WaitGroup
	workers.Add(len(requests))
	for _, request := range requests {
		go func(request intakeRequest) {
			defer workers.Done()
			<-start
			accepted, err := service.Accept(ctx, request.owner, request.key, request.data, request.mediaType)
			results <- acceptResult{accepted: accepted, err: err}
		}(request)
	}
	close(start)
	workers.Wait()
	close(results)
	out := make([]acceptResult, 0, len(requests))
	for result := range results {
		out = append(out, result)
	}
	return out
}

func expectedIntakeDigest(data []byte, normalizedMediaType string) [sha256.Size]byte {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte("passport-job-request-v1\x00"))
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(normalizedMediaType)))
	_, _ = hasher.Write(size[:])
	_, _ = hasher.Write([]byte(normalizedMediaType))
	binary.BigEndian.PutUint64(size[:], uint64(len(data)))
	_, _ = hasher.Write(size[:])
	_, _ = hasher.Write(data)
	var digest [sha256.Size]byte
	copy(digest[:], hasher.Sum(nil))
	return digest
}

func bytesOfSize(size int) []byte { return []byte(strings.Repeat("x", size)) }

func assertInitialOutbox(t *testing.T, ctx context.Context, pool *pgxpool.Pool, jobID string) {
	t.Helper()
	var eventType, version string
	var attempt int
	if err := pool.QueryRow(ctx, `
		SELECT event_type, schema_version, attempt_no
		FROM outbox WHERE job_id = $1`, jobID).Scan(&eventType, &version, &attempt); err != nil {
		t.Fatalf("read initial outbox row for job %q: %v", jobID, err)
	}
	if eventType != "recognition.requested" || version != "v1" || attempt != 1 {
		t.Fatalf("initial outbox metadata = (%q, %q, %d), want (recognition.requested, v1, 1)", eventType, version, attempt)
	}
}

func assertOwnerPrefixRowCounts(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ownerID, keyPattern string, wantJobs, wantOutbox, wantAttempts int64) {
	t.Helper()
	var jobsCount, outboxCount, attemptsCount int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM jobs WHERE owner_id = $1 AND idempotency_key LIKE $2`, ownerID, keyPattern).Scan(&jobsCount); err != nil {
		t.Fatalf("count jobs by owner/key prefix: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox o JOIN jobs j USING (job_id)
		WHERE j.owner_id = $1 AND j.idempotency_key LIKE $2`, ownerID, keyPattern).Scan(&outboxCount); err != nil {
		t.Fatalf("count outbox rows by owner/key prefix: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM job_attempts a JOIN jobs j USING (job_id)
		WHERE j.owner_id = $1 AND j.idempotency_key LIKE $2`, ownerID, keyPattern).Scan(&attemptsCount); err != nil {
		t.Fatalf("count attempts by owner/key prefix: %v", err)
	}
	if jobsCount != wantJobs || outboxCount != wantOutbox || attemptsCount != wantAttempts {
		t.Fatalf("row counts for owner/key prefix %q/%q = jobs:%d outbox:%d attempts:%d, want %d/%d/%d", ownerID, keyPattern,
			jobsCount, outboxCount, attemptsCount, wantJobs, wantOutbox, wantAttempts)
	}
}

func assertImageObjectCount(t *testing.T, root string, want int) {
	t.Helper()
	count := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("count files only under this integration's private image root: %v", err)
	}
	if count != want {
		t.Fatalf("regular image-store files = %d, want %d", count, want)
	}
}

func assertRepositoryRowCounts(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ownerID, key string, wantJobs, wantOutbox, wantAttempts int64) {
	t.Helper()
	var jobsCount, outboxCount, attemptsCount int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM jobs WHERE owner_id = $1 AND idempotency_key = $2`, ownerID, key).Scan(&jobsCount); err != nil {
		t.Fatalf("count jobs for owner/key: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox o JOIN jobs j USING (job_id)
		WHERE j.owner_id = $1 AND j.idempotency_key = $2`, ownerID, key).Scan(&outboxCount); err != nil {
		t.Fatalf("count outbox rows for owner/key: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM job_attempts a JOIN jobs j USING (job_id)
		WHERE j.owner_id = $1 AND j.idempotency_key = $2`, ownerID, key).Scan(&attemptsCount); err != nil {
		t.Fatalf("count attempts for owner/key: %v", err)
	}
	if jobsCount != wantJobs || outboxCount != wantOutbox || attemptsCount != wantAttempts {
		t.Fatalf("row counts for owner/key %q/%q = jobs:%d outbox:%d attempts:%d, want %d/%d/%d", ownerID, key,
			jobsCount, outboxCount, attemptsCount, wantJobs, wantOutbox, wantAttempts)
	}
}

func integrationDSN(t *testing.T) string {
	t.Helper()
	path := strings.TrimSpace(os.Getenv(testDSNFileEnv))
	if path == "" {
		t.Fatal("integration tests require PASSPORT_POSTGRES_TEST_DSN_FILE")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read private test DSN file: %v", err)
	}
	dsn := strings.TrimSpace(string(data))
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("private PostgreSQL test DSN did not parse")
	}
	if config.ConnConfig.Host != "postgres" || !strings.HasPrefix(config.ConnConfig.Database, "passport_pg_it_") || config.ConnConfig.User != "passport_test" {
		t.Fatalf("integration test requires the isolated Compose database (host=%q, databasePrefix=%t, user=%q)", config.ConnConfig.Host, strings.HasPrefix(config.ConnConfig.Database, "passport_pg_it_"), config.ConnConfig.User)
	}
	return dsn
}

func openTestPool(t *testing.T, ctx context.Context, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to isolated PostgreSQL test instance: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping isolated PostgreSQL test instance: %v", err)
	}
	return pool
}

func bytes32() []byte { return []byte(strings.Repeat("x", 32)) }

func insertQueuedJob(t *testing.T, ctx context.Context, pool *pgxpool.Pool, jobID, ownerID, key, imageKey string, digest []byte) {
	t.Helper()
	if err := insertQueuedJobErr(ctx, pool, jobID, ownerID, key, imageKey, digest); err != nil {
		t.Fatalf("insert valid queued job: %v", err)
	}
}

func insertQueuedJobErr(ctx context.Context, pool *pgxpool.Pool, jobID, ownerID, key, imageKey string, digest []byte) error {
	return insertQueuedJobDetailsErr(ctx, pool, jobID, ownerID, key, imageKey, digest, "image/jpeg", 12)
}

func insertQueuedJobDetailsErr(ctx context.Context, pool *pgxpool.Pool, jobID, ownerID, key, imageKey string, digest []byte, mediaType string, sizeBytes int64) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO jobs (job_id, owner_id, idempotency_key, request_digest, image_key, image_media_type, image_size_bytes)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, jobID, ownerID, key, digest, imageKey, mediaType, sizeBytes)
	return err
}

func insertLifecycleJobErr(ctx context.Context, pool *pgxpool.Pool, jobID, key, state string, started, finished *time.Time, result, errorCode string) error {
	created := time.Now().UTC().Add(-2 * time.Minute)
	updated := time.Now().UTC()
	var startedValue, finishedValue any
	if started != nil {
		startedValue = *started
	}
	if finished != nil {
		finishedValue = *finished
	}
	var resultValue, errorValue any
	if result != "" {
		resultValue = result
	}
	if errorCode != "" {
		errorValue = errorCode
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO jobs (job_id, owner_id, idempotency_key, request_digest, image_key,
		                  image_media_type, image_size_bytes, state, created_at, updated_at,
		                  available_at, started_at, finished_at, result, error_code)
		VALUES ($1, 'owner-lifecycle', $2, $3, $4, 'image/jpeg', 12, $5, $6, $7,
		        $6, $8, $9, CASE WHEN $10::text IS NULL THEN NULL ELSE $10::jsonb END, $11)`,
		jobID, key, bytes32(), randomTestUUID(), state, created, updated, startedValue, finishedValue, resultValue, errorValue)
	return err
}

func randomTestUUID() string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		panic("crypto/rand unavailable for PostgreSQL integration fixture")
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
}

func insertOutbox(t *testing.T, ctx context.Context, pool *pgxpool.Pool, eventID, jobID string) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO outbox (event_id, job_id, event_type, schema_version, attempt_no)
		VALUES ($1, $2, 'recognition.requested', 'v1', 1)`, eventID, jobID)
	if err != nil {
		t.Fatalf("insert valid outbox row: %v", err)
	}
}

func assertPostgresError(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("PostgreSQL error code = %v, want %s", err, code)
	}
}
