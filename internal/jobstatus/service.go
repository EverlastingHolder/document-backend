// Package jobstatus turns the owner-scoped database projection into the public
// asynchronous job status response.
package jobstatus

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"passport-backend/internal/api"
	"passport-backend/internal/jobs"
)

type Reader struct {
	repository jobs.StatusRepository
}

// New creates a read-only status service. The repository must enforce owner
// filtering in its query.
func New(repository jobs.StatusRepository) *Reader {
	return &Reader{repository: repository}
}

type Snapshot struct {
	JobID      string          `json:"job_id"`
	Status     jobs.Status     `json:"status"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
	StartedAt  *time.Time      `json:"started_at"`
	FinishedAt *time.Time      `json:"finished_at"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      *PublicError    `json:"error,omitempty"`
}

type PublicError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

var publicFailureMessages = map[string]string{
	"image_unprocessable": "Изображение не удалось обработать. Попробуйте сделать новый снимок.",
	"processing_failed":   "Не удалось обработать документ. Попробуйте позже.",
	"ocr_unavailable":     "Сервис распознавания временно недоступен. Попробуйте позже.",
	"processing_timeout":  "Обработка заняла слишком много времени. Попробуйте отправить документ ещё раз.",
}

func (r *Reader) ReadOwned(ctx context.Context, ownerID, jobID string) (Snapshot, error) {
	if r == nil || r.repository == nil || ctx == nil || ownerID == "" {
		return Snapshot{}, jobs.ErrUnavailable
	}
	record, err := r.repository.GetOwnedStatus(ctx, ownerID, jobID)
	if err != nil {
		if errors.Is(err, jobs.ErrNotFound) {
			return Snapshot{}, jobs.ErrNotFound
		}
		return Snapshot{}, jobs.ErrUnavailable
	}
	if !strings.EqualFold(record.ID, jobID) || !validStatusTimestamps(record) {
		return Snapshot{}, jobs.ErrUnavailable
	}
	if record.ResultPurged {
		if record.Status != jobs.StatusCompleted && record.Status != jobs.StatusFailed {
			return Snapshot{}, jobs.ErrUnavailable
		}
		if len(record.ResultJSON) != 0 {
			return Snapshot{}, jobs.ErrUnavailable
		}
		return Snapshot{}, jobs.ErrResultGone
	}

	snapshot := Snapshot{
		JobID:      record.ID,
		Status:     record.Status,
		CreatedAt:  record.CreatedAt,
		UpdatedAt:  record.UpdatedAt,
		StartedAt:  copyTime(record.StartedAt),
		FinishedAt: copyTime(record.FinishedAt),
	}
	switch record.Status {
	case jobs.StatusQueued:
		if record.FinishedAt != nil || len(record.ResultJSON) != 0 || record.ErrorCode != nil {
			return Snapshot{}, jobs.ErrUnavailable
		}
	case jobs.StatusProcessing:
		if record.StartedAt == nil || record.FinishedAt != nil || len(record.ResultJSON) != 0 || record.ErrorCode != nil || record.PipelineVersion != nil {
			return Snapshot{}, jobs.ErrUnavailable
		}
	case jobs.StatusCompleted:
		if record.StartedAt == nil || record.FinishedAt == nil || record.ErrorCode != nil || record.PipelineVersion == nil || *record.PipelineVersion == "" {
			return Snapshot{}, jobs.ErrUnavailable
		}
		result, err := api.DecodeStoredRecognitionResponse(record.ResultJSON, record.ID, *record.PipelineVersion)
		if err != nil {
			return Snapshot{}, jobs.ErrUnavailable
		}
		snapshot.Result = result
	case jobs.StatusFailed:
		if record.FinishedAt == nil || len(record.ResultJSON) != 0 || record.ErrorCode == nil {
			return Snapshot{}, jobs.ErrUnavailable
		}
		message, ok := publicFailureMessages[*record.ErrorCode]
		if !ok {
			return Snapshot{}, jobs.ErrUnavailable
		}
		snapshot.Error = &PublicError{Code: *record.ErrorCode, Message: message}
	default:
		return Snapshot{}, jobs.ErrUnavailable
	}
	return snapshot, nil
}

func validStatusTimestamps(record jobs.StatusRecord) bool {
	if record.ID == "" || record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() || record.UpdatedAt.Before(record.CreatedAt) {
		return false
	}
	if record.StartedAt != nil && (record.StartedAt.IsZero() || record.StartedAt.Before(record.CreatedAt) || record.UpdatedAt.Before(*record.StartedAt)) {
		return false
	}
	if record.FinishedAt != nil {
		floor := record.CreatedAt
		if record.StartedAt != nil {
			floor = *record.StartedAt
		}
		if record.FinishedAt.IsZero() || record.FinishedAt.Before(floor) || record.UpdatedAt.Before(*record.FinishedAt) {
			return false
		}
	}
	return true
}

func copyTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
