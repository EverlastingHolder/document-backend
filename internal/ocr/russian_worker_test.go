//go:build darwin || linux

package ocr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const russianWorkerHelperEnv = "PASSPORT_RUSSIAN_WORKER_HELPER"

func init() {
	if os.Getenv(russianWorkerHelperEnv) != "1" {
		return
	}
	signal.Ignore(syscall.SIGTERM)
	writer := bufio.NewWriter(os.Stdout)
	_, _ = fmt.Fprintf(writer, "{\"schema_version\":%q,\"type\":\"ready\"}\n", russianWorkerSchema)
	_ = writer.Flush()
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request russianRequest
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			os.Exit(120)
		}
		if strings.HasPrefix(request.RequestID, "33333333-") {
			for {
				time.Sleep(time.Hour)
			}
		}
		if strings.HasPrefix(request.RequestID, "44444444-") {
			_, _ = fmt.Fprintln(os.Stdout, strings.Repeat("x", 70<<10))
			continue
		}
		fields := make(map[string]*string, len(russianWorkerFields))
		for _, name := range russianWorkerFields {
			fields[name] = nil
		}
		response, _ := json.Marshal(russianResponse{
			SchemaVersion: russianWorkerSchema, Type: "result", RequestID: request.RequestID,
			ImageID: request.ImageID, Status: "completed", Code: "ok", Fields: fields,
		})
		_, _ = writer.Write(append(response, '\n'))
		_ = writer.Flush()
	}
	os.Exit(0)
}

func TestDecodeRussianResponseRequiresBoundedFieldSetAndStrictEnvelope(t *testing.T) {
	fields := make(map[string]*string, len(russianWorkerFields))
	for _, name := range russianWorkerFields {
		fields[name] = nil
	}
	base := russianResponse{
		SchemaVersion: russianWorkerSchema,
		Type:          "result",
		RequestID:     "11111111-1111-4111-8111-111111111111",
		ImageID:       "22222222-2222-4222-8222-222222222222",
		Status:        "completed",
		Code:          "ok",
		Fields:        fields,
	}
	encoded, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeRussianResponse(encoded); err != nil {
		t.Fatalf("valid response rejected: %v", err)
	}

	for name, data := range map[string][]byte{
		"duplicate key":        []byte(`{"schema_version":"x","schema_version":"y"}`),
		"unknown envelope key": append(encoded[:len(encoded)-1], []byte(`,"unexpected":true}`)...),
		"oversized value":      oversizedRussianResponse(t, base),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeRussianResponse(data); err == nil {
				t.Fatal("malformed response accepted")
			}
		})
	}
}

func TestRussianWorkerSerialProtocolCancelsAndRestarts(t *testing.T) {
	t.Setenv(russianWorkerHelperEnv, "1")
	root := t.TempDir()
	workRoot := filepath.Join(root, "work")
	if err := os.Mkdir(workRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	jobRoot := filepath.Join(workRoot, "job")
	if err := os.Mkdir(jobRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(jobRoot, "upload.png")
	if err := os.WriteFile(imagePath, []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	workerPath := filepath.Join(root, "worker.py")
	if err := os.WriteFile(workerPath, []byte("# helper is implemented by the Go test binary\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := Config{
		RussianPython: os.Args[0], RussianWorker: workerPath, RussianRepo: root,
		RussianModels: root, WorkRoot: workRoot, MaxOutputBytes: 64 << 10,
		MaxInputBytes: 1 << 20, MaxImagePixels: 100, StopGrace: 100 * time.Millisecond,
		PipelineVersion: "russiandocs-test-v1",
	}
	worker := &russianWorker{config: config, gate: make(chan struct{}, 1)}
	startCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := worker.start(startCtx); err != nil {
		t.Fatalf("worker startup handshake failed: %v", err)
	}
	defer func() { _ = worker.close() }()

	job := workerJob{ImagePath: imagePath, ImageID: "22222222-2222-4222-8222-222222222222", RequestID: "33333333-3333-4333-8333-333333333333", MediaType: "image/png"}
	ctx, cancelRequest := context.WithTimeout(context.Background(), 50*time.Millisecond)
	_, err := worker.recognize(ctx, job, []byte("synthetic"))
	cancelRequest()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled request error = %v, want deadline exceeded", err)
	}
	if worker.cmd != nil {
		t.Fatal("stuck worker process was not terminated after request deadline")
	}

	job.RequestID = "11111111-1111-4111-8111-111111111111"
	ctx, cancelRequest = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRequest()
	result, err := worker.recognize(ctx, job, []byte("synthetic"))
	if err != nil {
		t.Fatalf("request after restart failed: %v", err)
	}
	if result.ImageID != job.ImageID || !result.ReviewRequired || result.Fields["surname"].Value != nil {
		t.Fatalf("unexpected structured extraction: %#v", result)
	}

	job.RequestID = "44444444-4444-4444-8444-444444444444"
	if _, err := worker.recognize(ctx, job, []byte("synthetic")); err == nil || !strings.Contains(err.Error(), workerExitError) {
		t.Fatalf("oversized frame error = %v, want protocol failure", err)
	}
	if worker.cmd != nil {
		t.Fatal("worker with oversized frame was not stopped")
	}
}

func oversizedRussianResponse(t *testing.T, base russianResponse) []byte {
	t.Helper()
	tooLong := make([]byte, 4097)
	for i := range tooLong {
		tooLong[i] = 'x'
	}
	value := string(tooLong)
	base.Fields["surname"] = &value
	encoded, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
