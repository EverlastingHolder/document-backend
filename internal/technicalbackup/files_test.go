package technicalbackup

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateFilesRequirePrivateDirectoryAndNoOverwrite(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "key.bin")
	if err := WritePrivateNew(path, []byte("private payload")); err != nil {
		t.Fatalf("WritePrivateNew() error = %v", err)
	}
	if err := WritePrivateNew(path, []byte("replacement")); !errors.Is(err, ErrPrivateFile) {
		t.Fatalf("WritePrivateNew() overwrite error = %v, want ErrPrivateFile", err)
	}
	data, err := ReadPrivate(path, 64)
	if err != nil || string(data) != "private payload" {
		t.Fatalf("ReadPrivate() = (%q,%v)", data, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("output mode = %v, err=%v; want 0600", info.Mode().Perm(), err)
	}
	if err := os.Chmod(directory, 0750); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivate(path, 64); !errors.Is(err, ErrPrivateFile) {
		t.Fatalf("ReadPrivate() with non-private parent = %v, want ErrPrivateFile", err)
	}
}

func TestPrivateFileRejectsSymlink(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	actual := filepath.Join(directory, "actual")
	if err := os.WriteFile(actual, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link")
	if err := os.Symlink(actual, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivate(link, 100); !errors.Is(err, ErrPrivateFile) {
		t.Fatalf("ReadPrivate() on symlink = %v, want ErrPrivateFile", err)
	}
}
