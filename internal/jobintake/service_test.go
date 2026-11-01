package jobintake

import (
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"passport-backend/internal/imagestore"
	"passport-backend/internal/jobs"
)

type fakeRepository struct {
	jobs       map[string]jobs.Job
	lookupErr  error
	createErr  error
	outcome    jobs.Outcome
	createJob  jobs.Job
	lookups    int
	creates    int
	lastSubmit jobs.Submission
	lookupHook func(int)
}

func (r *fakeRepository) Lookup(_ context.Context, ownerID, key string) (jobs.Job, error) {
	r.lookups++
	if r.lookupHook != nil {
		r.lookupHook(r.lookups)
	}
	if r.lookupErr != nil {
		return jobs.Job{}, r.lookupErr
	}
	job, ok := r.jobs[ownerID+"\x00"+key]
	if !ok {
		return jobs.Job{}, jobs.ErrNotFound
	}
	return job, nil
}

func (r *fakeRepository) CreateQueued(_ context.Context, submission jobs.Submission) (jobs.Job, jobs.Outcome, error) {
	r.creates++
	r.lastSubmit = submission
	if r.createErr != nil {
		return jobs.Job{}, 0, r.createErr
	}
	if r.createJob.ID != "" {
		return r.createJob, r.outcome, nil
	}
	job := jobs.Job{
		ID: submission.JobID, OwnerID: submission.OwnerID, IdempotencyKey: submission.IdempotencyKey,
		RequestDigest: submission.RequestDigest, ImageKey: submission.ImageKey, MediaType: submission.MediaType,
		Size: submission.Size, Status: jobs.StatusQueued, CreatedAt: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC),
	}
	if r.jobs == nil {
		r.jobs = make(map[string]jobs.Job)
	}
	r.jobs[job.OwnerID+"\x00"+job.IdempotencyKey] = job
	return job, jobs.OutcomeCreated, nil
}

type trackedImages struct {
	store        *imagestore.Store
	puts         int
	deletes      int
	putErr       error
	deleteErr    error
	afterPut     func()
	lastMetadata imagestore.Metadata
}

func (s *trackedImages) Put(ctx context.Context, data []byte, mediaType string) (imagestore.Metadata, error) {
	s.puts++
	if s.putErr != nil {
		return imagestore.Metadata{}, s.putErr
	}
	metadata, err := s.store.Put(ctx, data, mediaType)
	if err == nil {
		s.lastMetadata = metadata
		if s.afterPut != nil {
			s.afterPut()
		}
	}
	return metadata, err
}

func (s *trackedImages) DeleteUploaded(ctx context.Context, metadata imagestore.Metadata) error {
	s.deletes++
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return s.store.DeleteUploaded(ctx, metadata)
}

func TestAcceptCreatedPersistsImageAndSubmitsBoundedCanonicalInputs(t *testing.T) {
	service, repository, images := newTestService(t, 0)
	data := []byte("bounded image bytes")
	accepted, err := service.Accept(context.Background(), "owner-42", "upload-1", data, "image/png")
	if err != nil {
		t.Fatalf("Accept() error = %v", err)
	}
	if accepted.JobID == "" || accepted.Status != jobs.StatusQueued || accepted.CreatedAt.IsZero() {
		t.Fatalf("Accept() = %+v, want persisted queued job", accepted)
	}
	if repository.creates != 1 || repository.lookups != 1 || images.puts != 1 || images.deletes != 0 {
		t.Fatalf("calls: lookups=%d creates=%d puts=%d deletes=%d", repository.lookups, repository.creates, images.puts, images.deletes)
	}
	submission := repository.lastSubmit
	if submission.OwnerID != "owner-42" || submission.IdempotencyKey != "upload-1" ||
		submission.MediaType != "image/png" || submission.Size != int64(len(data)) ||
		submission.ImageKey != images.lastMetadata.Key || submission.ImageSHA256 != images.lastMetadata.SHA256 {
		t.Fatalf("submission = %+v, want exact authenticated owner/key and stored metadata", submission)
	}
	if submission.JobID == submission.ImageKey || submission.EventID == submission.JobID || submission.EventID == submission.ImageKey {
		t.Fatalf("job/event/image identifiers are not independent: %+v", submission)
	}
	wantDigest := requestDigest(data, "image/png")
	if submission.RequestDigest != wantDigest || submission.RequestDigest == sha256.Sum256(data) {
		t.Fatal("request digest must bind exact bytes and normalized MIME with versioned framing")
	}
	file, err := images.store.Open(context.Background(), submission.ImageKey)
	if err != nil {
		t.Fatalf("Open(persisted image before successful return): %v", err)
	}
	_ = file.Close()
}

func TestAcceptPreflightReplayAndDigestConflictSkipImageWrite(t *testing.T) {
	service, repository, images := newTestService(t, 0)
	digest := requestDigest([]byte("same"), "image/jpeg")
	job := existingJob("job-old", "owner-42", "key", digest, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	repository.jobs[job.OwnerID+"\x00"+job.IdempotencyKey] = job
	accepted, err := service.Accept(context.Background(), job.OwnerID, job.IdempotencyKey, []byte("same"), "image/jpeg")
	if err != nil || accepted.JobID != job.ID || accepted.Status != job.Status {
		t.Fatalf("same-request replay = %+v, %v; want existing job", accepted, err)
	}
	if images.puts != 0 || repository.creates != 0 {
		t.Fatalf("preflight replay wrote or created: puts=%d creates=%d", images.puts, repository.creates)
	}
	if _, err := service.Accept(context.Background(), job.OwnerID, job.IdempotencyKey, []byte("different"), "image/jpeg"); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("different-content replay error = %v, want conflict", err)
	}
	if images.puts != 0 || repository.creates != 0 {
		t.Fatalf("preflight conflict wrote or created: puts=%d creates=%d", images.puts, repository.creates)
	}
	if _, err := service.Accept(context.Background(), job.OwnerID, job.IdempotencyKey, []byte("same"), "image/png"); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("same bytes with different normalized MIME error = %v, want conflict", err)
	}
}

func TestAcceptPreflightReplayStillWorksAtFullImageCapacity(t *testing.T) {
	service, repository, images := newTestService(t, 4)
	if _, err := images.store.Put(context.Background(), []byte("full"), "image/png"); err != nil {
		t.Fatalf("fill image-store capacity: %v", err)
	}
	data := []byte("same")
	winner := existingJob("job-retained", "owner", "key", requestDigest(data, "image/png"), "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	repository.jobs[winner.OwnerID+"\x00"+winner.IdempotencyKey] = winner
	accepted, err := service.Accept(context.Background(), "owner", "key", data, "image/png")
	if err != nil || accepted.JobID != winner.ID {
		t.Fatalf("preflight replay at full capacity = %+v, %v", accepted, err)
	}
	if images.puts != 0 || repository.creates != 0 {
		t.Fatalf("full-capacity replay wrote or created: puts=%d creates=%d", images.puts, repository.creates)
	}
}

func TestAcceptConfirmedReplayOrConflictDeletesOnlyLosingUpload(t *testing.T) {
	for _, outcome := range []jobs.Outcome{jobs.OutcomeReplay, jobs.OutcomeConflict} {
		t.Run(map[jobs.Outcome]string{jobs.OutcomeReplay: "replay", jobs.OutcomeConflict: "conflict"}[outcome], func(t *testing.T) {
			service, repository, images := newTestService(t, 0)
			data := []byte("candidate")
			winnerDigest := requestDigest(data, "image/png")
			winner := existingJob("job-winner", "owner", "key", winnerDigest, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
			if outcome == jobs.OutcomeConflict {
				winner.RequestDigest = requestDigest([]byte("other"), "image/png")
			}
			repository.createJob = winner
			repository.outcome = outcome
			accepted, err := service.Accept(context.Background(), "owner", "key", data, "image/png")
			if outcome == jobs.OutcomeReplay {
				if err != nil || accepted.JobID != winner.ID {
					t.Fatalf("confirmed replay = %+v, %v", accepted, err)
				}
			} else if !errors.Is(err, jobs.ErrConflict) {
				t.Fatalf("confirmed conflict error = %v", err)
			}
			if images.deletes != 1 {
				t.Fatalf("losing upload cleanup count = %d, want 1", images.deletes)
			}
			if _, err := images.store.Open(context.Background(), images.lastMetadata.Key); !errors.Is(err, imagestore.ErrNotFound) {
				t.Fatalf("losing image remains after confirmed outcome: %v", err)
			}
		})
	}
}

func TestAcceptCleanupFailureDoesNotChangeConfirmedReplay(t *testing.T) {
	service, repository, images := newTestService(t, 0)
	data := []byte("candidate")
	winner := existingJob("job-winner", "owner", "key", requestDigest(data, "image/png"), "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	repository.createJob, repository.outcome = winner, jobs.OutcomeReplay
	images.deleteErr = imagestore.ErrStorageFailure
	accepted, err := service.Accept(context.Background(), "owner", "key", data, "image/png")
	if err != nil || accepted.JobID != winner.ID || images.deletes != 1 {
		t.Fatalf("cleanup failure changed replay result: accepted=%+v err=%v deletes=%d", accepted, err, images.deletes)
	}
}

func TestAcceptRepositoryErrorKeepsImageForUnknownCommit(t *testing.T) {
	service, repository, images := newTestService(t, 0)
	repository.createErr = jobs.ErrUnavailable
	if _, err := service.Accept(context.Background(), "owner", "key", []byte("saved first"), "image/webp"); !errors.Is(err, jobs.ErrUnavailable) {
		t.Fatalf("Accept() error = %v, want unavailable", err)
	}
	if repository.creates != 1 || images.deletes != 0 {
		t.Fatalf("unknown commit cleanup/calls: creates=%d deletes=%d", repository.creates, images.deletes)
	}
	if _, err := images.store.Open(context.Background(), images.lastMetadata.Key); err != nil {
		t.Fatalf("unknown-commit image was deleted: %v", err)
	}
}

func TestAcceptPutFailureSkipsRepositoryAndCapacityRaceRechecksWinner(t *testing.T) {
	t.Run("failed put", func(t *testing.T) {
		service, repository, images := newTestService(t, 0)
		images.putErr = imagestore.ErrStorageFailure
		if _, err := service.Accept(context.Background(), "owner", "key", []byte("data"), "image/png"); !errors.Is(err, imagestore.ErrStorageFailure) {
			t.Fatalf("Accept() error = %v", err)
		}
		if repository.creates != 0 {
			t.Fatalf("CreateQueued called %d times after failed Put", repository.creates)
		}
	})
	t.Run("full capacity but concurrent replay winner", func(t *testing.T) {
		service, repository, images := newTestService(t, 4)
		data := []byte("full")
		images.putErr = imagestore.ErrCapacityExceeded
		winner := existingJob("job-winner", "owner", "key", requestDigest(data, "image/png"), "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
		repository.lookupHook = func(count int) {
			if count == 2 {
				repository.jobs[winner.OwnerID+"\x00"+winner.IdempotencyKey] = winner
			}
		}
		accepted, err := service.Accept(context.Background(), "owner", "key", data, "image/png")
		if err != nil || accepted.JobID != winner.ID {
			t.Fatalf("capacity race resolution = %+v, %v", accepted, err)
		}
		if repository.creates != 0 || images.deletes != 0 {
			t.Fatalf("capacity race created/deleted: creates=%d deletes=%d", repository.creates, images.deletes)
		}
	})
}

func TestAcceptCancellationAfterDurablePutDoesNotClaimOrDelete(t *testing.T) {
	service, repository, images := newTestService(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	images.afterPut = cancel
	if _, err := service.Accept(ctx, "owner", "key", []byte("durable"), "image/png"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Accept() error = %v, want canceled", err)
	}
	if repository.creates != 0 || images.deletes != 0 {
		t.Fatalf("canceled after Put: creates=%d deletes=%d", repository.creates, images.deletes)
	}
	if _, err := images.store.Open(context.Background(), images.lastMetadata.Key); err != nil {
		t.Fatalf("durable image removed after cancellation: %v", err)
	}
}

func TestAcceptRejectsMalformedInputsBeforeStorage(t *testing.T) {
	service, repository, images := newTestService(t, 0)
	cases := []struct {
		owner, key, media string
		data              []byte
	}{
		{"", "key", "image/png", []byte("x")},
		{"owner", "", "image/png", []byte("x")},
		{"owner", strings.Repeat("a", 129), "image/png", []byte("x")},
		{"owner", "bad\nkey", "image/png", []byte("x")},
		{"owner", "key", "IMAGE/PNG", []byte("x")},
		{"owner", "key", "image/png; charset=utf-8", []byte("x")},
		{"owner", "key", "image/png", nil},
	}
	for _, test := range cases {
		if _, err := service.Accept(context.Background(), test.owner, test.key, test.data, test.media); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("Accept(%q,%q,%q) error = %v, want invalid input", test.owner, test.key, test.media, err)
		}
	}
	if repository.lookups != 0 || images.puts != 0 {
		t.Fatalf("invalid inputs touched persistence: lookups=%d puts=%d", repository.lookups, images.puts)
	}
}

func existingJob(id, owner, key string, digest [sha256.Size]byte, imageKey string) jobs.Job {
	return jobs.Job{
		ID: id, OwnerID: owner, IdempotencyKey: key, RequestDigest: digest, ImageKey: imageKey,
		MediaType: "image/png", Size: 1, Status: jobs.StatusCompleted,
		CreatedAt: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 10, 30, 12, 1, 0, 0, time.UTC),
	}
}

func newTestService(t *testing.T, maxTotalBytes int64) (*Service, *fakeRepository, *trackedImages) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "images")
	store, err := imagestore.New(imagestore.Config{Root: root, MaxBytes: 1024, MaxTotalBytes: maxTotalBytes})
	if err != nil {
		t.Fatalf("imagestore.New(): %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("imagestore.Close(): %v", err)
		}
	})
	repository := &fakeRepository{jobs: make(map[string]jobs.Job)}
	images := &trackedImages{store: store}
	service, err := New(repository, images)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	return service, repository, images
}
