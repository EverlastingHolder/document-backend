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
	if strings.TrimSpace(configPath) == "" {
		return server.Config{}, ocr.Config{}, metrics.Config{}, errors.New("invalid config path")
	}
	absoluteConfigPath, err := filepath.Abs(configPath)
	if err != nil {
		return server.Config{}, ocr.Config{}, metrics.Config{}, errors.New("invalid config path")
	}
	file, err := os.Open(absoluteConfigPath)
	if err != nil {
		return server.Config{}, ocr.Config{}, metrics.Config{}, errors.New("config file unavailable")
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err != nil || len(contents) > maxConfigBytes {
		return server.Config{}, ocr.Config{}, metrics.Config{}, errors.New("invalid config file size")
	}
	if err := rejectDuplicateJSONKeys(contents); err != nil {
		return server.Config{}, ocr.Config{}, metrics.Config{}, errors.New("invalid config JSON")
	}

	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var parsed fileConfig
	if err := decoder.Decode(&parsed); err != nil {
		return server.Config{}, ocr.Config{}, metrics.Config{}, errors.New("invalid config JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return server.Config{}, ocr.Config{}, metrics.Config{}, errors.New("invalid config JSON")
	}

	serverConfig, ocrConfig, metricsConfig, err := buildRuntimeConfig(parsed, filepath.Dir(absoluteConfigPath))
	if err != nil {
		return server.Config{}, ocr.Config{}, metrics.Config{}, err
	}
	return serverConfig, ocrConfig, metricsConfig, nil
}

func buildRuntimeConfig(parsed fileConfig, configDir string) (server.Config, ocr.Config, metrics.Config, error) {
	if strings.TrimSpace(parsed.ListenAddress) == "" {
		return server.Config{}, ocr.Config{}, metrics.Config{}, errors.New("invalid config field: listen_address")
	}
	if parsed.AllowedClientNetworks == nil {
		return server.Config{}, ocr.Config{}, metrics.Config{}, errors.New("invalid config field: allowed_client_networks")
	}
	if parsed.MaxUploadBytes <= 0 || parsed.MaxUploadBytes > int64(^uint64(0)>>1)-(64<<10) {
		return server.Config{}, ocr.Config{}, metrics.Config{}, errors.New("invalid config field: max_upload_bytes")
	}
	if parsed.MaxConcurrent <= 0 {
		return server.Config{}, ocr.Config{}, metrics.Config{}, errors.New("invalid config field: max_concurrent")
	}
	if strings.TrimSpace(parsed.PipelineVersion) == "" {
		return server.Config{}, ocr.Config{}, metrics.Config{}, errors.New("invalid config field: pipeline_version")
	}
	if parsed.MaxHeaderBytes <= 0 {
		return server.Config{}, ocr.Config{}, metrics.Config{}, errors.New("invalid config field: max_header_bytes")
	}
	if parsed.MaxOutputBytes <= 0 {
		return server.Config{}, ocr.Config{}, metrics.Config{}, errors.New("invalid config field: max_output_bytes")
	}
	if parsed.MaxImagePixels <= 0 {
		return server.Config{}, ocr.Config{}, metrics.Config{}, errors.New("invalid config field: max_image_pixels")
	}
	logging := parsed.Logging
	logging.Path = strings.TrimSpace(logging.Path)
	logging.Level = strings.ToLower(strings.TrimSpace(logging.Level))
	if logging.Path == "" || (logging.Level != "debug" && logging.Level != "info" && logging.Level != "warn" && logging.Level != "error") || logging.MaxSizeBytes < 8192 || logging.MaxBackups < 0 || logging.MaxBackups > 10 {
		return server.Config{}, ocr.Config{}, metrics.Config{}, errors.New("invalid config field: logging")
	}
	if !filepath.IsAbs(logging.Path) {
		logging.Path = filepath.Join(configDir, logging.Path)
	}
	resolvedLogPath, err := filepath.Abs(logging.Path)
	if err != nil {
		return server.Config{}, ocr.Config{}, metrics.Config{}, errors.New("invalid config field: logging.path")
	}
	logging.Path = resolvedLogPath

	allowedNetworks := make([]netip.Prefix, 0, len(parsed.AllowedClientNetworks))
	for _, value := range parsed.AllowedClientNetworks {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return server.Config{}, ocr.Config{}, metrics.Config{}, errors.New("invalid config field: allowed_client_networks")
		}
		allowedNetworks = append(allowedNetworks, prefix)
	}

	ocrTimeout, err := parsePositiveDuration("ocr_timeout", parsed.OCRTimeout)
	if err != nil {
		return server.Config{}, ocr.Config{}, metrics.Config{}, err
	}
	readHeaderTimeout, err := parsePositiveDuration("read_header_timeout", parsed.ReadHeaderTimeout)
	if err != nil {
		return server.Config{}, ocr.Config{}, metrics.Config{}, err
	}
	readTimeout, err := parsePositiveDuration("read_timeout", parsed.ReadTimeout)
	if err != nil {
		return server.Config{}, ocr.Config{}, metrics.Config{}, err
	}
	writeTimeout, err := parsePositiveDuration("write_timeout", parsed.WriteTimeout)
	if err != nil {
		return server.Config{}, ocr.Config{}, metrics.Config{}, err
	}
	idleTimeout, err := parsePositiveDuration("idle_timeout", parsed.IdleTimeout)
	if err != nil {
		return server.Config{}, ocr.Config{}, metrics.Config{}, err
	}
	shutdownTimeout, err := parsePositiveDuration("shutdown_timeout", parsed.ShutdownTimeout)
	if err != nil {
		return server.Config{}, ocr.Config{}, metrics.Config{}, err
	}
	stopGrace, err := parsePositiveDuration("process_stop_grace", parsed.ProcessStopGrace)
	if err != nil {
		return server.Config{}, ocr.Config{}, metrics.Config{}, err
	}
	if ocrTimeout <= stopGrace {
		return server.Config{}, ocr.Config{}, metrics.Config{}, errors.New("invalid config timing: ocr_timeout must exceed process_stop_grace")
	}
	if shutdownTimeout <= stopGrace {
		return server.Config{}, ocr.Config{}, metrics.Config{}, errors.New("invalid config timing: shutdown_timeout must exceed process_stop_grace")
	}
	if readHeaderTimeout > readTimeout {
		return server.Config{}, ocr.Config{}, metrics.Config{}, errors.New("invalid config timing: read_header_timeout exceeds read_timeout")
	}
	if writeTimeout <= readTimeout || writeTimeout-readTimeout <= ocrTimeout {
		return server.Config{}, ocr.Config{}, metrics.Config{}, errors.New("invalid config timing: write_timeout must exceed read_timeout plus ocr_timeout")
	}

	resolveRequired := func(name, value string) (string, error) {
		if strings.TrimSpace(value) == "" {
			return "", errors.New("invalid config field: " + name)
		}
		absolute, err := resolveConfigPath(configDir, value)
		if err != nil {
			return "", errors.New("invalid config path field: " + name)
		}
		return absolute, nil
	}
	workRoot, err := resolveRequired("ocr.work_root", parsed.OCR.WorkRoot)
	if err != nil {
		return server.Config{}, ocr.Config{}, metrics.Config{}, err
	}
	ocrConfig := ocr.Config{
		WorkRoot:        workRoot,
		PipelineVersion: strings.TrimSpace(parsed.PipelineVersion),
		MaxInputBytes:   parsed.MaxUploadBytes,
		MaxOutputBytes:  parsed.MaxOutputBytes,
		MaxImagePixels:  parsed.MaxImagePixels,
		StopGrace:       stopGrace,
	}
	var startupTimeout time.Duration
	startupTimeout, err = parsePositiveDuration("ocr.russian_docs_startup_timeout", parsed.OCR.StartupTimeout)
	if err != nil {
		return server.Config{}, ocr.Config{}, metrics.Config{}, err
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
		*path.dest, err = resolveRequired(path.name, path.value)
		if err != nil {
			return server.Config{}, ocr.Config{}, metrics.Config{}, err
		}
	}
	ocrConfig.StartupTimeout = startupTimeout
	statePath := strings.TrimSpace(parsed.Metrics.StatePath)
	if statePath == "" {
		return server.Config{}, ocr.Config{}, metrics.Config{}, errors.New("invalid config field: metrics.state_path")
	}
	if !filepath.IsAbs(statePath) {
		statePath = filepath.Join(configDir, statePath)
	}
	statePath, err = filepath.Abs(statePath)
	if err != nil || metricsPathConflicts(statePath, logging.Path, workRoot) {
		return server.Config{}, ocr.Config{}, metrics.Config{}, errors.New("invalid config path field: metrics.state_path")
	}

	serverConfig := server.Config{
		Address: parsed.ListenAddress,
		Logging: logging,
		HTTP: httpapi.Config{
			MaxUploadBytes:        parsed.MaxUploadBytes,
			OCRTimeout:            ocrTimeout,
			MaxConcurrent:         parsed.MaxConcurrent,
			PipelineVersion:       strings.TrimSpace(parsed.PipelineVersion),
			AllowedClientNetworks: allowedNetworks,
		},
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		ShutdownTimeout:   shutdownTimeout,
		MaxHeaderBytes:    parsed.MaxHeaderBytes,
	}
	return serverConfig, ocrConfig, metrics.Config{Path: statePath, PipelineVersion: ocrConfig.PipelineVersion}, nil
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

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if _, err := consumeJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return token, nil
	}
	switch delimiter {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("invalid object key")
			}
			if _, exists := object[key]; exists {
				return nil, errors.New("duplicate object key")
			}
			value, err := consumeJSONValue(decoder)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return nil, errors.New("invalid JSON object")
		}
		return object, nil
	case '[':
		for decoder.More() {
			if _, err := consumeJSONValue(decoder); err != nil {
				return nil, err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return nil, errors.New("invalid JSON array")
		}
		return nil, nil
	default:
		return nil, errors.New("unexpected JSON delimiter")
	}
}
