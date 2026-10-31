//go:build darwin || linux

package ocr

import (
	"bufio"
	"bytes"
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

	"passport-backend/internal/httpapi"
)

const russianWorkerHelperEnv = "PASSPORT_RUSSIAN_WORKER_HELPER"
const russianWorkerHelperModeEnv = "PASSPORT_RUSSIAN_WORKER_HELPER_MODE"

func init() {
	if os.Getenv(russianWorkerHelperEnv) != "1" {
		return
	}
	signal.Ignore(syscall.SIGTERM)
	writer := bufio.NewWriter(os.Stdout)
	switch os.Getenv(russianWorkerHelperModeEnv) {
	case "exit-before-ready":
		os.Exit(0)
	case "wait-before-ready":
		time.Sleep(time.Hour)
	case "malformed-ready":
		_, _ = fmt.Fprintln(writer, "{")
		_ = writer.Flush()
		os.Exit(0)
	case "bad-schema-ready":
		_, _ = fmt.Fprintln(writer, `{"schema_version":"wrong","type":"ready"}`)
		_ = writer.Flush()
		os.Exit(0)
	case "trailing-ready":
		_, _ = fmt.Fprintf(writer, `{"schema_version":%q,"type":"ready"} {}`+"\n", russianWorkerSchema)
		_ = writer.Flush()
		os.Exit(0)
	case "unknown-ready-field":
		_, _ = fmt.Fprintf(writer, `{"schema_version":%q,"type":"ready","unexpected":true}`+"\n", russianWorkerSchema)
		_ = writer.Flush()
		os.Exit(0)
	case "oversized-ready":
		_, _ = fmt.Fprintln(writer, strings.Repeat("x", 70<<10))
		_ = writer.Flush()
		os.Exit(0)
	}
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
		status, code := "completed", "ok"
		responseSchema, responseType := russianWorkerSchema, "result"
		responseRequestID, responseImageID := request.RequestID, request.ImageID
		responseLength := 0
		switch os.Getenv(russianWorkerHelperModeEnv) {
		case "invalid-image-result":
			status, code = "rejected", "invalid_image"
		case "runtime-unavailable-result":
			status, code = "failed", "runtime_unavailable"
		case "unknown-result-status":
			status, code = "failed", "unexpected"
		case "wrong-request-id-result":
			responseRequestID += "-mismatch"
		case "wrong-result-schema":
			responseSchema = "wrong-schema"
		case "wrong-result-type":
			responseType = "unexpected"
		case "wrong-image-id-result":
			responseImageID += "-mismatch"
		case "max-output-line-exact":
			responseLength = (64 << 10) - 1
		case "max-output-line-over":
			responseLength = 64 << 10
		}
		fields := make(map[string]*string, len(russianWorkerFields))
		for _, name := range russianWorkerFields {
			fields[name] = nil
		}
		response, _ := json.Marshal(russianResponse{
			SchemaVersion: responseSchema, Type: responseType, RequestID: responseRequestID,
			ImageID: responseImageID, Status: status, Code: code, Fields: fields,
		})
		if responseLength > len(response) {
			response = append(response, bytes.Repeat([]byte{' '}, responseLength-len(response))...)
		}
		_, _ = writer.Write(append(response, '\n'))
		_ = writer.Flush()
	}
	os.Exit(0)
}

func TestRussianWorkerRecognizeCancellationAndProtocolFailureBoundaries(t *testing.T) {
	t.Run("cancel while waiting for request slot", func(t *testing.T) {
		workRoot := t.TempDir()
		jobRoot := filepath.Join(workRoot, "job")
		if err := os.Mkdir(jobRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		imagePath := filepath.Join(jobRoot, "input.png")
		if err := os.WriteFile(imagePath, []byte("synthetic"), 0o600); err != nil {
			t.Fatal(err)
		}
		worker := &russianWorker{config: Config{WorkRoot: workRoot, MaxInputBytes: 1 << 20}, gate: make(chan struct{}, 1)}
		worker.gate <- struct{}{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := worker.recognize(ctx, workerJob{ImagePath: imagePath}, []byte("synthetic"))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("recognize() error = %v, want context canceled", err)
		}
		if worker.cmd != nil {
			t.Fatal("worker started while request slot was unavailable")
		}
	})

	t.Run("readiness failure maps to unavailable", func(t *testing.T) {
		t.Setenv(russianWorkerHelperEnv, "1")
		t.Setenv(russianWorkerHelperModeEnv, "bad-schema-ready")
		root := t.TempDir()
		workRoot := filepath.Join(root, "work")
		if err := os.Mkdir(workRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		jobRoot := filepath.Join(workRoot, "job")
		if err := os.Mkdir(jobRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		imagePath := filepath.Join(jobRoot, "input.png")
		if err := os.WriteFile(imagePath, []byte("synthetic"), 0o600); err != nil {
			t.Fatal(err)
		}
		workerPath := filepath.Join(root, "worker.py")
		if err := os.WriteFile(workerPath, []byte("# test helper is this Go test binary\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		worker := &russianWorker{config: Config{
			RussianPython: os.Args[0], RussianWorker: workerPath, RussianRepo: root,
			RussianModels: root, WorkRoot: workRoot, MaxOutputBytes: 64 << 10,
			MaxInputBytes: 1 << 20, MaxImagePixels: 100, StopGrace: 100 * time.Millisecond,
		}, gate: make(chan struct{}, 1)}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := worker.recognize(ctx, workerJob{
			ImagePath: imagePath, ImageID: "22222222-2222-4222-8222-222222222222",
			RequestID: "11111111-1111-4111-8111-111111111111", MediaType: "image/png",
		}, []byte("synthetic"))
		if !errors.Is(err, httpapi.ErrUnavailable) {
			t.Fatalf("recognize() error = %v, want unavailable", err)
		}
		if worker.cmd != nil {
			t.Fatal("worker remained attached after readiness failure")
		}
	})

	for _, mode := range []string{"wrong-result-schema", "wrong-result-type", "wrong-request-id-result", "wrong-image-id-result"} {
		t.Run(mode+" stops worker", func(t *testing.T) {
			assertWorkerRejectsCorrelatedResponse(t, mode)
		})
	}
}

func assertWorkerRejectsCorrelatedResponse(t *testing.T, mode string) {
	t.Helper()
	worker, job, input := newRussianWorkerFixture(t, mode, 1<<20, 64<<10)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := worker.recognize(ctx, job, input)
	if err == nil || !strings.Contains(err.Error(), workerExitError) {
		t.Fatalf("recognize() error = %v, want correlated protocol failure", err)
	}
	if worker.cmd != nil {
		t.Fatal("worker was not stopped after response correlation mismatch")
	}
}

func newRussianWorkerFixture(t *testing.T, mode string, maxInputBytes, maxOutputBytes int64) (*russianWorker, workerJob, []byte) {
	t.Helper()
	t.Setenv(russianWorkerHelperEnv, "1")
	t.Setenv(russianWorkerHelperModeEnv, mode)
	root := t.TempDir()
	workRoot := filepath.Join(root, "work")
	jobRoot := filepath.Join(workRoot, "job")
	for _, directory := range []string{workRoot, jobRoot} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	imagePath := filepath.Join(jobRoot, "input.png")
	input := []byte("synthetic")
	if err := os.WriteFile(imagePath, input, 0o600); err != nil {
		t.Fatal(err)
	}
	workerPath := filepath.Join(root, "worker.py")
	if err := os.WriteFile(workerPath, []byte("# test helper is this Go test binary\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	worker := &russianWorker{config: Config{
		RussianPython: os.Args[0], RussianWorker: workerPath, RussianRepo: root,
		RussianModels: root, WorkRoot: workRoot, MaxOutputBytes: maxOutputBytes,
		MaxInputBytes: maxInputBytes, MaxImagePixels: 100, StopGrace: 100 * time.Millisecond,
		PipelineVersion: "russiandocs-boundary-test-v1",
	}, gate: make(chan struct{}, 1)}
	job := workerJob{
		ImagePath: imagePath, ImageID: "22222222-2222-4222-8222-222222222222",
		RequestID: "11111111-1111-4111-8111-111111111111", MediaType: "image/png",
	}
	return worker, job, input
}

func TestRussianWorkerStatusResultsKeepPublicErrorCategories(t *testing.T) {
	t.Setenv(russianWorkerHelperEnv, "1")
	tests := []struct {
		name     string
		mode     string
		want     error
		wantText string
	}{
		{name: "invalid image", mode: "invalid-image-result", want: httpapi.ErrImageUnprocessable},
		{name: "runtime unavailable", mode: "runtime-unavailable-result", want: httpapi.ErrUnavailable},
		{name: "unknown status", mode: "unknown-result-status", wantText: workerExitError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(russianWorkerHelperModeEnv, test.mode)
			root := t.TempDir()
			workRoot := filepath.Join(root, "work")
			if err := os.Mkdir(workRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			jobRoot := filepath.Join(workRoot, "job")
			if err := os.Mkdir(jobRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			imagePath := filepath.Join(jobRoot, "input.png")
			if err := os.WriteFile(imagePath, []byte("synthetic"), 0o600); err != nil {
				t.Fatal(err)
			}
			workerPath := filepath.Join(root, "worker.py")
			if err := os.WriteFile(workerPath, []byte("# test helper is this Go test binary\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			config := Config{
				RussianPython: os.Args[0], RussianWorker: workerPath, RussianRepo: root,
				RussianModels: root, WorkRoot: workRoot, MaxOutputBytes: 64 << 10,
				MaxInputBytes: 1 << 20, MaxImagePixels: 100, StopGrace: 100 * time.Millisecond,
				PipelineVersion: "russiandocs-status-test-v1",
			}
			worker := &russianWorker{config: config, gate: make(chan struct{}, 1)}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			job := workerJob{
				ImagePath: imagePath, ImageID: "22222222-2222-4222-8222-222222222222",
				RequestID: "11111111-1111-4111-8111-111111111111", MediaType: "image/png",
			}
			_, err := worker.recognize(ctx, job, []byte("synthetic"))
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("recognize() error = %v, want category %v", err, test.want)
			}
			if test.wantText != "" && (err == nil || !strings.Contains(err.Error(), test.wantText)) {
				t.Fatalf("recognize() error = %v, want message containing %q", err, test.wantText)
			}
			if worker.cmd == nil {
				t.Fatal("status response unexpectedly stopped the worker")
			}
			if err := worker.close(); err != nil {
				t.Fatalf("close worker: %v", err)
			}
		})
	}
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
	unknownFields := make(map[string]*string, len(base.Fields)+1)
	for name, value := range base.Fields {
		unknownFields[name] = value
	}
	unknownFields["unexpected"] = nil
	unknownFieldResponse := base
	unknownFieldResponse.Fields = unknownFields
	unknownFieldData, err := json.Marshal(unknownFieldResponse)
	if err != nil {
		t.Fatal(err)
	}
	missingFields := make(map[string]*string, len(base.Fields)-1)
	for name, value := range base.Fields {
		if name != "surname" {
			missingFields[name] = value
		}
	}
	missingFieldResponse := base
	missingFieldResponse.Fields = missingFields
	missingFieldData, err := json.Marshal(missingFieldResponse)
	if err != nil {
		t.Fatal(err)
	}
	escapedDuplicateData := []byte(strings.Replace(string(encoded), `"surname":null`, `"surname":null,"sur\u006eame":null`, 1))
	if _, err := decodeRussianResponse(encoded); err != nil {
		t.Fatalf("valid response rejected: %v", err)
	}
	for _, valueLength := range []int{4096, 4097} {
		value := strings.Repeat("x", valueLength)
		bounded := base
		bounded.Fields = make(map[string]*string, len(base.Fields))
		for name, fieldValue := range base.Fields {
			bounded.Fields[name] = fieldValue
		}
		bounded.Fields["surname"] = &value
		data, err := json.Marshal(bounded)
		if err != nil {
			t.Fatal(err)
		}
		_, decodeErr := decodeRussianResponse(data)
		if valueLength == 4096 && decodeErr != nil {
			t.Fatalf("decodeRussianResponse() rejected 4096-character field: %v", decodeErr)
		}
		if valueLength == 4097 && decodeErr == nil {
			t.Fatal("decodeRussianResponse() accepted a 4097-character field")
		}
	}

	for name, data := range map[string][]byte{
		"non-object root":                  []byte(`[]`),
		"duplicate key":                    []byte(`{"schema_version":"x","schema_version":"y"}`),
		"escaped equivalent key in fields": escapedDuplicateData,
		"unknown envelope key":             append(encoded[:len(encoded)-1], []byte(`,"unexpected":true}`)...),
		"trailing JSON value":              append(append([]byte(nil), encoded...), []byte(` {}`)...),
		"unknown field key":                unknownFieldData,
		"missing field key":                missingFieldData,
		"oversized value":                  oversizedRussianResponse(t, base),
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

func TestRussianWorkerRejectsInvalidReadinessFrames(t *testing.T) {
	t.Setenv(russianWorkerHelperEnv, "1")
	for _, mode := range []string{"exit-before-ready", "malformed-ready", "bad-schema-ready", "trailing-ready", "unknown-ready-field", "oversized-ready", "wait-before-ready"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv(russianWorkerHelperModeEnv, mode)
			root := t.TempDir()
			workerPath := filepath.Join(root, "worker.py")
			if err := os.WriteFile(workerPath, []byte("# subprocess fixture\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			worker := &russianWorker{config: Config{
				RussianPython: os.Args[0], RussianWorker: workerPath,
				RussianRepo: root, RussianModels: root, WorkRoot: root,
				MaxInputBytes: 1 << 20, MaxOutputBytes: 64 << 10,
				MaxImagePixels: 100, StopGrace: 50 * time.Millisecond,
			}, gate: make(chan struct{}, 1)}
			startupTimeout := time.Second
			if mode == "wait-before-ready" {
				startupTimeout = 20 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), startupTimeout)
			defer cancel()
			if err := worker.start(ctx); err == nil {
				t.Fatal("start() accepted invalid readiness")
			}
			if worker.cmd != nil {
				t.Fatal("worker process remained attached after readiness failure")
			}
		})
	}
}

func TestRussianWorkerEnforcesInputAndOutputByteBoundaries(t *testing.T) {
	t.Run("input accepts exact cap and rejects cap plus one", func(t *testing.T) {
		input := []byte("synthetic")
		worker, job, _ := newRussianWorkerFixture(t, "", int64(len(input)), 64<<10)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := worker.recognize(ctx, job, input); err != nil {
			t.Fatalf("recognize() rejected input at exact byte cap: %v", err)
		}
		tooLarge := append(append([]byte(nil), input...), 'x')
		if err := os.WriteFile(job.ImagePath, tooLarge, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := worker.recognize(ctx, job, tooLarge); !errors.Is(err, httpapi.ErrImageUnprocessable) {
			t.Fatalf("recognize() error above input cap = %v, want image unprocessable", err)
		}
		if worker.cmd == nil {
			t.Fatal("worker was stopped for a rejected oversized input")
		}
		if err := worker.close(); err != nil {
			t.Fatalf("close worker: %v", err)
		}
	})

	for _, test := range []struct {
		mode        string
		wantFailure bool
	}{
		{mode: "max-output-line-exact"},
		{mode: "max-output-line-over", wantFailure: true},
	} {
		t.Run(test.mode, func(t *testing.T) {
			worker, job, input := newRussianWorkerFixture(t, test.mode, 1<<20, 64<<10)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := worker.recognize(ctx, job, input)
			if !test.wantFailure && err != nil {
				t.Fatalf("recognize() rejected line at configured frame limit: %v", err)
			}
			if test.wantFailure && (err == nil || !strings.Contains(err.Error(), workerExitError)) {
				t.Fatalf("recognize() error over frame limit = %v, want protocol failure", err)
			}
			if test.wantFailure && worker.cmd != nil {
				t.Fatal("worker was not stopped after oversized frame")
			}
			if worker.cmd != nil {
				if err := worker.close(); err != nil {
					t.Fatalf("close worker: %v", err)
				}
			}
		})
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
