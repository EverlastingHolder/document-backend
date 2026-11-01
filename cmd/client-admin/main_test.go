package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeRevoker struct {
	checkErr  error
	revokeErr error
	clientID  string
	tokenHash [32]byte
	candidate string
	closed    bool
}

func (f *fakeRevoker) CheckSchema(context.Context) error { return f.checkErr }
func (f *fakeRevoker) RevokeClientInstallation(_ context.Context, clientID string) (bool, error) {
	f.clientID = clientID
	return true, f.revokeErr
}
func (f *fakeRevoker) RevokeClientTokenHash(_ context.Context, tokenHash [32]byte, candidate string) (bool, error) {
	f.tokenHash = tokenHash
	f.candidate = candidate
	return true, f.revokeErr
}
func (f *fakeRevoker) Close() { f.closed = true }

func TestClientAdminRevokesWithoutEchoingIdentifierOrSecret(t *testing.T) {
	secret := "postgres://private-user:private-password@localhost/private-db"
	privateDir := t.TempDir()
	if err := os.Chmod(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dsnPath := filepath.Join(privateDir, "dsn")
	if err := os.WriteFile(dsnPath, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	clientID := "9aa239d6-a17b-40ce-8aca-a8d33c0d650e"
	store := &fakeRevoker{}
	var stdout, stderr bytes.Buffer
	opened := false
	code := mainExit(context.Background(), []string{
		"revoke", "-dsn-file", dsnPath, "-client-id", clientID,
		"-connect-timeout", "1s", "-migration-lock-timeout", "1s",
	}, &stdout, &stderr, func(_ context.Context, dsn string, connect, lock time.Duration) (clientRevoker, error) {
		opened = true
		if dsn != secret || connect != time.Second || lock != time.Second {
			t.Fatalf("unexpected opener arguments")
		}
		return store, nil
	})
	if code != 0 || !opened || !store.closed || store.clientID != clientID || strings.TrimSpace(stdout.String()) != "client installation is inactive" {
		t.Fatalf("code=%d opened=%v closed=%v client=%q stdout=%q stderr=%q", code, opened, store.closed, store.clientID, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), secret) || strings.Contains(stdout.String()+stderr.String(), clientID) {
		t.Fatal("CLI output disclosed a private value")
	}
}

func TestClientAdminCanPersistStaticHashRevocationWithoutEchoingHash(t *testing.T) {
	secret := "postgres://private-user:private-password@localhost/private-db"
	privateDir := t.TempDir()
	if err := os.Chmod(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dsnPath := filepath.Join(privateDir, "dsn")
	if err := os.WriteFile(dsnPath, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	hashFile := filepath.Join(privateDir, "token-hash")
	hashText := strings.Repeat("a1", 32)
	if err := os.WriteFile(hashFile, []byte(hashText), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &fakeRevoker{}
	var stdout, stderr bytes.Buffer
	code := mainExit(context.Background(), []string{
		"revoke-hash", "-dsn-file", dsnPath, "-token-hash-file", hashFile,
		"-connect-timeout", "1s", "-migration-lock-timeout", "1s",
	}, &stdout, &stderr, func(_ context.Context, dsn string, connect, lock time.Duration) (clientRevoker, error) {
		if dsn != secret || connect != time.Second || lock != time.Second {
			t.Fatalf("unexpected opener arguments")
		}
		return store, nil
	})
	if code != 0 || !store.closed || store.tokenHash == ([32]byte{}) || store.candidate == "" || strings.TrimSpace(stdout.String()) != "client credential is inactive" {
		t.Fatalf("code=%d closed=%v candidate=%q stdout=%q stderr=%q", code, store.closed, store.candidate, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), secret) || strings.Contains(stdout.String()+stderr.String(), hashText) {
		t.Fatal("CLI output disclosed a private value")
	}
}

func TestClientAdminRequiresPrivateDSNFile(t *testing.T) {
	sharedDir := t.TempDir()
	if err := os.Chmod(sharedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sharedDir, "dsn")
	if err := os.WriteFile(path, []byte("postgres://private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateDSN(path); err == nil {
		t.Fatal("readPrivateDSN accepted a non-private parent")
	}
}

func TestClientAdminRequiresPrivateHashFile(t *testing.T) {
	sharedDir := t.TempDir()
	if err := os.Chmod(sharedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sharedDir, "hash")
	if err := os.WriteFile(path, []byte(strings.Repeat("ab", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateTokenHash(path); err == nil {
		t.Fatal("readPrivateTokenHash accepted a non-private parent")
	}
}

func TestClientAdminUsesGenericFailureOutput(t *testing.T) {
	secret := "postgres://private-user:private-password@localhost/private-db"
	privateDir := t.TempDir()
	_ = os.Chmod(privateDir, 0o700)
	dsnPath := filepath.Join(privateDir, "dsn")
	_ = os.WriteFile(dsnPath, []byte(secret), 0o600)
	var stdout, stderr bytes.Buffer
	code := mainExit(context.Background(), []string{"revoke", "-dsn-file", dsnPath, "-client-id", "bad-id", "-connect-timeout", "1s", "-migration-lock-timeout", "1s"}, &stdout, &stderr,
		func(context.Context, string, time.Duration, time.Duration) (clientRevoker, error) {
			return nil, errors.New(secret)
		})
	if code != 1 || strings.Contains(stderr.String(), secret) || strings.Contains(stderr.String(), "bad-id") {
		t.Fatalf("code=%d output leaked or unexpected: %q", code, stderr.String())
	}
}
