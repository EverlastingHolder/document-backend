package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"passport-backend/internal/httpapi"
	"passport-backend/internal/jobs"
	"passport-backend/internal/outbox"
	"passport-backend/internal/server"
)

type statusRepositoryStub struct {
	owner string
	jobID string
}

func (s *statusRepositoryStub) GetOwnedStatus(_ context.Context, ownerID, jobID string) (jobs.StatusRecord, error) {
	s.owner, s.jobID = ownerID, jobID
	return jobs.StatusRecord{}, jobs.ErrNotFound
}

func TestReadPrivateDatabaseDSNRequiresPrivateRegularFile(t *testing.T) {
	root := t.TempDir()
	private := filepath.Join(root, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(private, "dsn.txt")
	if err := os.WriteFile(path, []byte("postgres://user:secret@localhost/db\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dsn, err := readPrivateDatabaseDSN(path)
	if err != nil || dsn != "postgres://user:secret@localhost/db" {
		t.Fatalf("readPrivateDatabaseDSN() = %q, %v", dsn, err)
	}

	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateDatabaseDSN(path); err == nil {
		t.Fatal("readPrivateDatabaseDSN accepted a group-readable DSN")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(private, "dsn-link.txt")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateDatabaseDSN(link); err == nil {
		t.Fatal("readPrivateDatabaseDSN accepted a symlink")
	}
}

func TestOpenAsyncJobsAbsentKeepsLegacyMode(t *testing.T) {
	resources, err := openAsyncJobs(t.Context(), nil, 1<<20, nil, nil, server.Config{})
	if err != nil || resources == nil || resources.publicConfig != nil || resources.cleanup == nil {
		t.Fatalf("openAsyncJobs() returned resources=%+v err=%v", resources, err)
	}
	if err := resources.cleanup(); err != nil {
		t.Fatal(err)
	}
}

func TestAsyncPublicConfigInjectsStatusReaderWithoutQueueConfig(t *testing.T) {
	repository := &statusRepositoryStub{}
	config := newAsyncPublicConfig(nil, nil, repository, 2, 7*time.Second)
	if config.StatusReader == nil || config.StatusReadTimeout != 7*time.Second {
		t.Fatalf("status reader config=%+v", config)
	}
	if _, err := config.StatusReader.ReadOwned(t.Context(), "owner", "job"); !errors.Is(err, jobs.ErrNotFound) {
		t.Fatalf("ReadOwned() error=%v, want ErrNotFound", err)
	}
	if repository.owner != "owner" || repository.jobID != "job" {
		t.Fatalf("status reader did not use the same repository: owner=%q job=%q", repository.owner, repository.jobID)
	}
	var _ httpapi.AsyncStatusReader = config.StatusReader
}

func TestOutboxCheckTreatsOnlyBrokerUnavailabilityAsDegraded(t *testing.T) {
	if err := handleOutboxCheckResult(outbox.ErrBrokerUnavailable, nil); err != nil {
		t.Fatalf("temporary broker outage should leave durable intake enabled: %v", err)
	}
	if err := handleOutboxCheckResult(outbox.ErrTopologyMismatch, nil); err != outbox.ErrTopologyMismatch {
		t.Fatalf("permanent topology mismatch error=%v, want typed failure", err)
	}
	for _, permanentErr := range []error{outbox.ErrRejectedAuth, outbox.ErrInvalidConfig} {
		if err := handleOutboxCheckResult(permanentErr, nil); err != permanentErr {
			t.Errorf("permanent broker check error=%v, want typed failure %v", err, permanentErr)
		}
	}
	unexpected := errors.New("unexpected internal failure")
	if err := handleOutboxCheckResult(unexpected, nil); err != unexpected {
		t.Fatalf("unexpected check error=%v, want original error", err)
	}
}
