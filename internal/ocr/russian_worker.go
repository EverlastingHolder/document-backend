//go:build darwin || linux

package ocr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"passport-backend/internal/api"
	"passport-backend/internal/httpapi"
	"passport-backend/internal/passport"
)

const russianWorkerSchema = "passport-russiandocsocr-worker/v1"

var russianWorkerFields = []string{
	"surname", "given_name", "patronymic", "gender", "birth_date", "birth_place",
	"series", "number", "issue_date", "issuer", "department_code",
}

type russianWorker struct {
	config Config
	gate   chan struct{}

	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	lines   chan []byte
	readErr chan error
	done    chan struct{}
	waitErr error
}

type russianRequest struct {
	SchemaVersion string `json:"schema_version"`
	Type          string `json:"type"`
	RequestID     string `json:"request_id"`
	ImageID       string `json:"image_id"`
	ImagePath     string `json:"image_path"`
	MediaType     string `json:"media_type"`
}

type russianResponse struct {
	SchemaVersion string             `json:"schema_version"`
	Type          string             `json:"type"`
	RequestID     string             `json:"request_id"`
	ImageID       string             `json:"image_id"`
	Status        string             `json:"status"`
	Code          string             `json:"code"`
	Fields        map[string]*string `json:"fields,omitempty"`
}

func newRussianRecognizer(config Config) (*Recognizer, error) {
	if config.StartupTimeout <= 0 {
		config.StartupTimeout = 90 * time.Second
	}
	if err := resolveRussianRuntimePaths(&config); err != nil {
		return nil, err
	}
	if config.RussianPython == "" || config.StartupTimeout <= 0 {
		return nil, errors.New("invalid RussianDocsOCR configuration")
	}
	r := &Recognizer{config: config, active: make(map[uint64]context.CancelFunc)}
	r.russian = &russianWorker{config: config, gate: make(chan struct{}, 1)}
	ctx, cancel := context.WithTimeout(context.Background(), config.StartupTimeout)
	defer cancel()
	if err := r.russian.start(ctx); err != nil {
		_ = r.russian.close()
		return nil, httpapi.ErrUnavailable
	}
	return r, nil
}

func resolveRussianRuntimePaths(config *Config) error {
	paths := []struct {
		value *string
		exec  bool
		dir   bool
	}{
		{&config.RussianPython, true, false},
		{&config.RussianWorker, false, false},
		{&config.RussianRepo, false, true},
		{&config.RussianModels, false, true},
		{&config.WorkRoot, false, true},
	}
	for _, item := range paths {
		path, err := configuredPath(*item.value, item.exec, item.dir)
		if err != nil {
			return errors.New("invalid RussianDocsOCR path")
		}
		*item.value = path
	}
	return nil
}

func (w *russianWorker) start(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cmd != nil {
		return nil
	}
	cmd, stdin, stdout, err := startWorkerProcess(w.config)
	if err != nil {
		return err
	}
	w.attachWorkerProcess(cmd, stdin, stdout)
	line, err := w.nextLine(ctx)
	if err != nil {
		_ = w.stopLocked()
		return err
	}
	if err := validateWorkerReadiness(line); err != nil {
		_ = w.stopLocked()
		return err
	}
	return nil
}

func (w *russianWorker) attachWorkerProcess(cmd *exec.Cmd, stdin io.WriteCloser, stdout io.ReadCloser) {
	w.cmd, w.stdin = cmd, stdin
	w.lines = make(chan []byte, 1)
	w.readErr = make(chan error, 1)
	w.done = make(chan struct{})
	waitDone := w.done
	readErrors := w.readErr
	maxLine := int(w.config.MaxOutputBytes - 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64<<10), maxLine)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			select {
			case w.lines <- line:
			case <-waitDone:
				return
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case readErrors <- err:
			case <-waitDone:
			}
		} else {
			select {
			case readErrors <- io.EOF:
			case <-waitDone:
			}
		}
	}()
	go func() {
		w.waitErr = cmd.Wait()
		close(waitDone)
	}()
}

func startWorkerProcess(config Config) (*exec.Cmd, io.WriteCloser, io.ReadCloser, error) {
	cmd := exec.Command(config.RussianPython, config.RussianWorker,
		"--repo", config.RussianRepo,
		"--models-root", config.RussianModels,
		"--work-root", config.WorkRoot,
		"--max-input-bytes", fmt.Sprint(config.MaxInputBytes),
		"--max-image-pixels", fmt.Sprint(config.MaxImagePixels),
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, nil, nil, err
	}
	return cmd, stdin, stdout, nil
}

func validateWorkerReadiness(line []byte) error {
	var ready struct {
		SchemaVersion string `json:"schema_version"`
		Type          string `json:"type"`
	}
	if strictJSON(line, &ready) != nil ||
		ready.SchemaVersion != russianWorkerSchema || ready.Type != "ready" {
		return errors.New("RussianDocsOCR worker readiness protocol failed")
	}
	return nil
}

func (w *russianWorker) recognize(ctx context.Context, job workerJob, input []byte) (passport.Extraction, error) {
	if err := validateWorkerInput(input, job.ImagePath, w.config); err != nil {
		return passport.Extraction{}, httpapi.ErrImageUnprocessable
	}
	release, err := w.acquireRequestSlot(ctx)
	if err != nil {
		return passport.Extraction{}, err
	}
	defer release()
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ensureStarted(ctx); err != nil {
		return passport.Extraction{}, err
	}
	line, err := w.exchangeRequest(ctx, job)
	if err != nil {
		return passport.Extraction{}, err
	}
	response, err := decodeCorrelatedRussianResponse(line, job)
	if err != nil {
		_ = w.stopLocked()
		return passport.Extraction{}, errors.New(workerExitError)
	}
	if ctx.Err() != nil {
		_ = w.stopLocked()
		return passport.Extraction{}, ctx.Err()
	}
	if err := russianResponseStatusError(response); err != nil {
		return passport.Extraction{}, err
	}
	extraction, err := extractionFromRussianResponse(response, job, w.config.PipelineVersion)
	if err != nil {
		_ = w.stopLocked()
		return passport.Extraction{}, errors.New(workerExitError)
	}
	return extraction, nil
}

func validateWorkerInput(input []byte, imagePath string, config Config) error {
	if int64(len(input)) > config.MaxInputBytes || !russianInputPathIsPrivate(imagePath, config.WorkRoot) {
		return httpapi.ErrImageUnprocessable
	}
	return nil
}

func (w *russianWorker) acquireRequestSlot(ctx context.Context) (func(), error) {
	select {
	case w.gate <- struct{}{}:
		return func() { <-w.gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (w *russianWorker) ensureStarted(ctx context.Context) error {
	if w.cmd == nil {
		if err := w.startLocked(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return httpapi.ErrUnavailable
		}
	}
	if err := ctx.Err(); err != nil {
		_ = w.stopLocked()
		return err
	}
	return nil
}

func (w *russianWorker) exchangeRequest(ctx context.Context, job workerJob) ([]byte, error) {
	request := russianRequest{
		SchemaVersion: russianWorkerSchema,
		Type:          "recognize", RequestID: job.RequestID, ImageID: job.ImageID,
		ImagePath: job.ImagePath, MediaType: job.MediaType,
	}
	encoded, err := json.Marshal(request)
	if err != nil || int64(len(encoded)+1) > w.config.MaxOutputBytes {
		return nil, errors.New(workerExitError)
	}
	if _, err := w.stdin.Write(append(encoded, '\n')); err != nil {
		_ = w.stopLocked()
		return nil, httpapi.ErrUnavailable
	}
	line, err := w.nextLine(ctx)
	if err != nil {
		_ = w.stopLocked()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New(workerExitError)
	}
	return line, nil
}

func decodeCorrelatedRussianResponse(line []byte, job workerJob) (russianResponse, error) {
	response, err := decodeRussianResponse(line)
	if err != nil || response.SchemaVersion != russianWorkerSchema ||
		response.Type != "result" || response.RequestID != job.RequestID || response.ImageID != job.ImageID {
		return russianResponse{}, errors.New(workerExitError)
	}
	return response, nil
}

func russianResponseStatusError(response russianResponse) error {
	if response.Status == "completed" {
		return nil
	}
	switch response.Code {
	case "invalid_image":
		return httpapi.ErrImageUnprocessable
	case "runtime_unavailable":
		return httpapi.ErrUnavailable
	default:
		return errors.New(workerExitError)
	}
}

func extractionFromRussianResponse(response russianResponse, job workerJob, pipelineVersion string) (passport.Extraction, error) {
	if !validRussianFields(response.Fields) {
		return passport.Extraction{}, errors.New(workerExitError)
	}
	values := make(map[string]*string, len(response.Fields))
	for name, value := range response.Fields {
		values[name] = value
	}
	extraction := passport.StructuredExtraction(job.ImageID, values)
	if _, err := api.MapExtraction(extraction, job.ImageID, pipelineVersion); err != nil {
		return passport.Extraction{}, errors.New(workerExitError)
	}
	return extraction, nil
}

func (w *russianWorker) startLocked(ctx context.Context) error {
	w.mu.Unlock()
	err := w.start(ctx)
	w.mu.Lock()
	return err
}

func (w *russianWorker) nextLine(ctx context.Context) ([]byte, error) {
	select {
	case line := <-w.lines:
		return line, nil
	case err := <-w.readErr:
		return nil, err
	case <-w.done:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (w *russianWorker) stopLocked() error {
	if w.cmd == nil {
		return nil
	}
	cmd, done := w.cmd, w.done
	_ = w.stdin.Close()
	_ = signalManagedGroup(cmd.Process.Pid, syscall.SIGTERM)
	timer := time.NewTimer(w.config.StopGrace)
	defer timer.Stop()
	select {
	case <-done:
		_ = signalManagedGroup(cmd.Process.Pid, syscall.SIGKILL)
	case <-timer.C:
		_ = signalManagedGroup(cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
	w.cmd, w.stdin, w.lines, w.readErr, w.done = nil, nil, nil, nil, nil
	return nil
}

func (w *russianWorker) close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stopLocked()
}

func validRussianFields(fields map[string]*string) bool {
	if fields == nil || len(fields) != len(russianWorkerFields) {
		return false
	}
	for _, name := range russianWorkerFields {
		if _, ok := fields[name]; !ok {
			return false
		}
		if value := fields[name]; value != nil && (len(*value) > 4096 || strings.ContainsRune(*value, '\x00')) {
			return false
		}
	}
	return true
}

func decodeRussianResponse(data []byte) (russianResponse, error) {
	var response russianResponse
	if err := strictJSON(data, &response); err != nil {
		return russianResponse{}, err
	}
	if response.Status == "completed" && !validRussianFields(response.Fields) {
		return russianResponse{}, errors.New("invalid worker field set")
	}
	return response, nil
}

func russianInputPathIsPrivate(path, root string) bool {
	if !pathWithinWorkspace(path, root) {
		return false
	}
	return isPrivateRegularFile(path)
}

func pathWithinWorkspace(path, root string) bool {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return false
	}
	return true
}

func isPrivateRegularFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o077 == 0
}
