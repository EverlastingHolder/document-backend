// Package metrics collects bounded aggregate service statistics separately
// from the privacy-filtered operational log.
package metrics

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	schemaVersion = "passport-metrics/v1"
	maxStateBytes = 128 << 10
)

var (
	ErrInvalidObservation = errors.New("invalid metrics observation")
	ErrCounterOverflow    = errors.New("metrics counter overflow")
	ErrClosed             = errors.New("metrics store closed")
	durationBoundaries    = [...]uint64{10, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000, 120000, 300000, 600000, 86400000}
	legacyStatusCodes     = [...]string{"0", "200", "400", "403", "404", "405", "413", "415", "422", "429", "500", "503", "504"}
	previousStatusCodes   = [...]string{"0", "200", "202", "400", "401", "403", "404", "405", "409", "413", "415", "422", "429", "500", "503", "504"}
	statusCodes           = [...]string{"0", "200", "201", "202", "400", "401", "403", "404", "405", "409", "413", "415", "422", "429", "500", "503", "504"}
	errorCodes            = [...]string{"image_unprocessable", "runtime_unavailable", "deadline_exceeded", "cancelled", "pipeline_failed", "processing_failed"}
	stageNames            = [...]string{"ocr", "baseline", "vl", "extract"}
)

// Config selects a private snapshot file and identifies the configured OCR pipeline.
type Config struct {
	Path            string
	PipelineVersion string
	Logger          *slog.Logger
}

// Snapshot is the stable bounded JSON representation returned to API readers.
type Snapshot struct {
	SchemaVersion   string                `json:"schema_version"`
	PipelineVersion string                `json:"pipeline_version"`
	Since           time.Time             `json:"since"`
	UpdatedAt       time.Time             `json:"updated_at"`
	Persistence     Persistence           `json:"persistence"`
	HTTP            HTTPStats             `json:"http"`
	OCR             OCRStats              `json:"ocr"`
	Stages          map[string]StageStats `json:"stages"`
}

type Persistence struct {
	Healthy   bool   `json:"healthy"`
	ErrorCode string `json:"error_code"`
}

type HTTPStats struct {
	Total              uint64            `json:"total"`
	Errors             uint64            `json:"errors"`
	ByStatus           map[string]uint64 `json:"by_status"`
	Duration           Histogram         `json:"duration"`
	ResponseBytesTotal uint64            `json:"response_bytes_total"`
}

type OCRStats struct {
	Total       uint64            `json:"total"`
	Completed   uint64            `json:"completed"`
	Failed      uint64            `json:"failed"`
	ByErrorCode map[string]uint64 `json:"by_error_code"`
	Duration    Histogram         `json:"duration"`
}

type StageStats struct {
	Completed uint64    `json:"completed"`
	Failed    uint64    `json:"failed"`
	Duration  Histogram `json:"duration"`
}

type Bucket struct {
	LEMS  *uint64 `json:"le_ms"`
	Count uint64  `json:"count"`
}

type Histogram struct {
	Count   uint64   `json:"count"`
	SumMS   uint64   `json:"sum_ms"`
	MinMS   uint64   `json:"min_ms"`
	MaxMS   uint64   `json:"max_ms"`
	Buckets []Bucket `json:"buckets"`
}

// Store serializes updates and atomically persists the complete bounded snapshot.
type Store struct {
	mu              sync.Mutex
	path            string
	lockFile        *os.File
	logger          *slog.Logger
	snapshot        Snapshot
	closed          bool
	reportedFailure bool
}

// Open acquires exclusive ownership of Path and loads or initializes its snapshot.
func Open(config Config) (*Store, error) {
	if strings.TrimSpace(config.Path) == "" || strings.TrimSpace(config.PipelineVersion) == "" || len(config.PipelineVersion) > 128 {
		return nil, errors.New("invalid metrics configuration")
	}
	path, err := filepath.Abs(config.Path)
	if err != nil {
		return nil, errors.New("invalid metrics path")
	}
	parent := filepath.Dir(path)
	if err := ensurePrivateParent(parent); err != nil {
		return nil, errors.New("unsafe metrics directory")
	}
	if err := validateExisting(path); err != nil {
		return nil, errors.New("unsafe metrics state file")
	}
	if err := validateExisting(path + ".lock"); err != nil {
		return nil, errors.New("unsafe metrics lock file")
	}
	lockFile, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, errors.New("metrics lock unavailable")
	}
	if err := lockFile.Chmod(0o600); err != nil {
		_ = lockFile.Close()
		return nil, errors.New("metrics lock unavailable")
	}
	if err := lockState(lockFile); err != nil {
		_ = lockFile.Close()
		return nil, errors.New("metrics state already owned or locking unavailable")
	}
	store := &Store{path: path, lockFile: lockFile, logger: config.Logger}
	if info, statErr := os.Lstat(path); statErr == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || info.Size() > maxStateBytes {
			_ = store.releaseLock()
			return nil, errors.New("unsafe metrics state file")
		}
		contents, readErr := os.ReadFile(path)
		if readErr != nil {
			_ = store.releaseLock()
			return nil, errors.New("metrics state unavailable")
		}
		upgraded, decodeErr := decodeSnapshot(contents, config.PipelineVersion, &store.snapshot)
		if decodeErr != nil {
			_ = store.releaseLock()
			return nil, errors.New("metrics state invalid")
		}
		if upgraded {
			if persistErr := store.persistLocked(store.snapshot); persistErr != nil {
				_ = store.releaseLock()
				return nil, errors.New("metrics state upgrade failed")
			}
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		_ = store.releaseLock()
		return nil, errors.New("metrics state unavailable")
	} else {
		now := time.Now().UTC()
		store.snapshot = emptySnapshot(config.PipelineVersion, now)
		if err := store.persistLocked(store.snapshot); err != nil {
			_ = store.releaseLock()
			return nil, errors.New("metrics state initialization failed")
		}
	}
	return store, nil
}

// ObserveHTTP records one completed HTTP response using fixed status categories.
func (s *Store) ObserveHTTP(status int, duration time.Duration, responseBytes int64) error {
	statusKey := fmt.Sprint(status)
	if !contains(statusCodes[:], statusKey) || duration < 0 || responseBytes < 0 {
		return ErrInvalidObservation
	}
	return s.update(func(next *Snapshot) error {
		if err := add(&next.HTTP.Total, 1); err != nil {
			return err
		}
		if status >= 400 {
			if err := add(&next.HTTP.Errors, 1); err != nil {
				return err
			}
		}
		count := next.HTTP.ByStatus[statusKey]
		if err := add(&count, 1); err != nil {
			return err
		}
		next.HTTP.ByStatus[statusKey] = count
		if err := add(&next.HTTP.ResponseBytesTotal, uint64(responseBytes)); err != nil {
			return err
		}
		return addDuration(&next.HTTP.Duration, duration)
	})
}

// ObserveOCRJob records one terminal OCR job outcome and its closed error category.
func (s *Store) ObserveOCRJob(success bool, errorCode string, duration time.Duration) error {
	if duration < 0 || (success && errorCode != "") || (!success && !contains(errorCodes[:], errorCode)) {
		return ErrInvalidObservation
	}
	return s.update(func(next *Snapshot) error {
		if err := add(&next.OCR.Total, 1); err != nil {
			return err
		}
		if success {
			if err := add(&next.OCR.Completed, 1); err != nil {
				return err
			}
		} else {
			if err := add(&next.OCR.Failed, 1); err != nil {
				return err
			}
			count := next.OCR.ByErrorCode[errorCode]
			if err := add(&count, 1); err != nil {
				return err
			}
			next.OCR.ByErrorCode[errorCode] = count
		}
		return addDuration(&next.OCR.Duration, duration)
	})
}

// ObserveStage records a terminal event for one allowlisted worker stage.
func (s *Store) ObserveStage(stage string, success bool, duration time.Duration) error {
	if duration < 0 || !contains(stageNames[:], stage) {
		return ErrInvalidObservation
	}
	return s.update(func(next *Snapshot) error {
		stats := next.Stages[stage]
		if success {
			if err := add(&stats.Completed, 1); err != nil {
				return err
			}
		} else if err := add(&stats.Failed, 1); err != nil {
			return err
		}
		if err := addDuration(&stats.Duration, duration); err != nil {
			return err
		}
		next.Stages[stage] = stats
		return nil
	})
}

// Snapshot returns a detached copy suitable for serialization.
func (s *Store) Snapshot() Snapshot {
	if s == nil {
		return Snapshot{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneSnapshot(s.snapshot)
}

// Close flushes the current snapshot and releases exclusive state ownership.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	var persistErr error
	next := cloneSnapshot(s.snapshot)
	next.Persistence = Persistence{Healthy: true}
	if err := s.persistLocked(next); err != nil {
		persistErr = err
		s.snapshot.Persistence = Persistence{Healthy: false, ErrorCode: "metrics_write_failed"}
	} else {
		s.snapshot = next
		s.reportedFailure = false
	}
	s.closed = true
	return errors.Join(persistErr, s.releaseLock())
}

func (s *Store) update(apply func(*Snapshot) error) error {
	if s == nil {
		return ErrClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	next := cloneSnapshot(s.snapshot)
	if err := apply(&next); err != nil {
		return err
	}
	next.UpdatedAt = time.Now().UTC()
	next.Persistence = Persistence{Healthy: true}
	if err := s.persistLocked(next); err != nil {
		next.Persistence = Persistence{Healthy: false, ErrorCode: "metrics_write_failed"}
		s.snapshot = next
		s.emitFailure()
		return errors.New("metrics snapshot write failed")
	}
	next.Persistence = Persistence{Healthy: true}
	s.snapshot = next
	s.reportedFailure = false
	return nil
}

func (s *Store) persistLocked(snapshot Snapshot) error {
	data, err := json.Marshal(snapshot)
	if err != nil || len(data) > maxStateBytes {
		return errors.New("metrics snapshot encoding failed")
	}
	if err := validateExisting(s.path); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".metrics-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	err = directory.Sync()
	closeErr := directory.Close()
	return errors.Join(err, closeErr)
}

func (s *Store) emitFailure() {
	if s.logger != nil && !s.reportedFailure {
		s.reportedFailure = true
		s.logger.Error("metrics.persist.failed", "event", "metrics.persist.failed", "component", "runtime", "error_code", "metrics_write_failed")
	}
}

func (s *Store) releaseLock() error {
	if s.lockFile == nil {
		return nil
	}
	err := unlockState(s.lockFile)
	closeErr := s.lockFile.Close()
	s.lockFile = nil
	return errors.Join(err, closeErr)
}

func emptySnapshot(pipeline string, now time.Time) Snapshot {
	status := make(map[string]uint64, len(statusCodes))
	for _, key := range statusCodes {
		status[key] = 0
	}
	errorsByCode := make(map[string]uint64, len(errorCodes))
	for _, key := range errorCodes {
		errorsByCode[key] = 0
	}
	stages := make(map[string]StageStats, len(stageNames))
	for _, key := range stageNames {
		stages[key] = StageStats{Duration: newHistogram()}
	}
	return Snapshot{SchemaVersion: schemaVersion, PipelineVersion: pipeline, Since: now, UpdatedAt: now,
		Persistence: Persistence{Healthy: true}, HTTP: HTTPStats{ByStatus: status, Duration: newHistogram()},
		OCR: OCRStats{ByErrorCode: errorsByCode, Duration: newHistogram()}, Stages: stages}
}

func newHistogram() Histogram {
	buckets := make([]Bucket, 0, len(durationBoundaries)+1)
	for _, boundary := range durationBoundaries {
		value := boundary
		buckets = append(buckets, Bucket{LEMS: &value})
	}
	buckets = append(buckets, Bucket{LEMS: nil})
	return Histogram{Buckets: buckets}
}

func addDuration(hist *Histogram, duration time.Duration) error {
	ms := uint64(duration / time.Millisecond)
	if err := add(&hist.Count, 1); err != nil {
		return err
	}
	if err := add(&hist.SumMS, ms); err != nil {
		return err
	}
	if hist.Count == 1 || ms < hist.MinMS {
		hist.MinMS = ms
	}
	if ms > hist.MaxMS {
		hist.MaxMS = ms
	}
	for i, boundary := range durationBoundaries {
		if ms <= boundary {
			if err := add(&hist.Buckets[i].Count, 1); err != nil {
				return err
			}
		}
	}
	return add(&hist.Buckets[len(hist.Buckets)-1].Count, 1)
}

func add(target *uint64, amount uint64) error {
	if math.MaxUint64-*target < amount {
		return ErrCounterOverflow
	}
	*target += amount
	return nil
}

func cloneSnapshot(src Snapshot) Snapshot {
	dst := src
	dst.HTTP.ByStatus = cloneCounts(src.HTTP.ByStatus)
	dst.HTTP.Duration = cloneHistogram(src.HTTP.Duration)
	dst.OCR.ByErrorCode = cloneCounts(src.OCR.ByErrorCode)
	dst.OCR.Duration = cloneHistogram(src.OCR.Duration)
	dst.Stages = make(map[string]StageStats, len(src.Stages))
	for key, value := range src.Stages {
		value.Duration = cloneHistogram(value.Duration)
		dst.Stages[key] = value
	}
	return dst
}

func cloneCounts(src map[string]uint64) map[string]uint64 {
	dst := make(map[string]uint64, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func cloneHistogram(src Histogram) Histogram {
	dst := src
	dst.Buckets = make([]Bucket, len(src.Buckets))
	for i, bucket := range src.Buckets {
		dst.Buckets[i] = Bucket{Count: bucket.Count}
		if bucket.LEMS != nil {
			value := *bucket.LEMS
			dst.Buckets[i].LEMS = &value
		}
	}
	return dst
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func decodeSnapshot(data []byte, pipeline string, snapshot *Snapshot) (bool, error) {
	if len(data) == 0 || len(data) > maxStateBytes || duplicateKeys(data) != nil {
		return false, errors.New("invalid metrics snapshot")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(snapshot); err != nil {
		return false, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return false, errors.New("trailing metrics snapshot data")
	}
	if snapshot.SchemaVersion != schemaVersion || snapshot.PipelineVersion != pipeline || snapshot.Since.IsZero() || snapshot.UpdatedAt.IsZero() || snapshot.Persistence.ErrorCode != "" || !snapshot.Persistence.Healthy {
		return false, errors.New("metrics snapshot metadata mismatch")
	}
	upgraded := false
	if len(snapshot.HTTP.ByStatus) == len(legacyStatusCodes) || len(snapshot.HTTP.ByStatus) == len(previousStatusCodes) {
		known := legacyStatusCodes[:]
		if len(snapshot.HTTP.ByStatus) == len(previousStatusCodes) {
			known = previousStatusCodes[:]
		}
		for _, key := range known {
			if _, ok := snapshot.HTTP.ByStatus[key]; !ok {
				return false, errors.New("metrics status missing")
			}
		}
		for key := range snapshot.HTTP.ByStatus {
			if !contains(known, key) {
				return false, errors.New("metrics status unknown")
			}
		}
		for _, key := range statusCodes {
			if _, ok := snapshot.HTTP.ByStatus[key]; !ok {
				snapshot.HTTP.ByStatus[key] = 0
			}
		}
		upgraded = true
	} else if len(snapshot.HTTP.ByStatus) != len(statusCodes) {
		return false, errors.New("metrics snapshot cardinality mismatch")
	}
	if len(snapshot.OCR.ByErrorCode) != len(errorCodes) || len(snapshot.Stages) != len(stageNames) {
		return false, errors.New("metrics snapshot cardinality mismatch")
	}
	for _, key := range statusCodes {
		if _, ok := snapshot.HTTP.ByStatus[key]; !ok {
			return false, errors.New("metrics status missing")
		}
	}
	for key := range snapshot.HTTP.ByStatus {
		if !contains(statusCodes[:], key) {
			return false, errors.New("metrics status unknown")
		}
	}
	for _, key := range errorCodes {
		if _, ok := snapshot.OCR.ByErrorCode[key]; !ok {
			return false, errors.New("metrics error code missing")
		}
	}
	for key := range snapshot.OCR.ByErrorCode {
		if !contains(errorCodes[:], key) {
			return false, errors.New("metrics error code unknown")
		}
	}
	for _, key := range stageNames {
		if _, ok := snapshot.Stages[key]; !ok {
			return false, errors.New("metrics stage missing")
		}
	}
	for key := range snapshot.Stages {
		if !contains(stageNames[:], key) {
			return false, errors.New("metrics stage unknown")
		}
	}
	if snapshot.HTTP.Errors > snapshot.HTTP.Total || snapshot.OCR.Completed > snapshot.OCR.Total || snapshot.OCR.Failed != snapshot.OCR.Total-snapshot.OCR.Completed {
		return false, errors.New("metrics counter invariant failed")
	}
	var httpStatusTotal uint64
	var httpErrorTotal uint64
	for key, count := range snapshot.HTTP.ByStatus {
		status, err := strconv.Atoi(key)
		if err != nil {
			return false, errors.New("metrics status invalid")
		}
		if math.MaxUint64-httpStatusTotal < count {
			return false, errors.New("metrics status overflow")
		}
		httpStatusTotal += count
		if status >= 400 {
			if math.MaxUint64-httpErrorTotal < count {
				return false, errors.New("metrics error overflow")
			}
			httpErrorTotal += count
		}
	}
	if httpStatusTotal != snapshot.HTTP.Total || httpErrorTotal != snapshot.HTTP.Errors {
		return false, errors.New("metrics status total mismatch")
	}
	var errorTotal uint64
	for _, count := range snapshot.OCR.ByErrorCode {
		if math.MaxUint64-errorTotal < count {
			return false, errors.New("metrics error overflow")
		}
		errorTotal += count
	}
	if errorTotal != snapshot.OCR.Failed {
		return false, errors.New("metrics error total mismatch")
	}
	if err := validateHistogram(snapshot.HTTP.Duration); err != nil {
		return false, err
	}
	if err := validateHistogram(snapshot.OCR.Duration); err != nil {
		return false, err
	}
	for _, stats := range snapshot.Stages {
		if stats.Completed > math.MaxUint64-stats.Failed || stats.Completed+stats.Failed != stats.Duration.Count {
			return false, errors.New("metrics stage invariant failed")
		}
		if err := validateHistogram(stats.Duration); err != nil {
			return false, err
		}
	}
	if snapshot.HTTP.Duration.Count != snapshot.HTTP.Total || snapshot.OCR.Duration.Count != snapshot.OCR.Total {
		return false, errors.New("metrics histogram invariant failed")
	}
	return upgraded, nil
}

func validateHistogram(hist Histogram) error {
	if len(hist.Buckets) != len(durationBoundaries)+1 || hist.Count != hist.Buckets[len(hist.Buckets)-1].Count || hist.Count == 0 && (hist.SumMS != 0 || hist.MinMS != 0 || hist.MaxMS != 0) || hist.Count > 0 && (hist.MinMS > hist.MaxMS || hist.SumMS < hist.MaxMS) {
		return errors.New("metrics histogram invalid")
	}
	if hist.Count > 0 {
		meanFloor := hist.SumMS / hist.Count
		meanRemainder := hist.SumMS % hist.Count
		if meanFloor < hist.MinMS || meanFloor > hist.MaxMS || meanFloor == hist.MaxMS && meanRemainder != 0 {
			return errors.New("metrics histogram mean outside observed range")
		}
	}
	var previous uint64
	for i, boundary := range durationBoundaries {
		bucket := hist.Buckets[i]
		if bucket.LEMS == nil || *bucket.LEMS != boundary || bucket.Count < previous || bucket.Count > hist.Count {
			return errors.New("metrics histogram bucket invalid")
		}
		if boundary < hist.MinMS && bucket.Count != 0 || boundary >= hist.MaxMS && bucket.Count != hist.Count {
			return errors.New("metrics histogram range inconsistent with buckets")
		}
		previous = bucket.Count
	}
	if hist.Buckets[len(hist.Buckets)-1].LEMS != nil {
		return errors.New("metrics histogram infinity bucket invalid")
	}
	return nil
}

func duplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := scanJSONValue(decoder); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("invalid object key")
			}
			if _, exists := seen[key]; exists {
				return errors.New("duplicate JSON key")
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return errors.New("unexpected JSON delimiter")
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
		return errors.New("unsafe metrics directory")
	}
	return nil
}

func rejectSymlinkComponents(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	current := filepath.VolumeName(absolute) + string(os.PathSeparator)
	rest := strings.TrimPrefix(absolute, current)
	for _, component := range strings.Split(rest, string(os.PathSeparator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlink in metrics path")
		}
	}
	return nil
}

func validateExisting(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("unsafe metrics state")
	}
	return nil
}
