package ocr

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"passport-backend/internal/httpapi"
	"passport-backend/internal/metrics"
	"passport-backend/internal/telemetry"
)

func TestSupportedRuntimePlatform(t *testing.T) {
	tests := []struct {
		goos string
		want bool
	}{
		{goos: "darwin", want: true},
		{goos: "linux", want: true},
		{goos: "windows", want: false},
		{goos: "", want: false},
	}
	for _, test := range tests {
		t.Run(test.goos, func(t *testing.T) {
			if got := supportedRuntimePlatform(test.goos); got != test.want {
				t.Errorf("supportedRuntimePlatform(%q) = %t, want %t", test.goos, got, test.want)
			}
		})
	}
}

func TestPublicNewRecognizeAndCloseLifecycle(t *testing.T) {
	t.Setenv(russianWorkerHelperEnv, "1")
	config := lifecycleTestConfig(t)
	recognizer, err := New(config)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = recognizer.Close(context.Background()) }()

	result, err := recognizer.Recognize(context.Background(), httpapi.Image{
		Data: lifecycleTestPNG(t), MediaType: "image/png",
	})
	if err != nil {
		t.Fatalf("Recognize() error = %v", err)
	}
	if result.ImageID == "" || !result.ReviewRequired || len(result.Fields) != len(russianWorkerFields) {
		t.Fatalf("unexpected public extraction metadata: %#v", result)
	}
	for _, name := range russianWorkerFields {
		field, ok := result.Fields[name]
		if !ok || field.State != "missing" || field.Value != nil {
			t.Errorf("field %q = %#v, want a present missing field", name, field)
		}
	}
	assertLifecycleWorkRootEmpty(t, config.WorkRoot)

	if err := recognizer.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := recognizer.Recognize(context.Background(), httpapi.Image{
		Data: lifecycleTestPNG(t), MediaType: "image/png",
	}); !errors.Is(err, httpapi.ErrUnavailable) {
		t.Fatalf("Recognize() after Close error = %v, want unavailable", err)
	}
}

func TestPublicNewAcceptsPositiveResourceBoundsAndMinimumOutput(t *testing.T) {
	t.Setenv(russianWorkerHelperEnv, "1")
	config := lifecycleTestConfig(t)
	config.MaxInputBytes = 1
	config.MaxImagePixels = 1
	config.StopGrace = time.Nanosecond
	config.MaxOutputBytes = 64 << 10
	recognizer, err := New(config)
	if err != nil {
		t.Fatalf("New() rejected positive minimum bounds: %v", err)
	}
	if err := recognizer.Close(context.Background()); err != nil {
		t.Fatalf("Close() after minimum-bound startup: %v", err)
	}
}

func TestPublicNewDefaultsZeroStartupTimeout(t *testing.T) {
	t.Setenv(russianWorkerHelperEnv, "1")
	config := lifecycleTestConfig(t)
	config.StartupTimeout = 0
	recognizer, err := New(config)
	if err != nil {
		t.Fatalf("New() rejected zero startup timeout: %v", err)
	}
	defer func() { _ = recognizer.Close(context.Background()) }()
	if recognizer.config.StartupTimeout != 90*time.Second {
		t.Fatalf("normalized startup timeout = %s, want 90s default", recognizer.config.StartupTimeout)
	}
}

func TestPublicNewRejectsExistingRelativeWorkerPath(t *testing.T) {
	t.Setenv(russianWorkerHelperEnv, "1")
	config := lifecycleTestConfig(t)
	currentDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relativeWorker, err := filepath.Rel(currentDirectory, config.RussianWorker)
	if err != nil {
		t.Fatal(err)
	}
	config.RussianWorker = relativeWorker
	if recognizer, err := New(config); err == nil {
		_ = recognizer.Close(context.Background())
		t.Fatal("New() accepted an existing relative worker path")
	} else if err.Error() != "invalid RussianDocsOCR path" {
		t.Fatalf("New() relative worker path error = %v, want invalid RussianDocsOCR path", err)
	}
}

func TestPublicRecognizeAcceptsUploadAtConfiguredByteLimit(t *testing.T) {
	t.Setenv(russianWorkerHelperEnv, "1")
	config := lifecycleTestConfig(t)
	data := lifecycleTestPNG(t)
	config.MaxInputBytes = int64(len(data))
	recognizer, err := New(config)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = recognizer.Close(context.Background()) }()
	result, err := recognizer.Recognize(context.Background(), httpapi.Image{Data: data, MediaType: "image/png"})
	if err != nil {
		t.Fatalf("Recognize() rejected upload at configured byte limit: %v", err)
	}
	if result.ImageID == "" {
		t.Fatal("Recognize() returned no image ID")
	}
}

func TestPublicRecognizeRecordsOCRMetricsOutcomes(t *testing.T) {
	t.Setenv(russianWorkerHelperEnv, "1")
	metricsParent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	metricsRoot := filepath.Join(metricsParent, "private")
	if err := os.Mkdir(metricsRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := metrics.Open(metrics.Config{Path: filepath.Join(metricsRoot, "metrics.json"), PipelineVersion: "lifecycle-metrics-test-v1"})
	if err != nil {
		t.Fatalf("Open(metrics): %v", err)
	}
	defer func() { _ = store.Close() }()
	config := lifecycleTestConfig(t)
	config.Metrics = store
	recognizer, err := New(config)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = recognizer.Close(context.Background()) }()
	if _, err := recognizer.Recognize(context.Background(), httpapi.Image{Data: lifecycleTestPNG(t), MediaType: "image/png"}); err != nil {
		t.Fatalf("successful Recognize() error = %v", err)
	}
	if _, err := recognizer.Recognize(context.Background(), httpapi.Image{MediaType: "image/png"}); !errors.Is(err, httpapi.ErrImageUnprocessable) {
		t.Fatalf("invalid Recognize() error = %v, want image unprocessable", err)
	}
	snapshot := store.Snapshot()
	if snapshot.OCR.Total != 2 || snapshot.OCR.Completed != 1 || snapshot.OCR.Failed != 1 || snapshot.OCR.ByErrorCode["image_unprocessable"] != 1 {
		t.Fatalf("OCR metrics = %+v, want one success and one categorized failure", snapshot.OCR)
	}
}

func TestReserveStopGraceSubtractsGraceFromParentDeadline(t *testing.T) {
	parentDeadline := time.Now().Add(time.Hour)
	parent, cancelParent := context.WithDeadline(context.Background(), parentDeadline)
	defer cancelParent()
	grace := 250 * time.Millisecond
	jobContext, cancelJob := reserveStopGrace(parent, grace)
	defer cancelJob()
	got, ok := jobContext.Deadline()
	want := parentDeadline.Add(-grace)
	if !ok || !got.Equal(want) {
		t.Fatalf("job deadline = %v (present %t), want parent deadline minus grace %v", got, ok, want)
	}
}

func TestPublicRecognizeRejectsInvalidUploadsBeforeWorkspace(t *testing.T) {
	t.Setenv(russianWorkerHelperEnv, "1")
	config := lifecycleTestConfig(t)
	recognizer, err := New(config)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = recognizer.Close(context.Background()) }()

	validPNG := lifecycleTestPNG(t)
	tests := []struct {
		name  string
		image httpapi.Image
	}{
		{name: "empty upload", image: httpapi.Image{MediaType: "image/png"}},
		{name: "oversized upload", image: httpapi.Image{Data: bytes.Repeat([]byte{'x'}, int(config.MaxInputBytes+1)), MediaType: "image/png"}},
		{name: "unsupported media type", image: httpapi.Image{Data: validPNG, MediaType: "image/tiff"}},
		{name: "media type parameters", image: httpapi.Image{Data: validPNG, MediaType: "image/png; charset=binary"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := recognizer.Recognize(context.Background(), test.image); !errors.Is(err, httpapi.ErrImageUnprocessable) {
				t.Fatalf("Recognize() error = %v, want image unprocessable", err)
			}
			assertLifecycleWorkRootEmpty(t, config.WorkRoot)
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := recognizer.Recognize(ctx, httpapi.Image{Data: validPNG, MediaType: "image/png"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Recognize() with cancelled context error = %v, want context canceled", err)
	}
	assertLifecycleWorkRootEmpty(t, config.WorkRoot)
}

func TestPublicCloseCancelsRecognitionAndRemovesWorkspace(t *testing.T) {
	t.Setenv(russianWorkerHelperEnv, "1")
	config := lifecycleTestConfig(t)
	recognizer, err := New(config)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	requestCtx := telemetry.WithRequestID(ctx, "33333333-3333-4333-8333-333333333333")
	data := lifecycleTestPNG(t)
	recognizeDone := make(chan error, 1)
	go func() {
		_, err := recognizer.Recognize(requestCtx, httpapi.Image{Data: data, MediaType: "image/png"})
		recognizeDone <- err
	}()

	waitForLifecycleWorkspace(t, config.WorkRoot)
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer closeCancel()
	if err := recognizer.Close(closeCtx); err != nil {
		t.Fatalf("Close() with active recognition error = %v", err)
	}
	select {
	case err := <-recognizeDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("active Recognize() error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("active Recognize() did not stop after Close")
	}
	assertLifecycleWorkRootEmpty(t, config.WorkRoot)
}

func TestPublicCloseHonorsCancelledShutdownContext(t *testing.T) {
	t.Setenv(russianWorkerHelperEnv, "1")
	config := lifecycleTestConfig(t)
	recognizer, err := New(config)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	requestCtx := telemetry.WithRequestID(context.Background(), "33333333-3333-4333-8333-333333333333")
	data := lifecycleTestPNG(t)
	recognizeDone := make(chan error, 1)
	go func() {
		_, err := recognizer.Recognize(requestCtx, httpapi.Image{Data: data, MediaType: "image/png"})
		recognizeDone <- err
	}()
	waitForLifecycleWorkspace(t, config.WorkRoot)

	closeCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := recognizer.Close(closeCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close() with cancelled shutdown context error = %v, want context canceled", err)
	}
	select {
	case err := <-recognizeDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("active Recognize() error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("active Recognize() did not stop after cancelled Close")
	}
	assertLifecycleWorkRootEmpty(t, config.WorkRoot)
}

func TestPublicNewRejectsInvalidConfigurationAndRuntimePaths(t *testing.T) {
	t.Setenv(russianWorkerHelperEnv, "1")
	base := lifecycleTestConfig(t)
	noExecutePath := filepath.Join(filepath.Dir(base.RussianPython), "not-executable")
	if err := os.WriteFile(noExecutePath, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	fileLink := filepath.Join(t.TempDir(), "worker-link.py")
	if err := os.Symlink(base.RussianWorker, fileLink); err != nil {
		t.Fatalf("create regular-file symlink fixture: %v", err)
	}
	directoryLink := filepath.Join(filepath.Dir(base.RussianModels), "models-link")
	if err := os.Symlink(base.RussianModels, directoryLink); err != nil {
		t.Fatalf("create directory symlink fixture: %v", err)
	}
	workLink := filepath.Join(filepath.Dir(base.WorkRoot), "work-link")
	if err := os.Symlink(base.WorkRoot, workLink); err != nil {
		t.Fatalf("create work-root symlink fixture: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "blank pipeline version", mutate: func(c *Config) { c.PipelineVersion = " " }},
		{name: "nonpositive input limit", mutate: func(c *Config) { c.MaxInputBytes = 0 }},
		{name: "nonpositive output limit", mutate: func(c *Config) { c.MaxOutputBytes = 0 }},
		{name: "output limit below minimum", mutate: func(c *Config) { c.MaxOutputBytes = 1 }},
		{name: "nonpositive image-pixel limit", mutate: func(c *Config) { c.MaxImagePixels = 0 }},
		{name: "nonpositive stop grace", mutate: func(c *Config) { c.StopGrace = 0 }},
		{name: "python path is relative", mutate: func(c *Config) { c.RussianPython = "python" }},
		{name: "python file is not executable", mutate: func(c *Config) { c.RussianPython = noExecutePath }},
		{name: "worker path is a directory", mutate: func(c *Config) { c.RussianWorker = c.RussianRepo }},
		{name: "repository path is a file", mutate: func(c *Config) { c.RussianRepo = c.RussianWorker }},
		{name: "models root is a directory symlink", mutate: func(c *Config) { c.RussianModels = directoryLink }},
		{name: "work root is a directory symlink", mutate: func(c *Config) { c.WorkRoot = workLink }},
		{name: "models root does not exist", mutate: func(c *Config) { c.RussianModels = filepath.Join(c.RussianModels, "missing") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := base
			test.mutate(&config)
			if recognizer, err := New(config); err == nil {
				_ = recognizer.Close(context.Background())
				t.Fatal("New() accepted invalid config")
			} else if test.name == "python file is not executable" && err.Error() != "invalid RussianDocsOCR path" {
				t.Fatalf("New() non-executable python error = %v, want invalid RussianDocsOCR path", err)
			}
		})
	}

	fileLinkConfig := base
	fileLinkConfig.RussianWorker = fileLink
	recognizer, err := New(fileLinkConfig)
	if err != nil {
		t.Fatalf("New() rejected a regular-file worker symlink: %v", err)
	}
	if err := recognizer.Close(context.Background()); err != nil {
		t.Fatalf("Close() after regular-file symlink startup: %v", err)
	}
}

func TestPublicNewFailsClosedWhenWorkerReadinessFails(t *testing.T) {
	t.Setenv(russianWorkerHelperEnv, "1")
	for _, mode := range []string{"bad-schema-ready", "wait-before-ready"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv(russianWorkerHelperModeEnv, mode)
			config := lifecycleTestConfig(t)
			if mode == "wait-before-ready" {
				config.StartupTimeout = 20 * time.Millisecond
			}
			if recognizer, err := New(config); !errors.Is(err, httpapi.ErrUnavailable) {
				if recognizer != nil {
					_ = recognizer.Close(context.Background())
				}
				t.Fatalf("New() = (%v, %v), want unavailable startup", recognizer, err)
			}
		})
	}
}

func TestNormalizedJobErrorMapsPublicCategories(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "unprocessable image", err: httpapi.ErrImageUnprocessable, want: "image_unprocessable"},
		{name: "runtime unavailable", err: httpapi.ErrUnavailable, want: "runtime_unavailable"},
		{name: "deadline", err: context.DeadlineExceeded, want: "deadline_exceeded"},
		{name: "cancellation", err: context.Canceled, want: "cancelled"},
		{name: "worker failure", err: errors.New(workerExitError), want: "pipeline_failed"},
		{name: "other failure", err: errors.New("workspace failed"), want: "processing_failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := normalizedJobError(test.err); got != test.want {
				t.Fatalf("normalizedJobError() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestMediaExtensionMatchesAcceptedTypes(t *testing.T) {
	tests := []struct {
		mediaType string
		want      string
	}{
		{mediaType: "image/jpeg", want: ".jpg"},
		{mediaType: "image/png", want: ".png"},
		{mediaType: "image/webp", want: ".webp"},
		{mediaType: "image/unsupported", want: ".bin"},
	}
	for _, test := range tests {
		t.Run(test.mediaType, func(t *testing.T) {
			if got := mediaExtension(test.mediaType); got != test.want {
				t.Fatalf("mediaExtension(%q) = %q, want %q", test.mediaType, got, test.want)
			}
		})
	}
}

func TestCreateWorkerJobWritesPrivateImageAndRejectsMissingRoot(t *testing.T) {
	config := lifecycleTestConfig(t)
	imageData := lifecycleTestPNG(t)
	const requestID = "11111111-1111-4111-8111-111111111111"
	job, jobRoot, err := createWorkerJob(config.WorkRoot, httpapi.Image{Data: imageData, MediaType: "image/png"}, requestID)
	if err != nil {
		t.Fatalf("createWorkerJob() error = %v", err)
	}
	if job.RequestID != requestID || job.MediaType != "image/png" || job.ImageID == "" {
		t.Fatalf("worker job metadata = %#v", job)
	}
	if filepath.Dir(job.ImagePath) != jobRoot || filepath.Base(job.ImagePath) != "upload.png" {
		t.Fatalf("worker image path = %q under job root %q", job.ImagePath, jobRoot)
	}
	info, err := os.Stat(job.ImagePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("image file mode = %04o, want 0600", info.Mode().Perm())
	}
	stored, err := os.ReadFile(job.ImagePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, imageData) {
		t.Fatal("workspace image contents differ from upload")
	}
	if err := os.RemoveAll(jobRoot); err != nil {
		t.Fatal(err)
	}

	_, _, err = createWorkerJob(filepath.Join(config.WorkRoot, "missing"), httpapi.Image{Data: imageData, MediaType: "image/png"}, requestID)
	if !errors.Is(err, httpapi.ErrUnavailable) {
		t.Fatalf("createWorkerJob() with absent root error = %v, want unavailable", err)
	}
}

func TestRussianWorkerRejectsNonPrivateInputPaths(t *testing.T) {
	worker, baseJob, input := newRussianWorkerFixture(t, "", 1<<20, 64<<10)
	defer func() { _ = worker.close() }()
	workRoot := worker.config.WorkRoot
	outsideRoot := t.TempDir()
	privatePath := filepath.Join(workRoot, "private.png")
	if err := os.WriteFile(privatePath, []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	readablePath := filepath.Join(workRoot, "readable.png")
	if err := os.WriteFile(readablePath, []byte("synthetic"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(readablePath, 0o644); err != nil {
		t.Fatal(err)
	}
	readableInfo, err := os.Stat(readablePath)
	if err != nil {
		t.Fatal(err)
	}
	if readableInfo.Mode().Perm() != 0o644 {
		t.Fatalf("readable fixture mode = %04o, want 0644", readableInfo.Mode().Perm())
	}
	outsidePath := filepath.Join(outsideRoot, "outside.png")
	if err := os.WriteFile(outsidePath, []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(workRoot, "linked.png")
	if err := os.Symlink(outsidePath, symlinkPath); err != nil {
		t.Fatal(err)
	}
	privateLink := filepath.Join(workRoot, "private-link.png")
	if err := os.Symlink(privatePath, privateLink); err != nil {
		t.Fatal(err)
	}
	directoryPath := filepath.Join(workRoot, "directory")
	if err := os.Mkdir(directoryPath, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"outside workspace":         outsidePath,
		"symlink outside workspace": symlinkPath,
		"symlink inside workspace":  privateLink,
		"group-readable file":       readablePath,
		"directory in workspace":    directoryPath,
		"workspace root itself":     workRoot,
		"missing path":              filepath.Join(workRoot, "missing.png"),
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			job := baseJob
			job.ImagePath = path
			_, err := worker.recognize(ctx, job, input)
			if !errors.Is(err, httpapi.ErrImageUnprocessable) {
				t.Fatalf("recognize() error = %v, want image unprocessable", err)
			}
			if worker.cmd != nil {
				t.Fatal("worker started for an input path outside private-file requirements")
			}
		})
	}
	if !russianInputPathIsPrivate(privatePath, workRoot) {
		t.Fatal("private regular file inside work root was rejected")
	}
}

func lifecycleTestConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	repoRoot := filepath.Join(root, "repo")
	modelsRoot := filepath.Join(root, "models")
	workRoot := filepath.Join(root, "work")
	for _, path := range []string{repoRoot, modelsRoot, workRoot} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatalf("Mkdir(%q): %v", path, err)
		}
	}
	_, workerPath, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() did not return worker test source path")
	}
	pythonPath, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable(): %v", err)
	}
	return Config{
		RussianPython: pythonPath, RussianWorker: workerPath, RussianRepo: repoRoot,
		RussianModels: modelsRoot, WorkRoot: workRoot, PipelineVersion: "russiandocs-lifecycle-test-v1",
		StartupTimeout: 5 * time.Second, MaxInputBytes: 1 << 20, MaxOutputBytes: 1 << 20,
		MaxImagePixels: 100, StopGrace: 100 * time.Millisecond,
	}
}

func lifecycleTestPNG(t *testing.T) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatalf("encode synthetic PNG: %v", err)
	}
	return encoded.Bytes()
}

func waitForLifecycleWorkspace(t *testing.T, root string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatalf("ReadDir(work root): %v", err)
		}
		if len(entries) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("recognition did not create its private workspace")
}

func assertLifecycleWorkRootEmpty(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir(work root): %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("work root retains %d entry/entries after recognition", len(entries))
	}
}
