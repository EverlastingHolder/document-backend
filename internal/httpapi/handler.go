package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sync/semaphore"

	"passport-backend/internal/api"
	"passport-backend/internal/metrics"
	"passport-backend/internal/passport"
	"passport-backend/internal/telemetry"
)

const (
	recognizePath           = "/v1/passport/recognize"
	statsPath               = "/v1/stats"
	clientInstallationsPath = "/v1/client-installations"
	healthPath              = "/healthz"
	readyPath               = "/readyz"
	multipartBodyOverhead   = int64(64 << 10)
)

var (
	requestIDPattern      = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	ErrImageUnprocessable = errors.New("image unprocessable")
	ErrUnavailable        = errors.New("recognizer unavailable")
)

type Recognizer interface {
	Recognize(context.Context, Image) (passport.Extraction, error)
}

type Image struct {
	Data      []byte
	MediaType string
}

type Config struct {
	MaxUploadBytes        int64
	OCRTimeout            time.Duration
	MaxConcurrent         int
	PipelineVersion       string
	AllowedClientNetworks []netip.Prefix
	Logger                *slog.Logger
	Metrics               *metrics.Store
	AsyncJobs             *AsyncJobsConfig
	ReadinessCheck        func(context.Context) error
	ReadinessTimeout      time.Duration
}

type errorResponse struct {
	RequestID string `json:"request_id"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

type handler struct {
	config         Config
	recognizer     Recognizer
	semaphore      chan struct{}
	asyncSemaphore *semaphore.Weighted
	logger         *slog.Logger
}

// NewHandler constructs the public HTTP handler with explicit operational limits.
func NewHandler(config Config, recognizer Recognizer) (http.Handler, error) {
	if config.MaxUploadBytes <= 0 || config.MaxUploadBytes > math.MaxInt64-multipartBodyOverhead ||
		config.OCRTimeout <= 0 || config.MaxConcurrent <= 0 || strings.TrimSpace(config.PipelineVersion) == "" || recognizer == nil {
		return nil, errors.New("invalid HTTP API configuration")
	}
	allowedNetworks, err := normalizeAllowedClientNetworks(config.AllowedClientNetworks)
	if err != nil {
		return nil, errors.New("invalid allowed client networks")
	}
	if err := validateAsyncConfig(config.AsyncJobs); err != nil {
		return nil, err
	}
	if (config.ReadinessCheck == nil) != (config.ReadinessTimeout <= 0) {
		return nil, errors.New("invalid readiness configuration")
	}
	config.AllowedClientNetworks = allowedNetworks
	handler := &handler{
		config:     config,
		recognizer: recognizer,
		semaphore:  make(chan struct{}, config.MaxConcurrent),
		logger:     telemetry.LoggerOrDiscard(config.Logger).With("component", "httpapi"),
	}
	if config.AsyncJobs != nil {
		handler.asyncSemaphore = semaphore.NewWeighted(int64(config.AsyncJobs.MaxConcurrentUploads))
	}
	return handler, nil
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	tracked := &loggingResponseWriter{ResponseWriter: w}
	w = tracked
	if r.URL.Path == healthPath || r.URL.Path == readyPath {
		h.serveHealth(w, r)
		return
	}
	if r.URL.Path == clientInstallationsPath {
		w.Header().Set("Cache-Control", "no-store")
	}
	if _, ok := receiptJobIDFromPath(r.URL.Path); ok {
		w.Header().Set("Cache-Control", "no-store")
	}
	if r.Method == http.MethodGet {
		if _, ok := jobIDFromPath(r.URL.Path); ok {
			w.Header().Set("Cache-Control", "no-store")
		}
	}
	requestID, validID, err := requestIDFor(r)
	if err != nil {
		return
	}
	r = r.WithContext(telemetry.WithRequestID(r.Context(), requestID))
	requestLogger := h.logger.With("request_id", requestID)
	completionErrorCode := ""
	logCompletion := func(errorCode string) {
		if errorCode == "" {
			if completionErrorCode != "" {
				errorCode = completionErrorCode
			} else if _, isReceipt := receiptJobIDFromPath(r.URL.Path); isReceipt && tracked.status == http.StatusConflict {
				errorCode = "job_not_terminal"
			} else {
				errorCode = errorCodeForStatus(tracked.status)
				if tracked.status == 0 && r.Context().Err() != nil {
					errorCode = "cancelled"
				}
			}
		}
		if r.URL.Path != statsPath && h.config.Metrics != nil {
			_ = h.config.Metrics.ObserveHTTP(tracked.status, time.Since(started), tracked.bytes)
		}
		level := requestLogLevel(tracked.status)
		if errorCode == "request_aborted" {
			level = slog.LevelWarn
		}
		if errorCode == "internal_panic" {
			level = slog.LevelError
		}
		requestLogger.Log(r.Context(), level, "http.request.completed",
			"event", "http.request.completed", "method", safeMethod(r.Method), "route", safeRoute(r.URL.Path),
			"status", tracked.status, "duration_ms", elapsedMillis(started), "response_bytes", tracked.bytes,
			"error_code", errorCode)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			if recovered == http.ErrAbortHandler {
				logCompletion("request_aborted")
				panic(http.ErrAbortHandler)
			}
			requestLogger.Error("http.request.panic", "event", "http.request.panic", "error_code", "internal_panic")
			logCompletion("internal_panic")
			panic(http.ErrAbortHandler)
		}
		logCompletion("")
	}()
	if !validID {
		writeError(w, requestID, http.StatusBadRequest, "invalid_request", "Некорректный запрос.")
		return
	}
	if r.URL.Path == jobsPath {
		h.serveJobSubmission(w, r, requestID, &completionErrorCode)
		return
	}
	if jobID, ok := receiptJobIDFromPath(r.URL.Path); ok {
		h.serveJobReceipt(w, r, jobID, requestID, &completionErrorCode)
		return
	}
	if r.URL.Path == clientInstallationsPath {
		h.serveClientInstallation(w, r, requestID, &completionErrorCode)
		return
	}
	if jobID, ok := jobIDFromPath(r.URL.Path); ok {
		h.serveJobStatus(w, r, jobID, requestID, &completionErrorCode)
		return
	}
	if r.URL.Path != recognizePath {
		if r.URL.Path == statsPath {
			h.serveStats(w, r, requestID, &completionErrorCode)
			return
		}
		writeError(w, requestID, http.StatusNotFound, "not_found", "Ресурс не найден.")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, requestID, http.StatusMethodNotAllowed, "method_not_allowed", "Метод запроса не поддерживается.")
		return
	}
	if r.Context().Err() != nil {
		return
	}
	if !remoteAddrAllowed(r.RemoteAddr, h.config.AllowedClientNetworks) {
		writeError(w, requestID, http.StatusForbidden, "access_denied", "Доступ запрещён.")
		return
	}
	select {
	case h.semaphore <- struct{}{}:
		defer func() { <-h.semaphore }()
	default:
		writeError(w, requestID, http.StatusTooManyRequests, "concurrency_limit_reached", "Сервис временно занят. Повторите попытку позже.")
		return
	}

	image, status, code, message := h.readImage(w, r)
	if status != 0 {
		if r.Context().Err() != nil {
			return
		}
		writeError(w, requestID, status, code, message)
		return
	}
	if r.Context().Err() != nil {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.config.OCRTimeout)
	defer cancel()
	extraction, recognizeErr := h.recognizer.Recognize(ctx, image)
	if r.Context().Err() != nil {
		return
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(recognizeErr, context.DeadlineExceeded) {
		writeError(w, requestID, http.StatusGatewayTimeout, "processing_timeout", "Время обработки изображения истекло.")
		return
	}
	if recognizeErr != nil {
		if errors.Is(recognizeErr, ErrImageUnprocessable) {
			writeError(w, requestID, http.StatusUnprocessableEntity, "image_unprocessable", "Изображение не удалось обработать. Проверьте файл и попробуйте ещё раз.")
			return
		}
		if errors.Is(recognizeErr, ErrUnavailable) {
			writeError(w, requestID, http.StatusServiceUnavailable, "ocr_unavailable", "Сервис распознавания временно недоступен.")
			return
		}
		writeError(w, requestID, http.StatusInternalServerError, "processing_failed", "Не удалось обработать запрос.")
		return
	}
	response, err := api.MapExtraction(extraction, requestID, h.config.PipelineVersion)
	if err != nil {
		writeError(w, requestID, http.StatusInternalServerError, "processing_failed", "Не удалось обработать запрос.")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Request-ID", requestID)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(response)
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *loggingResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *loggingResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *loggingResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	w.bytes += int64(n)
	return n, err
}

func safeMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return method
	default:
		return "OTHER"
	}
}

func safeRoute(path string) string {
	if path == healthPath || path == readyPath {
		return path
	}
	if path == clientInstallationsPath {
		return clientInstallationsPath
	}
	if path == recognizePath {
		return recognizePath
	}
	if path == jobsPath {
		return jobsPath
	}
	if _, ok := jobIDFromPath(path); ok {
		return jobsPath + "/{job_id}"
	}
	if _, ok := receiptJobIDFromPath(path); ok {
		return jobsPath + "/{job_id}/receipt"
	}
	if path == statsPath {
		return statsPath
	}
	return "unmatched"
}

func jobIDFromPath(path string) (string, bool) {
	prefix := jobsPath + "/"
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	jobID := strings.TrimPrefix(path, prefix)
	if jobID == "" || strings.Contains(jobID, "/") {
		return "", false
	}
	return jobID, true
}

func receiptJobIDFromPath(path string) (string, bool) {
	prefix := jobsPath + "/"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, "/receipt") {
		return "", false
	}
	jobID := strings.TrimSuffix(strings.TrimPrefix(path, prefix), "/receipt")
	if jobID == "" || strings.Contains(jobID, "/") {
		return "", false
	}
	return jobID, true
}

type statsResponse struct {
	RequestID  string           `json:"request_id"`
	Statistics metrics.Snapshot `json:"statistics"`
}

func (h *handler) serveStats(w http.ResponseWriter, r *http.Request, requestID string, completionErrorCode *string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, requestID, http.StatusMethodNotAllowed, "method_not_allowed", "Метод запроса не поддерживается.")
		return
	}
	if !remoteAddrAllowed(r.RemoteAddr, h.config.AllowedClientNetworks) {
		writeError(w, requestID, http.StatusForbidden, "access_denied", "Доступ запрещён.")
		return
	}
	if h.config.Metrics == nil {
		*completionErrorCode = "statistics_unavailable"
		writeError(w, requestID, http.StatusServiceUnavailable, "statistics_unavailable", "Статистика временно недоступна.")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Request-ID", requestID)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(statsResponse{RequestID: requestID, Statistics: h.config.Metrics.Snapshot()})
}

func elapsedMillis(started time.Time) int64 { return time.Since(started).Milliseconds() }

func requestLogLevel(status int) slog.Level {
	switch {
	case status >= 500:
		return slog.LevelError
	case status >= 400:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

func errorCodeForStatus(status int) string {
	switch status {
	case http.StatusOK, http.StatusCreated, http.StatusAccepted, http.StatusNoContent:
		return "ok"
	case http.StatusBadRequest:
		return "invalid_request"
	case http.StatusForbidden:
		return "access_denied"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusConflict:
		return "idempotency_conflict"
	case http.StatusRequestEntityTooLarge:
		return "upload_too_large"
	case http.StatusUnsupportedMediaType:
		return "unsupported_media_type"
	case http.StatusUnprocessableEntity:
		return "image_unprocessable"
	case http.StatusTooManyRequests:
		return "concurrency_limit_reached"
	case http.StatusInternalServerError:
		return "processing_failed"
	case http.StatusServiceUnavailable:
		return "ocr_unavailable"
	case http.StatusGatewayTimeout:
		return "processing_timeout"
	case http.StatusGone:
		return "job_result_gone"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusMethodNotAllowed:
		return "method_not_allowed"
	default:
		return ""
	}
}

func (h *handler) readImage(w http.ResponseWriter, r *http.Request) (Image, int, string, string) {
	contentType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "multipart/form-data" || params["boundary"] == "" {
		return Image{}, http.StatusBadRequest, "invalid_request", "Некорректный запрос."
	}
	bodyLimit := h.config.MaxUploadBytes + multipartBodyOverhead - 1
	if r.ContentLength > bodyLimit {
		return Image{}, http.StatusRequestEntityTooLarge, "upload_too_large", "Размер файла превышает допустимый предел."
	}
	limitedBody := http.MaxBytesReader(w, r.Body, bodyLimit)
	reader := multipart.NewReader(limitedBody, params["boundary"])
	var image Image
	fileFound := false
	for {
		part, nextErr := reader.NextPart()
		if isMaxBytesError(nextErr) {
			return Image{}, http.StatusRequestEntityTooLarge, "upload_too_large", "Размер файла превышает допустимый предел."
		}
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return Image{}, http.StatusBadRequest, "invalid_request", "Некорректный запрос."
		}
		if part.FormName() != "file" || part.FileName() == "" || fileFound {
			return Image{}, http.StatusBadRequest, "invalid_request", "Некорректный запрос."
		}
		fileFound = true
		mediaType, _, mediaErr := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if mediaErr != nil || !supportedMediaType(mediaType) {
			return Image{}, http.StatusUnsupportedMediaType, "unsupported_media_type", "Формат изображения не поддерживается."
		}
		data, readErr := io.ReadAll(io.LimitReader(part, h.config.MaxUploadBytes+1))
		if readErr != nil {
			if isMaxBytesError(readErr) {
				return Image{}, http.StatusRequestEntityTooLarge, "upload_too_large", "Размер файла превышает допустимый предел."
			}
			return Image{}, http.StatusBadRequest, "invalid_request", "Некорректный запрос."
		}
		if int64(len(data)) > h.config.MaxUploadBytes {
			return Image{}, http.StatusRequestEntityTooLarge, "upload_too_large", "Размер файла превышает допустимый предел."
		}
		if len(data) == 0 {
			return Image{}, http.StatusBadRequest, "invalid_request", "Некорректный запрос."
		}
		image = Image{Data: data, MediaType: mediaType}
	}
	if _, err := io.Copy(io.Discard, limitedBody); err != nil {
		if isMaxBytesError(err) {
			return Image{}, http.StatusRequestEntityTooLarge, "upload_too_large", "Размер файла превышает допустимый предел."
		}
		return Image{}, http.StatusBadRequest, "invalid_request", "Некорректный запрос."
	}
	if !fileFound {
		return Image{}, http.StatusBadRequest, "invalid_request", "Некорректный запрос."
	}
	return image, 0, "", ""
}

func isMaxBytesError(err error) bool {
	var maxBytesErr *http.MaxBytesError
	return errors.As(err, &maxBytesErr)
}

func supportedMediaType(mediaType string) bool {
	switch strings.ToLower(mediaType) {
	case "image/jpeg", "image/png", "image/webp":
		return true
	default:
		return false
	}
}

func requestIDFor(r *http.Request) (string, bool, error) {
	values := r.Header.Values("X-Request-ID")
	if len(values) == 0 {
		id, err := newRequestID()
		return id, true, err
	}
	if len(values) != 1 || !requestIDPattern.MatchString(values[0]) {
		id, err := newRequestID()
		return id, false, err
	}
	return values[0], true, nil
}

func newRequestID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	var encoded [36]byte
	hex.Encode(encoded[0:8], id[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], id[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], id[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], id[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], id[10:16])
	return string(encoded[:]), nil
}

func writeError(w http.ResponseWriter, requestID string, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Request-ID", requestID)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{RequestID: requestID, Code: code, Message: message})
}
