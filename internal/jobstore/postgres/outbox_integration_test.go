//go:build integration

package postgres_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	amqp "github.com/rabbitmq/amqp091-go"
	"passport-backend/internal/api"
	"passport-backend/internal/jobexecution"
	"passport-backend/internal/jobs"
	"passport-backend/internal/jobstore/postgres"
	"passport-backend/internal/outbox"
)

const testBrokerDSNFileEnv = "PASSPORT_POSTGRES_TEST_BROKER_DSN_FILE"
const testClientTokenFileEnv = "PASSPORT_POSTGRES_TEST_CLIENT_TOKEN_FILE"
const testAPIURLEnv = "PASSPORT_POSTGRES_TEST_API_URL"
const testOCRStateEnv = "PASSPORT_POSTGRES_TEST_OCR_STATE"

func TestPostgresTerminalTransitionsClampFutureTimestamps(t *testing.T) {
	dsn := integrationDSN(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	store := openIntegrationStore(t, ctx, dsn)
	pool := openTestPool(t, ctx, dsn)
	service, err := jobexecution.New(store, jobexecution.Config{
		PipelineVersion: "integration-terminal-clock-v1", LeaseDuration: 2 * time.Hour,
		ProcessingTimeout: time.Hour, CompletionBudget: 30 * time.Minute,
		RetryPolicy: jobs.RetryPolicy{MaxAttempts: 1, InitialBackoff: time.Second, MaxBackoff: time.Second},
	})
	if err != nil {
		t.Fatalf("construct job execution service: %v", err)
	}

	newFixture := func(t *testing.T, suffix string) jobs.Submission {
		t.Helper()
		digest := sha256.Sum256([]byte("terminal clock fixture " + suffix + " " + randomTestUUID()))
		submission := newSubmission("owner-terminal-clock-"+suffix+"-"+randomTestUUID(), "terminal-clock-"+suffix+"-"+randomTestUUID(), digest)
		if _, outcome, err := store.CreateQueued(ctx, submission); err != nil || outcome != jobs.OutcomeCreated {
			t.Fatalf("CreateQueued outcome=%v err=%v", outcome, err)
		}
		return submission
	}

	setFutureJobStart := func(t *testing.T, jobID string, processing bool) {
		t.Helper()
		var started time.Time
		err := pool.QueryRow(ctx, `
			WITH future AS MATERIALIZED (SELECT clock_timestamp() + interval '1 hour' AS at)
			UPDATE jobs SET started_at=future.at, updated_at=future.at
			FROM future WHERE job_id=$1 RETURNING started_at`, jobID).Scan(&started)
		if err != nil {
			t.Fatalf("set future job timestamps: %v", err)
		}
		if processing {
			if _, err := pool.Exec(ctx, `
				UPDATE job_attempts SET started_at=$2::timestamptz, lease_expires_at=$2::timestamptz + interval '2 hours'
				WHERE job_id=$1 AND attempt_no=1 AND state='processing'`, jobID, started); err != nil {
				t.Fatalf("set future attempt timestamps: %v", err)
			}
		}
	}

	assertTerminalTimestamps := func(t *testing.T, jobID, wantState string, attemptExpected bool) {
		t.Helper()
		var state string
		var started, finished, updated time.Time
		if err := pool.QueryRow(ctx, `SELECT state, started_at, finished_at, updated_at FROM jobs WHERE job_id=$1`, jobID).
			Scan(&state, &started, &finished, &updated); err != nil {
			t.Fatalf("read terminal job timestamps: %v", err)
		}
		if state != wantState || finished.Before(started) || updated.Before(finished) {
			t.Fatalf("terminal timestamp invariant state=%s finished>=started=%t updated>=finished=%t", state, !finished.Before(started), !updated.Before(finished))
		}
		if attemptExpected {
			var attemptState string
			var attemptStarted, attemptFinished time.Time
			if err := pool.QueryRow(ctx, `SELECT state, started_at, finished_at FROM job_attempts WHERE job_id=$1 AND attempt_no=1`, jobID).
				Scan(&attemptState, &attemptStarted, &attemptFinished); err != nil {
				t.Fatalf("read terminal attempt timestamps: %v", err)
			}
			if attemptState != wantState || attemptFinished.Before(attemptStarted) {
				t.Fatalf("terminal attempt timestamp invariant state=%s finished>=started=%t", attemptState, !attemptFinished.Before(attemptStarted))
			}
		}
	}

	t.Run("completed job and attempt share a clamped terminal instant", func(t *testing.T) {
		submission := newFixture(t, "completed")
		envelope := jobs.Envelope{SchemaVersion: 1, EventID: submission.EventID, JobID: submission.JobID, AttemptNo: 1}
		claim, err := service.Claim(ctx, envelope)
		if err != nil || claim.Execution == nil {
			t.Fatalf("claim = (%+v, %v)", claim, err)
		}
		setFutureJobStart(t, submission.JobID, true)
		fields := map[string]*string{
			"surname": nil, "given_name": nil, "patronymic": nil, "gender": nil, "birth_date": nil,
			"birth_place": nil, "series": nil, "number": nil, "issue_date": nil, "issuer": nil, "department_code": nil,
		}
		decision, err := service.CompleteRaw(ctx, jobs.Completion{Envelope: envelope, AttemptToken: claim.Execution.AttemptToken, Fields: fields})
		if err != nil || decision != jobs.DecisionAck {
			t.Fatalf("CompleteRaw = (%q, %v), want durable ACK", decision, err)
		}
		assertTerminalTimestamps(t, submission.JobID, string(jobs.StatusCompleted), true)
	})

	t.Run("failed job and attempt share a clamped terminal instant", func(t *testing.T) {
		submission := newFixture(t, "failed")
		envelope := jobs.Envelope{SchemaVersion: 1, EventID: submission.EventID, JobID: submission.JobID, AttemptNo: 1}
		claim, err := service.Claim(ctx, envelope)
		if err != nil || claim.Execution == nil {
			t.Fatalf("claim = (%+v, %v)", claim, err)
		}
		setFutureJobStart(t, submission.JobID, true)
		decision, err := service.Fail(ctx, jobs.Failure{Envelope: envelope, AttemptToken: claim.Execution.AttemptToken, ErrorCode: "processing_failed"})
		if err != nil || decision != jobs.DecisionAck {
			t.Fatalf("Fail = (%q, %v), want durable ACK", decision, err)
		}
		assertTerminalTimestamps(t, submission.JobID, string(jobs.StatusFailed), true)
	})

	t.Run("known queued poison clamps terminal job timestamp", func(t *testing.T) {
		submission := newFixture(t, "rejected")
		setFutureJobStart(t, submission.JobID, false)
		decision, err := store.RejectDelivery(ctx, jobs.Rejection{
			Envelope: jobs.Envelope{SchemaVersion: 1, EventID: submission.EventID, JobID: submission.JobID, AttemptNo: 1},
		})
		if err != nil || decision != jobs.DecisionAck {
			t.Fatalf("RejectDelivery = (%q, %v), want durable ACK", decision, err)
		}
		assertTerminalTimestamps(t, submission.JobID, string(jobs.StatusFailed), false)
	})
}

type relayRepository struct {
	jobs.ExecutionRepository
	firstLease    func() error
	once          sync.Once
	firstErr      error
	loseMark      atomic.Bool
	loseMarkEvent string
}

func (r *relayRepository) LeaseOutbox(ctx context.Context, limit int, leaseFor time.Duration) ([]jobs.LeasedOutbox, error) {
	r.once.Do(func() {
		if r.firstLease != nil {
			r.firstErr = r.firstLease()
		}
	})
	if r.firstErr != nil {
		return nil, r.firstErr
	}
	return r.ExecutionRepository.LeaseOutbox(ctx, limit, leaseFor)
}

func (r *relayRepository) MarkOutboxPublished(ctx context.Context, eventID, claimToken string) error {
	if eventID == r.loseMarkEvent && r.loseMark.CompareAndSwap(true, false) {
		return errors.New("injected lost database acknowledgement after broker confirm")
	}
	return r.ExecutionRepository.MarkOutboxPublished(ctx, eventID, claimToken)
}

func TestPostgresOutboxRelayPublishesRoutedPersistentEnvelope(t *testing.T) {
	dsn := integrationDSN(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	store := openIntegrationStore(t, ctx, dsn)
	pool := openTestPool(t, ctx, dsn)
	brokerDSN, brokerFile := integrationBrokerDSN(t)
	adminConn, err := amqp.Dial(brokerDSN)
	if err != nil {
		t.Fatal("connect to isolated RabbitMQ test broker")
	}
	defer func() { _ = adminConn.Close() }()
	admin, err := adminConn.Channel()
	if err != nil {
		t.Fatal("open isolated RabbitMQ test channel")
	}
	defer func() { _ = admin.Close() }()

	digest := sha256.Sum256([]byte("synthetic relay fixture"))
	submission := newSubmission("owner-relay-route", "relay-route-key", digest)
	if _, outcome, err := store.CreateQueued(ctx, submission); err != nil || outcome != jobs.OutcomeCreated {
		t.Fatalf("CreateQueued outcome=%v err=%v", outcome, err)
	}
	ready := make(chan error, 1)
	repo := &relayRepository{ExecutionRepository: store, firstLease: func() error {
		err := admin.QueueUnbind(outbox.WorkQueueName, outbox.WorkRoutingKey, outbox.ExchangeName, nil)
		ready <- err
		return err
	}}
	relay := newIntegrationRelay(t, repo, brokerFile, 3*time.Second)
	relayCtx, stopRelay := context.WithCancel(ctx)
	relayDone := make(chan error, 1)
	go func() { relayDone <- relay.Run(relayCtx) }()
	if err := <-ready; err != nil {
		stopRelay()
		t.Fatal("remove the isolated work-queue binding")
	}

	waitForOutboxState(t, ctx, pool, submission.EventID, "publish_failed", false)
	if err := admin.QueueBind(outbox.WorkQueueName, outbox.WorkRoutingKey, outbox.ExchangeName, false, nil); err != nil {
		stopRelay()
		t.Fatal("restore the isolated work-queue binding")
	}
	waitForOutboxState(t, ctx, pool, submission.EventID, "", true)
	message := getMessageByID(t, ctx, admin, submission.EventID)
	assertEnvelopeMessage(t, message, submission.EventID, submission.JobID)

	stopRelay()
	if err := <-relayDone; err != nil {
		t.Fatalf("relay.Run() after context cancellation = %v", err)
	}
}

func TestPostgresOutboxRelayLeaseExpiryCanDuplicateAfterConfirmBeforeMark(t *testing.T) {
	dsn := integrationDSN(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	store := openIntegrationStore(t, ctx, dsn)
	pool := openTestPool(t, ctx, dsn)
	brokerDSN, brokerFile := integrationBrokerDSN(t)
	conn, err := amqp.Dial(brokerDSN)
	if err != nil {
		t.Fatal("connect to isolated RabbitMQ test broker")
	}
	defer func() { _ = conn.Close() }()
	channel, err := conn.Channel()
	if err != nil {
		t.Fatal("open isolated RabbitMQ test channel")
	}
	defer func() { _ = channel.Close() }()

	digest := sha256.Sum256([]byte("synthetic relay duplicate fixture"))
	submission := newSubmission("owner-relay-duplicate", "relay-duplicate-key", digest)
	if _, outcome, err := store.CreateQueued(ctx, submission); err != nil || outcome != jobs.OutcomeCreated {
		t.Fatalf("CreateQueued outcome=%v err=%v", outcome, err)
	}
	repo := &relayRepository{ExecutionRepository: store}
	repo.loseMark.Store(true)
	repo.loseMarkEvent = submission.EventID
	relay := newIntegrationRelay(t, repo, brokerFile, 750*time.Millisecond)
	relayCtx, stopRelay := context.WithCancel(ctx)
	relayDone := make(chan error, 1)
	go func() { relayDone <- relay.Run(relayCtx) }()
	waitForOutboxState(t, ctx, pool, submission.EventID, "", true)
	first := getMessageByID(t, ctx, channel, submission.EventID)
	second := getMessageByID(t, ctx, channel, submission.EventID)
	assertEnvelopeMessage(t, first, submission.EventID, submission.JobID)
	assertEnvelopeMessage(t, second, submission.EventID, submission.JobID)

	stopRelay()
	if err := <-relayDone; err != nil {
		t.Fatalf("relay.Run() after context cancellation = %v", err)
	}
}

func TestPostgresQuorumQueueSurvivesMoreThanTwentyConsumerDisconnects(t *testing.T) {
	dsn := integrationDSN(t)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	store := openIntegrationStore(t, ctx, dsn)
	brokerDSN, brokerFile := integrationBrokerDSN(t)
	relay := newIntegrationRelay(t, store, brokerFile, 3*time.Second)
	if err := relay.Check(ctx); err != nil {
		t.Fatal("declare isolated quorum queue with broker delivery limit disabled")
	}
	connection, err := amqp.Dial(brokerDSN)
	if err != nil {
		t.Fatal("connect to isolated RabbitMQ test broker")
	}
	channel, err := connection.Channel()
	if err != nil {
		_ = connection.Close()
		t.Fatal("open isolated RabbitMQ publisher channel")
	}
	confirms := channel.NotifyPublish(make(chan amqp.Confirmation, 1))
	if err := channel.Confirm(false); err != nil {
		_ = connection.Close()
		t.Fatal("enable publisher confirms for queue redelivery fixture")
	}
	eventID := "019b0000-0000-4000-8000-000000000011"
	jobID := "019b0000-0000-4000-8000-000000000012"
	body, err := json.Marshal(jobs.Envelope{SchemaVersion: 1, EventID: eventID, JobID: jobID, AttemptNo: 1})
	if err != nil {
		t.Fatal("encode synthetic queue envelope")
	}
	if err := channel.Publish(outbox.ExchangeName, outbox.WorkRoutingKey, true, false, amqp.Publishing{
		ContentType: "application/json", DeliveryMode: amqp.Persistent, MessageId: eventID, Body: body,
	}); err != nil {
		_ = connection.Close()
		t.Fatal("publish synthetic quorum queue fixture")
	}
	select {
	case confirmation, ok := <-confirms:
		if !ok || !confirmation.Ack {
			_ = connection.Close()
			t.Fatal("RabbitMQ did not confirm synthetic quorum queue fixture")
		}
	case <-ctx.Done():
		_ = connection.Close()
		t.Fatal("timed out waiting for synthetic quorum queue publisher confirm")
	}
	_ = connection.Close()

	const disconnects = 23
	for attempt := 1; attempt <= disconnects; attempt++ {
		consumerConnection, err := amqp.Dial(brokerDSN)
		if err != nil {
			t.Fatal("reconnect to isolated RabbitMQ test broker")
		}
		consumerChannel, err := consumerConnection.Channel()
		if err != nil {
			_ = consumerConnection.Close()
			t.Fatal("open isolated RabbitMQ delivery channel")
		}
		if err := consumerChannel.Qos(1, 0, false); err != nil {
			_ = consumerConnection.Close()
			t.Fatal("set one-message prefetch for quorum queue redelivery fixture")
		}
		deliveries, err := consumerChannel.Consume(outbox.WorkQueueName, "", false, false, false, false, nil)
		if err != nil {
			_ = consumerConnection.Close()
			var protocolError *amqp.Error
			if errors.As(err, &protocolError) {
				t.Fatalf("delivery %d of %d failed with AMQP code %d", attempt, disconnects, protocolError.Code)
			}
			t.Fatalf("delivery %d of %d failed (%T)", attempt, disconnects, err)
		}
		var message amqp.Delivery
		for {
			select {
			case delivery, open := <-deliveries:
				if !open {
					_ = consumerConnection.Close()
					t.Fatalf("delivery %d of %d consumer closed", attempt, disconnects)
				}
				if delivery.MessageId == eventID {
					message = delivery
					break
				}
				if err := delivery.Ack(false); err != nil {
					_ = consumerConnection.Close()
					t.Fatal("ack unrelated preexisting quorum queue fixture")
				}
			case <-ctx.Done():
				_ = consumerConnection.Close()
				t.Fatalf("delivery %d of %d unavailable after consumer disconnects", attempt, disconnects)
			}
			if message.MessageId == eventID {
				break
			}
		}
		if message.MessageId != eventID || message.Redelivered != (attempt > 1) {
			_ = consumerConnection.Close()
			t.Fatalf("delivery %d lost the original event identity or redelivery marker", attempt)
		}
		if attempt == disconnects {
			if err := message.Ack(false); err != nil {
				_ = consumerConnection.Close()
				t.Fatal("ack final synthetic quorum queue delivery")
			}
		}
		_ = consumerConnection.Close()
	}
}

func TestPostgresOutboxRelayReconnectsAfterBrokerRestart(t *testing.T) {
	dsn := integrationDSN(t)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	store := openIntegrationStore(t, ctx, dsn)
	pool := openTestPool(t, ctx, dsn)
	brokerDSN, brokerFile := integrationBrokerDSN(t)
	controlRoot := os.Getenv(testImageRootEnv)
	if controlRoot == "" {
		t.Fatal("integration tests require PASSPORT_POSTGRES_TEST_IMAGE_ROOT for the broker restart control files")
	}
	readyFile := filepath.Join(controlRoot, "stage4-relay-restart-ready")
	continueFile := filepath.Join(controlRoot, "stage4-relay-restart-continue")
	_ = os.Remove(readyFile)
	_ = os.Remove(continueFile)
	t.Cleanup(func() {
		_ = os.Remove(readyFile)
		_ = os.Remove(continueFile)
	})

	first := newSubmission("owner-relay-restart", "relay-restart-before", sha256.Sum256([]byte("before RabbitMQ restart")))
	if _, outcome, err := store.CreateQueued(ctx, first); err != nil || outcome != jobs.OutcomeCreated {
		t.Fatalf("CreateQueued before broker restart outcome=%v err=%v", outcome, err)
	}
	relay := newIntegrationRelay(t, store, brokerFile, 3*time.Second)
	if err := relay.Check(ctx); err != nil {
		t.Fatal("check outbox topology before broker restart")
	}
	relayCtx, stopRelay := context.WithCancel(ctx)
	relayDone := make(chan error, 1)
	go func() { relayDone <- relay.Run(relayCtx) }()
	waitForOutboxState(t, ctx, pool, first.EventID, "", true)
	if err := os.WriteFile(readyFile, []byte("ready"), 0o600); err != nil {
		stopRelay()
		t.Fatal("signal broker restart harness readiness")
	}
	if !waitForFile(ctx, continueFile) {
		stopRelay()
		t.Fatal("timed out waiting for the harness to restart RabbitMQ")
	}

	second := newSubmission("owner-relay-restart", "relay-restart-after", sha256.Sum256([]byte("after RabbitMQ restart")))
	if _, outcome, err := store.CreateQueued(ctx, second); err != nil || outcome != jobs.OutcomeCreated {
		stopRelay()
		t.Fatalf("CreateQueued after broker restart outcome=%v err=%v", outcome, err)
	}
	waitForOutboxState(t, ctx, pool, second.EventID, "", true)
	connection, err := amqp.Dial(brokerDSN)
	if err != nil {
		stopRelay()
		t.Fatal("connect after RabbitMQ restart")
	}
	channel, err := connection.Channel()
	if err != nil {
		_ = connection.Close()
		stopRelay()
		t.Fatal("open RabbitMQ channel after restart")
	}
	firstMessage := getMessageByID(t, ctx, channel, first.EventID)
	secondMessage := getMessageByID(t, ctx, channel, second.EventID)
	assertEnvelopeMessage(t, firstMessage, first.EventID, first.JobID)
	assertEnvelopeMessage(t, secondMessage, second.EventID, second.JobID)
	_ = channel.Close()
	_ = connection.Close()
	stopRelay()
	if err := <-relayDone; err != nil {
		t.Fatalf("relay.Run() after context cancellation = %v", err)
	}
}

func TestPostgresAPIIntakeStartsWhileBrokerUnavailable(t *testing.T) {
	dsn := integrationDSN(t)
	ctx, cancel := context.WithTimeout(t.Context(), 75*time.Second)
	defer cancel()
	pool := openTestPool(t, ctx, dsn)
	apiURL := os.Getenv(testAPIURLEnv)
	parsed, err := url.Parse(apiURL)
	if err != nil || parsed.Scheme != "http" || parsed.Host != "api:8080" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		t.Fatal("integration tests require the isolated Compose API service")
	}
	tokenPath := os.Getenv(testClientTokenFileEnv)
	tokenInfo, err := os.Lstat(tokenPath)
	if err != nil || tokenInfo.Mode()&os.ModeSymlink != 0 || !tokenInfo.Mode().IsRegular() {
		t.Fatal("private integration client token unavailable")
	}
	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil || len(tokenBytes) < 48 || len(tokenBytes) > 128 {
		t.Fatal("private integration client token unavailable")
	}
	token := string(tokenBytes)
	if len(token) < 5 || token[:5] != "pbt1_" {
		t.Fatal("integration client credential has an unexpected format")
	}
	controlRoot := os.Getenv(testImageRootEnv)
	if controlRoot == "" {
		t.Fatal("integration tests require the broker outage control directory")
	}
	readyFile := filepath.Join(controlRoot, "stage4-api-outage-ready")
	continueFile := filepath.Join(controlRoot, "stage4-api-outage-continue")
	_ = os.Remove(readyFile)
	_ = os.Remove(continueFile)
	t.Cleanup(func() {
		_ = os.Remove(readyFile)
		_ = os.Remove(continueFile)
	})

	data := []byte("broker outage synthetic intake fixture")
	key := "stage4-outage-startup"
	client := &http.Client{Timeout: 5 * time.Second}
	var accepted acceptedPayload
	lastStatus := 0
	lastTransportError := ""
	for attempt := 0; attempt < 40; attempt++ {
		request := multipartRequest(t, apiURL+"/v1/passport/jobs", token, key, "image/png", data)
		request = request.WithContext(ctx)
		response, requestErr := client.Do(request)
		if requestErr != nil {
			lastTransportError = fmt.Sprintf("%T", requestErr)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		lastStatus = response.StatusCode
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 16<<10))
		_ = response.Body.Close()
		if readErr != nil {
			t.Fatal("read broker-outage intake response")
		}
		if response.StatusCode == http.StatusAccepted {
			if err := json.Unmarshal(body, &accepted); err != nil {
				t.Fatal("decode broker-outage accepted job")
			}
			break
		}
		if response.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("broker-outage API returned status %d, want 202", response.StatusCode)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if accepted.JobID == "" || accepted.Status != jobs.StatusQueued {
		t.Fatalf("API did not accept durable intake while RabbitMQ was unavailable at startup (last_status=%d, last_transport_error=%s)", lastStatus, lastTransportError)
	}
	var state string
	var publishedAt *time.Time
	var attemptCount int
	if err := pool.QueryRow(ctx, `
		SELECT j.state, j.attempt_count, o.published_at
		FROM jobs AS j JOIN outbox AS o ON o.job_id=j.job_id AND o.attempt_no=1
		WHERE j.job_id=$1`, accepted.JobID).Scan(&state, &attemptCount, &publishedAt); err != nil {
		t.Fatal("read broker-outage intake job and outbox from PostgreSQL")
	}
	if state != string(jobs.StatusQueued) || attemptCount != 0 || publishedAt != nil {
		t.Fatal("broker-outage API response was not backed by one queued, unpublished durable job")
	}
	if err := os.WriteFile(readyFile, []byte("ready"), 0o600); err != nil {
		t.Fatal("signal broker outage harness readiness")
	}
	if !waitForFile(ctx, continueFile) {
		t.Fatal("timed out waiting for RabbitMQ restoration")
	}
	eventID := mustOutboxEventID(t, ctx, pool, accepted.JobID)
	waitForOutboxState(t, ctx, pool, eventID, "", true)
	brokerDSN, _ := integrationBrokerDSN(t)
	connection, err := amqp.Dial(brokerDSN)
	if err != nil {
		t.Fatal("connect to RabbitMQ after broker restoration")
	}
	channel, err := connection.Channel()
	if err != nil {
		_ = connection.Close()
		t.Fatal("open RabbitMQ channel after broker restoration")
	}
	message := getMessageByID(t, ctx, channel, eventID)
	_ = channel.Close()
	_ = connection.Close()
	assertEnvelopeMessage(t, message, eventID, accepted.JobID)
}

func mustOutboxEventID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, jobID string) string {
	t.Helper()
	var eventID string
	if err := pool.QueryRow(ctx, `SELECT event_id::text FROM outbox WHERE job_id=$1 AND attempt_no=1`, jobID).Scan(&eventID); err != nil {
		t.Fatal("read broker-outage job outbox event ID")
	}
	return eventID
}

func TestPostgresAPIQueueToControlledConsumerCompletesOneAttempt(t *testing.T) {
	dsn := integrationDSN(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	store := openIntegrationStore(t, ctx, dsn)
	pool := openTestPool(t, ctx, dsn)
	apiURL := os.Getenv(testAPIURLEnv)
	parsed, err := url.Parse(apiURL)
	if err != nil || parsed.Scheme != "http" || parsed.Host != "api:8080" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		t.Fatal("integration tests require the isolated Compose API service")
	}
	tokenPath := os.Getenv(testClientTokenFileEnv)
	tokenInfo, err := os.Lstat(tokenPath)
	if err != nil || tokenInfo.Mode()&os.ModeSymlink != 0 || !tokenInfo.Mode().IsRegular() {
		t.Fatal("private integration client token unavailable")
	}
	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil || len(tokenBytes) < 48 || len(tokenBytes) > 128 {
		t.Fatal("private integration client token unavailable")
	}
	token := string(tokenBytes)
	if len(token) < 5 || token[:5] != "pbt1_" {
		t.Fatal("integration client credential has an unexpected format")
	}
	data := []byte("controlled OCR synthetic fixture bytes")
	key := "stage4-e2e-controlled-consumer"
	client := &http.Client{Timeout: 10 * time.Second}
	var accepted acceptedPayload
	for attempt := 0; attempt < 30; attempt++ {
		request := multipartRequest(t, apiURL+"/v1/passport/jobs", token, key, "image/png", data)
		request = request.WithContext(ctx)
		response, requestErr := client.Do(request)
		if requestErr != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 16<<10))
		_ = response.Body.Close()
		if readErr != nil {
			t.Fatal("read synthetic async API response")
		}
		if response.StatusCode == http.StatusAccepted {
			if err := json.Unmarshal(body, &accepted); err != nil {
				t.Fatal("decode synthetic accepted job")
			}
			break
		}
		if response.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("synthetic async API returned status %d, want 202", response.StatusCode)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if accepted.JobID == "" || accepted.Status != jobs.StatusQueued || accepted.CreatedAt.IsZero() || accepted.StatusURL != "/v1/passport/jobs/"+accepted.JobID {
		t.Fatal("synthetic async API did not return the queued acceptance envelope")
	}

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var state string
	var pipelineVersion *string
	var errorCode *string
	var attemptCount int
	var resultJSON []byte
	for {
		err := pool.QueryRow(ctx, `
			SELECT state, pipeline_version, error_code, attempt_count, result
			FROM jobs WHERE job_id = $1`, accepted.JobID).Scan(&state, &pipelineVersion, &errorCode, &attemptCount, &resultJSON)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				t.Fatal("read controlled-consumer job from PostgreSQL: no row for accepted job")
			}
			var scanError pgx.ScanArgError
			if errors.As(err, &scanError) {
				t.Fatalf("read controlled-consumer job from PostgreSQL: scan column=%d field=%s cause=%T", scanError.ColumnIndex, scanError.FieldName, scanError.Err)
			}
			t.Fatalf("read controlled-consumer job from PostgreSQL: query error type %T", err)
		}
		if state == string(jobs.StatusCompleted) || state == string(jobs.StatusFailed) {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for controlled OCR job completion")
		case <-ticker.C:
		}
	}
	if state != string(jobs.StatusCompleted) || errorCode != nil || attemptCount != 1 || pipelineVersion == nil || *pipelineVersion != "integration-stage4-controlled" || len(resultJSON) == 0 {
		t.Fatal("controlled consumer did not persist one completed canonical attempt")
	}
	var result api.RecognitionResponse
	if err := json.Unmarshal(resultJSON, &result); err != nil || result.RequestID != accepted.JobID || result.Status != "completed" || result.PipelineVersion != *pipelineVersion || len(result.Fields) != 11 {
		t.Fatal("controlled consumer result is not the existing canonical API response")
	}
	var attemptState string
	if err := pool.QueryRow(ctx, `SELECT state FROM job_attempts WHERE job_id=$1 AND attempt_no=1`, accepted.JobID).Scan(&attemptState); err != nil || attemptState != "completed" {
		t.Fatal("controlled consumer attempt was not durably completed")
	}
	var eventID string
	var publishedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT event_id::text, published_at FROM outbox WHERE job_id=$1 AND attempt_no=1`, accepted.JobID).Scan(&eventID, &publishedAt); err != nil || publishedAt == nil {
		t.Fatal("controlled consumer job lacks a publisher-confirmed outbox event")
	}
	var imageKey string
	var imageSize int64
	if err := pool.QueryRow(ctx, `SELECT image_key::text, image_size_bytes FROM jobs WHERE job_id=$1`, accepted.JobID).Scan(&imageKey, &imageSize); err != nil {
		t.Fatal("read controlled-consumer image metadata")
	}
	imageRoot := os.Getenv("PASSPORT_POSTGRES_TEST_E2E_IMAGE_ROOT")
	imageInfo, err := os.Stat(filepath.Join(imageRoot, imageKey))
	if err != nil || !imageInfo.Mode().IsRegular() || imageInfo.Size() != imageSize || imageSize != int64(len(data)) {
		t.Fatal("controlled-consumer job image is missing or differs from committed metadata")
	}
	_ = eventID // The Rabbit test runner separately checks exact envelope body and MessageId.
	_ = store
}

func TestPostgresAPIQueueCommitBeforeAckCrash(t *testing.T) {
	dsn := integrationDSN(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	if err := openIntegrationStore(t, ctx, dsn).CheckSchema(ctx); err != nil {
		t.Fatal("check isolated PostgreSQL schema")
	}
	pool := openTestPool(t, ctx, dsn)
	apiURL := os.Getenv(testAPIURLEnv)
	if apiURL != "http://api:8080" {
		t.Fatal("integration tests require the isolated Compose API service")
	}
	tokenPath := os.Getenv(testClientTokenFileEnv)
	tokenInfo, err := os.Lstat(tokenPath)
	if err != nil || tokenInfo.Mode()&os.ModeSymlink != 0 || !tokenInfo.Mode().IsRegular() {
		t.Fatal("private integration client token unavailable")
	}
	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil || len(tokenBytes) < 48 || len(tokenBytes) > 128 || string(tokenBytes[:5]) != "pbt1_" {
		t.Fatal("private integration client token unavailable")
	}
	key := "stage4-commit-before-ack"
	data := []byte("synthetic commit-before-ack image")
	request := multipartRequest(t, apiURL+"/v1/passport/jobs", string(tokenBytes), key, "image/png", data)
	request = request.WithContext(ctx)
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatal("POST synthetic commit-before-ACK job")
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 16<<10))
	_ = response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusAccepted {
		t.Fatalf("synthetic commit-before-ACK POST status=%d body_read_ok=%t, want 202", response.StatusCode, readErr == nil)
	}
	var accepted acceptedPayload
	if err := json.Unmarshal(body, &accepted); err != nil || accepted.JobID == "" || accepted.Status != jobs.StatusQueued {
		t.Fatal("synthetic commit-before-ACK response was not a queued acceptance")
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	var state string
	var attemptCount int
	var resultJSON []byte
	var errorCode *string
	for {
		err := pool.QueryRow(ctx, `SELECT state, attempt_count, result, error_code FROM jobs WHERE job_id=$1`, accepted.JobID).
			Scan(&state, &attemptCount, &resultJSON, &errorCode)
		if err != nil {
			t.Fatalf("read commit-before-ACK job state: %v", err)
		}
		if state == string(jobs.StatusCompleted) || state == string(jobs.StatusFailed) {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for durable completion before consumer crash")
		case <-ticker.C:
		}
	}
	if state != string(jobs.StatusCompleted) || attemptCount != 1 || len(resultJSON) == 0 {
		category := "none"
		if errorCode != nil {
			category = *errorCode
		}
		calls, callErr := os.ReadFile(filepath.Join(os.Getenv(testOCRStateEnv), "crash.calls"))
		callCount := 0
		if callErr == nil {
			callCount = len(bytes.Split(bytes.TrimSpace(calls), []byte("\n")))
		}
		t.Fatalf("consumer did not durably complete one attempt before crash: state=%s attempts=%d result_present=%t safe_error_code=%s recognizer_call_count=%d call_marker_readable=%t", state, attemptCount, len(resultJSON) > 0, category, callCount, callErr == nil)
	}
	imageRoot := os.Getenv("PASSPORT_POSTGRES_TEST_IMAGE_ROOT")
	snapshot := map[string]string{
		"job_id":        accepted.JobID,
		"result_sha256": fmt.Sprintf("%x", sha256.Sum256(resultJSON)),
	}
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil || os.WriteFile(filepath.Join(imageRoot, "stage4-ack-crash-snapshot.json"), snapshotJSON, 0o600) != nil {
		t.Fatal("save synthetic commit-before-ACK verification snapshot")
	}
}

func TestPostgresAPIQueueRedeliveryAfterCommitDoesNotReprocess(t *testing.T) {
	dsn := integrationDSN(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool := openTestPool(t, ctx, dsn)
	imageRoot := os.Getenv("PASSPORT_POSTGRES_TEST_IMAGE_ROOT")
	snapshotJSON, err := os.ReadFile(filepath.Join(imageRoot, "stage4-ack-crash-snapshot.json"))
	if err != nil {
		t.Fatal("commit-before-ACK snapshot unavailable")
	}
	var snapshot struct {
		JobID        string `json:"job_id"`
		ResultSHA256 string `json:"result_sha256"`
	}
	if json.Unmarshal(snapshotJSON, &snapshot) != nil || snapshot.JobID == "" || len(snapshot.ResultSHA256) != 64 {
		t.Fatal("commit-before-ACK snapshot invalid")
	}
	var state string
	var attemptCount int
	var resultJSON []byte
	if err := pool.QueryRow(ctx, `SELECT state, attempt_count, result FROM jobs WHERE job_id=$1`, snapshot.JobID).
		Scan(&state, &attemptCount, &resultJSON); err != nil {
		t.Fatal("read redelivered job after consumer restart")
	}
	if state != string(jobs.StatusCompleted) || attemptCount != 1 || fmt.Sprintf("%x", sha256.Sum256(resultJSON)) != snapshot.ResultSHA256 {
		t.Fatal("redelivery changed the committed canonical result or attempt count")
	}
	var attempts, events int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM job_attempts WHERE job_id=$1`, snapshot.JobID).Scan(&attempts); err != nil {
		t.Fatal("count redelivered job attempts")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE job_id=$1`, snapshot.JobID).Scan(&events); err != nil {
		t.Fatal("count redelivered job outbox events")
	}
	if attempts != 1 || events != 1 {
		t.Fatal("redelivery created duplicate attempt or outbox rows")
	}
	stateRoot := os.Getenv(testOCRStateEnv)
	crashCalls, err := os.ReadFile(filepath.Join(stateRoot, "crash.calls"))
	if err != nil || string(crashCalls) != "call\n" {
		t.Fatal("pre-crash consumer did not invoke the synthetic recognizer exactly once")
	}
	if _, err := os.Lstat(filepath.Join(stateRoot, "guard.calls")); err == nil {
		t.Fatal("redelivered terminal job invoked recognition a second time")
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("could not verify absence of repeated synthetic recognition")
	}
	redelivery, err := os.ReadFile(filepath.Join(stateRoot, "guard.redelivered"))
	if err != nil || string(redelivery) != "redelivered\n" {
		t.Fatal("replacement consumer did not receive a broker-marked redelivery")
	}
}

func openIntegrationStore(t *testing.T, ctx context.Context, dsn string) *postgres.Store {
	t.Helper()
	store, err := postgres.Open(ctx, postgres.Config{
		DSN: dsn, ConnectTimeout: 5 * time.Second, MigrationLockTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal("open isolated PostgreSQL integration store")
	}
	t.Cleanup(store.Close)
	if err := store.CheckSchema(ctx); err != nil {
		t.Fatal("check isolated PostgreSQL migration state")
	}
	return store
}

func integrationBrokerDSN(t *testing.T) (string, string) {
	t.Helper()
	source := os.Getenv(testBrokerDSNFileEnv)
	if source == "" {
		t.Fatal("integration tests require PASSPORT_POSTGRES_TEST_BROKER_DSN_FILE")
	}
	info, err := os.Lstat(source)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		t.Fatal("private broker DSN file unavailable")
	}
	data, err := os.ReadFile(source)
	if err != nil || len(data) == 0 || len(data) > 16<<10 {
		t.Fatal("private broker DSN file unavailable")
	}
	parsed, err := amqp.ParseURI(string(data))
	if err != nil || parsed.Host != "rabbitmq" || parsed.Port != 5672 || parsed.Username != "passport_test" || parsed.Vhost != "/" {
		t.Fatal("integration tests require the isolated Compose RabbitMQ broker")
	}
	tmp := t.TempDir()
	if err := os.Chmod(tmp, 0o700); err != nil {
		t.Fatal("prepare private broker test directory")
	}
	file := filepath.Join(tmp, "broker-dsn")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal("prepare private broker test file")
	}
	return string(data), file
}

func newIntegrationRelay(t *testing.T, repo jobs.ExecutionRepository, brokerFile string, lease time.Duration) *outbox.Relay {
	t.Helper()
	relay, err := outbox.New(outbox.Config{
		Repository: repo, BrokerDSNFile: brokerFile,
		ConnectTimeout: 3 * time.Second, PollInterval: 20 * time.Millisecond,
		PublisherLease: lease, PublishTimeout: 300 * time.Millisecond,
		PublishBackoff: 150 * time.Millisecond, ReconnectMin: 50 * time.Millisecond,
		ReconnectMax: 300 * time.Millisecond, ConsumerAckTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal("construct outbox relay with isolated test configuration")
	}
	return relay
}

func waitForOutboxState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, eventID, errorCode string, published bool) {
	t.Helper()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		var actualCode *string
		var publishedAt *time.Time
		err := pool.QueryRow(ctx, `SELECT last_error_code, published_at FROM outbox WHERE event_id = $1`, eventID).Scan(&actualCode, &publishedAt)
		if err != nil {
			t.Fatal("read isolated outbox event state")
		}
		codeMatches := errorCode == "" && actualCode == nil || actualCode != nil && *actualCode == errorCode
		if codeMatches && (publishedAt != nil) == published {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for expected isolated outbox state")
		case <-ticker.C:
		}
	}
}

func getMessageByID(t *testing.T, ctx context.Context, channel *amqp.Channel, eventID string) amqp.Delivery {
	t.Helper()
	if err := channel.Qos(1, 0, false); err != nil {
		t.Fatal("set one-message prefetch for isolated RabbitMQ consumer")
	}
	consumerTag := "passport-integration-" + eventID
	deliveries, err := channel.Consume(outbox.WorkQueueName, consumerTag, false, false, false, false, nil)
	if err != nil {
		var protocolError *amqp.Error
		if errors.As(err, &protocolError) {
			t.Fatalf("consume isolated RabbitMQ work queue failed with AMQP code %d", protocolError.Code)
		}
		t.Fatalf("consume isolated RabbitMQ work queue failed (%T)", err)
	}
	for {
		select {
		case message, open := <-deliveries:
			if !open {
				t.Fatal("isolated RabbitMQ consumer closed before delivery")
			}
			if message.MessageId == eventID {
				if err := message.Ack(false); err != nil {
					t.Fatal("ack isolated test message")
				}
				if err := channel.Cancel(consumerTag, false); err != nil {
					t.Fatal("cancel isolated one-message RabbitMQ consumer")
				}
				return message
			}
			if err := message.Ack(false); err != nil {
				t.Fatal("ack unrelated preexisting test message")
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for RabbitMQ work message")
		}
	}
}

func waitForFile(ctx context.Context, path string) bool {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

func assertEnvelopeMessage(t *testing.T, message amqp.Delivery, eventID, jobID string) {
	t.Helper()
	if message.MessageId != eventID || message.ContentType != "application/json" || message.DeliveryMode != amqp.Persistent {
		t.Fatalf("RabbitMQ message durable properties mismatch: id_matches=%t content_type=%q delivery_mode=%d", message.MessageId == eventID, message.ContentType, message.DeliveryMode)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(message.Body, &body); err != nil || len(body) != 4 {
		t.Fatal("RabbitMQ message body is not the exact four-field JSON envelope")
	}
	var envelope jobs.Envelope
	if err := json.Unmarshal(message.Body, &envelope); err != nil || envelope.SchemaVersion != 1 ||
		envelope.EventID != eventID || envelope.JobID != jobID || envelope.AttemptNo != 1 {
		t.Fatal("RabbitMQ message body differs from the committed outbox envelope")
	}
	for _, forbidden := range []string{"owner_id", "image_key", "image_path", "image_bytes", "ocr_result", "token"} {
		if _, ok := body[forbidden]; ok {
			t.Fatalf("RabbitMQ message contains forbidden field %q", forbidden)
		}
	}
}
