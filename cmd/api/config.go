package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"passport-backend/internal/httpapi"
	"passport-backend/internal/jsonstrict"
	"passport-backend/internal/metrics"
	"passport-backend/internal/ocr"
	"passport-backend/internal/server"
	"passport-backend/internal/telemetry"
)

const maxConfigBytes = 64 << 10

type runtimeConfigFailure struct {
	phase string
	err   error
}

func (e runtimeConfigFailure) Error() string { return e.err.Error() }

func (e runtimeConfigFailure) Unwrap() error { return e.err }

func runtimeConfigFailurePhase(err error) string {
	var failure runtimeConfigFailure
	if errors.As(err, &failure) {
		return failure.phase
	}
	return "config_validation"
}

func withRuntimeConfigFailure(phase string, err error) error {
	if err == nil {
		return nil
	}
	return runtimeConfigFailure{phase: phase, err: err}
}

type fileConfig struct {
	ListenAddress         string                     `json:"listen_address"`
	AllowedClientNetworks []string                   `json:"allowed_client_networks"`
	MaxUploadBytes        int64                      `json:"max_upload_bytes"`
	MaxConcurrent         int                        `json:"max_concurrent"`
	PipelineVersion       string                     `json:"pipeline_version"`
	OCRTimeout            string                     `json:"ocr_timeout"`
	ReadHeaderTimeout     string                     `json:"read_header_timeout"`
	ReadTimeout           string                     `json:"read_timeout"`
	WriteTimeout          string                     `json:"write_timeout"`
	IdleTimeout           string                     `json:"idle_timeout"`
	ShutdownTimeout       string                     `json:"shutdown_timeout"`
	MaxHeaderBytes        int                        `json:"max_header_bytes"`
	ProcessStopGrace      string                     `json:"process_stop_grace"`
	MaxOutputBytes        int64                      `json:"max_output_bytes"`
	MaxImagePixels        int64                      `json:"max_image_pixels"`
	Logging               telemetry.Config           `json:"logging"`
	Metrics               fileMetricsConfig          `json:"metrics"`
	OCR                   fileOCRConfig              `json:"ocr"`
	SyncOCREnabled        *bool                      `json:"sync_ocr_enabled,omitempty"`
	AsyncJobs             *fileAsyncJobsConfig       `json:"async_jobs,omitempty"`
	QueueProcessing       *fileQueueProcessingConfig `json:"queue_processing,omitempty"`
}

type fileAsyncJobsConfig struct {
	CredentialsFile      string                      `json:"credentials_file"`
	DatabaseDSNFile      string                      `json:"database_dsn_file"`
	ConnectTimeout       string                      `json:"connect_timeout"`
	MigrationLockTimeout string                      `json:"migration_lock_timeout"`
	ImageRoot            string                      `json:"image_root"`
	MaxStoredImageBytes  int64                       `json:"max_stored_image_bytes"`
	MaxConcurrentUploads int                         `json:"max_concurrent_uploads"`
	ClientEnrollment     *fileClientEnrollmentConfig `json:"client_enrollment,omitempty"`
	Retention            *fileRetentionConfig        `json:"retention"`
}

type fileRetentionConfig struct {
	ClaimLease       string `json:"claim_lease"`
	Interval         string `json:"interval"`
	OperationTimeout string `json:"operation_timeout"`
	BatchSize        int    `json:"batch_size"`
}

type fileClientEnrollmentConfig struct {
	MaxClients             int `json:"max_clients"`
	MaxNewClientsPerMinute int `json:"max_new_clients_per_minute"`
}

type fileQueueProcessingConfig struct {
	WorkerCredentialsFile  string `json:"worker_credentials_file"`
	PrivateListenerAddress string `json:"private_listener_address"`
	BrokerDSNFile          string `json:"broker_dsn_file"`
	ConnectTimeout         string `json:"connect_timeout"`
	PollInterval           string `json:"poll_interval"`
	PublisherLease         string `json:"publisher_lease"`
	PublishTimeout         string `json:"publish_timeout"`
	PublishBackoff         string `json:"publish_backoff"`
	ReconnectMin           string `json:"reconnect_min"`
	ReconnectMax           string `json:"reconnect_max"`
	ConsumerAckTimeout     string `json:"consumer_ack_timeout"`
	MaxAttempts            int    `json:"max_attempts"`
	RetryInitialDelay      string `json:"retry_initial_delay"`
	RetryMaxDelay          string `json:"retry_max_delay"`
	HardProcessingTimeout  string `json:"hard_processing_timeout"`
	CompletionBudget       string `json:"completion_budget"`
	AttemptLease           string `json:"attempt_lease"`
	RecoveryInterval       string `json:"recovery_interval"`
}

type asyncRuntimeConfig struct {
	Enabled              bool
	SyncOCREnabled       bool
	PipelineVersion      string
	CredentialsFile      string
	DatabaseDSNFile      string
	ConnectTimeout       time.Duration
	MigrationLockTimeout time.Duration
	ImageRoot            string
	MaxStoredImageBytes  int64
	MaxConcurrentUploads int
	ClientEnrollment     *clientEnrollmentRuntimeConfig
	Retention            retentionRuntimeConfig
	QueueProcessing      *queueProcessingRuntimeConfig
}

type retentionRuntimeConfig struct {
	ClaimLease       time.Duration
	Interval         time.Duration
	OperationTimeout time.Duration
	BatchSize        int
}

type clientEnrollmentRuntimeConfig struct {
	MaxClients             int
	MaxNewClientsPerMinute int
}

type queueProcessingRuntimeConfig struct {
	WorkerCredentialsFile  string
	PrivateListenerAddress string
	BrokerDSNFile          string
	ConnectTimeout         time.Duration
	PollInterval           time.Duration
	PublisherLease         time.Duration
	PublishTimeout         time.Duration
	PublishBackoff         time.Duration
	ReconnectMin           time.Duration
	ReconnectMax           time.Duration
	ConsumerAckTimeout     time.Duration
	MaxAttempts            int
	RetryInitialDelay      time.Duration
	RetryMaxDelay          time.Duration
	HardProcessingTimeout  time.Duration
	CompletionBudget       time.Duration
	AttemptLease           time.Duration
	RecoveryInterval       time.Duration
}

type fileMetricsConfig struct {
	StatePath string `json:"state_path"`
}

type fileOCRConfig struct {
	RussianPython  string `json:"russian_docs_python"`
	RussianWorker  string `json:"russian_docs_worker"`
	RussianRepo    string `json:"russian_docs_repo"`
	RussianModels  string `json:"russian_docs_models_root"`
	StartupTimeout string `json:"russian_docs_startup_timeout"`
	WorkRoot       string `json:"work_root"`
}

// loadConfig reads explicit operator configuration. It resolves paths without
// following symlinks so virtual-environment launchers keep their normal path.
func loadConfig(configPath string) (server.Config, ocr.Config, error) {
	serverConfig, ocrConfig, _, err := loadRuntimeConfig(configPath)
	return serverConfig, ocrConfig, err
}

func loadRuntimeConfig(configPath string) (server.Config, ocr.Config, metrics.Config, error) {
	serverConfig, ocrConfig, metricsConfig, _, err := loadFullRuntimeConfig(configPath)
	return serverConfig, ocrConfig, metricsConfig, err
}

func loadFullRuntimeConfig(configPath string) (server.Config, ocr.Config, metrics.Config, *asyncRuntimeConfig, error) {
	contents, absoluteConfigPath, err := readRuntimeConfigFile(configPath)
	if err != nil {
		return server.Config{}, ocr.Config{}, metrics.Config{}, nil, withRuntimeConfigFailure("config_file_read", err)
	}
	parsed, err := decodeRuntimeConfig(contents)
	if err != nil {
		return server.Config{}, ocr.Config{}, metrics.Config{}, nil, withRuntimeConfigFailure("config_json_decode", err)
	}
	serverConfig, ocrConfig, metricsConfig, err := buildRuntimeConfig(parsed, filepath.Dir(absoluteConfigPath))
	if err != nil {
		return server.Config{}, ocr.Config{}, metrics.Config{}, nil, withRuntimeConfigFailure("config_base_validation", err)
	}
	asyncConfig := &asyncRuntimeConfig{
		SyncOCREnabled:  parsed.SyncOCREnabled == nil || *parsed.SyncOCREnabled,
		PipelineVersion: strings.TrimSpace(parsed.PipelineVersion),
	}
	if !asyncConfig.SyncOCREnabled && parsed.AsyncJobs == nil {
		return server.Config{}, ocr.Config{}, metrics.Config{}, nil, withRuntimeConfigFailure("config_async_validation", errors.New("invalid config: sync_ocr_enabled false requires async_jobs"))
	}
	if parsed.AsyncJobs != nil {
		asyncConfig.Enabled = true
		baseAsync, buildErr := buildAsyncRuntimeConfig(parsed.AsyncJobs, filepath.Dir(absoluteConfigPath), serverConfig.Logging.Path, metricsConfig.Path, ocrConfig.WorkRoot)
		err = buildErr
		if err != nil {
			return server.Config{}, ocr.Config{}, metrics.Config{}, nil, withRuntimeConfigFailure("config_async_validation", err)
		}
		baseAsync.Enabled = true
		baseAsync.SyncOCREnabled = asyncConfig.SyncOCREnabled
		baseAsync.PipelineVersion = asyncConfig.PipelineVersion
		asyncConfig = baseAsync
	}
	if parsed.QueueProcessing != nil {
		if !asyncConfig.Enabled {
			return server.Config{}, ocr.Config{}, metrics.Config{}, nil, withRuntimeConfigFailure("config_queue_validation", errors.New("invalid config field: queue_processing requires async_jobs"))
		}
		asyncConfig.QueueProcessing, err = buildQueueProcessingConfig(parsed.QueueProcessing, filepath.Dir(absoluteConfigPath), serverConfig.Logging.Path, metricsConfig.Path, ocrConfig.WorkRoot)
		if err != nil {
			return server.Config{}, ocr.Config{}, metrics.Config{}, nil, withRuntimeConfigFailure("config_queue_validation", err)
		}
		if asyncConfig.QueueProcessing.PrivateListenerAddress == serverConfig.Address {
			return server.Config{}, ocr.Config{}, metrics.Config{}, nil, withRuntimeConfigFailure("config_queue_validation", errors.New("invalid config: public and private listener addresses must differ"))
		}
		for _, queuePath := range []string{asyncConfig.QueueProcessing.WorkerCredentialsFile, asyncConfig.QueueProcessing.BrokerDSNFile} {
			for _, asyncPath := range []string{asyncConfig.CredentialsFile, asyncConfig.DatabaseDSNFile, asyncConfig.ImageRoot} {
				if pathsOverlap(queuePath, asyncPath) {
					return server.Config{}, ocr.Config{}, metrics.Config{}, nil, withRuntimeConfigFailure("config_queue_validation", errors.New("invalid config field: queue_processing paths must be separate from async_jobs paths"))
				}
			}
		}
	}
	return serverConfig, ocrConfig, metricsConfig, asyncConfig, nil
}

func buildAsyncRuntimeConfig(parsed *fileAsyncJobsConfig, configDir, logPath, metricsPath, ocrWorkRoot string) (*asyncRuntimeConfig, error) {
	if parsed == nil || parsed.MaxStoredImageBytes <= 0 || parsed.MaxConcurrentUploads <= 0 {
		return nil, errors.New("invalid config field: async_jobs")
	}
	retentionConfig, err := buildRetentionRuntimeConfig(parsed.Retention)
	if err != nil {
		return nil, err
	}
	connectTimeout, err := time.ParseDuration(strings.TrimSpace(parsed.ConnectTimeout))
	if err != nil || connectTimeout <= 0 {
		return nil, errors.New("invalid config field: async_jobs.connect_timeout")
	}
	lockTimeout, err := time.ParseDuration(strings.TrimSpace(parsed.MigrationLockTimeout))
	if err != nil || lockTimeout <= 0 {
		return nil, errors.New("invalid config field: async_jobs.migration_lock_timeout")
	}
	resolve := func(value, field string) (string, error) {
		value = strings.TrimSpace(value)
		if value == "" {
			return "", errors.New("invalid config field: " + field)
		}
		if !filepath.IsAbs(value) {
			value = filepath.Join(configDir, value)
		}
		absolute, resolveErr := filepath.Abs(value)
		if resolveErr != nil {
			return "", errors.New("invalid config field: " + field)
		}
		return filepath.Clean(absolute), nil
	}
	var credentials string
	if strings.TrimSpace(parsed.CredentialsFile) != "" {
		credentials, err = resolve(parsed.CredentialsFile, "async_jobs.credentials_file")
		if err != nil {
			return nil, err
		}
	} else if parsed.ClientEnrollment == nil {
		return nil, errors.New("invalid config field: async_jobs.credentials_file")
	}
	dsnFile, err := resolve(parsed.DatabaseDSNFile, "async_jobs.database_dsn_file")
	if err != nil {
		return nil, err
	}
	imageRoot, err := resolve(parsed.ImageRoot, "async_jobs.image_root")
	if err != nil {
		return nil, err
	}
	for _, path := range []string{credentials, dsnFile, imageRoot} {
		if pathsOverlap(path, logPath) || pathsOverlap(path, metricsPath) || pathsOverlap(path, ocrWorkRoot) {
			return nil, errors.New("invalid config field: async_jobs paths must be separate from runtime data")
		}
	}
	if pathsOverlap(credentials, dsnFile) || pathsOverlap(credentials, imageRoot) || pathsOverlap(dsnFile, imageRoot) {
		return nil, errors.New("invalid config field: async_jobs paths must be distinct")
	}
	var enrollment *clientEnrollmentRuntimeConfig
	if parsed.ClientEnrollment != nil {
		if parsed.ClientEnrollment.MaxClients <= 0 || parsed.ClientEnrollment.MaxNewClientsPerMinute <= 0 {
			return nil, errors.New("invalid config field: async_jobs.client_enrollment")
		}
		enrollment = &clientEnrollmentRuntimeConfig{
			MaxClients:             parsed.ClientEnrollment.MaxClients,
			MaxNewClientsPerMinute: parsed.ClientEnrollment.MaxNewClientsPerMinute,
		}
	}
	return &asyncRuntimeConfig{
		CredentialsFile: credentials, DatabaseDSNFile: dsnFile,
		ConnectTimeout: connectTimeout, MigrationLockTimeout: lockTimeout,
		ImageRoot: imageRoot, MaxStoredImageBytes: parsed.MaxStoredImageBytes,
		MaxConcurrentUploads: parsed.MaxConcurrentUploads,
		ClientEnrollment:     enrollment,
		Retention:            retentionConfig,
	}, nil
}

func buildRetentionRuntimeConfig(parsed *fileRetentionConfig) (retentionRuntimeConfig, error) {
	if parsed == nil || parsed.BatchSize <= 0 {
		return retentionRuntimeConfig{}, errors.New("invalid config field: async_jobs.retention")
	}
	parse := func(name, value string) (time.Duration, error) {
		duration, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || duration <= 0 {
			return 0, errors.New("invalid config field: async_jobs.retention." + name)
		}
		return duration, nil
	}
	claimLease, err := parse("claim_lease", parsed.ClaimLease)
	if err != nil {
		return retentionRuntimeConfig{}, err
	}
	interval, err := parse("interval", parsed.Interval)
	if err != nil {
		return retentionRuntimeConfig{}, err
	}
	operationTimeout, err := parse("operation_timeout", parsed.OperationTimeout)
	if err != nil {
		return retentionRuntimeConfig{}, err
	}
	if interval > 5*time.Second || claimLease <= operationTimeout {
		return retentionRuntimeConfig{}, errors.New("invalid config field: async_jobs.retention")
	}
	return retentionRuntimeConfig{ClaimLease: claimLease, Interval: interval, OperationTimeout: operationTimeout, BatchSize: parsed.BatchSize}, nil
}

func buildQueueProcessingConfig(parsed *fileQueueProcessingConfig, configDir, logPath, metricsPath, ocrWorkRoot string) (*queueProcessingRuntimeConfig, error) {
	if parsed == nil || parsed.MaxAttempts <= 0 {
		return nil, errors.New("invalid config field: queue_processing")
	}
	parse := func(name, value string) (time.Duration, error) {
		duration, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || duration <= 0 {
			return 0, errors.New("invalid config field: queue_processing." + name)
		}
		return duration, nil
	}
	connectTimeout, err := parse("connect_timeout", parsed.ConnectTimeout)
	if err != nil {
		return nil, err
	}
	pollInterval, err := parse("poll_interval", parsed.PollInterval)
	if err != nil {
		return nil, err
	}
	publisherLease, err := parse("publisher_lease", parsed.PublisherLease)
	if err != nil {
		return nil, err
	}
	publishTimeout, err := parse("publish_timeout", parsed.PublishTimeout)
	if err != nil {
		return nil, err
	}
	publishBackoff, err := parse("publish_backoff", parsed.PublishBackoff)
	if err != nil {
		return nil, err
	}
	reconnectMin, err := parse("reconnect_min", parsed.ReconnectMin)
	if err != nil {
		return nil, err
	}
	reconnectMax, err := parse("reconnect_max", parsed.ReconnectMax)
	if err != nil {
		return nil, err
	}
	consumerAckTimeout, err := parse("consumer_ack_timeout", parsed.ConsumerAckTimeout)
	if err != nil {
		return nil, err
	}
	retryInitial, err := parse("retry_initial_delay", parsed.RetryInitialDelay)
	if err != nil {
		return nil, err
	}
	retryMax, err := parse("retry_max_delay", parsed.RetryMaxDelay)
	if err != nil {
		return nil, err
	}
	hardProcessingTimeout, err := parse("hard_processing_timeout", parsed.HardProcessingTimeout)
	if err != nil {
		return nil, err
	}
	completionBudget, err := parse("completion_budget", parsed.CompletionBudget)
	if err != nil {
		return nil, err
	}
	attemptLease, err := parse("attempt_lease", parsed.AttemptLease)
	if err != nil {
		return nil, err
	}
	recoveryInterval, err := parse("recovery_interval", parsed.RecoveryInterval)
	if err != nil {
		return nil, err
	}
	if publisherLease <= publishTimeout {
		return nil, errors.New("invalid config timing: publisher_lease must exceed publish_timeout")
	}
	if reconnectMax < reconnectMin {
		return nil, errors.New("invalid config timing: reconnect_max must be at least reconnect_min")
	}
	if retryMax < retryInitial {
		return nil, errors.New("invalid config timing: retry_max_delay must be at least retry_initial_delay")
	}
	if consumerAckTimeout%time.Millisecond != 0 || consumerAckTimeout < 5*time.Minute {
		return nil, errors.New("invalid config timing: consumer_ack_timeout must be millisecond-aligned and at least 5m")
	}
	if hardProcessingTimeout%time.Millisecond != 0 || completionBudget%time.Millisecond != 0 || attemptLease%time.Millisecond != 0 {
		return nil, errors.New("invalid config timing: worker execution durations must be millisecond-aligned")
	}
	if hardProcessingTimeout > time.Duration(1<<63-1)-completionBudget || attemptLease <= hardProcessingTimeout+completionBudget {
		return nil, errors.New("invalid config timing: attempt_lease must exceed hard_processing_timeout plus completion_budget")
	}
	if attemptLease > time.Duration(1<<63-1)-completionBudget || consumerAckTimeout <= attemptLease+completionBudget {
		return nil, errors.New("invalid config timing: consumer_ack_timeout must exceed attempt_lease plus completion_budget")
	}
	listenerAddress := strings.TrimSpace(parsed.PrivateListenerAddress)
	if !validConcretePrivateListenAddress(listenerAddress) {
		return nil, errors.New("invalid config field: queue_processing.private_listener_address")
	}
	resolve := func(value, field string) (string, error) {
		value = strings.TrimSpace(value)
		if value == "" {
			return "", errors.New("invalid config field: queue_processing." + field)
		}
		if !filepath.IsAbs(value) {
			value = filepath.Join(configDir, value)
		}
		absolute, resolveErr := filepath.Abs(value)
		if resolveErr != nil {
			return "", errors.New("invalid config field: queue_processing." + field)
		}
		return filepath.Clean(absolute), nil
	}
	workerCredentials, err := resolve(parsed.WorkerCredentialsFile, "worker_credentials_file")
	if err != nil {
		return nil, err
	}
	brokerDSN, err := resolve(parsed.BrokerDSNFile, "broker_dsn_file")
	if err != nil {
		return nil, err
	}
	for _, path := range []string{workerCredentials, brokerDSN} {
		if pathsOverlap(path, logPath) || pathsOverlap(path, metricsPath) || pathsOverlap(path, ocrWorkRoot) {
			return nil, errors.New("invalid config field: queue_processing paths must be separate from runtime data")
		}
	}
	if pathsOverlap(workerCredentials, brokerDSN) {
		return nil, errors.New("invalid config field: queue_processing paths must be distinct")
	}
	return &queueProcessingRuntimeConfig{
		WorkerCredentialsFile: workerCredentials, PrivateListenerAddress: listenerAddress, BrokerDSNFile: brokerDSN,
		ConnectTimeout: connectTimeout, PollInterval: pollInterval, PublisherLease: publisherLease,
		PublishTimeout: publishTimeout, PublishBackoff: publishBackoff, ReconnectMin: reconnectMin,
		ReconnectMax: reconnectMax, ConsumerAckTimeout: consumerAckTimeout, MaxAttempts: parsed.MaxAttempts,
		RetryInitialDelay: retryInitial, RetryMaxDelay: retryMax, HardProcessingTimeout: hardProcessingTimeout,
		CompletionBudget: completionBudget, AttemptLease: attemptLease, RecoveryInterval: recoveryInterval,
	}, nil
}

func validConcretePrivateListenAddress(value string) bool {
	host, port, err := net.SplitHostPort(value)
	if err != nil || port == "" {
		return false
	}
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil || portNumber == 0 {
		return false
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && !ip.IsUnspecified() && !ip.Is4In6() && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast())
}

func pathsOverlap(first, second string) bool {
	if strings.TrimSpace(first) == "" || strings.TrimSpace(second) == "" {
		return false
	}
	return sameOrNestedPath(first, second) || sameOrNestedPath(second, first)
}

func sameOrNestedPath(candidate, parent string) bool {
	rel, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(candidate))
	return err == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func readRuntimeConfigFile(configPath string) ([]byte, string, error) {
	if strings.TrimSpace(configPath) == "" {
		return nil, "", errors.New("invalid config path")
	}
	absoluteConfigPath, err := filepath.Abs(configPath)
	if err != nil {
		return nil, "", errors.New("invalid config path")
	}
	file, err := os.Open(absoluteConfigPath)
	if err != nil {
		return nil, "", errors.New("config file unavailable")
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err != nil || len(contents) > maxConfigBytes {
		return nil, "", errors.New("invalid config file size")
	}
	return contents, absoluteConfigPath, nil
}

func decodeRuntimeConfig(contents []byte) (fileConfig, error) {
	if err := jsonstrict.RejectDuplicateKeys(contents); err != nil {
		return fileConfig{}, errors.New("invalid config JSON")
	}

	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var parsed fileConfig
	if err := decoder.Decode(&parsed); err != nil {
		return fileConfig{}, errors.New("invalid config JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fileConfig{}, errors.New("invalid config JSON")
	}
	return parsed, nil
}

func buildRuntimeConfig(parsed fileConfig, configDir string) (server.Config, ocr.Config, metrics.Config, error) {
	if err := validateRequiredRuntimeFields(parsed); err != nil {
		return server.Config{}, ocr.Config{}, metrics.Config{}, err
	}
	logging, err := normalizeLoggingConfig(parsed.Logging, configDir)
	if err != nil {
		return server.Config{}, ocr.Config{}, metrics.Config{}, err
	}
	allowedNetworks, err := parseAllowedClientNetworks(parsed.AllowedClientNetworks)
	if err != nil {
		return server.Config{}, ocr.Config{}, metrics.Config{}, err
	}
	timeouts, err := parseRuntimeTimeouts(parsed)
	if err != nil {
		return server.Config{}, ocr.Config{}, metrics.Config{}, err
	}
	ocrConfig := ocr.Config{PipelineVersion: strings.TrimSpace(parsed.PipelineVersion), MaxInputBytes: parsed.MaxUploadBytes}
	if syncOCREnabled(parsed) {
		ocrConfig, err = buildOCRConfig(parsed, configDir, timeouts.stopGrace)
		if err != nil {
			return server.Config{}, ocr.Config{}, metrics.Config{}, err
		}
	}
	metricsConfig, err := buildMetricsConfig(parsed, configDir, logging.Path, ocrConfig.WorkRoot, parsed.PipelineVersion)
	if err != nil {
		return server.Config{}, ocr.Config{}, metrics.Config{}, err
	}
	serverConfig := server.Config{
		Address: parsed.ListenAddress,
		Logging: logging,
		HTTP: httpapi.Config{
			MaxUploadBytes:        parsed.MaxUploadBytes,
			OCRTimeout:            timeouts.ocr,
			MaxConcurrent:         parsed.MaxConcurrent,
			PipelineVersion:       ocrConfig.PipelineVersion,
			AllowedClientNetworks: allowedNetworks,
		},
		ReadHeaderTimeout: timeouts.readHeader,
		ReadTimeout:       timeouts.read,
		WriteTimeout:      timeouts.write,
		IdleTimeout:       timeouts.idle,
		ShutdownTimeout:   timeouts.shutdown,
		MaxHeaderBytes:    parsed.MaxHeaderBytes,
	}
	return serverConfig, ocrConfig, metricsConfig, nil
}

func validateRequiredRuntimeFields(parsed fileConfig) error {
	if strings.TrimSpace(parsed.ListenAddress) == "" {
		return errors.New("invalid config field: listen_address")
	}
	if parsed.AllowedClientNetworks == nil {
		return errors.New("invalid config field: allowed_client_networks")
	}
	if !validMaxUploadBytes(parsed.MaxUploadBytes) {
		return errors.New("invalid config field: max_upload_bytes")
	}
	if parsed.MaxConcurrent <= 0 {
		return errors.New("invalid config field: max_concurrent")
	}
	if strings.TrimSpace(parsed.PipelineVersion) == "" {
		return errors.New("invalid config field: pipeline_version")
	}
	if parsed.MaxHeaderBytes <= 0 {
		return errors.New("invalid config field: max_header_bytes")
	}
	if syncOCREnabled(parsed) {
		if parsed.MaxOutputBytes <= 0 {
			return errors.New("invalid config field: max_output_bytes")
		}
		if parsed.MaxImagePixels <= 0 {
			return errors.New("invalid config field: max_image_pixels")
		}
	}
	return nil
}

func syncOCREnabled(parsed fileConfig) bool {
	return parsed.SyncOCREnabled == nil || *parsed.SyncOCREnabled
}

func validMaxUploadBytes(value int64) bool {
	return value > 0 && value <= int64(^uint64(0)>>1)-(64<<10)
}

func normalizeLoggingConfig(parsed telemetry.Config, configDir string) (telemetry.Config, error) {
	logging := parsed
	logging.Path = strings.TrimSpace(logging.Path)
	logging.Level = strings.ToLower(strings.TrimSpace(logging.Level))
	if !validLoggingSettings(logging) {
		return telemetry.Config{}, errors.New("invalid config field: logging")
	}
	if !filepath.IsAbs(logging.Path) {
		logging.Path = filepath.Join(configDir, logging.Path)
	}
	resolvedLogPath, err := filepath.Abs(logging.Path)
	if err != nil {
		return telemetry.Config{}, errors.New("invalid config field: logging.path")
	}
	logging.Path = resolvedLogPath
	return logging, nil
}

func validLoggingSettings(logging telemetry.Config) bool {
	levelSupported := logging.Level == "debug" || logging.Level == "info" || logging.Level == "warn" || logging.Level == "error"
	return logging.Path != "" && levelSupported && logging.MaxSizeBytes >= 8192 && logging.MaxBackups >= 0 && logging.MaxBackups <= 10
}

func parseAllowedClientNetworks(values []string) ([]netip.Prefix, error) {
	allowedNetworks := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return nil, errors.New("invalid config field: allowed_client_networks")
		}
		allowedNetworks = append(allowedNetworks, prefix)
	}
	return allowedNetworks, nil
}

type runtimeTimeouts struct {
	ocr        time.Duration
	readHeader time.Duration
	read       time.Duration
	write      time.Duration
	idle       time.Duration
	shutdown   time.Duration
	stopGrace  time.Duration
}

func parseRuntimeTimeouts(parsed fileConfig) (runtimeTimeouts, error) {
	httpTimeouts, err := parseHTTPTimeouts(parsed)
	if err != nil {
		return runtimeTimeouts{}, err
	}
	lifecycleTimeouts, err := parseLifecycleTimeouts(parsed)
	if err != nil {
		return runtimeTimeouts{}, err
	}
	timeouts := runtimeTimeouts{
		ocr:        httpTimeouts.ocr,
		readHeader: httpTimeouts.readHeader,
		read:       httpTimeouts.read,
		write:      httpTimeouts.write,
		idle:       lifecycleTimeouts.idle,
		shutdown:   lifecycleTimeouts.shutdown,
		stopGrace:  lifecycleTimeouts.stopGrace,
	}
	if err := validateRuntimeTiming(timeouts); err != nil {
		return runtimeTimeouts{}, err
	}
	return timeouts, nil
}

func parseHTTPTimeouts(parsed fileConfig) (runtimeTimeouts, error) {
	ocrTimeout, err := parsePositiveDuration("ocr_timeout", parsed.OCRTimeout)
	if err != nil {
		return runtimeTimeouts{}, err
	}
	readHeaderTimeout, err := parsePositiveDuration("read_header_timeout", parsed.ReadHeaderTimeout)
	if err != nil {
		return runtimeTimeouts{}, err
	}
	readTimeout, err := parsePositiveDuration("read_timeout", parsed.ReadTimeout)
	if err != nil {
		return runtimeTimeouts{}, err
	}
	writeTimeout, err := parsePositiveDuration("write_timeout", parsed.WriteTimeout)
	if err != nil {
		return runtimeTimeouts{}, err
	}
	return runtimeTimeouts{
		ocr: ocrTimeout, readHeader: readHeaderTimeout,
		read: readTimeout, write: writeTimeout,
	}, nil
}

func parseLifecycleTimeouts(parsed fileConfig) (runtimeTimeouts, error) {
	idleTimeout, err := parsePositiveDuration("idle_timeout", parsed.IdleTimeout)
	if err != nil {
		return runtimeTimeouts{}, err
	}
	shutdownTimeout, err := parsePositiveDuration("shutdown_timeout", parsed.ShutdownTimeout)
	if err != nil {
		return runtimeTimeouts{}, err
	}
	var stopGrace time.Duration
	if syncOCREnabled(parsed) {
		stopGrace, err = parsePositiveDuration("process_stop_grace", parsed.ProcessStopGrace)
		if err != nil {
			return runtimeTimeouts{}, err
		}
	}
	return runtimeTimeouts{idle: idleTimeout, shutdown: shutdownTimeout, stopGrace: stopGrace}, nil
}

func validateRuntimeTiming(timeouts runtimeTimeouts) error {
	if timeouts.stopGrace > 0 && timeouts.ocr <= timeouts.stopGrace {
		return errors.New("invalid config timing: ocr_timeout must exceed process_stop_grace")
	}
	if timeouts.stopGrace > 0 && timeouts.shutdown <= timeouts.stopGrace {
		return errors.New("invalid config timing: shutdown_timeout must exceed process_stop_grace")
	}
	if timeouts.readHeader > timeouts.read {
		return errors.New("invalid config timing: read_header_timeout exceeds read_timeout")
	}
	if timeouts.write <= timeouts.read || timeouts.write-timeouts.read <= timeouts.ocr {
		return errors.New("invalid config timing: write_timeout must exceed read_timeout plus ocr_timeout")
	}
	return nil
}

func buildOCRConfig(parsed fileConfig, configDir string, stopGrace time.Duration) (ocr.Config, error) {
	workRoot, err := resolveRequiredConfigPath(configDir, "ocr.work_root", parsed.OCR.WorkRoot)
	if err != nil {
		return ocr.Config{}, err
	}
	ocrConfig := ocr.Config{
		WorkRoot:        workRoot,
		PipelineVersion: strings.TrimSpace(parsed.PipelineVersion),
		MaxInputBytes:   parsed.MaxUploadBytes,
		MaxOutputBytes:  parsed.MaxOutputBytes,
		MaxImagePixels:  parsed.MaxImagePixels,
		StopGrace:       stopGrace,
	}
	startupTimeout, err := parsePositiveDuration("ocr.russian_docs_startup_timeout", parsed.OCR.StartupTimeout)
	if err != nil {
		return ocr.Config{}, err
	}
	for _, path := range []struct {
		name  string
		value string
		dest  *string
	}{
		{"ocr.russian_docs_python", parsed.OCR.RussianPython, &ocrConfig.RussianPython},
		{"ocr.russian_docs_worker", parsed.OCR.RussianWorker, &ocrConfig.RussianWorker},
		{"ocr.russian_docs_repo", parsed.OCR.RussianRepo, &ocrConfig.RussianRepo},
		{"ocr.russian_docs_models_root", parsed.OCR.RussianModels, &ocrConfig.RussianModels},
	} {
		*path.dest, err = resolveRequiredConfigPath(configDir, path.name, path.value)
		if err != nil {
			return ocr.Config{}, err
		}
	}
	ocrConfig.StartupTimeout = startupTimeout
	return ocrConfig, nil
}

func resolveRequiredConfigPath(configDir, name, value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", errors.New("invalid config field: " + name)
	}
	absolute, err := resolveConfigPath(configDir, value)
	if err != nil {
		return "", errors.New("invalid config path field: " + name)
	}
	return absolute, nil
}

func buildMetricsConfig(parsed fileConfig, configDir, logPath, workRoot, pipelineVersion string) (metrics.Config, error) {
	statePath := strings.TrimSpace(parsed.Metrics.StatePath)
	if statePath == "" {
		return metrics.Config{}, errors.New("invalid config field: metrics.state_path")
	}
	if !filepath.IsAbs(statePath) {
		statePath = filepath.Join(configDir, statePath)
	}
	statePath, err := filepath.Abs(statePath)
	if err != nil || metricsPathConflicts(statePath, logPath, workRoot) {
		return metrics.Config{}, errors.New("invalid config path field: metrics.state_path")
	}
	return metrics.Config{Path: statePath, PipelineVersion: pipelineVersion}, nil
}

func metricsPathConflicts(statePath, logPath, workRoot string) bool {
	statePath, _ = filepath.Abs(filepath.Clean(statePath))
	logPath, _ = filepath.Abs(filepath.Clean(logPath))
	if strings.TrimSpace(workRoot) == "" {
		return pathsOverlapBySuffix(statePath, logPath)
	}
	workRoot, _ = filepath.Abs(filepath.Clean(workRoot))
	if pathsOverlapBySuffix(statePath, logPath) {
		return true
	}
	relative, err := filepath.Rel(workRoot, statePath)
	return err != nil || relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)))
}

func pathsOverlapBySuffix(first, second string) bool {
	return first == second || strings.HasPrefix(first, second+".") || strings.HasPrefix(second, first+".")
}

func parsePositiveDuration(field, value string) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, errors.New("invalid config field: " + field)
	}
	return duration, nil
}

func resolveConfigPath(configDir, value string) (string, error) {
	if !filepath.IsAbs(value) {
		value = filepath.Join(configDir, value)
	}
	return filepath.Abs(value)
}
