// Command client-admin provides private operator controls for enrolled clients.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"passport-backend/internal/clientauth"
	"passport-backend/internal/jobstore/postgres"
)

const maxDSNBytes = 16 << 10

type clientRevoker interface {
	CheckSchema(context.Context) error
	RevokeClientInstallation(context.Context, string) (bool, error)
	RevokeClientTokenHash(context.Context, [32]byte, string) (bool, error)
	Close()
}

type storeOpener func(context.Context, string, time.Duration, time.Duration) (clientRevoker, error)

func main() {
	if code := mainExit(context.Background(), os.Args[1:], os.Stdout, os.Stderr, openStore); code != 0 {
		os.Exit(code)
	}
}

func mainExit(ctx context.Context, args []string, stdout, stderr io.Writer, open storeOpener) int {
	if ctx == nil || stdout == nil || stderr == nil || open == nil || len(args) == 0 {
		writeFailure(stderr)
		return 2
	}
	if args[0] == "revoke-hash" {
		return revokeHash(ctx, args[1:], stdout, stderr, open)
	}
	if args[0] != "revoke" {
		writeFailure(stderr)
		return 2
	}
	flags := flag.NewFlagSet("client-admin revoke", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dsnFile := flags.String("dsn-file", "", "private file containing PostgreSQL connection string")
	clientID := flags.String("client-id", "", "installation UUID to revoke")
	connectTimeout := flags.Duration("connect-timeout", 0, "maximum time to connect and ping PostgreSQL")
	lockTimeout := flags.Duration("migration-lock-timeout", 0, "required PostgreSQL store setting")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *dsnFile == "" || *clientID == "" ||
		*connectTimeout <= 0 || *lockTimeout <= 0 {
		writeFailure(stderr)
		return 2
	}
	dsn, err := readPrivateDSN(*dsnFile)
	if err != nil {
		writeFailure(stderr)
		return 1
	}
	store, err := open(ctx, dsn, *connectTimeout, *lockTimeout)
	if err != nil || store == nil {
		writeFailure(stderr)
		return 1
	}
	defer store.Close()
	operationCtx, cancel := context.WithTimeout(ctx, *connectTimeout)
	defer cancel()
	if err := store.CheckSchema(operationCtx); err != nil {
		writeFailure(stderr)
		return 1
	}
	if _, err := store.RevokeClientInstallation(operationCtx, *clientID); err != nil {
		writeFailure(stderr)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, "client installation is inactive")
	return 0
}

func revokeHash(ctx context.Context, args []string, stdout, stderr io.Writer, open storeOpener) int {
	flags := flag.NewFlagSet("client-admin revoke-hash", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dsnFile := flags.String("dsn-file", "", "private file containing PostgreSQL connection string")
	hashFile := flags.String("token-hash-file", "", "private file containing a lowercase SHA-256 token hash")
	connectTimeout := flags.Duration("connect-timeout", 0, "maximum time to connect and ping PostgreSQL")
	lockTimeout := flags.Duration("migration-lock-timeout", 0, "required PostgreSQL store setting")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *dsnFile == "" || *hashFile == "" ||
		*connectTimeout <= 0 || *lockTimeout <= 0 {
		writeFailure(stderr)
		return 2
	}
	tokenHash, err := readPrivateTokenHash(*hashFile)
	if err != nil {
		writeFailure(stderr)
		return 1
	}
	dsn, err := readPrivateDSN(*dsnFile)
	if err != nil {
		writeFailure(stderr)
		return 1
	}
	store, err := open(ctx, dsn, *connectTimeout, *lockTimeout)
	if err != nil || store == nil {
		writeFailure(stderr)
		return 1
	}
	defer store.Close()
	operationCtx, cancel := context.WithTimeout(ctx, *connectTimeout)
	defer cancel()
	if err := store.CheckSchema(operationCtx); err != nil {
		writeFailure(stderr)
		return 1
	}
	candidateID, err := clientauth.NewOwnerID()
	if err != nil {
		writeFailure(stderr)
		return 1
	}
	if _, err := store.RevokeClientTokenHash(operationCtx, tokenHash, candidateID); err != nil {
		writeFailure(stderr)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, "client credential is inactive")
	return 0
}

func openStore(ctx context.Context, dsn string, connectTimeout, lockTimeout time.Duration) (clientRevoker, error) {
	return postgres.Open(ctx, postgres.Config{DSN: dsn, ConnectTimeout: connectTimeout, MigrationLockTimeout: lockTimeout})
}

func readPrivateDSN(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", errors.New("database connection secret unavailable")
	}
	absolute = filepath.Clean(absolute)
	parent := filepath.Dir(absolute)
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", errors.New("database connection secret unavailable")
	}
	info, err := os.Lstat(resolvedParent)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return "", errors.New("database connection secret unavailable")
	}
	root, err := os.OpenRoot(resolvedParent)
	if err != nil {
		return "", errors.New("database connection secret unavailable")
	}
	defer root.Close()
	name := filepath.Base(absolute)
	before, err := root.Lstat(name)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || before.Mode().Perm() != 0o600 || before.Size() <= 0 || before.Size() > maxDSNBytes {
		return "", errors.New("database connection secret unavailable")
	}
	file, err := root.Open(name)
	if err != nil {
		return "", errors.New("database connection secret unavailable")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Mode().Perm() != 0o600 || !os.SameFile(before, opened) {
		return "", errors.New("database connection secret unavailable")
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxDSNBytes+1))
	if err != nil || len(contents) == 0 || len(contents) > maxDSNBytes {
		return "", errors.New("database connection secret unavailable")
	}
	dsn := strings.TrimSpace(string(contents))
	if dsn == "" {
		return "", errors.New("database connection secret unavailable")
	}
	return dsn, nil
}

func readPrivateTokenHash(path string) ([32]byte, error) {
	var zero [32]byte
	absolute, err := filepath.Abs(path)
	if err != nil {
		return zero, errors.New("credential hash unavailable")
	}
	absolute = filepath.Clean(absolute)
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return zero, errors.New("credential hash unavailable")
	}
	info, err := os.Lstat(parent)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return zero, errors.New("credential hash unavailable")
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return zero, errors.New("credential hash unavailable")
	}
	defer root.Close()
	name := filepath.Base(absolute)
	before, err := root.Lstat(name)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || before.Mode().Perm() != 0o600 || before.Size() != 64 {
		return zero, errors.New("credential hash unavailable")
	}
	file, err := root.Open(name)
	if err != nil {
		return zero, errors.New("credential hash unavailable")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Mode().Perm() != 0o600 || !os.SameFile(before, opened) {
		return zero, errors.New("credential hash unavailable")
	}
	contents, err := io.ReadAll(io.LimitReader(file, 65))
	if err != nil || len(contents) != 64 {
		return zero, errors.New("credential hash unavailable")
	}
	hash, err := clientauth.ParseTokenHash(string(contents))
	if err != nil {
		return zero, errors.New("credential hash unavailable")
	}
	return hash, nil
}

func writeFailure(stderr io.Writer) {
	_, _ = fmt.Fprintln(stderr, "client administration failed")
}
