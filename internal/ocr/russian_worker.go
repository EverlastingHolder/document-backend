//go:build darwin || linux

package ocr

import (
	"bufio"
	"bytes"
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
	paths := []struct {
		value string
		exec  bool
		dir   bool
	}{
		{config.RussianPython, true, false},
		{config.RussianWorker, false, false},
		{config.RussianRepo, false, true},
		{config.RussianModels, false, true},
		{config.WorkRoot, false, true},
	}
	resolved := make([]string, 0, len(paths))
	for _, item := range paths {
		path, err := configuredPath(item.value, item.exec, item.dir)
		if err != nil {
			return nil, errors.New("invalid RussianDocsOCR path")
		}
		resolved = append(resolved, path)
	}
	config.RussianPython, config.RussianWorker, config.RussianRepo, config.RussianModels, config.WorkRoot =
		resolved[0], resolved[1], resolved[2], resolved[3], resolved[4]
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

func (w *russianWorker) start(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cmd != nil {
		return nil
	}
	cmd := exec.Command(w.config.RussianPython, w.config.RussianWorker,
		"--repo", w.config.RussianRepo,
		"--models-root", w.config.RussianModels,
		"--work-root", w.config.WorkRoot,
		"--max-input-bytes", fmt.Sprint(w.config.MaxInputBytes),
		"--max-image-pixels", fmt.Sprint(w.config.MaxImagePixels),
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return err
	}
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
	line, err := w.nextLine(ctx)
	if err != nil {
		_ = w.stopLocked()
		return err
	}
	var ready struct {
		SchemaVersion string `json:"schema_version"`
		Type          string `json:"type"`
	}
	if strictJSON(line, &ready) != nil || decodeUnknownFields(line, &ready) != nil ||
		ready.SchemaVersion != russianWorkerSchema || ready.Type != "ready" {
		_ = w.stopLocked()
		return errors.New("RussianDocsOCR worker readiness protocol failed")
	}
	return nil
}

func (w *russianWorker) recognize(ctx context.Context, job workerJob, input []byte) (passport.Extraction, error) {
	if int64(len(input)) > w.config.MaxInputBytes || !russianInputPathIsPrivate(job.ImagePath, w.config.WorkRoot) {
		return passport.Extraction{}, httpapi.ErrImageUnprocessable
	}
	select {
	case w.gate <- struct{}{}:
		defer func() { <-w.gate }()
	case <-ctx.Done():
		return passport.Extraction{}, ctx.Err()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cmd == nil {
		if err := w.startLocked(ctx); err != nil {
			if ctx.Err() != nil {
				return passport.Extraction{}, ctx.Err()
			}
			return passport.Extraction{}, httpapi.ErrUnavailable
		}
	}
	if err := ctx.Err(); err != nil {
		_ = w.stopLocked()
		return passport.Extraction{}, err
	}
	request := russianRequest{
		SchemaVersion: russianWorkerSchema,
		Type:          "recognize", RequestID: job.RequestID, ImageID: job.ImageID,
		ImagePath: job.ImagePath, MediaType: job.MediaType,
	}
	encoded, err := json.Marshal(request)
	if err != nil || int64(len(encoded)+1) > w.config.MaxOutputBytes {
		return passport.Extraction{}, errors.New(workerExitError)
	}
	if _, err := w.stdin.Write(append(encoded, '\n')); err != nil {
		_ = w.stopLocked()
		return passport.Extraction{}, httpapi.ErrUnavailable
	}
	line, err := w.nextLine(ctx)
	if err != nil {
		_ = w.stopLocked()
		if ctx.Err() != nil {
			return passport.Extraction{}, ctx.Err()
		}
		return passport.Extraction{}, errors.New(workerExitError)
	}
	response, decodeErr := decodeRussianResponse(line)
	if decodeErr != nil || response.SchemaVersion != russianWorkerSchema ||
		response.Type != "result" || response.RequestID != job.RequestID || response.ImageID != job.ImageID {
		_ = w.stopLocked()
		return passport.Extraction{}, errors.New(workerExitError)
	}
	if ctx.Err() != nil {
		_ = w.stopLocked()
		return passport.Extraction{}, ctx.Err()
	}
	if response.Status != "completed" {
		switch response.Code {
		case "invalid_image":
			return passport.Extraction{}, httpapi.ErrImageUnprocessable
		case "runtime_unavailable":
			return passport.Extraction{}, httpapi.ErrUnavailable
		default:
			return passport.Extraction{}, errors.New(workerExitError)
		}
	}
	if !validRussianFields(response.Fields) {
		_ = w.stopLocked()
		return passport.Extraction{}, errors.New(workerExitError)
	}
	values := make(map[string]*string, len(response.Fields))
	for name, value := range response.Fields {
		values[name] = value
	}
	extraction := passport.StructuredExtraction(job.ImageID, values)
	if _, err := api.MapExtraction(extraction, job.ImageID, w.config.PipelineVersion); err != nil {
		_ = w.stopLocked()
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
	for name := range fields {
		found := false
		for _, expected := range russianWorkerFields {
			found = found || name == expected
		}
		if !found {
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
	if err := decodeUnknownFields(data, &response); err != nil {
		return russianResponse{}, err
	}
	if response.Status == "completed" && !validRussianFields(response.Fields) {
		return russianResponse{}, errors.New("invalid worker field set")
	}
	return response, nil
}

func decodeUnknownFields(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return nil
}

func russianInputPathIsPrivate(path, root string) bool {
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
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o077 == 0
}
