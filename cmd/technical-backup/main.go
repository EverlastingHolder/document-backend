// Command technical-backup creates and restores encrypted, data-minimized
// PostgreSQL tombstone backups. It does not archive image files or SQL dumps.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"passport-backend/internal/jobstore/postgres"
	"passport-backend/internal/technicalbackup"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "technical backup operation failed")
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("invalid command")
	}
	switch args[0] {
	case "keygen":
		return runKeygen(args[1:])
	case "create":
		return runCreate(args[1:])
	case "restore":
		return runRestore(args[1:])
	default:
		return errors.New("invalid command")
	}
}

func runKeygen(args []string) error {
	flags := flag.NewFlagSet("keygen", flag.ContinueOnError)
	output := flags.String("output", "", "absolute path for a new private 32-byte key file")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *output == "" {
		return errors.New("invalid keygen arguments")
	}
	key, err := technicalbackup.GenerateKey()
	if err != nil {
		return err
	}
	defer clear(key)
	return technicalbackup.WritePrivateNew(*output, key)
}

func runCreate(args []string) error {
	flags := flag.NewFlagSet("create", flag.ContinueOnError)
	dsnPath := flags.String("dsn-file", "", "absolute path to a private PostgreSQL DSN file")
	keyPath := flags.String("key-file", "", "absolute path to the private 32-byte key file")
	output := flags.String("output", "", "absolute path for a new encrypted backup file")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *dsnPath == "" || *keyPath == "" || *output == "" {
		return errors.New("invalid create arguments")
	}
	dsn, key, err := loadConnectionAndKey(*dsnPath, *keyPath)
	if err != nil {
		return err
	}
	defer clear(dsn)
	defer clear(key)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	store, err := openStore(ctx, string(dsn))
	if err != nil {
		return err
	}
	defer store.Close()
	snapshot, err := store.ExportTechnicalBackup(ctx)
	if err != nil {
		return err
	}
	archive, err := technicalbackup.Encrypt(snapshot, key)
	if err != nil {
		return err
	}
	if err := technicalbackup.WritePrivateNew(*output, archive); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "technical backup created")
	return nil
}

func runRestore(args []string) error {
	flags := flag.NewFlagSet("restore", flag.ContinueOnError)
	dsnPath := flags.String("dsn-file", "", "absolute path to a private PostgreSQL DSN file")
	keyPath := flags.String("key-file", "", "absolute path to the private 32-byte key file")
	input := flags.String("input", "", "absolute path to an encrypted private backup file")
	confirmEmpty := flags.Bool("confirm-empty-target", false, "confirm target is the intended empty migrated database")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *dsnPath == "" || *keyPath == "" || *input == "" || !*confirmEmpty {
		return errors.New("invalid restore arguments")
	}
	dsn, key, err := loadConnectionAndKey(*dsnPath, *keyPath)
	if err != nil {
		return err
	}
	defer clear(dsn)
	defer clear(key)
	archive, err := technicalbackup.ReadPrivate(*input, technicalbackup.MaxArchiveSize)
	if err != nil {
		return err
	}
	defer clear(archive)
	snapshot, err := technicalbackup.Decrypt(archive, key)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	store, err := openStore(ctx, string(dsn))
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.RestoreTechnicalBackup(ctx, snapshot); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "technical backup restored as non-processable tombstones")
	return nil
}

func loadConnectionAndKey(dsnPath, keyPath string) ([]byte, []byte, error) {
	dsn, err := technicalbackup.ReadPrivate(dsnPath, 64<<10)
	if err != nil || strings.TrimSpace(string(dsn)) == "" {
		clear(dsn)
		return nil, nil, errors.New("invalid private database configuration")
	}
	key, err := technicalbackup.ReadKey(keyPath)
	if err != nil {
		clear(dsn)
		return nil, nil, errors.New("invalid private encryption key")
	}
	return dsn, key, nil
}

func openStore(ctx context.Context, dsn string) (*postgres.Store, error) {
	store, err := postgres.Open(ctx, postgres.Config{DSN: dsn, ConnectTimeout: 10 * time.Second, MigrationLockTimeout: 10 * time.Second})
	if err != nil {
		return nil, errors.New("database connection failed")
	}
	if err := store.CheckSchema(ctx); err != nil {
		store.Close()
		return nil, errors.New("database schema is not ready")
	}
	return store, nil
}
