package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"passport-backend/internal/jobs"
	"passport-backend/internal/retention"
)

func (s *Store) ClaimTerminalImage(ctx context.Context, jobID string, lease time.Duration) (retention.ImageCandidate, bool, error) {
	if s == nil || s.pool == nil || ctx == nil || lease <= 0 {
		return retention.ImageCandidate{}, false, jobs.ErrUnavailable
	}
	normalized, ok := normalizeJobUUID(jobID)
	if !ok {
		return retention.ImageCandidate{}, false, jobs.ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return retention.ImageCandidate{}, false, storageError(ctx)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	candidate, found, err := claimTerminalImage(ctx, tx, normalized, lease)
	if err != nil {
		return retention.ImageCandidate{}, false, storageError(ctx)
	}
	if !found {
		if err := tx.Commit(ctx); err != nil {
			return retention.ImageCandidate{}, false, storageError(ctx)
		}
		return retention.ImageCandidate{}, false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return retention.ImageCandidate{}, false, storageError(ctx)
	}
	return candidate, true, nil
}

func (s *Store) ClaimTerminalImages(ctx context.Context, limit int, lease time.Duration) ([]retention.ImageCandidate, error) {
	if s == nil || s.pool == nil || ctx == nil || limit <= 0 || lease <= 0 {
		return nil, jobs.ErrUnavailable
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, storageError(ctx)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	rows, err := tx.Query(ctx, `
		SELECT job_id::text
		FROM jobs
		WHERE state IN ('completed','failed') AND image_key IS NOT NULL
		  AND (image_purge_until IS NULL OR image_purge_until <= clock_timestamp())
		ORDER BY finished_at, job_id
		LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, storageError(ctx)
	}
	var jobIDs []string
	for rows.Next() {
		var jobID string
		if err := rows.Scan(&jobID); err != nil {
			rows.Close()
			return nil, storageError(ctx)
		}
		jobIDs = append(jobIDs, jobID)
	}
	rowsErr := rows.Err()
	rows.Close()
	if rowsErr != nil {
		return nil, storageError(ctx)
	}
	candidates := make([]retention.ImageCandidate, 0, len(jobIDs))
	for _, jobID := range jobIDs {
		candidate, found, err := claimTerminalImage(ctx, tx, jobID, lease)
		if err != nil {
			return nil, storageError(ctx)
		}
		if found {
			candidates = append(candidates, candidate)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, storageError(ctx)
	}
	return candidates, nil
}

func claimTerminalImage(ctx context.Context, tx pgx.Tx, jobID string, lease time.Duration) (retention.ImageCandidate, bool, error) {
	var candidate retention.ImageCandidate
	err := tx.QueryRow(ctx, `
		SELECT job_id::text, image_key::text, image_size_bytes, COALESCE(encode(image_sha256,'hex'), '')
		FROM jobs
		WHERE job_id=$1::uuid AND state IN ('completed','failed') AND image_key IS NOT NULL
		  AND (image_purge_until IS NULL OR image_purge_until <= clock_timestamp())
		FOR UPDATE SKIP LOCKED`, jobID).Scan(
		&candidate.JobID, &candidate.Key, &candidate.Size, &candidate.SHA256,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return retention.ImageCandidate{}, false, nil
	}
	if err != nil {
		return retention.ImageCandidate{}, false, err
	}
	token, err := randomUUIDv4()
	if err != nil {
		return retention.ImageCandidate{}, false, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE jobs SET image_purge_token=$2::uuid,
		    image_purge_until=clock_timestamp()+make_interval(secs => $3::double precision)
		WHERE job_id=$1::uuid AND image_key=$4::uuid`, jobID, token, lease.Seconds(), candidate.Key); err != nil {
		return retention.ImageCandidate{}, false, err
	}
	candidate.ClaimToken = token
	return candidate, true, nil
}

func (s *Store) MarkImagePurged(ctx context.Context, candidate retention.ImageCandidate) error {
	if s == nil || s.pool == nil || ctx == nil || !canonicalUUIDv4.MatchString(candidate.JobID) ||
		!canonicalUUIDv4.MatchString(candidate.Key) || !canonicalUUIDv4.MatchString(candidate.ClaimToken) {
		return jobs.ErrUnavailable
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE jobs
		SET image_key=NULL, image_media_type=NULL, image_size_bytes=NULL, image_sha256=NULL,
		    image_purged_at=clock_timestamp(), image_purge_token=NULL, image_purge_until=NULL
		WHERE job_id=$1::uuid AND state IN ('completed','failed')
		  AND image_key=$2::uuid AND image_purge_token=$3::uuid`, candidate.JobID, candidate.Key, candidate.ClaimToken)
	if err != nil {
		return storageError(ctx)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	return jobs.ErrInvalidTransition
}

func (s *Store) ReleaseImagePurge(ctx context.Context, candidate retention.ImageCandidate) error {
	if s == nil || s.pool == nil || ctx == nil || !canonicalUUIDv4.MatchString(candidate.JobID) || !canonicalUUIDv4.MatchString(candidate.ClaimToken) {
		return jobs.ErrUnavailable
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE jobs SET image_purge_token=NULL, image_purge_until=NULL
		WHERE job_id=$1::uuid AND image_purge_token=$2::uuid`, candidate.JobID, candidate.ClaimToken)
	if err != nil {
		return storageError(ctx)
	}
	return nil
}

func (s *Store) PurgeExpiredResults(ctx context.Context, limit int) (int, error) {
	if s == nil || s.pool == nil || ctx == nil || limit <= 0 {
		return 0, jobs.ErrUnavailable
	}
	tag, err := s.pool.Exec(ctx, `
		WITH candidates AS MATERIALIZED (
			SELECT job_id
			FROM jobs
			WHERE state IN ('completed','failed') AND result_purged_at IS NULL
			  AND finished_at <= clock_timestamp() - make_interval(secs => $2::double precision)
			ORDER BY finished_at, job_id
			LIMIT $1 FOR UPDATE SKIP LOCKED
		)
		UPDATE jobs j SET result=NULL, result_purged_at=clock_timestamp()
		FROM candidates c
		WHERE j.job_id=c.job_id AND j.result_purged_at IS NULL`, limit, retention.ResultRetention.Seconds())
	if err != nil {
		return 0, storageError(ctx)
	}
	return int(tag.RowsAffected()), nil
}

func (s *Store) AcknowledgeOwnedResult(ctx context.Context, ownerID, jobID string) (jobs.ReceiptOutcome, error) {
	if s == nil || s.pool == nil || ctx == nil || ownerID == "" {
		return 0, jobs.ErrNotFound
	}
	normalized, ok := normalizeJobUUID(jobID)
	if !ok {
		return 0, jobs.ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, storageError(ctx)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var state string
	var purged, expired bool
	err = tx.QueryRow(ctx, `
		SELECT state, result_purged_at IS NOT NULL,
		  state IN ('completed','failed') AND finished_at <= clock_timestamp()-make_interval(secs => $3::double precision)
		FROM jobs WHERE job_id=$1::uuid AND owner_id=$2 FOR UPDATE`, normalized, ownerID, retention.ResultRetention.Seconds()).Scan(&state, &purged, &expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, jobs.ErrNotFound
	}
	if err != nil {
		return 0, storageError(ctx)
	}
	if state != string(jobs.StatusCompleted) && state != string(jobs.StatusFailed) {
		if err := tx.Commit(ctx); err != nil {
			return 0, storageError(ctx)
		}
		return jobs.ReceiptActive, nil
	}
	if purged {
		if err := tx.Commit(ctx); err != nil {
			return 0, storageError(ctx)
		}
		return jobs.ReceiptAlreadyPurged, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET result=NULL, result_purged_at=clock_timestamp() WHERE job_id=$1::uuid AND owner_id=$2`, normalized, ownerID); err != nil {
		return 0, storageError(ctx)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, storageError(ctx)
	}
	if expired {
		return jobs.ReceiptAlreadyPurged, nil
	}
	return jobs.ReceiptReceived, nil
}

func (s *Store) ReadMaintenanceSnapshot(ctx context.Context) (jobs.MaintenanceSnapshot, error) {
	if s == nil || s.pool == nil || ctx == nil {
		return jobs.MaintenanceSnapshot{}, jobs.ErrUnavailable
	}
	var snapshot jobs.MaintenanceSnapshot
	var oldestAgeSeconds float64
	err := s.pool.QueryRow(ctx, `
		WITH now_value AS (SELECT clock_timestamp() AS at)
		SELECT
		  (SELECT count(*) FROM jobs WHERE state='queued'),
		  COALESCE((SELECT extract(epoch FROM now_value.at - (SELECT min(j.available_at) FROM jobs j WHERE j.state='queued' AND j.available_at <= now_value.at))::double precision FROM now_value), 0::double precision),
		  (SELECT count(*) FROM job_attempts WHERE state='processing' AND lease_expires_at <= (SELECT at FROM now_value)),
		  (SELECT count(*) FROM jobs WHERE state IN ('completed','failed') AND image_key IS NOT NULL),
		  (SELECT count(*) FROM jobs WHERE state IN ('completed','failed') AND result_purged_at IS NULL AND finished_at <= (SELECT at FROM now_value)-make_interval(secs => $1::double precision))`, retention.ResultRetention.Seconds()).Scan(
		&snapshot.QueuedJobs, &oldestAgeSeconds, &snapshot.ExpiredLeases,
		&snapshot.TerminalImagesPending, &snapshot.TerminalResultsPending,
	)
	if err != nil {
		return jobs.MaintenanceSnapshot{}, storageError(ctx, err)
	}
	if oldestAgeSeconds > 0 {
		snapshot.OldestQueuedAge = time.Duration(oldestAgeSeconds * float64(time.Second))
	}
	return snapshot, nil
}

func (s *Store) IsImageReferenced(ctx context.Context, key string) (bool, error) {
	if s == nil || s.pool == nil || ctx == nil || !canonicalUUIDv4.MatchString(key) {
		return false, jobs.ErrInvalidSubmission
	}
	var referenced bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM jobs WHERE image_key=$1::uuid)`, key).Scan(&referenced); err != nil {
		return false, storageError(ctx)
	}
	return referenced, nil
}

var _ jobs.ReceiptRepository = (*Store)(nil)
var _ jobs.MaintenanceRepository = (*Store)(nil)
var _ retention.Repository = (*Store)(nil)
var _ retention.OrphanReferenceRepository = (*Store)(nil)
