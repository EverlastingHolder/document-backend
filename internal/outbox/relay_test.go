package outbox

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"passport-backend/internal/jobs"
)

type repositoryStub struct{}

func (repositoryStub) LeaseOutbox(context.Context, int, time.Duration) ([]jobs.LeasedOutbox, error) {
	return nil, nil
}
func (repositoryStub) MarkOutboxPublished(context.Context, string, string) error { return nil }
func (repositoryStub) ReleaseOutbox(context.Context, string, string, string, time.Duration) error {
	return nil
}
func (repositoryStub) ClaimDelivery(context.Context, jobs.Envelope, time.Duration, jobs.RetryPolicy) (jobs.ClaimResult, error) {
	return jobs.ClaimResult{}, nil
}
func (repositoryStub) PersistCompletion(context.Context, jobs.PersistedCompletion, jobs.RetryPolicy) (jobs.DeliveryDecision, error) {
	return "", nil
}
func (repositoryStub) FailAttempt(context.Context, jobs.Failure, jobs.RetryPolicy) (jobs.DeliveryDecision, error) {
	return "", nil
}
func (repositoryStub) RejectDelivery(context.Context, jobs.Rejection) (jobs.DeliveryDecision, error) {
	return "", nil
}
func (repositoryStub) RecoverExpired(context.Context, int, jobs.RetryPolicy) (int, error) {
	return 0, nil
}

func TestNewRequiresPrivateBrokerURIAndTimeoutRelationships(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(root, "broker-dsn")
	if err := os.WriteFile(secret, []byte("amqp://user:password@rabbitmq:5672/%2f"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := Config{
		Repository: repositoryStub{}, BrokerDSNFile: secret,
		ConnectTimeout: time.Second, PollInterval: time.Second,
		PublisherLease: 10 * time.Second, PublishTimeout: time.Second,
		PublishBackoff: time.Second, ReconnectMin: time.Second, ReconnectMax: 5 * time.Second,
		ConsumerAckTimeout: 5 * time.Minute,
	}
	if _, err := New(base); err != nil {
		t.Fatalf("valid configuration rejected: %v", err)
	}

	cases := []struct {
		name   string
		change func(*Config)
	}{
		{name: "ack timeout below broker guidance", change: func(c *Config) { c.ConsumerAckTimeout = time.Minute }},
		{name: "publish timeout does not fit lease", change: func(c *Config) { c.PublishTimeout = c.PublisherLease }},
		{name: "backoff bounds reversed", change: func(c *Config) { c.ReconnectMin = 6 * time.Second }},
		{name: "dsn with URI query", change: func(c *Config) {
			if err := os.WriteFile(secret, []byte("amqp://user:password@rabbitmq:5672/%2f?heartbeat=0"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			if tc.change != nil {
				tc.change(&cfg)
			}
			if _, err := New(cfg); err == nil {
				t.Fatal("New accepted invalid configuration")
			}
		})
	}
}

func TestReadPrivateSecretRejectsUnsafePermissionsAndSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "secret")
	if err := os.WriteFile(file, []byte("opaque"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readPrivateSecret(file); err != nil || got != "opaque" {
		t.Fatalf("private secret read failed: got %q, err=%v", got, err)
	}
	if err := os.Chmod(file, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateSecret(file); err == nil {
		t.Fatal("readPrivateSecret accepted group-readable secret")
	}
	if err := os.Chmod(file, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateSecret(link); err == nil {
		t.Fatal("readPrivateSecret accepted symlink")
	}
}

func TestValidOutboxEnvelopeRequiresClosedVersionAndCanonicalUUIDs(t *testing.T) {
	item := jobs.LeasedOutbox{
		Envelope:  jobs.Envelope{SchemaVersion: 1, EventID: "019b0000-0000-4000-8000-000000000001", JobID: "019b0000-0000-4000-8000-000000000002", AttemptNo: 1},
		EventType: "recognition.requested", StoredSchemaVersion: "v1", ClaimToken: "019b0000-0000-4000-8000-000000000003",
	}
	if !validOutbox(item) {
		t.Fatal("valid outbox envelope rejected")
	}
	mutations := []func(*jobs.LeasedOutbox){
		func(item *jobs.LeasedOutbox) { item.EventType = "other" },
		func(item *jobs.LeasedOutbox) { item.StoredSchemaVersion = "v2" },
		func(item *jobs.LeasedOutbox) { item.Envelope.SchemaVersion = 2 },
		func(item *jobs.LeasedOutbox) { item.Envelope.EventID = "not-a-uuid" },
		func(item *jobs.LeasedOutbox) { item.Envelope.JobID = "019b0000-0000-4000-8000-00000000000A" },
		func(item *jobs.LeasedOutbox) { item.Envelope.AttemptNo = 0 },
		func(item *jobs.LeasedOutbox) { item.ClaimToken = "" },
	}
	for i, mutate := range mutations {
		candidate := item
		mutate(&candidate)
		if validOutbox(candidate) {
			t.Errorf("mutation %d accepted", i)
		}
	}
}

func TestClassifyBrokerErrorSeparatesTopologyMismatch(t *testing.T) {
	if !errors.Is(classifyBrokerError(&amqp.Error{Code: 406}), ErrTopologyMismatch) {
		t.Fatal("precondition failure was not classified as topology mismatch")
	}
	if !errors.Is(classifyBrokerError(&amqp.Error{Code: 320}), ErrBrokerUnavailable) {
		t.Fatal("connection failure was not classified as broker unavailable")
	}
	if !errors.Is(classifyBrokerError(&amqp.Error{Code: 403}), ErrRejectedAuth) {
		t.Fatal("access refusal was not classified as rejected credentials/permission")
	}
}

func TestRunFailureCategoriesAreSafeAndTransitionBounded(t *testing.T) {
	var output bytes.Buffer
	relay := &Relay{cfg: Config{Logger: slog.New(slog.NewTextHandler(&output, nil))}}
	category, code := relay.failureCategory(&amqp.Error{Code: 403, Reason: "secret-bearing broker detail"})
	if category != "outbox.authentication_rejected" || code != "outbox_authentication_rejected" {
		t.Fatalf("safe classification = %q/%q", category, code)
	}
	relay.logCategory(slog.LevelError, category, code)
	if bytes.Contains(output.Bytes(), []byte("secret-bearing broker detail")) {
		t.Fatal("safe category logging included broker error details")
	}
}
