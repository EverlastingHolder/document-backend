//go:build integration

package postgres_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"passport-backend/internal/imagestore"
	"passport-backend/internal/jobintake"
	"passport-backend/internal/jobs"
	"passport-backend/internal/jobstatus"
	"passport-backend/internal/jobstore/postgres"
	"passport-backend/internal/technicalbackup"
)

const (
	technicalBackupSourceDSNEnv = "PASSPORT_TECHNICAL_BACKUP_SOURCE_DSN_FILE"
	technicalBackupTargetDSNEnv = "PASSPORT_TECHNICAL_BACKUP_TARGET_DSN_FILE"
	backupPIISentinel           = "synthetic-person-private@example.invalid"
	backupPathSentinel          = "/private/source-volume/passport-image-secret-path"
)

func TestPostgresTechnicalBackupRestoreTombstones(t *testing.T) {
	if strings.TrimSpace(os.Getenv(technicalBackupSourceDSNEnv)) == "" && strings.TrimSpace(os.Getenv(technicalBackupTargetDSNEnv)) == "" {
		t.Skip("dedicated technical-backup source and target DSNs are not configured")
	}
	sourceDSN := privateDatabaseDSN(t, technicalBackupSourceDSNEnv)
	targetDSN := privateDatabaseDSN(t, technicalBackupTargetDSNEnv)
	sourceConfig, sourceConfigErr := pgxpool.ParseConfig(sourceDSN)
	targetConfig, targetConfigErr := pgxpool.ParseConfig(targetDSN)
	if sourceConfigErr != nil || targetConfigErr != nil || sourceConfig.ConnConfig.Database == targetConfig.ConnConfig.Database {
		t.Fatal("technical-backup rehearsal requires two distinct isolated databases")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	source, err := postgres.Open(ctx, postgres.Config{DSN: sourceDSN, ConnectTimeout: 10 * time.Second, MigrationLockTimeout: 20 * time.Second})
	if err != nil {
		t.Fatal("open isolated technical backup source")
	}
	defer source.Close()
	if err := source.Migrate(ctx); err != nil {
		t.Fatal("migrate isolated technical backup source")
	}
	if err := source.CheckSchema(ctx); err != nil {
		t.Fatal("check isolated technical backup source schema")
	}
	sourcePool := openTestPool(t, ctx, sourceDSN)
	target, err := postgres.Open(ctx, postgres.Config{DSN: targetDSN, ConnectTimeout: 10 * time.Second, MigrationLockTimeout: 20 * time.Second})
	if err != nil {
		t.Fatal("open isolated technical backup restore target")
	}
	defer target.Close()
	if err := target.Migrate(ctx); err != nil {
		t.Fatal("migrate fresh technical backup restore target")
	}
	targetPool := openTestPool(t, ctx, targetDSN)
	emptySnapshot, err := target.ReadMaintenanceSnapshot(ctx)
	if err != nil {
		t.Fatal("read empty target maintenance snapshot")
	}
	assertEmptyMaintenanceSnapshot(t, emptySnapshot)

	activeID, revokedID := randomTestUUID(), randomTestUUID()
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		for _, pool := range []*pgxpool.Pool{sourcePool, targetPool} {
			if activeID != "" {
				_, _ = pool.Exec(cleanupCtx, `DELETE FROM jobs WHERE owner_id=$1`, activeID)
			}
			if activeID != "" {
				_, _ = pool.Exec(cleanupCtx, `DELETE FROM client_installations WHERE client_id=$1::uuid`, activeID)
			}
			if revokedID != "" {
				_, _ = pool.Exec(cleanupCtx, `DELETE FROM client_installations WHERE client_id=$1::uuid`, revokedID)
			}
		}
	})
	activeTokenHash := sha256.Sum256([]byte("synthetic-active-client-token"))
	revokedTokenHash := sha256.Sum256([]byte("synthetic-revoked-client-token"))
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := sourcePool.Exec(ctx, `INSERT INTO client_installations (client_id, token_sha256, created_at) VALUES ($1::uuid, $2, $3)`, activeID, activeTokenHash[:], now); err != nil {
		t.Fatal("create active synthetic installation")
	}
	if _, err := sourcePool.Exec(ctx, `INSERT INTO client_installations (client_id, token_sha256, created_at, revoked_at) VALUES ($1::uuid, $2, $3, $4)`, revokedID, revokedTokenHash[:], now, now.Add(time.Second)); err != nil {
		t.Fatal("create revoked synthetic installation")
	}

	imageRoot := filepath.Join(t.TempDir(), "source-images")
	images, err := imagestore.New(imagestore.Config{Root: imageRoot, MaxBytes: 1 << 20, MaxTotalBytes: 1 << 20})
	if err != nil {
		t.Fatal("create source synthetic image store")
	}
	defer images.Close()
	intake, err := jobintake.New(source, images)
	if err != nil {
		t.Fatal("create source intake service")
	}
	queuedBytes := []byte("synthetic image bytes for preserved idempotency")
	queuedKey := "restore-replay-" + randomTestUUID()
	queued, err := intake.Accept(ctx, activeID, queuedKey, queuedBytes, "image/jpeg")
	if err != nil {
		t.Fatal("create queued source fixture")
	}
	if _, err := sourcePool.Exec(ctx, `UPDATE jobs SET created_at=clock_timestamp()-interval '5 seconds', available_at=clock_timestamp()-interval '1 second', updated_at=clock_timestamp() WHERE job_id=$1::uuid`, queued.JobID); err != nil {
		t.Fatal("make synthetic queued fixture due for aggregate snapshot")
	}

	completedID, eventID, attemptID, imageKey := randomTestUUID(), randomTestUUID(), randomTestUUID(), randomTestUUID()
	completedCreated := now.Add(-time.Minute)
	completedStarted := completedCreated.Add(time.Second)
	completedFinished := completedStarted.Add(time.Second)
	result := map[string]any{"synthetic_person": backupPIISentinel, "synthetic_image_path": backupPathSentinel, "given_name": "Test"}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		t.Fatal("marshal synthetic source result")
	}
	if _, err := sourcePool.Exec(ctx, `
		INSERT INTO jobs (job_id, owner_id, idempotency_key, request_digest, image_key, image_media_type, image_size_bytes,
		                  state, attempt_count, available_at, created_at, updated_at, started_at, finished_at, result, pipeline_version)
		VALUES ($1::uuid, $2, $3, $4, $5::uuid, 'image/webp', 123, 'completed', 1, $6, $6, $7, $8, $9, $10::jsonb, 'synthetic-pipeline')`,
		completedID, activeID, "completed-"+randomTestUUID(), bytes32(), imageKey, completedCreated, completedFinished, completedStarted, completedFinished, resultJSON); err != nil {
		t.Fatal("create completed source fixture with synthetic private payload")
	}
	if _, err := sourcePool.Exec(ctx, `
		INSERT INTO job_attempts (attempt_id, job_id, attempt_no, attempt_token, state, started_at, lease_expires_at, finished_at)
		VALUES ($1::uuid, $2::uuid, 1, $3::uuid, 'completed', $4, $5, $5)`, attemptID, completedID, randomTestUUID(), completedStarted, completedFinished); err != nil {
		t.Fatal("create source attempt fixture")
	}
	if _, err := sourcePool.Exec(ctx, `INSERT INTO outbox (event_id, job_id, event_type, schema_version, attempt_no, published_at) VALUES ($1::uuid, $2::uuid, 'recognition.requested', 'v1', 1, clock_timestamp())`, eventID, completedID); err != nil {
		t.Fatal("create source outbox fixture")
	}
	sourceSnapshot, err := source.ReadMaintenanceSnapshot(ctx)
	if err != nil {
		t.Fatal("read source aggregate snapshot with queued and terminal fixtures")
	}
	if sourceSnapshot.QueuedJobs != 1 || sourceSnapshot.OldestQueuedAge <= 0 || sourceSnapshot.ExpiredLeases != 0 ||
		sourceSnapshot.TerminalImagesPending != 1 || sourceSnapshot.TerminalResultsPending != 0 {
		t.Fatal("source aggregate snapshot did not reflect the known queued/due and terminal fixture rows")
	}

	snapshot, err := source.ExportTechnicalBackup(ctx)
	if err != nil {
		t.Fatal("export technical-only snapshot")
	}
	plain, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal("marshal exported technical snapshot")
	}
	for _, forbidden := range []string{backupPIISentinel, backupPathSentinel, "result", "image_key", "image_media_type", "image_size_bytes", "outbox", "attempt_token"} {
		if strings.Contains(string(plain), forbidden) {
			t.Fatalf("technical snapshot includes forbidden field/data %q", forbidden)
		}
	}
	backupDir := filepath.Join(t.TempDir(), "private-backups")
	if err := os.Mkdir(backupDir, 0700); err != nil {
		t.Fatal("create private backup fixture directory")
	}
	key, err := technicalbackup.GenerateKey()
	if err != nil {
		t.Fatal("generate synthetic archive key")
	}
	defer clear(key)
	archive, err := technicalbackup.Encrypt(snapshot, key)
	if err != nil {
		t.Fatal("encrypt technical snapshot")
	}
	archivePath := filepath.Join(backupDir, "fixture.pstb")
	if err := technicalbackup.WritePrivateNew(archivePath, archive); err != nil {
		t.Fatal("write private synthetic archive")
	}
	archiveRead, err := technicalbackup.ReadPrivate(archivePath, technicalbackup.MaxArchiveSize)
	if err != nil {
		t.Fatal("read private synthetic archive")
	}
	decrypted, err := technicalbackup.Decrypt(archiveRead, key)
	if err != nil {
		t.Fatal("decrypt synthetic archive")
	}
	decryptedJSON, err := json.Marshal(decrypted)
	if err != nil {
		t.Fatal("marshal decrypted technical snapshot")
	}
	if strings.Contains(string(decryptedJSON), backupPIISentinel) || strings.Contains(string(decryptedJSON), backupPathSentinel) {
		t.Fatal("decrypted technical backup contains synthetic OCR PII or image path")
	}

	if err := target.RestoreTechnicalBackup(ctx, decrypted); err != nil {
		t.Fatal("restore technical tombstones into empty target")
	}
	if err := target.RestoreTechnicalBackup(ctx, decrypted); !errors.Is(err, postgres.ErrRestoreTargetNotEmpty) {
		t.Fatalf("second restore into populated target = %v, want ErrRestoreTargetNotEmpty", err)
	}
	var clientCount, jobCount, attemptCount, outboxCount int
	if err := targetPool.QueryRow(ctx, `SELECT (SELECT count(*) FROM client_installations), (SELECT count(*) FROM jobs), (SELECT count(*) FROM job_attempts), (SELECT count(*) FROM outbox)`).Scan(&clientCount, &jobCount, &attemptCount, &outboxCount); err != nil {
		t.Fatal("read restored technical row counts")
	}
	if clientCount != 2 || jobCount != 2 || attemptCount != 0 || outboxCount != 0 {
		t.Fatalf("restored rows = clients:%d jobs:%d attempts:%d outbox:%d", clientCount, jobCount, attemptCount, outboxCount)
	}
	var invalidPayloadRows int
	if err := targetPool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE state <> 'failed' OR image_key IS NOT NULL OR image_media_type IS NOT NULL OR image_size_bytes IS NOT NULL OR image_sha256 IS NOT NULL OR image_purged_at IS NULL OR result IS NOT NULL OR result_purged_at IS NULL OR error_code <> 'processing_failed'`).Scan(&invalidPayloadRows); err != nil || invalidPayloadRows != 0 {
		t.Fatalf("restored tombstone payload rows = %d, err=%v", invalidPayloadRows, err)
	}

	var restoredActive, restoredRevoked [32]byte
	copy(restoredActive[:], activeTokenHash[:])
	copy(restoredRevoked[:], revokedTokenHash[:])
	if owner, err := target.LookupClientOwner(ctx, restoredActive); err != nil || owner != activeID {
		t.Fatal("active client hash did not survive restore")
	}
	if _, err := target.LookupClientOwner(ctx, restoredRevoked); err == nil {
		t.Fatal("revoked client hash authenticated after restore")
	}
	if _, found, revoked, err := target.LookupClientCredentialState(ctx, restoredRevoked); err != nil || !found || !revoked {
		t.Fatal("revoked credential state did not survive restore")
	}

	reader := jobstatus.New(target)
	for _, id := range []string{queued.JobID, completedID} {
		if _, err := reader.ReadOwned(ctx, activeID, id); !errors.Is(err, jobs.ErrResultGone) {
			t.Fatalf("restored job status error = %v, want ErrResultGone", err)
		}
	}
	replayRoot := filepath.Join(t.TempDir(), "replay-images")
	replayImages, err := imagestore.New(imagestore.Config{Root: replayRoot, MaxBytes: 1 << 20, MaxTotalBytes: 1 << 20})
	if err != nil {
		t.Fatal("create target replay image store")
	}
	defer replayImages.Close()
	replayIntake, err := jobintake.New(target, replayImages)
	if err != nil {
		t.Fatal("create target replay intake")
	}
	replay, err := replayIntake.Accept(ctx, activeID, queuedKey, queuedBytes, "image/jpeg")
	if err != nil || replay.JobID != queued.JobID || replay.Status != jobs.StatusFailed {
		t.Fatalf("restored idempotency replay = (%+v,%v), want original failed tombstone", replay, err)
	}
	usage, err := replayImages.Usage(ctx)
	if err != nil || usage.UsedBytes != 0 {
		t.Fatalf("replay created a processable image: usage=%+v err=%v", usage, err)
	}
	postRestoreSnapshot, err := target.ReadMaintenanceSnapshot(ctx)
	if err != nil {
		t.Fatal("read post-restore purged maintenance snapshot")
	}
	assertEmptyMaintenanceSnapshot(t, postRestoreSnapshot)
	if _, err := replayIntake.Accept(ctx, activeID, queuedKey, []byte("changed synthetic image"), "image/jpeg"); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("changed request after restore = %v, want conflict", err)
	}
}

func assertEmptyMaintenanceSnapshot(t *testing.T, snapshot jobs.MaintenanceSnapshot) {
	t.Helper()
	if snapshot.QueuedJobs != 0 || snapshot.OldestQueuedAge != 0 || snapshot.ExpiredLeases != 0 ||
		snapshot.TerminalImagesPending != 0 || snapshot.TerminalResultsPending != 0 {
		t.Fatal("empty or fully purged database returned a nonzero maintenance snapshot")
	}
}

func privateDatabaseDSN(t *testing.T, envName string) string {
	t.Helper()
	path := strings.TrimSpace(os.Getenv(envName))
	if path == "" {
		t.Fatalf("integration test requires %s", envName)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read private test database DSN file %s", envName)
	}
	dsn := strings.TrimSpace(string(data))
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil || config.ConnConfig.Host != "postgres" || !strings.HasPrefix(config.ConnConfig.Database, "passport_pg_it_") || config.ConnConfig.User != "passport_test" {
		t.Fatalf("%s must identify an isolated Compose PostgreSQL test database", envName)
	}
	return dsn
}
