//go:build integration

package postgres_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"passport-backend/internal/clientauth"
	"passport-backend/internal/clientenrollment"
	"passport-backend/internal/jobs"
	"passport-backend/internal/jobstore/postgres"
)

func TestPostgresClientEnrollmentReplayAdmissionAndOwnerAuthentication(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	dsn := integrationDSN(t)
	store := openIntegrationStore(t, ctx, dsn)
	pool := openTestPool(t, ctx, dsn)
	resetClientEnrollmentRows(t, ctx, pool)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM client_installations`)
	})

	t.Run("same hash concurrent registrations converge and replay bypasses full caps", func(t *testing.T) {
		service, err := clientenrollment.New(store, nil, 2, 2)
		if err != nil {
			t.Fatal("construct bounded enrollment service")
		}
		token, _, err := clientauth.NewToken()
		if err != nil {
			t.Fatal("generate integration-only client token")
		}
		hash, err := clientauth.HashToken(token)
		if err != nil {
			t.Fatal("hash integration-only client token")
		}

		const workers = 16
		start := make(chan struct{})
		type registration struct {
			installation clientenrollment.Installation
			replay       bool
			err          error
		}
		results := make([]registration, workers)
		var wg sync.WaitGroup
		wg.Add(workers)
		for i := range workers {
			go func(index int) {
				defer wg.Done()
				<-start
				results[index].installation, results[index].replay, results[index].err = service.Register(ctx, hash)
			}(i)
		}
		close(start)
		wg.Wait()
		winner := results[0].installation
		if winner.ClientID == "" || winner.CreatedAt.IsZero() {
			t.Fatal("concurrent registration returned an empty installation")
		}
		for _, result := range results {
			if result.err != nil || result.installation.ClientID != winner.ClientID || !result.installation.CreatedAt.Equal(winner.CreatedAt) {
				t.Fatal("same-hash concurrent registrations did not converge on the persisted installation")
			}
		}
		var count int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM client_installations WHERE token_sha256=$1", hash[:]).Scan(&count); err != nil || count != 1 {
			t.Fatal("same-hash concurrent registrations did not persist exactly one row")
		}

		secondToken, _, err := clientauth.NewToken()
		if err != nil {
			t.Fatal("generate second integration-only token")
		}
		secondHash, err := clientauth.HashToken(secondToken)
		if err != nil {
			t.Fatal("hash second integration-only token")
		}
		if _, replay, err := service.Register(ctx, secondHash); err != nil || replay {
			t.Fatal("second distinct installation did not consume the final configured slots")
		}
		replayed, wasReplay, err := service.Register(ctx, hash)
		if err != nil || !wasReplay || replayed.ClientID != winner.ClientID || !replayed.CreatedAt.Equal(winner.CreatedAt) {
			t.Fatal("existing installation replay failed after client and rate caps were full")
		}

		thirdToken, _, err := clientauth.NewToken()
		if err != nil {
			t.Fatal("generate third integration-only token")
		}
		thirdHash, err := clientauth.HashToken(thirdToken)
		if err != nil {
			t.Fatal("hash third integration-only token")
		}
		if _, _, err := service.Register(ctx, thirdHash); !errors.Is(err, clientenrollment.ErrRateLimited) {
			t.Fatal("new installation at configured limits was not rejected safely")
		}

		if _, err := pool.Exec(ctx, `UPDATE client_installations SET created_at=clock_timestamp()-interval '2 minutes'`); err != nil {
			t.Fatal("age synthetic rows for independent client-cap check")
		}
		clientCapOnly, err := clientenrollment.New(store, nil, 2, 5)
		if err != nil {
			t.Fatal("construct client-cap-only service")
		}
		if _, _, err := clientCapOnly.Register(ctx, thirdHash); !errors.Is(err, clientenrollment.ErrRateLimited) {
			t.Fatal("maximum client count was not enforced independently of recent-registration rate")
		}

		resetClientEnrollmentRows(t, ctx, pool)
		rateLimited, err := clientenrollment.New(store, nil, 5, 1)
		if err != nil {
			t.Fatal("construct rate-cap-only service")
		}
		firstRateToken, _, err := clientauth.NewToken()
		if err != nil {
			t.Fatal("generate first rate fixture token")
		}
		firstRateHash, err := clientauth.HashToken(firstRateToken)
		if err != nil {
			t.Fatal("hash first rate fixture token")
		}
		if _, _, err := rateLimited.Register(ctx, firstRateHash); err != nil {
			t.Fatal("first registration under rate cap failed")
		}
		secondRateToken, _, err := clientauth.NewToken()
		if err != nil {
			t.Fatal("generate second rate fixture token")
		}
		secondRateHash, err := clientauth.HashToken(secondRateToken)
		if err != nil {
			t.Fatal("hash second rate fixture token")
		}
		if _, _, err := rateLimited.Register(ctx, secondRateHash); !errors.Is(err, clientenrollment.ErrRateLimited) {
			t.Fatal("per-minute admission cap was not enforced independently")
		}
	})

	resetClientEnrollmentRows(t, ctx, pool)
	staticCredential := newCredentialFixture(t)
	staticRegistry := writeClientEnrollmentStaticRegistry(t, staticCredential)
	staticEnrollment, err := clientenrollment.New(store, staticRegistry, 20, 20)
	if err != nil {
		t.Fatal("construct enrollment service with reserved static credential")
	}
	if _, _, err := staticEnrollment.Register(ctx, mustHash(t, staticCredential.Token)); !errors.Is(err, clientenrollment.ErrStaticCredential) {
		t.Fatal("admin-provisioned static credential hash was not reserved from anonymous enrollment")
	}
	dynamicAuthenticator, err := clientauth.NewAuthenticator(nil, store)
	if err != nil {
		t.Fatal("construct database-only dynamic authenticator")
	}
	staticAuthenticator, err := clientauth.NewAuthenticator(staticRegistry, store)
	if err != nil {
		t.Fatal("construct static-registry compatibility authenticator")
	}
	staticPrincipal, err := staticAuthenticator.AuthenticateContext(ctx, []string{"Bearer " + staticCredential.Token})
	if err != nil || staticPrincipal.OwnerID != staticCredential.Owner {
		t.Fatal("admin-provisioned static credential compatibility failed")
	}
	apiBaseURL := os.Getenv(testAPIURLEnv)
	if apiBaseURL != "http://api:8080" {
		t.Fatal("enrollment integration must call the actual private Compose API")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	waitForEnrollmentAPI(t, client, apiBaseURL)

	firstToken, firstTokenFixture := newEnrollmentToken(t)
	firstHashHex := firstTokenFixture.hashHex
	secondToken, secondTokenFixture := newEnrollmentToken(t)
	if _, err := dynamicAuthenticator.AuthenticateContext(ctx, []string{"Bearer " + firstToken}); !errors.Is(err, clientauth.ErrUnauthorized) {
		t.Fatal("unregistered token authenticated before enrollment")
	}
	assertEnrollmentStatus(t, client, apiBaseURL, firstToken, randomTestUUID(), http.StatusUnauthorized)
	firstResponse, firstBody := postEnrollment(t, client, apiBaseURL, firstHashHex)
	if firstResponse.StatusCode != http.StatusCreated {
		t.Fatalf("first enrollment returned HTTP %d, want 201", firstResponse.StatusCode)
	}
	firstInstallation := decodeEnrollmentResponse(t, firstResponse, firstBody)
	if strings.Contains(string(firstBody), firstToken) || strings.Contains(string(firstBody), firstHashHex) {
		t.Fatal("enrollment response exposed credential material")
	}
	firstReplayResponse, firstReplayBody := postEnrollment(t, client, apiBaseURL, firstHashHex)
	if firstReplayResponse.StatusCode != http.StatusOK {
		t.Fatalf("replayed enrollment returned HTTP %d, want 200", firstReplayResponse.StatusCode)
	}
	if got := decodeEnrollmentResponse(t, firstReplayResponse, firstReplayBody); got != firstInstallation {
		t.Fatal("replayed enrollment changed client identity or creation time")
	}

	secondResponse, secondBody := postEnrollment(t, client, apiBaseURL, secondTokenFixture.hashHex)
	if secondResponse.StatusCode != http.StatusCreated {
		t.Fatalf("second enrollment returned HTTP %d, want 201", secondResponse.StatusCode)
	}
	secondInstallation := decodeEnrollmentResponse(t, secondResponse, secondBody)
	if secondInstallation.ClientID == firstInstallation.ClientID {
		t.Fatal("distinct client token hashes received the same installation identity")
	}
	// The server configuration intentionally has no static registry path. A
	// second in-process static registry check above protects migration behavior;
	// the real HTTP path here proves anonymous dynamic enrollment needs no file.
	for _, body := range []string{
		`{"token_sha256":"` + firstHashHex + `","owner_id":"` + firstInstallation.ClientID + `"}`,
		`{"token_sha256":"` + strings.ToUpper(firstHashHex) + `"}`,
	} {
		response := postEnrollmentBody(t, client, apiBaseURL, body)
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid enrollment body returned HTTP %d, want 400", response.StatusCode)
		}
		_ = response.Body.Close()
	}

	firstPrincipal, err := dynamicAuthenticator.AuthenticateContext(ctx, []string{"Bearer " + firstToken})
	if err != nil || firstPrincipal.OwnerID != firstInstallation.ClientID {
		t.Fatal("database-backed authenticator did not resolve registered token to persisted owner")
	}
	secondPrincipal, err := dynamicAuthenticator.AuthenticateContext(ctx, []string{"Bearer " + secondToken})
	if err != nil || secondPrincipal.OwnerID != secondInstallation.ClientID {
		t.Fatal("second registered token did not resolve to its own persisted owner")
	}
	workerToken, _, err := clientauth.NewWorkerToken()
	if err != nil {
		t.Fatal("generate wrong-purpose worker token fixture")
	}
	if _, err := dynamicAuthenticator.AuthenticateContext(ctx, []string{"Bearer " + workerToken}); !errors.Is(err, clientauth.ErrUnauthorized) {
		t.Fatal("worker-purpose token authenticated as a public client")
	}

	firstJob := postEnrollmentJob(t, client, apiBaseURL, firstToken, "enrollment-owner-one")
	secondJob := postEnrollmentJob(t, client, apiBaseURL, secondToken, "enrollment-owner-two")
	assertEnrollmentStatus(t, client, apiBaseURL, firstToken, firstJob.JobID, http.StatusOK)
	assertEnrollmentStatus(t, client, apiBaseURL, firstToken, secondJob.JobID, http.StatusNotFound)
	assertEnrollmentStatus(t, client, apiBaseURL, secondToken, firstJob.JobID, http.StatusNotFound)
	assertEnrollmentStatus(t, client, apiBaseURL, workerToken, firstJob.JobID, http.StatusUnauthorized)

	firstTokenHash := mustHash(t, firstToken)
	if _, err := pool.Exec(ctx, `UPDATE client_installations SET revoked_at=clock_timestamp() WHERE token_sha256=$1`, firstTokenHash[:]); err != nil {
		t.Fatal("revoke synthetic dynamic credential")
	}
	if _, err := dynamicAuthenticator.AuthenticateContext(ctx, []string{"Bearer " + firstToken}); !errors.Is(err, clientauth.ErrUnauthorized) {
		t.Fatal("revoked dynamic credential still authenticated")
	}
	assertEnrollmentStatus(t, client, apiBaseURL, firstToken, firstJob.JobID, http.StatusUnauthorized)
	if response, _ := postEnrollment(t, client, apiBaseURL, firstHashHex); response.StatusCode != http.StatusForbidden {
		t.Fatalf("revoked dynamic credential was reactivated by enrollment: HTTP %d", response.StatusCode)
	}

	assertComposedStaticCredentialDBState(t, ctx, store, pool, client, apiBaseURL)

	var persistedRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM client_installations`).Scan(&persistedRows); err != nil || persistedRows != 4 {
		t.Fatal("enrollment flow persisted an unexpected number of dynamic, overlap, or revoked-hash rows")
	}
}

func assertComposedStaticCredentialDBState(t *testing.T, ctx context.Context, store *postgres.Store, pool *pgxpool.Pool, client *http.Client, apiBaseURL string) {
	t.Helper()
	staticDBToken := readEnrollmentFixture(t, "PASSPORT_POSTGRES_TEST_STATIC_DB_TOKEN_FILE")
	staticOnlyToken := readEnrollmentFixture(t, "PASSPORT_POSTGRES_TEST_STATIC_ONLY_TOKEN_FILE")
	ownerBytes, err := os.ReadFile(os.Getenv("PASSPORT_POSTGRES_TEST_STATIC_OWNER_IDS_FILE"))
	if err != nil {
		t.Fatal("read synthetic static-owner fixtures")
	}
	var owners []string
	if err := json.Unmarshal(ownerBytes, &owners); err != nil || len(owners) != 2 || owners[0] == owners[1] {
		t.Fatal("static-owner fixtures were invalid")
	}
	overlapHash := mustHash(t, staticDBToken)
	installation, replay, err := store.RegisterClientToken(ctx, overlapHash, owners[0], 20, 20)
	if err != nil || replay || installation.ClientID != owners[0] {
		t.Fatal("could not seed an active DB row matching the static credential owner")
	}
	var overlapRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM client_installations WHERE token_sha256=$1 AND client_id=$2 AND revoked_at IS NULL`, overlapHash[:], owners[0]).Scan(&overlapRows); err != nil || overlapRows != 1 {
		t.Fatal("static credential overlap fixture was not active in PostgreSQL")
	}

	job := postEnrollmentJob(t, client, apiBaseURL, staticDBToken, "static-db-overlap")
	assertEnrollmentStatus(t, client, apiBaseURL, staticDBToken, job.JobID, http.StatusOK)
	response, _ := postEnrollment(t, client, apiBaseURL, tokenHash(t, staticDBToken))
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("static hash overlap enrollment returned HTTP %d, want 403", response.StatusCode)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal("close static hash enrollment response")
	}

	revoked, err := store.RevokeClientInstallation(ctx, owners[0])
	if err != nil || !revoked {
		t.Fatal("could not revoke the overlapping dynamic installation")
	}
	assertEnrollmentStatus(t, client, apiBaseURL, staticDBToken, job.JobID, http.StatusUnauthorized)

	staticOnlyHash := mustHash(t, staticOnlyToken)
	var staticOnlyRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM client_installations WHERE token_sha256=$1`, staticOnlyHash[:]).Scan(&staticOnlyRows); err != nil || staticOnlyRows != 0 {
		t.Fatal("static-only credential unexpectedly had a database row")
	}
	staticOnlyJob := postEnrollmentJob(t, client, apiBaseURL, staticOnlyToken, "static-only-owner")
	assertEnrollmentStatus(t, client, apiBaseURL, staticOnlyToken, staticOnlyJob.JobID, http.StatusOK)

	assertStaticCredentialFailsClosedWhenDatabaseUnavailable(t, ctx, pool, client, apiBaseURL, staticOnlyToken)
	recoveredStore := openIntegrationStore(t, ctx, integrationDSN(t))

	candidate, err := clientauth.NewOwnerID()
	if err != nil {
		t.Fatal("generate opaque revoked-hash owner ID")
	}
	tombstoneCreated, err := recoveredStore.RevokeClientTokenHash(ctx, staticOnlyHash, candidate)
	if err != nil {
		var safe *postgres.SafeStorageFailure
		if errors.As(err, &safe) {
			t.Fatalf("persist revoked-hash tombstone: operation=%s sqlstate=%s constraint=%s", safe.Operation, safe.SQLState, safe.ConstraintName)
		}
		t.Fatalf("persist revoked-hash tombstone: error_type=%T", err)
	}
	if !tombstoneCreated {
		t.Fatal("missing static-only hash did not create a revoked tombstone")
	}
	assertEnrollmentStatus(t, client, apiBaseURL, staticOnlyToken, staticOnlyJob.JobID, http.StatusUnauthorized)
	withoutStaticRegistry, err := clientenrollment.New(recoveredStore, nil, 20, 20)
	if err != nil {
		t.Fatal("construct enrollment service without static registry")
	}
	if _, _, err := withoutStaticRegistry.Register(ctx, staticOnlyHash); !errors.Is(err, clientenrollment.ErrRevokedCredential) {
		t.Fatal("removing a static registry entry could reactivate its database tombstone")
	}
}

func assertStaticCredentialFailsClosedWhenDatabaseUnavailable(t *testing.T, ctx context.Context, pool *pgxpool.Pool, client *http.Client, apiBaseURL, token string) {
	t.Helper()
	config, err := pgxpool.ParseConfig(integrationDSN(t))
	if err != nil {
		t.Fatal("parse isolated database configuration")
	}
	databaseName := config.ConnConfig.Database
	config.ConnConfig.Database = "postgres"
	admin, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("open isolated PostgreSQL administration connection")
	}
	databaseIdent := pgx.Identifier{databaseName}.Sanitize()
	if _, err := admin.Exec(ctx, "ALTER DATABASE "+databaseIdent+" WITH ALLOW_CONNECTIONS false"); err != nil {
		t.Fatal("disable only the isolated integration database")
	}
	connectionsDisabled := true
	t.Cleanup(func() {
		if connectionsDisabled {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cleanupAdmin, err := pgxpool.NewWithConfig(cleanupCtx, config)
			if err == nil {
				_, _ = cleanupAdmin.Exec(cleanupCtx, "ALTER DATABASE "+databaseIdent+" WITH ALLOW_CONNECTIONS true")
				cleanupAdmin.Close()
			}
		}
		admin.Close()
	})
	if _, err := admin.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid<>pg_backend_pid()`, databaseName); err != nil {
		t.Fatal("terminate connections to only the isolated integration database")
	}
	request, err := http.NewRequest(http.MethodGet, apiBaseURL+"/v1/passport/jobs/"+randomTestUUID(), nil)
	if err != nil {
		t.Fatal("construct static credential outage request")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Request-ID", randomTestUUID())
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("call composed API during isolated database outage")
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("static credential with unavailable DB returned HTTP %d, want 503", response.StatusCode)
	}
	if _, err := admin.Exec(ctx, "ALTER DATABASE "+databaseIdent+" WITH ALLOW_CONNECTIONS true"); err != nil {
		t.Fatal("re-enable only the isolated integration database")
	}
	connectionsDisabled = false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if pool.Ping(ctx) == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("isolated integration database did not accept connections after recovery")
}

func readEnrollmentFixture(t *testing.T, envName string) string {
	t.Helper()
	path := os.Getenv(envName)
	if path == "" {
		t.Fatalf("missing private integration fixture path %s", envName)
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		t.Fatalf("could not read synthetic credential fixture %s", envName)
	}
	return strings.TrimSpace(string(data))
}

type enrollmentTokenFixture struct {
	raw     string
	hashHex string
}

func newEnrollmentToken(t *testing.T) (string, enrollmentTokenFixture) {
	t.Helper()
	raw, hashHex, err := clientauth.NewToken()
	if err != nil {
		t.Fatal("generate enrollment token fixture")
	}
	return raw, enrollmentTokenFixture{raw: raw, hashHex: hashHex}
}

func resetClientEnrollmentRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DELETE FROM client_installations`); err != nil {
		t.Fatal("reset synthetic client enrollment rows in isolated test database")
	}
}

func writeClientEnrollmentStaticRegistry(t *testing.T, credential credentialFixture) *clientauth.Registry {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal("protect static test registry directory")
	}
	path := filepath.Join(dir, "client-credentials.json")
	body, err := json.Marshal(struct {
		SchemaVersion string           `json:"schema_version"`
		Credentials   []credentialJSON `json:"credentials"`
	}{"passport-client-credentials/v1", []credentialJSON{{OwnerID: credential.Owner, TokenSHA256: tokenHash(t, credential.Token)}}})
	if err != nil {
		t.Fatal("encode static test credential registry")
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal("write static test credential registry")
	}
	registry, err := clientauth.Load(path)
	if err != nil {
		t.Fatal("load static test credential registry")
	}
	return registry
}

func postEnrollment(t *testing.T, client *http.Client, baseURL, hashHex string) (*http.Response, []byte) {
	t.Helper()
	response := postEnrollmentBody(t, client, baseURL, `{"token_sha256":"`+hashHex+`"}`)
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal("read enrollment response")
	}
	return response, body
}

func waitForEnrollmentAPI(t *testing.T, client *http.Client, baseURL string) {
	t.Helper()
	for attempt := 0; attempt < 60; attempt++ {
		request, err := http.NewRequest(http.MethodGet, baseURL+"/v1/client-installations", nil)
		if err != nil {
			t.Fatal("construct API readiness request")
		}
		request.Header.Set("X-Request-ID", randomTestUUID())
		response, requestErr := client.Do(request)
		if requestErr == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("actual API did not become reachable for the enrollment integration test")
}

func postEnrollmentBody(t *testing.T, client *http.Client, baseURL, body string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, baseURL+"/v1/client-installations", strings.NewReader(body))
	if err != nil {
		t.Fatal("construct enrollment request")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Request-ID", randomTestUUID())
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("call enrollment handler")
	}
	return response
}

func decodeEnrollmentResponse(t *testing.T, response *http.Response, body []byte) enrollmentResponseFixture {
	t.Helper()
	responseRequestID := response.Header.Get("X-Request-ID")
	if responseRequestID == "" {
		t.Fatal("enrollment response omitted request ID")
	}
	assertNoStoreAndRequestID(t, response.Header, responseRequestID)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || len(fields) != 2 || fields["client_id"] == nil || fields["created_at"] == nil {
		t.Fatal("enrollment response did not contain only client_id and created_at")
	}
	var result enrollmentResponseFixture
	if err := json.Unmarshal(body, &result); err != nil || result.ClientID == "" || result.CreatedAt.IsZero() {
		t.Fatal("enrollment response fields were invalid")
	}
	return result
}

type enrollmentResponseFixture struct {
	ClientID  string    `json:"client_id"`
	CreatedAt time.Time `json:"created_at"`
}

func postEnrollmentJob(t *testing.T, client *http.Client, baseURL, token, key string) acceptedPayload {
	t.Helper()
	request := multipartRequest(t, baseURL+"/v1/passport/jobs", token, key, "image/png", []byte("synthetic enrollment integration image"))
	requestID := randomTestUUID()
	request.Header.Set("X-Request-ID", requestID)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("POST real async job through composed Go API")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusAccepted {
		t.Fatalf("real API job acceptance returned HTTP %d or unreadable body", response.StatusCode)
	}
	assertNoStoreAndRequestID(t, response.Header, requestID)
	var accepted acceptedPayload
	if err := json.Unmarshal(body, &accepted); err != nil || accepted.JobID == "" || accepted.Status != jobs.StatusQueued || accepted.StatusURL != "/v1/passport/jobs/"+accepted.JobID {
		t.Fatal("real API did not durably accept a queued job for the enrolled owner")
	}
	return accepted
}

func assertEnrollmentStatus(t *testing.T, client *http.Client, baseURL, token, jobID string, wantStatus int) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, baseURL+"/v1/passport/jobs/"+jobID, nil)
	if err != nil {
		t.Fatal("construct owner-scoped status request")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	requestID := randomTestUUID()
	request.Header.Set("X-Request-ID", requestID)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("GET real job status through composed Go API")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != wantStatus {
		t.Fatalf("owner-scoped status returned HTTP %d, want %d", response.StatusCode, wantStatus)
	}
	assertNoStoreAndRequestID(t, response.Header, requestID)
	if wantStatus == http.StatusOK {
		var status struct {
			JobID  string      `json:"job_id"`
			Status jobs.Status `json:"status"`
		}
		if err := json.Unmarshal(body, &status); err != nil || status.JobID != jobID || status.Status != jobs.StatusQueued {
			t.Fatal("owner status response did not return the persisted queued job")
		}
	}
}

func mustHash(t *testing.T, token string) [sha256.Size]byte {
	t.Helper()
	hash, err := clientauth.HashToken(token)
	if err != nil {
		t.Fatal("hash synthetic token")
	}
	return hash
}
