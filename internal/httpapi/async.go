package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"passport-backend/internal/clientauth"
	"passport-backend/internal/clientenrollment"
	"passport-backend/internal/imagestore"
	"passport-backend/internal/jobs"
	"passport-backend/internal/jobstatus"
)

const jobsPath = "/v1/passport/jobs"

var idempotencyKeyPattern = regexp.MustCompile(`^[!-~]{1,128}$`)

type AsyncAuthenticator interface {
	AuthenticateContext(context.Context, []string) (clientauth.Principal, error)
}

type AsyncAcceptor interface {
	Accept(context.Context, string, string, []byte, string) (jobs.Accepted, error)
}

type AsyncStatusReader interface {
	ReadOwned(context.Context, string, string) (jobstatus.Snapshot, error)
}

type AsyncReceiptAcknowledger interface {
	AcknowledgeOwnedResult(context.Context, string, string) (jobs.ReceiptOutcome, error)
}

type ClientInstallationRegistrar interface {
	Register(context.Context, [32]byte) (clientenrollment.Installation, bool, error)
}

type AsyncJobsConfig struct {
	Authenticator        AsyncAuthenticator
	Acceptor             AsyncAcceptor
	StatusReader         AsyncStatusReader
	ReceiptAcknowledger  AsyncReceiptAcknowledger
	ClientEnrollment     ClientInstallationRegistrar
	StatusReadTimeout    time.Duration
	MaxConcurrentUploads int
}

type jobAcceptedResponse struct {
	JobID     string      `json:"job_id"`
	Status    jobs.Status `json:"status"`
	StatusURL string      `json:"status_url"`
	CreatedAt time.Time   `json:"created_at"`
}

type jobStatusResponse struct {
	JobID      string                 `json:"job_id"`
	Status     jobs.Status            `json:"status"`
	CreatedAt  time.Time              `json:"created_at"`
	UpdatedAt  time.Time              `json:"updated_at"`
	StartedAt  *time.Time             `json:"started_at"`
	FinishedAt *time.Time             `json:"finished_at"`
	Result     json.RawMessage        `json:"result,omitempty"`
	Error      *jobstatus.PublicError `json:"error,omitempty"`
}

func (h *handler) serveJobStatus(w http.ResponseWriter, r *http.Request, jobID, requestID string, completionErrorCode *string) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
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
	async := h.config.AsyncJobs
	if async == nil || async.StatusReader == nil {
		*completionErrorCode = "job_status_unavailable"
		writeError(w, requestID, http.StatusServiceUnavailable, "job_status_unavailable", "Статус задания временно недоступен.")
		return
	}
	readCtx, cancelRead := context.WithTimeout(r.Context(), async.StatusReadTimeout)
	defer cancelRead()
	principal, err := async.Authenticator.AuthenticateContext(readCtx, r.Header.Values("Authorization"))
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		if errors.Is(err, clientauth.ErrAuthenticationUnavailable) || errors.Is(err, context.DeadlineExceeded) {
			writeError(w, requestID, http.StatusServiceUnavailable, "job_status_unavailable", "Статус задания временно недоступен.")
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="passport-jobs"`)
		writeError(w, requestID, http.StatusUnauthorized, "unauthorized", "Требуется действительный токен клиента.")
		return
	}
	snapshot, err := async.StatusReader.ReadOwned(readCtx, principal.OwnerID, jobID)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		if errors.Is(err, jobs.ErrNotFound) {
			writeError(w, requestID, http.StatusNotFound, "not_found", "Ресурс не найден.")
			return
		}
		if errors.Is(err, jobs.ErrResultGone) {
			writeError(w, requestID, http.StatusGone, "job_result_gone", "Результат задания больше не хранится.")
			return
		}
		*completionErrorCode = "job_status_unavailable"
		writeError(w, requestID, http.StatusServiceUnavailable, "job_status_unavailable", "Статус задания временно недоступен.")
		return
	}
	response := jobStatusResponse{
		JobID: snapshot.JobID, Status: snapshot.Status,
		CreatedAt: snapshot.CreatedAt, UpdatedAt: snapshot.UpdatedAt,
		StartedAt: snapshot.StartedAt, FinishedAt: snapshot.FinishedAt,
		Result: snapshot.Result, Error: snapshot.Error,
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Request-ID", requestID)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(response)
}

func (h *handler) serveJobReceipt(w http.ResponseWriter, r *http.Request, jobID, requestID string, completionErrorCode *string) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, requestID, http.StatusMethodNotAllowed, "method_not_allowed", "Метод запроса не поддерживается.")
		return
	}
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		writeError(w, requestID, http.StatusBadRequest, "invalid_request", "Некорректный запрос.")
		return
	}
	if r.Context().Err() != nil {
		return
	}
	if !remoteAddrAllowed(r.RemoteAddr, h.config.AllowedClientNetworks) {
		writeError(w, requestID, http.StatusForbidden, "access_denied", "Доступ запрещён.")
		return
	}
	async := h.config.AsyncJobs
	if async == nil || async.ReceiptAcknowledger == nil {
		*completionErrorCode = "job_status_unavailable"
		writeError(w, requestID, http.StatusServiceUnavailable, "job_status_unavailable", "Статус задания временно недоступен.")
		return
	}
	operationCtx, cancel := context.WithTimeout(r.Context(), async.StatusReadTimeout)
	defer cancel()
	principal, err := async.Authenticator.AuthenticateContext(operationCtx, r.Header.Values("Authorization"))
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		if errors.Is(err, clientauth.ErrAuthenticationUnavailable) || errors.Is(err, context.DeadlineExceeded) {
			*completionErrorCode = "job_status_unavailable"
			writeError(w, requestID, http.StatusServiceUnavailable, "job_status_unavailable", "Статус задания временно недоступен.")
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="passport-jobs"`)
		writeError(w, requestID, http.StatusUnauthorized, "unauthorized", "Требуется действительный токен клиента.")
		return
	}
	outcome, err := async.ReceiptAcknowledger.AcknowledgeOwnedResult(operationCtx, principal.OwnerID, jobID)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		if errors.Is(err, jobs.ErrNotFound) {
			writeError(w, requestID, http.StatusNotFound, "not_found", "Ресурс не найден.")
			return
		}
		*completionErrorCode = "job_status_unavailable"
		writeError(w, requestID, http.StatusServiceUnavailable, "job_status_unavailable", "Статус задания временно недоступен.")
		return
	}
	if outcome == jobs.ReceiptActive {
		writeError(w, requestID, http.StatusConflict, "job_not_terminal", "Задание ещё выполняется.")
		return
	}
	if outcome != jobs.ReceiptReceived && outcome != jobs.ReceiptAlreadyPurged {
		*completionErrorCode = "job_status_unavailable"
		writeError(w, requestID, http.StatusServiceUnavailable, "job_status_unavailable", "Статус задания временно недоступен.")
		return
	}
	w.Header().Set("X-Request-ID", requestID)
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) serveJobSubmission(w http.ResponseWriter, r *http.Request, requestID string, completionErrorCode *string) {
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
	async := h.config.AsyncJobs
	if async == nil {
		*completionErrorCode = "job_acceptance_unavailable"
		writeError(w, requestID, http.StatusServiceUnavailable, "job_acceptance_unavailable", "Приём заданий временно не настроен.")
		return
	}
	operationCtx, cancelOperation := context.WithTimeout(r.Context(), async.StatusReadTimeout)
	defer cancelOperation()
	principal, err := async.Authenticator.AuthenticateContext(operationCtx, r.Header.Values("Authorization"))
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		if errors.Is(err, clientauth.ErrAuthenticationUnavailable) || errors.Is(err, context.DeadlineExceeded) {
			*completionErrorCode = "job_acceptance_unavailable"
			writeError(w, requestID, http.StatusServiceUnavailable, "job_acceptance_unavailable", "Приём заданий временно не настроен.")
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="passport-jobs"`)
		writeError(w, requestID, http.StatusUnauthorized, "unauthorized", "Требуется действительный токен клиента.")
		return
	}
	idempotencyValues := r.Header.Values("Idempotency-Key")
	if len(idempotencyValues) != 1 || !idempotencyKeyPattern.MatchString(idempotencyValues[0]) {
		writeError(w, requestID, http.StatusBadRequest, "invalid_request", "Некорректный запрос.")
		return
	}
	if err := h.asyncSemaphore.Acquire(operationCtx, 1); err != nil {
		if r.Context().Err() == nil {
			*completionErrorCode = "job_acceptance_unavailable"
			writeError(w, requestID, http.StatusServiceUnavailable, "job_acceptance_unavailable", "Приём заданий временно не настроен.")
		}
		return
	}
	defer h.asyncSemaphore.Release(1)

	operationRequest := r.WithContext(operationCtx)
	image, status, code, message := h.readImage(w, operationRequest)
	if status != 0 {
		if r.Context().Err() != nil {
			return
		}
		if operationCtx.Err() != nil {
			*completionErrorCode = "job_acceptance_unavailable"
			writeError(w, requestID, http.StatusServiceUnavailable, "job_acceptance_unavailable", "Приём заданий временно не настроен.")
			return
		}
		writeError(w, requestID, status, code, message)
		return
	}
	if r.Context().Err() != nil {
		return
	}
	if operationCtx.Err() != nil {
		*completionErrorCode = "job_acceptance_unavailable"
		writeError(w, requestID, http.StatusServiceUnavailable, "job_acceptance_unavailable", "Задание не удалось сохранить. Повторите попытку позже.")
		return
	}
	accepted, err := async.Acceptor.Accept(operationCtx, principal.OwnerID, idempotencyValues[0], image.Data, image.MediaType)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		if operationCtx.Err() != nil {
			*completionErrorCode = "job_acceptance_unavailable"
			writeError(w, requestID, http.StatusServiceUnavailable, "job_acceptance_unavailable", "Задание не удалось сохранить. Повторите попытку позже.")
			return
		}
		if errors.Is(err, jobs.ErrConflict) {
			writeError(w, requestID, http.StatusConflict, "idempotency_conflict", "Ключ повтора уже использован с другим содержимым.")
			return
		}
		if errors.Is(err, imagestore.ErrImageTooLarge) {
			writeError(w, requestID, http.StatusRequestEntityTooLarge, "upload_too_large", "Размер файла превышает допустимый предел.")
			return
		}
		if errors.Is(err, imagestore.ErrInvalidImage) {
			writeError(w, requestID, http.StatusUnsupportedMediaType, "unsupported_media_type", "Формат изображения не поддерживается.")
			return
		}
		*completionErrorCode = "job_acceptance_unavailable"
		writeError(w, requestID, http.StatusServiceUnavailable, "job_acceptance_unavailable", "Задание не удалось сохранить. Повторите попытку позже.")
		return
	}
	statusURL := jobsPath + "/" + url.PathEscape(accepted.JobID)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Location", statusURL)
	w.Header().Set("X-Request-ID", requestID)
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(jobAcceptedResponse{
		JobID: accepted.JobID, Status: accepted.Status, StatusURL: statusURL, CreatedAt: accepted.CreatedAt,
	})
}

func validateAsyncConfig(config *AsyncJobsConfig) error {
	if config == nil {
		return nil
	}
	if config.Authenticator == nil || config.Acceptor == nil || config.MaxConcurrentUploads <= 0 || config.StatusReadTimeout <= 0 {
		return fmt.Errorf("invalid asynchronous jobs configuration")
	}
	return nil
}
