package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"passport-backend/internal/clientauth"
	"passport-backend/internal/jobs"
	"passport-backend/internal/passport"
	"passport-backend/internal/telemetry"
)

func TestRequestLogIsCorrelatedAndExcludesSensitiveInputs(t *testing.T) {
	logger, path, closeSink := testLogSink(t)
	defer closeSink()
	const requestID = "94B86c5c-2520-4c0f-b91e-6d4a948fc88a"
	const sentinel = "PRIVATE-PASSPORT-LOG-SENTINEL"
	var gotRequestID string
	recognizer := &fakeRecognizer{recognize: func(ctx context.Context, _ Image) (passport.Extraction, error) {
		gotRequestID = telemetry.RequestID(ctx)
		return passport.Extraction{}, errors.New(sentinel)
	}}
	config := testConfig()
	config.Logger = logger
	handler := newTestHandler(t, config, recognizer)
	recorder := serveUploadWithHeaders(t, handler, uploadSpec{
		fileName: sentinel + ".png", mediaType: "image/png", data: syntheticPNG(),
	}, http.Header{"X-Request-Id": []string{requestID}, "X-Private-Sentinel": []string{sentinel}})
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
	if gotRequestID != requestID {
		t.Fatalf("recognizer request ID = %q, want %q", gotRequestID, requestID)
	}
	if err := closeSink(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), sentinel) || strings.Contains(string(data), "X-Private-Sentinel") {
		t.Fatalf("sensitive request data leaked into log: %s", data)
	}
	entries := decodeLogLines(t, data)
	if len(entries) != 1 {
		t.Fatalf("logged events = %d, want one final request event: %s", len(entries), data)
	}
	entry := entries[0]
	if entry["event"] != "http.request.completed" || entry["msg"] != "http.request.completed" || entry["component"] != "httpapi" || entry["service"] != "passport-api" || entry["request_id"] != requestID || entry["method"] != "POST" || entry["route"] != recognizePath || entry["status"] != float64(500) || entry["error_code"] != "processing_failed" || entry["response_bytes"] == nil || entry["duration_ms"] == nil {
		t.Fatalf("unexpected request log fields: %#v", entry)
	}
}

func TestSuccessfulRequestLogHasInfoStatusAndTiming(t *testing.T) {
	logger, path, closeSink := testLogSink(t)
	defer closeSink()
	config := testConfig()
	config.Logger = logger
	handler := newTestHandler(t, config, &fakeRecognizer{extraction: validTestExtraction()})
	recorder := serveUploadWithHeaders(t, handler, uploadSpec{fileName: "ignored.png", mediaType: "image/png", data: syntheticPNG()}, http.Header{
		"X-Request-Id": []string{"94B86c5c-2520-4c0f-b91e-6d4a948fc88a"},
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if err := closeSink(); err != nil {
		t.Fatal(err)
	}
	entries := decodeLogLines(t, mustReadHTTPLog(t, path))
	if len(entries) != 1 || entries[0]["event"] != "http.request.completed" || entries[0]["method"] != http.MethodPost || entries[0]["route"] != recognizePath || entries[0]["status"] != float64(http.StatusOK) || entries[0]["error_code"] != "ok" || entries[0]["level"] != "INFO" || entries[0]["response_bytes"].(float64) == 0 || entries[0]["duration_ms"] == nil || entries[0]["request_id"] != "94B86c5c-2520-4c0f-b91e-6d4a948fc88a" {
		t.Fatalf("success log = %#v", entries)
	}
}

func TestTimedOutRequestLogsProcessingTimeout(t *testing.T) {
	logger, path, closeSink := testLogSink(t)
	defer closeSink()
	config := testConfig()
	config.Logger = logger
	handler := newTestHandler(t, config, &fakeRecognizer{err: context.DeadlineExceeded})
	recorder := serveUpload(t, handler, uploadSpec{fileName: "upload.png", mediaType: "image/png", data: syntheticPNG()})
	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", recorder.Code)
	}
	if err := closeSink(); err != nil {
		t.Fatal(err)
	}
	entries := decodeLogLines(t, mustReadHTTPLog(t, path))
	if len(entries) != 1 || entries[0]["status"] != float64(http.StatusGatewayTimeout) || entries[0]["error_code"] != "processing_timeout" || entries[0]["level"] != "ERROR" {
		t.Fatalf("timeout log = %#v", entries)
	}
}

func TestRequestLogNormalizesUnmatchedPathAndAccessDenial(t *testing.T) {
	logger, path, closeSink := testLogSink(t)
	defer closeSink()
	config := testConfig()
	config.Logger = logger
	config.AllowedClientNetworks = []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")}
	handler := newTestHandler(t, config, &fakeRecognizer{})
	request := httptest.NewRequest(http.MethodGet, "http://example.invalid/private/path?token=SECRET", nil)
	request.RemoteAddr = "203.0.113.9:1234"
	request.Header.Set("X-Request-ID", "94b86c5c-2520-4c0f-b91e-6d4a948fc88a")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	deniedRequest := httptest.NewRequest(http.MethodPost, "http://example.invalid/v1/passport/recognize", nil)
	deniedRequest.Header.Set("Content-Type", "multipart/form-data; boundary=unread")
	deniedRequest.Header.Set("X-Request-ID", "94b86c5c-2520-4c0f-b91e-6d4a948fc88a")
	deniedRequest.RemoteAddr = "203.0.113.9:1234"
	deniedRecorder := httptest.NewRecorder()
	handler.ServeHTTP(deniedRecorder, deniedRequest)
	if deniedRecorder.Code != http.StatusForbidden {
		t.Fatalf("denied status = %d, want 403", deniedRecorder.Code)
	}
	if err := closeSink(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private/path") || strings.Contains(string(data), "SECRET") || strings.Contains(string(data), "203.0.113.9") {
		t.Fatalf("untrusted path/query/client address leaked: %s", data)
	}
	entries := decodeLogLines(t, data)
	if len(entries) != 2 || entries[0]["route"] != "unmatched" || entries[0]["status"] != float64(http.StatusNotFound) || entries[0]["error_code"] != "not_found" || entries[0]["level"] != "WARN" || entries[1]["route"] != recognizePath || entries[1]["status"] != float64(http.StatusForbidden) || entries[1]["error_code"] != "access_denied" || entries[1]["level"] != "WARN" {
		t.Fatalf("unexpected access denial log: %#v", entries)
	}
}

func TestStatusRequestLogUsesFixedRouteTemplate(t *testing.T) {
	logger, path, closeSink := testLogSink(t)
	defer closeSink()
	const jobID = "9cfe2e2f-135d-42f4-8db5-ddabc5a2f831"
	config := testConfig()
	config.Logger = logger
	config.AsyncJobs = &AsyncJobsConfig{
		Authenticator: &asyncAuthStub{principal: clientauth.Principal{OwnerID: "owner"}},
		Acceptor:      &asyncAcceptorStub{}, MaxConcurrentUploads: 1,
		StatusReader: &asyncStatusReaderStub{err: jobs.ErrNotFound}, StatusReadTimeout: time.Second,
	}
	handler := newTestHandler(t, config, &fakeRecognizer{})
	request := httptest.NewRequest(http.MethodGet, jobsPath+"/"+jobID, nil)
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Authorization", "Bearer pbt1_test")
	request.Header.Set("X-Request-ID", "94b86c5c-2520-4c0f-b91e-6d4a948fc88a")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if err := closeSink(); err != nil {
		t.Fatal(err)
	}
	data := mustReadHTTPLog(t, path)
	if bytes.Contains(data, []byte(jobID)) {
		t.Fatalf("job ID leaked to logs: %s", data)
	}
	entries := decodeLogLines(t, data)
	if len(entries) != 1 || entries[0]["route"] != jobsPath+"/{job_id}" || entries[0]["status"] != float64(http.StatusNotFound) || entries[0]["error_code"] != "not_found" {
		t.Fatalf("unexpected status request log: %#v", entries)
	}
}

func TestRequestPanicLogsOnlySafeCategoryAndRethrowsAbortHandler(t *testing.T) {
	logger, path, closeSink := testLogSink(t)
	defer closeSink()
	const sentinel = "PRIVATE-PASSPORT-PANIC-SENTINEL"
	config := testConfig()
	config.Logger = logger
	handler := newTestHandler(t, config, &fakeRecognizer{recognize: func(context.Context, Image) (passport.Extraction, error) {
		panic(sentinel)
	}})
	recorder := httptest.NewRecorder()
	body, contentType := syntheticUploadRequest(t)
	request := httptest.NewRequest(http.MethodPost, handlerPath, body)
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("X-Request-ID", "94b86c5c-2520-4c0f-b91e-6d4a948fc88a")
	func() {
		defer func() {
			if recovered := recover(); recovered != http.ErrAbortHandler {
				t.Fatalf("panic = %#v, want http.ErrAbortHandler", recovered)
			}
		}()
		handler.ServeHTTP(recorder, request)
	}()
	if err := closeSink(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), sentinel) {
		t.Fatalf("panic value leaked to logs: %s", data)
	}
	entries := decodeLogLines(t, data)
	if recorder.Body.Len() != 0 {
		t.Fatalf("panic wrote %d response bytes, want zero", recorder.Body.Len())
	}
	if len(entries) != 2 || entries[0]["event"] != "http.request.panic" || entries[0]["error_code"] != "internal_panic" || entries[1]["event"] != "http.request.completed" || entries[1]["error_code"] != "internal_panic" || entries[1]["status"] != float64(0) || entries[1]["response_bytes"] != float64(0) {
		t.Fatalf("unexpected panic log sequence: %#v", entries)
	}
}

func TestExplicitAbortLogsOneTerminalEventAndRethrows(t *testing.T) {
	logger, path, closeSink := testLogSink(t)
	defer closeSink()
	config := testConfig()
	config.Logger = logger
	handler := newTestHandler(t, config, &fakeRecognizer{recognize: func(context.Context, Image) (passport.Extraction, error) {
		panic(http.ErrAbortHandler)
	}})
	body, contentType := syntheticUploadRequest(t)
	request := httptest.NewRequest(http.MethodPost, handlerPath, body)
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("X-Request-ID", "94b86c5c-2520-4c0f-b91e-6d4a948fc88a")
	func() {
		defer func() {
			if recovered := recover(); recovered != http.ErrAbortHandler {
				t.Fatalf("panic = %#v, want http.ErrAbortHandler", recovered)
			}
		}()
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}()
	if err := closeSink(); err != nil {
		t.Fatal(err)
	}
	entries := decodeLogLines(t, mustReadHTTPLog(t, path))
	if len(entries) != 1 || entries[0]["event"] != "http.request.completed" || entries[0]["error_code"] != "request_aborted" || entries[0]["status"] != float64(0) || entries[0]["response_bytes"] != float64(0) || entries[0]["level"] != "WARN" {
		t.Fatalf("explicit abort log = %#v", entries)
	}
}

func TestCancelledRequestIsLoggedWithoutInventingHTTPStatus(t *testing.T) {
	logger, path, closeSink := testLogSink(t)
	defer closeSink()
	config := testConfig()
	config.Logger = logger
	handler := newTestHandler(t, config, &fakeRecognizer{})
	body, contentType := syntheticUploadRequest(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodPost, handlerPath, body).WithContext(ctx)
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("X-Request-ID", "94b86c5c-2520-4c0f-b91e-6d4a948fc88a")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if err := closeSink(); err != nil {
		t.Fatal(err)
	}
	entries := decodeLogLines(t, mustReadHTTPLog(t, path))
	if len(entries) != 1 || entries[0]["status"] != float64(0) || entries[0]["error_code"] != "cancelled" || entries[0]["event"] != "http.request.completed" {
		t.Fatalf("cancelled request log = %#v", entries)
	}
}

func testLogSink(t *testing.T) (*slog.Logger, string, func() error) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	privateDir := filepath.Join(root, "private")
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(privateDir, "api.jsonl")
	sink, err := telemetry.Open(telemetry.Config{Path: path, Level: "debug", MaxSizeBytes: 64 << 10, MaxBackups: 1})
	if err != nil {
		t.Fatal(err)
	}
	var closed bool
	closeSink := func() error {
		if closed {
			return nil
		}
		closed = true
		return sink.Close()
	}
	return sink.Logger(), path, closeSink
}

func decodeLogLines(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	entries := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("invalid JSON log record: %v", err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func mustReadHTTPLog(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func syntheticUploadRequest(t *testing.T) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="file"; filename="passport.png"`},
		"Content-Type":        {"image/png"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(syntheticPNG()); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return &body, writer.FormDataContentType()
}
