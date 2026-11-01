package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunRejectsCommandsThatCouldDeleteUncoordinatedData(t *testing.T) {
	var output strings.Builder
	err := run(context.Background(), []string{"orphans"}, func(string) string { return "" }, &output)
	if err == nil || output.Len() != 0 {
		t.Fatalf("unsupported orphan command error/output = %v/%q", err, output.String())
	}
}

func TestRunRequiresStoppedAPIBeforeReadingAnyCredentials(t *testing.T) {
	var output strings.Builder
	environment := map[string]string{
		"PASSPORT_MAINTENANCE_DSN_FILE": "/does/not/exist",
		"PASSPORT_IMAGE_ROOT":           "/does/not/exist",
	}
	err := run(context.Background(), []string{"sweep"}, func(key string) string { return environment[key] }, &output)
	if err == nil || !strings.Contains(err.Error(), "API to be stopped") || output.Len() != 0 {
		t.Fatalf("run without offline guard = (%v,%q)", err, output.String())
	}
}

func TestReadPrivateFileRequiresPrivateRegularFileAndParent(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "dsn")
	if err := os.WriteFile(path, []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	if value, err := readPrivateFile(path); err != nil || value != "synthetic" {
		t.Fatalf("read private file = (%q,%v)", value, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateFile(path); err == nil {
		t.Fatal("readPrivateFile accepted a broadly readable secret")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateFile(path); err == nil {
		t.Fatal("readPrivateFile accepted a non-private parent")
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "dsn-link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateFile(link); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatal("readPrivateFile accepted or followed a secret symlink")
	}
}

func TestValidateExistingImageRootDoesNotCreateOrFollowSymlink(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "missing")
	if err := validateExistingImageRoot(missing); err == nil {
		t.Fatal("missing image root passed validation")
	}
	root := filepath.Join(base, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateExistingImageRoot(root); err != nil {
		t.Fatalf("private existing root rejected: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if err := validateExistingImageRoot(link); err == nil {
		t.Fatal("symlink image root passed validation")
	}
}
