// Package jobintake coordinates durable image storage and PostgreSQL job
// acceptance. It deliberately does not publish to RabbitMQ or run OCR.
package jobintake

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"mime"
	"strings"

	"passport-backend/internal/imagestore"
	"passport-backend/internal/jobs"
)

var (
	ErrInvalidInput = errors.New("invalid job intake input")
	ErrConflict     = jobs.ErrConflict
)

type ImageStore interface {
	Put(context.Context, []byte, string) (imagestore.Metadata, error)
	DeleteUploaded(context.Context, imagestore.Metadata) error
}

// Service is the application boundary for an authenticated, already bounded
// multipart upload. ownerID must come from verified authentication context.
type Service struct {
	repository jobs.Repository
	images     ImageStore
	newID      func() (string, error)
}

func New(repository jobs.Repository, images ImageStore) (*Service, error) {
	if repository == nil || images == nil {
		return nil, ErrInvalidInput
	}
	return &Service{repository: repository, images: images, newID: randomUUIDv4}, nil
}

// Accept stores the image before asking PostgreSQL to atomically create the
// queued job and initial outbox event. A confirmed commit is the only path to
// a new acceptance response. On repository errors the image is intentionally
// retained because the commit outcome may be unknown.
func (s *Service) Accept(ctx context.Context, ownerID, idempotencyKey string, data []byte, mediaType string) (jobs.Accepted, error) {
	if s == nil || ctx == nil || strings.TrimSpace(ownerID) == "" || !validIdempotencyKey(idempotencyKey) ||
		len(data) == 0 || !isNormalizedMediaType(mediaType) {
		return jobs.Accepted{}, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return jobs.Accepted{}, err
	}
	digest := requestDigest(data, mediaType)

	existing, err := s.repository.Lookup(ctx, ownerID, idempotencyKey)
	if err == nil {
		return acceptedReplay(existing, digest)
	}
	if !errors.Is(err, jobs.ErrNotFound) {
		if ctx.Err() != nil {
			return jobs.Accepted{}, ctx.Err()
		}
		return jobs.Accepted{}, jobs.ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return jobs.Accepted{}, err
	}

	image, err := s.images.Put(ctx, data, mediaType)
	if err != nil {
		if errors.Is(err, imagestore.ErrCapacityExceeded) {
			// A concurrent request may have committed the same owner/key after the
			// initial lookup. Resolve that replay even when the volume is full.
			if accepted, lookupErr := s.lookupRaceWinner(ctx, ownerID, idempotencyKey, digest); lookupErr == nil {
				return accepted, nil
			} else if errors.Is(lookupErr, jobs.ErrConflict) {
				return jobs.Accepted{}, jobs.ErrConflict
			}
		}
		if ctx.Err() != nil {
			return jobs.Accepted{}, ctx.Err()
		}
		return jobs.Accepted{}, err
	}
	if err := ctx.Err(); err != nil {
		// Put has already made a durable private object. Do not remove it: no
		// database outcome has been established and a future flow may reference it.
		return jobs.Accepted{}, err
	}

	jobID, err := s.newID()
	if err != nil {
		return jobs.Accepted{}, jobs.ErrUnavailable
	}
	eventID, err := s.newID()
	if err != nil {
		return jobs.Accepted{}, jobs.ErrUnavailable
	}
	submission := jobs.Submission{
		JobID: jobID, EventID: eventID, OwnerID: ownerID, IdempotencyKey: idempotencyKey,
		RequestDigest: digest, ImageKey: image.Key, MediaType: image.MediaType, Size: image.Size,
		ImageSHA256: image.SHA256,
	}
	job, outcome, err := s.repository.CreateQueued(ctx, submission)
	if err != nil {
		if ctx.Err() != nil {
			return jobs.Accepted{}, ctx.Err()
		}
		// CreateQueued may have committed before a connection/commit error. Keep
		// the uploaded object so a committed row can never point at a deleted file.
		return jobs.Accepted{}, jobs.ErrUnavailable
	}
	switch outcome {
	case jobs.OutcomeCreated:
		return acceptedFromJob(job)
	case jobs.OutcomeReplay:
		if job.ImageKey != image.Key {
			_ = s.images.DeleteUploaded(ctx, image) // confirmed loser; cleanup is best effort
		}
		return acceptedReplay(job, digest)
	case jobs.OutcomeConflict:
		if job.ImageKey != image.Key {
			_ = s.images.DeleteUploaded(ctx, image) // confirmed loser; cleanup is best effort
		}
		return jobs.Accepted{}, jobs.ErrConflict
	default:
		// An unknown outcome is not evidence that this image is unused.
		return jobs.Accepted{}, jobs.ErrUnavailable
	}
}

func (s *Service) lookupRaceWinner(ctx context.Context, ownerID, key string, digest [sha256.Size]byte) (jobs.Accepted, error) {
	job, err := s.repository.Lookup(ctx, ownerID, key)
	if err != nil {
		return jobs.Accepted{}, err
	}
	return acceptedReplay(job, digest)
}

func acceptedReplay(job jobs.Job, digest [sha256.Size]byte) (jobs.Accepted, error) {
	if job.RequestDigest != digest {
		return jobs.Accepted{}, jobs.ErrConflict
	}
	return acceptedFromJob(job)
}

func acceptedFromJob(job jobs.Job) (jobs.Accepted, error) {
	if strings.TrimSpace(job.ID) == "" || job.Status == "" || job.CreatedAt.IsZero() {
		return jobs.Accepted{}, jobs.ErrUnavailable
	}
	return jobs.Accepted{JobID: job.ID, Status: job.Status, CreatedAt: job.CreatedAt}, nil
}

func requestDigest(data []byte, normalizedMediaType string) [sha256.Size]byte {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte("passport-job-request-v1\x00"))
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(normalizedMediaType)))
	_, _ = hasher.Write(length[:])
	_, _ = hasher.Write([]byte(normalizedMediaType))
	binary.BigEndian.PutUint64(length[:], uint64(len(data)))
	_, _ = hasher.Write(length[:])
	_, _ = hasher.Write(data)
	var digest [sha256.Size]byte
	copy(digest[:], hasher.Sum(nil))
	return digest
}

func validIdempotencyKey(key string) bool {
	if len(key) < 1 || len(key) > 128 {
		return false
	}
	for index := 0; index < len(key); index++ {
		if key[index] < 0x21 || key[index] > 0x7e {
			return false
		}
	}
	return true
}

func isNormalizedMediaType(value string) bool {
	mediaType, params, err := mime.ParseMediaType(value)
	if err != nil || len(params) != 0 || mediaType != value {
		return false
	}
	switch value {
	case "image/jpeg", "image/png", "image/webp":
		return true
	default:
		return false
	}
}

func randomUUIDv4() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}
