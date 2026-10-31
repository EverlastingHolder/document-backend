package metrics

import (
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
		func() error { return store.ObserveHTTP(201, time.Second, 0) },
		func() error { return store.ObserveHTTP(200, -time.Second, 0) },
		func() error { return store.ObserveOCRJob(false, "raw-error", time.Second) },
		func() error { return store.ObserveStage("unknown", true, time.Second) },
	} {
		if err := call(); err == nil {
			t.Fatal("invalid observation succeeded")
		}
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
