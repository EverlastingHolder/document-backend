package metrics

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(root, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	return Config{Path: filepath.Join(private, "metrics.json"), PipelineVersion: "test-v1"}
}

func persistedTestSnapshot(t *testing.T, config Config, recordHTTP bool) Snapshot {
	t.Helper()
	store, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	if recordHTTP {
		if err := store.ObserveHTTP(200, time.Millisecond, 1); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := store.Snapshot()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func writeTestSnapshot(t *testing.T, config Config, snapshot Snapshot) {
	t.Helper()
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.Path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOpenAcceptsSnapshotAtMaximumSize(t *testing.T) {
	config := testConfig(t)
	persistedTestSnapshot(t, config, false)
	data, err := os.ReadFile(config.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) >= maxStateBytes {
		t.Fatalf("seed snapshot size=%d, want less than %d", len(data), maxStateBytes)
	}
	data = append(data, bytes.Repeat([]byte(" "), maxStateBytes-len(data))...)
	if err := os.WriteFile(config.Path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(config)
	if err != nil {
		t.Fatalf("Open() rejected a valid snapshot exactly at the size limit: %v", err)
	}
	defer store.Close()
	if got := store.Snapshot(); got.PipelineVersion != config.PipelineVersion || got.Persistence.Healthy != true {
		t.Fatalf("opened boundary snapshot = %+v", got)
	}
}

func TestDecodeSnapshotRejectsOversizedValidJSON(t *testing.T) {
	config := testConfig(t)
	persistedTestSnapshot(t, config, false)
	data, err := os.ReadFile(config.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) >= maxStateBytes {
		t.Fatalf("seed snapshot size=%d, want less than %d", len(data), maxStateBytes)
	}
	data = append(data, bytes.Repeat([]byte(" "), maxStateBytes+1-len(data))...)
	var snapshot Snapshot
	if _, err := decodeSnapshot(data, config.PipelineVersion, &snapshot); err == nil {
		t.Fatal("decodeSnapshot accepted a valid JSON document above the state size limit")
	}
}

func TestStorePersistsBoundedHTTPOCRAndStageStatistics(t *testing.T) {
	config := testConfig(t)
	store, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveHTTP(200, 25*time.Millisecond, 1024); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveHTTP(422, 2*time.Second, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveHTTP(0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveOCRJob(false, "image_unprocessable", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveStage("vl", true, 700*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snapshot := store.Snapshot()
	if snapshot.SchemaVersion != "passport-metrics/v1" || snapshot.PipelineVersion != "test-v1" || !snapshot.Persistence.Healthy {
		t.Fatalf("snapshot metadata = %+v", snapshot)
	}
	if snapshot.HTTP.Total != 3 || snapshot.HTTP.Errors != 1 || snapshot.HTTP.ByStatus["0"] != 1 || snapshot.HTTP.ByStatus["200"] != 1 || snapshot.HTTP.ByStatus["422"] != 1 || snapshot.HTTP.ResponseBytesTotal != 1024 {
		t.Fatalf("HTTP stats = %+v", snapshot.HTTP)
	}
	if snapshot.HTTP.Duration.Count != 3 || snapshot.HTTP.Duration.SumMS != 2025 || snapshot.HTTP.Duration.Buckets[1].Count != 2 {
		t.Fatalf("HTTP histogram = %+v", snapshot.HTTP.Duration)
	}
	if snapshot.OCR.Total != 1 || snapshot.OCR.Completed != 0 || snapshot.OCR.Failed != 1 || snapshot.OCR.ByErrorCode["image_unprocessable"] != 1 {
		t.Fatalf("OCR stats = %+v", snapshot.OCR)
	}
	if snapshot.Stages["vl"].Completed != 1 || snapshot.Stages["vl"].Duration.Count != 1 || snapshot.Stages["ocr"].Completed != 0 {
		t.Fatalf("stage stats = %+v", snapshot.Stages)
	}
	data, err := os.ReadFile(config.Path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Snapshot
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.HTTP.ByStatus) != len(statusCodes) || len(decoded.OCR.ByErrorCode) != len(errorCodes) || len(decoded.Stages) != len(stageNames) {
		t.Fatalf("snapshot cardinality is not fixed: http=%d errors=%d stages=%d", len(decoded.HTTP.ByStatus), len(decoded.OCR.ByErrorCode), len(decoded.Stages))
	}
}

func TestOpenEnforcesPrivatePermissionsAndSingleWriter(t *testing.T) {
	config := testConfig(t)
	store, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(config); err == nil {
		t.Fatal("second writer acquired same state path")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Dir(config.Path), config.Path, config.Path + ".lock"} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0o600)
		if path == filepath.Dir(config.Path) {
			want = 0o700
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s mode=%o want=%o", path, info.Mode().Perm(), want)
		}
	}
}

func TestOpenRejectsCorruptStateAndPipelineMismatch(t *testing.T) {
	config := testConfig(t)
	store, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(config.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range [][]byte{append(append([]byte(nil), data...), []byte(` {}`)...), []byte(`{"schema_version":"passport-metrics/v1","pipeline_version":"test-v1","http":{"by_status":{"999":1}}}`)} {
		if err := os.WriteFile(config.Path, invalid, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(config); err == nil {
			t.Fatal("Open accepted invalid state")
		}
	}
	if err := os.WriteFile(config.Path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	config.PipelineVersion = "different-v2"
	if _, err := Open(config); err == nil {
		t.Fatal("Open accepted pipeline mismatch")
	}
}

func TestOpenRejectsDuplicateSnapshotKeys(t *testing.T) {
	config := testConfig(t)
	persistedTestSnapshot(t, config, false)
	data, err := os.ReadFile(config.Path)
	if err != nil {
		t.Fatal(err)
	}
	data = append([]byte(`{"schema_version":"passport-metrics/v1",`), data[1:]...)
	if err := os.WriteFile(config.Path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if store, err := Open(config); err == nil {
		_ = store.Close()
		t.Fatal("Open() accepted a persisted snapshot with a duplicate key")
	}
}

func TestOpenRejectsInconsistentPersistedCountersAndHistograms(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Snapshot)
	}{
		{
			name: "completed OCR count cannot exceed total",
			mutate: func(snapshot *Snapshot) {
				snapshot.OCR.Completed = 1
				snapshot.OCR.Failed = ^uint64(0)
				snapshot.OCR.ByErrorCode["image_unprocessable"] = ^uint64(0)
			},
		},
		{
			name: "HTTP status counts must match total",
			mutate: func(snapshot *Snapshot) {
				snapshot.HTTP.ByStatus["200"] = 0
			},
		},
		{
			name: "HTTP duration count must match total",
			mutate: func(snapshot *Snapshot) {
				snapshot.HTTP.Duration.Count = 0
				snapshot.HTTP.Duration.SumMS = 0
				snapshot.HTTP.Duration.MinMS = 0
				snapshot.HTTP.Duration.MaxMS = 0
				for i := range snapshot.HTTP.Duration.Buckets {
					snapshot.HTTP.Duration.Buckets[i].Count = 0
				}
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig(t)
			snapshot := persistedTestSnapshot(t, config, true)
			test.mutate(&snapshot)
			writeTestSnapshot(t, config, snapshot)
			if store, err := Open(config); err == nil {
				_ = store.Close()
				t.Fatal("Open() accepted inconsistent persisted counters")
			}
		})
	}
}

func TestOpenAcceptsPersistedSuccessfulOCRJob(t *testing.T) {
	config := testConfig(t)
	store, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveOCRJob(true, "", 25*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(config)
	if err != nil {
		t.Fatalf("Open() rejected a persisted successful OCR job: %v", err)
	}
	defer store.Close()
	got := store.Snapshot().OCR
	if got.Total != 1 || got.Completed != 1 || got.Failed != 0 || got.Duration.Count != 1 {
		t.Fatalf("persisted OCR counters = %+v", got)
	}
}

func TestOpenUpgradesLegacyHTTPStatusBucketsAdditively(t *testing.T) {
	config := testConfig(t)
	store, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveHTTP(200, time.Millisecond, 12); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(config.Path)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(data, &legacy); err != nil {
		t.Fatal(err)
	}
	httpStats := legacy["http"].(map[string]any)
	byStatus := httpStats["by_status"].(map[string]any)
	delete(byStatus, "201")
	delete(byStatus, "202")
	delete(byStatus, "401")
	delete(byStatus, "409")
	legacyData, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.Path, legacyData, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err = Open(config)
	if err != nil {
		t.Fatalf("Open() rejected legacy fixed buckets: %v", err)
	}
	if got := store.Snapshot(); got.HTTP.ByStatus["200"] != 1 || got.HTTP.ByStatus["202"] != 0 || got.HTTP.ByStatus["401"] != 0 || got.HTTP.ByStatus["409"] != 0 {
		t.Fatalf("upgraded HTTP buckets=%v", got.HTTP.ByStatus)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	persisted, err := os.ReadFile(config.Path)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(persisted, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.HTTP.ByStatus) != len(statusCodes) {
		t.Fatalf("upgraded snapshot has %d status buckets, want %d", len(snapshot.HTTP.ByStatus), len(statusCodes))
	}
}

func TestOpenAddsClientRegistrationBucketToPreviousSnapshot(t *testing.T) {
	config := testConfig(t)
	store, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	contents, err := os.ReadFile(config.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(contents, &snapshot); err != nil {
		t.Fatal(err)
	}
	httpStats := snapshot["http"].(map[string]any)
	byStatus := httpStats["by_status"].(map[string]any)
	delete(byStatus, "201")
	previous, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.Path, previous, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err = Open(config)
	if err != nil {
		t.Fatalf("Open() rejected prior fixed status buckets: %v", err)
	}
	defer store.Close()
	if got := store.Snapshot().HTTP.ByStatus; len(got) != len(statusCodes) || got["201"] != 0 {
		t.Fatalf("upgraded HTTP buckets=%v", got)
	}
}

func TestAsyncHTTPStatusBucketsDoNotImplyOCRCompletion(t *testing.T) {
	store, err := Open(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, status := range []int{202, 401, 409} {
		if err := store.ObserveHTTP(status, time.Millisecond, 0); err != nil {
			t.Fatalf("ObserveHTTP(%d): %v", status, err)
		}
	}
	snapshot := store.Snapshot()
	if snapshot.HTTP.Total != 3 || snapshot.HTTP.Errors != 2 || snapshot.HTTP.ByStatus["202"] != 1 || snapshot.HTTP.ByStatus["401"] != 1 || snapshot.HTTP.ByStatus["409"] != 1 {
		t.Fatalf("HTTP stats=%+v", snapshot.HTTP)
	}
	if snapshot.OCR.Total != 0 || snapshot.OCR.Completed != 0 || snapshot.OCR.Failed != 0 {
		t.Fatalf("HTTP-only async statuses changed OCR counters: %+v", snapshot.OCR)
	}
}

func TestOpenRejectsHistogramWhoseMeanFallsBelowObservedMinimum(t *testing.T) {
	config := testConfig(t)
	store, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(config.Path)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.HTTP.Total = 3
	snapshot.HTTP.ByStatus["200"] = 3
	hist := &snapshot.HTTP.Duration
	hist.Count, hist.SumMS, hist.MinMS, hist.MaxMS = 3, 250, 100, 200
	for i := range hist.Buckets {
		boundary := hist.Buckets[i].LEMS
		if boundary != nil && *boundary >= 100 && *boundary < 250 {
			hist.Buckets[i].Count = 2
		} else if boundary != nil && *boundary >= 250 || boundary == nil {
			hist.Buckets[i].Count = 3
		}
	}
	corrupt, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.Path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(config); err == nil {
		t.Fatal("Open accepted histogram whose mean is below all observations")
	}
}

func TestObservationsValidateAndOverflowWithoutMutating(t *testing.T) {
	store, err := Open(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, call := range []func() error{
		func() error { return store.ObserveHTTP(418, time.Second, 0) },
		func() error { return store.ObserveHTTP(200, -time.Second, 0) },
		func() error { return store.ObserveOCRJob(false, "raw-error", time.Second) },
		func() error { return store.ObserveStage("unknown", true, time.Second) },
	} {
		if err := call(); err == nil {
			t.Fatal("invalid observation succeeded")
		}
	}
	if err := store.ObserveHTTP(201, time.Second, 0); err != nil {
		t.Fatalf("201 registration response was rejected: %v", err)
	}
	if err := store.ObserveHTTP(200, 10*time.Millisecond, 1); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.snapshot.HTTP.Total = ^uint64(0)
	store.mu.Unlock()
	if err := store.ObserveHTTP(200, 10*time.Millisecond, 1); !errors.Is(err, ErrCounterOverflow) {
		t.Fatalf("overflow error = %v", err)
	}
	if store.Snapshot().HTTP.Total != ^uint64(0) {
		t.Fatal("overflow changed the in-memory counter")
	}
}

func TestConcurrentObservationsAreCounted(t *testing.T) {
	store, err := Open(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const workers, perWorker = 8, 8
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWorker {
				if err := store.ObserveHTTP(200, time.Millisecond, 1); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if got := store.Snapshot().HTTP.Total; got != workers*perWorker {
		t.Fatalf("total=%d want=%d", got, workers*perWorker)
	}
}

func TestPersistenceFailureIsReportedWithoutDroppingInMemoryObservation(t *testing.T) {
	config := testConfig(t)
	store, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(config.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(config.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveHTTP(200, time.Millisecond, 0); err == nil {
		t.Fatal("observation succeeded despite unavailable snapshot path")
	}
	snapshot := store.Snapshot()
	if snapshot.Persistence.Healthy || snapshot.Persistence.ErrorCode != "metrics_write_failed" || snapshot.HTTP.Total != 1 {
		t.Fatalf("persistence failure state = %+v", snapshot)
	}
	if err := store.Close(); err == nil {
		t.Fatal("Close succeeded despite unavailable snapshot path")
	}
}
