// Package workerapi exposes authenticated worker-only lifecycle callbacks on
// a separate private listener. It contains no public job status routes.
package workerapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"passport-backend/internal/clientauth"
	"passport-backend/internal/jobs"
	"passport-backend/internal/jsonstrict"
	"passport-backend/internal/metrics"
	"passport-backend/internal/telemetry"
)

const (
	claimPath    = "/internal/worker/v1/claim"
	completePath = "/internal/worker/v1/complete"
	failPath     = "/internal/worker/v1/fail"
	rejectPath   = "/internal/worker/v1/reject"
	maxBodyBytes = 64 << 10
)

type Authenticator interface {
	AuthenticateWorker(authorizationHeaders []string) (clientauth.WorkerPrincipal, error)
}

type Executor interface {
	Claim(context.Context, jobs.Envelope) (jobs.ClaimResult, error)
	CompleteRaw(context.Context, jobs.Completion) (jobs.DeliveryDecision, error)
	Fail(context.Context, jobs.Failure) (jobs.DeliveryDecision, error)
	Reject(context.Context, jobs.Rejection) (jobs.DeliveryDecision, error)
}

type Config struct {
	Authenticator    Authenticator
	Executor         Executor
	Metrics          *metrics.Store
	Logger           *slog.Logger
	WaitPollInterval time.Duration
}

type handler struct {
	config Config
	logger *slog.Logger
}

type errorResponse struct {
	Code    string `json:"error_code"`
	Message string `json:"message"`
}

type decisionResponse struct {
	Decision     jobs.DeliveryDecision `json:"decision"`
	RetryAfterMS int64                 `json:"retry_after_ms,omitempty"`
}

type completionRequest struct {
	jobs.Envelope
	AttemptToken string             `json:"attempt_token"`
	Fields       map[string]*string `json:"fields"`
}

type failureRequest struct {
	jobs.Envelope
	AttemptToken string `json:"attempt_token"`
	ErrorCode    string `json:"error_code"`
}

// NewHandler validates the independent worker credential and lifecycle service.
func NewHandler(config Config) (http.Handler, error) {
	if config.Authenticator == nil || config.Executor == nil || config.WaitPollInterval <= 0 {
		return nil, errors.New("invalid worker API configuration")
	}
	return &handler{config: config, logger: telemetry.LoggerOrDiscard(config.Logger).With("component", "workerapi")}, nil
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	tracked := &trackedResponseWriter{ResponseWriter: w}
	completionCode := "ok"
	defer func() {
		if tracked.status == 0 {
			tracked.status = http.StatusOK
		}
		if h.config.Metrics != nil {
			_ = h.config.Metrics.ObserveHTTP(tracked.status, time.Since(started), tracked.bytes)
		}
		h.logger.Log(r.Context(), workerLogLevel(tracked.status), "worker.request.completed",
			"event", "worker.request.completed", "method", workerMethod(r.Method),
			"route", workerRoute(r.URL.Path), "status", tracked.status,
			"duration_ms", time.Since(started).Milliseconds(), "response_bytes", tracked.bytes,
			"error_code", completionCode)
	}()

	path := r.URL.Path
	if !workerRouteKnown(path) {
		completionCode = "worker_invalid_request"
		writeError(tracked, http.StatusNotFound, completionCode)
		return
	}
	if r.Method != http.MethodPost {
		tracked.Header().Set("Allow", http.MethodPost)
		completionCode = "worker_invalid_request"
		writeError(tracked, http.StatusMethodNotAllowed, completionCode)
		return
	}
	if r.URL.RawQuery != "" {
		completionCode = "worker_invalid_request"
		writeError(tracked, http.StatusBadRequest, completionCode)
		return
	}
	if _, err := h.config.Authenticator.AuthenticateWorker(r.Header.Values("Authorization")); err != nil {
		tracked.Header().Set("WWW-Authenticate", `Bearer realm="passport-worker"`)
		completionCode = "worker_unauthorized"
		writeError(tracked, http.StatusUnauthorized, completionCode)
		return
	}
	body, ok := readStrictBody(tracked, r)
	if !ok {
		completionCode = "worker_invalid_request"
		writeError(tracked, http.StatusBadRequest, completionCode)
		return
	}

	switch path {
	case claimPath:
		var envelope jobs.Envelope
		if !decodeStrict(body, &envelope) || !validEnvelope(envelope) {
			completionCode = "worker_invalid_request"
			writeError(tracked, http.StatusBadRequest, completionCode)
			return
		}
		result, err := h.config.Executor.Claim(r.Context(), envelope)
		if err != nil {
			h.writeExecutorError(tracked, err, &completionCode)
			return
		}
		if !validClaimResult(result) {
			completionCode = "worker_protocol_error"
			writeError(tracked, http.StatusServiceUnavailable, "worker_storage_unavailable")
			return
		}
		writeJSON(tracked, http.StatusOK, result)
	case completePath:
		var request completionRequest
		if !decodeStrict(body, &request) || !validEnvelope(request.Envelope) || !validUUID(request.AttemptToken) {
			completionCode = "worker_invalid_request"
			writeError(tracked, http.StatusBadRequest, completionCode)
			return
		}
		completion := jobs.Completion{Envelope: request.Envelope, AttemptToken: request.AttemptToken, Fields: request.Fields}
		decision, err := h.config.Executor.CompleteRaw(r.Context(), completion)
		if errors.Is(err, jobs.ErrConflictingCompletion) && decision == jobs.DecisionAck {
			completionCode = "worker_completion_conflict"
			writeJSON(tracked, http.StatusOK, decisionResponse{Decision: jobs.DecisionAck})
			return
		}
		h.writeDurableDecision(tracked, decision, err, &completionCode)
	case failPath:
		var request failureRequest
		if !decodeStrict(body, &request) || !validEnvelope(request.Envelope) || !validUUID(request.AttemptToken) || !validFailureCode(request.ErrorCode) {
			completionCode = "worker_invalid_request"
			writeError(tracked, http.StatusBadRequest, completionCode)
			return
		}
		failure := jobs.Failure{Envelope: request.Envelope, AttemptToken: request.AttemptToken, ErrorCode: request.ErrorCode}
		decision, err := h.config.Executor.Fail(r.Context(), failure)
		h.writeDurableDecision(tracked, decision, err, &completionCode)
	case rejectPath:
		var envelope jobs.Envelope
		if !decodeStrict(body, &envelope) || !validPoisonEnvelope(envelope) {
			completionCode = "worker_invalid_request"
			writeError(tracked, http.StatusBadRequest, completionCode)
			return
		}
		rejection := jobs.Rejection{Envelope: envelope}
		decision, err := h.config.Executor.Reject(r.Context(), rejection)
		h.writeDurableDecision(tracked, decision, err, &completionCode)
	}
}

func (h *handler) writeExecutorError(w http.ResponseWriter, err error, completionCode *string) {
	switch {
	case errors.Is(err, jobs.ErrInvalidDelivery), errors.Is(err, jobs.ErrInvalidTransition):
		*completionCode = "worker_invalid_request"
		writeError(w, http.StatusBadRequest, *completionCode)
	default:
		*completionCode = "worker_storage_unavailable"
		writeError(w, http.StatusServiceUnavailable, *completionCode)
	}
}

func (h *handler) writeDurableDecision(w http.ResponseWriter, decision jobs.DeliveryDecision, err error, completionCode *string) {
	if errors.Is(err, jobs.ErrConflictingCompletion) && decision == jobs.DecisionAck {
		*completionCode = "worker_completion_conflict"
		writeJSON(w, http.StatusOK, decisionResponse{Decision: jobs.DecisionAck})
		return
	}
	if err != nil {
		h.writeExecutorError(w, err, completionCode)
		return
	}
	if decision != jobs.DecisionAck && decision != jobs.DecisionWait {
		*completionCode = "worker_protocol_error"
		writeError(w, http.StatusServiceUnavailable, "worker_storage_unavailable")
		return
	}
	response := decisionResponse{Decision: decision}
	if decision == jobs.DecisionWait {
		response.RetryAfterMS = int64(h.config.WaitPollInterval / time.Millisecond)
		if h.config.WaitPollInterval%time.Millisecond != 0 {
			response.RetryAfterMS++
		}
		if response.RetryAfterMS < 1 {
			response.RetryAfterMS = 1
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func readStrictBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || len(params) > 1 || (len(params) == 1 && params["charset"] != "utf-8") {
		return nil, false
	}
	if r.ContentLength > maxBodyBytes {
		return nil, false
	}
	reader := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	data, err := io.ReadAll(reader)
	if err != nil || len(data) == 0 || jsonstrict.RejectDuplicateKeys(data) != nil {
		return nil, false
	}
	return data, true
}

func decodeStrict(data []byte, target any) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return false
	}
	var trailing any
	return decoder.Decode(&trailing) == io.EOF
}

func validEnvelope(value jobs.Envelope) bool {
	return value.SchemaVersion == 1 && validUUID(value.EventID) && validUUID(value.JobID) && value.AttemptNo > 0
}

func validPoisonEnvelope(value jobs.Envelope) bool {
	return value.SchemaVersion > 0 && validUUID(value.EventID) && validUUID(value.JobID) && value.AttemptNo > 0
}

func validClaimResult(value jobs.ClaimResult) bool {
	switch value.Decision {
	case jobs.DecisionExecute:
		if value.Execution == nil || !validUUID(value.Execution.ImageKey) || !validUUID(value.Execution.AttemptToken) ||
			value.Execution.AttemptNo <= 0 || value.Execution.Size <= 0 || !validWorkerMediaType(value.Execution.MediaType) ||
			strings.TrimSpace(value.Execution.PipelineVersion) == "" || value.Execution.HardProcessingTimeoutMS <= 0 ||
			value.Execution.CompletionBudgetMS <= 0 || !validWorkerTimestamp(value.Execution.DeadlineAt) ||
			!validWorkerTimestamp(value.Execution.LeaseExpiresAt) {
			return false
		}
		maxMillis := int64((1<<63 - 1) / int64(time.Millisecond))
		if value.Execution.HardProcessingTimeoutMS > maxMillis-value.Execution.CompletionBudgetMS {
			return false
		}
		minimumLease := time.Duration(value.Execution.CompletionBudgetMS) * time.Millisecond
		if !value.Execution.LeaseExpiresAt.After(value.Execution.DeadlineAt) ||
			value.Execution.LeaseExpiresAt.Sub(value.Execution.DeadlineAt) <= minimumLease {
			return false
		}
		return true
	case jobs.DecisionWait:
		return value.Execution == nil && value.RetryAfterMS > 0
	case jobs.DecisionAck:
		return value.Execution == nil
	default:
		return false
	}
}

func validWorkerTimestamp(value time.Time) bool {
	return !value.IsZero() && value.Year() >= 1 && value.Year() <= 9999
}

func validFailureCode(value string) bool {
	switch value {
	case "image_unprocessable", "processing_failed", "ocr_unavailable", "processing_timeout":
		return true
	default:
		return false
	}
}

func validWorkerMediaType(value string) bool {
	switch value {
	case "image/jpeg", "image/png", "image/webp":
		return true
	default:
		return false
	}
}

func isUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for index, char := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func validUUID(value string) bool {
	return isUUID(value) && value[14] == '4' && stringsContainsVariant(value[19])
}

func stringsContainsVariant(value byte) bool {
	return value == '8' || value == '9' || value == 'a' || value == 'b'
}

func workerRoute(path string) string {
	if workerRouteKnown(path) {
		return path
	}
	return "unmatched"
}

func workerRouteKnown(path string) bool {
	switch path {
	case claimPath, completePath, failPath, rejectPath:
		return true
	default:
		return false
	}
}

func workerMethod(method string) string {
	if method == http.MethodPost {
		return method
	}
	return "OTHER"
}

func workerLogLevel(status int) slog.Level {
	if status >= 500 {
		return slog.LevelError
	}
	if status >= 400 {
		return slog.LevelWarn
	}
	return slog.LevelInfo
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code string) {
	message := "Внутренний запрос не выполнен."
	if status == http.StatusUnauthorized {
		message = "Требуется действительный токен worker."
	} else if status == http.StatusBadRequest {
		message = "Некорректный внутренний запрос."
	} else if status == http.StatusNotFound {
		message = "Внутренний ресурс не найден."
	}
	writeJSON(w, status, errorResponse{Code: code, Message: message})
}

type trackedResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *trackedResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *trackedResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *trackedResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	w.bytes += int64(n)
	return n, err
}
