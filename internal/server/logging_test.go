package server

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"passport-backend/internal/telemetry"
)

func TestServerLogsBoundLifecycleEvents(t *testing.T) {
	path, sink := serverTestSink(t)
	defer sink.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := validServerConfig()
	cfg.Logger = sink.Logger()
	instance, err := New(cfg, &testRecognizer{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- instance.Serve(ctx, listener) }()
	waitForEvent(t, path, "service.started")
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop")
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	events := readServerEvents(t, path)
	if strings.Join(events, ",") != "service.started,service.stopping,service.stopped" {
		t.Fatalf("lifecycle events = %v", events)
	}
}

func TestServerLogsSafeStartupFailure(t *testing.T) {
	path, sink := serverTestSink(t)
	defer sink.Close()
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	cfg := validServerConfig()
	cfg.Address = occupied.Addr().String()
	cfg.Logger = sink.Logger()
	instance, err := New(cfg, &testRecognizer{})
	if err != nil {
		t.Fatal(err)
	}
	if err := instance.Run(context.Background()); err == nil {
		t.Fatal("Run() error = nil, want bind failure")
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), occupied.Addr().String()) || strings.Contains(string(data), "address already in use") {
		t.Fatalf("raw bind detail leaked: %s", data)
	}
	events := readServerEvents(t, path)
	if strings.Join(events, ",") != "service.failed" {
		t.Fatalf("failure events = %v", events)
	}
}

func serverTestSink(t *testing.T) (string, *telemetry.Sink) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(root, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(private, "api.jsonl")
	sink, err := telemetry.Open(telemetry.Config{Path: path, Level: "debug", MaxSizeBytes: 64 << 10, MaxBackups: 1})
	if err != nil {
		t.Fatal(err)
	}
	return path, sink
}

func waitForEvent(t *testing.T, path, wanted string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if events := readServerEvents(t, path); len(events) > 0 && events[0] == wanted {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("event %q not logged", wanted)
}

func readServerEvents(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var events []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("invalid server log JSON: %v", err)
		}
		if entry["component"] != "server" || entry["service"] != "passport-api" {
			t.Fatalf("server log base fields: %#v", entry)
		}
		event, _ := entry["event"].(string)
		events = append(events, event)
	}
	return events
}
