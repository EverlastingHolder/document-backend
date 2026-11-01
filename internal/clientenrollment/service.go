// Package clientenrollment registers anonymous client installations by a
// hash-only proof of a client-generated high-entropy Bearer credential.
package clientenrollment

import (
	"context"
	"errors"
	"regexp"
	"time"

	"passport-backend/internal/clientauth"
)

var (
	ErrInvalidConfig     = errors.New("invalid client enrollment configuration")
	ErrInvalidHash       = errors.New("invalid client token hash")
	ErrStaticCredential  = errors.New("client token hash is reserved")
	ErrRevokedCredential = errors.New("client credential was revoked")
	ErrRateLimited       = errors.New("client registration rate limited")
	ErrUnavailable       = errors.New("client enrollment unavailable")
)

var clientIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type Installation struct {
	ClientID  string
	CreatedAt time.Time
}

type Store interface {
	RegisterClientToken(context.Context, [32]byte, string, int, int) (Installation, bool, error)
}

type Service struct {
	store        Store
	static       *clientauth.Registry
	maxClients   int
	maxPerMinute int
}

func New(store Store, static *clientauth.Registry, maxClients, maxPerMinute int) (*Service, error) {
	if store == nil || maxClients <= 0 || maxPerMinute <= 0 {
		return nil, ErrInvalidConfig
	}
	return &Service{store: store, static: static, maxClients: maxClients, maxPerMinute: maxPerMinute}, nil
}

func (s *Service) Register(ctx context.Context, tokenHash [32]byte) (Installation, bool, error) {
	if s == nil || s.store == nil || ctx == nil {
		return Installation{}, false, ErrInvalidConfig
	}
	if ctx.Err() != nil {
		return Installation{}, false, ctx.Err()
	}
	if tokenHash == ([32]byte{}) {
		return Installation{}, false, ErrInvalidHash
	}
	if s.static != nil && s.static.ContainsHash(tokenHash) {
		return Installation{}, false, ErrStaticCredential
	}
	clientID, err := clientauth.NewOwnerID()
	if err != nil {
		return Installation{}, false, ErrUnavailable
	}
	installation, replay, err := s.store.RegisterClientToken(ctx, tokenHash, clientID, s.maxClients, s.maxPerMinute)
	if err != nil {
		if ctx.Err() != nil {
			return Installation{}, false, ctx.Err()
		}
		switch {
		case errors.Is(err, ErrRateLimited), errors.Is(err, ErrRevokedCredential):
			return Installation{}, false, err
		default:
			return Installation{}, false, ErrUnavailable
		}
	}
	if !clientIDPattern.MatchString(installation.ClientID) || installation.CreatedAt.IsZero() {
		return Installation{}, false, ErrUnavailable
	}
	return installation, replay, nil
}
