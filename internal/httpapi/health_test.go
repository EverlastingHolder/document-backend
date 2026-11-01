package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHealthAndReadinessRoutes(t *testing.T) {
	readyErr := errors.New("private sentinel")
	checks := 0
	config := testConfig()
	config.ReadinessTimeout = time.Second
	config.ReadinessCheck = func(ctx context.Context) error {
		checks++
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("readiness check has no deadline")
		}
		return readyErr
	}
	handler := newTestHandler(t, config, &fakeRecognizer{})

	liveness := httptest.NewRecorder()
	livenessRequest := httptest.NewRequest(http.MethodGet, healthPath, nil)
	livenessRequest.Header.Set("X-Request-ID", "malformed-private-sentinel")
	handler.ServeHTTP(liveness, livenessRequest)
	if liveness.Code != http.StatusNoContent || liveness.Body.Len() != 0 || checks != 0 {
		t.Fatalf("liveness = %d %q; readiness checks = %d", liveness.Code, liveness.Body.String(), checks)
	}

	readiness := httptest.NewRecorder()
	handler.ServeHTTP(readiness, httptest.NewRequest(http.MethodGet, readyPath, nil))
	if readiness.Code != http.StatusServiceUnavailable || readiness.Body.Len() != 0 || checks != 1 {
		t.Fatalf("readiness = %d %q; readiness checks = %d", readiness.Code, readiness.Body.String(), checks)
	}
	if readiness.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("readiness cache header = %q", readiness.Header().Get("Cache-Control"))
	}

	wrongMethod := httptest.NewRecorder()
	handler.ServeHTTP(wrongMethod, httptest.NewRequest(http.MethodPost, readyPath, nil))
	if wrongMethod.Code != http.StatusMethodNotAllowed || wrongMethod.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("wrong method = %d Allow=%q", wrongMethod.Code, wrongMethod.Header().Get("Allow"))
	}
}

func TestReadinessWithoutAsyncDependenciesIsReady(t *testing.T) {
	handler := newTestHandler(t, testConfig(), &fakeRecognizer{})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, readyPath, nil))
	if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
		t.Fatalf("readiness = %d %q", response.Code, response.Body.String())
	}
}
