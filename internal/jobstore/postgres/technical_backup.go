package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

const TechnicalBackupSchemaVersion = 1
const technicalBackupCurrentMigration = 9
const technicalBackupMaxRecords = 100_000

var (
	ErrInvalidTechnicalBackup = errors.New("invalid technical backup")
	ErrRestoreTargetNotEmpty  = errors.New("technical backup restore target is not empty")
	technicalUUIDv4           = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	technicalIdempotencyKey   = regexp.MustCompile(`^[!-~]{1,128}$`)
)

// TechnicalBackup contains only the identity and idempotency metadata required
// to reject revoked credentials and prevent duplicate work after restoration.
// It intentionally has no result, status, image, attempt, outbox, or path fields.
type TechnicalBackup struct {
	SchemaVersion         int                     `json:"schema_version"`
	DatabaseSchemaVersion int                     `json:"database_schema_version"`
	Clients               []TechnicalBackupClient `json:"clients"`
	Jobs                  []TechnicalBackupJob    `json:"jobs"`
}

type TechnicalBackupClient struct {
	ClientID    string     `json:"client_id"`
	TokenSHA256 string     `json:"token_sha256"`
	CreatedAt   time.Time  `json:"created_at"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

type TechnicalBackupJob struct {
	JobID          string    `json:"job_id"`
	OwnerID        string    `json:"owner_id"`
	IdempotencyKey string    `json:"idempotency_key"`
	RequestSHA256  string    `json:"request_sha256"`
	CreatedAt      time.Time `json:"created_at"`
}

// ExportTechnicalBackup reads only explicitly allowlisted columns under one
// repeatable-read snapshot. In particular, result and image metadata are never
// selected into this export path.
func (s *Store) ExportTechnicalBackup(ctx context.Context) (TechnicalBackup, error) {
	if s == nil || s.pool == nil || ctx == nil {
		return TechnicalBackup{}, ErrInvalidTechnicalBackup
	}
	if err := s.CheckSchema(ctx); err != nil {
		return TechnicalBackup{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return TechnicalBackup{}, storageError(ctx, err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := checkTechnicalBackupSchema(ctx, tx); err != nil {
		return TechnicalBackup{}, err
	}
	snapshot := TechnicalBackup{SchemaVersion: TechnicalBackupSchemaVersion, DatabaseSchemaVersion: technicalBackupCurrentMigration}
	clientRows, err := tx.Query(ctx, `
		SELECT client_id::text, encode(token_sha256, 'hex'), created_at, revoked_at
		FROM client_installations ORDER BY client_id`)
	if err != nil {
		return TechnicalBackup{}, storageError(ctx, err)
	}
	for clientRows.Next() {
		if len(snapshot.Clients)+len(snapshot.Jobs) >= technicalBackupMaxRecords {
			clientRows.Close()
			return TechnicalBackup{}, ErrInvalidTechnicalBackup
		}
		var client TechnicalBackupClient
		if err := clientRows.Scan(&client.ClientID, &client.TokenSHA256, &client.CreatedAt, &client.RevokedAt); err != nil {
			clientRows.Close()
			return TechnicalBackup{}, storageError(ctx, err)
		}
		snapshot.Clients = append(snapshot.Clients, client)
	}
	if err := clientRows.Err(); err != nil {
		clientRows.Close()
		return TechnicalBackup{}, storageError(ctx, err)
	}
	clientRows.Close()

	jobRows, err := tx.Query(ctx, `
		SELECT job_id::text, owner_id, idempotency_key, encode(request_digest, 'hex'), created_at
		FROM jobs ORDER BY job_id`)
	if err != nil {
		return TechnicalBackup{}, storageError(ctx, err)
	}
	for jobRows.Next() {
		if len(snapshot.Clients)+len(snapshot.Jobs) >= technicalBackupMaxRecords {
			jobRows.Close()
			return TechnicalBackup{}, ErrInvalidTechnicalBackup
		}
		var job TechnicalBackupJob
		if err := jobRows.Scan(&job.JobID, &job.OwnerID, &job.IdempotencyKey, &job.RequestSHA256, &job.CreatedAt); err != nil {
			jobRows.Close()
			return TechnicalBackup{}, storageError(ctx, err)
		}
		snapshot.Jobs = append(snapshot.Jobs, job)
	}
	if err := jobRows.Err(); err != nil {
		jobRows.Close()
		return TechnicalBackup{}, storageError(ctx, err)
	}
	jobRows.Close()
	if err := ValidateTechnicalBackup(snapshot); err != nil {
		return TechnicalBackup{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return TechnicalBackup{}, storageError(ctx, err)
	}
	return snapshot, nil
}

// RestoreTechnicalBackup installs credential tombstones and failed job
// tombstones into an empty, already-migrated database. No attempts or outbox
// messages are restored, so a lost image can never be scheduled again.
func (s *Store) RestoreTechnicalBackup(ctx context.Context, snapshot TechnicalBackup) error {
	if s == nil || s.pool == nil || ctx == nil {
		return ErrInvalidTechnicalBackup
	}
	if err := ValidateTechnicalBackup(snapshot); err != nil {
		return err
	}
	if err := s.CheckSchema(ctx); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return storageError(ctx, err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := checkTechnicalBackupSchema(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(0x544543484241434B)); err != nil {
		return storageError(ctx, err)
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE client_installations, jobs, job_attempts, outbox IN ACCESS EXCLUSIVE MODE`); err != nil {
		return storageError(ctx, err)
	}
	var clients, jobsCount, attempts, outbox int64
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM client_installations), (SELECT count(*) FROM jobs), (SELECT count(*) FROM job_attempts), (SELECT count(*) FROM outbox)`).Scan(&clients, &jobsCount, &attempts, &outbox); err != nil {
		return storageError(ctx, err)
	}
	if clients != 0 || jobsCount != 0 || attempts != 0 || outbox != 0 {
		return ErrRestoreTargetNotEmpty
	}
	for _, client := range snapshot.Clients {
		hash, _ := hex.DecodeString(client.TokenSHA256)
		if _, err := tx.Exec(ctx, `
			INSERT INTO client_installations (client_id, token_sha256, created_at, revoked_at)
			VALUES ($1::uuid, $2, $3, $4)`, client.ClientID, hash, client.CreatedAt, client.RevokedAt); err != nil {
			return storageError(ctx, err)
		}
	}
	for _, job := range snapshot.Jobs {
		digest, _ := hex.DecodeString(job.RequestSHA256)
		if _, err := tx.Exec(ctx, `
			WITH tombstone_clock AS MATERIALIZED (
				SELECT GREATEST($5::timestamptz, clock_timestamp()) AS at
			)
			INSERT INTO jobs (
				job_id, owner_id, idempotency_key, request_digest, state, attempt_count,
				available_at, created_at, updated_at, finished_at, result, error_code,
				image_key, image_media_type, image_size_bytes, image_sha256,
				image_purged_at, result_purged_at
			) SELECT $1::uuid, $2, $3, $4, 'failed', 0,
				$5, $5, tombstone_clock.at, tombstone_clock.at, NULL, 'processing_failed',
				NULL, NULL, NULL, NULL, tombstone_clock.at, tombstone_clock.at
			FROM tombstone_clock`, job.JobID, job.OwnerID, job.IdempotencyKey, digest, job.CreatedAt); err != nil {
			return storageError(ctx, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return storageError(ctx, err)
	}
	return nil
}

type schemaQuery interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func checkTechnicalBackupSchema(ctx context.Context, query schemaQuery) error {
	var latest int64
	var count int64
	if err := query.QueryRow(ctx, `SELECT COALESCE(max(version), 0), count(*) FROM schema_migrations`).Scan(&latest, &count); err != nil {
		return ErrSchemaNotReady
	}
	if latest != technicalBackupCurrentMigration || count != technicalBackupCurrentMigration {
		return ErrSchemaNotReady
	}
	return nil
}

func ValidateTechnicalBackup(snapshot TechnicalBackup) error {
	if snapshot.SchemaVersion != TechnicalBackupSchemaVersion || snapshot.DatabaseSchemaVersion != technicalBackupCurrentMigration || len(snapshot.Clients)+len(snapshot.Jobs) > technicalBackupMaxRecords {
		return ErrInvalidTechnicalBackup
	}
	seenClients := make(map[string]struct{}, len(snapshot.Clients))
	seenHashes := make(map[string]struct{}, len(snapshot.Clients))
	for _, client := range snapshot.Clients {
		if !technicalUUIDv4.MatchString(client.ClientID) || !validTechnicalHash(client.TokenSHA256) || client.CreatedAt.IsZero() || (client.RevokedAt != nil && client.RevokedAt.Before(client.CreatedAt)) {
			return ErrInvalidTechnicalBackup
		}
		if _, exists := seenClients[client.ClientID]; exists {
			return ErrInvalidTechnicalBackup
		}
		if _, exists := seenHashes[client.TokenSHA256]; exists {
			return ErrInvalidTechnicalBackup
		}
		seenClients[client.ClientID] = struct{}{}
		seenHashes[client.TokenSHA256] = struct{}{}
	}
	seenJobs := make(map[string]struct{}, len(snapshot.Jobs))
	seenKeys := make(map[string]struct{}, len(snapshot.Jobs))
	for _, job := range snapshot.Jobs {
		pair := job.OwnerID + "\x00" + job.IdempotencyKey
		if !technicalUUIDv4.MatchString(job.JobID) || !technicalUUIDv4.MatchString(job.OwnerID) || !technicalIdempotencyKey.MatchString(job.IdempotencyKey) || !validTechnicalHash(job.RequestSHA256) || job.CreatedAt.IsZero() {
			return ErrInvalidTechnicalBackup
		}
		if _, exists := seenJobs[job.JobID]; exists {
			return ErrInvalidTechnicalBackup
		}
		if _, exists := seenKeys[pair]; exists {
			return ErrInvalidTechnicalBackup
		}
		seenJobs[job.JobID] = struct{}{}
		seenKeys[pair] = struct{}{}
	}
	return nil
}

func validTechnicalHash(value string) bool {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != value {
		return false
	}
	return true
}
