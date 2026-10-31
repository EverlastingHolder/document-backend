// Package server composes the public HTTP handler with an owned HTTP server.
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
	"net/netip"
	"strconv"
	"sync/atomic"
	"time"

	"passport-backend/internal/httpapi"
	"passport-backend/internal/metrics"
	"passport-backend/internal/telemetry"
)

const maxHTTPHeaderBytes = int64(^uint64(0)>>1) - 4096

// Config contains explicit listener, request, and shutdown limits. No
// deployment defaults are applied by this package.
type Config struct {
	Address           string
	Logging           telemetry.Config
	HTTP              httpapi.Config
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
	MaxHeaderBytes    int
	Logger            *slog.Logger
	Metrics           *metrics.Store
}

// Server is a single-use composition of the HTTP API and its listener.
type Server struct {
	config Config
	http   *http.Server
	used   atomic.Bool
	logger *slog.Logger
}

// New validates the complete runtime configuration and builds the existing
// HTTP API handler around the required recognizer.
func New(config Config, recognizer httpapi.Recognizer) (*Server, error) {
	if !validBindAddress(config.Address) || len(config.HTTP.AllowedClientNetworks) == 0 ||
		config.ReadHeaderTimeout <= 0 || config.ReadTimeout <= 0 ||
		config.WriteTimeout <= 0 || config.IdleTimeout <= 0 ||
		config.ShutdownTimeout <= 0 || config.MaxHeaderBytes <= 0 ||
		int64(config.MaxHeaderBytes) > maxHTTPHeaderBytes ||
		config.ReadHeaderTimeout > config.ReadTimeout ||
		config.WriteTimeout-config.ReadTimeout <= config.HTTP.OCRTimeout {
		return nil, errors.New("invalid server configuration")
	}
	config.HTTP.AllowedClientNetworks = append([]netip.Prefix(nil), config.HTTP.AllowedClientNetworks...)
	for i, prefix := range config.HTTP.AllowedClientNetworks {
		config.HTTP.AllowedClientNetworks[i] = prefix.Masked()
	}
	if config.Metrics != nil {
		if config.HTTP.Metrics != nil && config.HTTP.Metrics != config.Metrics {
			return nil, errors.New("HTTP and server metrics stores must match")
		}
		config.HTTP.Metrics = config.Metrics
	}
	logger := telemetry.LoggerOrDiscard(config.Logger).With("component", "server")
	if config.HTTP.Logger == nil {
		config.HTTP.Logger = logger.With("component", "httpapi")
	}
	handler, err := httpapi.NewHandler(config.HTTP, recognizer)
	if err != nil {
		return nil, fmt.Errorf("invalid HTTP API configuration: %w", err)
	}
	return &Server{
		config: config,
		logger: logger,
		http: &http.Server{
			Handler: handler,
			// Keep net/http's unstructured diagnostics disabled; request panic
			// telemetry is emitted by the handler without panic values.
			ErrorLog:          log.New(io.Discard, "", 0),
			ReadHeaderTimeout: config.ReadHeaderTimeout,
			ReadTimeout:       config.ReadTimeout,
			WriteTimeout:      config.WriteTimeout,
			IdleTimeout:       config.IdleTimeout,
			MaxHeaderBytes:    config.MaxHeaderBytes,
		},
	}, nil
}

// Run binds the configured TCP address and serves until cancellation or an
// unexpected server error. A pre-cancelled context returns without binding.
func (s *Server) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("nil server context")
	}
	if !s.used.CompareAndSwap(false, true) {
		return errors.New("server is single-use")
	}
	if ctx.Err() != nil {
		return nil
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.config.Address)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		s.logServiceFailure("startup_failed")
		return fmt.Errorf("listen failed: %w", err)
	}
	if ctx.Err() != nil {
		return listener.Close()
	}
	return s.serveClaimed(ctx, listener)
}

// Serve accepts an already-created listener and owns it for the duration of
// the call, including when the context is already cancelled or this server
// has already been used.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	if listener == nil {
		return errors.New("nil server listener")
	}
	if ctx == nil {
		return errors.Join(errors.New("nil server context"), listener.Close())
	}
	if !s.used.CompareAndSwap(false, true) {
		return errors.Join(errors.New("server is single-use"), listener.Close())
	}
	if ctx.Err() != nil {
		return listener.Close()
	}
	if !validTCPListenerAddress(listener) {
		s.logServiceFailure("startup_failed")
		return errors.Join(errors.New("listener address must be a specific local TCP address"), listener.Close())
	}
	return s.serveClaimed(ctx, listener)
}

func validBindAddress(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil || !numericPort(port) {
		return false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Is4In6() {
		return false
	}
	return isLocalAddress(ip)
}

func validTCPListenerAddress(listener net.Listener) bool {
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok || addr == nil || addr.Port <= 0 || addr.Port > 65535 || len(addr.IP) == 0 {
		return false
	}
	var ip netip.Addr
	if v4 := addr.IP.To4(); v4 != nil {
		var raw [4]byte
		copy(raw[:], v4)
		ip = netip.AddrFrom4(raw)
	} else {
		var valid bool
		ip, valid = netip.AddrFromSlice(addr.IP)
		if !valid || ip.Is4In6() {
			return false
		}
	}
	if addr.Zone != "" {
		if !ip.Is6() || !ip.IsLinkLocalUnicast() {
			return false
		}
		ip = ip.WithZone(addr.Zone)
	}
	return isLocalAddress(ip)
}

func numericPort(port string) bool {
	if port == "" {
		return false
	}
	for _, digit := range port {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	_, err := strconv.ParseUint(port, 10, 16)
	return err == nil
}

func isLocalAddress(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Is4In6() {
		return false
	}
	if ip.Zone() != "" {
		if !ip.Is6() || !ip.IsLinkLocalUnicast() {
			return false
		}
		ip = ip.WithZone("")
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

func (s *Server) serveClaimed(ctx context.Context, listener net.Listener) error {
	s.logger.Info("service.started", "event", "service.started")
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- s.http.Serve(listener)
	}()

	select {
	case serveErr := <-serveDone:
		err := s.finishServe(serveErr)
		if err == nil {
			s.logger.Info("service.stopped", "event", "service.stopped")
		}
		return err
	case <-ctx.Done():
	}

	s.logger.Info("service.stopping", "event", "service.stopping")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.config.ShutdownTimeout)
	shutdownErr := s.http.Shutdown(shutdownCtx)
	cancel()
	if shutdownErr != nil {
		closeErr := s.http.Close()
		serveErr := <-serveDone
		s.logServiceFailure("shutdown_failed")
		return errors.Join(
			fmt.Errorf("graceful shutdown: %w", shutdownErr),
			wrapCloseError(closeErr),
			unexpectedServeError(serveErr),
		)
	}
	serveErr := <-serveDone
	if err := s.finishServe(serveErr); err != nil {
		return err
	}
	s.logger.Info("service.stopped", "event", "service.stopped")
	return nil
}

func (s *Server) finishServe(serveErr error) error {
	if serveErr == nil || errors.Is(serveErr, http.ErrServerClosed) {
		return nil
	}
	closeErr := s.http.Close()
	s.logServiceFailure("shutdown_failed")
	return errors.Join(fmt.Errorf("serve HTTP server: %w", serveErr), wrapCloseError(closeErr))
}

func (s *Server) logServiceFailure(code string) {
	s.logger.Error("service.failed", "event", "service.failed", "error_code", code)
}

func unexpectedServeError(err error) error {
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("serve HTTP server: %w", err)
}

func wrapCloseError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("close HTTP server: %w", err)
}
