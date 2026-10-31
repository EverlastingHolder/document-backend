// Package telemetry provides bounded, privacy-filtered JSON logs for the API.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config selects a private JSON-lines file and its bounded rotation policy.
type Config struct {
	Path         string `json:"path"`
	Level        string `json:"level"`
	MaxSizeBytes int64  `json:"max_size_bytes"`
	MaxBackups   int    `json:"max_backups"`
}

// Sink owns the configured log file and its slog logger.
type Sink struct {
	writer *rollingWriter
	logger *slog.Logger
}

const maxEventBytes = 8192

// Open validates and opens a private rolling log file.
func Open(config Config) (*Sink, error) {
	level, ok := parseLevel(config.Level)
	if !ok || strings.TrimSpace(config.Path) == "" || config.MaxSizeBytes < maxEventBytes || config.MaxBackups < 0 || config.MaxBackups > 10 {
		return nil, errors.New("invalid logging configuration")
	}
	path, err := filepath.Abs(config.Path)
	if err != nil {
		return nil, errors.New("invalid logging path")
	}
	if err := ensurePrivateParent(filepath.Dir(path)); err != nil {
		return nil, errors.New("unsafe logging directory")
	}
	writer, err := newRollingWriter(path, config.MaxSizeBytes, config.MaxBackups)
	if err != nil {
		return nil, errors.New("logging output unavailable")
	}
	handler := &safeHandler{next: slog.NewJSONHandler(writer, &slog.HandlerOptions{Level: level})}
	return &Sink{writer: writer, logger: slog.New(handler)}, nil
}

// Logger returns the structured logger owned by this sink.
func (s *Sink) Logger() *slog.Logger {
	if s == nil || s.logger == nil {
		return LoggerOrDiscard(nil)
	}
	return s.logger
}

// Close flushes and closes the current log file.
func (s *Sink) Close() error {
	if s == nil || s.writer == nil {
		return nil
	}
	return s.writer.Close()
}

// LoggerOrDiscard returns logger when configured, or a no-op logger otherwise.
func LoggerOrDiscard(logger *slog.Logger) *slog.Logger {
	if logger != nil {
		return logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type requestIDKey struct{}

// WithRequestID stores a validated request UUID in context without changing its case.
func WithRequestID(ctx context.Context, id string) context.Context {
	if ctx == nil || !validRequestID(id) {
		return ctx
	}
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID returns the validated request UUID, or an empty string.
func RequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(requestIDKey{}).(string)
	if !validRequestID(id) {
		return ""
	}
	return id
}

func validRequestID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, r := range id {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
				return false
			}
		}
	}
	return true
}

func parseLevel(value string) (slog.Level, bool) {
	switch strings.ToLower(value) {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default:
		return 0, false
	}
}

func ensurePrivateParent(parent string) error {
	if err := rejectSymlinkComponents(parent); err != nil {
		return err
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("unsafe log directory")
	}
	return nil
}

func rejectSymlinkComponents(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(absolute)
	rest := strings.TrimPrefix(absolute, volume)
	current := volume + string(os.PathSeparator)
	for _, component := range strings.Split(strings.TrimPrefix(rest, string(os.PathSeparator)), string(os.PathSeparator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlink in logging path")
		}
	}
	return nil
}

type rollingWriter struct {
	mu         sync.Mutex
	path       string
	maxSize    int64
	maxBackups int
	file       *os.File
	lockFile   *os.File
	failed     atomic.Bool
	closed     bool
}

func newRollingWriter(path string, maxSize int64, maxBackups int) (*rollingWriter, error) {
	lockPath := path + ".lock"
	if err := validateExistingLog(lockPath); err != nil {
		return nil, err
	}
	lockFile, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockLogFile(lockFile); err != nil {
		_ = lockFile.Close()
		return nil, err
	}
	for i := 0; i <= maxBackups; i++ {
		candidate := pathForBackup(path, i)
		if err := validateExistingLog(candidate); err != nil {
			_ = unlockLogFile(lockFile)
			_ = lockFile.Close()
			return nil, err
		}
		if info, err := os.Stat(candidate); err == nil && info.Size() > maxSize {
			_ = unlockLogFile(lockFile)
			_ = lockFile.Close()
			return nil, errors.New("existing log exceeds configured size")
		}
	}
	if err := rejectStaleBackups(path, maxBackups); err != nil {
		_ = unlockLogFile(lockFile)
		_ = lockFile.Close()
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		_ = unlockLogFile(lockFile)
		_ = lockFile.Close()
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = unlockLogFile(lockFile)
		_ = lockFile.Close()
		return nil, err
	}
	return &rollingWriter{path: path, maxSize: maxSize, maxBackups: maxBackups, file: file, lockFile: lockFile}, nil
}

func pathForBackup(path string, index int) string {
	if index == 0 {
		return path
	}
	return fmt.Sprintf("%s.%d", path, index)
}

func validateExistingLog(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("unsafe existing log file")
	}
	return nil
}

func rejectStaleBackups(path string, maxBackups int) error {
	paths, err := filepath.Glob(path + ".*")
	if err != nil {
		return err
	}
	for _, candidate := range paths {
		if candidate == path+".lock" {
			continue
		}
		var index int
		if _, err := fmt.Sscanf(strings.TrimPrefix(candidate, path+"."), "%d", &index); err != nil {
			continue
		}
		if index > maxBackups {
			return errors.New("existing log backup exceeds configured retention")
		}
	}
	return nil
}

func (w *rollingWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, errors.New("log sink closed")
	}
	if int64(len(data)) > maxEventBytes || int64(len(data)) > w.maxSize {
		w.alertOnce()
		return len(data), nil
	}
	info, err := w.file.Stat()
	if err != nil {
		w.alertOnce()
		return len(data), nil
	}
	if info.Size()+int64(len(data)) > w.maxSize {
		if err := w.rotate(); err != nil {
			w.alertOnce()
			return len(data), nil
		}
	}
	if _, err := w.file.Write(data); err != nil {
		w.alertOnce()
	}
	return len(data), nil
}

func (w *rollingWriter) rotate() error {
	if err := w.file.Close(); err != nil {
		return err
	}
	if w.maxBackups == 0 {
		if err := os.Remove(w.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else {
		oldest := pathForBackup(w.path, w.maxBackups)
		if err := validateExistingLog(oldest); err != nil {
			return err
		}
		if err := os.Remove(oldest); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		for i := w.maxBackups - 1; i >= 1; i-- {
			source, target := pathForBackup(w.path, i), pathForBackup(w.path, i+1)
			if err := validateExistingLog(source); err != nil {
				return err
			}
			if err := os.Rename(source, target); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if err := validateExistingLog(w.path); err != nil {
			return err
		}
		if err := os.Rename(w.path, pathForBackup(w.path, 1)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	file, err := os.OpenFile(w.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	w.file = file
	return nil
}

func (w *rollingWriter) alertOnce() {
	if w.failed.CompareAndSwap(false, true) {
		_, _ = fmt.Fprintln(os.Stderr, `{"time":"`+time.Now().UTC().Format(time.RFC3339Nano)+`","level":"ERROR","service":"passport-api","component":"runtime","event":"logging.failed","msg":"logging.failed","error_code":"write_failed"}`)
	}
}

func (w *rollingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	syncErr := w.file.Sync()
	closeErr := w.file.Close()
	unlockErr := unlockLogFile(w.lockFile)
	lockCloseErr := w.lockFile.Close()
	closeResult := errors.Join(syncErr, closeErr, unlockErr, lockCloseErr)
	if closeResult != nil {
		w.alertOnce()
	}
	return closeResult
}

type safeHandler struct {
	next  slog.Handler
	attrs []slog.Attr
}

func (h *safeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}
func (h *safeHandler) WithGroup(_ string) slog.Handler {
	return &safeHandler{next: h.next, attrs: append([]slog.Attr(nil), h.attrs...)}
}
func (h *safeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	filtered := filterAttrs(attrs)
	return &safeHandler{next: h.next, attrs: append(append([]slog.Attr(nil), h.attrs...), filtered...)}
}
func (h *safeHandler) Handle(ctx context.Context, record slog.Record) error {
	filtered := filterAttrsFromRecord(record)
	allAttrs := dedupeAttrs(filterAttrs(append(append([]slog.Attr(nil), h.attrs...), filtered...)))
	event := eventFrom(record.Message, allAttrs)
	clone := slog.NewRecord(record.Time.UTC(), record.Level, event, record.PC)
	component := "runtime"
	for _, attr := range allAttrs {
		if attr.Key == "component" && attr.Value.Kind() == slog.KindString {
			component = attr.Value.String()
		}
	}
	clone.AddAttrs(slog.String("service", "passport-api"), slog.String("component", component), slog.String("event", event))
	if event == "logging.rejected" {
		clone.AddAttrs(slog.String("error_code", "invalid_event"))
	}
	for _, attr := range allAttrs {
		if attr.Key != "component" && attr.Key != "event" && attr.Key != "service" && !(event == "logging.rejected" && attr.Key == "error_code") {
			clone.AddAttrs(attr)
		}
	}
	return h.next.Handle(ctx, clone)
}

func dedupeAttrs(attrs []slog.Attr) []slog.Attr {
	result := make([]slog.Attr, 0, len(attrs))
	indices := make(map[string]int, len(attrs))
	for _, attr := range attrs {
		if index, exists := indices[attr.Key]; exists {
			result[index] = attr
			continue
		}
		indices[attr.Key] = len(result)
		result = append(result, attr)
	}
	return result
}

func filterAttrsFromRecord(record slog.Record) []slog.Attr {
	attrs := make([]slog.Attr, 0, record.NumAttrs())
	record.Attrs(func(attr slog.Attr) bool { attrs = append(attrs, attr); return true })
	return filterAttrs(attrs)
}

func filterAttrs(attrs []slog.Attr) []slog.Attr {
	filtered := make([]slog.Attr, 0, len(attrs))
	for _, attr := range attrs {
		attr.Value = attr.Value.Resolve()
		if !attrAllowed(attr.Key, attr.Value) {
			continue
		}
		filtered = append(filtered, attr)
	}
	return filtered
}

func attrAllowed(key string, value slog.Value) bool {
	if value.Kind() == slog.KindGroup {
		return false
	}
	switch key {
	case "service":
		return value.String() == "passport-api"
	case "component":
		return value.Kind() == slog.KindString && oneOf(value.String(), "api", "httpapi", "server", "ocr", "worker", "runtime", "startup")
	case "event":
		return value.Kind() == slog.KindString && eventAllowed(value.String())
	case "request_id":
		return value.Kind() == slog.KindString && validRequestID(value.String())
	case "method":
		return value.Kind() == slog.KindString && oneOf(value.String(), "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "OTHER")
	case "route":
		return value.Kind() == slog.KindString && oneOf(value.String(), "/v1/passport/recognize", "/v1/stats", "unmatched")
	case "status":
		return value.Kind() == slog.KindInt64 && value.Int64() >= 0 && value.Int64() <= 599
	case "duration_ms", "response_bytes", "phase_ms":
		return value.Kind() == slog.KindInt64 && value.Int64() >= 0
	case "error_code":
		return value.Kind() == slog.KindString && oneOf(value.String(), "invalid_request", "access_denied", "upload_too_large", "unsupported_media_type", "image_unprocessable", "concurrency_limit_reached", "processing_failed", "ocr_unavailable", "processing_timeout", "not_found", "method_not_allowed", "statistics_unavailable", "metrics_write_failed", "internal_panic", "request_aborted", "startup_failed", "shutdown_failed", "worker_protocol_failed", "worker_failed", "worker_unavailable", "write_failed", "ok", "deadline_exceeded", "runtime_unavailable", "pipeline_failed", "cancelled", "invalid_image", "invalid_event")
	case "phase":
		return value.Kind() == slog.KindString && oneOf(value.String(), "image_validation", "ocr", "baseline_and_plan", "vl", "final_extraction", "cleanup", "worker_launch", "status_validation")
	case "stage":
		return value.Kind() == slog.KindString && oneOf(value.String(), "ocr", "baseline", "vl", "extract")
	case "stage_scope":
		return value.Kind() == slog.KindString && value.String() == "process"
	case "outcome":
		return value.Kind() == slog.KindString && oneOf(value.String(), "started", "completed", "success", "failure")
	case "phase_status":
		return value.Kind() == slog.KindString && oneOf(value.String(), "completed", "failed", "skipped")
	default:
		return false
	}
}

func eventFrom(message string, attrs []slog.Attr) string {
	if eventAllowed(message) {
		return message
	}
	for _, attr := range attrs {
		if attr.Key == "event" && attr.Value.Kind() == slog.KindString && eventAllowed(attr.Value.String()) {
			return attr.Value.String()
		}
	}
	return "logging.rejected"
}

func eventAllowed(value string) bool {
	return oneOf(value, "service.started", "service.stopping", "service.stopped", "service.failed", "logging.failed", "logging.rejected", "http.request.completed", "http.request.panic", "ocr.job.started", "ocr.job.completed", "ocr.job.failed", "ocr.stage.started", "ocr.stage.completed", "ocr.stage.failed", "ocr.telemetry.rejected", "metrics.persist.failed")
}

func oneOf(value string, options ...string) bool {
	for _, option := range options {
		if value == option {
			return true
		}
	}
	return false
}

var _ slog.Handler = (*safeHandler)(nil)
var _ io.WriteCloser = (*rollingWriter)(nil)
var _ = time.UTC
