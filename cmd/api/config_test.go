package main

import (
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"passport-backend/internal/ocr"
)

func TestLoadConfigMapsExplicitValuesAndResolvesRelativePaths(t *testing.T) {
	configPath := writeAPIConfig(t, validAPIConfig())
	configDir := filepath.Dir(configPath)
	serverConfig, ocrConfig, metricsConfig, err := loadRuntimeConfig(configPath)
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}

	if serverConfig.Address != "127.0.0.1:8765" {
		t.Errorf("Address = %q, want loopback example", serverConfig.Address)
	}
	if got := serverConfig.HTTP.AllowedClientNetworks; len(got) != 1 || got[0] != netip.MustParsePrefix("127.0.0.0/8") {
		t.Errorf("AllowedClientNetworks = %v, want [127.0.0.0/8]", got)
	}
	if serverConfig.HTTP.MaxUploadBytes != 4<<20 || ocrConfig.MaxInputBytes != serverConfig.HTTP.MaxUploadBytes {
		t.Errorf("upload/input limits = %d/%d, want shared 4194304", serverConfig.HTTP.MaxUploadBytes, ocrConfig.MaxInputBytes)
	}
	if serverConfig.HTTP.PipelineVersion != "russiandocs-local-example-v1" || ocrConfig.PipelineVersion != serverConfig.HTTP.PipelineVersion {
		t.Errorf("pipeline versions = %q/%q, want one shared value", serverConfig.HTTP.PipelineVersion, ocrConfig.PipelineVersion)
	}
	if metricsConfig.Path != filepath.Join(configDir, "private", "metrics", "state.json") || metricsConfig.PipelineVersion != ocrConfig.PipelineVersion {
		t.Errorf("metrics config = %+v, want config-relative state path and shared pipeline version", metricsConfig)
	}
	if serverConfig.HTTP.OCRTimeout != 8*time.Minute || ocrConfig.StopGrace != 15*time.Second || serverConfig.ShutdownTimeout != 30*time.Second {
		t.Errorf("timeouts = OCR %s, stop grace %s, shutdown %s", serverConfig.HTTP.OCRTimeout, ocrConfig.StopGrace, serverConfig.ShutdownTimeout)
	}
	if serverConfig.HTTP.MaxConcurrent != 2 || serverConfig.MaxHeaderBytes != 32<<10 || ocrConfig.MaxOutputBytes != 1<<20 || ocrConfig.MaxImagePixels != 24_000_000 {
		t.Errorf("configured limits were not mapped: server=%+v OCR=%+v", serverConfig, ocrConfig)
	}
	if ocrConfig.StartupTimeout != 90*time.Second {
		t.Errorf("StartupTimeout = %s, want explicit 90s", ocrConfig.StartupTimeout)
	}
	wantLogPath := filepath.Join(configDir, "private", "logs", "api.jsonl")
	if serverConfig.Logging.Path != wantLogPath || serverConfig.Logging.Level != "info" || serverConfig.Logging.MaxSizeBytes != 5<<20 || serverConfig.Logging.MaxBackups != 3 {
		t.Errorf("Logging = %+v, want explicit bounded JSON log config", serverConfig.Logging)
	}

	base := filepath.Dir(configPath)
	wantPaths := map[string]string{
		"RussianPython": filepath.Join(base, "venv-russian", "bin", "python"),
		"RussianWorker": filepath.Join(base, "ocr", "runtime", "russiandocs_worker.py"),
		"RussianRepo":   filepath.Join(base, "vendor", "RussianDocsOCR"),
		"RussianModels": filepath.Join(base, "private", "models"),
		"WorkRoot":      filepath.Join(base, "work"),
	}
	gotPaths := map[string]string{
		"RussianPython": ocrConfig.RussianPython, "RussianWorker": ocrConfig.RussianWorker,
		"RussianRepo": ocrConfig.RussianRepo, "RussianModels": ocrConfig.RussianModels,
		"WorkRoot": ocrConfig.WorkRoot,
	}
	for name, want := range wantPaths {
		if gotPaths[name] != want {
			t.Errorf("%s = %q, want config-relative absolute path %q", name, gotPaths[name], want)
		}
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("test config disappeared: %v", err)
	}
}

func TestLoadLocalExampleConfigHasExplicitLoopbackSettings(t *testing.T) {
	_, sourcePath, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() did not return test source path")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(sourcePath), "..", ".."))
	configPath := filepath.Join(repoRoot, "deploy", "api.local.example.json")
	serverConfig, providerConfig, metricsConfig, err := loadRuntimeConfig(configPath)
	if err != nil {
		t.Fatalf("load example config: %v", err)
	}
	if serverConfig.Address != "127.0.0.1:8080" {
		t.Errorf("Address = %q, want loopback example", serverConfig.Address)
	}
	if metricsConfig.Path != filepath.Join(repoRoot, ".local/ocr-api/metrics/state.json") || metricsConfig.PipelineVersion != providerConfig.PipelineVersion {
		t.Errorf("example metrics config = %+v", metricsConfig)
	}
	if got := serverConfig.HTTP.AllowedClientNetworks; len(got) != 1 || got[0] != netip.MustParsePrefix("127.0.0.0/8") {
		t.Errorf("AllowedClientNetworks = %v, want explicit loopback CIDR", got)
	}
	if serverConfig.HTTP.MaxUploadBytes != 8<<20 || providerConfig.MaxInputBytes != 8<<20 {
		t.Errorf("upload/input limits = %d/%d, want 8 MiB", serverConfig.HTTP.MaxUploadBytes, providerConfig.MaxInputBytes)
	}
	if serverConfig.HTTP.MaxConcurrent != 1 || providerConfig.MaxOutputBytes != 2<<20 || providerConfig.MaxImagePixels != 40_000_000 {
		t.Errorf("example limits = server %+v, provider %+v", serverConfig.HTTP, providerConfig)
	}
	if providerConfig.StartupTimeout <= 0 || providerConfig.RussianPython == "" || providerConfig.RussianWorker == "" || providerConfig.RussianRepo == "" || providerConfig.RussianModels == "" {
		t.Errorf("example RussianDocsOCR paths/timeout are incomplete: %+v", providerConfig)
	}
	if strings.Contains(providerConfig.PipelineVersion, "auto-vl") {
		t.Errorf("pipeline version still names the removed provider: %q", providerConfig.PipelineVersion)
	}
	if serverConfig.WriteTimeout <= serverConfig.ReadTimeout+serverConfig.HTTP.OCRTimeout ||
		serverConfig.ShutdownTimeout <= providerConfig.StopGrace || serverConfig.HTTP.OCRTimeout <= providerConfig.StopGrace {
		t.Error("example timeouts do not leave the configured cleanup margins")
	}
}

func TestLoadQueueExampleConfigUsesGoOnlyRuntimeFields(t *testing.T) {
	_, sourcePath, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() did not return test source path")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(sourcePath), "..", ".."))
	path := filepath.Join(repoRoot, "deploy", "api.queue.example.json")
	serverConfig, ocrConfig, _, runtimeConfig, err := loadFullRuntimeConfig(path)
	if err != nil {
		t.Fatalf("load queue example: %v", err)
	}
	if serverConfig.Address != "172.30.0.10:8080" || serverConfig.HTTP.AsyncJobs != nil ||
		runtimeConfig == nil || !runtimeConfig.Enabled || runtimeConfig.SyncOCREnabled || runtimeConfig.QueueProcessing == nil {
		t.Fatalf("queue example config did not map to Go-only queue mode: server=%+v runtime=%+v", serverConfig, runtimeConfig)
	}
	if ocrConfig.RussianPython != "" || ocrConfig.RussianRepo != "" || ocrConfig.WorkRoot != "" {
		t.Fatalf("queue example unexpectedly configures Python in API: %+v", ocrConfig)
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed := decodeFixtureMap(t, string(contents))
	parsed["pipeline_version"] = "integration-stage4-controlled"
	queue := parsed["queue_processing"].(map[string]any)
	queue["connect_timeout"] = "3s"
	queue["poll_interval"] = "50ms"
	queue["publisher_lease"] = "2s"
	queue["publish_timeout"] = "500ms"
	queue["publish_backoff"] = "100ms"
	queue["reconnect_min"] = "100ms"
	queue["reconnect_max"] = "500ms"
	queue["consumer_ack_timeout"] = "5m"
	queue["max_attempts"] = float64(2)
	queue["retry_initial_delay"] = "100ms"
	queue["retry_max_delay"] = "500ms"
	queue["hard_processing_timeout"] = "5s"
	queue["completion_budget"] = "2s"
	queue["attempt_lease"] = "15s"
	queue["recovery_interval"] = "1s"
	parsed["async_jobs"].(map[string]any)["connect_timeout"] = "5s"
	parsed["async_jobs"].(map[string]any)["migration_lock_timeout"] = "10s"
	parsed["async_jobs"].(map[string]any)["max_stored_image_bytes"] = float64(1 << 20)
	parsed["async_jobs"].(map[string]any)["retention"] = map[string]any{
		"claim_lease": "30s", "interval": "1s", "operation_timeout": "5s", "batch_size": float64(100),
	}
	if _, _, _, _, err := loadFullRuntimeConfig(writeAPIConfig(t, encodeFixture(t, parsed))); err != nil {
		t.Fatalf("load queue example with integration timing policy: %v", err)
	}
}

func TestLoadFullRuntimeConfigAcceptsOptionalAsyncJobsBlock(t *testing.T) {
	parsed := decodeFixtureMap(t, validAPIConfig())
	parsed["async_jobs"] = map[string]any{
		"credentials_file":       "private/async/credentials.json",
		"database_dsn_file":      "private/async/database-dsn.txt",
		"connect_timeout":        "5s",
		"migration_lock_timeout": "10s",
		"image_root":             "private/images",
		"max_stored_image_bytes": float64(1 << 30),
		"max_concurrent_uploads": float64(4),
		"retention":              map[string]any{"claim_lease": "30s", "interval": "1s", "operation_timeout": "5s", "batch_size": float64(100)},
	}
	configPath := writeAPIConfig(t, encodeFixture(t, parsed))
	serverConfig, _, _, asyncConfig, err := loadFullRuntimeConfig(configPath)
	if err != nil {
		t.Fatalf("loadFullRuntimeConfig() error = %v", err)
	}
	base := filepath.Dir(configPath)
	if asyncConfig == nil || asyncConfig.CredentialsFile != filepath.Join(base, "private/async/credentials.json") ||
		asyncConfig.DatabaseDSNFile != filepath.Join(base, "private/async/database-dsn.txt") ||
		asyncConfig.ImageRoot != filepath.Join(base, "private/images") || asyncConfig.ConnectTimeout != 5*time.Second ||
		asyncConfig.MigrationLockTimeout != 10*time.Second || asyncConfig.MaxStoredImageBytes != 1<<30 || asyncConfig.MaxConcurrentUploads != 4 ||
		asyncConfig.Retention.ClaimLease != 30*time.Second || asyncConfig.Retention.Interval != time.Second || asyncConfig.Retention.OperationTimeout != 5*time.Second || asyncConfig.Retention.BatchSize != 100 {
		t.Fatalf("async config = %+v", asyncConfig)
	}
	if serverConfig.HTTP.AsyncJobs != nil {
		t.Fatal("loader must leave async dependencies unconstructed")
	}
}

func TestLoadFullRuntimeConfigAllowsEnrollmentWithoutStaticClientRegistry(t *testing.T) {
	parsed := decodeFixtureMap(t, validAPIConfig())
	parsed["async_jobs"] = map[string]any{
		"database_dsn_file":      "private/async/database-dsn.txt",
		"connect_timeout":        "5s",
		"migration_lock_timeout": "10s",
		"image_root":             "private/images",
		"max_stored_image_bytes": float64(1 << 30),
		"max_concurrent_uploads": float64(4),
		"retention":              map[string]any{"claim_lease": "30s", "interval": "1s", "operation_timeout": "5s", "batch_size": float64(100)},
		"client_enrollment": map[string]any{
			"max_clients":                float64(1000),
			"max_new_clients_per_minute": float64(25),
		},
	}
	configPath := writeAPIConfig(t, encodeFixture(t, parsed))
	_, _, _, asyncConfig, err := loadFullRuntimeConfig(configPath)
	if err != nil {
		t.Fatalf("loadFullRuntimeConfig() error = %v", err)
	}
	if asyncConfig.CredentialsFile != "" || asyncConfig.ClientEnrollment == nil ||
		asyncConfig.ClientEnrollment.MaxClients != 1000 || asyncConfig.ClientEnrollment.MaxNewClientsPerMinute != 25 {
		t.Fatalf("async enrollment config = %+v", asyncConfig)
	}
	parsed["async_jobs"].(map[string]any)["client_enrollment"].(map[string]any)["max_new_clients_per_minute"] = float64(0)
	if _, _, _, _, err := loadFullRuntimeConfig(writeAPIConfig(t, encodeFixture(t, parsed))); err == nil {
		t.Fatal("loadFullRuntimeConfig() accepted a non-positive enrollment rate cap")
	}
}

func TestLoadFullRuntimeConfigSupportsQueueModeWithoutPythonConfiguration(t *testing.T) {
	parsed := decodeFixtureMap(t, validAPIConfig())
	delete(parsed, "ocr")
	delete(parsed, "process_stop_grace")
	delete(parsed, "max_output_bytes")
	delete(parsed, "max_image_pixels")
	parsed["sync_ocr_enabled"] = false
	parsed["async_jobs"] = validAsyncJobsFixture()
	parsed["queue_processing"] = validQueueProcessingFixture()
	configPath := writeAPIConfig(t, encodeFixture(t, parsed))
	serverConfig, ocrConfig, _, asyncConfig, err := loadFullRuntimeConfig(configPath)
	if err != nil {
		t.Fatalf("loadFullRuntimeConfig() error = %v", err)
	}
	if asyncConfig == nil || !asyncConfig.Enabled || asyncConfig.SyncOCREnabled || asyncConfig.QueueProcessing == nil {
		t.Fatalf("async runtime config = %+v, want queue enabled and sync OCR disabled", asyncConfig)
	}
	if ocrConfig.RussianPython != "" || ocrConfig.RussianRepo != "" || ocrConfig.WorkRoot != "" || serverConfig.HTTP.OCRTimeout == 0 {
		t.Fatalf("disabled Python config should not be loaded: OCR=%+v HTTP=%+v", ocrConfig, serverConfig.HTTP)
	}
	queue := asyncConfig.QueueProcessing
	base := filepath.Dir(configPath)
	if queue.WorkerCredentialsFile != filepath.Join(base, "private/queue/workers.json") ||
		queue.BrokerDSNFile != filepath.Join(base, "private/queue/broker-dsn.txt") ||
		queue.PrivateListenerAddress != "127.0.0.1:8766" || queue.ConsumerAckTimeout != 22*time.Minute ||
		queue.AttemptLease != 15*time.Minute || asyncConfig.PipelineVersion != "russiandocs-local-example-v1" {
		t.Fatalf("queue runtime config = %+v", queue)
	}
}

func TestLoadFullRuntimeConfigRejectsQueueTimingAndPathDrift(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(map[string]any, map[string]any)
	}{
		{"lease does not exceed processing plus completion", func(_ map[string]any, queue map[string]any) { queue["attempt_lease"] = "12m" }},
		{"ack timeout does not exceed lease plus completion", func(_ map[string]any, queue map[string]any) { queue["consumer_ack_timeout"] = "17m" }},
		{"broker secret overlaps client registry", func(async, queue map[string]any) { queue["broker_dsn_file"] = async["credentials_file"] }},
		{"private listener duplicates public bind", func(_ map[string]any, queue map[string]any) { queue["private_listener_address"] = "127.0.0.1:8765" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			parsed := decodeFixtureMap(t, validAPIConfig())
			async := validAsyncJobsFixture()
			queue := validQueueProcessingFixture()
			test.edit(async, queue)
			parsed["async_jobs"] = async
			parsed["queue_processing"] = queue
			if _, _, _, _, err := loadFullRuntimeConfig(writeAPIConfig(t, encodeFixture(t, parsed))); err == nil {
				t.Fatal("loadFullRuntimeConfig() accepted invalid queue processing configuration")
			}
		})
	}
}

func validAsyncJobsFixture() map[string]any {
	return map[string]any{
		"credentials_file": "private/async/credentials.json", "database_dsn_file": "private/async/database-dsn.txt",
		"connect_timeout": "5s", "migration_lock_timeout": "10s", "image_root": "private/images",
		"max_stored_image_bytes": float64(1 << 30), "max_concurrent_uploads": float64(4),
		"retention": map[string]any{"claim_lease": "30s", "interval": "1s", "operation_timeout": "5s", "batch_size": float64(100)},
	}
}

func validQueueProcessingFixture() map[string]any {
	return map[string]any{
		"worker_credentials_file": "private/queue/workers.json", "private_listener_address": "127.0.0.1:8766",
		"broker_dsn_file": "private/queue/broker-dsn.txt", "connect_timeout": "5s", "poll_interval": "1s",
		"publisher_lease": "30s", "publish_timeout": "5s", "publish_backoff": "1s",
		"reconnect_min": "1s", "reconnect_max": "30s", "consumer_ack_timeout": "22m",
		"max_attempts": float64(5), "retry_initial_delay": "1s", "retry_max_delay": "1m",
		"hard_processing_timeout": "10m", "completion_budget": "2m", "attempt_lease": "15m",
		"recovery_interval": "10s",
	}
}

func TestLoadFullRuntimeConfigRejectsInvalidAsyncLimitAndOverlappingPaths(t *testing.T) {
	for _, test := range []struct {
		name   string
		modify func(map[string]any)
	}{
		{name: "zero total bytes", modify: func(async map[string]any) { async["max_stored_image_bytes"] = float64(0) }},
		{name: "zero concurrency", modify: func(async map[string]any) { async["max_concurrent_uploads"] = float64(0) }},
		{name: "image root overlaps credentials", modify: func(async map[string]any) { async["image_root"] = "private/async" }},
		{name: "retention interval exceeds five seconds", modify: func(async map[string]any) { async["retention"].(map[string]any)["interval"] = "6s" }},
		{name: "retention operation outlives claim lease", modify: func(async map[string]any) { async["retention"].(map[string]any)["operation_timeout"] = "30s" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			parsed := decodeFixtureMap(t, validAPIConfig())
			async := map[string]any{
				"credentials_file": "private/async/credentials.json", "database_dsn_file": "private/async/database-dsn.txt",
				"connect_timeout": "5s", "migration_lock_timeout": "10s", "image_root": "private/images",
				"max_stored_image_bytes": float64(1 << 30), "max_concurrent_uploads": float64(4),
				"retention": map[string]any{"claim_lease": "30s", "interval": "1s", "operation_timeout": "5s", "batch_size": float64(100)},
			}
			test.modify(async)
			parsed["async_jobs"] = async
			if _, _, _, _, err := loadFullRuntimeConfig(writeAPIConfig(t, encodeFixture(t, parsed))); err == nil {
				t.Fatal("loadFullRuntimeConfig() accepted invalid async jobs configuration")
			}
		})
	}
}

func TestLoadConfigRejectsMalformedOrNonStrictJSON(t *testing.T) {
	base := validAPIConfig()
	tests := []struct {
		name string
		json string
	}{
		{name: "unknown top-level key", json: strings.TrimSuffix(base, "}") + `,"unexpected":true}`},
		{name: "unknown nested key", json: strings.Replace(base, `"ocr":{`, `"ocr":{"unexpected":true,`, 1)},
		{name: "duplicate top-level key", json: strings.Replace(base, `"listen_address":`, `"listen_address":"127.0.0.1:8765","listen_address":`, 1)},
		{name: "duplicate nested key", json: strings.Replace(base, `"russian_docs_python":`, `"russian_docs_python":"first","russian_docs_python":`, 1)},
		{name: "trailing JSON value", json: base + ` {}`},
		{name: "malformed JSON", json: `{"listen_address":`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := loadConfig(writeAPIConfig(t, test.json)); err == nil {
				t.Fatal("loadConfig() error = nil, want rejection")
			}
		})
	}
}

func TestLoadRuntimeConfigRejectsFilesAboveSizeLimit(t *testing.T) {
	configPath := writeAPIConfig(t, strings.Repeat(" ", maxConfigBytes+1))
	_, _, _, err := loadRuntimeConfig(configPath)
	if err == nil || err.Error() != "invalid config file size" {
		t.Fatalf("loadRuntimeConfig() error = %v, want oversized file rejection", err)
	}
}

func TestLoadRuntimeConfigAcceptsFileAtSizeLimit(t *testing.T) {
	contents := validAPIConfig()
	contents += strings.Repeat(" ", maxConfigBytes-len(contents))
	if len(contents) != maxConfigBytes {
		t.Fatalf("config fixture size = %d, want %d", len(contents), maxConfigBytes)
	}
	if _, _, _, err := loadRuntimeConfig(writeAPIConfig(t, contents)); err != nil {
		t.Fatalf("loadRuntimeConfig() rejected config at size limit: %v", err)
	}
}

func TestLoadRuntimeConfigAcceptsInclusiveLimitBoundaries(t *testing.T) {
	const maxUploadBytes = int64(^uint64(0)>>1) - (64 << 10)
	tests := []struct {
		name  string
		field string
		value any
	}{
		{name: "minimum upload bytes", field: "max_upload_bytes", value: int64(1)},
		{name: "maximum upload bytes", field: "max_upload_bytes", value: maxUploadBytes},
		{name: "minimum header bytes", field: "max_header_bytes", value: 1},
		{name: "minimum output bytes", field: "max_output_bytes", value: 1},
		{name: "minimum image pixels", field: "max_image_pixels", value: int64(1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := decodeFixtureMap(t, validAPIConfig())
			config[test.field] = test.value
			if _, _, _, err := loadRuntimeConfig(writeAPIConfig(t, encodeFixture(t, config))); err != nil {
				t.Fatalf("loadRuntimeConfig() rejected inclusive boundary: %v", err)
			}
		})
	}
}

func TestLoadRuntimeConfigRejectsUploadBytesAboveInclusiveLimit(t *testing.T) {
	const maxUploadBytes = int64(^uint64(0)>>1) - (64 << 10)
	config := decodeFixtureMap(t, validAPIConfig())
	config["max_upload_bytes"] = maxUploadBytes + 1
	if _, _, _, err := loadRuntimeConfig(writeAPIConfig(t, encodeFixture(t, config))); err == nil || err.Error() != "invalid config field: max_upload_bytes" {
		t.Fatalf("loadRuntimeConfig() error = %v, want upload limit rejection", err)
	}
}

func TestLoadRuntimeConfigAcceptsInclusiveLoggingBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		maxSize    int64
		maxBackups int
	}{
		{name: "minimum size", maxSize: 8192, maxBackups: 3},
		{name: "zero backups", maxSize: 5 << 20, maxBackups: 0},
		{name: "maximum backups", maxSize: 5 << 20, maxBackups: 10},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := decodeFixtureMap(t, validAPIConfig())
			logging := config["logging"].(map[string]any)
			logging["max_size_bytes"] = test.maxSize
			logging["max_backups"] = test.maxBackups
			if _, _, _, err := loadRuntimeConfig(writeAPIConfig(t, encodeFixture(t, config))); err != nil {
				t.Fatalf("loadRuntimeConfig() rejected inclusive logging boundary: %v", err)
			}
		})
	}
}

func TestLoadRuntimeConfigAcceptsReadHeaderTimeoutEqualToReadTimeout(t *testing.T) {
	config := decodeFixtureMap(t, validAPIConfig())
	config["read_header_timeout"] = "30s"
	if _, _, _, err := loadRuntimeConfig(writeAPIConfig(t, encodeFixture(t, config))); err != nil {
		t.Fatalf("loadRuntimeConfig() rejected equal read timeouts: %v", err)
	}
}

func TestLoadRuntimeConfigAcceptsOneNanosecondStartupTimeout(t *testing.T) {
	config := decodeFixtureMap(t, validAPIConfig())
	config["ocr"].(map[string]any)["russian_docs_startup_timeout"] = "1ns"
	_, providerConfig, _, err := loadRuntimeConfig(writeAPIConfig(t, encodeFixture(t, config)))
	if err != nil {
		t.Fatalf("loadRuntimeConfig() rejected positive startup timeout: %v", err)
	}
	if providerConfig.StartupTimeout != time.Nanosecond {
		t.Errorf("StartupTimeout = %s, want %s", providerConfig.StartupTimeout, time.Nanosecond)
	}
}

func TestLoadRuntimeConfigRejectsWriteTimeoutAtExactOCRMargin(t *testing.T) {
	config := decodeFixtureMap(t, validAPIConfig())
	config["write_timeout"] = "8m30s"
	if _, _, _, err := loadRuntimeConfig(writeAPIConfig(t, encodeFixture(t, config))); err == nil || err.Error() != "invalid config timing: write_timeout must exceed read_timeout plus ocr_timeout" {
		t.Fatalf("loadRuntimeConfig() error = %v, want exact OCR margin rejection", err)
	}
}

func TestLoadConfigRequiresEveryExplicitSetting(t *testing.T) {
	base := validAPIConfig()
	missing := []struct {
		name string
		key  string
	}{
		{"listen address", `"listen_address":"127.0.0.1:8765",`},
		{"allowed networks", `"allowed_client_networks":["127.0.0.0/8"],`},
		{"upload limit", `"max_upload_bytes":4194304,`},
		{"concurrency", `"max_concurrent":2,`},
		{"OCR timeout", `"ocr_timeout":"8m",`},
		{"pipeline version", `"pipeline_version":"russiandocs-local-example-v1",`},
		{"read header timeout", `"read_header_timeout":"5s",`},
		{"read timeout", `"read_timeout":"30s",`},
		{"write timeout", `"write_timeout":"10m",`},
		{"idle timeout", `"idle_timeout":"60s",`},
		{"shutdown timeout", `"shutdown_timeout":"30s",`},
		{"maximum header bytes", `"max_header_bytes":32768,`},
		{"stop grace", `"process_stop_grace":"15s",`},
		{"max output", `"max_output_bytes":1048576,`},
		{"max pixels", `"max_image_pixels":24000000,`},
		{"logging configuration", `"logging":{"path":"private/logs/api.jsonl","level":"info","max_size_bytes":5242880,"max_backups":3},`},
		{"metrics state path", `"metrics":{"state_path":"private/metrics/state.json"},`},
		{"RussianDocsOCR Python", `"russian_docs_python":"venv-russian/bin/python",`},
		{"RussianDocsOCR worker", `"russian_docs_worker":"ocr/runtime/russiandocs_worker.py",`},
		{"RussianDocsOCR repository", `"russian_docs_repo":"vendor/RussianDocsOCR",`},
		{"RussianDocsOCR models", `"russian_docs_models_root":"private/models",`},
		{"RussianDocsOCR startup timeout", `,"russian_docs_startup_timeout":"90s"`},
		{"nested work root", `"work_root":"work",`},
	}
	for _, field := range missing {
		t.Run(field.name, func(t *testing.T) {
			mutated := strings.Replace(base, field.key, "", 1)
			if mutated == base {
				t.Fatalf("fixture did not contain field marker %q", field.key)
			}
			if !json.Valid([]byte(mutated)) {
				t.Fatal("mutated config is invalid JSON; missing-setting check would be inconclusive")
			}
			if _, _, err := loadConfig(writeAPIConfig(t, mutated)); err == nil {
				t.Fatal("loadConfig() error = nil, want missing explicit setting rejected")
			} else if field.name == "nested work root" && !strings.Contains(err.Error(), "ocr.work_root") {
				t.Fatalf("missing work_root error = %q, want field-specific error", err)
			}
		})
	}
}

func TestLoadConfigRejectsInvalidDurationsAndLimits(t *testing.T) {
	tests := []struct {
		name string
		edit func(map[string]any)
	}{
		{"duration must be a string", func(c map[string]any) { c["ocr_timeout"] = 480 }},
		{"invalid duration", func(c map[string]any) { c["ocr_timeout"] = "later" }},
		{"nonpositive OCR timeout", func(c map[string]any) { c["ocr_timeout"] = "0s" }},
		{"nonpositive read header timeout", func(c map[string]any) { c["read_header_timeout"] = "0s" }},
		{"nonpositive read timeout", func(c map[string]any) { c["read_timeout"] = "0s" }},
		{"nonpositive write timeout", func(c map[string]any) { c["write_timeout"] = "0s" }},
		{"nonpositive idle timeout", func(c map[string]any) { c["idle_timeout"] = "0s" }},
		{"nonpositive shutdown timeout", func(c map[string]any) { c["shutdown_timeout"] = "0s" }},
		{"nonpositive header limit", func(c map[string]any) { c["max_header_bytes"] = 0 }},
		{"zero upload limit", func(c map[string]any) { c["max_upload_bytes"] = 0 }},
		{"negative output limit", func(c map[string]any) { c["max_output_bytes"] = -1 }},
		{"zero image pixels", func(c map[string]any) { c["max_image_pixels"] = 0 }},
		{"zero concurrency", func(c map[string]any) { c["max_concurrent"] = 0 }},
		{"nonpositive stop grace", func(c map[string]any) { c["process_stop_grace"] = "0s" }},
		{"OCR timeout does not reserve stop grace", func(c map[string]any) { c["ocr_timeout"] = "15s" }},
		{"shutdown does not exceed stop grace", func(c map[string]any) { c["shutdown_timeout"] = "15s" }},
		{"write timeout leaves no OCR margin", func(c map[string]any) { c["write_timeout"] = "8m20s" }},
		{"logging path required", func(c map[string]any) { c["logging"].(map[string]any)["path"] = " " }},
		{"logging level must be supported", func(c map[string]any) { c["logging"].(map[string]any)["level"] = "trace" }},
		{"logging size must fit one event", func(c map[string]any) { c["logging"].(map[string]any)["max_size_bytes"] = 8191 }},
		{"logging backup count bounded", func(c map[string]any) { c["logging"].(map[string]any)["max_backups"] = 11 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := decodeFixtureMap(t, validAPIConfig())
			test.edit(config)
			if _, _, err := loadConfig(writeAPIConfig(t, encodeFixture(t, config))); err == nil {
				t.Fatal("loadConfig() error = nil, want invalid limit rejected")
			}
		})
	}
}

func TestLoadConfigPreservesValidationErrorPrecedence(t *testing.T) {
	tests := []struct {
		name    string
		edit    func(map[string]any)
		wantErr string
	}{
		{
			name: "required listener before logging",
			edit: func(config map[string]any) {
				config["listen_address"] = " "
				config["logging"].(map[string]any)["level"] = "trace"
			},
			wantErr: "invalid config field: listen_address",
		},
		{
			name: "CIDRs before durations",
			edit: func(config map[string]any) {
				config["allowed_client_networks"] = []string{"local"}
				config["ocr_timeout"] = "later"
			},
			wantErr: "invalid config field: allowed_client_networks",
		},
		{
			name: "durations before OCR paths",
			edit: func(config map[string]any) {
				config["ocr_timeout"] = "later"
				config["ocr"].(map[string]any)["work_root"] = " "
			},
			wantErr: "invalid config field: ocr_timeout",
		},
		{
			name: "OCR paths before metrics path",
			edit: func(config map[string]any) {
				config["ocr"].(map[string]any)["work_root"] = " "
				config["metrics"].(map[string]any)["state_path"] = " "
			},
			wantErr: "invalid config field: ocr.work_root",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := decodeFixtureMap(t, validAPIConfig())
			test.edit(config)
			_, _, err := loadConfig(writeAPIConfig(t, encodeFixture(t, config)))
			if err == nil || err.Error() != test.wantErr {
				t.Fatalf("loadConfig() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestLoadConfigParsesCIDRsAndRequiresCurrentFields(t *testing.T) {
	tests := []struct {
		name string
		edit func(map[string]any)
	}{
		{"malformed CIDR", func(c map[string]any) { c["allowed_client_networks"] = []string{"local"} }},
		{"blank pipeline version", func(c map[string]any) { c["pipeline_version"] = " " }},
		{"legacy engine selector rejected", func(c map[string]any) { c["ocr"].(map[string]any)["engine"] = "paddle_auto_vl" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := decodeFixtureMap(t, validAPIConfig())
			test.edit(config)
			if _, _, err := loadConfig(writeAPIConfig(t, encodeFixture(t, config))); err == nil {
				t.Fatal("loadConfig() error = nil, want config rejected")
			}
		})
	}
}

func TestLoadRuntimeConfigRejectsMetricsPathCollisions(t *testing.T) {
	for _, path := range []string{
		"private/logs/api.jsonl",
		"private/logs/api.jsonl.lock",
		"private/logs/api.jsonl.1",
		"work/metrics/state.json",
	} {
		t.Run(path, func(t *testing.T) {
			config := decodeFixtureMap(t, validAPIConfig())
			config["metrics"].(map[string]any)["state_path"] = path
			if _, _, _, err := loadRuntimeConfig(writeAPIConfig(t, encodeFixture(t, config))); err == nil {
				t.Fatalf("loadRuntimeConfig accepted conflicting metrics path %q", path)
			}
		})
	}
}

func TestLoadRuntimeConfigRejectsMetricsLockAndLogCollision(t *testing.T) {
	config := decodeFixtureMap(t, validAPIConfig())
	config["metrics"].(map[string]any)["state_path"] = "private/metrics/state.json"
	config["logging"].(map[string]any)["path"] = "private/metrics/state.json.lock"
	if _, _, _, err := loadRuntimeConfig(writeAPIConfig(t, encodeFixture(t, config))); err == nil {
		t.Fatal("loadRuntimeConfig accepted logging path that collides with metrics lock")
	}
}

func TestMetricsPathConflictsDoesNotTreatEmptyWorkRootAsCurrentDirectory(t *testing.T) {
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(string(filepath.Separator)); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(previous); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	}()

	statePath := "/var/lib/passport/state/metrics/state.json"
	logPath := "/var/lib/passport/state/logs/api.jsonl"
	if metricsPathConflicts(statePath, logPath, "") {
		t.Fatal("empty OCR work root was interpreted as the process working directory")
	}
	if !metricsPathConflicts(statePath, logPath, "/var/lib/passport/state") {
		t.Fatal("metrics path under the configured OCR work root was not rejected")
	}
	if metricsPathConflicts(statePath, logPath, "/var/lib/passport/ocr-work") {
		t.Fatal("metrics path outside the configured OCR work root was rejected")
	}
}

func validAPIConfig() string {
	return `{"listen_address":"127.0.0.1:8765","allowed_client_networks":["127.0.0.0/8"],"max_upload_bytes":4194304,"max_concurrent":2,"pipeline_version":"russiandocs-local-example-v1","ocr_timeout":"8m","read_header_timeout":"5s","read_timeout":"30s","write_timeout":"10m","idle_timeout":"60s","shutdown_timeout":"30s","max_header_bytes":32768,"process_stop_grace":"15s","max_output_bytes":1048576,"max_image_pixels":24000000,"logging":{"path":"private/logs/api.jsonl","level":"info","max_size_bytes":5242880,"max_backups":3},"metrics":{"state_path":"private/metrics/state.json"},"ocr":{"russian_docs_python":"venv-russian/bin/python","russian_docs_worker":"ocr/runtime/russiandocs_worker.py","russian_docs_repo":"vendor/RussianDocsOCR","russian_docs_models_root":"private/models","work_root":"work","russian_docs_startup_timeout":"90s"}}`
}

func writeAPIConfig(t *testing.T, contents string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "api.json")
	if err := os.MkdirAll(filepath.Join(filepath.Dir(path), "private", "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(filepath.Dir(path), "private"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(filepath.Dir(path), "private", "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	return path
}

func decodeFixtureMap(t *testing.T, contents string) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal([]byte(contents), &result); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return result
}

func encodeFixture(t *testing.T, value map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return string(encoded)
}

// Keep the compile-time dependency explicit: loadConfig must return the
// production provider configuration, not a command-specific substitute.
var _ ocr.Config
