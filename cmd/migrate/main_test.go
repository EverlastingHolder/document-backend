package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"passport-backend/internal/jobstore/postgres"
)

func TestReadDSNFromEnvironmentOrSecretFile(t *testing.T) {
	tests := []struct {
		name     string
		envName  string
		fileName string
		env      map[string]string
		files    map[string][]byte
		want     string
	}{
		{
			name:    "environment",
			envName: "DATABASE_URL",
			env:     map[string]string{"DATABASE_URL": " postgres://user:pass@db:5432/app "},
			want:    "postgres://user:pass@db:5432/app",
		},
		{
			name:     "secret file",
			envName:  "DATABASE_URL",
			fileName: "/run/secrets/database_url",
			files:    map[string][]byte{"/run/secrets/database_url": []byte("postgres://user:pass@db/app\n")},
			want:     "postgres://user:pass@db/app",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lookup := func(name string) (string, bool) {
				value, ok := test.env[name]
				return value, ok
			}
			read := func(path string) ([]byte, error) {
				value, ok := test.files[path]
				if !ok {
					return nil, errors.New("private read failure")
				}
				return value, nil
			}
			got, err := readDSN(test.envName, test.fileName, lookup, read)
			if err != nil || got != test.want {
				t.Fatalf("readDSN() = %q, %v; want %q, nil", got, err, test.want)
			}
		})
	}
}

func TestReadDSNDoesNotExposeSecretFileOrEnvironmentErrors(t *testing.T) {
	secret := "postgres://sensitive-user:private-password@db.invalid/passport"
	got, err := readDSN("DATABASE_URL", "", func(string) (string, bool) { return secret, true }, func(string) ([]byte, error) {
		return nil, nil
	})
	if err != nil || got != secret {
		t.Fatalf("readDSN() = %q, %v; want supplied DSN", got, err)
	}

	_, err = readDSN("DATABASE_URL", "/secret/path", func(string) (string, bool) { return "", false }, func(string) ([]byte, error) {
		return nil, errors.New("read failed: " + secret)
	})
	if !errors.Is(err, ErrDSNUnavailable) || strings.Contains(err.Error(), secret) {
		t.Fatalf("secret file error = %v, want redacted ErrDSNUnavailable", err)
	}
}

func TestRunRejectsAmbiguousSecretSourcesAndPositionalDSN(t *testing.T) {
	base := []string{
		"-connect-timeout", time.Second.String(),
		"-migration-lock-timeout", time.Second.String(),
		"-migration-timeout", time.Second.String(),
	}
	for _, args := range [][]string{
		append(append([]string{}, base...), "-dsn-env", "DATABASE_URL", "-dsn-file", "/run/secrets/db"),
		append(append([]string{}, base...), "postgres://private-secret@db/app"),
	} {
		err := run(context.Background(), args, func(string) (string, bool) {
			t.Fatal("environment read before argument validation")
			return "", false
		}, func(string) ([]byte, error) {
			t.Fatal("secret file read before argument validation")
			return nil, nil
		})
		if !errors.Is(err, postgres.ErrInvalidConfig) || strings.Contains(err.Error(), "private-secret") {
			t.Fatalf("run() error = %v, want redacted invalid configuration", err)
		}
	}
}
