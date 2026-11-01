// Package imagestore stores private, immutable image objects on a local
// filesystem. Database references and job lifecycle are deliberately owned by
// other packages.
package imagestore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

const (
	privateDirMode  = 0o700
	privateFileMode = 0o600
	writeChunkSize  = 64 << 10
)

var (
	// ErrInvalidConfig indicates that the store root or size bound is invalid.
	ErrInvalidConfig = errors.New("invalid image store configuration")
	// ErrInvalidImage indicates an empty or unsupported image input.
	ErrInvalidImage = errors.New("invalid image")
	// ErrImageTooLarge indicates that the configured byte limit was exceeded.
	ErrImageTooLarge = errors.New("image exceeds configured size limit")
	// ErrInvalidKey indicates that an object key is not a generated opaque key.
	ErrInvalidKey = errors.New("invalid image key")
	// ErrInvalidObject indicates that an object is unsafe or not owned by this store.
	ErrInvalidObject = errors.New("invalid image object")
	// ErrNotFound indicates that an object does not exist.
	ErrNotFound = errors.New("image object not found")
	// ErrKeyCollision indicates that a generated object key already exists.
	ErrKeyCollision = errors.New("image key collision")
	// ErrStorageFailure hides filesystem details from callers.
	ErrStorageFailure = errors.New("image storage failure")
	// ErrCapacityExceeded indicates that the configured private volume budget
	// cannot accommodate another object.
	ErrCapacityExceeded = errors.New("image storage capacity exceeded")
)

var imageKeyPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var temporaryKeyPattern = regexp.MustCompile(`^\.upload-[0-9a-f]{32}$`)

// Config supplies the private root directory and mandatory maximum object size.
// MaxBytes must be selected by the caller; the store has no product default.
// MaxTotalBytes optionally caps logical bytes in the root; zero leaves the
// store unbounded for legacy callers. Async composition must set a positive cap.
type Config struct {
	Root          string
	MaxBytes      int64
	MaxTotalBytes int64
}

// Metadata describes a stored image. SHA256 is private metadata for the
// idempotency repository; callers must not expose or log it.
type Metadata struct {
	Key       string
	SHA256    string
	Size      int64
	MediaType string

	store             *Store
	identity          os.FileInfo
	originalKey       string
	originalSize      int64
	originalMediaType string
	originalSHA256    [sha256.Size]byte
}

type Usage struct {
	UsedBytes      int64
	MaxObjectBytes int64
	MaxTotalBytes  int64
}

// File is an opaque same-store snapshot used only by the offline maintenance
// command. Its filesystem identity and content digest are intentionally private.
type File struct {
	Key       string
	Size      int64
	SHA256    string
	Temporary bool

	store    *Store
	identity os.FileInfo
}

// Store owns a private directory containing immutable image files.
type Store struct {
	mu            sync.RWMutex
	root          *os.Root
	maxBytes      int64
	maxTotalBytes int64
	quotaGate     chan struct{}
	writeChunk    func(*os.File, []byte) (int, error)
	syncFile      func(*os.File) error
	syncDir       func(*os.Root) error
	newImageKey   func() (string, error)
	closeOnce     sync.Once
	closeErr      error
}

// New creates or validates the private storage root. An existing root with
// broader or different permissions is rejected rather than silently changed.
func New(config Config) (*Store, error) {
	if strings.TrimSpace(config.Root) == "" || config.MaxBytes <= 0 || config.MaxTotalBytes < 0 {
		return nil, ErrInvalidConfig
	}

	root, err := filepath.Abs(config.Root)
	if err != nil {
		return nil, ErrInvalidConfig
	}
	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		root, err = createPrivateRoot(root, syncRootDirectory)
		if err != nil {
			return nil, err
		}
		info, err = os.Lstat(root)
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != privateDirMode {
		return nil, ErrInvalidConfig
	}

	// Resolve symlinked ancestors once so subsequent object paths are rooted at
	// the actual directory. The configured root itself is rejected above if it
	// is a symlink.
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, ErrStorageFailure
	}
	resolvedRoot, err = filepath.Abs(resolvedRoot)
	if err != nil {
		return nil, ErrStorageFailure
	}
	rootHandle, err := os.OpenRoot(filepath.Clean(resolvedRoot))
	if err != nil {
		return nil, ErrStorageFailure
	}
	rootDirectory, err := rootHandle.Open(".")
	if err != nil {
		_ = rootHandle.Close()
		return nil, ErrStorageFailure
	}
	rootInfo, statErr := rootDirectory.Stat()
	closeErr := rootDirectory.Close()
	if statErr != nil || closeErr != nil || rootInfo == nil || !os.SameFile(info, rootInfo) {
		_ = rootHandle.Close()
		return nil, ErrInvalidConfig
	}
	store := &Store{
		root:          rootHandle,
		maxBytes:      config.MaxBytes,
		maxTotalBytes: config.MaxTotalBytes,
		writeChunk:    (*os.File).Write,
		syncFile:      func(file *os.File) error { return file.Sync() },
		syncDir:       syncRootDirectory,
		newImageKey:   randomImageKey,
	}
	if config.MaxTotalBytes > 0 {
		store.quotaGate = make(chan struct{}, 1)
	}
	return store, nil
}

// Close releases the rooted directory handle. It is safe to call more than once.
func (s *Store) Close() error {
	if s == nil || s.root == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeOnce.Do(func() {
		s.closeErr = s.root.Close()
	})
	if s.closeErr != nil {
		return ErrStorageFailure
	}
	return nil
}

// CheckAvailable verifies that the already-open private root remains usable.
// It does not resolve the configured path again, scan objects, or change data.
func (s *Store) CheckAvailable(ctx context.Context) error {
	if s == nil || s.root == nil || ctx == nil {
		return ErrInvalidConfig
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	directory, err := s.root.Open(".")
	if err != nil {
		return ErrStorageFailure
	}
	info, statErr := directory.Stat()
	closeErr := directory.Close()
	if statErr != nil || closeErr != nil || !info.IsDir() || info.Mode().Perm() != privateDirMode {
		return ErrStorageFailure
	}
	return ctx.Err()
}

// Usage returns aggregate logical-byte counts only; it never reports object
// names, keys, paths, or individual sizes.
func (s *Store) Usage(ctx context.Context) (Usage, error) {
	if s == nil || s.root == nil || ctx == nil {
		return Usage{}, ErrInvalidConfig
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.maxTotalBytes > 0 {
		select {
		case s.quotaGate <- struct{}{}:
			defer func() { <-s.quotaGate }()
		case <-ctx.Done():
			return Usage{}, ctx.Err()
		}
	}
	used, err := s.currentUsage(ctx)
	if err != nil {
		return Usage{}, err
	}
	return Usage{UsedBytes: used, MaxObjectBytes: s.maxBytes, MaxTotalBytes: s.maxTotalBytes}, nil
}

// ListFiles snapshots only files created by Put. Unknown names or unsafe
// entries fail closed so maintenance never removes unrelated root contents.
func (s *Store) ListFiles(ctx context.Context) ([]File, error) {
	if s == nil || s.root == nil || ctx == nil {
		return nil, ErrInvalidConfig
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	directory, err := s.root.Open(".")
	if err != nil {
		return nil, ErrStorageFailure
	}
	entries, readErr := directory.ReadDir(-1)
	closeErr := directory.Close()
	if readErr != nil || closeErr != nil {
		return nil, ErrStorageFailure
	}
	files := make([]File, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key := entry.Name()
		temporary := temporaryKeyPattern.MatchString(key)
		if !temporary && !imageKeyPattern.MatchString(key) {
			return nil, ErrInvalidObject
		}
		before, err := s.root.Lstat(key)
		if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() ||
			before.Mode().Perm() != privateFileMode || before.Size() < 0 || (!temporary && before.Size() == 0) || before.Size() > s.maxBytes {
			return nil, ErrInvalidObject
		}
		opened, err := s.root.Open(key)
		if err != nil {
			return nil, ErrStorageFailure
		}
		info, statErr := opened.Stat()
		if statErr != nil || info.Size() != before.Size() || !info.Mode().IsRegular() || info.Mode().Perm() != privateFileMode || !os.SameFile(before, info) {
			_ = opened.Close()
			return nil, ErrInvalidObject
		}
		digest, hashErr := hashContents(ctx, opened, before.Size())
		closeErr := opened.Close()
		if hashErr != nil || closeErr != nil {
			return nil, ErrStorageFailure
		}
		after, err := s.root.Lstat(key)
		if err != nil || after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() ||
			after.Mode().Perm() != privateFileMode || after.Size() != before.Size() || !os.SameFile(before, after) {
			return nil, ErrInvalidObject
		}
		files = append(files, File{Key: key, Size: before.Size(), SHA256: hex.EncodeToString(digest[:]), Temporary: temporary, store: s, identity: before})
	}
	return files, nil
}

// DeleteOrphan removes only a file returned by ListFiles on this Store. The
// caller must first confirm that UUID objects have no database reference and
// must run this operation with API intake stopped. The API retention runner
// never calls this method.
func (s *Store) DeleteOrphan(ctx context.Context, file File) error {
	if s == nil || ctx == nil {
		return ErrInvalidConfig
	}
	validKey := imageKeyPattern.MatchString(file.Key)
	if file.Temporary {
		validKey = temporaryKeyPattern.MatchString(file.Key)
	}
	if file.store != s || file.identity == nil || !validKey || file.Size < 0 || file.Size > s.maxBytes ||
		(!file.Temporary && file.Size == 0) || len(file.SHA256) != 64 {
		return ErrInvalidObject
	}
	digestBytes, err := hex.DecodeString(file.SHA256)
	if err != nil || hex.EncodeToString(digestBytes) != file.SHA256 {
		return ErrInvalidObject
	}
	var digest [sha256.Size]byte
	copy(digest[:], digestBytes)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := s.root.Lstat(file.Key)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() || current.Mode().Perm() != privateFileMode ||
		current.Size() != file.Size || !os.SameFile(file.identity, current) {
		return ErrInvalidObject
	}
	opened, err := s.root.Open(file.Key)
	if err != nil {
		return ErrStorageFailure
	}
	info, statErr := opened.Stat()
	if statErr != nil || info.Size() != file.Size || !info.Mode().IsRegular() || info.Mode().Perm() != privateFileMode || !os.SameFile(current, info) {
		_ = opened.Close()
		return ErrInvalidObject
	}
	if err := verifyContents(ctx, opened, file.Size, digest); err != nil {
		_ = opened.Close()
		return err
	}
	if err := opened.Close(); err != nil {
		return ErrStorageFailure
	}
	latest, err := s.root.Lstat(file.Key)
	if err != nil || latest.Mode()&os.ModeSymlink != 0 || !latest.Mode().IsRegular() || latest.Mode().Perm() != privateFileMode || latest.Size() != file.Size ||
		!os.SameFile(file.identity, latest) {
		return ErrInvalidObject
	}
	if err := s.root.Remove(file.Key); err != nil {
		return ErrStorageFailure
	}
	if err := syncRootDirectory(s.root); err != nil {
		return ErrStorageFailure
	}
	return nil
}

// Put writes a bounded image to a temporary file, syncs it, and atomically
// promotes it to an immutable opaque key without replacing an existing object.
// On an error after promotion the object may remain orphaned; callers must not
// infer that a database transaction did or did not commit from this error.
func (s *Store) Put(ctx context.Context, data []byte, mediaType string) (Metadata, error) {
	if s == nil || ctx == nil {
		return Metadata{}, ErrInvalidConfig
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return Metadata{}, err
	}
	normalizedType, ok := normalizeMediaType(mediaType)
	if !ok || len(data) == 0 {
		return Metadata{}, ErrInvalidImage
	}
	if int64(len(data)) > s.maxBytes {
		return Metadata{}, ErrImageTooLarge
	}
	if s.maxTotalBytes > 0 {
		select {
		case s.quotaGate <- struct{}{}:
			defer func() { <-s.quotaGate }()
		case <-ctx.Done():
			return Metadata{}, ctx.Err()
		}
		usage, err := s.currentUsage(ctx)
		if err != nil {
			return Metadata{}, err
		}
		if usage > s.maxTotalBytes || int64(len(data)) > s.maxTotalBytes-usage {
			return Metadata{}, ErrCapacityExceeded
		}
	}

	key, err := s.newImageKey()
	if err != nil || !imageKeyPattern.MatchString(key) {
		return Metadata{}, ErrStorageFailure
	}

	temporaryName, err := randomTemporaryName()
	if err != nil {
		return Metadata{}, ErrStorageFailure
	}
	temporary, err := s.root.OpenFile(temporaryName, os.O_CREATE|os.O_EXCL|os.O_RDWR, privateFileMode)
	if err != nil {
		return Metadata{}, ErrStorageFailure
	}
	defer func() { _ = s.root.Remove(temporaryName) }()
	if err := temporary.Chmod(privateFileMode); err != nil {
		_ = temporary.Close()
		return Metadata{}, ErrStorageFailure
	}
	if err := writeContext(ctx, temporary, data, s.writeChunk); err != nil {
		_ = temporary.Close()
		return Metadata{}, err
	}
	if err := ctx.Err(); err != nil {
		_ = temporary.Close()
		return Metadata{}, err
	}
	if err := s.syncFile(temporary); err != nil {
		_ = temporary.Close()
		return Metadata{}, ErrStorageFailure
	}
	if err := temporary.Close(); err != nil {
		return Metadata{}, ErrStorageFailure
	}

	// A hard link on the same filesystem publishes the complete inode atomically
	// and fails if the destination already exists; rename would overwrite it.
	if err := s.root.Link(temporaryName, key); err != nil {
		if errors.Is(err, os.ErrExist) {
			return Metadata{}, ErrKeyCollision
		}
		return Metadata{}, ErrStorageFailure
	}
	if err := s.root.Remove(temporaryName); err != nil {
		return Metadata{}, ErrStorageFailure
	}
	if err := s.syncDir(s.root); err != nil {
		return Metadata{}, ErrStorageFailure
	}

	info, err := s.root.Lstat(key)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != privateFileMode {
		return Metadata{}, ErrStorageFailure
	}
	digest := sha256.Sum256(data)
	return Metadata{
		Key: key, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(data)),
		MediaType: normalizedType, store: s, identity: info,
		originalKey: key, originalSize: int64(len(data)), originalMediaType: normalizedType,
		originalSHA256: digest,
	}, nil
}

// currentUsage counts every private regular file in the rooted directory,
// including published orphans and leftover upload temporaries. The store root
// is single-writer: concurrent external filesystem writers are unsupported.
func (s *Store) currentUsage(ctx context.Context) (int64, error) {
	directory, err := s.root.Open(".")
	if err != nil {
		return 0, ErrStorageFailure
	}
	entries, readErr := directory.ReadDir(-1)
	closeErr := directory.Close()
	if readErr != nil || closeErr != nil {
		return 0, ErrStorageFailure
	}
	var total int64
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		name := entry.Name()
		before, err := s.root.Lstat(name)
		if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || before.Mode().Perm() != privateFileMode || before.Size() < 0 {
			return 0, ErrStorageFailure
		}
		file, err := s.root.Open(name)
		if err != nil {
			return 0, ErrStorageFailure
		}
		opened, statErr := file.Stat()
		closeErr := file.Close()
		if statErr != nil || closeErr != nil || !opened.Mode().IsRegular() || opened.Mode().Perm() != privateFileMode ||
			opened.Size() != before.Size() || !os.SameFile(before, opened) {
			return 0, ErrStorageFailure
		}
		if before.Size() > math.MaxInt64-total {
			return 0, ErrStorageFailure
		}
		total += before.Size()
	}
	return total, nil
}

// Open returns a read-only handle for an object key loaded from trusted job
// metadata. It rejects malformed keys and symlink/non-regular object paths.
func (s *Store) Open(ctx context.Context, key string) (*os.File, error) {
	if s == nil || ctx == nil {
		return nil, ErrInvalidConfig
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !imageKeyPattern.MatchString(key) {
		return nil, ErrInvalidKey
	}
	before, err := s.root.Lstat(key)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, ErrStorageFailure
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || before.Mode().Perm() != privateFileMode {
		return nil, ErrInvalidObject
	}

	file, err := s.root.Open(key)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, ErrStorageFailure
	}
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || after.Mode().Perm() != privateFileMode || !os.SameFile(before, after) {
		_ = file.Close()
		return nil, ErrInvalidObject
	}
	if err := ctx.Err(); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// DeleteUploaded removes only an object returned by Put on this Store and only
// if the path still names the same regular file. It is for a caller that has
// positively established that its database insert lost an idempotency race.
// Never use it after an ambiguous transaction/commit error. External filesystem
// writers are unsupported; keep the root private and give the OCR worker
// read-only access.
func (s *Store) DeleteUploaded(ctx context.Context, object Metadata) error {
	if s == nil || ctx == nil {
		return ErrInvalidConfig
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if object.store != s || object.identity == nil || !imageKeyPattern.MatchString(object.originalKey) ||
		object.Key != object.originalKey || object.Size != object.originalSize ||
		object.MediaType != object.originalMediaType || object.SHA256 != hex.EncodeToString(object.originalSHA256[:]) {
		return ErrInvalidObject
	}
	current, err := s.root.Lstat(object.originalKey)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return ErrStorageFailure
	}
	if current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() || current.Size() != object.originalSize ||
		!os.SameFile(object.identity, current) {
		return ErrInvalidObject
	}
	file, err := s.root.Open(object.originalKey)
	if errors.Is(err, os.ErrNotExist) {
		return ErrInvalidObject
	}
	if err != nil {
		return ErrStorageFailure
	}
	opened, statErr := file.Stat()
	if statErr != nil || opened.Mode()&os.ModeSymlink != 0 || !opened.Mode().IsRegular() ||
		opened.Size() != object.originalSize || !os.SameFile(object.identity, opened) || !os.SameFile(current, opened) {
		_ = file.Close()
		return ErrInvalidObject
	}
	if err := verifyContents(ctx, file, object.originalSize, object.originalSHA256); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return ErrStorageFailure
	}
	latest, err := s.root.Lstat(object.originalKey)
	if err != nil || latest.Mode()&os.ModeSymlink != 0 || !latest.Mode().IsRegular() ||
		latest.Size() != object.originalSize || !os.SameFile(object.identity, latest) {
		return ErrInvalidObject
	}
	if err := s.root.Remove(object.originalKey); err != nil {
		return ErrStorageFailure
	}
	if err := syncRootDirectory(s.root); err != nil {
		return ErrStorageFailure
	}
	return nil
}

// DeleteObject removes a previously persisted object by its opaque key after
// the database has confirmed that the owning job is terminal. expectedSHA256
// is empty only for legacy rows created before the digest column existed.
// The root is single-writer and the worker has read-only access.
func (s *Store) DeleteObject(ctx context.Context, key string, expectedSize int64, expectedSHA256 string) error {
	if s == nil || ctx == nil || !imageKeyPattern.MatchString(key) || expectedSize <= 0 || expectedSize > s.maxBytes {
		return ErrInvalidObject
	}
	var digest [sha256.Size]byte
	if expectedSHA256 != "" {
		decoded, err := hex.DecodeString(expectedSHA256)
		if err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != expectedSHA256 {
			return ErrInvalidObject
		}
		copy(digest[:], decoded)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	before, err := s.root.Lstat(key)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return ErrStorageFailure
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || before.Mode().Perm() != privateFileMode || before.Size() != expectedSize {
		return ErrInvalidObject
	}
	file, err := s.root.Open(key)
	if err != nil {
		return ErrStorageFailure
	}
	opened, statErr := file.Stat()
	if statErr != nil || !opened.Mode().IsRegular() || opened.Mode().Perm() != privateFileMode ||
		opened.Size() != expectedSize || !os.SameFile(before, opened) {
		_ = file.Close()
		return ErrInvalidObject
	}
	if expectedSHA256 != "" {
		if err := verifyContents(ctx, file, expectedSize, digest); err != nil {
			_ = file.Close()
			return err
		}
	}
	if err := file.Close(); err != nil {
		return ErrStorageFailure
	}
	latest, err := s.root.Lstat(key)
	if err != nil || latest.Mode()&os.ModeSymlink != 0 || !latest.Mode().IsRegular() || latest.Mode().Perm() != privateFileMode ||
		latest.Size() != expectedSize || !os.SameFile(before, latest) {
		return ErrInvalidObject
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.root.Remove(key); err != nil {
		return ErrStorageFailure
	}
	if err := syncRootDirectory(s.root); err != nil {
		return ErrStorageFailure
	}
	return nil
}

// verifyContents hashes no more than expectedSize+1 bytes, allowing detection
// of an unexpectedly larger replacement without reading it without bound.
func verifyContents(ctx context.Context, file *os.File, expectedSize int64, expectedDigest [sha256.Size]byte) error {
	if expectedSize < 0 || expectedSize >= math.MaxInt64 {
		return ErrInvalidObject
	}
	hasher := sha256.New()
	buffer := make([]byte, writeChunkSize)
	remaining := expectedSize + 1
	var read int64
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk := buffer
		if int64(len(chunk)) > remaining {
			chunk = chunk[:remaining]
		}
		count, err := file.Read(chunk)
		if count > 0 {
			_, _ = hasher.Write(chunk[:count])
			read += int64(count)
			remaining -= int64(count)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || count == 0 {
			return ErrStorageFailure
		}
	}
	if read != expectedSize || !bytes.Equal(hasher.Sum(nil), expectedDigest[:]) {
		return ErrInvalidObject
	}
	return nil
}

func hashContents(ctx context.Context, file *os.File, expectedSize int64) ([sha256.Size]byte, error) {
	if expectedSize < 0 || expectedSize >= math.MaxInt64 {
		return [sha256.Size]byte{}, ErrInvalidObject
	}
	hasher := sha256.New()
	buffer := make([]byte, writeChunkSize)
	remaining := expectedSize + 1
	var read int64
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return [sha256.Size]byte{}, err
		}
		chunk := buffer
		if int64(len(chunk)) > remaining {
			chunk = chunk[:remaining]
		}
		count, err := file.Read(chunk)
		if count > 0 {
			_, _ = hasher.Write(chunk[:count])
			read += int64(count)
			remaining -= int64(count)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || count == 0 {
			return [sha256.Size]byte{}, ErrStorageFailure
		}
	}
	if read != expectedSize {
		return [sha256.Size]byte{}, ErrInvalidObject
	}
	var result [sha256.Size]byte
	copy(result[:], hasher.Sum(nil))
	return result, nil
}

func normalizeMediaType(value string) (string, bool) {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return "", false
	}
	mediaType = strings.ToLower(mediaType)
	switch mediaType {
	case "image/jpeg", "image/png", "image/webp":
		return mediaType, true
	default:
		return "", false
	}
}

func writeContext(ctx context.Context, file *os.File, data []byte, writeChunk func(*os.File, []byte) (int, error)) error {
	for len(data) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk := data
		if len(chunk) > writeChunkSize {
			chunk = chunk[:writeChunkSize]
		}
		written, err := writeChunk(file, chunk)
		if err != nil {
			return ErrStorageFailure
		}
		if written <= 0 || written > len(chunk) {
			return ErrStorageFailure
		}
		data = data[written:]
	}
	return nil
}

func syncRootDirectory(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// createPrivateRoot builds only the missing suffix of an absolute directory
// path. It syncs each created directory and its parent; existing ancestors are
// neither chmodded nor removed.
func createPrivateRoot(configured string, syncDir func(*os.Root) error) (string, error) {
	cursor := configured
	missing := make([]string, 0, 4)
	var existing string
	for {
		info, err := os.Lstat(cursor)
		if err == nil {
			if !info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
				return "", ErrInvalidConfig
			}
			resolved, err := filepath.EvalSymlinks(cursor)
			if err != nil {
				return "", ErrStorageFailure
			}
			resolvedInfo, err := os.Stat(resolved)
			if err != nil || !resolvedInfo.IsDir() {
				return "", ErrInvalidConfig
			}
			existing = resolved
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", ErrStorageFailure
		}
		parent := filepath.Dir(cursor)
		if parent == cursor {
			return "", ErrInvalidConfig
		}
		missing = append(missing, filepath.Base(cursor))
		cursor = parent
	}

	parent := existing
	parentRoot, err := os.OpenRoot(parent)
	if err != nil {
		return "", ErrStorageFailure
	}
	defer func() { _ = parentRoot.Close() }()
	for index := len(missing) - 1; index >= 0; index-- {
		component := missing[index]
		if component == "" || component == "." || component == string(filepath.Separator) {
			return "", ErrInvalidConfig
		}
		childPath := filepath.Join(parent, component)
		mkdirErr := parentRoot.Mkdir(component, privateDirMode)
		if mkdirErr != nil && !errors.Is(mkdirErr, os.ErrExist) {
			return "", ErrStorageFailure
		}
		info, statErr := parentRoot.Lstat(component)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != privateDirMode {
			return "", ErrInvalidConfig
		}
		childRoot, err := parentRoot.OpenRoot(component)
		if err != nil {
			return "", ErrStorageFailure
		}
		childDirectory, err := childRoot.Open(".")
		if err != nil {
			_ = childRoot.Close()
			return "", ErrStorageFailure
		}
		childInfo, statErr := childDirectory.Stat()
		closeErr := childDirectory.Close()
		if statErr != nil || closeErr != nil || childInfo == nil || !os.SameFile(info, childInfo) {
			_ = childRoot.Close()
			return "", ErrInvalidConfig
		}
		if err := syncDir(childRoot); err != nil {
			_ = childRoot.Close()
			return "", ErrStorageFailure
		}
		if err := syncDir(parentRoot); err != nil {
			_ = childRoot.Close()
			return "", ErrStorageFailure
		}
		if err := parentRoot.Close(); err != nil {
			_ = childRoot.Close()
			return "", ErrStorageFailure
		}
		parentRoot = childRoot
		parent = childPath
	}
	return parent, nil
}

func randomTemporaryName() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return ".upload-" + hex.EncodeToString(value[:]), nil
}

func randomImageKey() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return hex.EncodeToString(value[0:4]) + "-" +
		hex.EncodeToString(value[4:6]) + "-" +
		hex.EncodeToString(value[6:8]) + "-" +
		hex.EncodeToString(value[8:10]) + "-" +
		hex.EncodeToString(value[10:16]), nil
}
