// Package postgres owns PostgreSQL connectivity and forward-only schema migrations.
package postgres

import (
	"context"
	"crypto/sha256"
	"embed"
	"errors"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalidConfig     = errors.New("invalid PostgreSQL configuration")
	ErrUnavailable       = errors.New("PostgreSQL unavailable")
	ErrMigrationFailed   = errors.New("PostgreSQL migration failed")
	ErrMigrationDrift    = errors.New("PostgreSQL migration drift detected")
	ErrSchemaNotReady    = errors.New("PostgreSQL schema not ready")
	ErrInvalidMigrations = errors.New("invalid embedded PostgreSQL migrations")
)

const migrationLockID int64 = 0x50415353504F5254

var migrationFilename = regexp.MustCompile(`^([0-9]{4})_([a-z0-9_]+)\.sql$`)

//go:embed migrations/*.sql
var migrationFS embed.FS

type Config struct {
	DSN                  string
	ConnectTimeout       time.Duration
	MigrationLockTimeout time.Duration
}

type Store struct {
	pool                 *pgxpool.Pool
	migrationLockTimeout time.Duration
}

type migration struct {
	version  int64
	name     string
	sql      []byte
	checksum [sha256.Size]byte
}

type appliedMigration struct {
	version  int64
	name     string
	checksum [sha256.Size]byte
}

// Open validates the private connection string, creates a small bounded pool,
// and confirms the database is reachable before returning.
func Open(ctx context.Context, config Config) (*Store, error) {
	if ctx == nil || strings.TrimSpace(config.DSN) == "" || config.ConnectTimeout <= 0 || config.MigrationLockTimeout <= 0 {
		return nil, ErrInvalidConfig
	}

	poolConfig, err := pgxpool.ParseConfig(strings.TrimSpace(config.DSN))
	if err != nil {
		return nil, ErrInvalidConfig
	}
	poolConfig.MaxConns = 2
	connectCtx, cancel := context.WithTimeout(ctx, config.ConnectTimeout)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(connectCtx, poolConfig)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrUnavailable
	}
	if err := pool.Ping(connectCtx); err != nil {
		pool.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrUnavailable
	}
	return &Store{pool: pool, migrationLockTimeout: config.MigrationLockTimeout}, nil
}

func (s *Store) Ping(ctx context.Context) error {
	if s == nil || s.pool == nil || ctx == nil {
		return ErrUnavailable
	}
	if err := s.pool.Ping(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrUnavailable
	}
	return nil
}

// Migrate checks applied checksums and applies all pending embedded migrations
// atomically while holding a transaction-scoped advisory lock.
func (s *Store) Migrate(ctx context.Context) error {
	if s == nil || s.pool == nil || ctx == nil || s.migrationLockTimeout <= 0 {
		return ErrInvalidConfig
	}
	migrations, err := loadMigrations(migrationFS)
	if err != nil {
		return err
	}

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrUnavailable
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrMigrationFailed
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	lockCtx, cancel := context.WithTimeout(ctx, s.migrationLockTimeout)
	_, err = tx.Exec(lockCtx, "SELECT pg_advisory_xact_lock($1)", migrationLockID)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrMigrationFailed
	}
	if _, err := tx.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version bigint PRIMARY KEY,
			name text NOT NULL,
			checksum bytea NOT NULL CHECK (octet_length(checksum) = 32),
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return migrationContextError(ctx)
	}

	applied, err := readAppliedMigrations(ctx, tx)
	if err != nil {
		return migrationContextError(ctx)
	}
	if err := validateAppliedMigrations(migrations, applied); err != nil {
		return err
	}
	appliedVersions := make(map[int64]struct{}, len(applied))
	for _, record := range applied {
		appliedVersions[record.version] = struct{}{}
	}
	for _, item := range migrations {
		if _, ok := appliedVersions[item.version]; ok {
			continue
		}
		if _, err := tx.Exec(ctx, string(item.sql)); err != nil {
			return migrationContextError(ctx)
		}
		if _, err := tx.Exec(ctx,
			"INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)",
			item.version, item.name, item.checksum[:],
		); err != nil {
			return migrationContextError(ctx)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return migrationContextError(ctx)
	}
	return nil
}

// CheckSchema verifies that all embedded migrations have already been applied
// and that the core job tables exist. It is read-only; runtime API startup must
// not silently perform schema changes.
func (s *Store) CheckSchema(ctx context.Context) error {
	if s == nil || s.pool == nil || ctx == nil {
		return ErrInvalidConfig
	}
	migrations, err := loadMigrations(migrationFS)
	if err != nil {
		return err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT version, name, checksum
		FROM schema_migrations
		ORDER BY version`)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrSchemaNotReady
	}
	applied := make([]appliedMigration, 0, len(migrations))
	for rows.Next() {
		var item appliedMigration
		var checksum []byte
		if err := rows.Scan(&item.version, &item.name, &checksum); err != nil {
			rows.Close()
			return ErrSchemaNotReady
		}
		if len(checksum) != sha256.Size {
			rows.Close()
			return ErrMigrationDrift
		}
		copy(item.checksum[:], checksum)
		applied = append(applied, item)
	}
	queryErr := rows.Err()
	rows.Close()
	if queryErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var postgresErr *pgconn.PgError
		if errors.As(queryErr, &postgresErr) && postgresErr.Code == "42P01" {
			return ErrSchemaNotReady
		}
		return ErrUnavailable
	}
	if err := validateAppliedMigrations(migrations, applied); err != nil {
		return err
	}
	if len(applied) != len(migrations) {
		return ErrSchemaNotReady
	}
	var coreTablesReady bool
	if err := s.pool.QueryRow(ctx, `
		SELECT to_regclass('jobs') IS NOT NULL
		   AND to_regclass('job_attempts') IS NOT NULL
		   AND to_regclass('outbox') IS NOT NULL`).Scan(&coreTablesReady); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrUnavailable
	}
	if !coreTablesReady {
		return ErrSchemaNotReady
	}
	return nil
}

func (s *Store) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}

func migrationContextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrMigrationFailed
}

func readAppliedMigrations(ctx context.Context, tx pgx.Tx) ([]appliedMigration, error) {
	rows, err := tx.Query(ctx, "SELECT version, name, checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []appliedMigration
	for rows.Next() {
		var item appliedMigration
		var checksum []byte
		if err := rows.Scan(&item.version, &item.name, &checksum); err != nil {
			return nil, err
		}
		if len(checksum) != sha256.Size {
			return nil, ErrMigrationDrift
		}
		copy(item.checksum[:], checksum)
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func loadMigrations(source fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(source, "migrations")
	if err != nil {
		return nil, ErrInvalidMigrations
	}
	items := make([]migration, 0, len(entries))
	seen := make(map[int64]struct{}, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil, ErrInvalidMigrations
		}
		matches := migrationFilename.FindStringSubmatch(entry.Name())
		if matches == nil {
			return nil, ErrInvalidMigrations
		}
		version, err := strconv.ParseInt(matches[1], 10, 64)
		if err != nil || version <= 0 {
			return nil, ErrInvalidMigrations
		}
		if _, exists := seen[version]; exists {
			return nil, ErrInvalidMigrations
		}
		seen[version] = struct{}{}
		path := "migrations/" + entry.Name()
		contents, err := fs.ReadFile(source, path)
		if err != nil || len(strings.TrimSpace(string(contents))) == 0 {
			return nil, ErrInvalidMigrations
		}
		items = append(items, migration{
			version:  version,
			name:     strings.TrimSuffix(entry.Name()[len(matches[1])+1:], ".sql"),
			sql:      contents,
			checksum: sha256.Sum256(contents),
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].version < items[j].version })
	return items, nil
}

func validateAppliedMigrations(migrations []migration, applied []appliedMigration) error {
	byVersion := make(map[int64]migration, len(migrations))
	for _, item := range migrations {
		byVersion[item.version] = item
	}
	seen := make(map[int64]struct{}, len(applied))
	for _, record := range applied {
		if _, exists := seen[record.version]; exists {
			return ErrMigrationDrift
		}
		seen[record.version] = struct{}{}
		item, exists := byVersion[record.version]
		if !exists || item.name != record.name || item.checksum != record.checksum {
			return ErrMigrationDrift
		}
	}
	return nil
}
