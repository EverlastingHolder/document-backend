package postgres

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"passport-backend/internal/jobs"
	"passport-backend/internal/retention"
)

// GetOwnedStatus reads the public status projection for one job without
// locking or changing it. Owner filtering happens in the same query so an
// absent job and a job belonging to another owner are indistinguishable.
func (s *Store) GetOwnedStatus(ctx context.Context, ownerID, jobID string) (jobs.StatusRecord, error) {
	if s == nil || s.pool == nil || ctx == nil || ownerID == "" {
		return jobs.StatusRecord{}, jobs.ErrNotFound
	}
	normalizedJobID, ok := normalizeJobUUID(jobID)
	if !ok {
		return jobs.StatusRecord{}, jobs.ErrNotFound
	}

	var record jobs.StatusRecord
	var state string
	err := s.pool.QueryRow(ctx, `
		SELECT job_id::text, state, created_at, updated_at, started_at, finished_at,
		       CASE WHEN result_purged_at IS NOT NULL OR
		         (state IN ('completed','failed') AND finished_at <= clock_timestamp()-make_interval(secs => $3::double precision))
		         THEN NULL ELSE result::text END,
		       pipeline_version, error_code,
		       result_purged_at IS NOT NULL OR
		         (state IN ('completed','failed') AND finished_at <= clock_timestamp()-make_interval(secs => $3::double precision))
		FROM jobs
		WHERE job_id = $1::uuid AND owner_id = $2`, normalizedJobID, ownerID, retention.ResultRetention.Seconds()).Scan(
		&record.ID, &state, &record.CreatedAt, &record.UpdatedAt,
		&record.StartedAt, &record.FinishedAt, &record.ResultJSON,
		&record.PipelineVersion, &record.ErrorCode, &record.ResultPurged,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return jobs.StatusRecord{}, jobs.ErrNotFound
	}
	if err != nil {
		return jobs.StatusRecord{}, storageError(ctx, err)
	}
	record.Status = jobs.Status(state)
	return record, nil
}

func normalizeJobUUID(value string) (string, bool) {
	value = strings.ToLower(value)
	if !canonicalUUIDv4.MatchString(value) {
		return "", false
	}
	return value, true
}

var _ jobs.StatusRepository = (*Store)(nil)
