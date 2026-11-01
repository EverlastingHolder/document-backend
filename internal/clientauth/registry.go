// Package clientauth resolves opaque static Bearer tokens to stable local
// owner IDs. It does not issue, persist, or log raw tokens.
package clientauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"passport-backend/internal/jsonstrict"
)

const (
	clientRegistrySchema = "passport-client-credentials/v1"
	workerRegistrySchema = "passport-worker-credentials/v1"
	clientTokenPrefix    = "pbt1_"
	workerTokenPrefix    = "pwt1_"
	maxFileBytes         = 64 << 10
	registrySchema       = clientRegistrySchema
)

var (
	ErrInvalidRegistry           = errors.New("invalid client credential registry")
	ErrUnauthorized              = errors.New("unauthorized client")
	ErrAuthenticationUnavailable = errors.New("client authentication unavailable")
	ownerIDPattern               = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	tokenHashPattern             = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type Principal struct {
	OwnerID string
}

type WorkerPrincipal struct {
	WorkerID string
}

type credentialFile struct {
	SchemaVersion string       `json:"schema_version"`
	Credentials   []credential `json:"credentials"`
}

type credential struct {
	OwnerID     string `json:"owner_id"`
	TokenSHA256 string `json:"token_sha256"`
}

type workerCredentialFile struct {
	SchemaVersion string             `json:"schema_version"`
	Credentials   []workerCredential `json:"credentials"`
}

type workerCredential struct {
	WorkerID    string `json:"worker_id"`
	TokenSHA256 string `json:"token_sha256"`
}

type Registry struct {
	principalIDByTokenHash map[[sha256.Size]byte]string
	purpose                Purpose
}

// ClientTokenLookup resolves a dynamic token hash to its stable client ID.
// Implementations must return ErrUnauthorized for absent or revoked hashes.
type ClientTokenLookup interface {
	LookupClientOwner(context.Context, [sha256.Size]byte) (string, error)
}

// ClientCredentialStateLookup distinguishes absent credentials from revoked
// database credentials. It is consulted before accepting a matching static
// registry entry, so a stale static entry cannot override an explicit revoke.
type ClientCredentialStateLookup interface {
	LookupClientCredentialState(context.Context, [sha256.Size]byte) (ownerID string, found bool, revoked bool, err error)
}

// Authenticator checks the optional static registry first, then falls back to
// the database-backed enrollment table. Raw credentials never leave this package.
type Authenticator struct {
	static  *Registry
	dynamic ClientTokenLookup
}

type Purpose uint8

const (
	PurposeClient Purpose = iota + 1
	PurposeWorker
)

// Load reads a bounded strict registry from a regular private file in a
// private direct parent directory. The registry stores only token hashes.
func Load(path string) (*Registry, error) {
	return load(path, PurposeClient)
}

// LoadWorker reads a worker-only hash registry. Client registries and client
// token prefixes are rejected by this purpose-specific loader.
func LoadWorker(path string) (*Registry, error) {
	return load(path, PurposeWorker)
}

func load(path string, purpose Purpose) (*Registry, error) {
	if strings.TrimSpace(path) == "" {
		return nil, ErrInvalidRegistry
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, ErrInvalidRegistry
	}
	absolute = filepath.Clean(absolute)
	parentPath := filepath.Dir(absolute)
	parentInfo, err := os.Lstat(parentPath)
	if err != nil || parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() || parentInfo.Mode().Perm()&0o077 != 0 {
		return nil, ErrInvalidRegistry
	}

	root, err := os.OpenRoot(parentPath)
	if err != nil {
		return nil, ErrInvalidRegistry
	}
	defer func() { _ = root.Close() }()
	rootInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(parentInfo, rootInfo) {
		return nil, ErrInvalidRegistry
	}
	name := filepath.Base(absolute)
	before, err := root.Lstat(name)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || before.Mode().Perm() != 0o600 || before.Size() > maxFileBytes {
		return nil, ErrInvalidRegistry
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, ErrInvalidRegistry
	}
	defer func() { _ = file.Close() }()
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || after.Mode().Perm() != 0o600 || !os.SameFile(before, after) || after.Size() > maxFileBytes {
		return nil, ErrInvalidRegistry
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxFileBytes+1))
	if err != nil || len(contents) > maxFileBytes {
		return nil, ErrInvalidRegistry
	}
	if err := jsonstrict.RejectDuplicateKeys(contents); err != nil {
		return nil, ErrInvalidRegistry
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var principals map[[sha256.Size]byte]string
	var schema string
	switch purpose {
	case PurposeClient:
		var parsed credentialFile
		if err := decoder.Decode(&parsed); err != nil {
			return nil, ErrInvalidRegistry
		}
		schema = parsed.SchemaVersion
		if len(parsed.Credentials) == 0 {
			return nil, ErrInvalidRegistry
		}
		principals = make(map[[sha256.Size]byte]string, len(parsed.Credentials))
		for _, item := range parsed.Credentials {
			if !ownerIDPattern.MatchString(item.OwnerID) || !tokenHashPattern.MatchString(item.TokenSHA256) || !addHash(principals, item.TokenSHA256, item.OwnerID) {
				return nil, ErrInvalidRegistry
			}
		}
	case PurposeWorker:
		var parsed workerCredentialFile
		if err := decoder.Decode(&parsed); err != nil {
			return nil, ErrInvalidRegistry
		}
		schema = parsed.SchemaVersion
		if len(parsed.Credentials) == 0 {
			return nil, ErrInvalidRegistry
		}
		principals = make(map[[sha256.Size]byte]string, len(parsed.Credentials))
		for _, item := range parsed.Credentials {
			if !ownerIDPattern.MatchString(item.WorkerID) || !tokenHashPattern.MatchString(item.TokenSHA256) || !addHash(principals, item.TokenSHA256, item.WorkerID) {
				return nil, ErrInvalidRegistry
			}
		}
	default:
		return nil, ErrInvalidRegistry
	}
	var trailing any
	wantSchema := clientRegistrySchema
	if purpose == PurposeWorker {
		wantSchema = workerRegistrySchema
	}
	if err := decoder.Decode(&trailing); err != io.EOF || schema != wantSchema {
		return nil, ErrInvalidRegistry
	}
	return &Registry{principalIDByTokenHash: principals, purpose: purpose}, nil
}

func addHash(principals map[[sha256.Size]byte]string, encoded, principalID string) bool {
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) != sha256.Size {
		return false
	}
	var tokenHash [sha256.Size]byte
	copy(tokenHash[:], decoded)
	if _, exists := principals[tokenHash]; exists {
		return false
	}
	principals[tokenHash] = principalID
	return true
}

// Authenticate maps exactly one Authorization header to its configured owner.
// Token material is never returned and invalid credentials share one error.
func (r *Registry) Authenticate(authorizationHeaders []string) (Principal, error) {
	if r == nil || r.purpose != PurposeClient {
		return Principal{}, ErrUnauthorized
	}
	principalID, err := r.authenticate(authorizationHeaders, clientTokenPrefix)
	if err != nil {
		return Principal{}, err
	}
	return Principal{OwnerID: principalID}, nil
}

// AuthenticateContext preserves the static registry behavior while satisfying
// request-scoped public API authentication. Worker auth continues to use its
// purpose-specific AuthenticateWorker method.
func (r *Registry) AuthenticateContext(ctx context.Context, authorizationHeaders []string) (Principal, error) {
	if ctx == nil || ctx.Err() != nil {
		if ctx == nil {
			return Principal{}, ErrUnauthorized
		}
		return Principal{}, ctx.Err()
	}
	return r.Authenticate(authorizationHeaders)
}

func NewAuthenticator(static *Registry, dynamic ClientTokenLookup) (*Authenticator, error) {
	if static != nil && static.purpose != PurposeClient || static == nil && dynamic == nil {
		return nil, ErrInvalidRegistry
	}
	return &Authenticator{static: static, dynamic: dynamic}, nil
}

func (a *Authenticator) AuthenticateContext(ctx context.Context, authorizationHeaders []string) (Principal, error) {
	if ctx == nil {
		return Principal{}, ErrUnauthorized
	}
	if err := ctx.Err(); err != nil {
		return Principal{}, err
	}
	if a == nil {
		return Principal{}, ErrUnauthorized
	}
	hash, err := bearerTokenHash(authorizationHeaders, clientTokenPrefix)
	if err != nil {
		return Principal{}, ErrUnauthorized
	}
	if a.static != nil {
		if principal, ok := a.static.principalForHash(hash); ok {
			if stateLookup, supported := a.dynamic.(ClientCredentialStateLookup); supported {
				ownerID, found, revoked, err := stateLookup.LookupClientCredentialState(ctx, hash)
				if err != nil {
					if ctx.Err() != nil {
						return Principal{}, ctx.Err()
					}
					return Principal{}, ErrAuthenticationUnavailable
				}
				if revoked {
					return Principal{}, ErrUnauthorized
				}
				if found && !ownerIDPattern.MatchString(ownerID) {
					return Principal{}, ErrAuthenticationUnavailable
				}
				if found && ownerID != principal.OwnerID {
					return Principal{}, ErrUnauthorized
				}
			}
			return principal, nil
		}
	}
	if a.dynamic == nil {
		return Principal{}, ErrUnauthorized
	}
	ownerID, err := a.dynamic.LookupClientOwner(ctx, hash)
	if err != nil {
		if ctx.Err() != nil {
			return Principal{}, ctx.Err()
		}
		if errors.Is(err, ErrUnauthorized) {
			return Principal{}, ErrUnauthorized
		}
		return Principal{}, ErrAuthenticationUnavailable
	}
	if !ownerIDPattern.MatchString(ownerID) {
		return Principal{}, ErrAuthenticationUnavailable
	}
	return Principal{OwnerID: ownerID}, nil
}

func (r *Registry) principalForHash(hash [sha256.Size]byte) (Principal, bool) {
	if r == nil || r.purpose != PurposeClient {
		return Principal{}, false
	}
	ownerID, ok := r.principalIDByTokenHash[hash]
	return Principal{OwnerID: ownerID}, ok
}

// ContainsHash reports whether a client hash is reserved in this static
// registry without returning or reconstructing any token material.
func (r *Registry) ContainsHash(hash [sha256.Size]byte) bool {
	_, ok := r.principalForHash(hash)
	return ok
}

// ParseTokenHash accepts only a canonical lower-case SHA-256 hex value.
func ParseTokenHash(value string) ([sha256.Size]byte, error) {
	var result [sha256.Size]byte
	if len(value) != sha256.Size*2 || !tokenHashPattern.MatchString(value) {
		return result, ErrUnauthorized
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return result, ErrUnauthorized
	}
	copy(result[:], decoded)
	return result, nil
}

func HashToken(raw string) ([sha256.Size]byte, error) {
	if !validToken(raw) {
		return [sha256.Size]byte{}, ErrUnauthorized
	}
	return sha256.Sum256([]byte(raw)), nil
}

// AuthenticateWorker maps a worker-only Bearer token to its stable worker ID.
func (r *Registry) AuthenticateWorker(authorizationHeaders []string) (WorkerPrincipal, error) {
	if r == nil || r.purpose != PurposeWorker {
		return WorkerPrincipal{}, ErrUnauthorized
	}
	principalID, err := r.authenticate(authorizationHeaders, workerTokenPrefix)
	if err != nil {
		return WorkerPrincipal{}, err
	}
	return WorkerPrincipal{WorkerID: principalID}, nil
}

func (r *Registry) authenticate(authorizationHeaders []string, prefix string) (string, error) {
	digest, err := bearerTokenHash(authorizationHeaders, prefix)
	if err != nil {
		return "", err
	}
	principalID, ok := r.principalIDByTokenHash[digest]
	if !ok {
		return "", ErrUnauthorized
	}
	return principalID, nil
}

func bearerTokenHash(authorizationHeaders []string, prefix string) ([sha256.Size]byte, error) {
	if len(authorizationHeaders) != 1 {
		return [sha256.Size]byte{}, ErrUnauthorized
	}
	header := authorizationHeaders[0]
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t\r\n,") || !validTokenFor(token, prefix) {
		return [sha256.Size]byte{}, ErrUnauthorized
	}
	return sha256.Sum256([]byte(token)), nil
}

// NewToken creates an opaque high-entropy Bearer token and its lowercase
// SHA-256 registry value. The caller is responsible for delivering the raw
// token through a private one-time channel.
func NewToken() (raw string, tokenSHA256 string, err error) {
	return newToken(clientTokenPrefix)
}

func NewWorkerToken() (raw string, tokenSHA256 string, err error) {
	return newToken(workerTokenPrefix)
}

func newToken(prefix string) (raw string, tokenSHA256 string, err error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", "", ErrInvalidRegistry
	}
	raw = prefix + base64.RawURLEncoding.EncodeToString(secret[:])
	digest := sha256.Sum256([]byte(raw))
	return raw, hex.EncodeToString(digest[:]), nil
}

// NewOwnerID creates a stable opaque UUIDv4 owner identifier.
func NewOwnerID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", ErrInvalidRegistry
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

func validToken(token string) bool { return validTokenFor(token, clientTokenPrefix) }

func validTokenFor(token, prefix string) bool {
	if len(token) != len(prefix)+43 || !strings.HasPrefix(token, prefix) {
		return false
	}
	encoded := strings.TrimPrefix(token, prefix)
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == encoded
}
