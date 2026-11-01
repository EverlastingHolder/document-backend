package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"passport-backend/internal/httpapi"
	"passport-backend/internal/metrics"
	"passport-backend/internal/ocr"
	"passport-backend/internal/passport"
	"passport-backend/internal/server"
	"passport-backend/internal/telemetry"
)

type managedRecognizer interface {
	httpapi.Recognizer
	Close(context.Context) error
}

type runner interface {
	Run(context.Context) error
}

type unavailableRecognizer struct{}

func (unavailableRecognizer) Recognize(context.Context, httpapi.Image) (passport.Extraction, error) {
	return passport.Extraction{}, httpapi.ErrUnavailable
}

func (unavailableRecognizer) Close(context.Context) error { return nil }

func main() {
	os.Exit(mainExit(os.Args[1:], os.Stderr))
}

func mainExit(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("api", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "path to API JSON configuration")
	checkConfig := flags.Bool("check-config", false, "validate configuration without starting services")
	if err := flags.Parse(args); err != nil || *configPath == "" || flags.NArg() != 0 {
		writeCategory(stderr, "usage: api -config /path/to/config.json [-check-config]")
		return 2
	}
	if *checkConfig {
		if _, _, _, _, err := loadFullRuntimeConfig(*configPath); err != nil {
			writeCategory(stderr, "configuration_invalid")
			return 2
		}
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *configPath, stderr, nil, nil); err != nil {
		return 1
	}
	return 0
}

func run(
	ctx context.Context,
	configPath string,
	stderr io.Writer,
	newRecognizer func(ocr.Config) (managedRecognizer, error),
	newServer func(server.Config, httpapi.Recognizer) (runner, error),
) (retErr error) {
	if ctx == nil {
		writeFallbackEvent(stderr, "startup", "context_validation", "startup_failed")
		return errors.New("API startup failed")
	}
	if ctx.Err() != nil {
		return nil
	}
	serverConfig, ocrConfig, metricsConfig, asyncRuntime, err := loadFullRuntimeConfig(configPath)
	if err != nil {
		writeFallbackEvent(stderr, "startup", runtimeConfigFailurePhase(err), "startup_failed")
		return errors.New("API configuration failed")
	}
	sink, err := telemetry.Open(serverConfig.Logging)
	if err != nil {
		writeFallbackEvent(stderr, "startup", "logging_open", "startup_failed")
		return errors.New("logging setup failed")
	}
	logger := sink.Logger()
	startupLogger := logger.With("component", "startup")
	defer func() {
		if err := sink.Close(); err != nil {
			writeFallbackEvent(stderr, "runtime", "logging_close", "shutdown_failed")
			if retErr == nil {
				retErr = errors.New("logging shutdown failed")
			}
		}
	}()
	metricsConfig.Logger = logger.With("component", "runtime")
	metricsStore, err := metrics.Open(metricsConfig)
	if err != nil {
		startupLogger.Error("service.failed", "event", "service.failed", "error_code", "startup_failed")
		return errors.New("metrics setup failed")
	}
	defer func() {
		if err := metricsStore.Close(); err != nil {
			startupLogger.Error("service.failed", "event", "service.failed", "error_code", "shutdown_failed")
			retErr = errors.Join(retErr, errors.New("metrics shutdown failed"))
		}
	}()
	serverConfig.Metrics = metricsStore
	serverConfig.HTTP.Metrics = metricsStore
	ocrConfig.Metrics = metricsStore
	serverConfig.Logger = logger
	serverConfig.HTTP.Logger = logger.With("component", "httpapi")
	ocrConfig.Logger = logger.With("component", "ocr")
	if asyncRuntime.SyncOCREnabled {
		if err := os.MkdirAll(ocrConfig.WorkRoot, 0o700); err != nil {
			startupLogger.Error("service.failed", "event", "service.failed", "error_code", "startup_failed")
			return errors.New("OCR workspace setup failed")
		}
	}
	asyncResources, err := openAsyncJobs(ctx, asyncRuntime, serverConfig.HTTP.MaxUploadBytes, metricsStore, logger, serverConfig)
	if err != nil {
		startupLogger.Error("service.failed", "event", "service.failed", "error_code", "startup_failed")
		return errors.New("async job storage startup failed")
	}
	serverConfig.HTTP.AsyncJobs = asyncResources.publicConfig
	serverConfig.HTTP.ReadinessCheck = asyncResources.readiness
	if asyncResources.readiness != nil {
		serverConfig.HTTP.ReadinessTimeout = serverConfig.ReadTimeout
	}
	defer func() {
		if err := asyncResources.cleanup(); err != nil {
			startupLogger.Error("service.failed", "event", "service.failed", "error_code", "shutdown_failed")
			retErr = errors.Join(retErr, errors.New("async storage cleanup failed"))
		}
	}()

	var provider managedRecognizer
	if asyncRuntime.SyncOCREnabled {
		if newRecognizer == nil {
			newRecognizer = func(config ocr.Config) (managedRecognizer, error) {
				recognizer, err := ocr.New(config)
				return recognizer, err
			}
		}
		provider, err = newRecognizer(ocrConfig)
		if err != nil || provider == nil {
			startupLogger.Error("service.failed", "event", "service.failed", "error_code", "startup_failed")
			return errors.New("OCR provider startup failed")
		}
	} else {
		provider = unavailableRecognizer{}
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), serverConfig.ShutdownTimeout)
		defer cancel()
		if err := provider.Close(closeCtx); err != nil {
			startupLogger.Error("service.failed", "event", "service.failed", "error_code", "shutdown_failed")
			retErr = errors.New("OCR cleanup failed")
		}
	}()

	if newServer == nil {
		newServer = newProductionServer
	}
	apiServer, err := newServer(serverConfig, provider)
	if err != nil || apiServer == nil {
		startupLogger.Error("service.failed", "event", "service.failed", "error_code", "startup_failed")
		return errors.New("API server startup failed")
	}
	components := []runner{apiServer}
	if asyncResources.private != nil {
		components = append(components, asyncResources.private)
	}
	components = append(components, asyncResources.background...)
	if err := runComponents(ctx, components...); err != nil {
		startupLogger.Error("service.failed", "event", "service.failed", "error_code", "shutdown_failed")
		return errors.New("API runtime failed")
	}
	return nil
}

func runComponents(ctx context.Context, components ...runner) error {
	if ctx == nil || len(components) == 0 {
		return errors.New("invalid runtime components")
	}
	for _, component := range components {
		if component == nil {
			return errors.New("invalid runtime component")
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct{ err error }
	results := make(chan result, len(components))
	for _, component := range components {
		go func(current runner) { results <- result{err: current.Run(runCtx)} }(component)
	}
	var runErr error
	for range components {
		completed := <-results
		if completed.err != nil && runErr == nil {
			runErr = completed.err
		}
		cancel()
	}
	if ctx.Err() != nil {
		return nil
	}
	return runErr
}

func newProductionServer(config server.Config, recognizer httpapi.Recognizer) (runner, error) {
	apiServer, err := server.New(config, recognizer)
	return apiServer, err
}

func writeCategory(stderr io.Writer, category string) {
	if stderr != nil {
		_, _ = fmt.Fprintln(stderr, category)
	}
}

func writeFallbackEvent(stderr io.Writer, component, phase, errorCode string) {
	if stderr == nil {
		return
	}
	component = safeFallbackComponent(component)
	phase = safeFallbackPhase(phase)
	errorCode = safeFallbackCode(errorCode)
	_ = json.NewEncoder(stderr).Encode(map[string]any{
		"time": time.Now().UTC().Format(time.RFC3339Nano), "level": slog.LevelError.String(),
		"service": "passport-api", "component": component, "event": "service.failed",
		"msg": "service.failed", "phase": phase, "error_code": errorCode,
	})
}

func safeFallbackPhase(value string) string {
	switch value {
	case "context_validation", "config_file_read", "config_json_decode", "config_base_validation", "config_async_validation", "config_queue_validation", "config_validation", "logging_open", "logging_close":
		return value
	default:
		return "context_validation"
	}
}

func safeFallbackComponent(value string) string {
	switch value {
	case "startup", "runtime":
		return value
	default:
		return "runtime"
	}
}

func safeFallbackCode(value string) string {
	switch value {
	case "startup_failed", "shutdown_failed":
		return value
	default:
		return "startup_failed"
	}
}
