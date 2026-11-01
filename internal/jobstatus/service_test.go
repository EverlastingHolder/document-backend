package jobstatus

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"passport-backend/internal/api"
	"passport-backend/internal/jobs"
	"passport-backend/internal/passport"
)

const testJobID = "94b86c5c-2520-4c0f-b91e-6d4a948fc88a"

type statusRepositoryFunc func(context.Context, string, string) (jobs.StatusRecord, error)

func (f statusRepositoryFunc) GetOwnedStatus(ctx context.Context, ownerID, jobID string) (jobs.StatusRecord, error) {
	return f(ctx, ownerID, jobID)
}

func TestReadOwnedRendersAllStates(t *testing.T) {
	base := statusRecord(jobs.StatusQueued)
	response, err := api.MapExtraction(passport.Extraction{}, testJobID, "pipeline-v7")
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	pipeline := "pipeline-v7"
	errorCode := "processing_timeout"
	started := base.CreatedAt.Add(time.Second)
	finished := started.Add(time.Second)
	tests := []struct {
		name   string
		record jobs.StatusRecord
		check  func(*testing.T, Snapshot)
	}{
		{
			name:   "queued",
			record: base,
			check: func(t *testing.T, got Snapshot) {
				if got.StartedAt != nil || got.FinishedAt != nil || len(got.Result) != 0 || got.Error != nil {
					t.Fatalf("queued status contains terminal data: %#v", got)
				}
			},
		},
		{
			name: "queued after retry retains attempt start and version",
			record: func() jobs.StatusRecord {
				r := base
				priorStart := r.CreatedAt.Add(time.Second)
				version := "pipeline-v6"
				r.StartedAt, r.UpdatedAt, r.PipelineVersion = &priorStart, priorStart.Add(time.Second), &version
				return r
			}(),
			check: func(t *testing.T, got Snapshot) {
				if got.StartedAt == nil || got.FinishedAt != nil || len(got.Result) != 0 || got.Error != nil {
					t.Fatalf("retry queued status lost valid previous attempt timestamps: %#v", got)
				}
			},
		},
		{
			name: "processing",
			record: func() jobs.StatusRecord {
				r := base
				r.Status, r.StartedAt = jobs.StatusProcessing, &started
				r.UpdatedAt = started
				return r
			}(),
			check: func(t *testing.T, got Snapshot) {
				if got.StartedAt == nil || got.FinishedAt != nil || len(got.Result) != 0 || got.Error != nil {
					t.Fatalf("processing status has incorrect nullable fields: %#v", got)
				}
			},
		},
		{
			name: "completed",
			record: func() jobs.StatusRecord {
				r := base
				r.Status, r.StartedAt, r.FinishedAt = jobs.StatusCompleted, &started, &finished
				r.UpdatedAt, r.ResultJSON, r.PipelineVersion = finished, canonical, &pipeline
				return r
			}(),
			check: func(t *testing.T, got Snapshot) {
				if string(got.Result) != string(canonical) || got.Error != nil {
					t.Fatalf("completed result changed or error present: result=%s error=%#v", got.Result, got.Error)
				}
			},
		},
		{
			name: "failed before processing",
			record: func() jobs.StatusRecord {
				r := base
				r.Status, r.FinishedAt, r.ErrorCode = jobs.StatusFailed, &finished, &errorCode
				r.UpdatedAt = finished
				return r
			}(),
			check: func(t *testing.T, got Snapshot) {
				if got.StartedAt != nil || got.FinishedAt == nil || got.Error == nil || got.Error.Code != errorCode || got.Error.Message != publicFailureMessages[errorCode] || len(got.Result) != 0 {
					t.Fatalf("failed status is incorrect: %#v", got)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var gotOwner, gotID string
			reader := New(statusRepositoryFunc(func(_ context.Context, ownerID, jobID string) (jobs.StatusRecord, error) {
				gotOwner, gotID = ownerID, jobID
				return test.record, nil
			}))
			got, err := reader.ReadOwned(context.Background(), "trusted-owner", testJobID)
			if err != nil {
				t.Fatalf("ReadOwned() error = %v", err)
			}
			if gotOwner != "trusted-owner" || gotID != testJobID || got.JobID != testJobID || got.Status != test.record.Status {
				t.Fatalf("wrong query/snapshot identity: owner=%q queryID=%q response=%#v", gotOwner, gotID, got)
			}
			test.check(t, got)
		})
	}
}

func TestReadOwnedHidesRepositoryAndCorruptionDetails(t *testing.T) {
	privateSentinel := "PRIVATE-DATABASE-DIAGNOSTIC"
	tests := []struct {
		name   string
		record jobs.StatusRecord
		err    error
	}{
		{name: "repository error", err: errors.New(privateSentinel)},
		{name: "bad result JSON", record: func() jobs.StatusRecord {
			r := statusRecord(jobs.StatusCompleted)
			start, finish, version := r.CreatedAt.Add(time.Second), r.CreatedAt.Add(2*time.Second), "pipeline-v7"
			r.StartedAt, r.FinishedAt, r.UpdatedAt, r.PipelineVersion = &start, &finish, finish, &version
			r.ResultJSON = []byte(`{"private":"` + privateSentinel + `"}`)
			return r
		}()},
		{name: "wrong result request ID", record: func() jobs.StatusRecord {
			r := statusRecord(jobs.StatusCompleted)
			start, finish, version := r.CreatedAt.Add(time.Second), r.CreatedAt.Add(2*time.Second), "pipeline-v7"
			response, _ := api.MapExtraction(passport.Extraction{}, "8d2fae9c-efb7-4e98-8a90-f10b00edbdf2", version)
			r.StartedAt, r.FinishedAt, r.UpdatedAt, r.PipelineVersion = &start, &finish, finish, &version
			r.ResultJSON, _ = json.Marshal(response)
			return r
		}()},
		{name: "unknown failure code", record: func() jobs.StatusRecord {
			r := statusRecord(jobs.StatusFailed)
			finish := r.CreatedAt.Add(time.Second)
			r.FinishedAt, r.UpdatedAt = &finish, finish
			code := privateSentinel
			r.ErrorCode = &code
			return r
		}()},
		{name: "inconsistent timestamps", record: func() jobs.StatusRecord {
			r := statusRecord(jobs.StatusProcessing)
			started := r.CreatedAt.Add(2 * time.Second)
			r.StartedAt, r.UpdatedAt = &started, r.CreatedAt.Add(time.Second)
			return r
		}()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := New(statusRepositoryFunc(func(context.Context, string, string) (jobs.StatusRecord, error) {
				return test.record, test.err
			}))
			got, err := reader.ReadOwned(context.Background(), "trusted-owner", testJobID)
			if !errors.Is(err, jobs.ErrUnavailable) {
				t.Fatalf("ReadOwned() error = %v, want generic unavailable", err)
			}
			if strings.Contains(err.Error(), privateSentinel) || got.JobID != "" || len(got.Result) != 0 {
				t.Fatalf("private/corrupt data escaped: snapshot=%#v err=%v", got, err)
			}
		})
	}
}

func TestReadOwnedPreservesNotFoundAndDoesNotQueryWithoutOwner(t *testing.T) {
	called := false
	reader := New(statusRepositoryFunc(func(context.Context, string, string) (jobs.StatusRecord, error) {
		called = true
		return jobs.StatusRecord{}, jobs.ErrNotFound
	}))
	if _, err := reader.ReadOwned(context.Background(), "trusted-owner", testJobID); !errors.Is(err, jobs.ErrNotFound) {
		t.Fatalf("ReadOwned() error = %v, want not found", err)
	}
	called = false
	if _, err := reader.ReadOwned(context.Background(), "", testJobID); !errors.Is(err, jobs.ErrUnavailable) {
		t.Fatalf("empty owner error = %v, want unavailable", err)
	}
	if called {
		t.Fatal("repository queried without authenticated owner")
	}
}

func TestReadOwnedReturnsGoneOnlyForPurgedTerminalSnapshot(t *testing.T) {
	record := statusRecord(jobs.StatusCompleted)
	started := record.CreatedAt.Add(time.Second)
	finished := started.Add(time.Second)
	version := "pipeline-v7"
	record.StartedAt, record.FinishedAt, record.UpdatedAt = &started, &finished, finished
	record.PipelineVersion, record.ResultPurged = &version, true
	reader := New(statusRepositoryFunc(func(context.Context, string, string) (jobs.StatusRecord, error) {
		return record, nil
	}))
	if snapshot, err := reader.ReadOwned(context.Background(), "trusted-owner", testJobID); !errors.Is(err, jobs.ErrResultGone) || snapshot.JobID != "" {
		t.Fatalf("purged completed result = (%#v, %v), want empty snapshot and ErrResultGone", snapshot, err)
	}
	record.Status = jobs.StatusQueued
	if _, err := reader.ReadOwned(context.Background(), "trusted-owner", testJobID); !errors.Is(err, jobs.ErrUnavailable) {
		t.Fatalf("purged queued result error = %v, want ErrUnavailable", err)
	}
}

func statusRecord(status jobs.Status) jobs.StatusRecord {
	created := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	return jobs.StatusRecord{
		ID: testJobID, Status: status, CreatedAt: created, UpdatedAt: created,
	}
}
