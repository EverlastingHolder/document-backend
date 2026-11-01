package imagestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNewRequiresRootAndPositiveLimit(t *testing.T) {
	for _, config := range []Config{
		{Root: " ", MaxBytes: 1},
		{Root: filepath.Join(t.TempDir(), "images"), MaxBytes: 0},
		{Root: filepath.Join(t.TempDir(), "images"), MaxBytes: -1},
	} {
		if _, err := New(config); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("New(%+v) error = %v, want ErrInvalidConfig", config, err)
		}
	}
}

func TestNewRejectsNegativeTotalLimit(t *testing.T) {
	if _, err := New(Config{Root: filepath.Join(t.TempDir(), "images"), MaxBytes: 8, MaxTotalBytes: -1}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New() error = %v, want ErrInvalidConfig", err)
	}
}

func TestNewCreatesPrivateRootAndRejectsUnsafeExistingRoots(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private", "images")
	store, err := New(Config{Root: root, MaxBytes: 128})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close(): %v", err)
		}
	})
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatalf("Lstat(root): %v", err)
	}
	if !info.IsDir() || info.Mode().Perm() != privateDirMode {
		t.Fatalf("root mode/type = %v, want directory %o", info.Mode(), privateDirMode)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks(root): %v", err)
	}
	if store.root.Name() != resolvedRoot {
		t.Fatalf("resolved root = %q, want %q", store.root.Name(), resolvedRoot)
	}

	tooOpen := filepath.Join(t.TempDir(), "too-open")
	if err := os.Mkdir(tooOpen, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Root: tooOpen, MaxBytes: 128}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New() accepted non-private existing root: %v", err)
	}

	target := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(target, privateDirMode); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := New(Config{Root: link, MaxBytes: 128}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New() accepted symlink root: %v", err)
	}
}

func TestNewSyncsCreatedDirectoriesAndFailsClosedOnParentSyncFailure(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "private", "images")
	canonicalBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatalf("EvalSymlinks(base): %v", err)
	}
	var synced []string
	created, err := createPrivateRoot(root, func(directory *os.Root) error {
		synced = append(synced, directory.Name())
		return syncRootDirectory(directory)
	})
	if err != nil {
		t.Fatalf("createPrivateRoot() error = %v", err)
	}
	wantRoot := filepath.Join(canonicalBase, "private", "images")
	if created != wantRoot {
		t.Fatalf("created root = %q, want %q", created, wantRoot)
	}
	wantSyncs := []string{
		filepath.Join(canonicalBase, "private"),
		canonicalBase,
		filepath.Join(canonicalBase, "private", "images"),
		filepath.Join(canonicalBase, "private"),
	}
	if !reflect.DeepEqual(synced, wantSyncs) {
		t.Fatalf("sync order = %q, want %q", synced, wantSyncs)
	}

	failingRoot := filepath.Join(canonicalBase, "failure", "images")
	_, err = createPrivateRoot(failingRoot, func(directory *os.Root) error {
		if directory.Name() == canonicalBase {
			return errors.New("injected parent directory sync failure")
		}
		return syncRootDirectory(directory)
	})
	if !errors.Is(err, ErrStorageFailure) {
		t.Fatalf("createPrivateRoot() parent-sync error = %v, want ErrStorageFailure", err)
	}
	partialRoot := filepath.Join(canonicalBase, "failure")
	info, statErr := os.Lstat(partialRoot)
	if statErr != nil || !info.IsDir() || info.Mode().Perm() != privateDirMode {
		t.Fatalf("failed private ancestor was removed or unsafe: info=%v err=%v", info, statErr)
	}
	if _, err := os.Lstat(failingRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("creation continued after parent sync failure: %v", err)
	}
}

func TestPutStoresPrivateOpaqueObjectAndOpenSurvivesReopen(t *testing.T) {
	root := filepath.Join(t.TempDir(), "images")
	store := newTestStore(t, root, 128)
	data := []byte("private image bytes")
	object, err := store.Put(context.Background(), data, "IMAGE/PNG; charset=binary")
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if object.MediaType != "image/png" || object.Size != int64(len(data)) {
		t.Fatalf("metadata = %+v, want normalized media type and byte size", object)
	}
	if !imageKeyPattern.MatchString(object.Key) || strings.Contains(object.Key, "/") {
		t.Fatalf("object key %q is not opaque", object.Key)
	}
	digest := sha256.Sum256(data)
	if want := hex.EncodeToString(digest[:]); object.SHA256 != want {
		t.Fatalf("SHA256 = %q, want %q", object.SHA256, want)
	}
	if object.store != store || object.identity == nil {
		t.Fatal("Put() did not return a store-owned deletion handle")
	}

	path := filepath.Join(root, object.Key)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat(object): %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != privateFileMode {
		t.Fatalf("object mode/type = %v, want regular file %o", info.Mode(), privateFileMode)
	}

	reopened := newTestStore(t, root, 128)
	file, err := reopened.Open(context.Background(), object.Key)
	if err != nil {
		t.Fatalf("Open() after store reopen: %v", err)
	}
	defer file.Close()
	contents, err := io.ReadAll(file)
	if err != nil {
		t.Fatalf("ReadAll(): %v", err)
	}
	if !bytes.Equal(contents, data) {
		t.Fatalf("reopened bytes = %q, want %q", contents, data)
	}
}

func TestPutRejectsEmptyOversizeInvalidMediaTypeAndCancelledContext(t *testing.T) {
	store := newTestStore(t, filepath.Join(t.TempDir(), "images"), 4)
	cases := []struct {
		name      string
		data      []byte
		mediaType string
		want      error
	}{
		{name: "empty", data: nil, mediaType: "image/png", want: ErrInvalidImage},
		{name: "unsupported type", data: []byte("a"), mediaType: "text/plain", want: ErrInvalidImage},
		{name: "malformed type", data: []byte("a"), mediaType: "image/", want: ErrInvalidImage},
		{name: "oversize", data: []byte("12345"), mediaType: "image/png", want: ErrImageTooLarge},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := store.Put(context.Background(), test.data, test.mediaType); !errors.Is(err, test.want) {
				t.Fatalf("Put() error = %v, want %v", err, test.want)
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Put(ctx, []byte("a"), "image/png"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put() cancelled error = %v, want context.Canceled", err)
	}
	entries, err := os.ReadDir(store.root.Name())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("invalid inputs created files: %v", entries)
	}
}

func TestPutDoesNotOverwriteOnGeneratedKeyCollision(t *testing.T) {
	store := newTestStore(t, filepath.Join(t.TempDir(), "images"), 128)
	key := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	store.newImageKey = func() (string, error) { return key, nil }
	path := filepath.Join(store.root.Name(), key)
	want := []byte("existing object")
	if err := os.WriteFile(path, want, privateFileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), []byte("replacement"), "image/png"); !errors.Is(err, ErrKeyCollision) {
		t.Fatalf("Put() collision error = %v, want ErrKeyCollision", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("existing object changed to %q", got)
	}
	entries, err := os.ReadDir(store.root.Name())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("temporary file left after collision: %v", entries)
	}
}

func TestPutGeneratesDistinctKeysAndDeleteUploadedRemovesOwnedObject(t *testing.T) {
	store := newTestStore(t, filepath.Join(t.TempDir(), "images"), 128)
	first, err := store.Put(context.Background(), []byte("first"), "image/jpeg")
	if err != nil {
		t.Fatalf("first Put(): %v", err)
	}
	second, err := store.Put(context.Background(), []byte("second"), "image/jpeg")
	if err != nil {
		t.Fatalf("second Put(): %v", err)
	}
	if first.Key == second.Key {
		t.Fatalf("Put() generated duplicate key %q", first.Key)
	}
	if err := store.DeleteUploaded(context.Background(), first); err != nil {
		t.Fatalf("DeleteUploaded(owned object): %v", err)
	}
	if _, err := store.Open(context.Background(), first.Key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open(deleted object) error = %v, want ErrNotFound", err)
	}
	if file, err := store.Open(context.Background(), second.Key); err != nil {
		t.Fatalf("Open(other object after delete): %v", err)
	} else {
		_ = file.Close()
	}
}

func TestTotalQuotaCountsPersistedObjectsAndDeleteFreesCapacity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "images")
	store := newTestStoreWithQuota(t, root, 8, 10)
	first, err := store.Put(context.Background(), []byte("123456"), "image/jpeg")
	if err != nil {
		t.Fatalf("first Put(): %v", err)
	}
	if _, err := store.Put(context.Background(), []byte("12345"), "image/jpeg"); !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("Put() beyond total quota error = %v, want ErrCapacityExceeded", err)
	}
	if err := store.DeleteUploaded(context.Background(), first); err != nil {
		t.Fatalf("DeleteUploaded(): %v", err)
	}
	if _, err := store.Put(context.Background(), []byte("123456"), "image/jpeg"); err != nil {
		t.Fatalf("Put() after deletion freed capacity: %v", err)
	}
}

func TestTotalQuotaCountsOrphanFilesAndSurvivesReopen(t *testing.T) {
	root := filepath.Join(t.TempDir(), "images")
	store := newTestStoreWithQuota(t, root, 8, 10)
	if _, err := store.Put(context.Background(), []byte("123456"), "image/jpeg"); err != nil {
		t.Fatalf("Put() initial object: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	store = newTestStoreWithQuota(t, root, 8, 10)
	// A promoted object without a job row is still retained filesystem usage.
	if _, err := store.Put(context.Background(), []byte("12345"), "image/jpeg"); !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("Put() after reopen error = %v, want retained object counted", err)
	}
	// A leftover exclusive temp file from a crash also consumes the quota.
	orphan, err := store.root.OpenFile(".upload-recovery-fixture", os.O_CREATE|os.O_EXCL|os.O_WRONLY, privateFileMode)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orphan.Write([]byte("1234")); err != nil {
		_ = orphan.Close()
		t.Fatal(err)
	}
	if err := orphan.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), []byte("1"), "image/jpeg"); !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("Put() with orphan temp error = %v, want ErrCapacityExceeded", err)
	}
}

func TestTotalQuotaRejectsUnexpectedDirectoriesAndSymlinks(t *testing.T) {
	t.Run("directory", func(t *testing.T) {
		store := newTestStoreWithQuota(t, filepath.Join(t.TempDir(), "images"), 8, 16)
		if err := store.root.Mkdir("unexpected", privateDirMode); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Put(context.Background(), []byte("x"), "image/jpeg"); !errors.Is(err, ErrStorageFailure) {
			t.Fatalf("Put() with unexpected directory error = %v, want ErrStorageFailure", err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		store := newTestStoreWithQuota(t, filepath.Join(t.TempDir(), "images"), 8, 16)
		outside := filepath.Join(t.TempDir(), "outside")
		if err := os.WriteFile(outside, []byte("outside"), privateFileMode); err != nil {
			t.Fatal(err)
		}
		if err := store.root.Symlink(outside, "unexpected-link"); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if _, err := store.Put(context.Background(), []byte("x"), "image/jpeg"); !errors.Is(err, ErrStorageFailure) {
			t.Fatalf("Put() with unexpected symlink error = %v, want ErrStorageFailure", err)
		}
	})
}

func TestConcurrentQuotaPutsCannotExceedTotalLimit(t *testing.T) {
	store := newTestStoreWithQuota(t, filepath.Join(t.TempDir(), "images"), 8, 10)
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := store.Put(context.Background(), []byte("123456"), "image/jpeg")
			results <- err
		}()
	}
	close(start)
	first, second := <-results, <-results
	successes := 0
	capacityFailures := 0
	for _, err := range []error{first, second} {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrCapacityExceeded):
			capacityFailures++
		default:
			t.Fatalf("Put() error = %v, want success or capacity failure", err)
		}
	}
	if successes != 1 || capacityFailures != 1 {
		t.Fatalf("successes=%d capacityFailures=%d, want 1 each", successes, capacityFailures)
	}
}

func TestQuotaWaitRespectsContextCancellation(t *testing.T) {
	store := newTestStoreWithQuota(t, filepath.Join(t.TempDir(), "images"), 8, 16)
	store.quotaGate <- struct{}{} // Hold the quota slot to model another in-flight quota scan/write.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Put(ctx, []byte("cancelled"), "image/png"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put() while waiting for quota gate error = %v, want context.Canceled", err)
	}
	entries, err := os.ReadDir(store.root.Name())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("cancelled quota wait created files: %v", entries)
	}
}

func TestPutWriteFailureRemovesTemporaryFile(t *testing.T) {
	store := newTestStore(t, filepath.Join(t.TempDir(), "images"), writeChunkSize+1)
	// Make the second chunk fail after a partial temporary-file write.
	writeCount := 0
	store.writeChunk = func(file *os.File, data []byte) (int, error) {
		writeCount++
		if writeCount > 1 {
			return 0, errors.New("injected write failure")
		}
		if len(data) > 1 {
			data = data[:1]
		}
		return file.Write(data)
	}
	large := bytes.Repeat([]byte("x"), writeChunkSize+1)
	if _, err := store.Put(context.Background(), large, "image/png"); !errors.Is(err, ErrStorageFailure) {
		t.Fatalf("Put() error = %v, want ErrStorageFailure", err)
	}
	entries, err := os.ReadDir(store.root.Name())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary object remains after write failure: %v", entries)
	}
}

func TestPutFileSyncFailureDoesNotPromoteObject(t *testing.T) {
	store := newTestStore(t, filepath.Join(t.TempDir(), "images"), 128)
	store.syncFile = func(*os.File) error {
		return errors.New("injected file sync failure")
	}
	if object, err := store.Put(context.Background(), []byte("private"), "image/png"); !errors.Is(err, ErrStorageFailure) || object.Key != "" {
		t.Fatalf("Put() = (%+v, %v), want no object and ErrStorageFailure", object, err)
	}
	entries, err := os.ReadDir(store.root.Name())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("file sync failure promoted or leaked an object: %v", entries)
	}
}

func TestPutDirectorySyncFailureReturnsErrorAndLeavesSafeOrphan(t *testing.T) {
	store := newTestStore(t, filepath.Join(t.TempDir(), "images"), 128)
	store.syncDir = func(*os.Root) error {
		return errors.New("injected object directory sync failure")
	}
	object, err := store.Put(context.Background(), []byte("private"), "image/png")
	if !errors.Is(err, ErrStorageFailure) || object.Key != "" {
		t.Fatalf("Put() = (%+v, %v), want no success metadata and ErrStorageFailure", object, err)
	}
	entries, err := os.ReadDir(store.root.Name())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !imageKeyPattern.MatchString(entries[0].Name()) {
		t.Fatalf("directory sync failure left unsafe entries: %v", entries)
	}
	info, err := os.Lstat(filepath.Join(store.root.Name(), entries[0].Name()))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != privateFileMode {
		t.Fatalf("directory sync failure orphan is unsafe: info=%v err=%v", info, err)
	}
}

func TestOpenRejectsTraversalMalformedAndSymlinkObjects(t *testing.T) {
	store := newTestStore(t, filepath.Join(t.TempDir(), "images"), 128)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside"), privateFileMode); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"../outside", "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA", "", "sub/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"} {
		if _, err := store.Open(context.Background(), key); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Open(%q) error = %v, want ErrInvalidKey", key, err)
		}
	}

	key := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	if err := os.Symlink(outside, filepath.Join(store.root.Name(), key)); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := store.Open(context.Background(), key); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("Open(symlink key) error = %v, want ErrInvalidObject", err)
	}
}

func TestDeleteUploadedRequiresOwnUnchangedObject(t *testing.T) {
	store := newTestStore(t, filepath.Join(t.TempDir(), "images"), 128)
	other := newTestStore(t, filepath.Join(t.TempDir(), "other-images"), 128)
	object, err := store.Put(context.Background(), []byte("private"), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if err := other.DeleteUploaded(context.Background(), object); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("other store deleted image: %v", err)
	}
	if err := store.DeleteUploaded(context.Background(), Metadata{Key: object.Key}); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("forged metadata deleted image: %v", err)
	}

	path := filepath.Join(store.root.Name(), object.Key)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), privateFileMode); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteUploaded(context.Background(), object); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("DeleteUploaded() removed replaced inode: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "replacement" {
		t.Fatalf("replacement object = %q, error %v", got, err)
	}
}

func TestDeleteUploadedRejectsMutatedPublicMetadata(t *testing.T) {
	store := newTestStore(t, filepath.Join(t.TempDir(), "images"), 128)
	object, err := store.Put(context.Background(), []byte("private"), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		mutate func(*Metadata)
	}{
		{name: "key", mutate: func(value *Metadata) { value.Key = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb" }},
		{name: "digest", mutate: func(value *Metadata) { value.SHA256 = strings.Repeat("0", 64) }},
		{name: "size", mutate: func(value *Metadata) { value.Size++ }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := object
			test.mutate(&mutated)
			if err := store.DeleteUploaded(context.Background(), mutated); !errors.Is(err, ErrInvalidObject) {
				t.Fatalf("DeleteUploaded(mutated metadata) error = %v, want ErrInvalidObject", err)
			}
		})
	}
	file, err := store.Open(context.Background(), object.Key)
	if err != nil {
		t.Fatalf("Open(original object): %v", err)
	}
	defer file.Close()
	got, err := io.ReadAll(file)
	if err != nil || string(got) != "private" {
		t.Fatalf("original object = %q, error %v", got, err)
	}
}

func TestDeleteUploadedRejectsSameSizeInPlaceMutation(t *testing.T) {
	store := newTestStore(t, filepath.Join(t.TempDir(), "images"), 128)
	object, err := store.Put(context.Background(), []byte("private"), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	file, err := store.root.OpenFile(object.Key, os.O_WRONLY|os.O_TRUNC, privateFileMode)
	if err != nil {
		t.Fatalf("OpenFile(object): %v", err)
	}
	if _, err := file.Write([]byte("changed")); err != nil {
		_ = file.Close()
		t.Fatalf("Write(replacement): %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close(replacement): %v", err)
	}
	if err := store.DeleteUploaded(context.Background(), object); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("DeleteUploaded(same-size mutation) error = %v, want ErrInvalidObject", err)
	}
	file, err = store.Open(context.Background(), object.Key)
	if err != nil {
		t.Fatalf("Open(mutated object): %v", err)
	}
	defer file.Close()
	got, err := io.ReadAll(file)
	if err != nil || string(got) != "changed" {
		t.Fatalf("mutated object = %q, error %v", got, err)
	}
}

func TestDeleteObjectPersistsAcrossStoreReopenAndGuardsDigest(t *testing.T) {
	root := filepath.Join(t.TempDir(), "images")
	store, err := New(Config{Root: root, MaxBytes: 128})
	if err != nil {
		t.Fatal(err)
	}
	object, err := store.Put(context.Background(), []byte("private image"), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(Config{Root: root, MaxBytes: 128})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	usage, err := reopened.Usage(context.Background())
	if err != nil || usage.UsedBytes != object.Size {
		t.Fatalf("Usage() before deletion = (%+v,%v)", usage, err)
	}
	if err := reopened.DeleteObject(context.Background(), object.Key, object.Size, strings.Repeat("0", 64)); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("DeleteObject(wrong digest) error = %v, want ErrInvalidObject", err)
	}
	if _, err := reopened.Open(context.Background(), object.Key); err != nil {
		t.Fatalf("wrong digest removed stored object: %v", err)
	}
	if err := reopened.DeleteObject(context.Background(), object.Key, object.Size, object.SHA256); err != nil {
		t.Fatalf("DeleteObject(correct digest): %v", err)
	}
	usage, err = reopened.Usage(context.Background())
	if err != nil || usage.UsedBytes != 0 {
		t.Fatalf("Usage() after deletion = (%+v,%v), want zero bytes", usage, err)
	}
	if err := reopened.DeleteObject(context.Background(), object.Key, object.Size, object.SHA256); err != nil {
		t.Fatalf("DeleteObject(absent idempotent object): %v", err)
	}
}

func TestCheckAvailableUsesOpenPrivateRoot(t *testing.T) {
	store := newTestStore(t, filepath.Join(t.TempDir(), "images"), 128)
	if err := store.CheckAvailable(context.Background()); err != nil {
		t.Fatalf("CheckAvailable(): %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.CheckAvailable(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("CheckAvailable(canceled) = %v, want context.Canceled", err)
	}
}

func TestListFilesAndDeleteOrphanOnlyRemoveStoreOwnedSnapshots(t *testing.T) {
	root := filepath.Join(t.TempDir(), "images")
	store := newTestStore(t, root, 128)
	object, err := store.Put(context.Background(), []byte("synthetic orphan image"), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	temporaryName := ".upload-0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(filepath.Join(root, temporaryName), []byte("temporary"), privateFileMode); err != nil {
		t.Fatal(err)
	}
	files, err := store.ListFiles(context.Background())
	if err != nil || len(files) != 2 {
		t.Fatalf("ListFiles() = (%d,%v), want image and upload temporary", len(files), err)
	}
	var imageFile, temporaryFile File
	for _, file := range files {
		if file.Temporary {
			temporaryFile = file
		} else {
			imageFile = file
		}
	}
	if imageFile.Key != object.Key || imageFile.SHA256 != object.SHA256 || !temporaryFile.Temporary {
		t.Fatalf("ListFiles() returned mismatched store snapshots: %#v", files)
	}
	if err := store.DeleteOrphan(context.Background(), temporaryFile); err != nil {
		t.Fatalf("DeleteOrphan(temp): %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, temporaryName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary file still exists: %v", err)
	}
	if err := store.DeleteOrphan(context.Background(), imageFile); err != nil {
		t.Fatalf("DeleteOrphan(image): %v", err)
	}
	if _, err := store.Open(context.Background(), object.Key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("orphan image still exists: %v", err)
	}
}

func TestOrphanListingFailsClosedOnUnknownEntriesAndDeleteChecksSnapshot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "images")
	store := newTestStore(t, root, 128)
	object, err := store.Put(context.Background(), []byte("synthetic orphan image"), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	files, err := store.ListFiles(context.Background())
	if err != nil || len(files) != 1 {
		t.Fatalf("ListFiles() = (%d,%v), want one", len(files), err)
	}
	file := files[0]
	path := filepath.Join(root, object.Key)
	if err := os.WriteFile(filepath.Join(root, "unrelated.txt"), []byte("leave me"), privateFileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListFiles(context.Background()); !errors.Is(err, ErrInvalidObject) {
		t.Fatal("ListFiles() accepted an unrelated root entry")
	}
	if err := os.Remove(filepath.Join(root, "unrelated.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("same-size replacement"), privateFileMode); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteOrphan(context.Background(), file); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("DeleteOrphan(mutated snapshot) = %v, want ErrInvalidObject", err)
	}
	if _, err := store.Open(context.Background(), object.Key); err != nil {
		t.Fatalf("mutated object was deleted: %v", err)
	}
}

func newTestStore(t *testing.T, root string, maxBytes int64) *Store {
	return newTestStoreWithQuota(t, root, maxBytes, 0)
}

func newTestStoreWithQuota(t *testing.T, root string, maxBytes, maxTotalBytes int64) *Store {
	t.Helper()
	store, err := New(Config{Root: root, MaxBytes: maxBytes, MaxTotalBytes: maxTotalBytes})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close(): %v", err)
		}
	})
	return store
}
