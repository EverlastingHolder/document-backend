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

func main() {
	os.Exit(mainExit(os.Args[1:], os.Stderr))
}

func mainExit(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("api", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "path to API JSON configuration")
	if err := flags.Parse(args); err != nil || *configPath == "" || flags.NArg() != 0 {
		writeCategory(stderr, "usage: api -config /path/to/config.json")
		return 2
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
		writeFallbackEvent(stderr, "startup", "startup_failed")
		return errors.New("API startup failed")
	}
	if ctx.Err() != nil {
		return nil
	}
	serverConfig, ocrConfig, metricsConfig, err := loadRuntimeConfig(configPath)
	if err != nil {
		writeFallbackEvent(stderr, "startup", "startup_failed")
		return errors.New("API configuration failed")
	}
	sink, err := telemetry.Open(serverConfig.Logging)
	if err != nil {
		writeFallbackEvent(stderr, "startup", "startup_failed")
		return errors.New("logging setup failed")
	}
	logger := sink.Logger()
	startupLogger := logger.With("component", "startup")
	defer func() {
		if err := sink.Close(); err != nil {
			writeFallbackEvent(stderr, "runtime", "shutdown_failed")
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
	if err := os.MkdirAll(ocrConfig.WorkRoot, 0o700); err != nil {
		startupLogger.Error("service.failed", "event", "service.failed", "error_code", "startup_failed")
		return errors.New("OCR workspace setup failed")
	}

	if newRecognizer == nil {
		newRecognizer = func(config ocr.Config) (managedRecognizer, error) {
			recognizer, err := ocr.New(config)
			return recognizer, err
		}
	}
	provider, err := newRecognizer(ocrConfig)
	if err != nil || provider == nil {
		startupLogger.Error("service.failed", "event", "service.failed", "error_code", "startup_failed")
		return errors.New("OCR provider startup failed")
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
	if err := apiServer.Run(ctx); err != nil {
		startupLogger.Error("service.failed", "event", "service.failed", "error_code", "shutdown_failed")
		return errors.New("API server failed")
	}
	return nil
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

func writeFallbackEvent(stderr io.Writer, component, errorCode string) {
	if stderr == nil {
		return
	}
	component = safeFallbackComponent(component)
	errorCode = safeFallbackCode(errorCode)
	_ = json.NewEncoder(stderr).Encode(map[string]any{
		"time": time.Now().UTC().Format(time.RFC3339Nano), "level": slog.LevelError.String(),
		"service": "passport-api", "component": component, "event": "service.failed",
		"msg": "service.failed", "error_code": errorCode,
	})
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
