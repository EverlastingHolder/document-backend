// Package retention coordinates deletion of terminal image objects and
// canonical results. It never selects queued, processing, or retryable jobs.
package retention

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"passport-backend/internal/imagestore"
	"passport-backend/internal/jobs"
)

const ResultRetention = 15 * time.Minute

var ErrInvalidConfig = errors.New("invalid retention configuration")

type ImageCandidate struct {
	JobID      string
	Key        string
	Size       int64
	SHA256     string
	ClaimToken string
}

type Repository interface {
	ClaimTerminalImage(ctx context.Context, jobID string, lease time.Duration) (ImageCandidate, bool, error)
	ClaimTerminalImages(ctx context.Context, limit int, lease time.Duration) ([]ImageCandidate, error)
	MarkImagePurged(ctx context.Context, candidate ImageCandidate) error
	ReleaseImagePurge(ctx context.Context, candidate ImageCandidate) error
	PurgeExpiredResults(ctx context.Context, limit int) (int, error)
	ReadMaintenanceSnapshot(ctx context.Context) (jobs.MaintenanceSnapshot, error)
}

type ImageDeleter interface {
	DeleteObject(ctx context.Context, key string, expectedSize int64, expectedSHA256 string) error
}

type OrphanImageStore interface {
	ListFiles(ctx context.Context) ([]imagestore.File, error)
	DeleteOrphan(ctx context.Context, file imagestore.File) error
}

type OrphanReferenceRepository interface {
	IsImageReferenced(ctx context.Context, key string) (bool, error)
}

type JobImagePurger interface {
	PurgeJobImage(ctx context.Context, jobID string) error
	PurgeTerminalImages(ctx context.Context, limit int) (int, error)
}

type Config struct {
	ClaimLease       time.Duration
	Interval         time.Duration
	OperationTimeout time.Duration
	BatchSize        int
	Logger           *slog.Logger
}

type Service struct {
	repository Repository
	images     ImageDeleter
	config     Config
	logger     *slog.Logger
}

func New(repository Repository, images ImageDeleter, config Config) (*Service, error) {
	if repository == nil || images == nil || config.ClaimLease <= 0 || config.Interval <= 0 ||
		config.OperationTimeout <= 0 || config.ClaimLease <= config.OperationTimeout || config.BatchSize <= 0 {
		return nil, ErrInvalidConfig
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{repository: repository, images: images, config: config, logger: logger}, nil
}

func (s *Service) PurgeJobImage(ctx context.Context, jobID string) error {
	if s == nil || s.repository == nil || s.images == nil || ctx == nil || jobID == "" {
		return ErrInvalidConfig
	}
	operationCtx, cancel := context.WithTimeout(ctx, s.config.OperationTimeout)
	defer cancel()
	candidate, found, err := s.repository.ClaimTerminalImage(operationCtx, jobID, s.config.ClaimLease)
	if err != nil || !found {
		return err
	}
	return s.purgeClaimed(operationCtx, candidate)
}

func (s *Service) PurgeTerminalImages(ctx context.Context, limit int) (int, error) {
	if s == nil || s.repository == nil || s.images == nil || ctx == nil || limit <= 0 {
		return 0, ErrInvalidConfig
	}
	operationCtx, cancel := context.WithTimeout(ctx, s.config.OperationTimeout)
	defer cancel()
	candidates, err := s.repository.ClaimTerminalImages(operationCtx, limit, s.config.ClaimLease)
	if err != nil {
		return 0, err
	}
	var purged int
	for _, candidate := range candidates {
		if err := s.purgeClaimed(operationCtx, candidate); err != nil {
			return purged, err
		}
		purged++
	}
	return purged, nil
}

func (s *Service) purgeClaimed(ctx context.Context, candidate ImageCandidate) error {
	if err := s.images.DeleteObject(ctx, candidate.Key, candidate.Size, candidate.SHA256); err != nil {
		releaseCtx, cancel := context.WithTimeout(context.Background(), s.config.OperationTimeout)
		_ = s.repository.ReleaseImagePurge(releaseCtx, candidate)
		cancel()
		return jobs.ErrUnavailable
	}
	if err := s.repository.MarkImagePurged(ctx, candidate); err != nil {
		return jobs.ErrUnavailable
	}
	return nil
}

func (s *Service) PurgeExpiredResults(ctx context.Context, limit int) (int, error) {
	if s == nil || s.repository == nil || ctx == nil || limit <= 0 {
		return 0, ErrInvalidConfig
	}
	operationCtx, cancel := context.WithTimeout(ctx, s.config.OperationTimeout)
	defer cancel()
	return s.repository.PurgeExpiredResults(operationCtx, limit)
}

func (s *Service) ReadSnapshot(ctx context.Context) (jobs.MaintenanceSnapshot, error) {
	if s == nil || s.repository == nil || ctx == nil {
		return jobs.MaintenanceSnapshot{}, ErrInvalidConfig
	}
	return s.repository.ReadMaintenanceSnapshot(ctx)
}

// PurgeOrphansOffline deletes only store-owned names with no DB reference.
// The caller must stop every API/intake process before invoking it; online
// intake may have promoted a file but not yet committed its job row.
func (s *Service) PurgeOrphansOffline(ctx context.Context) (int, error) {
	if s == nil || ctx == nil {
		return 0, ErrInvalidConfig
	}
	files, ok := s.images.(OrphanImageStore)
	if !ok {
		return 0, ErrInvalidConfig
	}
	references, ok := s.repository.(OrphanReferenceRepository)
	if !ok {
		return 0, ErrInvalidConfig
	}
	objects, err := files.ListFiles(ctx)
	if err != nil {
		return 0, jobs.ErrUnavailable
	}
	var deleted int
	for _, object := range objects {
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
		if !object.Temporary {
			referenced, err := references.IsImageReferenced(ctx, object.Key)
			if err != nil {
				return deleted, jobs.ErrUnavailable
			}
			if referenced {
				continue
			}
		}
		if err := files.DeleteOrphan(ctx, object); err != nil {
			return deleted, jobs.ErrUnavailable
		}
		deleted++
	}
	return deleted, nil
}

func (s *Service) Sweep(ctx context.Context) error {
	for {
		count, err := s.PurgeTerminalImages(ctx, s.config.BatchSize)
		if err != nil {
			return err
		}
		if count < s.config.BatchSize {
			break
		}
	}
	for {
		count, err := s.PurgeExpiredResults(ctx, s.config.BatchSize)
		if err != nil {
			return err
		}
		if count < s.config.BatchSize {
			return nil
		}
	}
}

func (s *Service) Run(ctx context.Context) error {
	if s == nil || ctx == nil {
		return ErrInvalidConfig
	}
	wasUnavailable := false
	if err := s.runSweep(ctx); err != nil && ctx.Err() == nil {
		s.logger.Warn("retention.sweep_failed", "event", "retention.sweep_failed", "error_code", "retention_unavailable")
		wasUnavailable = true
	}
	ticker := time.NewTicker(s.config.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			err := s.runSweep(ctx)
			if err != nil && ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil && !wasUnavailable {
				s.logger.Warn("retention.sweep_failed", "event", "retention.sweep_failed", "error_code", "retention_unavailable")
			} else if err == nil && wasUnavailable {
				s.logger.Info("retention.sweep_recovered", "event", "retention.sweep_recovered", "error_code", "ok")
			}
			wasUnavailable = err != nil
		}
	}
}

func (s *Service) runSweep(ctx context.Context) error {
	operationCtx, cancel := context.WithTimeout(ctx, s.config.OperationTimeout)
	defer cancel()
	return s.Sweep(operationCtx)
}

var _ JobImagePurger = (*Service)(nil)
var _ ImageDeleter = (*imagestore.Store)(nil)
var _ OrphanImageStore = (*imagestore.Store)(nil)
