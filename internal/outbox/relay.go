// Package outbox publishes privacy-minimal PostgreSQL outbox envelopes to
// RabbitMQ. PostgreSQL remains authoritative; this package never holds a SQL
// transaction open while performing broker I/O.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"passport-backend/internal/jobs"
)

const (
	ExchangeName       = "passport.jobs"
	WorkQueueName      = "passport.recognition.work"
	WorkRoutingKey     = "recognition.requested"
	DeadExchangeName   = "passport.jobs.dlx"
	DeadQueueName      = "passport.recognition.dead"
	DeadRoutingKey     = "recognition.dead"
	minConsumerTimeout = 5 * time.Minute
	maxSecretBytes     = 16 << 10
)

var (
	ErrInvalidConfig     = errors.New("invalid outbox relay configuration")
	errRelayConfig       = ErrInvalidConfig
	ErrBrokerUnavailable = errors.New("outbox broker unavailable")
	ErrTopologyMismatch  = errors.New("outbox broker topology mismatch")
	ErrRejectedAuth      = errors.New("outbox broker authentication rejected")
)

// Config contains operational settings for the outbox relay. The consumer
// timeout is repeated in the queue declaration so that Go and Python fail on
// incompatible queue arguments instead of silently diverging.
type Config struct {
	Repository         jobs.ExecutionRepository
	BrokerDSNFile      string
	ConnectTimeout     time.Duration
	PollInterval       time.Duration
	PublisherLease     time.Duration
	PublishTimeout     time.Duration
	PublishBackoff     time.Duration
	ReconnectMin       time.Duration
	ReconnectMax       time.Duration
	ConsumerAckTimeout time.Duration
	Logger             *slog.Logger
}

// Relay periodically leases one due outbox row, publishes it as a persistent
// mandatory message, and marks it published only after a positive confirm and
// a successful routing check.
type Relay struct {
	cfg       Config
	brokerDSN string
}

// New validates configuration and reads the broker URI from a private file.
// Errors intentionally omit paths and connection details.
func New(cfg Config) (*Relay, error) {
	if cfg.Repository == nil || cfg.ConnectTimeout <= 0 || cfg.PollInterval <= 0 ||
		cfg.PublisherLease <= 0 || cfg.PublishTimeout <= 0 || cfg.PublishTimeout >= cfg.PublisherLease ||
		cfg.PublishBackoff <= 0 || cfg.ReconnectMin <= 0 || cfg.ReconnectMax < cfg.ReconnectMin ||
		cfg.ConsumerAckTimeout < minConsumerTimeout || cfg.ConsumerAckTimeout.Milliseconds() > int64(^uint32(0)>>1) {
		return nil, ErrInvalidConfig
	}
	secret, err := readPrivateSecret(cfg.BrokerDSNFile)
	if err != nil || !validBrokerDSN(secret) {
		return nil, ErrInvalidConfig
	}
	cfg.BrokerDSNFile = ""
	return &Relay{cfg: cfg, brokerDSN: secret}, nil
}

// Check synchronously verifies broker connectivity and the complete durable
// topology. Applications should call it before becoming ready so a declaration
// mismatch (including the consumer timeout) cannot be hidden by Run's retry
// loop.
func (r *Relay) Check(ctx context.Context) error {
	if r == nil || ctx == nil {
		return ErrInvalidConfig
	}
	checkCtx, cancel := context.WithTimeout(ctx, r.cfg.ConnectTimeout)
	defer cancel()
	conn, err := r.dial()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return classifyBrokerError(err)
	}
	defer func() { _ = conn.Close() }()
	stopClose := make(chan struct{})
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		select {
		case <-checkCtx.Done():
			_ = conn.Close()
		case <-stopClose:
		}
	}()
	defer func() {
		close(stopClose)
		<-closed
	}()
	channel, err := conn.Channel()
	if err != nil {
		return classifyBrokerError(err)
	}
	defer func() { _ = channel.Close() }()
	if err := declareTopology(channel, r.cfg.ConsumerAckTimeout); err != nil {
		return classifyBrokerError(err)
	}
	if err := channel.Confirm(false); err != nil {
		return classifyBrokerError(err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkCtx.Err(); err != nil {
		return ErrBrokerUnavailable
	}
	return nil
}

func classifyBrokerError(err error) error {
	var brokerError *amqp.Error
	if errors.As(err, &brokerError) {
		switch brokerError.Code {
		case 403:
			return ErrRejectedAuth
		case 406:
			return ErrTopologyMismatch
		}
	}
	return ErrBrokerUnavailable
}

// Run owns RabbitMQ connections for its lifetime and retries connection or
// topology failures with bounded exponential backoff. It returns nil on
// context cancellation.
func (r *Relay) Run(ctx context.Context) error {
	if r == nil || ctx == nil {
		return errRelayConfig
	}
	backoff := r.cfg.ReconnectMin
	lastCategory := ""
	for ctx.Err() == nil {
		conn, err := r.dial()
		if err == nil {
			backoff = r.cfg.ReconnectMin
			err = r.runConnection(ctx, conn, func() {
				if lastCategory != "" {
					r.logCategory(slog.LevelInfo, "outbox.reconnected", "broker_connection_restored")
					lastCategory = ""
				}
			})
			_ = conn.Close()
		}
		if ctx.Err() != nil {
			return nil
		}
		category, code := r.failureCategory(err)
		if category != "" && category != lastCategory {
			level := slog.LevelWarn
			if errors.Is(err, ErrTopologyMismatch) || errors.Is(err, ErrRejectedAuth) || errors.Is(err, ErrInvalidConfig) {
				level = slog.LevelError
			}
			r.logCategory(level, category, code)
			lastCategory = category
		}
		if !waitContext(ctx, backoff) {
			return nil
		}
		if backoff < r.cfg.ReconnectMax {
			backoff = min(backoff*2, r.cfg.ReconnectMax)
		}
	}
	return nil
}

func (r *Relay) failureCategory(err error) (string, string) {
	classified := err
	if !errors.Is(err, ErrInvalidConfig) && !errors.Is(err, errRelayConfig) &&
		!errors.Is(err, ErrTopologyMismatch) && !errors.Is(err, ErrRejectedAuth) &&
		!errors.Is(err, ErrBrokerUnavailable) {
		classified = classifyBrokerError(err)
	}
	switch {
	case errors.Is(classified, ErrTopologyMismatch):
		return "outbox.topology_mismatch", "outbox_topology_mismatch"
	case errors.Is(classified, ErrRejectedAuth):
		return "outbox.authentication_rejected", "outbox_authentication_rejected"
	case errors.Is(classified, ErrInvalidConfig), errors.Is(classified, errRelayConfig):
		return "outbox.invalid_config", "outbox_invalid_config"
	default:
		return "outbox.unavailable", "outbox_unavailable"
	}
}

func (r *Relay) logCategory(level slog.Level, category, code string) {
	if r.cfg.Logger == nil {
		return
	}
	r.cfg.Logger.Log(context.Background(), level, category, "event", category, "error_code", code)
}

func (r *Relay) dial() (*amqp.Connection, error) {
	timeout := r.cfg.ConnectTimeout
	return amqp.DialConfig(r.brokerDSN, amqp.Config{
		Dial: func(network, address string) (net.Conn, error) {
			conn, err := net.DialTimeout(network, address, timeout)
			if err != nil {
				return nil, err
			}
			if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
				_ = conn.Close()
				return nil, err
			}
			return conn, nil
		},
	})
}

func (r *Relay) runConnection(ctx context.Context, conn *amqp.Connection, onReady func()) error {
	channel, err := conn.Channel()
	if err != nil {
		return err
	}
	defer func() { _ = channel.Close() }()
	if err := declareTopology(channel, r.cfg.ConsumerAckTimeout); err != nil {
		return err
	}
	if err := channel.Confirm(false); err != nil {
		return err
	}
	if onReady != nil {
		onReady()
	}
	returns := channel.NotifyReturn(make(chan amqp.Return, 1))
	confirms := channel.NotifyPublish(make(chan amqp.Confirmation, 1))
	channelClosed := channel.NotifyClose(make(chan *amqp.Error, 1))
	connectionClosed := conn.NotifyClose(make(chan *amqp.Error, 1))
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()

	for {
		if ctx.Err() != nil {
			return nil
		}
		leased, err := r.cfg.Repository.LeaseOutbox(ctx, 1, r.cfg.PublisherLease)
		if err != nil {
			if !waitContext(ctx, r.cfg.PollInterval) {
				return nil
			}
			continue
		}
		if len(leased) > 1 {
			return errRelayConfig
		}
		if len(leased) == 0 {
			select {
			case <-ctx.Done():
				return nil
			case err := <-channelClosed:
				return err
			case err := <-connectionClosed:
				return err
			case <-ticker.C:
				continue
			}
		}

		item := leased[0]
		if !validOutbox(item) {
			_ = r.cfg.Repository.ReleaseOutbox(ctx, item.Envelope.EventID, item.ClaimToken, "publish_failed", r.cfg.PublishBackoff)
			continue
		}
		published, healthy := r.publish(ctx, conn, channel, returns, confirms, channelClosed, connectionClosed, item.Envelope)
		if !healthy {
			_ = r.cfg.Repository.ReleaseOutbox(ctx, item.Envelope.EventID, item.ClaimToken, "publish_failed", r.cfg.PublishBackoff)
			return errors.New("broker connection unavailable")
		}
		if !published {
			_ = r.cfg.Repository.ReleaseOutbox(ctx, item.Envelope.EventID, item.ClaimToken, "publish_failed", r.cfg.PublishBackoff)
			continue
		}
		// If this commit fails after RabbitMQ confirmed, leave the lease to expire.
		// The next publication may duplicate the event; the consumer is idempotent.
		_ = r.cfg.Repository.MarkOutboxPublished(ctx, item.Envelope.EventID, item.ClaimToken)
	}
}

func (r *Relay) publish(
	parent context.Context,
	conn *amqp.Connection,
	channel *amqp.Channel,
	returns <-chan amqp.Return,
	confirms <-chan amqp.Confirmation,
	channelClosed <-chan *amqp.Error,
	connectionClosed <-chan *amqp.Error,
	envelope jobs.Envelope,
) (published, healthy bool) {
	body, err := json.Marshal(envelope)
	if err != nil {
		return false, true
	}
	ctx, cancel := context.WithTimeout(parent, r.cfg.PublishTimeout)
	defer cancel()

	// amqp091-go's channel write is not interruptible by context once begun.
	// Closing this connection on cancellation bounds a blocked publish and
	// causes uncertain deliveries to be safely retried from the outbox.
	stopClose := make(chan struct{})
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stopClose:
		}
	}()
	defer func() {
		close(stopClose)
		<-closed
	}()

	err = channel.Publish(ExchangeName, WorkRoutingKey, true, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		MessageId:    envelope.EventID,
		Body:         body,
	})
	if err != nil {
		return false, false
	}
	var returned bool
	for {
		select {
		case item, ok := <-returns:
			if !ok {
				return false, false
			}
			if item.MessageId != envelope.EventID {
				return false, false
			}
			returned = true
		case confirmation, ok := <-confirms:
			if !ok {
				return false, false
			}
			// RabbitMQ sends a mandatory basic.return before its confirm. Drain
			// once after the confirm to consume that ordered notification.
			select {
			case item, ok := <-returns:
				if !ok {
					return false, false
				}
				if item.MessageId != envelope.EventID {
					return false, false
				}
				returned = true
			default:
			}
			return confirmation.Ack && !returned, true
		case <-channelClosed:
			return false, false
		case <-connectionClosed:
			return false, false
		case <-ctx.Done():
			return false, false
		}
	}
}

func declareTopology(channel *amqp.Channel, ackTimeout time.Duration) error {
	if err := channel.ExchangeDeclare(ExchangeName, "direct", true, false, false, false, nil); err != nil {
		return err
	}
	if err := channel.ExchangeDeclare(DeadExchangeName, "direct", true, false, false, false, nil); err != nil {
		return err
	}
	if _, err := channel.QueueDeclare(DeadQueueName, true, false, false, false, amqp.Table{"x-queue-type": "quorum"}); err != nil {
		return err
	}
	if err := channel.QueueBind(DeadQueueName, DeadRoutingKey, DeadExchangeName, false, nil); err != nil {
		return err
	}
	if _, err := channel.QueueDeclare(WorkQueueName, true, false, false, false, amqp.Table{
		"x-queue-type":              "quorum",
		"x-single-active-consumer":  true,
		"x-consumer-timeout":        int32(ackTimeout.Milliseconds()),
		"x-delivery-limit":          int32(-1),
		"x-dead-letter-exchange":    DeadExchangeName,
		"x-dead-letter-routing-key": DeadRoutingKey,
	}); err != nil {
		return err
	}
	return channel.QueueBind(WorkQueueName, WorkRoutingKey, ExchangeName, false, nil)
}

func validOutbox(item jobs.LeasedOutbox) bool {
	e := item.Envelope
	return item.EventType == "recognition.requested" && item.StoredSchemaVersion == "v1" && e.SchemaVersion == 1 &&
		canonicalUUIDv4(e.EventID) && canonicalUUIDv4(e.JobID) && e.AttemptNo > 0 && canonicalUUIDv4(item.ClaimToken)
}

func canonicalUUIDv4(value string) bool {
	if len(value) != 36 || strings.ToLower(value) != value || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' || value[14] != '4' {
		return false
	}
	switch value[19] {
	case '8', '9', 'a', 'b':
	default:
		return false
	}
	for i, char := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func validBrokerDSN(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "amqp" && parsed.Scheme != "amqps") || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	_, err = amqp.ParseURI(value)
	return err == nil
}

func readPrivateSecret(path string) (string, error) {
	if path == "" {
		return "", errRelayConfig
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", errRelayConfig
	}
	parentPath, name := filepath.Dir(filepath.Clean(abs)), filepath.Base(abs)
	parent, err := os.Lstat(parentPath)
	if err != nil || !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 || parent.Mode().Perm()&0o077 != 0 {
		return "", errRelayConfig
	}
	root, err := os.OpenRoot(parentPath)
	if err != nil {
		return "", errRelayConfig
	}
	defer func() { _ = root.Close() }()
	rootInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(parent, rootInfo) {
		return "", errRelayConfig
	}
	before, err := root.Lstat(name)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || before.Mode().Perm() != 0o600 || before.Size() > maxSecretBytes {
		return "", errRelayConfig
	}
	file, err := root.Open(name)
	if err != nil {
		return "", errRelayConfig
	}
	defer func() { _ = file.Close() }()
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || after.Mode().Perm() != 0o600 || !os.SameFile(before, after) || after.Size() > maxSecretBytes {
		return "", errRelayConfig
	}
	data, err := io.ReadAll(io.LimitReader(file, maxSecretBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxSecretBytes {
		return "", errRelayConfig
	}
	secret := strings.TrimSpace(string(data))
	if secret == "" || strings.ContainsAny(secret, "\r\n\x00") {
		return "", errRelayConfig
	}
	return secret, nil
}

func waitContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
