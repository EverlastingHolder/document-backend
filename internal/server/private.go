package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"passport-backend/internal/telemetry"
)

// PrivateConfig configures the worker-only HTTP listener. It is deliberately
// independent from the public API Server and requires a concrete private bind.
type PrivateConfig struct {
	Address           string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
	MaxHeaderBytes    int
	Logger            *slog.Logger
}

// PrivateServer owns the worker-only HTTP listener.
type PrivateServer struct {
	config PrivateConfig
	http   *http.Server
	logger *slog.Logger
	used   atomic.Bool
}

// NewPrivateServer builds an HTTP server for internal worker routes only.
func NewPrivateServer(config PrivateConfig, handler http.Handler) (*PrivateServer, error) {
	if !validPrivateBindAddress(config.Address) || handler == nil ||
		config.ReadHeaderTimeout <= 0 || config.ReadTimeout <= 0 || config.WriteTimeout <= 0 ||
		config.IdleTimeout <= 0 || config.ShutdownTimeout <= 0 || config.MaxHeaderBytes <= 0 ||
		int64(config.MaxHeaderBytes) > maxHTTPHeaderBytes || config.ReadHeaderTimeout > config.ReadTimeout {
		return nil, errors.New("invalid private HTTP server configuration")
	}
	logger := telemetry.LoggerOrDiscard(config.Logger).With("component", "server")
	return &PrivateServer{
		config: config, logger: logger,
		http: &http.Server{
			Handler:           handler,
			ErrorLog:          log.New(io.Discard, "", 0),
			ReadHeaderTimeout: config.ReadHeaderTimeout,
			ReadTimeout:       config.ReadTimeout,
			WriteTimeout:      config.WriteTimeout,
			IdleTimeout:       config.IdleTimeout,
			MaxHeaderBytes:    config.MaxHeaderBytes,
		},
	}, nil
}

func validPrivateBindAddress(address string) bool {
	if !validBindAddress(address) {
		return false
	}
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	value, err := strconv.ParseUint(port, 10, 16)
	return err == nil && value > 0
}

// Run binds the configured concrete private address and drains requests on
// cancellation. A PrivateServer instance is single-use.
func (s *PrivateServer) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("nil private server context")
	}
	if !s.used.CompareAndSwap(false, true) {
		return errors.New("private server is single-use")
	}
	if ctx.Err() != nil {
		return nil
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.config.Address)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		s.logFailure("startup_failed")
		return fmt.Errorf("private listen failed: %w", err)
	}
	if ctx.Err() != nil {
		return listener.Close()
	}
	s.logger.Info("service.started", "event", "service.started")
	serveDone := make(chan error, 1)
	go func() { serveDone <- s.http.Serve(listener) }()
	select {
	case serveErr := <-serveDone:
		if serveErr == nil || errors.Is(serveErr, http.ErrServerClosed) {
			s.logger.Info("service.stopped", "event", "service.stopped")
			return nil
		}
		_ = s.http.Close()
		s.logFailure("shutdown_failed")
		return fmt.Errorf("serve private HTTP server: %w", serveErr)
	case <-ctx.Done():
	}
	s.logger.Info("service.stopping", "event", "service.stopping")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.config.ShutdownTimeout)
	err = s.http.Shutdown(shutdownCtx)
	cancel()
	if err != nil {
		closeErr := s.http.Close()
		serveErr := <-serveDone
		s.logFailure("shutdown_failed")
		return errors.Join(fmt.Errorf("private HTTP graceful shutdown: %w", err), wrapCloseError(closeErr), unexpectedServeError(serveErr))
	}
	serveErr := <-serveDone
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		s.logFailure("shutdown_failed")
		return fmt.Errorf("serve private HTTP server: %w", serveErr)
	}
	s.logger.Info("service.stopped", "event", "service.stopped")
	return nil
}

func (s *PrivateServer) logFailure(code string) {
	s.logger.Error("service.failed", "event", "service.failed", "error_code", code)
}
