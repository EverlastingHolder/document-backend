package postgres

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"passport-backend/internal/clientauth"
	"passport-backend/internal/clientenrollment"
)

const clientRegistrationLockID int64 = 0x50415353434C494E

var clientIDv4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// LookupClientOwner resolves only active database-enrolled client hashes.
// Static file credentials are checked before this repository by clientauth.
func (s *Store) LookupClientOwner(ctx context.Context, tokenHash [32]byte) (string, error) {
	if s == nil || s.pool == nil || ctx == nil || tokenHash == ([32]byte{}) {
		return "", clientauth.ErrUnauthorized
	}
	var clientID string
	err := s.pool.QueryRow(ctx, `
		SELECT client_id::text
		FROM client_installations
		WHERE token_sha256=$1 AND revoked_at IS NULL`, tokenHash[:]).Scan(&clientID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", clientauth.ErrUnauthorized
	}
	if err != nil {
		return "", storageError(ctx, err)
	}
	if !clientIDv4Pattern.MatchString(clientID) {
		return "", clientauth.ErrAuthenticationUnavailable
	}
	return clientID, nil
}

// LookupClientCredentialState returns whether a hash has a dynamic enrollment
// row and whether that row is revoked. Static client credentials remain valid
// when no row exists, but a database revocation must override a stale static
// registry entry.
func (s *Store) LookupClientCredentialState(ctx context.Context, tokenHash [32]byte) (string, bool, bool, error) {
	if s == nil || s.pool == nil || ctx == nil || tokenHash == ([32]byte{}) {
		return "", false, false, clientauth.ErrAuthenticationUnavailable
	}
	var clientID string
	var revoked bool
	err := s.pool.QueryRow(ctx, `
		SELECT client_id::text, revoked_at IS NOT NULL
		FROM client_installations
		WHERE token_sha256=$1`, tokenHash[:]).Scan(&clientID, &revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, false, nil
	}
	if err != nil {
		return "", false, false, storageError(ctx, err)
	}
	if !clientIDv4Pattern.MatchString(clientID) {
		return "", false, false, clientauth.ErrAuthenticationUnavailable
	}
	return clientID, true, revoked, nil
}

// RegisterClientToken serializes admission across API instances. Existing
// hashes are replayed before checking either cap; revoked hashes are never
// reactivated and unknown hashes are inserted only after both limits pass.
func (s *Store) RegisterClientToken(
	ctx context.Context,
	tokenHash [32]byte,
	candidateID string,
	maxClients int,
	maxNewPerMinute int,
) (clientenrollment.Installation, bool, error) {
	if s == nil || s.pool == nil || ctx == nil || tokenHash == ([32]byte{}) ||
		!clientIDv4Pattern.MatchString(candidateID) || maxClients <= 0 || maxNewPerMinute <= 0 {
		return clientenrollment.Installation{}, false, clientenrollment.ErrInvalidConfig
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return clientenrollment.Installation{}, false, storageError(ctx, err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", clientRegistrationLockID); err != nil {
		return clientenrollment.Installation{}, false, storageError(ctx, err)
	}

	var installation clientenrollment.Installation
	var revokedAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT client_id::text, created_at, revoked_at
		FROM client_installations
		WHERE token_sha256=$1`, tokenHash[:]).Scan(&installation.ClientID, &installation.CreatedAt, &revokedAt)
	if err == nil {
		if revokedAt != nil {
			return clientenrollment.Installation{}, false, clientenrollment.ErrRevokedCredential
		}
		if !clientIDv4Pattern.MatchString(installation.ClientID) {
			return clientenrollment.Installation{}, false, clientenrollment.ErrUnavailable
		}
		if err := tx.Commit(ctx); err != nil {
			return clientenrollment.Installation{}, false, storageError(ctx, err)
		}
		return installation, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return clientenrollment.Installation{}, false, storageError(ctx, err)
	}

	var activeClients, recentRegistrations int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE revoked_at IS NULL)::int,
		       count(*) FILTER (WHERE created_at >= clock_timestamp() - interval '1 minute')::int
		FROM client_installations`).Scan(&activeClients, &recentRegistrations); err != nil {
		return clientenrollment.Installation{}, false, storageError(ctx, err)
	}
	if activeClients >= maxClients || recentRegistrations >= maxNewPerMinute {
		return clientenrollment.Installation{}, false, clientenrollment.ErrRateLimited
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO client_installations (client_id, token_sha256)
		VALUES ($1, $2)
		RETURNING created_at`, candidateID, tokenHash[:]).Scan(&installation.CreatedAt); err != nil {
		return clientenrollment.Installation{}, false, storageError(ctx, err)
	}
	installation.ClientID = candidateID
	if err := tx.Commit(ctx); err != nil {
		return clientenrollment.Installation{}, false, storageError(ctx, err)
	}
	return installation, false, nil
}

// RevokeClientInstallation disables an enrolled installation without deleting
// its hash tombstone or any technical job-idempotency rows.
func (s *Store) RevokeClientInstallation(ctx context.Context, clientID string) (bool, error) {
	if s == nil || s.pool == nil || ctx == nil || !clientIDv4Pattern.MatchString(clientID) {
		return false, clientenrollment.ErrInvalidConfig
	}
	result, err := s.pool.Exec(ctx, `
		UPDATE client_installations SET revoked_at=clock_timestamp()
		WHERE client_id=$1 AND revoked_at IS NULL`, clientID)
	if err != nil {
		return false, storageError(ctx, err)
	}
	return result.RowsAffected() == 1, nil
}

// RevokeClientTokenHash records or revokes a hash while holding the same
// admission lock as registration. This supports persistent revocation of a
// legacy static-only credential before removing it from its file registry.
func (s *Store) RevokeClientTokenHash(ctx context.Context, tokenHash [32]byte, candidateID string) (bool, error) {
	if s == nil || s.pool == nil || ctx == nil || tokenHash == ([32]byte{}) || !clientIDv4Pattern.MatchString(candidateID) {
		return false, clientenrollment.ErrInvalidConfig
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, storageError(ctx, err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", clientRegistrationLockID); err != nil {
		return false, storageError(ctx, err)
	}

	var clientID string
	var revoked bool
	err = tx.QueryRow(ctx, `
		SELECT client_id::text, revoked_at IS NOT NULL
		FROM client_installations
		WHERE token_sha256=$1`, tokenHash[:]).Scan(&clientID, &revoked)
	if err == nil {
		if revoked {
			if err := tx.Commit(ctx); err != nil {
				return false, storageError(ctx, err)
			}
			return false, nil
		}
		if _, err := tx.Exec(ctx, `
			UPDATE client_installations SET revoked_at=clock_timestamp()
			WHERE client_id=$1 AND revoked_at IS NULL`, clientID); err != nil {
			return false, storageError(ctx, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return false, storageError(ctx, err)
		}
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, storageError(ctx, err)
	}
	if _, err := tx.Exec(ctx, `
		WITH stamp AS MATERIALIZED (SELECT clock_timestamp() AS at)
		INSERT INTO client_installations (client_id, token_sha256, created_at, revoked_at)
		SELECT $1, $2, stamp.at, stamp.at FROM stamp`, candidateID, tokenHash[:]); err != nil {
		return false, storageError(ctx, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, storageError(ctx, err)
	}
	return true, nil
}

var _ clientauth.ClientTokenLookup = (*Store)(nil)
var _ clientauth.ClientCredentialStateLookup = (*Store)(nil)
var _ clientenrollment.Store = (*Store)(nil)
