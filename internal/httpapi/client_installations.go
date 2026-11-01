package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"passport-backend/internal/clientauth"
	"passport-backend/internal/clientenrollment"
	"passport-backend/internal/jsonstrict"
)

const maxClientInstallationBodyBytes = 4096

type clientInstallationRequest struct {
	TokenSHA256 string `json:"token_sha256"`
}

type clientInstallationResponse struct {
	ClientID  string    `json:"client_id"`
	CreatedAt time.Time `json:"created_at"`
}

func (h *handler) serveClientInstallation(w http.ResponseWriter, r *http.Request, requestID string, completionErrorCode *string) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Context().Err() != nil {
		return
	}
	if !remoteAddrAllowed(r.RemoteAddr, h.config.AllowedClientNetworks) {
		*completionErrorCode = "access_denied"
		writeError(w, requestID, http.StatusForbidden, "access_denied", "Доступ запрещён.")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		*completionErrorCode = "method_not_allowed"
		writeError(w, requestID, http.StatusMethodNotAllowed, "method_not_allowed", "Метод запроса не поддерживается.")
		return
	}
	if h.config.AsyncJobs == nil || h.config.AsyncJobs.ClientEnrollment == nil {
		*completionErrorCode = "client_registration_unavailable"
		writeError(w, requestID, http.StatusServiceUnavailable, "client_registration_unavailable", "Регистрация клиента временно недоступна.")
		return
	}
	mediaType, _, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != "application/json" {
		writeClientInstallationError(w, requestID, http.StatusBadRequest, "invalid_request", "Некорректный запрос.")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxClientInstallationBodyBytes+1))
	if err != nil || len(body) == 0 || len(body) > maxClientInstallationBodyBytes || jsonstrict.RejectDuplicateKeys(body) != nil {
		writeClientInstallationError(w, requestID, http.StatusBadRequest, "invalid_request", "Некорректный запрос.")
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var request clientInstallationRequest
	if err := decoder.Decode(&request); err != nil || decoder.Decode(new(any)) != io.EOF {
		writeClientInstallationError(w, requestID, http.StatusBadRequest, "invalid_request", "Некорректный запрос.")
		return
	}
	hash, err := clientauth.ParseTokenHash(request.TokenSHA256)
	if err != nil {
		writeClientInstallationError(w, requestID, http.StatusBadRequest, "invalid_request", "Некорректный запрос.")
		return
	}
	if r.Context().Err() != nil {
		return
	}
	storeCtx, cancel := context.WithTimeout(r.Context(), h.config.AsyncJobs.StatusReadTimeout)
	installation, replay, err := h.config.AsyncJobs.ClientEnrollment.Register(storeCtx, hash)
	cancel()
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		switch {
		case errors.Is(err, clientenrollment.ErrInvalidHash), errors.Is(err, clientenrollment.ErrInvalidConfig):
			writeClientInstallationError(w, requestID, http.StatusBadRequest, "invalid_request", "Некорректный запрос.")
		case errors.Is(err, clientenrollment.ErrStaticCredential), errors.Is(err, clientenrollment.ErrRevokedCredential):
			*completionErrorCode = "access_denied"
			writeClientInstallationError(w, requestID, http.StatusForbidden, "access_denied", "Доступ запрещён.")
		case errors.Is(err, clientenrollment.ErrRateLimited):
			*completionErrorCode = "registration_rate_limited"
			writeClientInstallationError(w, requestID, http.StatusTooManyRequests, "registration_rate_limited", "Регистрация временно ограничена.")
		default:
			*completionErrorCode = "client_registration_unavailable"
			writeClientInstallationError(w, requestID, http.StatusServiceUnavailable, "client_registration_unavailable", "Регистрация клиента временно недоступна.")
		}
		return
	}
	status := http.StatusCreated
	if replay {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Request-ID", requestID)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(clientInstallationResponse{ClientID: installation.ClientID, CreatedAt: installation.CreatedAt.UTC()})
}

func writeClientInstallationError(w http.ResponseWriter, requestID string, status int, code, message string) {
	w.Header().Set("Cache-Control", "no-store")
	writeError(w, requestID, status, code, message)
}
