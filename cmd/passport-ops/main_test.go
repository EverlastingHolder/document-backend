package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"passport-backend/internal/jobstore/postgres"
)

func TestSnapshotFailureStagesAreFixedAndDoNotExposeDetails(t *testing.T) {
	stages := []struct {
		stage failureStage
		name  string
	}{
		{failureArguments, "arguments_invalid"},
		{failureDSN, "dsn_unavailable"},
		{failureImageRoot, "image_root_invalid"},
		{failureDatabase, "database_unavailable"},
		{failureSchema, "schema_unavailable"},
		{failureSnapshotQuery, "snapshot_query_failed"},
		{failureImageStore, "image_store_unavailable"},
		{failureImageUsage, "image_usage_unavailable"},
		{failureOutput, "output_write_failed"},
		{failureStage(255), "unknown"},
	}
	const privateDetail = "postgres://private-user:secret@host/db /var/private/image-root"
	for _, test := range stages {
		var stderr bytes.Buffer
		writeFailure(&stderr, test.stage)
		got := stderr.String()
		want := "operational snapshot unavailable stage=" + test.name + "\n"
		if got != want {
			t.Errorf("writeFailure(%d) = %q, want %q", test.stage, got, want)
		}
		if strings.Contains(got, privateDetail) || strings.Contains(got, "secret") || strings.Contains(got, "image-root") {
			t.Errorf("failure output leaked a private value: %q", got)
		}
	}
}

func TestSnapshotQueryFailureIncludesOnlyValidatedSQLState(t *testing.T) {
	for _, test := range []struct {
		name      string
		sqlState  string
		wantState string
	}{
		{name: "valid", sqlState: "42501", wantState: "42501"},
		{name: "invalid", sqlState: "secret", wantState: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			writeSnapshotQueryFailure(&stderr, &postgres.SafeStorageFailure{
				Operation: "private-operation", SQLState: test.sqlState, ConstraintName: "private_constraint",
			})
			got := stderr.String()
			want := "operational snapshot unavailable stage=snapshot_query_failed"
			if test.wantState != "" {
				want += " sqlstate=" + test.wantState
			}
			want += "\n"
			if got != want {
				t.Fatalf("writeSnapshotQueryFailure() = %q, want %q", got, want)
			}
			for _, forbidden := range []string{"private-operation", "private_constraint", "secret", "SQL", "DSN", "/private"} {
				if strings.Contains(got, forbidden) {
					t.Errorf("failure output leaked %q: %q", forbidden, got)
				}
			}
		})
	}
}

func TestParseSnapshotOptionsRequiresPositiveThresholdsAndBounds(t *testing.T) {
	args := []string{
		"-dsn-file", "/private/dsn", "-image-root", "/private/images",
		"-max-upload-bytes", "1000", "-max-stored-image-bytes", "10000",
		"-connect-timeout", "1s", "-migration-lock-timeout", "2s", "-operation-timeout", "3s",
		"-queued-jobs-threshold", "5", "-oldest-queued-threshold", "10s",
		"-expired-leases-threshold", "1", "-terminal-images-threshold", "1",
		"-terminal-results-threshold", "1", "-image-used-bytes-threshold", "9000",
	}
	options, err := parseSnapshotOptions(args)
	if err != nil {
		t.Fatalf("valid options rejected: %v", err)
	}
	if options.OldestQueuedThreshold != 10*time.Second || options.ImageUsedThreshold != 9000 {
		t.Fatalf("options = %+v", options)
	}
	for _, args := range [][]string{
		append(append([]string(nil), args[:len(args)-2]...), "-image-used-bytes-threshold", "0"),
		append(append([]string(nil), args...), "unexpected"),
	} {
		if _, err := parseSnapshotOptions(args); err == nil {
			t.Fatalf("parseSnapshotOptions(%v) accepted invalid values", args)
		}
	}
}

func TestReadPrivateDSNEnforcesPrivateFileAndParent(t *testing.T) {
	privateDir := t.TempDir()
	if err := os.Chmod(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(privateDir, "dsn")
	secret := "postgres://private-user:private-password@localhost/private-db"
	if err := os.WriteFile(path, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readPrivateDSN(path)
	if err != nil || got != secret {
		t.Fatalf("readPrivateDSN() = %q, %v", got, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateDSN(path); err == nil {
		t.Fatal("readPrivateDSN accepted a broadly-readable file")
	}
}
