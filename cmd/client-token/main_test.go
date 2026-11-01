package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"passport-backend/internal/clientauth"
)

func TestRunWritesRawTokenOnlyToPrivateFile(t *testing.T) {
	privateDir := filepath.Join(t.TempDir(), "credentials")
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := run([]string{"-private-dir", privateDir}, &stdout); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if strings.Contains(stdout.String(), "pbt1_") {
		t.Fatalf("stdout exposed raw token: %q", stdout.String())
	}
	entries, err := os.ReadDir(privateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("private artifacts = %d, want token and registry: %v", len(entries), entries)
	}
	var tokenFile, registryFile string
	for _, entry := range entries {
		path := filepath.Join(privateDir, entry.Name())
		info, err := entry.Info()
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("artifact %q mode=%v err=%v, want 0600", entry.Name(), info.Mode(), err)
		}
		if strings.HasSuffix(entry.Name(), ".token") {
			tokenFile = path
		} else if strings.HasSuffix(entry.Name(), ".registry.json") {
			registryFile = path
		}
	}
	rawToken, err := os.ReadFile(tokenFile)
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(rawToken)), "pbt1_") {
		t.Fatalf("raw token artifact invalid: err=%v", err)
	}
	registryBytes, err := os.ReadFile(registryFile)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(registryBytes, bytes.TrimSpace(rawToken)) {
		t.Fatal("registry artifact contains raw token")
	}
	var registry credentialFile
	if err := json.Unmarshal(registryBytes, &registry); err != nil || len(registry.Credentials) != 1 {
		t.Fatalf("registry artifact invalid: err=%v", err)
	}
	loaded, err := clientauth.Load(registryFile)
	if err != nil {
		t.Fatalf("generated registry cannot be loaded by API: %v", err)
	}
	principal, err := loaded.Authenticate([]string{"Bearer " + strings.TrimSpace(string(rawToken))})
	if err != nil || principal.OwnerID != registry.Credentials[0].OwnerID {
		t.Fatalf("generated token mapping = %+v, %v", principal, err)
	}
}

func TestRunCanGenerateAdditionalTokenForExistingOwner(t *testing.T) {
	privateDir := filepath.Join(t.TempDir(), "credentials")
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ownerID := "8d2fae9c-efb7-4e98-8a90-f10b00edbdf2"
	if err := run([]string{"-private-dir", privateDir, "-owner-id", ownerID}, &bytes.Buffer{}); err != nil {
		t.Fatalf("run(rotation) error = %v", err)
	}
	entries, err := os.ReadDir(privateDir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("rotation artifacts = %d, err=%v", len(entries), err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".registry.json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(privateDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var registry credentialFile
		if err := json.Unmarshal(data, &registry); err != nil {
			t.Fatal(err)
		}
		if len(registry.Credentials) != 1 || registry.Credentials[0].OwnerID != ownerID {
			t.Fatalf("registry = %+v, want requested owner", registry)
		}
	}
}

func TestRunCanGenerateSeparateWorkerCredential(t *testing.T) {
	privateDir := filepath.Join(t.TempDir(), "credentials")
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := run([]string{"-private-dir", privateDir, "-purpose", "worker"}, &stdout); err != nil {
		t.Fatalf("run(worker) error=%v", err)
	}
	if strings.Contains(stdout.String(), "pwt1_") {
		t.Fatalf("stdout exposed worker token: %q", stdout.String())
	}
	entries, err := os.ReadDir(privateDir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("worker artifacts=%d err=%v", len(entries), err)
	}
	var tokenPath, registryPath string
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "passport-worker-") {
			t.Fatalf("worker artifact has wrong namespace name: %s", entry.Name())
		}
		if strings.HasSuffix(entry.Name(), ".token") {
			tokenPath = filepath.Join(privateDir, entry.Name())
		}
		if strings.HasSuffix(entry.Name(), ".registry.json") {
			registryPath = filepath.Join(privateDir, entry.Name())
		}
	}
	rawToken, err := os.ReadFile(tokenPath)
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(rawToken)), "pwt1_") {
		t.Fatalf("worker raw token artifact invalid: %v", err)
	}
	registry, err := clientauth.LoadWorker(registryPath)
	if err != nil {
		t.Fatalf("generated worker registry cannot be loaded: %v", err)
	}
	principal, err := registry.AuthenticateWorker([]string{"Bearer " + strings.TrimSpace(string(rawToken))})
	if err != nil || principal.WorkerID == "" {
		t.Fatalf("worker token mapping=(%+v,%v)", principal, err)
	}
	if _, err := clientauth.Load(registryPath); err == nil {
		t.Fatal("client registry loader accepted worker credential artifact")
	}
}

func TestRunRejectsUnsafeOutputDirectories(t *testing.T) {
	var stdout bytes.Buffer
	publicDir := filepath.Join(t.TempDir(), "public")
	if err := os.Mkdir(publicDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-private-dir", publicDir}, &stdout); err == nil {
		t.Fatal("run() accepted public output directory")
	}
	if stdout.Len() != 0 {
		t.Fatalf("invalid invocation wrote stdout: %q", stdout.String())
	}
	if err := run([]string{"-private-dir", "."}, &stdout); err == nil {
		t.Fatal("run() accepted repository directory")
	}
}

func TestValidatePrivateDirRejectsSymlinkAliasIntoRepository(t *testing.T) {
	repoDir, err := os.MkdirTemp(".", ".client-token-private-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(repoDir)
	privateTarget := filepath.Join(repoDir, "private")
	if err := os.Mkdir(privateTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "outside-looking-link")
	if err := os.Symlink(privateTarget, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := validatePrivateDir(alias); err == nil {
		t.Fatal("validatePrivateDir accepted an outside symlink resolving inside the repository")
	}
}
