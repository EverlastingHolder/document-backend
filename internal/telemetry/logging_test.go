package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestOpenWritesSafeJSONAndRotatesWithinConfiguredBounds(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "logs", "api.jsonl")
	sink, err := Open(Config{Path: path, Level: "info", MaxSizeBytes: 8192, MaxBackups: 2})
	if err != nil {
		t.Fatal(err)
	}
	logger := sink.Logger().With("component", "api")
	for i := 0; i < 400; i++ {
		logger.Info("service.started", "event", "service.started", "counter", i)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(path + "*")
	if err != nil {
		t.Fatal(err)
	}
	filteredFiles := files[:0]
	for _, file := range files {
		if file != path+".lock" {
			filteredFiles = append(filteredFiles, file)
		}
	}
	files = filteredFiles
	if len(files) < 2 || len(files) > 3 {
		t.Fatalf("rotation files = %d (%v), want current plus at most two backups", len(files), files)
	}
	for _, file := range files {
		info, err := os.Lstat(file)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > 8192 {
			t.Errorf("unsafe/unbounded log file %s: mode=%v size=%d", filepath.Base(file), info.Mode(), info.Size())
		}
		contents, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(contents)), "\n") {
			if line == "" {
				continue
			}
			var event map[string]any
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				t.Fatalf("invalid JSON log line: %v", err)
			}
			if event["service"] != "passport-api" || event["event"] != "service.started" || event["msg"] != "service.started" || event["component"] != "api" || event["time"] == nil || event["level"] != "INFO" {
				t.Errorf("missing base JSON fields: %#v", event)
			}
		}
	}
}

func TestOpenRejectsUnsafeExistingTargets(t *testing.T) {
	for name, setup := range map[string]func(*testing.T, string){
		"symlink": func(t *testing.T, path string) {
			if err := os.Symlink(filepath.Join(filepath.Dir(path), "target"), path); err != nil {
				t.Fatal(err)
			}
		},
		"directory": func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"unsafe mode": func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			// WriteFile's creation mode is filtered by the caller's umask. Force
			// the deliberately unsafe fixture mode so the rejection stays valid
			// in service environments that run tests with umask 077.
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(privateTempDir(t), "api.jsonl")
			setup(t, path)
			if _, err := Open(Config{Path: path, Level: "info", MaxSizeBytes: maxEventBytes, MaxBackups: 1}); err == nil {
				t.Fatal("Open() accepted unsafe target")
			}
		})
	}
}

func TestOpenValidatesLevelAndBounds(t *testing.T) {
	for _, config := range []Config{
		{Path: "x", Level: "verbose", MaxSizeBytes: maxEventBytes, MaxBackups: 1},
		{Path: "x", Level: "info", MaxSizeBytes: 0, MaxBackups: 1},
		{Path: "x", Level: "info", MaxSizeBytes: maxEventBytes, MaxBackups: -1},
		{Path: "x", Level: "info", MaxSizeBytes: maxEventBytes - 1, MaxBackups: 1},
	} {
		if _, err := Open(config); err == nil {
			t.Errorf("Open(%+v) succeeded for invalid configuration", config)
		}
	}
}

func TestOpenEnforcesSingleWriterAndRejectsOversizedStaleFiles(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "api.jsonl")
	config := Config{Path: path, Level: "info", MaxSizeBytes: maxEventBytes, MaxBackups: 1}
	sink, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(config); err == nil {
		t.Fatal("second writer acquired same log path")
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".2", []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(config); err == nil {
		t.Fatal("Open accepted backup outside configured retention")
	}
}

func TestSafeHandlerDropsUntrustedFieldsAndUnknownEvents(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "api.jsonl")
	sink, err := Open(Config{Path: path, Level: "debug", MaxSizeBytes: 1 << 16, MaxBackups: 1})
	if err != nil {
		t.Fatal(err)
	}
	logger := sink.Logger().With("component", "api")
	logger.Error("ocr.stage.failed", "event", "ocr.stage.failed", "stage", "vl", "stage_scope", "process", "duration_ms", int64(7), "outcome", "failure", "error_code", "pipeline_failed", "error", "PRIVATE-RAW-ERROR", "filename", "/private/passport.jpg", "body", "PRIVATE-OCR-TEXT")
	logger.Error("untrusted dynamic event PRIVATE-RAW-EVENT", "request_id", "94b86c5c-2520-4c0f-b91e-6d4a948fc88a", "message", "PRIVATE-RAW-MESSAGE")
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	data := string(mustRead(t, path))
	firstLine := strings.SplitN(data, "\n", 2)[0]
	for _, key := range []string{"\"service\"", "\"component\"", "\"event\"", "\"msg\""} {
		if count := strings.Count(firstLine, key); count != 1 {
			t.Errorf("schema field %s appears %d times in %s", key, count, firstLine)
		}
	}
	for _, secret := range []string{"PRIVATE-RAW-ERROR", "/private/passport.jpg", "PRIVATE-OCR-TEXT", "PRIVATE-RAW-EVENT", "PRIVATE-RAW-MESSAGE"} {
		if strings.Contains(data, secret) {
			t.Errorf("untrusted field %q leaked into %s", secret, data)
		}
	}
	entries := decodeTelemetryLines(t, []byte(data))
	if len(entries) != 2 || entries[0]["event"] != "ocr.stage.failed" || entries[0]["stage"] != "vl" || entries[0]["error_code"] != "pipeline_failed" || entries[1]["event"] != "logging.rejected" || entries[1]["error_code"] != "invalid_event" {
		t.Fatalf("safe event filtering mismatch: %#v", entries)
	}
}

func TestSafeHandlerAllowsBoundedMetricsFailureEvent(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "api.jsonl")
	sink, err := Open(Config{Path: path, Level: "debug", MaxSizeBytes: 1 << 16, MaxBackups: 1})
	if err != nil {
		t.Fatal(err)
	}
	sink.Logger().Error("metrics.persist.failed", "event", "metrics.persist.failed", "component", "runtime", "error_code", "metrics_write_failed", "route", "/v1/stats", "request_id", "94b86c5c-2520-4c0f-b91e-6d4a948fc88a", "path", "/private/state.json")
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	data := string(mustRead(t, path))
	if strings.Contains(data, "/private/state.json") {
		t.Fatalf("metrics failure log leaked private path: %s", data)
	}
	entries := decodeTelemetryLines(t, []byte(data))
	if len(entries) != 1 || entries[0]["event"] != "metrics.persist.failed" || entries[0]["error_code"] != "metrics_write_failed" || entries[0]["route"] != "/v1/stats" {
		t.Fatalf("metrics failure event not safely retained: %#v", entries)
	}
}

func TestWriteFailureEmitsOneSafeFallbackAndDoesNotPropagate(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "api.jsonl")
	sink, err := Open(Config{Path: path, Level: "debug", MaxSizeBytes: 1 << 16, MaxBackups: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.writer.file.Close(); err != nil {
		t.Fatal(err)
	}
	readPipe, writePipe, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	originalStderr := os.Stderr
	os.Stderr = writePipe
	sink.Logger().Error("service.failed", "event", "service.failed", "error_code", "startup_failed")
	sink.Logger().Error("service.failed", "event", "service.failed", "error_code", "startup_failed")
	os.Stderr = originalStderr
	if err := writePipe.Close(); err != nil {
		t.Fatal(err)
	}
	var fallback bytes.Buffer
	if _, err := fallback.ReadFrom(readPipe); err != nil {
		t.Fatal(err)
	}
	_ = readPipe.Close()
	lines := strings.Split(strings.TrimSpace(fallback.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("fallback lines = %d, want one: %q", len(lines), fallback.String())
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &event); err != nil {
		t.Fatalf("fallback not JSON: %v", err)
	}
	if event["event"] != "logging.failed" || event["error_code"] != "write_failed" || event["level"] != "ERROR" || event["service"] != "passport-api" {
		t.Fatalf("unsafe/incorrect fallback event: %#v", event)
	}
	if err := sink.Close(); err == nil {
		t.Fatal("Close() did not report the deliberately closed log file")
	}
}

func decodeTelemetryLines(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	entries := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("invalid log JSON: %v", err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func TestConcurrentWritesRemainValidJSON(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "api.jsonl")
	sink, err := Open(Config{Path: path, Level: "debug", MaxSizeBytes: 1 << 20, MaxBackups: 1})
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for i := 0; i < 20; i++ {
				sink.Logger().Info("service.started", "event", "service.started", "counter", i)
			}
		}()
	}
	group.Wait()
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(mustRead(t, path))), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("invalid concurrent JSON record: %v", err)
		}
	}
}

func TestRequestIDContextValidatesAndBoundsValue(t *testing.T) {
	ctx := WithRequestID(context.Background(), "94b86c5c-2520-4c0f-b91e-6d4a948fc88a")
	if got := RequestID(ctx); got != "94b86c5c-2520-4c0f-b91e-6d4a948fc88a" {
		t.Fatalf("RequestID = %q", got)
	}
	for _, value := range []string{"bad", strings.Repeat("a", 400)} {
		if got := RequestID(WithRequestID(context.Background(), value)); got != "" {
			t.Errorf("invalid ID accepted: %q", got)
		}
	}
	for _, value := range []string{"94B86c5c-2520-4c0f-b91e-6d4a948fc88a", "94b86c5c-2520-4c0f-b91e-6d4a948fc88A"} {
		if got := RequestID(WithRequestID(context.Background(), value)); got != value {
			t.Errorf("valid mixed-case ID = %q, want exact %q", got, value)
		}
	}
}

func TestLoggerOrDiscard(t *testing.T) {
	logger := LoggerOrDiscard(nil)
	if logger == nil {
		t.Fatal("LoggerOrDiscard(nil) = nil")
	}
}

func privateTempDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "private")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
