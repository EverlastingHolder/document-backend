package retention

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"passport-backend/internal/imagestore"
	"passport-backend/internal/jobs"
)

type repositoryStub struct {
	candidate  ImageCandidate
	found      bool
	claimed    int
	marked     int
	released   int
	expired    int
	claimErr   error
	markErr    error
	referenced map[string]bool
}

func (r *repositoryStub) ClaimTerminalImage(context.Context, string, time.Duration) (ImageCandidate, bool, error) {
	r.claimed++
	return r.candidate, r.found, r.claimErr
}
func (r *repositoryStub) ClaimTerminalImages(context.Context, int, time.Duration) ([]ImageCandidate, error) {
	if !r.found {
		return nil, nil
	}
	r.claimed++
	return []ImageCandidate{r.candidate}, nil
}
func (r *repositoryStub) MarkImagePurged(context.Context, ImageCandidate) error {
	r.marked++
	return r.markErr
}
func (r *repositoryStub) ReleaseImagePurge(context.Context, ImageCandidate) error {
	r.released++
	return nil
}
func (r *repositoryStub) PurgeExpiredResults(context.Context, int) (int, error) {
	return r.expired, nil
}
func (r *repositoryStub) ReadMaintenanceSnapshot(context.Context) (jobs.MaintenanceSnapshot, error) {
	return jobs.MaintenanceSnapshot{}, nil
}
func (r *repositoryStub) IsImageReferenced(_ context.Context, key string) (bool, error) {
	return r.referenced[key], nil
}

type imageDeleterStub struct {
	keys []string
	err  error
}

func (d *imageDeleterStub) DeleteObject(_ context.Context, key string, _ int64, _ string) error {
	d.keys = append(d.keys, key)
	return d.err
}

func TestPurgeJobImageClaimsBeforeDeleteAndMarksAfter(t *testing.T) {
	repository := &repositoryStub{candidate: ImageCandidate{JobID: "job", Key: "opaque", Size: 10, ClaimToken: "claim"}, found: true}
	images := &imageDeleterStub{}
	service, err := New(repository, images, Config{ClaimLease: time.Minute, Interval: time.Minute, OperationTimeout: time.Second, BatchSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.PurgeJobImage(context.Background(), "job"); err != nil {
		t.Fatal(err)
	}
	if repository.claimed != 1 || len(images.keys) != 1 || images.keys[0] != "opaque" || repository.marked != 1 || repository.released != 0 {
		t.Fatalf("purge sequence claim/delete/mark/release = %d/%v/%d/%d", repository.claimed, images.keys, repository.marked, repository.released)
	}
}

func TestNewRequiresClaimLeaseLongerThanOperationTimeout(t *testing.T) {
	repository := &repositoryStub{}
	images := &imageDeleterStub{}
	if _, err := New(repository, images, Config{ClaimLease: time.Second, Interval: time.Second, OperationTimeout: time.Second, BatchSize: 1}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New(equal claim/operation budgets) = %v, want invalid config", err)
	}
}

func TestPurgeJobImageReleasesFailedDeleteForRetry(t *testing.T) {
	repository := &repositoryStub{candidate: ImageCandidate{JobID: "job", Key: "opaque", Size: 10, ClaimToken: "claim"}, found: true}
	images := &imageDeleterStub{err: errors.New("private path diagnostic")}
	service, err := New(repository, images, Config{ClaimLease: time.Minute, Interval: time.Minute, OperationTimeout: time.Second, BatchSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.PurgeJobImage(context.Background(), "job"); !errors.Is(err, jobs.ErrUnavailable) {
		t.Fatalf("purge error = %v, want generic unavailable", err)
	}
	if repository.released != 1 || repository.marked != 0 {
		t.Fatalf("failed delete was not released for retry: release=%d mark=%d", repository.released, repository.marked)
	}
}

func TestOfflineOrphanSweepKeepsDatabaseReferencesAndDeletesOnlyStoreNames(t *testing.T) {
	root := filepath.Join(t.TempDir(), "images")
	store, err := imagestore.New(imagestore.Config{Root: root, MaxBytes: 128})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	referenced, err := store.Put(context.Background(), []byte("still referenced"), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := store.Put(context.Background(), []byte("unreferenced"), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	temporaryKey := ".upload-0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(filepath.Join(root, temporaryKey), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	repository := &repositoryStub{referenced: map[string]bool{referenced.Key: true}}
	service, err := New(repository, store, Config{ClaimLease: time.Minute, Interval: time.Second, OperationTimeout: time.Second, BatchSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	removed, err := service.PurgeOrphansOffline(context.Background())
	if err != nil || removed != 2 {
		t.Fatalf("offline orphan sweep = (%d,%v), want orphan object + temp", removed, err)
	}
	if _, err := store.Open(context.Background(), referenced.Key); err != nil {
		t.Fatalf("referenced object was deleted: %v", err)
	}
	if _, err := store.Open(context.Background(), orphan.Key); !errors.Is(err, imagestore.ErrNotFound) {
		t.Fatalf("unreferenced object remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, temporaryKey)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial temp remains: %v", err)
	}
}

var _ ImageDeleter = (*imagestore.Store)(nil)
