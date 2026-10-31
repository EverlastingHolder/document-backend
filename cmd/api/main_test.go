package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"passport-backend/internal/httpapi"
	"passport-backend/internal/metrics"
	"passport-backend/internal/ocr"
	"passport-backend/internal/passport"
	"passport-backend/internal/server"
)

func TestRunReturnsBeforeConstructionWhenContextIsAlreadyCancelled(t *testing.T) {
	configPath := writeAPIConfig(t, validAPIConfig())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var providers, servers int
	err := run(ctx, configPath, &bytes.Buffer{},
		func(ocr.Config) (managedRecognizer, error) {
			providers++
			return &fakeManagedRecognizer{}, nil
		},
		func(server.Config, httpapi.Recognizer) (runner, error) {
			servers++
			return &fakeRunner{}, nil
		},
	)
	if err != nil {
		t.Fatalf("run() error = %v, want nil for pre-cancelled context", err)
	}
	if providers != 0 || servers != 0 {
		t.Fatalf("constructors called provider=%d server=%d; want neither", providers, servers)
	}
}

func TestRunPreparesPrivateWorkRootBeforeProviderConstruction(t *testing.T) {
	configPath := writeAPIConfig(t, validAPIConfig())
	configDir := filepath.Dir(configPath)
	workRoot := filepath.Join(configDir, "work")
	var output bytes.Buffer
	var providerMetrics *metrics.Store
	runErr := run(context.Background(), configPath, &output,
		func(config ocr.Config) (managedRecognizer, error) {
			providerMetrics = config.Metrics
			if config.Metrics == nil {
				t.Error("provider factory received nil metrics store")
			}
			if config.WorkRoot != workRoot {
				t.Errorf("provider WorkRoot = %q, want %q", config.WorkRoot, workRoot)
			}
			info, err := os.Stat(workRoot)
			if err != nil {
				t.Errorf("WorkRoot missing before provider construction: %v", err)
			} else if info.Mode().Perm() != 0o700 {
				t.Errorf("new WorkRoot mode = %o, want 700", info.Mode().Perm())
			}
			return &fakeManagedRecognizer{}, nil
		},
		func(config server.Config, recognizer httpapi.Recognizer) (runner, error) {
			if config.Metrics == nil || recognizer == nil {
				t.Fatal("server factory did not receive provider and metrics store")
			}
			if providerMetrics == nil || providerMetrics != config.Metrics {
				t.Error("provider and server received different metrics stores")
			}
			if config.HTTP.MaxUploadBytes != 4<<20 {
				t.Errorf("MaxUploadBytes = %d, want 4194304", config.HTTP.MaxUploadBytes)
			}
			if config.Metrics == nil || config.HTTP.Metrics != config.Metrics {
				t.Error("server and HTTP config did not receive the same metrics store")
			}
			if recognizer == nil {
				t.Error("server factory received nil recognizer")
			}
			return &fakeRunner{}, nil
		},
	)
	if runErr != nil {
		t.Fatalf("run() error = %v", runErr)
	}
	if output.Len() != 0 {
		t.Errorf("stderr = %q, want empty on clean cancellation", output.String())
	}
	metricsPath := filepath.Join(configDir, "private", "metrics", "state.json")
	if info, err := os.Stat(metricsPath); err != nil {
		t.Errorf("metrics state missing after run: %v", err)
	} else if info.Mode().Perm() != 0o600 {
		t.Errorf("metrics state mode = %o, want 600", info.Mode().Perm())
	}
}

func TestRunDoesNotConstructProviderForInvalidConfigOrWorkRoot(t *testing.T) {
	tests := []struct {
		name   string
		config func(t *testing.T) string
	}{
		{
			name: "invalid config",
			config: func(t *testing.T) string {
				return writeAPIConfig(t, strings.Replace(validAPIConfig(), `"ocr_timeout":"8m"`, `"ocr_timeout":"0s"`, 1))
			},
		},
		{
			name: "work root cannot be created",
			config: func(t *testing.T) string {
				path := writeAPIConfig(t, validAPIConfig())
				blocker := filepath.Join(filepath.Dir(path), "blocked")
				if err := os.WriteFile(blocker, []byte("file"), 0o600); err != nil {
					t.Fatal(err)
				}
				config := decodeFixtureMap(t, validAPIConfig())
				config["ocr"].(map[string]any)["work_root"] = "blocked/work"
				return writeAPIConfigAt(t, path, encodeFixture(t, config))
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var providers, servers int
			var output bytes.Buffer
			err := run(context.Background(), test.config(t), &output,
				func(ocr.Config) (managedRecognizer, error) {
					providers++
					return &fakeManagedRecognizer{}, nil
				},
				func(server.Config, httpapi.Recognizer) (runner, error) {
					servers++
					return &fakeRunner{}, nil
				},
			)
			if err == nil {
				t.Fatal("run() error = nil, want setup error")
			}
			if providers != 0 || servers != 0 {
				t.Fatalf("constructors called provider=%d server=%d before setup succeeded", providers, servers)
			}
			assertSafeOutput(t, output.String(), "blocked", "invalid config")
		})
	}
}

func TestRunProviderConstructionFailureDoesNotStartServer(t *testing.T) {
	configPath := writeAPIConfig(t, validAPIConfig())
	secret := errors.New("secret provider path /private/models/passport")
	var servers int
	var output bytes.Buffer
	err := run(context.Background(), configPath, &output,
		func(ocr.Config) (managedRecognizer, error) { return nil, secret },
		func(server.Config, httpapi.Recognizer) (runner, error) {
			servers++
			return &fakeRunner{}, nil
		},
	)
	if err == nil {
		t.Fatal("run() error = nil, want provider construction failure")
	}
	if servers != 0 {
		t.Fatalf("server factory calls = %d, want zero", servers)
	}
	assertSafeOutput(t, output.String(), "secret provider path", "/private/models")
}

func TestRunClosesProviderAfterServerConstructionFailure(t *testing.T) {
	configPath := writeAPIConfig(t, withConfigEdit(t, validAPIConfig(), func(config map[string]any) {
		config["allowed_client_networks"] = []string{"8.8.8.0/24"}
	}))
	provider := &fakeManagedRecognizer{}
	var output bytes.Buffer
	err := run(context.Background(), configPath, &output,
		func(ocr.Config) (managedRecognizer, error) { return provider, nil },
		func(config server.Config, recognizer httpapi.Recognizer) (runner, error) {
			if recognizer != provider {
				t.Error("server factory did not receive constructed provider")
			}
			return newProductionServer(config, recognizer)
		},
	)
	if err == nil {
		t.Fatal("run() error = nil, want server constructor rejection for public CIDR")
	}
	if provider.closeCalls != 1 {
		t.Fatalf("provider Close calls = %d, want 1", provider.closeCalls)
	}
	if !provider.closeHadDeadline || provider.closeContextWasCancelled {
		t.Errorf("provider Close context deadline=%v cancelled=%v, want independent bounded context", provider.closeHadDeadline, provider.closeContextWasCancelled)
	}
	assertSafeOutput(t, output.String(), "8.8.8.0/24", "provider secret")
}

func TestRunClosesProviderAfterRunReturnsAndUsesIndependentTimeout(t *testing.T) {
	configPath := writeAPIConfig(t, validAPIConfig())
	provider := &fakeManagedRecognizer{}
	runFailure := errors.New("private listener address secret")
	serverDouble := &fakeRunner{runErr: runFailure}
	var output bytes.Buffer
	err := run(context.Background(), configPath, &output,
		func(ocr.Config) (managedRecognizer, error) { return provider, nil },
		func(server.Config, httpapi.Recognizer) (runner, error) { return serverDouble, nil },
	)
	if err == nil {
		t.Fatal("run() error = nil, want server Run failure")
	}
	if serverDouble.runCalls != 1 || provider.closeCalls != 1 {
		t.Fatalf("Run calls=%d, Close calls=%d, want one each", serverDouble.runCalls, provider.closeCalls)
	}
	if provider.closeContextWasCancelled || !provider.closeHadDeadline {
		t.Errorf("provider Close context cancelled=%v deadline=%v, want independent bounded context", provider.closeContextWasCancelled, provider.closeHadDeadline)
	}
	assertSafeOutput(t, output.String(), runFailure.Error(), "listener address", "secret")
}

func TestRunReportsCloseFailureWithoutRawError(t *testing.T) {
	configPath := writeAPIConfig(t, validAPIConfig())
	closeFailure := errors.New("private workspace /sensitive/image-and-name")
	provider := &fakeManagedRecognizer{closeErr: closeFailure}
	var output bytes.Buffer
	err := run(context.Background(), configPath, &output,
		func(ocr.Config) (managedRecognizer, error) { return provider, nil },
		func(server.Config, httpapi.Recognizer) (runner, error) { return &fakeRunner{}, nil },
	)
	if err == nil {
		t.Fatal("run() error = nil, want provider cleanup failure")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "cleanup") && !strings.Contains(strings.ToLower(err.Error()), "close") {
		t.Errorf("returned error %q does not classify cleanup failure", err)
	}
	assertSafeOutput(t, output.String(), closeFailure.Error(), "/sensitive", "image-and-name")
}

func TestRunClosesProviderWhenCallerContextIsCancelled(t *testing.T) {
	configPath := writeAPIConfig(t, validAPIConfig())
	provider := &fakeManagedRecognizer{}
	ctx, cancel := context.WithCancel(context.Background())
	serverDouble := &fakeRunner{run: func(context.Context) error {
		cancel()
		return nil
	}}
	err := run(ctx, configPath, &bytes.Buffer{},
		func(ocr.Config) (managedRecognizer, error) { return provider, nil },
		func(server.Config, httpapi.Recognizer) (runner, error) { return serverDouble, nil },
	)
	if err != nil {
		t.Fatalf("run() error = %v, want nil after graceful cancellation", err)
	}
	if provider.closeCalls != 1 || provider.closeContextWasCancelled || !provider.closeHadDeadline {
		t.Fatalf("Close calls=%d cancelled=%v deadline=%v; want one independent bounded cleanup", provider.closeCalls, provider.closeContextWasCancelled, provider.closeHadDeadline)
	}
}

func TestRunRequiresProductionServerFactoryToValidateLANConfig(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"empty client CIDR list", func(config map[string]any) { config["allowed_client_networks"] = []string{} }},
		{"public CIDR", func(config map[string]any) { config["allowed_client_networks"] = []string{"0.0.0.0/0"} }},
		{"public bind", func(config map[string]any) { config["listen_address"] = "0.0.0.0:8765" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			configPath := writeAPIConfig(t, withConfigEdit(t, validAPIConfig(), test.edit))
			provider := &fakeManagedRecognizer{}
			err := run(context.Background(), configPath, &bytes.Buffer{},
				func(ocr.Config) (managedRecognizer, error) { return provider, nil },
				func(config server.Config, recognizer httpapi.Recognizer) (runner, error) {
					return newProductionServer(config, recognizer)
				},
			)
			if err == nil {
				t.Fatal("run() error = nil, want server.New LAN validation error")
			}
			if provider.closeCalls != 1 {
				t.Fatalf("provider Close calls=%d, want close once after server.New rejection", provider.closeCalls)
			}
		})
	}
}

func TestRunPropagatesLoggerAndWritesSafeStructuredEvents(t *testing.T) {
	configPath := writeAPIConfig(t, validAPIConfig())
	provider := &fakeManagedRecognizer{}
	var output bytes.Buffer
	var providerLogger, serverLogger, httpLogger bool
	err := run(context.Background(), configPath, &output,
		func(config ocr.Config) (managedRecognizer, error) {
			providerLogger = config.Logger != nil
			if config.Logger != nil {
				config.Logger.Info("ocr.job.started", "event", "ocr.job.started", "request_id", "94B86c5c-2520-4c0f-b91e-6d4a948fc88a", "outcome", "started")
			}
			return provider, nil
		},
		func(config server.Config, _ httpapi.Recognizer) (runner, error) {
			serverLogger = config.Logger != nil
			httpLogger = config.HTTP.Logger != nil
			if config.Logger != nil {
				config.Logger.With("component", "server").Info("service.started", "event", "service.started")
			}
			return &fakeRunner{}, nil
		},
	)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if !providerLogger || !serverLogger || !httpLogger {
		t.Fatalf("logger propagation provider/server/http=%v/%v/%v", providerLogger, serverLogger, httpLogger)
	}
	if output.Len() != 0 {
		t.Fatalf("stderr = %q, want no fallback for healthy logging", output.String())
	}
	logPath := filepath.Join(filepath.Dir(configPath), "private", "logs", "api.jsonl")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("invalid JSON event: %v", err)
		}
		if record["service"] != "passport-api" || record["time"] == nil || record["event"] != record["msg"] {
			t.Fatalf("invalid event schema: %#v", record)
		}
		events = append(events, record["event"].(string))
	}
	if strings.Join(events, ",") != "ocr.job.started,service.started" {
		t.Fatalf("events = %v", events)
	}
}

func TestRunFailsClosedAndWritesSafeJSONWhenLogTargetIsUnsafe(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(root, "private")
	logs := filepath.Join(private, "logs")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(private, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	config := decodeFixtureMap(t, validAPIConfig())
	config["logging"].(map[string]any)["path"] = "private/logs/link.jsonl"
	configPath := filepath.Join(root, "api.json")
	writeAPIConfigAt(t, configPath, encodeFixture(t, config))
	if err := os.Symlink(filepath.Join(logs, "target"), filepath.Join(logs, "link.jsonl")); err != nil {
		t.Fatal(err)
	}
	var providers int
	var output bytes.Buffer
	err = run(context.Background(), configPath, &output,
		func(ocr.Config) (managedRecognizer, error) { providers++; return &fakeManagedRecognizer{}, nil },
		func(server.Config, httpapi.Recognizer) (runner, error) { return &fakeRunner{}, nil },
	)
	if err == nil || providers != 0 {
		t.Fatalf("run error=%v provider constructions=%d; want fail closed before provider", err, providers)
	}
	var fallback map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &fallback); err != nil {
		t.Fatalf("fallback is not JSON: %v (%q)", err, output.String())
	}
	if fallback["service"] != "passport-api" || fallback["component"] != "startup" || fallback["event"] != "service.failed" || fallback["error_code"] != "startup_failed" || strings.Contains(output.String(), "link.jsonl") {
		t.Fatalf("unsafe startup fallback: %#v", fallback)
	}
}

func withConfigEdit(t *testing.T, contents string, edit func(map[string]any)) string {
	t.Helper()
	config := decodeFixtureMap(t, contents)
	edit(config)
	return encodeFixture(t, config)
}

func writeAPIConfigAt(t *testing.T, path, contents string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertSafeOutput(t *testing.T, output string, forbidden ...string) {
	t.Helper()
	for _, secret := range forbidden {
		if secret != "" && strings.Contains(output, secret) {
			t.Errorf("output %q exposed sensitive detail %q", output, secret)
		}
	}
}

type fakeManagedRecognizer struct {
	closeCalls               int
	closeHadDeadline         bool
	closeContextWasCancelled bool
	closeErr                 error
}

func (f *fakeManagedRecognizer) Recognize(context.Context, httpapi.Image) (passport.Extraction, error) {
	return passport.Extraction{}, nil
}

func (f *fakeManagedRecognizer) Close(ctx context.Context) error {
	f.closeCalls++
	_, f.closeHadDeadline = ctx.Deadline()
	f.closeContextWasCancelled = ctx.Err() != nil
	return f.closeErr
}

type fakeRunner struct {
	runCalls int
	runErr   error
	run      func(context.Context) error
}

func (f *fakeRunner) Run(ctx context.Context) error {
	f.runCalls++
	if f.run != nil {
		return f.run(ctx)
	}
	return f.runErr
}

var _ managedRecognizer = (*fakeManagedRecognizer)(nil)
var _ runner = (*fakeRunner)(nil)
