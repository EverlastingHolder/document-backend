package postgres

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"passport-backend/internal/jobs"
)

const (
	initialEventType    = "recognition.requested"
	initialEventVersion = "v1"
)

var (
	visibleASCIIKey = regexp.MustCompile(`^[!-~]{1,128}$`)
	canonicalUUIDv4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

// Lookup returns a job only within the supplied owner scope. It is used for
// owner-scoped idempotency and never performs a lookup by an untrusted job ID.
func (s *Store) Lookup(ctx context.Context, ownerID, idempotencyKey string) (jobs.Job, error) {
	if s == nil || s.pool == nil || ctx == nil || ownerID == "" || !visibleASCIIKey.MatchString(idempotencyKey) {
		return jobs.Job{}, jobs.ErrInvalidSubmission
	}
	job, err := scanJob(s.pool.QueryRow(ctx, `
		SELECT job_id::text, owner_id, idempotency_key, request_digest, COALESCE(image_key::text, ''),
		       COALESCE(image_media_type, ''), COALESCE(image_size_bytes, 0), state, created_at, updated_at
		FROM jobs
		WHERE owner_id = $1 AND idempotency_key = $2`, ownerID, idempotencyKey))
	if errors.Is(err, pgx.ErrNoRows) {
		return jobs.Job{}, jobs.ErrNotFound
	}
	if err != nil {
		return jobs.Job{}, storageError(ctx)
	}
	return job, nil
}

// CreateQueued commits a queued job and its first outbox event in one
// transaction. A duplicate owner/key is resolved against the winning row; a
// commit error is deliberately reported as unavailable because its outcome
// may be unknown to the caller.
func (s *Store) CreateQueued(ctx context.Context, submission jobs.Submission) (jobs.Job, jobs.Outcome, error) {
	if s == nil || s.pool == nil || ctx == nil || !validSubmission(submission) {
		return jobs.Job{}, 0, jobs.ErrInvalidSubmission
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return jobs.Job{}, 0, storageError(ctx)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	job, err := scanJob(tx.QueryRow(ctx, `
		INSERT INTO jobs (
			job_id, owner_id, idempotency_key, request_digest, image_key,
			image_media_type, image_size_bytes, image_sha256, state
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, decode($8, 'hex'), 'queued')
		ON CONFLICT (owner_id, idempotency_key) DO NOTHING
		RETURNING job_id::text, owner_id, idempotency_key, request_digest, COALESCE(image_key::text, ''),
		          COALESCE(image_media_type, ''), COALESCE(image_size_bytes, 0), state, created_at, updated_at`,
		submission.JobID, submission.OwnerID, submission.IdempotencyKey,
		submission.RequestDigest[:], submission.ImageKey, submission.MediaType, submission.Size, submission.ImageSHA256,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		winner, lookupErr := scanJob(tx.QueryRow(ctx, `
			SELECT job_id::text, owner_id, idempotency_key, request_digest, COALESCE(image_key::text, ''),
			       COALESCE(image_media_type, ''), COALESCE(image_size_bytes, 0), state, created_at, updated_at
			FROM jobs
			WHERE owner_id = $1 AND idempotency_key = $2`, submission.OwnerID, submission.IdempotencyKey))
		if lookupErr != nil {
			return jobs.Job{}, 0, storageError(ctx)
		}
		if err := tx.Commit(ctx); err != nil {
			return jobs.Job{}, 0, storageError(ctx)
		}
		if subtle.ConstantTimeCompare(winner.RequestDigest[:], submission.RequestDigest[:]) != 1 {
			return jobs.Job{}, jobs.OutcomeConflict, nil
		}
		return winner, jobs.OutcomeReplay, nil
	}
	if err != nil {
		return jobs.Job{}, 0, storageError(ctx)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO outbox (event_id, job_id, event_type, schema_version, attempt_no)
		VALUES ($1, $2, $3, $4, 1)`, submission.EventID, submission.JobID, initialEventType, initialEventVersion); err != nil {
		return jobs.Job{}, 0, storageError(ctx)
	}
	if err := tx.Commit(ctx); err != nil {
		return jobs.Job{}, 0, storageError(ctx)
	}
	return job, jobs.OutcomeCreated, nil
}

func validSubmission(submission jobs.Submission) bool {
	return canonicalUUIDv4.MatchString(submission.JobID) &&
		canonicalUUIDv4.MatchString(submission.EventID) &&
		canonicalUUIDv4.MatchString(submission.ImageKey) &&
		submission.OwnerID != "" &&
		visibleASCIIKey.MatchString(submission.IdempotencyKey) &&
		(submission.MediaType == "image/jpeg" || submission.MediaType == "image/png" || submission.MediaType == "image/webp") &&
		submission.Size > 0 && validSHA256Hex(submission.ImageSHA256)
}

func validSHA256Hex(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(row rowScanner) (jobs.Job, error) {
	var job jobs.Job
	var digest []byte
	var state string
	if err := row.Scan(
		&job.ID, &job.OwnerID, &job.IdempotencyKey, &digest, &job.ImageKey,
		&job.MediaType, &job.Size, &state, &job.CreatedAt, &job.UpdatedAt,
	); err != nil {
		return jobs.Job{}, err
	}
	if len(digest) != len(job.RequestDigest) {
		return jobs.Job{}, jobs.ErrUnavailable
	}
	copy(job.RequestDigest[:], digest)
	job.Status = jobs.Status(state)
	return job, nil
}

func storageError(ctx context.Context, causes ...error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(causes) > 0 && causes[0] != nil {
		return safeStorageFailure(ctx, "job_storage", causes[0])
	}
	return jobs.ErrUnavailable
}
