package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"passport-backend/internal/api"
	"passport-backend/internal/passport"
)

const (
	recognizePath         = "/v1/passport/recognize"
	multipartBodyOverhead = int64(64 << 10)
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
}

type errorResponse struct {
	RequestID string `json:"request_id"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

type handler struct {
	config     Config
	recognizer Recognizer
	semaphore  chan struct{}
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
	config.AllowedClientNetworks = allowedNetworks
	return &handler{
		config:     config,
		recognizer: recognizer,
		semaphore:  make(chan struct{}, config.MaxConcurrent),
	}, nil
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID, validID, err := requestIDFor(r)
	if err != nil {
		return
	}
	if !validID {
		writeError(w, requestID, http.StatusBadRequest, "invalid_request", "Некорректный запрос.")
		return
	}
	if r.URL.Path != recognizePath {
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
