package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"os"
	"path/filepath"
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

type fileConfig struct {
	ListenAddress         string            `json:"listen_address"`
	AllowedClientNetworks []string          `json:"allowed_client_networks"`
	MaxUploadBytes        int64             `json:"max_upload_bytes"`
	MaxConcurrent         int               `json:"max_concurrent"`
	PipelineVersion       string            `json:"pipeline_version"`
	OCRTimeout            string            `json:"ocr_timeout"`
	ReadHeaderTimeout     string            `json:"read_header_timeout"`
	ReadTimeout           string            `json:"read_timeout"`
	WriteTimeout          string            `json:"write_timeout"`
	IdleTimeout           string            `json:"idle_timeout"`
	ShutdownTimeout       string            `json:"shutdown_timeout"`
	MaxHeaderBytes        int               `json:"max_header_bytes"`
	ProcessStopGrace      string            `json:"process_stop_grace"`
	MaxOutputBytes        int64             `json:"max_output_bytes"`
	MaxImagePixels        int64             `json:"max_image_pixels"`
	Logging               telemetry.Config  `json:"logging"`
	Metrics               fileMetricsConfig `json:"metrics"`
	OCR                   fileOCRConfig     `json:"ocr"`
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
	contents, absoluteConfigPath, err := readRuntimeConfigFile(configPath)
	if err != nil {
		return server.Config{}, ocr.Config{}, metrics.Config{}, err
	}
	parsed, err := decodeRuntimeConfig(contents)
	if err != nil {
		return server.Config{}, ocr.Config{}, metrics.Config{}, err
	}
	serverConfig, ocrConfig, metricsConfig, err := buildRuntimeConfig(parsed, filepath.Dir(absoluteConfigPath))
	if err != nil {
		return server.Config{}, ocr.Config{}, metrics.Config{}, err
	}
	return serverConfig, ocrConfig, metricsConfig, nil
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
	ocrConfig, err := buildOCRConfig(parsed, configDir, timeouts.stopGrace)
	if err != nil {
		return server.Config{}, ocr.Config{}, metrics.Config{}, err
	}
	metricsConfig, err := buildMetricsConfig(parsed, configDir, logging.Path, ocrConfig.WorkRoot, ocrConfig.PipelineVersion)
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
	if parsed.MaxOutputBytes <= 0 {
		return errors.New("invalid config field: max_output_bytes")
	}
	if parsed.MaxImagePixels <= 0 {
		return errors.New("invalid config field: max_image_pixels")
	}
	return nil
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
	stopGrace, err := parsePositiveDuration("process_stop_grace", parsed.ProcessStopGrace)
	if err != nil {
		return runtimeTimeouts{}, err
	}
	return runtimeTimeouts{idle: idleTimeout, shutdown: shutdownTimeout, stopGrace: stopGrace}, nil
}

func validateRuntimeTiming(timeouts runtimeTimeouts) error {
	if timeouts.ocr <= timeouts.stopGrace {
		return errors.New("invalid config timing: ocr_timeout must exceed process_stop_grace")
	}
	if timeouts.shutdown <= timeouts.stopGrace {
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
