// Command passport-ops emits a private aggregate-only operational snapshot.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"passport-backend/internal/imagestore"
	"passport-backend/internal/jobstore/postgres"
)

const maxDSNBytes = 16 << 10

var safeSQLStatePattern = regexp.MustCompile(`^[0-9A-Z]{5}$`)

type failureStage uint8

const (
	failureArguments failureStage = iota + 1
	failureDSN
	failureImageRoot
	failureDatabase
	failureSchema
	failureSnapshotQuery
	failureImageStore
	failureImageUsage
	failureOutput
)

type snapshotOptions struct {
	DSNFile                 string
	ImageRoot               string
	MaxUploadBytes          int64
	MaxStoredImageBytes     int64
	ConnectTimeout          time.Duration
	MigrationLockTimeout    time.Duration
	OperationTimeout        time.Duration
	QueuedJobsThreshold     int64
	OldestQueuedThreshold   time.Duration
	ExpiredLeasesThreshold  int64
	ImagesPendingThreshold  int64
	ResultsPendingThreshold int64
	ImageUsedThreshold      int64
}

type alertSnapshot struct {
	QueuedJobs             bool `json:"queued_jobs"`
	OldestQueuedAge        bool `json:"oldest_queued_age"`
	ExpiredLeases          bool `json:"expired_leases"`
	TerminalImagesPending  bool `json:"terminal_images_pending"`
	TerminalResultsPending bool `json:"terminal_results_pending"`
	ImageUsedBytes         bool `json:"image_used_bytes"`
}

type operationalSnapshot struct {
	QueuedJobs             int64         `json:"queued_jobs"`
	OldestQueuedAgeMS      int64         `json:"oldest_queued_age_ms"`
	ExpiredLeases          int64         `json:"expired_leases"`
	TerminalImagesPending  int64         `json:"terminal_images_pending"`
	TerminalResultsPending int64         `json:"terminal_results_pending"`
	ImageUsedBytes         int64         `json:"image_used_bytes"`
	ImageQuotaBytes        int64         `json:"image_quota_bytes"`
	ImageFreeBytes         int64         `json:"image_free_bytes"`
	Alerts                 alertSnapshot `json:"alerts"`
}

func main() {
	if code := mainExit(context.Background(), os.Args[1:], os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

func mainExit(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if ctx == nil || stdout == nil || stderr == nil || len(args) == 0 || args[0] != "snapshot" {
		writeFailure(stderr, failureArguments)
		return 2
	}
	options, err := parseSnapshotOptions(args[1:])
	if err != nil {
		writeFailure(stderr, failureArguments)
		return 2
	}
	dsn, err := readPrivateDSN(options.DSNFile)
	if err != nil {
		writeFailure(stderr, failureDSN)
		return 1
	}
	imageInfo, err := os.Lstat(options.ImageRoot)
	if err != nil || imageInfo.Mode()&os.ModeSymlink != 0 || !imageInfo.IsDir() || imageInfo.Mode().Perm() != 0o700 {
		writeFailure(stderr, failureImageRoot)
		return 1
	}
	store, err := postgres.Open(ctx, postgres.Config{DSN: dsn, ConnectTimeout: options.ConnectTimeout, MigrationLockTimeout: options.MigrationLockTimeout})
	if err != nil {
		writeFailure(stderr, failureDatabase)
		return 1
	}
	defer store.Close()
	operationCtx, cancel := context.WithTimeout(ctx, options.OperationTimeout)
	defer cancel()
	if err := store.CheckSchema(operationCtx); err != nil {
		writeFailure(stderr, failureSchema)
		return 1
	}
	dbSnapshot, err := store.ReadMaintenanceSnapshot(operationCtx)
	if err != nil {
		writeSnapshotQueryFailure(stderr, err)
		return 1
	}
	images, err := imagestore.New(imagestore.Config{
		Root: options.ImageRoot, MaxBytes: options.MaxUploadBytes, MaxTotalBytes: options.MaxStoredImageBytes,
	})
	if err != nil {
		writeFailure(stderr, failureImageStore)
		return 1
	}
	defer images.Close()
	usage, err := images.Usage(operationCtx)
	if err != nil {
		writeFailure(stderr, failureImageUsage)
		return 1
	}
	used := dbSnapshot
	snapshot := operationalSnapshot{
		QueuedJobs: used.QueuedJobs, OldestQueuedAgeMS: int64(used.OldestQueuedAge / time.Millisecond),
		ExpiredLeases: used.ExpiredLeases, TerminalImagesPending: used.TerminalImagesPending,
		TerminalResultsPending: used.TerminalResultsPending, ImageUsedBytes: usage.UsedBytes,
		ImageQuotaBytes: usage.MaxTotalBytes, ImageFreeBytes: usage.MaxTotalBytes - usage.UsedBytes,
	}
	snapshot.Alerts = alertSnapshot{
		QueuedJobs:             snapshot.QueuedJobs > options.QueuedJobsThreshold,
		OldestQueuedAge:        dbSnapshot.OldestQueuedAge > options.OldestQueuedThreshold,
		ExpiredLeases:          snapshot.ExpiredLeases > options.ExpiredLeasesThreshold,
		TerminalImagesPending:  snapshot.TerminalImagesPending > options.ImagesPendingThreshold,
		TerminalResultsPending: snapshot.TerminalResultsPending > options.ResultsPendingThreshold,
		ImageUsedBytes:         snapshot.ImageUsedBytes > options.ImageUsedThreshold,
	}
	if err := json.NewEncoder(stdout).Encode(snapshot); err != nil {
		writeFailure(stderr, failureOutput)
		return 1
	}
	return 0
}

func parseSnapshotOptions(args []string) (snapshotOptions, error) {
	flags := flag.NewFlagSet("passport-ops snapshot", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var options snapshotOptions
	flags.StringVar(&options.DSNFile, "dsn-file", "", "private PostgreSQL DSN file")
	flags.StringVar(&options.ImageRoot, "image-root", "", "existing private image root")
	flags.Int64Var(&options.MaxUploadBytes, "max-upload-bytes", 0, "configured maximum image object size")
	flags.Int64Var(&options.MaxStoredImageBytes, "max-stored-image-bytes", 0, "configured image-store quota")
	flags.DurationVar(&options.ConnectTimeout, "connect-timeout", 0, "maximum database connect time")
	flags.DurationVar(&options.MigrationLockTimeout, "migration-lock-timeout", 0, "required PostgreSQL store setting")
	flags.DurationVar(&options.OperationTimeout, "operation-timeout", 0, "maximum snapshot operation time")
	flags.Int64Var(&options.QueuedJobsThreshold, "queued-jobs-threshold", 0, "alert when queued jobs exceed this value")
	flags.DurationVar(&options.OldestQueuedThreshold, "oldest-queued-threshold", 0, "alert when oldest queued age exceeds this duration")
	flags.Int64Var(&options.ExpiredLeasesThreshold, "expired-leases-threshold", 0, "alert when expired leases exceed this value")
	flags.Int64Var(&options.ImagesPendingThreshold, "terminal-images-threshold", 0, "alert when terminal image cleanup backlog exceeds this value")
	flags.Int64Var(&options.ResultsPendingThreshold, "terminal-results-threshold", 0, "alert when terminal result cleanup backlog exceeds this value")
	flags.Int64Var(&options.ImageUsedThreshold, "image-used-bytes-threshold", 0, "alert when image storage use exceeds this byte count")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || options.DSNFile == "" || options.ImageRoot == "" ||
		options.MaxUploadBytes <= 0 || options.MaxStoredImageBytes <= 0 ||
		options.ConnectTimeout <= 0 || options.MigrationLockTimeout <= 0 || options.OperationTimeout <= 0 ||
		options.QueuedJobsThreshold <= 0 || options.OldestQueuedThreshold <= 0 || options.ExpiredLeasesThreshold <= 0 ||
		options.ImagesPendingThreshold <= 0 || options.ResultsPendingThreshold <= 0 || options.ImageUsedThreshold <= 0 ||
		options.ImageUsedThreshold > options.MaxStoredImageBytes {
		return snapshotOptions{}, errors.New("invalid snapshot configuration")
	}
	return options, nil
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

func writeFailure(stderr io.Writer, stage failureStage) {
	_, _ = fmt.Fprintf(stderr, "operational snapshot unavailable stage=%s\n", stage.String())
}

func writeSnapshotQueryFailure(stderr io.Writer, err error) {
	var failure *postgres.SafeStorageFailure
	if errors.As(err, &failure) && safeSQLStatePattern.MatchString(failure.SQLState) {
		_, _ = fmt.Fprintf(stderr, "operational snapshot unavailable stage=%s sqlstate=%s\n", failureSnapshotQuery.String(), failure.SQLState)
		return
	}
	writeFailure(stderr, failureSnapshotQuery)
}

func (stage failureStage) String() string {
	switch stage {
	case failureArguments:
		return "arguments_invalid"
	case failureDSN:
		return "dsn_unavailable"
	case failureImageRoot:
		return "image_root_invalid"
	case failureDatabase:
		return "database_unavailable"
	case failureSchema:
		return "schema_unavailable"
	case failureSnapshotQuery:
		return "snapshot_query_failed"
	case failureImageStore:
		return "image_store_unavailable"
	case failureImageUsage:
		return "image_usage_unavailable"
	case failureOutput:
		return "output_write_failed"
	default:
		return "unknown"
	}
}
