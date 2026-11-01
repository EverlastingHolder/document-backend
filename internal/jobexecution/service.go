// Package jobexecution owns the private OCR job lifecycle and canonical Go
// result mapping. It is separate from the public HTTP handler and PostgreSQL
// implementation so worker protocol and persistence can be tested independently.
package jobexecution

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"passport-backend/internal/api"
	"passport-backend/internal/jobs"
	"passport-backend/internal/ocr"
	"passport-backend/internal/retention"
)

var ErrInvalidConfig = errors.New("invalid OCR job execution configuration")

type Config struct {
	PipelineVersion     string
	LeaseDuration       time.Duration
	ProcessingTimeout   time.Duration
	CompletionBudget    time.Duration
	RetryPolicy         jobs.RetryPolicy
	TerminalImagePurger retention.JobImagePurger
}

type Service struct {
	repository          jobs.ExecutionRepository
	config              Config
	terminalImagePurger retention.JobImagePurger
}

func New(repository jobs.ExecutionRepository, config Config) (*Service, error) {
	pipelineVersion := strings.TrimSpace(config.PipelineVersion)
	if repository == nil || pipelineVersion == "" || utf8.RuneCountInString(pipelineVersion) > 128 || strings.ContainsRune(pipelineVersion, '\x00') ||
		config.LeaseDuration <= 0 || config.ProcessingTimeout <= 0 || config.CompletionBudget <= 0 ||
		config.LeaseDuration <= config.ProcessingTimeout ||
		config.LeaseDuration-config.ProcessingTimeout <= config.CompletionBudget ||
		config.LeaseDuration.Microseconds() <= 0 || config.ProcessingTimeout.Milliseconds() <= 0 ||
		config.CompletionBudget.Milliseconds() <= 0 || !validRetryPolicy(config.RetryPolicy) {
		return nil, ErrInvalidConfig
	}
	config.PipelineVersion = pipelineVersion
	return &Service{repository: repository, config: config, terminalImagePurger: config.TerminalImagePurger}, nil
}

func (s *Service) Claim(ctx context.Context, envelope jobs.Envelope) (jobs.ClaimResult, error) {
	if s == nil || s.repository == nil || ctx == nil {
		return jobs.ClaimResult{}, jobs.ErrInvalidDelivery
	}
	result, err := s.repository.ClaimDelivery(ctx, envelope, s.config.LeaseDuration, s.config.RetryPolicy)
	if err != nil || result.Decision != jobs.DecisionExecute {
		if result.Decision == jobs.DecisionWait && result.RetryAfter > 0 {
			result.RetryAfterMS = int64(result.RetryAfter / time.Millisecond)
			if result.RetryAfter%time.Millisecond != 0 {
				result.RetryAfterMS++
			}
		}
		return result, err
	}
	if result.Execution == nil {
		return jobs.ClaimResult{}, jobs.ErrUnavailable
	}
	if result.Execution.StartedAt.IsZero() || !result.Execution.LeaseExpiresAt.After(result.Execution.StartedAt) {
		return jobs.ClaimResult{}, jobs.ErrUnavailable
	}
	result.Execution.PipelineVersion = s.config.PipelineVersion
	result.Execution.HardProcessingTimeoutMS = s.config.ProcessingTimeout.Milliseconds()
	result.Execution.CompletionBudgetMS = s.config.CompletionBudget.Milliseconds()
	result.Execution.DeadlineAt = result.Execution.StartedAt.Add(s.config.ProcessingTimeout)
	return result, nil
}

// CompleteRaw validates the worker's exact field set, adapts it to the shared
// internal extraction type, and persists the existing canonical API response.
func (s *Service) CompleteRaw(ctx context.Context, completion jobs.Completion) (jobs.DeliveryDecision, error) {
	if s == nil || s.repository == nil || ctx == nil {
		return "", jobs.ErrInvalidDelivery
	}
	extraction, err := ocr.RussianFieldsExtraction(completion.Envelope.JobID, completion.Fields)
	if err != nil {
		return s.Fail(ctx, jobs.Failure{
			Envelope: completion.Envelope, AttemptToken: completion.AttemptToken,
			ErrorCode: "processing_failed",
		})
	}
	response, err := api.MapExtraction(extraction, completion.Envelope.JobID, s.config.PipelineVersion)
	if err != nil {
		return s.Fail(ctx, jobs.Failure{
			Envelope: completion.Envelope, AttemptToken: completion.AttemptToken,
			ErrorCode: "processing_failed",
		})
	}
	canonical, err := json.Marshal(response)
	if err != nil {
		return "", jobs.ErrUnavailable
	}
	decision, persistErr := s.repository.PersistCompletion(ctx, jobs.PersistedCompletion{
		Envelope: completion.Envelope, AttemptToken: completion.AttemptToken,
		PipelineVersion: s.config.PipelineVersion, CanonicalJSON: canonical,
	}, s.config.RetryPolicy)
	if persistErr == nil && decision == jobs.DecisionAck {
		s.purgeTerminalImage(ctx, completion.Envelope.JobID)
	}
	return decision, persistErr
}

func (s *Service) Fail(ctx context.Context, failure jobs.Failure) (jobs.DeliveryDecision, error) {
	if s == nil || s.repository == nil || ctx == nil {
		return "", jobs.ErrInvalidDelivery
	}
	decision, err := s.repository.FailAttempt(ctx, failure, s.config.RetryPolicy)
	if err == nil && decision == jobs.DecisionAck {
		s.purgeTerminalImage(ctx, failure.Envelope.JobID)
	}
	return decision, err
}

func (s *Service) Reject(ctx context.Context, rejection jobs.Rejection) (jobs.DeliveryDecision, error) {
	if s == nil || s.repository == nil || ctx == nil {
		return "", jobs.ErrInvalidDelivery
	}
	decision, err := s.repository.RejectDelivery(ctx, rejection)
	if err == nil && decision == jobs.DecisionAck {
		s.purgeTerminalImage(ctx, rejection.Envelope.JobID)
	}
	return decision, err
}

func (s *Service) RecoverExpired(ctx context.Context, limit int) (int, error) {
	if s == nil || s.repository == nil || ctx == nil {
		return 0, ErrInvalidConfig
	}
	count, err := s.repository.RecoverExpired(ctx, limit, s.config.RetryPolicy)
	if err == nil && count > 0 && s.terminalImagePurger != nil {
		_, _ = s.terminalImagePurger.PurgeTerminalImages(ctx, limit)
	}
	return count, err
}

func (s *Service) purgeTerminalImage(ctx context.Context, jobID string) {
	if s.terminalImagePurger != nil {
		_ = s.terminalImagePurger.PurgeJobImage(ctx, jobID)
	}
}

func validRetryPolicy(policy jobs.RetryPolicy) bool {
	return policy.MaxAttempts > 0 && policy.InitialBackoff > 0 && policy.MaxBackoff >= policy.InitialBackoff
}
