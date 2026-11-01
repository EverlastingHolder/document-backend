package postgres

import (
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestLoadMigrationsOrdersAndChecksumsFiles(t *testing.T) {
	files := fstest.MapFS{
		"migrations/0002_attempts.sql": &fstest.MapFile{Data: []byte("CREATE TABLE attempts (id integer);")},
		"migrations/0001_jobs.sql":     &fstest.MapFile{Data: []byte("CREATE TABLE jobs (id integer);")},
	}

	got, err := loadMigrations(files)
	if err != nil {
		t.Fatalf("loadMigrations() error = %v", err)
	}
	if len(got) != 2 || got[0].version != 1 || got[0].name != "jobs" || got[1].version != 2 {
		t.Fatalf("migrations = %#v, want ordered versions 1 and 2", got)
	}
	want := sha256.Sum256(files["migrations/0001_jobs.sql"].Data)
	if got[0].checksum != want {
		t.Fatalf("first checksum = %x, want %x", got[0].checksum, want)
	}
}

func TestLoadEmbeddedMigrations(t *testing.T) {
	got, err := loadMigrations(migrationFS)
	if err != nil {
		t.Fatalf("loadMigrations(embedded) error = %v", err)
	}
	if len(got) != 9 {
		t.Fatalf("embedded migrations = %d, want 9", len(got))
	}
	for index, item := range got {
		if item.version != int64(index+1) {
			t.Fatalf("migration[%d].version = %d, want %d", index, item.version, index+1)
		}
	}
}

func TestLoadMigrationsRejectsInvalidOrDuplicateVersions(t *testing.T) {
	tests := []struct {
		name  string
		files fstest.MapFS
	}{
		{
			name: "invalid filename",
			files: fstest.MapFS{
				"migrations/one.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
			},
		},
		{
			name: "duplicate version",
			files: fstest.MapFS{
				"migrations/0001_jobs.sql":      &fstest.MapFile{Data: []byte("SELECT 1;")},
				"migrations/0001_more_jobs.sql": &fstest.MapFile{Data: []byte("SELECT 2;")},
			},
		},
		{
			name: "empty migration",
			files: fstest.MapFS{
				"migrations/0001_jobs.sql": &fstest.MapFile{Data: []byte("  \n")},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := loadMigrations(test.files); !errors.Is(err, ErrInvalidMigrations) {
				t.Fatalf("loadMigrations() error = %v, want ErrInvalidMigrations", err)
			}
		})
	}
}

func TestValidateAppliedMigrationsRejectsUnknownAndChangedFiles(t *testing.T) {
	migrations, err := loadMigrations(fstest.MapFS{
		"migrations/0001_jobs.sql": &fstest.MapFile{Data: []byte("CREATE TABLE jobs (id integer);")},
	})
	if err != nil {
		t.Fatalf("loadMigrations() error = %v", err)
	}

	tests := []struct {
		name    string
		applied []appliedMigration
	}{
		{
			name:    "unknown applied version",
			applied: []appliedMigration{{version: 7, name: "removed", checksum: sha256.Sum256([]byte("old"))}},
		},
		{
			name:    "changed checksum",
			applied: []appliedMigration{{version: 1, name: "jobs", checksum: sha256.Sum256([]byte("edited"))}},
		},
		{
			name:    "changed name",
			applied: []appliedMigration{{version: 1, name: "renamed", checksum: migrations[0].checksum}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateAppliedMigrations(migrations, test.applied); !errors.Is(err, ErrMigrationDrift) {
				t.Fatalf("validateAppliedMigrations() error = %v, want ErrMigrationDrift", err)
			}
		})
	}
}

func TestOpenDoesNotExposeConnectionStringWhenParsingFails(t *testing.T) {
	secretDSN := "private-secret-value://user:password@db.invalid/schema"
	_, err := Open(t.Context(), Config{
		DSN:                  secretDSN,
		ConnectTimeout:       time.Second,
		MigrationLockTimeout: time.Second,
	})
	if !errors.Is(err, ErrInvalidConfig) || strings.Contains(err.Error(), secretDSN) {
		t.Fatalf("Open() error = %v, want redacted ErrInvalidConfig", err)
	}
}
