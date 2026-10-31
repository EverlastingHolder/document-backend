package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"passport-backend/internal/metrics"
	"passport-backend/internal/passport"
)

func openTestMetrics(t *testing.T) *metrics.Store {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(root, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := metrics.Open(metrics.Config{Path: filepath.Join(private, "stats.json"), PipelineVersion: testPipeline})
	if err != nil {
		t.Fatalf("metrics.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("metrics.Close() error = %v", err)
		}
	})
	return store
}

func TestStatsEndpointUsesLANACLAndExcludesItsOwnRequests(t *testing.T) {
	store := openTestMetrics(t)
	logger, logPath, closeSink := testLogSink(t)
	defer closeSink()
	config := testConfig()
	config.Metrics = store
	config.Logger = logger
	config.AllowedClientNetworks = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	recognizer := &fakeRecognizer{extraction: validTestExtraction()}
	handler := newTestHandler(t, config, recognizer)

	body, contentType := syntheticUploadRequest(t)
	recognize := httptest.NewRequest(http.MethodPost, handlerPath, body)
	recognize.Header.Set("Content-Type", contentType)
	recognize.RemoteAddr = "127.0.0.1:1234"
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, recognize)
	if result.Code != http.StatusOK {
		t.Fatalf("recognize status = %d, want 200: %s", result.Code, result.Body.String())
	}

	const requestID = "94B86c5c-2520-4c0f-b91e-6d4a948fc88a"
	statsRequest := httptest.NewRequest(http.MethodGet, "http://example.invalid/v1/stats?secret=ignored", nil)
	statsRequest.Header.Set("X-Request-ID", requestID)
	statsRequest.RemoteAddr = "127.0.0.1:4321"
	statsResponse := httptest.NewRecorder()
	handler.ServeHTTP(statsResponse, statsRequest)
	if statsResponse.Code != http.StatusOK {
		t.Fatalf("stats status = %d, want 200: %s", statsResponse.Code, statsResponse.Body.String())
	}
	if statsResponse.Header().Get("X-Request-ID") != requestID {
		t.Fatalf("X-Request-ID = %q, want %q", statsResponse.Header().Get("X-Request-ID"), requestID)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(statsResponse.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode stats envelope: %v", err)
	}
	if len(envelope) != 2 || envelope["request_id"] == nil || envelope["statistics"] == nil || envelope["pipeline_version"] != nil {
		t.Fatalf("stats envelope keys = %v, want request_id and statistics only", envelope)
	}
	var payload struct {
		RequestID  string           `json:"request_id"`
		Statistics metrics.Snapshot `json:"statistics"`
	}
	if err := json.Unmarshal(statsResponse.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode stats payload: %v", err)
	}
	if payload.RequestID != requestID || payload.Statistics.HTTP.Total != 1 || payload.Statistics.HTTP.Errors != 0 || payload.Statistics.HTTP.ByStatus["200"] != 1 || payload.Statistics.HTTP.Duration.Count != 1 || payload.Statistics.HTTP.ResponseBytesTotal == 0 {
		t.Fatalf("stats payload request_id=%q HTTP=%+v", payload.RequestID, payload.Statistics.HTTP)
	}
	if recognizer.callCount.Load() != 1 {
		t.Fatalf("recognizer calls = %d, want 1", recognizer.callCount.Load())
	}

	denied := httptest.NewRequest(http.MethodGet, "http://example.invalid/v1/stats", nil)
	denied.RemoteAddr = "203.0.113.9:1234"
	deniedRecorder := httptest.NewRecorder()
	handler.ServeHTTP(deniedRecorder, denied)
	if deniedRecorder.Code != http.StatusForbidden {
		t.Fatalf("disallowed stats status = %d, want 403", deniedRecorder.Code)
	}
	if strings.Contains(deniedRecorder.Body.String(), "statistics") {
		t.Fatalf("disallowed response exposed statistics: %s", deniedRecorder.Body.String())
	}
	if got := store.Snapshot().HTTP.Total; got != 1 {
		t.Fatalf("HTTP total after stats and rejected stats requests = %d, want 1", got)
	}
	if err := closeSink(); err != nil {
		t.Fatal(err)
	}
	entries := decodeLogLines(t, mustReadHTTPLog(t, logPath))
	if len(entries) != 3 || entries[1]["route"] != statsPath || entries[1]["status"] != float64(http.StatusOK) || entries[2]["route"] != statsPath || entries[2]["status"] != float64(http.StatusForbidden) {
		t.Fatalf("stats access requests were not logged safely: %#v", entries)
	}
}

func TestStatsEndpointMethodAndMissingStoreResponses(t *testing.T) {
	for _, test := range []struct {
		name       string
		method     string
		config     Config
		wantStatus int
		wantCode   string
	}{
		{name: "wrong method", method: http.MethodPost, config: testConfig(), wantStatus: http.StatusMethodNotAllowed, wantCode: "method_not_allowed"},
		{name: "store unavailable", method: http.MethodGet, config: testConfig(), wantStatus: http.StatusServiceUnavailable, wantCode: "statistics_unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := newTestHandler(t, test.config, &fakeRecognizer{})
			request := httptest.NewRequest(test.method, "http://example.invalid/v1/stats", nil)
			request.RemoteAddr = "127.0.0.1:1234"
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, test.wantStatus, recorder.Body.String())
			}
			var response errorResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode error: %v", err)
			}
			if response.Code != test.wantCode || !isUUID(response.RequestID) || recorder.Header().Get("X-Request-ID") != response.RequestID {
				t.Fatalf("error response = %+v header request ID=%q", response, recorder.Header().Get("X-Request-ID"))
			}
			if test.method == http.MethodPost && recorder.Header().Get("Allow") != http.MethodGet {
				t.Fatalf("Allow = %q, want GET", recorder.Header().Get("Allow"))
			}
		})
	}
}

func TestStatsEndpointDoesNotWaitForRecognitionSemaphore(t *testing.T) {
	store := openTestMetrics(t)
	config := testConfig()
	config.Metrics = store
	started := make(chan struct{})
	release := make(chan struct{})
	recognizer := &fakeRecognizer{recognize: func(context.Context, Image) (passport.Extraction, error) {
		close(started)
		<-release
		return validTestExtraction(), nil
	}}
	handler := newTestHandler(t, config, recognizer)
	body, contentType := syntheticUploadRequest(t)
	recognize := httptest.NewRequest(http.MethodPost, handlerPath, body)
	recognize.Header.Set("Content-Type", contentType)
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), recognize)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("recognizer did not start")
	}

	statsRequest := httptest.NewRequest(http.MethodGet, "http://example.invalid/v1/stats", nil)
	statsResponse := httptest.NewRecorder()
	handler.ServeHTTP(statsResponse, statsRequest)
	if statsResponse.Code != http.StatusOK {
		t.Fatalf("stats status while recognition occupies semaphore = %d, want 200: %s", statsResponse.Code, statsResponse.Body.String())
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("recognition request did not finish")
	}
	if recognizer.callCount.Load() != 1 {
		t.Fatalf("recognizer calls = %d, want 1", recognizer.callCount.Load())
	}
}

func TestHTTPMetricsCountUnmatchedAndTerminalRequestsWithoutAStatus(t *testing.T) {
	store := openTestMetrics(t)
	config := testConfig()
	config.Metrics = store

	missingHandler := newTestHandler(t, config, &fakeRecognizer{})
	missing := httptest.NewRequest(http.MethodGet, "http://example.invalid/private/path?token=ignored", nil)
	missingRecorder := httptest.NewRecorder()
	missingHandler.ServeHTTP(missingRecorder, missing)
	if missingRecorder.Code != http.StatusNotFound {
		t.Fatalf("unmatched route status = %d, want 404", missingRecorder.Code)
	}

	cancelledHandler := newTestHandler(t, config, &fakeRecognizer{})
	body, contentType := syntheticUploadRequest(t)
	cancelledContext, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := httptest.NewRequest(http.MethodPost, handlerPath, body).WithContext(cancelledContext)
	cancelled.Header.Set("Content-Type", contentType)
	cancelledRecorder := httptest.NewRecorder()
	cancelledHandler.ServeHTTP(cancelledRecorder, cancelled)
	if cancelledRecorder.Body.Len() != 0 {
		t.Fatalf("cancelled request wrote %d response bytes, want none", cancelledRecorder.Body.Len())
	}

	panicHandler := newTestHandler(t, config, &fakeRecognizer{recognize: func(context.Context, Image) (passport.Extraction, error) {
		panic("private panic detail")
	}})
	body, contentType = syntheticUploadRequest(t)
	panicking := httptest.NewRequest(http.MethodPost, handlerPath, body)
	panicking.Header.Set("Content-Type", contentType)
	panickingRecorder := httptest.NewRecorder()
	func() {
		defer func() {
			if recovered := recover(); recovered != http.ErrAbortHandler {
				t.Fatalf("panic = %#v, want http.ErrAbortHandler", recovered)
			}
		}()
		panicHandler.ServeHTTP(panickingRecorder, panicking)
	}()
	if panickingRecorder.Body.Len() != 0 {
		t.Fatalf("panicking request wrote %d response bytes, want none", panickingRecorder.Body.Len())
	}

	snapshot := store.Snapshot()
	if snapshot.HTTP.Total != 3 || snapshot.HTTP.ByStatus["404"] != 1 || snapshot.HTTP.ByStatus["0"] != 2 || snapshot.HTTP.Errors != 1 {
		t.Fatalf("HTTP stats = %+v, want one 404 and two status-zero terminal requests", snapshot.HTTP)
	}
}
