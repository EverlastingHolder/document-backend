package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/textproto"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"passport-backend/internal/passport"
)

const (
	handlerPath     = "/v1/passport/recognize"
	testPipeline    = "test-pipeline-v1"
	testRequestID   = "94b86c5c-2520-4c0f-b91e-6d4a948fc88a"
	maxTestUpload   = int64(1024)
	privateSentinel = "PRIVATE-PII-SENTINEL"
)

type fakeRecognizer struct {
	extraction passport.Extraction
	err        error
	callCount  atomic.Int32
	recognize  func(context.Context, Image) (passport.Extraction, error)
}

func (f *fakeRecognizer) Recognize(ctx context.Context, image Image) (passport.Extraction, error) {
	f.callCount.Add(1)
	if f.recognize != nil {
		return f.recognize(ctx, image)
	}
	return f.extraction, f.err
}

func testConfig() Config {
	return Config{
		MaxUploadBytes:  maxTestUpload,
		OCRTimeout:      time.Second,
		MaxConcurrent:   1,
		PipelineVersion: testPipeline,
	}
}

func newTestHandler(t *testing.T, config Config, recognizer Recognizer) http.Handler {
	t.Helper()
	handler, err := NewHandler(config, recognizer)
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	return handler
}

func TestNewHandlerRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name   string
		config Config
		useNil bool
	}{
		{name: "zero upload limit", config: Config{OCRTimeout: time.Second, MaxConcurrent: 1, PipelineVersion: testPipeline}},
		{name: "negative upload limit", config: Config{MaxUploadBytes: -1, OCRTimeout: time.Second, MaxConcurrent: 1, PipelineVersion: testPipeline}},
		{name: "upload limit overflows multipart overhead", config: Config{MaxUploadBytes: int64(^uint64(0) >> 1), OCRTimeout: time.Second, MaxConcurrent: 1, PipelineVersion: testPipeline}},
		{name: "zero OCR timeout", config: Config{MaxUploadBytes: 1, MaxConcurrent: 1, PipelineVersion: testPipeline}},
		{name: "negative OCR timeout", config: Config{MaxUploadBytes: 1, OCRTimeout: -time.Second, MaxConcurrent: 1, PipelineVersion: testPipeline}},
		{name: "zero concurrency", config: Config{MaxUploadBytes: 1, OCRTimeout: time.Second, PipelineVersion: testPipeline}},
		{name: "negative concurrency", config: Config{MaxUploadBytes: 1, OCRTimeout: time.Second, MaxConcurrent: -1, PipelineVersion: testPipeline}},
		{name: "blank pipeline version", config: Config{MaxUploadBytes: 1, OCRTimeout: time.Second, MaxConcurrent: 1, PipelineVersion: " \t "}},
		{name: "nil recognizer", config: testConfig(), useNil: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var recognizer Recognizer = &fakeRecognizer{extraction: validTestExtraction()}
			if test.useNil {
				recognizer = nil
			}
			if _, err := NewHandler(test.config, recognizer); err == nil {
				t.Fatal("NewHandler() error = nil, want invalid configuration error")
			}
		})
	}
}

func TestRecognizeReturnsMappedPublicResponseAndEchoesRequestID(t *testing.T) {
	recognizer := &fakeRecognizer{extraction: validTestExtraction()}
	handler := newTestHandler(t, testConfig(), recognizer)
	recorder := serveUpload(t, handler, uploadSpec{fileName: "client-path-private-sentinel.png", mediaType: "image/png", data: syntheticPNG()})
	// The helper deliberately does not set an ID; this case checks generation separately.
	assertJSONResponse(t, recorder)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	response := decodeJSON(t, recorder)
	if recorder.Header().Get("X-Request-ID") != response["request_id"] {
		t.Fatalf("header request ID %q does not match body %q", recorder.Header().Get("X-Request-ID"), response["request_id"])
	}
	if !isUUID(response["request_id"]) {
		t.Fatalf("generated request_id = %#v, want UUID", response["request_id"])
	}
	if response["status"] != "completed" || response["review_required"] != true || response["pipeline_version"] != testPipeline {
		t.Fatalf("unexpected public response envelope: %#v", response)
	}
	fields, ok := response["fields"].(map[string]any)
	if !ok || len(fields) != 11 {
		t.Fatalf("public fields = %#v, want all 11 fields", response["fields"])
	}
	warnings, ok := response["warnings"].([]any)
	if !ok || warnings == nil || len(warnings) != 0 {
		t.Fatalf("warnings = %#v, want empty JSON array", response["warnings"])
	}
	if strings.Contains(recorder.Body.String(), "client-path-private-sentinel") {
		t.Fatal("response reflected untrusted upload filename")
	}
}

func TestRecognizeEchoesValidRequestIDInBodyAndHeader(t *testing.T) {
	handler := newTestHandler(t, testConfig(), &fakeRecognizer{extraction: validTestExtraction()})
	recorder := serveUploadWithHeaders(t, handler, uploadSpec{fileName: "upload.png", mediaType: "image/png", data: syntheticPNG()}, http.Header{"X-Request-Id": []string{testRequestID}})
	assertJSONResponse(t, recorder)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	response := decodeJSON(t, recorder)
	if response["request_id"] != testRequestID || recorder.Header().Get("X-Request-ID") != testRequestID {
		t.Fatalf("request ID was not echoed consistently: body=%#v header=%q", response["request_id"], recorder.Header().Get("X-Request-ID"))
	}
}

func TestRecognizeReturns200AndWarningWhenEveryPublicValueIsNull(t *testing.T) {
	recognizer := &fakeRecognizer{extraction: allNullTestExtraction()}
	handler := newTestHandler(t, testConfig(), recognizer)
	recorder := serveUpload(t, handler, uploadSpec{fileName: "upload.png", mediaType: "image/png", data: syntheticPNG()})
	assertJSONResponse(t, recorder)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for completed all-null extraction: %s", recorder.Code, recorder.Body.String())
	}
	response := decodeJSON(t, recorder)
	warnings, ok := response["warnings"].([]any)
	if !ok || len(warnings) != 1 || warnings[0].(map[string]any)["code"] != "no_fields_recognized" {
		t.Fatalf("warnings = %#v, want no_fields_recognized", response["warnings"])
	}
}

func TestRecognizeAcceptsSupportedDeclaredImageMediaTypes(t *testing.T) {
	handler := newTestHandler(t, testConfig(), &fakeRecognizer{extraction: validTestExtraction()})
	for _, mediaType := range []string{"image/jpeg", "image/png", "image/webp", "IMAGE/PNG; charset=binary"} {
		t.Run(mediaType, func(t *testing.T) {
			recorder := serveUpload(t, handler, uploadSpec{fileName: "synthetic-image", mediaType: mediaType, data: syntheticPNG()})
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 for declared media type %q: %s", recorder.Code, mediaType, recorder.Body.String())
			}
		})
	}
}

func TestRecognizePassesFileContentToRecognizer(t *testing.T) {
	fileContent := []byte{0x89, 'P', 'N', 'G', '\r', '\n', '=', '4', '1', '=', '\r', '\n', 0xff}
	tests := []struct {
		name                    string
		contentTransferEncoding string
	}{
		{name: "binary without transfer encoding"},
		{name: "quoted-printable transfer encoding", contentTransferEncoding: "quoted-printable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var captured Image
			recognizer := &fakeRecognizer{
				extraction: validTestExtraction(),
				recognize: func(_ context.Context, image Image) (passport.Extraction, error) {
					captured = image
					return validTestExtraction(), nil
				},
			}
			handler := newTestHandler(t, testConfig(), recognizer)
			recorder := serveUpload(t, handler, uploadSpec{
				fileName: "upload.png", mediaType: "image/png", data: fileContent,
				contentTransferEncoding: test.contentTransferEncoding,
			})
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
			}
			if captured.MediaType != "image/png" {
				t.Errorf("recognizer media type = %q, want image/png", captured.MediaType)
			}
			if !bytes.Equal(captured.Data, fileContent) {
				t.Errorf("recognizer bytes = % x, want file content % x", captured.Data, fileContent)
			}
		})
	}
}

func TestNewHandlerLANEmptyAllowlistKeepsBareHandlerUnrestricted(t *testing.T) {
	config := testConfig()
	recognizer := &fakeRecognizer{extraction: validTestExtraction()}
	handler := newTestHandler(t, config, recognizer)
	recorder := serveUploadFromRemoteAddr(t, handler,
		uploadSpec{fileName: "upload.png", mediaType: "image/png", data: syntheticPNG()},
		"8.8.8.8:49152", nil,
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for legacy bare handler without an allowlist: %s", recorder.Code, recorder.Body.String())
	}
	if recognizer.callCount.Load() != 1 {
		t.Fatalf("recognizer calls = %d, want 1", recognizer.callCount.Load())
	}
}

func TestRecognizeLANUsesRemoteAddrAndIgnoresForwardedHeaders(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		forwarded  string
		allowed    bool
	}{
		{name: "private IPv4 in configured prefix", remoteAddr: "192.168.10.25:49152", forwarded: "8.8.8.8", allowed: true},
		{name: "IPv4 mapped address is unmapped", remoteAddr: "[::ffff:192.168.10.25]:49152", allowed: true},
		{name: "IPv4 mapped address with zone is denied", remoteAddr: "[::ffff:192.168.10.25%en0]:49152", allowed: false},
		{name: "IPv4 mapped link-local address with zone is denied", remoteAddr: "[::ffff:169.254.1.2%en0]:49152", allowed: false},
		{name: "zoned IPv6 link local address", remoteAddr: "[fe80::12%en0]:49152", allowed: true},
		{name: "zone on non-link-local IPv6 is denied", remoteAddr: "[fc00::12%en0]:49152", allowed: false},
		{name: "public source is denied despite private forwarded address", remoteAddr: "8.8.8.8:49152", forwarded: "192.168.10.25", allowed: false},
		{name: "missing port is denied", remoteAddr: "192.168.10.25", allowed: false},
		{name: "signed port is denied", remoteAddr: "192.168.10.25:+49152", allowed: false},
		{name: "malformed IP is denied", remoteAddr: "999.1.1.1:49152", allowed: false},
		{name: "hostname source is denied", remoteAddr: "scanner.local:49152", allowed: false},
		{name: "unconfigured private prefix is denied", remoteAddr: "192.168.11.25:49152", allowed: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig()
			config.AllowedClientNetworks = []netip.Prefix{
				netip.MustParsePrefix("192.168.10.0/24"),
				netip.MustParsePrefix("169.254.0.0/16"),
				netip.MustParsePrefix("fe80::/10"),
				netip.MustParsePrefix("fc00::/7"),
			}
			recognizer := &fakeRecognizer{extraction: validTestExtraction()}
			handler := newTestHandler(t, config, recognizer)
			headers := make(http.Header)
			if test.forwarded != "" {
				headers.Set("X-Forwarded-For", test.forwarded)
				headers.Set("X-Real-IP", test.forwarded)
			}
			recorder := serveUploadFromRemoteAddr(t, handler,
				uploadSpec{fileName: "upload.png", mediaType: "image/png", data: syntheticPNG()},
				test.remoteAddr, headers,
			)
			if test.allowed {
				if recorder.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
				}
				if recognizer.callCount.Load() != 1 {
					t.Fatalf("recognizer calls = %d, want 1", recognizer.callCount.Load())
				}
				return
			}
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403: %s", recorder.Code, recorder.Body.String())
			}
			response := assertErrorResponse(t, recorder)
			if response["code"] != "access_denied" {
				t.Errorf("error code = %#v, want access_denied", response["code"])
			}
			if recognizer.callCount.Load() != 0 {
				t.Fatalf("recognizer calls = %d, want 0", recognizer.callCount.Load())
			}
		})
	}
}

func TestRecognizeLANCopiesAllowedClientNetworks(t *testing.T) {
	config := testConfig()
	callerNetworks := []netip.Prefix{netip.MustParsePrefix("192.168.1.19/24")}
	config.AllowedClientNetworks = callerNetworks
	recognizer := &fakeRecognizer{extraction: validTestExtraction()}
	handler := newTestHandler(t, config, recognizer)
	callerNetworks[0] = netip.MustParsePrefix("10.0.0.0/8")

	allowed := serveUploadFromRemoteAddr(t, handler,
		uploadSpec{fileName: "allowed.png", mediaType: "image/png", data: syntheticPNG()},
		"192.168.1.20:49152", nil,
	)
	if allowed.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 from copied 192.168.1.0/24 prefix: %s", allowed.Code, allowed.Body.String())
	}
	denied := serveUploadFromRemoteAddr(t, handler,
		uploadSpec{fileName: "denied.png", mediaType: "image/png", data: syntheticPNG()},
		"10.1.1.1:49152", nil,
	)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 after caller mutates its config slice: %s", denied.Code, denied.Body.String())
	}
	if got := recognizer.callCount.Load(); got != 1 {
		t.Fatalf("recognizer calls = %d, want only the request from copied prefix", got)
	}
}

func TestRecognizeLANRejectsBeforeSemaphoreAndBodyRead(t *testing.T) {
	config := testConfig()
	config.AllowedClientNetworks = []netip.Prefix{netip.MustParsePrefix("192.168.10.0/24")}
	started := make(chan struct{})
	release := make(chan struct{})
	recognizer := &fakeRecognizer{recognize: func(context.Context, Image) (passport.Extraction, error) {
		close(started)
		<-release
		return validTestExtraction(), nil
	}}
	handler := newTestHandler(t, config, recognizer)
	firstBody, firstContentType := multipartBody(t, []uploadSpec{{fileName: "allowed.png", mediaType: "image/png", data: syntheticPNG()}})
	firstRecorder := httptest.NewRecorder()
	firstRequest := httptest.NewRequest(http.MethodPost, handlerPath, bytes.NewReader(firstBody))
	firstRequest.Header.Set("Content-Type", firstContentType)
	firstRequest.RemoteAddr = "192.168.10.25:49152"
	firstDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(firstRecorder, firstRequest)
		close(firstDone)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("allowed request did not reach recognizer")
	}
	defer func() {
		close(release)
		select {
		case <-firstDone:
		case <-time.After(time.Second):
			t.Error("allowed request did not finish after recognizer release")
		}
	}()

	body, contentType := multipartBody(t, []uploadSpec{{fileName: "denied.png", mediaType: "image/png", data: syntheticPNG()}})
	counted := &countingReader{reader: bytes.NewReader(body)}
	deniedRecorder := httptest.NewRecorder()
	deniedRequest := httptest.NewRequest(http.MethodPost, handlerPath, io.NopCloser(counted))
	deniedRequest.ContentLength = -1
	deniedRequest.Header.Set("Content-Type", contentType)
	deniedRequest.Header.Set("X-Request-ID", testRequestID)
	deniedRequest.RemoteAddr = "8.8.8.8:49152"
	handler.ServeHTTP(deniedRecorder, deniedRequest)
	if deniedRecorder.Code != http.StatusForbidden {
		t.Fatalf("denied request status = %d, want 403 even while concurrency slot is occupied: %s", deniedRecorder.Code, deniedRecorder.Body.String())
	}
	response := assertErrorResponse(t, deniedRecorder)
	if response["code"] != "access_denied" {
		t.Errorf("error code = %#v, want access_denied", response["code"])
	}
	if response["request_id"] != testRequestID || deniedRecorder.Header().Get("X-Request-ID") != testRequestID {
		t.Errorf("request ID body=%#v header=%q, want %q", response["request_id"], deniedRecorder.Header().Get("X-Request-ID"), testRequestID)
	}
	if counted.bytesRead != 0 {
		t.Errorf("disallowed request read %d upload bytes, want 0", counted.bytesRead)
	}
	if recognizer.callCount.Load() != 1 {
		t.Errorf("recognizer calls = %d while allowed request remains blocked, want 1", recognizer.callCount.Load())
	}
}

func TestRecognizeValidatesRouteAndMethod(t *testing.T) {
	handler := newTestHandler(t, testConfig(), &fakeRecognizer{extraction: validTestExtraction()})
	for _, test := range []struct {
		name, method, path string
		wantStatus         int
		wantAllow          string
	}{
		{"wrong path", http.MethodPost, "/v1/other", http.StatusNotFound, ""},
		{"wrong method", http.MethodGet, handlerPath, http.StatusMethodNotAllowed, http.MethodPost},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(test.method, test.path, nil)
			handler.ServeHTTP(recorder, request)
			assertJSONResponse(t, recorder)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, test.wantStatus, recorder.Body.String())
			}
			if test.wantAllow != "" && recorder.Header().Get("Allow") != test.wantAllow {
				t.Errorf("Allow = %q, want %q", recorder.Header().Get("Allow"), test.wantAllow)
			}
		})
	}
}

func TestRecognizeRejectsMalformedOrAmbiguousRequestIDWithoutReflectingIt(t *testing.T) {
	handler := newTestHandler(t, testConfig(), &fakeRecognizer{extraction: validTestExtraction()})
	for _, test := range []struct {
		name   string
		values []string
	}{
		{"malformed", []string{privateSentinel}},
		{"ambiguous", []string{testRequestID, "00000000-0000-0000-0000-000000000000"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			header := make(http.Header)
			for _, value := range test.values {
				header.Add("X-Request-ID", value)
			}
			recorder := serveUploadWithHeaders(t, handler, uploadSpec{fileName: "upload.png", mediaType: "image/png", data: syntheticPNG()}, header)
			assertJSONResponse(t, recorder)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", recorder.Code, recorder.Body.String())
			}
			response := decodeJSON(t, recorder)
			if !isUUID(response["request_id"]) || recorder.Header().Get("X-Request-ID") != response["request_id"] {
				t.Fatalf("invalid request ID response metadata: body=%#v header=%q", response["request_id"], recorder.Header().Get("X-Request-ID"))
			}
			if strings.Contains(recorder.Body.String(), privateSentinel) || slicesContains(test.values, recorder.Header().Get("X-Request-ID")) {
				t.Fatal("response reflected untrusted request ID")
			}
		})
	}
}

func TestRecognizeReturnsJSONErrorForMalformedMultipartAndMissingEmptyFiles(t *testing.T) {
	handler := newTestHandler(t, testConfig(), &fakeRecognizer{extraction: validTestExtraction()})
	missingFileBody, missingFileType := multipartBody(t, []uploadSpec{{fieldName: "other", fileName: "x", mediaType: "image/png", data: syntheticPNG()}})
	emptyFileBody, emptyFileType := multipartBody(t, []uploadSpec{{fieldName: "file", fileName: "x", mediaType: "image/png", data: nil}})
	truncatedMultipart := []byte("--broken\r\nContent-Disposition: form-data; name=\"file\"; filename=\"x.png\"\r\nContent-Type: image/png\r\n\r\ntruncated")
	tests := []struct {
		name        string
		contentType string
		body        []byte
	}{
		{"not multipart", "application/json", []byte(`{}`)},
		{"missing boundary", "multipart/form-data", []byte("anything")},
		{"truncated multipart", "multipart/form-data; boundary=broken", truncatedMultipart},
		{"missing file", missingFileType, missingFileBody},
		{"empty file", emptyFileType, emptyFileBody},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := serveRaw(t, handler, test.body, test.contentType)
			assertJSONResponse(t, recorder)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", recorder.Code, recorder.Body.String())
			}
			assertErrorResponse(t, recorder)
		})
	}
}

func TestRecognizeRejectsDuplicateFileAndUnexpectedFormParts(t *testing.T) {
	handler := newTestHandler(t, testConfig(), &fakeRecognizer{extraction: validTestExtraction()})
	tests := []struct {
		name  string
		parts []uploadSpec
	}{
		{"duplicate file", []uploadSpec{
			{fieldName: "file", fileName: "one.png", mediaType: "image/png", data: syntheticPNG()},
			{fieldName: "file", fileName: "two.png", mediaType: "image/png", data: syntheticPNG()},
		}},
		{"unexpected form field", []uploadSpec{
			{fieldName: "file", fileName: "one.png", mediaType: "image/png", data: syntheticPNG()},
			{fieldName: "token", fileName: "token.txt", mediaType: "text/plain", data: []byte("secret")},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body, contentType := multipartBody(t, test.parts)
			recorder := serveRaw(t, handler, body, contentType)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestRecognizeRejectsUnexpectedPartBeforeReadingItsBody(t *testing.T) {
	prefix := []byte("--upload\r\nContent-Disposition: form-data; name=x\r\n\r\n")
	body := &failOnSecondReadBody{prefix: prefix}
	recognizer := &fakeRecognizer{extraction: validTestExtraction()}
	handler := newTestHandler(t, testConfig(), recognizer)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, handlerPath, nil)
	request.Header.Set("Content-Type", "multipart/form-data; boundary=upload")
	request.ContentLength = -1
	request.Body = body
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for unexpected form part: %s", recorder.Code, recorder.Body.String())
	}
	if body.readCalls != 1 {
		t.Fatalf("request body read calls = %d, want one header read before rejecting unknown part", body.readCalls)
	}
	if got := recognizer.callCount.Load(); got != 0 {
		t.Fatalf("recognizer calls = %d, want 0 for unexpected form part", got)
	}
}

func TestRecognizeEnforcesBodyLimitForChunkedMultipartEpilogue(t *testing.T) {
	config := testConfig()
	config.MaxUploadBytes = 1
	recognizer := &fakeRecognizer{extraction: validTestExtraction()}
	handler := newTestHandler(t, config, recognizer)
	body, contentType := multipartBody(t, []uploadSpec{{fieldName: "file", fileName: "upload.png", mediaType: "image/png", data: []byte{1}}})
	body = append(body, bytes.Repeat([]byte("e"), 80<<10)...)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, handlerPath, bytes.NewReader(body))
	request.ContentLength = -1
	request.TransferEncoding = []string{"chunked"}
	request.Header.Set("Content-Type", contentType)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 for oversized chunked multipart body: %s", recorder.Code, recorder.Body.String())
	}
	if got := recognizer.callCount.Load(); got != 0 {
		t.Fatalf("recognizer calls = %d, want 0 when total body exceeds bounded overhead", got)
	}
}

func TestRecognizeRejectsUnsupportedFileMediaType(t *testing.T) {
	handler := newTestHandler(t, testConfig(), &fakeRecognizer{extraction: validTestExtraction()})
	for _, test := range []struct {
		name      string
		mediaType string
	}{
		{"unsupported", "application/octet-stream"},
		{"missing", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := serveUpload(t, handler, uploadSpec{fileName: "upload.bin", mediaType: test.mediaType, data: syntheticPNG()})
			assertJSONResponse(t, recorder)
			if recorder.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("status = %d, want 415: %s", recorder.Code, recorder.Body.String())
			}
			assertErrorResponse(t, recorder)
		})
	}
}

func TestRecognizeEnforcesFileSizeLimit(t *testing.T) {
	config := testConfig()
	config.MaxUploadBytes = 16
	recognizer := &fakeRecognizer{extraction: validTestExtraction()}
	handler := newTestHandler(t, config, recognizer)
	for _, size := range []int{16, 17} {
		t.Run(fmt.Sprintf("%d bytes", size), func(t *testing.T) {
			recorder := serveUpload(t, handler, uploadSpec{fileName: "synthetic.bin", mediaType: "image/png", data: bytes.Repeat([]byte("x"), size)})
			wantStatus := http.StatusOK
			if size > int(config.MaxUploadBytes) {
				wantStatus = http.StatusRequestEntityTooLarge
			}
			if recorder.Code != wantStatus {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, wantStatus, recorder.Body.String())
			}
		})
	}
	if got := recognizer.callCount.Load(); got != 1 {
		t.Fatalf("recognizer calls = %d, want only exact-limit upload", got)
	}
}

func TestRecognizeBoundsBodyReadsWhenFileExceedsLimit(t *testing.T) {
	config := testConfig()
	config.MaxUploadBytes = 32
	recognizer := &fakeRecognizer{extraction: validTestExtraction()}
	handler := newTestHandler(t, config, recognizer)
	body, contentType := multipartBody(t, []uploadSpec{{fieldName: "file", fileName: "upload.png", mediaType: "image/png", data: bytes.Repeat([]byte("x"), 4<<20)}})
	counted := &countingReader{reader: bytes.NewReader(body)}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, handlerPath, io.NopCloser(counted))
	request.ContentLength = -1
	request.Header.Set("Content-Type", contentType)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", recorder.Code, recorder.Body.String())
	}
	if counted.bytesRead > int(config.MaxUploadBytes)+(64<<10) {
		t.Fatalf("read %d body bytes for %d-byte file limit; expected bounded multipart overhead", counted.bytesRead, config.MaxUploadBytes)
	}
	if got := recognizer.callCount.Load(); got != 0 {
		t.Fatalf("recognizer calls = %d, want 0 for oversized upload", got)
	}
}

func TestRecognizeDoesNotReflectUploadOrRuntimeDiagnostics(t *testing.T) {
	recognizer := &fakeRecognizer{err: errors.New(privateSentinel + " raw runtime diagnostic 001234")}
	handler := newTestHandler(t, testConfig(), recognizer)
	recorder := serveUpload(t, handler, uploadSpec{fileName: privateSentinel + ".png", mediaType: "image/png", data: []byte(privateSentinel)})
	assertJSONResponse(t, recorder)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for unknown runtime error: %s", recorder.Code, recorder.Body.String())
	}
	assertErrorResponse(t, recorder)
	for _, sentinel := range []string{privateSentinel, "raw runtime diagnostic", "001234", ".png"} {
		if strings.Contains(recorder.Body.String(), sentinel) {
			t.Errorf("error response leaked %q: %s", sentinel, recorder.Body.String())
		}
	}
}

func TestRecognizeMapsTypedRuntimeErrorsToPublicStatuses(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"unprocessable image", ErrImageUnprocessable, http.StatusUnprocessableEntity, "image_unprocessable"},
		{"runtime unavailable", ErrUnavailable, http.StatusServiceUnavailable, "ocr_unavailable"},
		{"unexpected error", errors.New(privateSentinel), http.StatusInternalServerError, "processing_failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := newTestHandler(t, testConfig(), &fakeRecognizer{err: test.err})
			recorder := serveUpload(t, handler, uploadSpec{fileName: "upload.png", mediaType: "image/png", data: syntheticPNG()})
			assertJSONResponse(t, recorder)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, test.wantStatus, recorder.Body.String())
			}
			response := assertErrorResponse(t, recorder)
			if response["code"] != test.wantCode {
				t.Errorf("error code = %#v, want %q", response["code"], test.wantCode)
			}
			if strings.Contains(recorder.Body.String(), privateSentinel) {
				t.Fatal("error response leaked recognizer diagnostic")
			}
		})
	}
}

func TestRecognizeMapsMalformedInternalExtractionToSafeServerError(t *testing.T) {
	extraction := validTestExtraction()
	extraction.Fields["surname"] = passport.FieldResult{Value: stringPointer(privateSentinel), State: "invalid_internal_state"}
	handler := newTestHandler(t, testConfig(), &fakeRecognizer{extraction: extraction})
	recorder := serveUpload(t, handler, uploadSpec{fileName: "upload.png", mediaType: "image/png", data: syntheticPNG()})
	assertJSONResponse(t, recorder)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for malformed extraction: %s", recorder.Code, recorder.Body.String())
	}
	assertErrorResponse(t, recorder)
	if strings.Contains(recorder.Body.String(), privateSentinel) || strings.Contains(recorder.Body.String(), "invalid extraction field state") {
		t.Fatalf("malformed mapper error was exposed: %s", recorder.Body.String())
	}
}

func TestRecognizeMapsOCRDeadlineToGatewayTimeout(t *testing.T) {
	config := testConfig()
	config.OCRTimeout = 30 * time.Millisecond
	recognizer := &fakeRecognizer{recognize: func(ctx context.Context, _ Image) (passport.Extraction, error) {
		<-ctx.Done()
		return passport.Extraction{}, ctx.Err()
	}}
	handler := newTestHandler(t, config, recognizer)
	recorder := serveUpload(t, handler, uploadSpec{fileName: "upload.png", mediaType: "image/png", data: syntheticPNG()})
	assertJSONResponse(t, recorder)
	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504: %s", recorder.Code, recorder.Body.String())
	}
	response := assertErrorResponse(t, recorder)
	if response["code"] != "processing_timeout" {
		t.Errorf("error code = %#v, want processing_timeout", response["code"])
	}
}

func TestRecognizeReturns504WhenRecognizerReturnsResultAfterDeadline(t *testing.T) {
	config := testConfig()
	config.OCRTimeout = 30 * time.Millisecond
	recognizer := &fakeRecognizer{recognize: func(ctx context.Context, _ Image) (passport.Extraction, error) {
		<-ctx.Done()
		return validTestExtraction(), nil
	}}
	handler := newTestHandler(t, config, recognizer)
	recorder := serveUpload(t, handler, uploadSpec{fileName: "upload.png", mediaType: "image/png", data: syntheticPNG()})
	assertJSONResponse(t, recorder)
	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 when recognition completes after deadline: %s", recorder.Code, recorder.Body.String())
	}
	response := assertErrorResponse(t, recorder)
	if response["code"] != "processing_timeout" {
		t.Errorf("error code = %#v, want processing_timeout", response["code"])
	}
}

func TestRecognizePreCancelledRequestSkipsRecognizerAndDoesNotWriteSuccess(t *testing.T) {
	recognizer := &fakeRecognizer{extraction: validTestExtraction()}
	handler := newTestHandler(t, testConfig(), recognizer)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	body, contentType := multipartBody(t, []uploadSpec{{fieldName: "file", fileName: "upload.png", mediaType: "image/png", data: syntheticPNG()}})
	recorder := httptest.NewRecorder()
	tracked := &trackingResponseWriter{ResponseWriter: recorder}
	request := httptest.NewRequest(http.MethodPost, handlerPath, bytes.NewReader(body)).WithContext(ctx)
	request.Header.Set("Content-Type", contentType)
	handler.ServeHTTP(tracked, request)
	assertNoResponseWritten(t, tracked, recorder)
	if got := recognizer.callCount.Load(); got != 0 {
		t.Fatalf("recognizer calls = %d, want 0 for pre-cancelled request", got)
	}
}

func TestRecognizeUploadCancellationDuringReadDoesNotWriteResponseOrCallRecognizer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	readStarted := make(chan struct{})
	recognizer := &fakeRecognizer{extraction: validTestExtraction()}
	handler := newTestHandler(t, testConfig(), recognizer)
	recorder := httptest.NewRecorder()
	tracked := &trackingResponseWriter{ResponseWriter: recorder}
	request := httptest.NewRequest(http.MethodPost, handlerPath, nil).WithContext(ctx)
	request.Header.Set("Content-Type", "multipart/form-data; boundary=upload")
	request.ContentLength = -1
	request.Body = &cancellationAwareBody{ctx: ctx, readStarted: readStarted}
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(tracked, request)
		close(done)
	}()
	select {
	case <-readStarted:
	case <-time.After(time.Second):
		t.Fatal("handler did not begin reading upload body")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not return after upload cancellation")
	}
	assertNoResponseWritten(t, tracked, recorder)
	if got := recognizer.callCount.Load(); got != 0 {
		t.Fatalf("recognizer calls = %d, want 0 when upload is cancelled", got)
	}
}

func TestRecognizePropagatesInFlightCancellationToRecognizer(t *testing.T) {
	started := make(chan struct{})
	cancelObserved := make(chan error, 1)
	recognizer := &fakeRecognizer{recognize: func(ctx context.Context, _ Image) (passport.Extraction, error) {
		close(started)
		<-ctx.Done()
		cancelObserved <- ctx.Err()
		return passport.Extraction{}, ctx.Err()
	}}
	handler := newTestHandler(t, testConfig(), recognizer)
	body, contentType := multipartBody(t, []uploadSpec{{fieldName: "file", fileName: "upload.png", mediaType: "image/png", data: syntheticPNG()}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recorder := httptest.NewRecorder()
	tracked := &trackingResponseWriter{ResponseWriter: recorder}
	request := httptest.NewRequest(http.MethodPost, handlerPath, bytes.NewReader(body)).WithContext(ctx)
	request.Header.Set("Content-Type", contentType)
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(tracked, request)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("recognizer was not called")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not return after recognizer observed cancellation")
	}
	select {
	case err := <-cancelObserved:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("recognizer context error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("recognizer did not observe request cancellation")
	}
	if got := recognizer.callCount.Load(); got != 1 {
		t.Fatalf("recognizer calls = %d, want 1", got)
	}
	assertNoResponseWritten(t, tracked, recorder)
}

func TestRecognizeRejectsBusyRequestsBeforeReadingUploadAndReleasesSlotOnError(t *testing.T) {
	config := testConfig()
	config.MaxConcurrent = 1
	started := make(chan struct{})
	release := make(chan struct{})
	var call atomic.Int32
	recognizer := &fakeRecognizer{recognize: func(ctx context.Context, _ Image) (passport.Extraction, error) {
		if call.Add(1) == 1 {
			close(started)
			<-release
			return passport.Extraction{}, ErrUnavailable
		}
		return validTestExtraction(), nil
	}}
	handler := newTestHandler(t, config, recognizer)
	firstBody, firstContentType := multipartBody(t, []uploadSpec{{fieldName: "file", fileName: "first.png", mediaType: "image/png", data: syntheticPNG()}})
	firstRecorder := httptest.NewRecorder()
	firstRequest := httptest.NewRequest(http.MethodPost, handlerPath, bytes.NewReader(firstBody))
	firstRequest.Header.Set("Content-Type", firstContentType)
	firstDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(firstRecorder, firstRequest)
		close(firstDone)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first request did not reach recognizer")
	}

	secondBody, secondContentType := multipartBody(t, []uploadSpec{{fieldName: "file", fileName: "second.png", mediaType: "image/png", data: syntheticPNG()}})
	counted := &countingReader{reader: bytes.NewReader(secondBody)}
	secondRecorder := httptest.NewRecorder()
	secondRequest := httptest.NewRequest(http.MethodPost, handlerPath, io.NopCloser(counted))
	secondRequest.ContentLength = -1
	secondRequest.Header.Set("Content-Type", secondContentType)
	handler.ServeHTTP(secondRecorder, secondRequest)
	if secondRecorder.Code != http.StatusTooManyRequests {
		t.Fatalf("busy request status = %d, want 429: %s", secondRecorder.Code, secondRecorder.Body.String())
	}
	if counted.bytesRead != 0 {
		t.Fatalf("busy request read %d body bytes before returning 429", counted.bytesRead)
	}

	close(release)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first request did not return after recognizer release")
	}
	if firstRecorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("first request status = %d, want 503: %s", firstRecorder.Code, firstRecorder.Body.String())
	}
	thirdRecorder := serveUpload(t, handler, uploadSpec{fileName: "third.png", mediaType: "image/png", data: syntheticPNG()})
	if thirdRecorder.Code != http.StatusOK {
		t.Fatalf("slot was not released after runtime error; next status = %d: %s", thirdRecorder.Code, thirdRecorder.Body.String())
	}
}

type uploadSpec struct {
	fieldName               string
	fileName                string
	mediaType               string
	contentTransferEncoding string
	data                    []byte
}

func serveUpload(t *testing.T, handler http.Handler, spec uploadSpec) *httptest.ResponseRecorder {
	t.Helper()
	body, contentType := multipartBody(t, []uploadSpec{spec})
	return serveRaw(t, handler, body, contentType)
}

func serveUploadWithHeaders(t *testing.T, handler http.Handler, spec uploadSpec, headers http.Header) *httptest.ResponseRecorder {
	t.Helper()
	body, contentType := multipartBody(t, []uploadSpec{spec})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, handlerPath, bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	for key, values := range headers {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	handler.ServeHTTP(recorder, request)
	return recorder
}

func serveRaw(t *testing.T, handler http.Handler, body []byte, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, handlerPath, bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	handler.ServeHTTP(recorder, request)
	return recorder
}

func serveUploadFromRemoteAddr(t *testing.T, handler http.Handler, spec uploadSpec, remoteAddr string, headers http.Header) *httptest.ResponseRecorder {
	t.Helper()
	body, contentType := multipartBody(t, []uploadSpec{spec})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, handlerPath, bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	request.RemoteAddr = remoteAddr
	for key, values := range headers {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	handler.ServeHTTP(recorder, request)
	return recorder
}

func multipartBody(t *testing.T, parts []uploadSpec) ([]byte, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for index, part := range parts {
		field := part.fieldName
		if field == "" {
			field = "file"
		}
		filename := part.fileName
		if filename == "" {
			filename = fmt.Sprintf("upload-%d", index)
		}
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, field, filename))
		if part.mediaType != "" {
			header.Set("Content-Type", part.mediaType)
		}
		if part.contentTransferEncoding != "" {
			header.Set("Content-Transfer-Encoding", part.contentTransferEncoding)
		}
		file, err := writer.CreatePart(header)
		if err != nil {
			t.Fatalf("CreatePart() error = %v", err)
		}
		if part.contentTransferEncoding == "quoted-printable" {
			encoder := quotedprintable.NewWriter(file)
			encoder.Binary = true
			if _, err := encoder.Write(part.data); err != nil {
				t.Fatalf("quoted-printable write error = %v", err)
			}
			if err := encoder.Close(); err != nil {
				t.Fatalf("quoted-printable close error = %v", err)
			}
		} else if _, err := file.Write(part.data); err != nil {
			t.Fatalf("multipart part write error = %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("multipart Close() error = %v", err)
	}
	return body.Bytes(), writer.FormDataContentType()
}

func syntheticPNG() []byte {
	var buffer bytes.Buffer
	imageData := image.NewRGBA(image.Rect(0, 0, 2, 2))
	imageData.Set(0, 0, color.RGBA{R: 255, A: 255})
	if err := png.Encode(&buffer, imageData); err != nil {
		panic(err)
	}
	return buffer.Bytes()
}

func validTestExtraction() passport.Extraction {
	values := map[string]string{
		"surname": "ИВАНОВ", "given_name": "АЛЕКСЕЙ", "gender": "M",
		"birth_date": "1990-04-12", "birth_place": "ГОРОД ПРИМЕР", "series": "0012",
		"number": "000345", "issue_date": "2010-06-18", "issuer": "ОТДЕЛ ПРИМЕРА",
		"department_code": "123-456",
	}
	fields := make(map[string]passport.FieldResult, 11)
	for name, value := range values {
		fields[name] = passport.FieldResult{Value: stringPointer(value), State: "recognized", Issues: []string{}}
	}
	fields["patronymic"] = passport.FieldResult{State: "missing", Issues: []string{}}
	return passport.Extraction{Fields: fields}
}

func allNullTestExtraction() passport.Extraction {
	extraction := validTestExtraction()
	for name, field := range extraction.Fields {
		field.Value = nil
		field.State = "missing"
		extraction.Fields[name] = field
	}
	return extraction
}

func assertJSONResponse(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

func assertNoResponseWritten(t *testing.T, tracked *trackingResponseWriter, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if tracked.writeCalls != 0 || recorder.Body.Len() != 0 {
		t.Fatalf("cancelled request wrote %d response calls and body %q; want no HTTP response", tracked.writeCalls, recorder.Body.String())
	}
}

func decodeJSON(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
		t.Fatalf("response is not JSON object: %v; body=%q", err, recorder.Body.String())
	}
	return value
}

func assertErrorResponse(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	response := decodeJSON(t, recorder)
	if !isUUID(response["request_id"]) {
		t.Errorf("error request_id = %#v, want UUID", response["request_id"])
	}
	if recorder.Header().Get("X-Request-ID") != response["request_id"] {
		t.Errorf("error request ID header=%q body=%#v", recorder.Header().Get("X-Request-ID"), response["request_id"])
	}
	if code, ok := response["code"].(string); !ok || code == "" {
		t.Errorf("error code = %#v, want non-empty string", response["code"])
	}
	if message, ok := response["message"].(string); !ok || message == "" {
		t.Errorf("error message = %#v, want non-empty string", response["message"])
	}
	if len(response) != 3 {
		t.Errorf("error response keys = %#v, want exactly request_id/code/message", response)
	}
	return response
}

func isUUID(value any) bool {
	text, ok := value.(string)
	if !ok {
		return false
	}
	return regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`).MatchString(text)
}

func stringPointer(value string) *string { return &value }

func slicesContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

type countingReader struct {
	reader    io.Reader
	bytesRead int
}

type failOnSecondReadBody struct {
	prefix    []byte
	readCalls int
}

func (body *failOnSecondReadBody) Read(buffer []byte) (int, error) {
	body.readCalls++
	if body.readCalls == 1 {
		return copy(buffer, body.prefix), nil
	}
	return 0, errors.New("synthetic body read sentinel")
}

func (*failOnSecondReadBody) Close() error { return nil }

type cancellationAwareBody struct {
	ctx         context.Context
	readStarted chan struct{}
}

func (body *cancellationAwareBody) Read([]byte) (int, error) {
	close(body.readStarted)
	<-body.ctx.Done()
	return 0, body.ctx.Err()
}

func (*cancellationAwareBody) Close() error { return nil }

type trackingResponseWriter struct {
	http.ResponseWriter
	writeCalls int
}

func (w *trackingResponseWriter) WriteHeader(statusCode int) {
	w.writeCalls++
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *trackingResponseWriter) Write(data []byte) (int, error) {
	w.writeCalls++
	return w.ResponseWriter.Write(data)
}

func (r *countingReader) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	r.bytesRead += n
	return n, err
}
