// Package ocr adapts the private RussianDocsOCR worker to the Go API.
package ocr

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"passport-backend/internal/httpapi"
	"passport-backend/internal/metrics"
	"passport-backend/internal/passport"
	"passport-backend/internal/telemetry"
)

const (
	workerExitError = "OCR worker failed"
)

// Config contains explicit RussianDocsOCR runtime paths and resource bounds.
type Config struct {
	RussianPython   string
	RussianWorker   string
	RussianRepo     string
	RussianModels   string
	StartupTimeout  time.Duration
	WorkRoot        string
	PipelineVersion string
	MaxInputBytes   int64
	MaxOutputBytes  int64
	MaxImagePixels  int64
	StopGrace       time.Duration
	Logger          *slog.Logger
	Metrics         *metrics.Store
}

type workerJob struct {
	ImagePath string
	ImageID   string
	RequestID string
	MediaType string
}

// Recognizer owns each private worker workspace and can stop in-flight jobs
// before the service process exits.
type Recognizer struct {
	config  Config
	russian *russianWorker

	mu      sync.Mutex
	closed  bool
	active  map[uint64]context.CancelFunc
	nextID  uint64
	workers sync.WaitGroup
}

// New validates configured paths and starts the private RussianDocsOCR worker.
func New(config Config) (*Recognizer, error) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return nil, httpapi.ErrUnavailable
	}
	if strings.TrimSpace(config.PipelineVersion) == "" || config.MaxInputBytes <= 0 ||
		config.MaxOutputBytes <= 0 || config.MaxImagePixels <= 0 || config.StopGrace <= 0 {
		return nil, errors.New("invalid OCR configuration")
	}
	if config.MaxOutputBytes < 64<<10 {
		return nil, errors.New("invalid RussianDocsOCR output limit")
	}
	return newRussianRecognizer(config)
}

func configuredPath(value string, executable, directory bool) (string, error) {
	if value == "" || !filepath.IsAbs(value) {
		return "", errors.New("path must be absolute")
	}
	path := filepath.Clean(value)
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if directory {
		if !info.IsDir() {
			return "", errors.New("path is not a directory")
		}
		linkInfo, err := os.Lstat(path)
		if err != nil || linkInfo.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("directory symlink is not allowed")
		}
	} else if !info.Mode().IsRegular() {
		return "", errors.New("path is not a regular file")
	}
	if executable && info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("path is not executable")
	}
	return path, nil
}

// Recognize runs the private RussianDocsOCR worker and returns its canonical
// structured extraction for the public mapper.
func (r *Recognizer) Recognize(ctx context.Context, image httpapi.Image) (result passport.Extraction, retErr error) {
	logger := telemetry.LoggerOrDiscard(r.config.Logger)
	requestID := telemetry.RequestID(ctx)
	if requestID == "" {
		var err error
		requestID, err = newImageID()
		if err != nil {
			return passport.Extraction{}, errors.New("OCR request setup failed")
		}
	}
	if ctx != nil {
		ctx = telemetry.WithRequestID(ctx, requestID)
	}
	started := time.Now()
	logger.Info("ocr.job.started", "event", "ocr.job.started", "request_id", requestID, "outcome", "started")
	defer func() {
		elapsed := time.Since(started)
		duration := elapsed.Milliseconds()
		if r.config.Metrics != nil {
			if retErr == nil {
				_ = r.config.Metrics.ObserveOCRJob(true, "", elapsed)
			} else {
				_ = r.config.Metrics.ObserveOCRJob(false, normalizedJobError(retErr), elapsed)
			}
		}
		if retErr != nil {
			logger.Warn("ocr.job.failed", "event", "ocr.job.failed", "request_id", requestID,
				"duration_ms", duration, "error_code", normalizedJobError(retErr), "outcome", "failure")
			return
		}
		logger.Info("ocr.job.completed", "event", "ocr.job.completed", "request_id", requestID,
			"duration_ms", duration, "outcome", "success")
	}()
	return r.recognize(ctx, image, requestID)
}

func (r *Recognizer) recognize(ctx context.Context, image httpapi.Image, requestID string) (result passport.Extraction, retErr error) {
	if ctx == nil {
		return passport.Extraction{}, errors.New("nil OCR context")
	}
	jobCtx, release, err := r.register(ctx)
	if err != nil {
		return passport.Extraction{}, err
	}
	defer release()

	if err := validateUpload(jobCtx, image, r.config); err != nil {
		return passport.Extraction{}, err
	}
	jobCtx, cancelJob := reserveStopGrace(jobCtx, r.config.StopGrace)
	defer cancelJob()
	if err := jobCtx.Err(); err != nil {
		return passport.Extraction{}, err
	}

	jobRoot, err := os.MkdirTemp(r.config.WorkRoot, "job-")
	if err != nil {
		return passport.Extraction{}, httpapi.ErrUnavailable
	}
	defer func() {
		if err := os.RemoveAll(jobRoot); err != nil {
			result = passport.Extraction{}
			retErr = errors.New("OCR workspace cleanup failed")
		}
	}()
	if err := os.Chmod(jobRoot, 0o700); err != nil {
		return passport.Extraction{}, errors.New("OCR workspace setup failed")
	}

	imageID, err := newImageID()
	if err != nil {
		return passport.Extraction{}, errors.New("OCR request setup failed")
	}
	imagePath := filepath.Join(jobRoot, "upload"+mediaExtension(image.MediaType))
	file, err := os.OpenFile(imagePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return passport.Extraction{}, errors.New("OCR workspace setup failed")
	}
	if _, err = file.Write(image.Data); err != nil {
		_ = file.Close()
		return passport.Extraction{}, errors.New("OCR workspace setup failed")
	}
	if err = file.Close(); err != nil {
		return passport.Extraction{}, errors.New("OCR workspace setup failed")
	}

	job := workerJob{ImagePath: imagePath, ImageID: imageID, RequestID: requestID, MediaType: image.MediaType}
	return r.russian.recognize(jobCtx, job, image.Data)
}

func normalizedJobError(err error) string {
	switch {
	case errors.Is(err, httpapi.ErrImageUnprocessable):
		return "image_unprocessable"
	case errors.Is(err, httpapi.ErrUnavailable):
		return "runtime_unavailable"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case strings.Contains(err.Error(), workerExitError):
		return "pipeline_failed"
	default:
		return "processing_failed"
	}
}

func validateUpload(ctx context.Context, image httpapi.Image, config Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(image.Data) == 0 || int64(len(image.Data)) > config.MaxInputBytes {
		return httpapi.ErrImageUnprocessable
	}
	mediaType, _, err := mime.ParseMediaType(image.MediaType)
	if err != nil || mediaType != image.MediaType ||
		(mediaType != "image/jpeg" && mediaType != "image/png" && mediaType != "image/webp") {
		return httpapi.ErrImageUnprocessable
	}
	return nil
}

func reserveStopGrace(parent context.Context, grace time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := parent.Deadline(); ok {
		jobDeadline := deadline.Add(-grace)
		return context.WithDeadline(parent, jobDeadline)
	}
	return context.WithCancel(parent)
}

func (r *Recognizer) register(parent context.Context) (context.Context, func(), error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, nil, httpapi.ErrUnavailable
	}
	r.nextID++
	id := r.nextID
	base, cancel := context.WithCancel(parent)
	r.active[id] = cancel
	r.workers.Add(1)
	r.mu.Unlock()
	return base, func() {
		cancel()
		r.mu.Lock()
		delete(r.active, id)
		r.mu.Unlock()
		r.workers.Done()
	}, nil
}

// Close prevents new requests, cancels active workers, and waits for their
// workspaces to be removed, subject to the supplied shutdown context.
func (r *Recognizer) Close(ctx context.Context) error {
	if ctx == nil {
		return errors.New("nil OCR close context")
	}
	r.mu.Lock()
	r.closed = true
	cancels := make([]context.CancelFunc, 0, len(r.active))
	for _, cancel := range r.active {
		cancels = append(cancels, cancel)
	}
	r.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	done := make(chan struct{})
	go func() {
		r.workers.Wait()
		close(done)
	}()
	select {
	case <-done:
		return r.russian.close()
	case <-ctx.Done():
		_ = r.russian.close()
		return ctx.Err()
	}
}

func newImageID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	var encoded [36]byte
	hex.Encode(encoded[0:8], raw[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], raw[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], raw[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], raw[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], raw[10:16])
	return string(encoded[:]), nil
}

func mediaExtension(mediaType string) string {
	switch mediaType {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	default:
		return ".bin"
	}
}

func strictJSON(data []byte, target any) error {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	root, err := decoder.Token()
	if err != nil || root != json.Delim('{') {
		return errors.New("OCR JSON root must be an object")
	}
	if err := scanJSONObject(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON token")
		}
		return err
	}
	return nil
}

func scanJSONObject(decoder *json.Decoder) error {
	seen := make(map[string]struct{})
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return errors.New("invalid JSON object key")
		}
		if _, exists := seen[key]; exists {
			return errors.New("duplicate JSON object key")
		}
		seen[key] = struct{}{}
		value, err := decoder.Token()
		if err != nil {
			return err
		}
		if err := scanJSONValue(decoder, value); err != nil {
			return err
		}
	}
	_, err := decoder.Token()
	return err
}

func scanJSONArray(decoder *json.Decoder) error {
	for decoder.More() {
		value, err := decoder.Token()
		if err != nil {
			return err
		}
		if err := scanJSONValue(decoder, value); err != nil {
			return err
		}
	}
	_, err := decoder.Token()
	return err
}

func scanJSONValue(decoder *json.Decoder, value json.Token) error {
	delim, ok := value.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		return scanJSONObject(decoder)
	case '[':
		return scanJSONArray(decoder)
	default:
		return errors.New("unexpected JSON delimiter")
	}
}
