// Command job-maintenance performs one bounded privacy-retention sweep.
// It does not scan or delete unreferenced files while intake may be running.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"passport-backend/internal/imagestore"
	"passport-backend/internal/jobstore/postgres"
	"passport-backend/internal/retention"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv, os.Stdout); err != nil {
		_, _ = io.WriteString(os.Stderr, "job maintenance failed\n")
		os.Exit(1)
	}
}

func run(parent context.Context, args []string, getenv func(string) string, stdout io.Writer) error {
	if len(args) != 1 || args[0] != "sweep" {
		return errors.New("invalid command")
	}
	dsnFile := strings.TrimSpace(getenv("PASSPORT_MAINTENANCE_DSN_FILE"))
	imageRoot := strings.TrimSpace(getenv("PASSPORT_IMAGE_ROOT"))
	if dsnFile == "" || imageRoot == "" {
		return errors.New("required configuration missing")
	}
	if strings.TrimSpace(getenv("PASSPORT_MAINTENANCE_API_STOPPED")) != "true" {
		return errors.New("maintenance requires the API to be stopped")
	}
	dsn, err := readPrivateFile(dsnFile)
	if err != nil || strings.TrimSpace(dsn) == "" {
		return errors.New("database credential unavailable")
	}
	maxImageBytes, err := positiveInt64(getenv("PASSPORT_MAX_UPLOAD_BYTES"))
	if err != nil {
		return errors.New("image size limit unavailable")
	}
	connectTimeout, err := positiveDuration(getenv("PASSPORT_MAINTENANCE_CONNECT_TIMEOUT"))
	if err != nil {
		return errors.New("connect timeout unavailable")
	}
	lockTimeout, err := positiveDuration(getenv("PASSPORT_MAINTENANCE_LOCK_TIMEOUT"))
	if err != nil {
		return errors.New("migration lock timeout unavailable")
	}
	operationTimeout, err := positiveDuration(getenv("PASSPORT_MAINTENANCE_OPERATION_TIMEOUT"))
	if err != nil {
		return errors.New("operation timeout unavailable")
	}
	claimLease, err := positiveDuration(getenv("PASSPORT_RETENTION_CLAIM_LEASE"))
	if err != nil {
		return errors.New("claim lease unavailable")
	}
	interval, err := positiveDuration(getenv("PASSPORT_RETENTION_INTERVAL"))
	if err != nil {
		return errors.New("retention interval unavailable")
	}
	batchSize, err := positiveInt(getenv("PASSPORT_RETENTION_BATCH_SIZE"))
	if err != nil {
		return errors.New("batch size unavailable")
	}
	if err := validateExistingImageRoot(imageRoot); err != nil {
		return errors.New("image root unavailable")
	}

	ctx, cancel := context.WithTimeout(parent, operationTimeout)
	defer cancel()
	store, err := postgres.Open(ctx, postgres.Config{
		DSN: dsn, ConnectTimeout: connectTimeout, MigrationLockTimeout: lockTimeout,
	})
	if err != nil {
		return errors.New("database unavailable")
	}
	defer store.Close()
	if err := store.CheckSchema(ctx); err != nil {
		return errors.New("database schema unavailable")
	}
	images, err := imagestore.New(imagestore.Config{Root: imageRoot, MaxBytes: maxImageBytes})
	if err != nil {
		return errors.New("image root unavailable")
	}
	defer func() { _ = images.Close() }()
	service, err := retention.New(store, images, retention.Config{
		ClaimLease: claimLease, Interval: interval, OperationTimeout: operationTimeout, BatchSize: batchSize,
	})
	if err != nil {
		return errors.New("retention configuration unavailable")
	}
	imageCount, err := service.PurgeTerminalImages(ctx, batchSize)
	if err != nil {
		return errors.New("image retention failed")
	}
	resultCount, err := service.PurgeExpiredResults(ctx, batchSize)
	if err != nil {
		return errors.New("result retention failed")
	}
	orphanCount, err := service.PurgeOrphansOffline(ctx)
	if err != nil {
		return errors.New("offline orphan cleanup failed")
	}
	if _, err := fmt.Fprintf(stdout, "image_objects_purged=%d results_purged=%d orphan_files_purged=%d\n", imageCount, resultCount, orphanCount); err != nil {
		return errors.New("maintenance output failed")
	}
	return nil
}

func readPrivateFile(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 || info.Size() <= 0 || info.Size() > 16<<10 {
		return "", errors.New("invalid private file")
	}
	parentInfo, err := os.Lstat(filepath.Dir(path))
	if err != nil || parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() || parentInfo.Mode().Perm() != 0o700 {
		return "", errors.New("invalid private parent")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("private file unavailable")
	}
	return string(data), nil
}

func validateExistingImageRoot(path string) error {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("invalid private image root")
	}
	return nil
}

func positiveDuration(value string) (time.Duration, error) {
	parsed, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return 0, errors.New("invalid duration")
	}
	return parsed, nil
}

func positiveInt(value string) (int, error) {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return 0, errors.New("invalid integer")
	}
	return parsed, nil
}

func positiveInt64(value string) (int64, error) {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || parsed <= 0 {
		return 0, errors.New("invalid integer")
	}
	return parsed, nil
}
