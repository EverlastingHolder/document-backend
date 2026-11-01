package clientauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type clientTokenLookupStub struct {
	owner string
	err   error
	hash  [32]byte
}

type clientCredentialStateLookupStub struct {
	owner   string
	found   bool
	revoked bool
	err     error
	calls   int
}

func (s *clientCredentialStateLookupStub) LookupClientOwner(context.Context, [32]byte) (string, error) {
	return s.owner, s.err
}

func (s *clientCredentialStateLookupStub) LookupClientCredentialState(_ context.Context, _ [32]byte) (string, bool, bool, error) {
	s.calls++
	return s.owner, s.found, s.revoked, s.err
}

func (s *clientTokenLookupStub) LookupClientOwner(_ context.Context, hash [32]byte) (string, error) {
	s.hash = hash
	return s.owner, s.err
}

func TestAuthenticatorUsesStaticThenDynamicAndDistinguishesUnavailable(t *testing.T) {
	staticToken, _, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	staticOwner := "9cfe2e2f-135d-42f4-8db5-ddabc5a2f831"
	staticHash, _ := HashToken(staticToken)
	static := &Registry{principalIDByTokenHash: map[[32]byte]string{staticHash: staticOwner}, purpose: PurposeClient}
	dynamicToken, _, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	lookup := &clientTokenLookupStub{owner: "9cfe2e2f-135d-42f4-8db5-ddabc5a2f832"}
	auth, err := NewAuthenticator(static, lookup)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := auth.AuthenticateContext(context.Background(), []string{"Bearer " + staticToken})
	if err != nil || principal.OwnerID != staticOwner {
		t.Fatalf("static principal=%#v err=%v", principal, err)
	}
	if lookup.hash != ([32]byte{}) {
		t.Fatal("static credential unexpectedly queried dynamic store")
	}
	principal, err = auth.AuthenticateContext(context.Background(), []string{"Bearer " + dynamicToken})
	wantHash, _ := HashToken(dynamicToken)
	if err != nil || principal.OwnerID != lookup.owner || lookup.hash != wantHash {
		t.Fatalf("dynamic principal=%#v hashMatches=%t err=%v", principal, lookup.hash == wantHash, err)
	}
	lookup.err = ErrUnauthorized
	if _, err := auth.AuthenticateContext(context.Background(), []string{"Bearer " + dynamicToken}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unknown token err=%v, want unauthorized", err)
	}
	lookup.err = errors.New("private database detail")
	if _, err := auth.AuthenticateContext(context.Background(), []string{"Bearer " + dynamicToken}); !errors.Is(err, ErrAuthenticationUnavailable) || strings.Contains(err.Error(), "private database") {
		t.Fatalf("database error mapping=%v", err)
	}
}

func TestAuthenticatorStaticCredentialHonorsDatabaseRevocationWhenConfigured(t *testing.T) {
	token, _, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := HashToken(token)
	staticOwner := "9cfe2e2f-135d-42f4-8db5-ddabc5a2f831"
	static := &Registry{principalIDByTokenHash: map[[32]byte]string{hash: staticOwner}, purpose: PurposeClient}

	tests := []struct {
		name      string
		lookup    *clientCredentialStateLookupStub
		wantOwner string
		wantErr   error
	}{
		{
			name:      "unknown database hash preserves legacy static credential",
			lookup:    &clientCredentialStateLookupStub{},
			wantOwner: staticOwner,
		},
		{
			name:    "revoked database hash overrides static credential",
			lookup:  &clientCredentialStateLookupStub{owner: staticOwner, found: true, revoked: true},
			wantErr: ErrUnauthorized,
		},
		{
			name:    "different database owner fails closed",
			lookup:  &clientCredentialStateLookupStub{owner: "9cfe2e2f-135d-42f4-8db5-ddabc5a2f832", found: true},
			wantErr: ErrUnauthorized,
		},
		{
			name:    "database failure fails closed without leaking detail",
			lookup:  &clientCredentialStateLookupStub{err: errors.New("private database detail")},
			wantErr: ErrAuthenticationUnavailable,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			auth, err := NewAuthenticator(static, test.lookup)
			if err != nil {
				t.Fatal("construct authenticator")
			}
			principal, err := auth.AuthenticateContext(context.Background(), []string{"Bearer " + token})
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) || principal.OwnerID != "" {
					t.Fatalf("AuthenticateContext()=(%+v,%v), want empty principal and %v", principal, err, test.wantErr)
				}
			} else if err != nil || principal.OwnerID != test.wantOwner {
				t.Fatalf("AuthenticateContext()=(%+v,%v), want owner %q", principal, err, test.wantOwner)
			}
			if test.lookup.calls != 1 {
				t.Fatalf("database state lookup calls=%d, want 1", test.lookup.calls)
			}
			if strings.Contains(errString(err), "private database detail") {
				t.Fatal("authentication error leaked database detail")
			}
		})
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestNewTokenAndOwnerIDAreOpaqueAndValid(t *testing.T) {
	token, tokenHash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if !validToken(token) || strings.Contains(token, "=") || len(tokenHash) != 64 {
		t.Fatalf("generated token metadata is invalid: tokenLength=%d hashLength=%d", len(token), len(tokenHash))
	}
	digest := sha256.Sum256([]byte(token))
	if got := hex.EncodeToString(digest[:]); got != tokenHash {
		t.Fatalf("token hash = %q, want SHA-256 %q", tokenHash, got)
	}
	owner, err := NewOwnerID()
	if err != nil || !ownerIDPattern.MatchString(owner) {
		t.Fatalf("NewOwnerID() = %q, %v", owner, err)
	}
}

func TestLoadAuthenticatesTokenAndMapsStableOwner(t *testing.T) {
	root := t.TempDir()
	token, tokenHash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	owner := "8d2fae9c-efb7-4e98-8a90-f10b00edbdf2"
	path := writeRegistry(t, root, credentialFile{
		SchemaVersion: registrySchema,
		Credentials:   []credential{{OwnerID: owner, TokenSHA256: tokenHash}},
	})
	registry, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	principal, err := registry.Authenticate([]string{"Bearer " + token})
	if err != nil || principal.OwnerID != owner {
		t.Fatalf("Authenticate() = (%+v, %v), want owner %q", principal, err, owner)
	}
	principal, err = registry.Authenticate([]string{"bearer " + token})
	if err != nil || principal.OwnerID != owner {
		t.Fatalf("case-insensitive scheme Authenticate() = (%+v, %v)", principal, err)
	}
}

func TestAuthenticateRejectsMalformedUnknownAndRepeatedAuthorization(t *testing.T) {
	_, tokenHash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	path := writeRegistry(t, root, credentialFile{
		SchemaVersion: registrySchema,
		Credentials:   []credential{{OwnerID: "8d2fae9c-efb7-4e98-8a90-f10b00edbdf2", TokenSHA256: tokenHash}},
	})
	registry, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, headers := range [][]string{
		nil,
		{""},
		{"Basic abc"},
		{"Bearer"},
		{"Bearer token"},
		{"Bearer " + strings.Repeat("A", 43)},
		{"Bearer " + strings.Repeat("a", 43)},
		{"Bearer " + strings.Repeat("a", 43) + ", Bearer " + strings.Repeat("b", 43)},
		{"Bearer token1", "Bearer token2"},
	} {
		if _, err := registry.Authenticate(headers); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("Authenticate(%q) error = %v, want ErrUnauthorized", headers, err)
		}
	}
}

func TestLoadAllowsMultipleTokensForOneOwnerAndRejectsDuplicateHashes(t *testing.T) {
	root := t.TempDir()
	_, hash1, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	_, hash2, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	owner := "8d2fae9c-efb7-4e98-8a90-f10b00edbdf2"
	path := writeRegistry(t, root, credentialFile{
		SchemaVersion: registrySchema,
		Credentials: []credential{
			{OwnerID: owner, TokenSHA256: hash1},
			{OwnerID: owner, TokenSHA256: hash2},
		},
	})
	if _, err := Load(path); err != nil {
		t.Fatalf("Load(multiple tokens for one owner): %v", err)
	}
	path = writeRegistry(t, root, credentialFile{
		SchemaVersion: registrySchema,
		Credentials: []credential{
			{OwnerID: owner, TokenSHA256: hash1},
			{OwnerID: "f1fd0bc9-1bdf-433a-960b-55debcac2c17", TokenSHA256: hash1},
		},
	})
	if _, err := Load(path); !errors.Is(err, ErrInvalidRegistry) {
		t.Fatalf("Load(duplicate token hash) error = %v, want ErrInvalidRegistry", err)
	}
}

func TestWorkerRegistryUsesSeparateSchemaPrefixAndPrincipalType(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	token, tokenHash, err := NewWorkerToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, workerTokenPrefix) {
		t.Fatalf("worker token prefix=%q", token)
	}
	workerID := "8d2fae9c-efb7-4e98-8a90-f10b00edbdf2"
	data, err := json.Marshal(workerCredentialFile{
		SchemaVersion: workerRegistrySchema,
		Credentials:   []workerCredential{{WorkerID: workerID, TokenSHA256: tokenHash}},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "workers.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := LoadWorker(path)
	if err != nil {
		t.Fatalf("LoadWorker() error=%v", err)
	}
	principal, err := registry.AuthenticateWorker([]string{"Bearer " + token})
	if err != nil || principal.WorkerID != workerID {
		t.Fatalf("AuthenticateWorker()=(%+v,%v)", principal, err)
	}
	if _, err := registry.Authenticate([]string{"Bearer " + token}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("worker registry authenticated as a client: %v", err)
	}
	if _, err := Load(path); !errors.Is(err, ErrInvalidRegistry) {
		t.Fatalf("client loader accepted worker registry: %v", err)
	}

	clientToken, clientHash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	clientPath := writeRegistry(t, root, credentialFile{
		SchemaVersion: clientRegistrySchema,
		Credentials:   []credential{{OwnerID: workerID, TokenSHA256: clientHash}},
	})
	if _, err := LoadWorker(clientPath); !errors.Is(err, ErrInvalidRegistry) {
		t.Fatalf("worker loader accepted client registry: %v", err)
	}
	if _, err := registry.AuthenticateWorker([]string{"Bearer " + clientToken}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("worker registry authenticated a client token: %v", err)
	}
}

func TestLoadRejectsInvalidPrivateFilesAndRegistryJSON(t *testing.T) {
	root := t.TempDir()
	for name, contents := range map[string]string{
		"duplicate-key": `{"schema_version":"passport-client-credentials/v1","schema_version":"passport-client-credentials/v1","credentials":[]}`,
		"unknown-key":   `{"schema_version":"passport-client-credentials/v1","credentials":[],"token":"secret"}`,
		"trailing":      `{"schema_version":"passport-client-credentials/v1","credentials":[]} {}`,
		"wrong-schema":  `{"schema_version":"v2","credentials":[]}`,
		"empty-list":    `{"schema_version":"passport-client-credentials/v1","credentials":[]}`,
		"bad-owner":     `{"schema_version":"passport-client-credentials/v1","credentials":[{"owner_id":"alice","token_sha256":"` + strings.Repeat("a", 64) + `"}]}`,
	} {
		path := filepath.Join(root, name+".json")
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); !errors.Is(err, ErrInvalidRegistry) {
			t.Errorf("Load(%s) error = %v, want ErrInvalidRegistry", name, err)
		}
	}
	unsafeFile := filepath.Join(root, "unsafe.json")
	if err := os.WriteFile(unsafeFile, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(unsafeFile); !errors.Is(err, ErrInvalidRegistry) {
		t.Errorf("Load(public file) error = %v, want ErrInvalidRegistry", err)
	}
	unsafeParent := filepath.Join(t.TempDir(), "public")
	if err := os.Mkdir(unsafeParent, 0o755); err != nil {
		t.Fatal(err)
	}
	unsafePath := filepath.Join(unsafeParent, "registry.json")
	if err := os.WriteFile(unsafePath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(unsafePath); !errors.Is(err, ErrInvalidRegistry) {
		t.Errorf("Load(public parent) error = %v, want ErrInvalidRegistry", err)
	}
}

func TestLoadRejectsSymlinkedRegistry(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.json")
	if err := os.WriteFile(target, []byte(`{"schema_version":"passport-client-credentials/v1","credentials":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := Load(link); !errors.Is(err, ErrInvalidRegistry) {
		t.Fatalf("Load(symlink) error = %v, want ErrInvalidRegistry", err)
	}
}

func writeRegistry(t *testing.T, root string, value credentialFile) string {
	t.Helper()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	contents, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "credentials.json")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
