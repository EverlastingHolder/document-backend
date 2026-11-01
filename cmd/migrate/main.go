package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"passport-backend/internal/jobstore/postgres"
)

var ErrDSNUnavailable = errors.New("database connection secret unavailable")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.LookupEnv, os.ReadFile); err != nil {
		// Do not print the driver error or connection string; either may contain credentials.
		_, _ = fmt.Fprintln(os.Stderr, "database migration failed")
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, lookupEnv func(string) (string, bool), readFile func(string) ([]byte, error)) error {
	if ctx == nil || lookupEnv == nil || readFile == nil {
		return postgres.ErrInvalidConfig
	}
	flags := flag.NewFlagSet("migrate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dsnEnv := flags.String("dsn-env", "DATABASE_URL", "environment variable containing the PostgreSQL connection string")
	dsnFile := flags.String("dsn-file", "", "file containing the PostgreSQL connection string")
	connectTimeout := flags.Duration("connect-timeout", 0, "maximum time to connect and ping PostgreSQL")
	lockTimeout := flags.Duration("migration-lock-timeout", 0, "maximum time to wait for the schema migration lock")
	migrationTimeout := flags.Duration("migration-timeout", 0, "maximum total time for migrations")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return postgres.ErrInvalidConfig
	}
	dsnEnvExplicit := false
	flags.Visit(func(item *flag.Flag) {
		if item.Name == "dsn-env" {
			dsnEnvExplicit = true
		}
	})
	if *connectTimeout <= 0 || *lockTimeout <= 0 || *migrationTimeout <= 0 {
		return postgres.ErrInvalidConfig
	}
	if *dsnFile != "" && dsnEnvExplicit {
		return postgres.ErrInvalidConfig
	}
	dsn, err := readDSN(*dsnEnv, *dsnFile, lookupEnv, readFile)
	if err != nil {
		return err
	}
	store, err := postgres.Open(ctx, postgres.Config{
		DSN:                  dsn,
		ConnectTimeout:       *connectTimeout,
		MigrationLockTimeout: *lockTimeout,
	})
	if err != nil {
		return err
	}
	defer store.Close()

	migrationCtx, cancel := context.WithTimeout(ctx, *migrationTimeout)
	defer cancel()
	if err := store.Migrate(migrationCtx); err != nil {
		return err
	}
	return nil
}

func readDSN(envName, fileName string, lookupEnv func(string) (string, bool), readFile func(string) ([]byte, error)) (string, error) {
	if fileName != "" {
		contents, err := readFile(fileName)
		if err != nil {
			return "", ErrDSNUnavailable
		}
		dsn := strings.TrimSpace(string(contents))
		if dsn == "" {
			return "", ErrDSNUnavailable
		}
		return dsn, nil
	}
	if strings.TrimSpace(envName) == "" {
		return "", ErrDSNUnavailable
	}
	dsn, ok := lookupEnv(envName)
	dsn = strings.TrimSpace(dsn)
	if !ok || dsn == "" {
		return "", ErrDSNUnavailable
	}
	return dsn, nil
}
