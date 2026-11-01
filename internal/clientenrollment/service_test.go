package clientenrollment

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"passport-backend/internal/clientauth"
)

type storeStub struct {
	installation Installation
	replay       bool
	err          error
	calls        int
	hash         [32]byte
}

func (s *storeStub) RegisterClientToken(_ context.Context, hash [32]byte, candidate string, maxClients, maxRate int) (Installation, bool, error) {
	s.calls++
	s.hash = hash
	if maxClients <= 0 || maxRate <= 0 || candidate == "" {
		return Installation{}, false, ErrInvalidConfig
	}
	return s.installation, s.replay, s.err
}

func TestServiceBlocksStaticHashesBeforeStore(t *testing.T) {
	const token = "pbt1_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	hash, err := clientauth.HashToken(token)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	registryPath := filepath.Join(dir, "registry.json")
	registryJSON := `{"schema_version":"passport-client-credentials/v1","credentials":[{"owner_id":"9cfe2e2f-135d-42f4-8db5-ddabc5a2f831","token_sha256":"` + hex.EncodeToString(hash[:]) + `"}]}`
	if err := os.WriteFile(registryPath, []byte(registryJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := clientauth.Load(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	store := &storeStub{}
	service, err := New(store, registry, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Register(context.Background(), hash); !errors.Is(err, ErrStaticCredential) {
		t.Fatalf("Register static hash error=%v", err)
	}
	if store.calls != 0 {
		t.Fatal("static hash reached dynamic registration store")
	}
}

func TestServicePassesHashAndReturnsConclusiveStoreOutcome(t *testing.T) {
	var hash [32]byte
	hash[0] = 1
	store := &storeStub{installation: Installation{ClientID: "123e4567-e89b-42d3-a456-426614174000", CreatedAt: time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)}, replay: true}
	service, err := New(store, nil, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	got, replay, err := service.Register(context.Background(), hash)
	if err != nil || !replay || got.ClientID != store.installation.ClientID || store.hash != hash {
		t.Fatalf("Register outcome=%#v replay=%t err=%v", got, replay, err)
	}
	store.err = ErrRateLimited
	if _, _, err := service.Register(context.Background(), hash); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("rate limit error=%v", err)
	}
	store.err = errors.New("private database detail")
	if _, _, err := service.Register(context.Background(), hash); !errors.Is(err, ErrUnavailable) || err.Error() != ErrUnavailable.Error() {
		t.Fatalf("storage error mapping=%v", err)
	}
	store.err = nil
	store.installation.ClientID = "malformed"
	if _, _, err := service.Register(context.Background(), hash); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("invalid stored identity error=%v", err)
	}
}
