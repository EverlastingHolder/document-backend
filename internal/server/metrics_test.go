package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"passport-backend/internal/metrics"
)

func TestServerPassesMetricsStoreToHTTPHandler(t *testing.T) {
	store := openServerTestMetrics(t, "stats.json")

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := validServerConfig()
	cfg.Metrics = store
	instance, err := New(cfg, &testRecognizer{})
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	run := serveServer(t, instance, ctx, cancel, listener)
	baseURL := "http://" + run.listener.Addr().String()

	response, err := postSyntheticUpload(ctx, baseURL+"/v1/passport/recognize")
	if err != nil {
		t.Fatalf("POST recognize: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("recognize status = %d, want 200", response.StatusCode)
	}

	statsResponse, err := http.Get(baseURL + "/v1/stats")
	if err != nil {
		t.Fatalf("GET stats: %v", err)
	}
	defer statsResponse.Body.Close()
	if statsResponse.StatusCode != http.StatusOK {
		t.Fatalf("stats status = %d, want 200", statsResponse.StatusCode)
	}
	var payload struct {
		RequestID  string `json:"request_id"`
		Statistics struct {
			HTTP struct {
				Total    uint64            `json:"total"`
				ByStatus map[string]uint64 `json:"by_status"`
			} `json:"http"`
		} `json:"statistics"`
	}
	if err := json.NewDecoder(statsResponse.Body).Decode(&payload); err != nil {
		t.Fatalf("decode stats response: %v", err)
	}
	if payload.RequestID == "" || statsResponse.Header.Get("X-Request-ID") != payload.RequestID || payload.Statistics.HTTP.Total != 1 || payload.Statistics.HTTP.ByStatus["200"] != 1 {
		t.Fatalf("stats payload=%+v header request ID=%q", payload, statsResponse.Header.Get("X-Request-ID"))
	}
}

func TestServerRejectsDifferentHTTPAndServerMetricsStores(t *testing.T) {
	cfg := validServerConfig()
	cfg.Metrics = openServerTestMetrics(t, "server.json")
	cfg.HTTP.Metrics = openServerTestMetrics(t, "http.json")
	if _, err := New(cfg, &testRecognizer{}); err == nil {
		t.Fatal("New() error = nil, want distinct HTTP/server metrics stores rejected")
	}
}

func openServerTestMetrics(t *testing.T, filename string) *metrics.Store {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(root, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := metrics.Open(metrics.Config{Path: filepath.Join(private, filename), PipelineVersion: testPipelineVersion})
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
