package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"passport-backend/internal/clientauth"
	"passport-backend/internal/clientenrollment"
	"passport-backend/internal/httpapi"
	"passport-backend/internal/imagestore"
	"passport-backend/internal/jobexecution"
	"passport-backend/internal/jobintake"
	"passport-backend/internal/jobs"
	"passport-backend/internal/jobstatus"
	"passport-backend/internal/jobstore/postgres"
	"passport-backend/internal/metrics"
	"passport-backend/internal/outbox"
	"passport-backend/internal/retention"
	"passport-backend/internal/server"
	"passport-backend/internal/telemetry"
	"passport-backend/internal/workerapi"
)

const maxDatabaseDSNBytes = 16 << 10

type asyncRuntimeResources struct {
	publicConfig *httpapi.AsyncJobsConfig
	private      runner
	background   []runner
	cleanup      func() error
	readiness    func(context.Context) error
}

func openAsyncJobs(
	ctx context.Context,
	config *asyncRuntimeConfig,
	maxUploadBytes int64,
	metricsStore *metrics.Store,
	logger *slog.Logger,
	serverConfig server.Config,
) (*asyncRuntimeResources, error) {
	resources := &asyncRuntimeResources{cleanup: func() error { return nil }}
	if config == nil || !config.Enabled {
		return resources, nil
	}
	var err error
	var registry *clientauth.Registry
	if config.CredentialsFile != "" {
		registry, err = clientauth.Load(config.CredentialsFile)
		if err != nil {
			return nil, errors.New("client credential registry unavailable")
		}
	}
	dsn, err := readPrivateDatabaseDSN(config.DatabaseDSNFile)
	if err != nil {
		return nil, errors.New("database connection configuration unavailable")
	}
	store, err := postgres.Open(ctx, postgres.Config{
		DSN: dsn, ConnectTimeout: config.ConnectTimeout,
		MigrationLockTimeout: config.MigrationLockTimeout,
	})
	if err != nil {
		return nil, errors.New("job database unavailable")
	}
	closeStore := func() { store.Close() }
	checkCtx, cancel := context.WithTimeout(ctx, config.ConnectTimeout)
	err = store.CheckSchema(checkCtx)
	cancel()
	if err != nil {
		closeStore()
		return nil, errors.New("job database schema is not ready")
	}
	images, err := imagestore.New(imagestore.Config{
		Root: config.ImageRoot, MaxBytes: maxUploadBytes, MaxTotalBytes: config.MaxStoredImageBytes,
	})
	if err != nil {
		closeStore()
		return nil, errors.New("job image storage unavailable")
	}
	resources.readiness = func(checkCtx context.Context) error {
		if err := store.Ping(checkCtx); err != nil {
			return err
		}
		return images.CheckAvailable(checkCtx)
	}
	closeResources := func() error {
		imageErr := images.Close()
		store.Close()
		return imageErr
	}
	acceptor, err := jobintake.New(store, images)
	if err != nil {
		_ = closeResources()
		return nil, errors.New("job intake unavailable")
	}
	retentionService, err := retention.New(store, images, retention.Config{
		ClaimLease: config.Retention.ClaimLease, Interval: config.Retention.Interval,
		OperationTimeout: config.Retention.OperationTimeout, BatchSize: config.Retention.BatchSize,
		Logger: logger.With("component", "retention"),
	})
	if err != nil {
		_ = closeResources()
		return nil, errors.New("job retention unavailable")
	}
	authenticator, err := clientauth.NewAuthenticator(registry, store)
	if err != nil {
		_ = closeResources()
		return nil, errors.New("client authentication unavailable")
	}
	var enrollment httpapi.ClientInstallationRegistrar
	if config.ClientEnrollment != nil {
		enrollment, err = clientenrollment.New(store, registry, config.ClientEnrollment.MaxClients, config.ClientEnrollment.MaxNewClientsPerMinute)
		if err != nil {
			_ = closeResources()
			return nil, errors.New("client enrollment unavailable")
		}
	}
	resources.publicConfig = newAsyncPublicConfig(authenticator, acceptor, store, config.MaxConcurrentUploads, serverConfig.ReadTimeout, enrollment)
	resources.background = append(resources.background, retentionService)
	resources.cleanup = closeResources

	if config.QueueProcessing == nil {
		return resources, nil
	}
	queueConfig := config.QueueProcessing
	workerRegistry, err := clientauth.LoadWorker(queueConfig.WorkerCredentialsFile)
	if err != nil {
		_ = resources.cleanup()
		return nil, errors.New("worker credential registry unavailable")
	}
	if executionRepository, ok := any(store).(jobs.ExecutionRepository); !ok {
		_ = resources.cleanup()
		return nil, errors.New("job execution storage unavailable")
	} else {
		executionService, serviceErr := jobexecution.New(executionRepository, jobexecution.Config{
			PipelineVersion: config.PipelineVersion, LeaseDuration: queueConfig.AttemptLease,
			ProcessingTimeout: queueConfig.HardProcessingTimeout, CompletionBudget: queueConfig.CompletionBudget,
			TerminalImagePurger: retentionService,
			RetryPolicy: jobs.RetryPolicy{MaxAttempts: queueConfig.MaxAttempts,
				InitialBackoff: queueConfig.RetryInitialDelay, MaxBackoff: queueConfig.RetryMaxDelay},
		})
		if serviceErr != nil {
			_ = resources.cleanup()
			return nil, errors.New("job execution service unavailable")
		}
		workerHandler, handlerErr := workerapi.NewHandler(workerapi.Config{
			Authenticator: workerRegistry, Executor: executionService, Metrics: metricsStore,
			Logger: logger.With("component", "workerapi"), WaitPollInterval: queueConfig.PollInterval,
		})
		if handlerErr != nil {
			_ = resources.cleanup()
			return nil, errors.New("worker API unavailable")
		}
		privateServer, privateErr := server.NewPrivateServer(server.PrivateConfig{
			Address: queueConfig.PrivateListenerAddress, ReadHeaderTimeout: serverConfig.ReadHeaderTimeout,
			ReadTimeout: serverConfig.ReadTimeout, WriteTimeout: serverConfig.WriteTimeout,
			IdleTimeout: serverConfig.IdleTimeout, ShutdownTimeout: serverConfig.ShutdownTimeout,
			MaxHeaderBytes: serverConfig.MaxHeaderBytes, Logger: logger.With("component", "server"),
		}, workerHandler)
		if privateErr != nil {
			_ = resources.cleanup()
			return nil, errors.New("private worker listener configuration invalid")
		}
		broker, relayErr := outbox.New(outbox.Config{
			Repository: executionRepository, BrokerDSNFile: queueConfig.BrokerDSNFile,
			ConnectTimeout: queueConfig.ConnectTimeout, PollInterval: queueConfig.PollInterval,
			PublisherLease: queueConfig.PublisherLease, PublishTimeout: queueConfig.PublishTimeout,
			PublishBackoff: queueConfig.PublishBackoff, ReconnectMin: queueConfig.ReconnectMin,
			ReconnectMax: queueConfig.ReconnectMax, ConsumerAckTimeout: queueConfig.ConsumerAckTimeout,
			Logger: logger.With("component", "outbox"),
		})
		if relayErr != nil {
			if errors.Is(relayErr, outbox.ErrInvalidConfig) {
				logger.With("component", "outbox").Error("outbox.invalid_config", "event", "outbox.invalid_config", "error_code", "outbox_invalid_config")
			}
			_ = resources.cleanup()
			return nil, errors.New("outbox configuration unavailable")
		}
		checkCtx, cancel := context.WithTimeout(ctx, queueConfig.ConnectTimeout)
		relayErr = broker.Check(checkCtx)
		cancel()
		relayErr = handleOutboxCheckResult(relayErr, logger)
		if relayErr != nil {
			_ = resources.cleanup()
			return nil, errors.New("outbox startup check failed")
		}
		resources.private = privateServer
		resources.background = append(resources.background,
			broker,
			&recoveryRunner{service: executionService, interval: queueConfig.RecoveryInterval, logger: logger.With("component", "job_recovery")},
		)
	}
	return resources, nil
}

func newAsyncPublicConfig(authenticator httpapi.AsyncAuthenticator, acceptor httpapi.AsyncAcceptor, repository jobs.StatusRepository, maxConcurrentUploads int, statusReadTimeout time.Duration, enrollment ...httpapi.ClientInstallationRegistrar) *httpapi.AsyncJobsConfig {
	config := &httpapi.AsyncJobsConfig{
		Authenticator: authenticator, Acceptor: acceptor,
		StatusReader: jobstatus.New(repository), StatusReadTimeout: statusReadTimeout,
		MaxConcurrentUploads: maxConcurrentUploads,
	}
	if acknowledger, ok := any(repository).(jobs.ReceiptRepository); ok {
		config.ReceiptAcknowledger = acknowledger
	}
	if len(enrollment) > 0 {
		config.ClientEnrollment = enrollment[0]
	}
	return config
}

func handleOutboxCheckResult(err error, logger *slog.Logger) error {
	if err == nil {
		return nil
	}
	outboxLogger := telemetry.LoggerOrDiscard(logger).With("component", "outbox")
	if errors.Is(err, outbox.ErrBrokerUnavailable) {
		outboxLogger.Warn("outbox.unavailable", "event", "outbox.unavailable", "error_code", "outbox_unavailable")
		return nil
	}
	if errors.Is(err, outbox.ErrTopologyMismatch) {
		outboxLogger.Error("outbox.topology_mismatch", "event", "outbox.topology_mismatch", "error_code", "outbox_topology_mismatch")
		return outbox.ErrTopologyMismatch
	}
	if errors.Is(err, outbox.ErrRejectedAuth) {
		outboxLogger.Error("outbox.authentication_rejected", "event", "outbox.authentication_rejected", "error_code", "outbox_authentication_rejected")
		return outbox.ErrRejectedAuth
	}
	if errors.Is(err, outbox.ErrInvalidConfig) {
		outboxLogger.Error("outbox.invalid_config", "event", "outbox.invalid_config", "error_code", "outbox_invalid_config")
		return outbox.ErrInvalidConfig
	}
	return err
}

type recoveryService interface {
	RecoverExpired(context.Context, int) (int, error)
}

type recoveryRunner struct {
	service  recoveryService
	interval time.Duration
	logger   *slog.Logger
}

func (r *recoveryRunner) Run(ctx context.Context) error {
	if ctx == nil || r.service == nil || r.interval <= 0 {
		return errors.New("invalid recovery runner configuration")
	}
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			recoveryCtx, cancel := context.WithTimeout(ctx, r.interval)
			_, err := r.service.RecoverExpired(recoveryCtx, 100)
			cancel()
			if err != nil && ctx.Err() == nil {
				r.logger.Warn("job.recovery.failed", "event", "job.recovery.failed", "error_code", "job_storage_unavailable")
			}
		}
	}
}

func readPrivateDatabaseDSN(path string) (string, error) {
	return readPrivateValue(path, maxDatabaseDSNBytes)
}

func readPrivateValue(path string, maxBytes int64) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", errors.New("invalid private configuration path")
	}
	absolute = filepath.Clean(absolute)
	parentPath, name := filepath.Dir(absolute), filepath.Base(absolute)
	parent, err := os.Lstat(parentPath)
	if err != nil || !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 || parent.Mode().Perm()&0o077 != 0 {
		return "", errors.New("unsafe private configuration directory")
	}
	root, err := os.OpenRoot(parentPath)
	if err != nil {
		return "", errors.New("private configuration unavailable")
	}
	defer func() { _ = root.Close() }()
	rootInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(parent, rootInfo) {
		return "", errors.New("unsafe private configuration directory")
	}
	before, err := root.Lstat(name)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || before.Mode().Perm() != 0o600 || before.Size() > maxBytes {
		return "", errors.New("unsafe private configuration file")
	}
	file, err := root.Open(name)
	if err != nil {
		return "", errors.New("private configuration unavailable")
	}
	defer func() { _ = file.Close() }()
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || after.Mode().Perm() != 0o600 || !os.SameFile(before, after) || after.Size() > maxBytes {
		return "", errors.New("unsafe private configuration file")
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || len(contents) == 0 || int64(len(contents)) > maxBytes {
		return "", errors.New("private configuration unavailable")
	}
	value := strings.TrimSpace(string(contents))
	if value == "" {
		return "", errors.New("private configuration unavailable")
	}
	return value, nil
}

var _ jobs.ExecutionRepository = (*postgres.Store)(nil)
var _ runner = (*outbox.Relay)(nil)
var _ runner = (*server.PrivateServer)(nil)
var _ runner = (*recoveryRunner)(nil)
