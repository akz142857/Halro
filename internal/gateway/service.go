package gateway

import (
	"bytes"
	"context"
	"crypto/hmac"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"sync/atomic"
	"time"

	"github.com/akz142857/Heimdall/internal/auth"
	"github.com/akz142857/Heimdall/internal/budget"
	"github.com/akz142857/Heimdall/internal/circuit"
	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/id"
	"github.com/akz142857/Heimdall/internal/limiter"
	"github.com/akz142857/Heimdall/internal/openaiapi"
	"github.com/akz142857/Heimdall/internal/provider"
	"github.com/akz142857/Heimdall/internal/redaction"
	"github.com/akz142857/Heimdall/internal/requestmeta"
	"github.com/akz142857/Heimdall/internal/semantic"
	"github.com/akz142857/Heimdall/internal/tokenguard"
)

const (
	defaultMaximumOutputTokens = int64(1024)
	cleanupTimeout             = 10 * time.Second
	maxAccountableTokens       = int64(1_000_000_000_000)
)

var errDeploymentConcurrency = errors.New("deployment concurrency limit exceeded")

type Error struct {
	Code       string
	Message    string
	HTTPStatus int
	RetryAfter time.Duration
	Cause      error
}

func (e *Error) Error() string {
	return e.Message
}

func (e *Error) Unwrap() error {
	return e.Cause
}

type Service struct {
	auth                  *auth.Snapshot
	registry              *provider.Registry
	accounting            *budget.Manager
	limiter               *limiter.Manager
	redactor              *redaction.Engine
	breakers              *circuit.Manager
	maxAttempts           int
	maxAttemptsPerTarget  int
	retryBaseDelay        time.Duration
	retryMaxDelay         time.Duration
	retryJitter           bool
	tokenGuard            *tokenguard.Manager
	providerConcurrency   *provider.ConcurrencyManager
	deploymentConcurrency *provider.ConcurrencyManager
	rejections            rejectionCounters
	sourceHashKey         [32]byte
	now                   func() time.Time
}

func NewService(authSnapshot *auth.Snapshot, registry *provider.Registry, accounting *budget.Manager) (*Service, error) {
	return NewServiceWithOptions(authSnapshot, registry, accounting, ServiceOptions{})
}

type ServiceOptions struct {
	MaxAttempts                int
	CircuitFailureThreshold    int
	CircuitOpenDuration        time.Duration
	CircuitHalfOpenMaxRequests int
	MaxAttemptsPerTarget       int
	RetryBaseDelay             time.Duration
	RetryMaxDelay              time.Duration
	RetryJitter                bool
	TokenGuard                 *tokenguard.Manager
	Redactor                   *redaction.Engine
}

type RejectionMetrics struct {
	RPM                   uint64
	TPM                   uint64
	ProjectConcurrency    uint64
	ProviderConcurrency   uint64
	DeploymentConcurrency uint64
	Budget                uint64
	TokenGuard            uint64
}

type rejectionCounters struct {
	rpm                   atomic.Uint64
	tpm                   atomic.Uint64
	projectConcurrency    atomic.Uint64
	providerConcurrency   atomic.Uint64
	deploymentConcurrency atomic.Uint64
	budget                atomic.Uint64
	tokenGuard            atomic.Uint64
}

func NewServiceWithOptions(
	authSnapshot *auth.Snapshot,
	registry *provider.Registry,
	accounting *budget.Manager,
	options ServiceOptions,
) (*Service, error) {
	if authSnapshot == nil || registry == nil || accounting == nil {
		return nil, errors.New("auth snapshot, provider registry, and accounting manager are required")
	}
	if options.MaxAttempts <= 0 {
		options.MaxAttempts = 3
	}
	if options.CircuitFailureThreshold <= 0 {
		options.CircuitFailureThreshold = 5
	}
	if options.CircuitOpenDuration <= 0 {
		options.CircuitOpenDuration = 30 * time.Second
	}
	if options.CircuitHalfOpenMaxRequests <= 0 {
		options.CircuitHalfOpenMaxRequests = 1
	}
	if options.MaxAttemptsPerTarget <= 0 {
		options.MaxAttemptsPerTarget = 2
	}
	if options.RetryBaseDelay <= 0 {
		options.RetryBaseDelay = 100 * time.Millisecond
	}
	if options.RetryMaxDelay < options.RetryBaseDelay {
		options.RetryMaxDelay = 2 * time.Second
	}
	breakers, err := circuit.New(circuit.Config{
		FailureThreshold:    options.CircuitFailureThreshold,
		OpenDuration:        options.CircuitOpenDuration,
		HalfOpenMaxRequests: options.CircuitHalfOpenMaxRequests,
	})
	if err != nil {
		return nil, err
	}
	if options.TokenGuard == nil {
		options.TokenGuard, err = tokenguard.New(nil)
		if err != nil {
			return nil, err
		}
	}
	if options.Redactor == nil {
		options.Redactor = redaction.NewDefault()
	}
	var sourceHashKey [32]byte
	if _, err := cryptorand.Read(sourceHashKey[:]); err != nil {
		return nil, errors.New("generate source hashing key")
	}
	return &Service{
		auth:                  authSnapshot,
		registry:              registry,
		accounting:            accounting,
		limiter:               limiter.New(),
		redactor:              options.Redactor,
		breakers:              breakers,
		maxAttempts:           options.MaxAttempts,
		maxAttemptsPerTarget:  options.MaxAttemptsPerTarget,
		retryBaseDelay:        options.RetryBaseDelay,
		retryMaxDelay:         options.RetryMaxDelay,
		retryJitter:           options.RetryJitter,
		tokenGuard:            options.TokenGuard,
		providerConcurrency:   provider.NewConcurrencyManager(),
		deploymentConcurrency: provider.NewConcurrencyManager(),
		sourceHashKey:         sourceHashKey,
		now:                   time.Now,
	}, nil
}

func (s *Service) RejectionMetrics() RejectionMetrics {
	return RejectionMetrics{
		RPM: s.rejections.rpm.Load(), TPM: s.rejections.tpm.Load(),
		ProjectConcurrency:    s.rejections.projectConcurrency.Load(),
		ProviderConcurrency:   s.rejections.providerConcurrency.Load(),
		DeploymentConcurrency: s.rejections.deploymentConcurrency.Load(),
		Budget:                s.rejections.budget.Load(), TokenGuard: s.rejections.tokenGuard.Load(),
	}
}

func (s *Service) ActiveProviderRequests() map[string]int64 {
	return s.providerConcurrency.Active()
}

func (s *Service) ActiveDeploymentRequests() map[string]int64 {
	return s.deploymentConcurrency.Active()
}

func providerConcurrencyKey(target provider.Target) string {
	if target.ProviderID != "" {
		return target.ProviderID
	}
	return target.ID
}

type targetConcurrencyLease struct {
	provider   *provider.ConcurrencyLease
	deployment *provider.ConcurrencyLease
}

func (lease *targetConcurrencyLease) Release() {
	if lease == nil {
		return
	}
	lease.deployment.Release()
	lease.provider.Release()
}

func (s *Service) acquireTargetConcurrency(target provider.Target) (*targetConcurrencyLease, error) {
	providerLease, err := s.providerConcurrency.Acquire(providerConcurrencyKey(target), target.MaxConcurrency)
	if err != nil {
		return nil, err
	}
	lease := &targetConcurrencyLease{provider: providerLease}
	if target.DeploymentID == "" {
		return lease, nil
	}
	lease.deployment, err = s.deploymentConcurrency.Acquire(target.DeploymentID, target.DeploymentConcurrency)
	if err != nil {
		providerLease.Release()
		return nil, fmt.Errorf("%w: %v", errDeploymentConcurrency, err)
	}
	return lease, nil
}

func (s *Service) Chat(
	ctx context.Context,
	plaintextKey string,
	request openaiapi.ChatCompletionRequest,
) (openaiapi.ChatCompletionResponse, error) {
	if request.Stream {
		return openaiapi.ChatCompletionResponse{}, gatewayError("unsupported_feature", "streaming is not available yet", 400, nil)
	}
	principal, err := s.auth.Authenticate(plaintextKey, s.now())
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, gatewayError("invalid_api_key", "invalid API key", 401, err)
	}
	if !slices.Contains(principal.Project.AllowedRoutes, request.Model) {
		return openaiapi.ChatCompletionResponse{}, gatewayError("model_not_allowed", "model is not allowed for this project", 403, nil)
	}
	if err := authorizeSource(ctx, principal.Project); err != nil {
		return openaiapi.ChatCompletionResponse{}, err
	}
	targets := s.registry.ResolveCandidatesFor(request.Model, provider.OperationChat)
	if len(targets) == 0 {
		if len(s.registry.ResolveAll(request.Model)) > 0 {
			return openaiapi.ChatCompletionResponse{}, gatewayError("unsupported_feature", "model route does not support chat completions", 400, nil)
		}
		return openaiapi.ChatCompletionResponse{}, gatewayError("model_not_found", "model route was not found", 404, nil)
	}
	targets = filterChatCapabilities(targets, request)
	if len(targets) == 0 {
		return openaiapi.ChatCompletionResponse{}, gatewayError("unsupported_feature", "model route does not support the requested chat capabilities", 400, nil)
	}
	request, err = s.redactor.ProcessInboundChat(principal.Project.RedactionPolicyID, request)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, gatewayError("sensitive_data_detected", "request contains secret material", 400, err)
	}
	inputTokens := estimateInputTokens(request.EstimatedInputBytes())
	if principal.Project.MaxInputTokens > 0 && inputTokens > principal.Project.MaxInputTokens {
		return openaiapi.ChatCompletionResponse{}, gatewayError("token_limit_exceeded", "estimated input tokens exceed the project limit", 400, nil)
	}
	outputTokens, err := requestedOutputTokens(request, principal.Project.MaxOutputTokens)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, err
	}
	totalTokens, err := addTokens(inputTokens, outputTokens)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, gatewayError("token_limit_exceeded", "requested token count is too large", 400, err)
	}
	targets = filterTokenCapabilities(targets, inputTokens, outputTokens)
	if len(targets) == 0 {
		return openaiapi.ChatCompletionResponse{}, gatewayError("token_limit_exceeded", "request exceeds the model deployment token limits", 400, nil)
	}
	if err := s.admitTokenGuard(ctx, principal, targets, totalTokens, inputTokens, outputTokens); err != nil {
		return openaiapi.ChatCompletionResponse{}, err
	}
	providerCalled := false
	providerFailed := false
	defer func() {
		if providerCalled {
			s.tokenGuard.Complete(
				principal.Project.TokenGuardPolicyID,
				principal.Project.ID,
				principal.Key.ID,
				s.now(),
				providerFailed,
			)
		}
	}()
	policyLease, err := s.limiter.Acquire(principal.Project, totalTokens, s.now())
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, s.mapLimitError(err)
	}
	actualTPMTokens := int64(0)
	defer func() {
		_ = policyLease.Reconcile(actualTPMTokens, s.now())
		policyLease.Release()
	}()
	requestID, err := id.New("req")
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, gatewayError("internal_error", "unable to create request ID", 500, err)
	}
	requestLease, err := s.accounting.BeginRequestDetailed(
		ctx, principal.Project.ID, principal.Key.ID, requestID, request.Model,
	)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, gatewayError("accounting_unavailable", "accounting is unavailable", 503, err)
	}
	var lastErr error
	attemptCount := 0
	for targetIndex, target := range targets {
		for targetTry := 0; targetTry < s.maxAttemptsPerTarget && attemptCount < s.maxAttempts; targetTry++ {
			if ctx.Err() != nil {
				lastErr = ctx.Err()
				break
			}
			if attemptCount > 0 {
				if err := s.waitRetry(ctx, attemptCount-1); err != nil {
					lastErr = err
					break
				}
			}
			breakerLease, err := s.breakers.Acquire(target.ID, s.now())
			if err != nil {
				lastErr = err
				break
			}
			providerLease, err := s.acquireTargetConcurrency(target)
			if err != nil {
				breakerLease.Done(nil, s.now())
				if errors.Is(err, errDeploymentConcurrency) {
					s.rejections.deploymentConcurrency.Add(1)
				} else {
					s.rejections.providerConcurrency.Add(1)
				}
				lastErr = err
				break
			}
			reservation, err := estimateReservation(inputTokens, outputTokens, target)
			if err != nil {
				providerLease.Release()
				breakerLease.Done(nil, s.now())
				if !errors.Is(err, budget.ErrExceeded) {
					finalizeErr := s.finalizeRequest(requestLease, "accounting_error")
					return openaiapi.ChatCompletionResponse{}, gatewayError(
						"accounting_unavailable", "accounting is unavailable", 503,
						errors.Join(err, finalizeErr),
					)
				}
				lastErr = err
				break
			}
			attempt, err := s.accounting.ReserveAttemptDetailed(
				ctx, requestLease, principal.Project.DailyBudgetMicrosUSD, reservation,
				budget.AttemptMetadata{
					RouteID: target.ID, DeploymentID: target.DeploymentID,
					ProviderID: target.ProviderID, ProviderModel: target.ProviderModel,
					AttemptNumber: attemptCount + 1, RetryCount: targetTry, FallbackCount: targetIndex,
				},
			)
			if err != nil {
				providerLease.Release()
				breakerLease.Done(nil, s.now())
				lastErr = err
				break
			}
			attemptCount++
			if err := s.accounting.MarkStarted(ctx, attempt); err != nil {
				providerLease.Release()
				breakerLease.Done(nil, s.now())
				cleanupErr := s.settleAttempt(attempt, budget.Settlement{Outcome: "start_failed"})
				finalizeErr := s.finalizeRequest(requestLease, "accounting_error")
				return openaiapi.ChatCompletionResponse{}, gatewayError(
					"accounting_unavailable", "accounting is unavailable", 503,
					errors.Join(err, cleanupErr, finalizeErr),
				)
			}
			providerCalled = true
			startedAt := s.now()
			response, providerErr := target.Adapter.Chat(ctx, provider.ChatCall{
				RequestID: requestID, ProviderModel: target.ProviderModel, Request: request,
			})
			providerLease.Release()
			providerFailed = providerErr != nil
			settlement := settlementForResult(
				response, providerErr, inputTokens, outputTokens, target, attempt.ReservationMicrosUSD,
			)
			actualTPMTokens = accumulateTPMTokens(actualTPMTokens, settlement)
			enrichSettlement(&settlement, providerErr, startedAt, s.now())
			if err := s.settleAttempt(attempt, settlement); err != nil {
				breakerLease.Done(availabilityFailure(providerErr), s.now())
				finalizeErr := s.finalizeRequest(requestLease, "accounting_error")
				return openaiapi.ChatCompletionResponse{}, gatewayError(
					"accounting_unavailable", "request accounting could not be finalized", 503,
					errors.Join(err, finalizeErr),
				)
			}
			breakerLease.Done(availabilityFailure(providerErr), s.now())
			if providerErr == nil {
				response, err = s.redactor.ProcessOutboundChat(
					principal.Project.RedactionPolicyID, response,
				)
				outcome := "success"
				if err != nil {
					outcome = "policy_rejected"
				}
				if finalizeErr := s.finalizeRequest(requestLease, outcome); finalizeErr != nil {
					return openaiapi.ChatCompletionResponse{}, gatewayError(
						"accounting_unavailable", "request accounting could not be finalized", 503, finalizeErr,
					)
				}
				if err != nil {
					return openaiapi.ChatCompletionResponse{}, gatewayError(
						"sensitive_output_detected", "provider output violated redaction policy", 422, err,
					)
				}
				response.Model = request.Model
				return response, nil
			}
			lastErr = providerErr
			if !retryable(providerErr) {
				if err := s.finalizeRequest(requestLease, "provider_error"); err != nil {
					return openaiapi.ChatCompletionResponse{}, gatewayError(
						"accounting_unavailable", "request accounting could not be finalized", 503, err,
					)
				}
				return openaiapi.ChatCompletionResponse{}, mapProviderError(providerErr)
			}
		}
		if attemptCount >= s.maxAttempts || ctx.Err() != nil {
			break
		}
	}
	if err := s.finalizeRequest(requestLease, "provider_error"); err != nil {
		return openaiapi.ChatCompletionResponse{}, gatewayError(
			"accounting_unavailable", "request accounting could not be finalized", 503, err,
		)
	}
	switch {
	case errors.Is(lastErr, budget.ErrExceeded):
		s.rejections.budget.Add(1)
		return openaiapi.ChatCompletionResponse{}, gatewayError("budget_exceeded", "daily budget exceeded", 403, lastErr)
	case errors.Is(lastErr, errDeploymentConcurrency):
		return openaiapi.ChatCompletionResponse{}, gatewayError("deployment_concurrency_limit_exceeded", "deployment concurrency limit exceeded", 429, lastErr)
	case errors.Is(lastErr, provider.ErrConcurrency):
		err := gatewayError("provider_concurrency_limit_exceeded", "all eligible providers are at their concurrency limit", 429, lastErr)
		err.RetryAfter = time.Second
		return openaiapi.ChatCompletionResponse{}, err
	case errors.Is(lastErr, circuit.ErrOpen):
		return openaiapi.ChatCompletionResponse{}, gatewayError("provider_unavailable", "all provider circuits are open", 503, lastErr)
	case lastErr != nil:
		return openaiapi.ChatCompletionResponse{}, mapProviderError(lastErr)
	default:
		return openaiapi.ChatCompletionResponse{}, gatewayError("provider_unavailable", "no provider attempt was available", 503, nil)
	}
}

func (s *Service) ChatStream(
	ctx context.Context,
	plaintextKey string,
	request openaiapi.ChatCompletionRequest,
	emit func(openaiapi.ChatCompletionResponse) error,
) error {
	if !request.Stream {
		return gatewayError("invalid_request_error", "stream must be true", 400, nil)
	}
	principal, err := s.auth.Authenticate(plaintextKey, s.now())
	if err != nil {
		return gatewayError("invalid_api_key", "invalid API key", 401, err)
	}
	if !slices.Contains(principal.Project.AllowedRoutes, request.Model) {
		return gatewayError("model_not_allowed", "model is not allowed for this project", 403, nil)
	}
	if err := authorizeSource(ctx, principal.Project); err != nil {
		return err
	}
	targets := s.registry.ResolveCandidatesFor(request.Model, provider.OperationChatStream)
	if len(targets) == 0 {
		if len(s.registry.ResolveAll(request.Model)) > 0 {
			return gatewayError("unsupported_feature", "model route does not support streaming", 400, nil)
		}
		return gatewayError("model_not_found", "model route was not found", 404, nil)
	}
	targets = filterChatCapabilities(targets, request)
	if len(targets) == 0 {
		return gatewayError("unsupported_feature", "model route does not support the requested chat capabilities", 400, nil)
	}
	if !s.redactor.AllowsStreaming(principal.Project.RedactionPolicyID) {
		return gatewayError(
			"streaming_redaction_incompatible",
			"streaming is disabled by the Project redaction policy",
			400, nil,
		)
	}
	request, err = s.redactor.ProcessInboundChat(principal.Project.RedactionPolicyID, request)
	if err != nil {
		return gatewayError("sensitive_data_detected", "request contains secret material", 400, err)
	}
	inputTokens := estimateInputTokens(request.EstimatedInputBytes())
	if principal.Project.MaxInputTokens > 0 && inputTokens > principal.Project.MaxInputTokens {
		return gatewayError("token_limit_exceeded", "estimated input tokens exceed the project limit", 400, nil)
	}
	outputTokens, err := requestedOutputTokens(request, principal.Project.MaxOutputTokens)
	if err != nil {
		return err
	}
	totalTokens, err := addTokens(inputTokens, outputTokens)
	if err != nil {
		return gatewayError("token_limit_exceeded", "requested token count is too large", 400, err)
	}
	targets = filterTokenCapabilities(targets, inputTokens, outputTokens)
	if len(targets) == 0 {
		return gatewayError("token_limit_exceeded", "request exceeds the model deployment token limits", 400, nil)
	}
	if err := s.admitTokenGuard(ctx, principal, targets, totalTokens, inputTokens, outputTokens); err != nil {
		return err
	}
	providerCalled := false
	providerFailed := false
	defer func() {
		if providerCalled {
			s.tokenGuard.Complete(
				principal.Project.TokenGuardPolicyID,
				principal.Project.ID,
				principal.Key.ID,
				s.now(),
				providerFailed,
			)
		}
	}()
	policyLease, err := s.limiter.Acquire(principal.Project, totalTokens, s.now())
	if err != nil {
		return s.mapLimitError(err)
	}
	actualTPMTokens := int64(0)
	defer func() {
		_ = policyLease.Reconcile(actualTPMTokens, s.now())
		policyLease.Release()
	}()
	requestID, err := id.New("req")
	if err != nil {
		return gatewayError("internal_error", "unable to create request ID", 500, err)
	}
	requestLease, err := s.accounting.BeginRequestDetailed(
		ctx, principal.Project.ID, principal.Key.ID, requestID, request.Model,
	)
	if err != nil {
		return gatewayError("accounting_unavailable", "accounting is unavailable", 503, err)
	}
	var lastErr error
	totalAttempts := 0
	emitted := false
	for targetIndex, target := range targets {
		for targetTry := 0; targetTry < s.maxAttemptsPerTarget && totalAttempts < s.maxAttempts; targetTry++ {
			if ctx.Err() != nil {
				lastErr = ctx.Err()
				break
			}
			if totalAttempts > 0 {
				if err := s.waitRetry(ctx, totalAttempts-1); err != nil {
					lastErr = err
					break
				}
			}
			breakerLease, err := s.breakers.Acquire(target.ID, s.now())
			if err != nil {
				lastErr = err
				break
			}
			providerLease, err := s.acquireTargetConcurrency(target)
			if err != nil {
				breakerLease.Done(nil, s.now())
				if errors.Is(err, errDeploymentConcurrency) {
					s.rejections.deploymentConcurrency.Add(1)
				} else {
					s.rejections.providerConcurrency.Add(1)
				}
				lastErr = err
				break
			}
			reservation, err := estimateReservation(inputTokens, outputTokens, target)
			if err != nil {
				providerLease.Release()
				breakerLease.Done(nil, s.now())
				if !errors.Is(err, budget.ErrExceeded) {
					finalizeErr := s.finalizeRequest(requestLease, "accounting_error")
					return gatewayError(
						"accounting_unavailable", "accounting is unavailable", 503,
						errors.Join(err, finalizeErr),
					)
				}
				lastErr = err
				break
			}
			attempt, err := s.accounting.ReserveAttemptDetailed(
				ctx, requestLease, principal.Project.DailyBudgetMicrosUSD, reservation,
				budget.AttemptMetadata{
					RouteID: target.ID, DeploymentID: target.DeploymentID,
					ProviderID: target.ProviderID, ProviderModel: target.ProviderModel,
					AttemptNumber: totalAttempts + 1, RetryCount: targetTry, FallbackCount: targetIndex,
				},
			)
			if err != nil {
				providerLease.Release()
				breakerLease.Done(nil, s.now())
				lastErr = err
				break
			}
			totalAttempts++
			if err := s.accounting.MarkStarted(ctx, attempt); err != nil {
				providerLease.Release()
				breakerLease.Done(nil, s.now())
				cleanupErr := s.settleAttempt(attempt, budget.Settlement{Outcome: "start_failed"})
				finalizeErr := s.finalizeRequest(requestLease, "accounting_error")
				return gatewayError(
					"accounting_unavailable", "accounting is unavailable", 503,
					errors.Join(err, cleanupErr, finalizeErr),
				)
			}
			attemptEmitted := false
			startedAt := s.now()
			streamRedactor, streamErr := s.redactor.NewStream(
				principal.Project.RedactionPolicyID,
			)
			if streamErr != nil {
				providerLease.Release()
				breakerLease.Done(nil, s.now())
				cleanupErr := s.settleAttempt(attempt, budget.Settlement{Outcome: "policy_rejected"})
				finalizeErr := s.finalizeRequest(requestLease, "policy_rejected")
				return gatewayError(
					"streaming_redaction_incompatible",
					"streaming is disabled by the Project redaction policy",
					400, errors.Join(streamErr, cleanupErr, finalizeErr),
				)
			}
			providerCalled = true
			usage, providerErr := target.Adapter.ChatStream(ctx, provider.ChatCall{
				RequestID: requestID, ProviderModel: target.ProviderModel, Request: request,
			}, func(event semantic.Event) error {
				chunk, conversionErr := event.OpenAIChunk()
				if conversionErr != nil {
					return conversionErr
				}
				chunks, redactionErr := streamRedactor.Process(chunk)
				if redactionErr != nil {
					return redactionErr
				}
				for _, safeChunk := range chunks {
					safeChunk.Model = request.Model
					if err := emit(safeChunk); err != nil {
						return err
					}
					attemptEmitted = true
					emitted = true
				}
				return nil
			})
			if providerErr == nil {
				var chunks []openaiapi.ChatCompletionResponse
				chunks, providerErr = streamRedactor.Flush()
				for _, safeChunk := range chunks {
					if providerErr != nil {
						break
					}
					safeChunk.Model = request.Model
					providerErr = emit(safeChunk)
					if providerErr == nil {
						attemptEmitted = true
						emitted = true
					}
				}
			}
			providerLease.Release()
			providerFailed = providerErr != nil
			settlement := streamSettlement(
				usage, providerErr, attemptEmitted, inputTokens, outputTokens,
				target, attempt.ReservationMicrosUSD,
			)
			actualTPMTokens = accumulateTPMTokens(actualTPMTokens, settlement)
			enrichSettlement(&settlement, providerErr, startedAt, s.now())
			if err := s.settleAttempt(attempt, settlement); err != nil {
				breakerLease.Done(availabilityFailure(providerErr), s.now())
				finalizeErr := s.finalizeRequest(requestLease, "accounting_error")
				return gatewayError(
					"accounting_unavailable", "request accounting could not be finalized", 503,
					errors.Join(err, finalizeErr),
				)
			}
			breakerLease.Done(availabilityFailure(providerErr), s.now())
			if providerErr == nil {
				if err := s.finalizeRequest(requestLease, "success"); err != nil {
					return gatewayError("accounting_unavailable", "request accounting could not be finalized", 503, err)
				}
				return nil
			}
			lastErr = providerErr
			if emitted || !retryable(providerErr) {
				outcome := "provider_error"
				if errors.Is(providerErr, redaction.ErrPolicyRejected) {
					outcome = "policy_rejected"
				}
				if err := s.finalizeRequest(requestLease, outcome); err != nil {
					return gatewayError("accounting_unavailable", "request accounting could not be finalized", 503, err)
				}
				if errors.Is(providerErr, redaction.ErrPolicyRejected) {
					return gatewayError(
						"sensitive_output_detected", "provider output violated redaction policy", 422,
						providerErr,
					)
				}
				if errors.Is(providerErr, context.Canceled) || errors.Is(providerErr, context.DeadlineExceeded) {
					return providerErr
				}
				return mapProviderError(providerErr)
			}
		}
		if totalAttempts >= s.maxAttempts || emitted || ctx.Err() != nil {
			break
		}
	}
	if err := s.finalizeRequest(requestLease, "provider_error"); err != nil {
		return gatewayError("accounting_unavailable", "request accounting could not be finalized", 503, err)
	}
	switch {
	case errors.Is(lastErr, budget.ErrExceeded):
		s.rejections.budget.Add(1)
		return gatewayError("budget_exceeded", "daily budget exceeded", 403, lastErr)
	case errors.Is(lastErr, errDeploymentConcurrency):
		return gatewayError("deployment_concurrency_limit_exceeded", "deployment concurrency limit exceeded", 429, lastErr)
	case errors.Is(lastErr, provider.ErrConcurrency):
		err := gatewayError("provider_concurrency_limit_exceeded", "all eligible providers are at their concurrency limit", 429, lastErr)
		err.RetryAfter = time.Second
		return err
	case errors.Is(lastErr, circuit.ErrOpen):
		return gatewayError("provider_unavailable", "all provider circuits are open", 503, lastErr)
	case errors.Is(lastErr, context.Canceled), errors.Is(lastErr, context.DeadlineExceeded):
		return lastErr
	case lastErr != nil:
		return mapProviderError(lastErr)
	default:
		return gatewayError("provider_unavailable", "no provider attempt was available", 503, nil)
	}
}

func (s *Service) Embeddings(
	ctx context.Context,
	plaintextKey string,
	request openaiapi.EmbeddingRequest,
) (openaiapi.EmbeddingResponse, error) {
	principal, err := s.auth.Authenticate(plaintextKey, s.now())
	if err != nil {
		return openaiapi.EmbeddingResponse{}, gatewayError("invalid_api_key", "invalid API key", 401, err)
	}
	if !slices.Contains(principal.Project.AllowedRoutes, request.Model) {
		return openaiapi.EmbeddingResponse{}, gatewayError("model_not_allowed", "model is not allowed for this project", 403, nil)
	}
	if err := authorizeSource(ctx, principal.Project); err != nil {
		return openaiapi.EmbeddingResponse{}, err
	}
	targets := s.registry.ResolveCandidatesFor(request.Model, provider.OperationEmbeddings)
	if len(targets) == 0 {
		if len(s.registry.ResolveAll(request.Model)) > 0 {
			return openaiapi.EmbeddingResponse{}, gatewayError("unsupported_feature", "model route does not support embeddings", 400, nil)
		}
		return openaiapi.EmbeddingResponse{}, gatewayError("model_not_found", "model route was not found", 404, nil)
	}
	request, err = s.redactor.ProcessInboundEmbedding(
		principal.Project.RedactionPolicyID, request,
	)
	if err != nil {
		return openaiapi.EmbeddingResponse{}, gatewayError("sensitive_data_detected", "request contains secret material", 400, err)
	}
	inputTokens := request.EstimatedInputTokens()
	if principal.Project.MaxInputTokens > 0 && inputTokens > principal.Project.MaxInputTokens {
		return openaiapi.EmbeddingResponse{}, gatewayError("token_limit_exceeded", "estimated input tokens exceed the project limit", 400, nil)
	}
	targets = filterTokenCapabilities(targets, inputTokens, 0)
	if len(targets) == 0 {
		return openaiapi.EmbeddingResponse{}, gatewayError("token_limit_exceeded", "request exceeds the model deployment token limits", 400, nil)
	}
	if err := s.admitTokenGuard(ctx, principal, targets, inputTokens, inputTokens, 0); err != nil {
		return openaiapi.EmbeddingResponse{}, err
	}
	providerCalled := false
	providerFailed := false
	defer func() {
		if providerCalled {
			s.tokenGuard.Complete(
				principal.Project.TokenGuardPolicyID,
				principal.Project.ID,
				principal.Key.ID,
				s.now(),
				providerFailed,
			)
		}
	}()
	policyLease, err := s.limiter.Acquire(principal.Project, inputTokens, s.now())
	if err != nil {
		return openaiapi.EmbeddingResponse{}, s.mapLimitError(err)
	}
	actualTPMTokens := int64(0)
	defer func() {
		_ = policyLease.Reconcile(actualTPMTokens, s.now())
		policyLease.Release()
	}()
	requestID, err := id.New("req")
	if err != nil {
		return openaiapi.EmbeddingResponse{}, gatewayError("internal_error", "unable to create request ID", 500, err)
	}
	requestLease, err := s.accounting.BeginRequestDetailed(
		ctx, principal.Project.ID, principal.Key.ID, requestID, request.Model,
	)
	if err != nil {
		return openaiapi.EmbeddingResponse{}, gatewayError("accounting_unavailable", "accounting is unavailable", 503, err)
	}
	var lastErr error
	attemptCount := 0
	for targetIndex, target := range targets {
		for targetTry := 0; targetTry < s.maxAttemptsPerTarget && attemptCount < s.maxAttempts; targetTry++ {
			if ctx.Err() != nil {
				lastErr = ctx.Err()
				break
			}
			if attemptCount > 0 {
				if err := s.waitRetry(ctx, attemptCount-1); err != nil {
					lastErr = err
					break
				}
			}
			breakerLease, err := s.breakers.Acquire(target.ID, s.now())
			if err != nil {
				lastErr = err
				break
			}
			providerLease, err := s.acquireTargetConcurrency(target)
			if err != nil {
				breakerLease.Done(nil, s.now())
				if errors.Is(err, errDeploymentConcurrency) {
					s.rejections.deploymentConcurrency.Add(1)
				} else {
					s.rejections.providerConcurrency.Add(1)
				}
				lastErr = err
				break
			}
			reservation, err := estimateReservation(inputTokens, 0, target)
			if err != nil {
				providerLease.Release()
				breakerLease.Done(nil, s.now())
				lastErr = err
				break
			}
			attempt, err := s.accounting.ReserveAttemptDetailed(
				ctx, requestLease, principal.Project.DailyBudgetMicrosUSD, reservation,
				budget.AttemptMetadata{
					RouteID: target.ID, DeploymentID: target.DeploymentID,
					ProviderID: target.ProviderID, ProviderModel: target.ProviderModel,
					AttemptNumber: attemptCount + 1, RetryCount: targetTry, FallbackCount: targetIndex,
				},
			)
			if err != nil {
				providerLease.Release()
				breakerLease.Done(nil, s.now())
				if !errors.Is(err, budget.ErrExceeded) {
					finalizeErr := s.finalizeRequest(requestLease, "accounting_error")
					return openaiapi.EmbeddingResponse{}, gatewayError(
						"accounting_unavailable", "accounting is unavailable", 503,
						errors.Join(err, finalizeErr),
					)
				}
				lastErr = err
				break
			}
			attemptCount++
			if err := s.accounting.MarkStarted(ctx, attempt); err != nil {
				providerLease.Release()
				breakerLease.Done(nil, s.now())
				cleanupErr := s.settleAttempt(attempt, budget.Settlement{Outcome: "start_failed"})
				finalizeErr := s.finalizeRequest(requestLease, "accounting_error")
				return openaiapi.EmbeddingResponse{}, gatewayError(
					"accounting_unavailable", "accounting is unavailable", 503,
					errors.Join(err, cleanupErr, finalizeErr),
				)
			}
			providerCalled = true
			startedAt := s.now()
			response, providerErr := target.Adapter.Embed(ctx, provider.EmbeddingCall{
				RequestID: requestID, ProviderModel: target.ProviderModel, Request: request,
			})
			providerLease.Release()
			providerFailed = providerErr != nil
			settlement := embeddingSettlement(
				response, providerErr, inputTokens, target, attempt.ReservationMicrosUSD,
			)
			actualTPMTokens = accumulateTPMTokens(actualTPMTokens, settlement)
			enrichSettlement(&settlement, providerErr, startedAt, s.now())
			if err := s.settleAttempt(attempt, settlement); err != nil {
				breakerLease.Done(availabilityFailure(providerErr), s.now())
				finalizeErr := s.finalizeRequest(requestLease, "accounting_error")
				return openaiapi.EmbeddingResponse{}, gatewayError(
					"accounting_unavailable", "request accounting could not be finalized", 503,
					errors.Join(err, finalizeErr),
				)
			}
			breakerLease.Done(availabilityFailure(providerErr), s.now())
			if providerErr == nil {
				if err := s.finalizeRequest(requestLease, "success"); err != nil {
					return openaiapi.EmbeddingResponse{}, gatewayError(
						"accounting_unavailable", "request accounting could not be finalized", 503, err,
					)
				}
				response.Model = request.Model
				return response, nil
			}
			lastErr = providerErr
			if !retryable(providerErr) {
				if err := s.finalizeRequest(requestLease, "provider_error"); err != nil {
					return openaiapi.EmbeddingResponse{}, gatewayError(
						"accounting_unavailable", "request accounting could not be finalized", 503, err,
					)
				}
				return openaiapi.EmbeddingResponse{}, mapProviderError(providerErr)
			}
		}
		if attemptCount >= s.maxAttempts || ctx.Err() != nil {
			break
		}
	}
	if err := s.finalizeRequest(requestLease, "provider_error"); err != nil {
		return openaiapi.EmbeddingResponse{}, gatewayError(
			"accounting_unavailable", "request accounting could not be finalized", 503, err,
		)
	}
	switch {
	case errors.Is(lastErr, budget.ErrExceeded):
		s.rejections.budget.Add(1)
		return openaiapi.EmbeddingResponse{}, gatewayError("budget_exceeded", "daily budget exceeded", 403, lastErr)
	case errors.Is(lastErr, errDeploymentConcurrency):
		return openaiapi.EmbeddingResponse{}, gatewayError("deployment_concurrency_limit_exceeded", "deployment concurrency limit exceeded", 429, lastErr)
	case errors.Is(lastErr, provider.ErrConcurrency):
		err := gatewayError("provider_concurrency_limit_exceeded", "all eligible providers are at their concurrency limit", 429, lastErr)
		err.RetryAfter = time.Second
		return openaiapi.EmbeddingResponse{}, err
	case errors.Is(lastErr, circuit.ErrOpen):
		return openaiapi.EmbeddingResponse{}, gatewayError("provider_unavailable", "all provider circuits are open", 503, lastErr)
	case lastErr != nil:
		return openaiapi.EmbeddingResponse{}, mapProviderError(lastErr)
	default:
		return openaiapi.EmbeddingResponse{}, gatewayError("provider_unavailable", "no provider attempt was available", 503, nil)
	}
}

func accumulateTPMTokens(total int64, settlement budget.Settlement) int64 {
	attempt := settlement.ProviderInputTokens + settlement.ProviderOutputTokens
	if attempt < 0 || total > math.MaxInt64-attempt {
		return math.MaxInt64
	}
	return total + attempt
}

func (s *Service) cleanup(attempt budget.Attempt, settlement budget.Settlement, outcome string) error {
	if err := s.settleAttempt(attempt, settlement); err != nil {
		return err
	}
	return s.finalizeRequest(attempt.Request(), outcome)
}

func (s *Service) settleAttempt(attempt budget.Attempt, settlement budget.Settlement) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	return s.accounting.Settle(ctx, attempt, settlement)
}

func (s *Service) finalizeRequest(request budget.Request, outcome string) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	return s.accounting.Finalize(ctx, request, outcome)
}

func estimateReservation(inputTokens, outputTokens int64, target provider.Target) (int64, error) {
	reservation, err := budget.EstimateCostMicros(
		inputTokens, outputTokens, target.InputMicrosPerMillion, target.OutputMicrosPerMillion,
	)
	if err != nil {
		return 0, gatewayError("accounting_error", "unable to estimate request cost", 503, err)
	}
	if reservation == 0 {
		reservation = 1
	}
	return reservation, nil
}

func retryable(err error) bool {
	var classified *provider.Error
	if !errors.As(err, &classified) {
		return false
	}
	if classified.Ambiguous {
		return false
	}
	return classified.Retryable ||
		classified.Class == provider.ErrorMalformed ||
		classified.Class == provider.ErrorProvider5xx
}

func availabilityFailure(err error) error {
	if err == nil {
		return nil
	}
	var classified *provider.Error
	if !errors.As(err, &classified) {
		return nil
	}
	switch classified.Class {
	case provider.ErrorConnect, provider.ErrorTimeout, provider.ErrorProvider5xx, provider.ErrorMalformed:
		return err
	default:
		return nil
	}
}

func (s *Service) waitRetry(ctx context.Context, retryIndex int) error {
	delay := s.retryBaseDelay
	for index := 0; index < retryIndex && delay < s.retryMaxDelay; index++ {
		if delay > s.retryMaxDelay/2 {
			delay = s.retryMaxDelay
			break
		}
		delay *= 2
	}
	if delay > s.retryMaxDelay {
		delay = s.retryMaxDelay
	}
	if s.retryJitter {
		var random [1]byte
		if _, err := cryptorand.Read(random[:]); err == nil {
			delay = delay/2 + time.Duration(int64(delay/2)*int64(random[0])/255)
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *Service) admitTokenGuard(
	ctx context.Context,
	principal auth.AuthResult,
	targets []provider.Target,
	totalTokens, inputTokens, outputTokens int64,
) error {
	var maximumCost int64
	for _, target := range targets {
		cost, err := budget.EstimateCostMicros(
			inputTokens, outputTokens,
			target.InputMicrosPerMillion, target.OutputMicrosPerMillion,
		)
		if err == nil && cost > maximumCost {
			maximumCost = cost
		}
	}
	input := tokenguard.Input{
		PolicyID:               principal.Project.TokenGuardPolicyID,
		ProjectID:              principal.Project.ID,
		KeyID:                  principal.Key.ID,
		EstimatedTokens:        totalTokens,
		EstimatedCostMicrosUSD: maximumCost,
		Now:                    s.now(),
	}
	if source, ok := requestmeta.SourceIP(ctx); ok {
		mac := hmac.New(sha256.New, s.sourceHashKey[:])
		_, _ = mac.Write(source.AsSlice())
		copy(input.SourceHash[:], mac.Sum(nil))
		input.HasSource = true
	}
	decision := s.tokenGuard.Admit(input)
	if !decision.Allowed {
		s.rejections.tokenGuard.Add(1)
		return gatewayError(
			"token_guard_blocked",
			"gateway key is temporarily blocked due to anomalous token usage",
			403,
			nil,
		)
	}
	return nil
}

func filterChatCapabilities(targets []provider.Target, request openaiapi.ChatCompletionRequest) []provider.Target {
	requiresTools := len(request.Tools) > 0 || hasJSONValue(request.ToolChoice)
	requiresVision := requestUsesVision(request)
	requiresJSON := requestUsesJSONMode(request.ResponseFormat)
	requiresDeveloper := false
	requiresReasoning := request.ReasoningEffort != ""
	for _, message := range request.Messages {
		requiresDeveloper = requiresDeveloper || message.Role == "developer"
		requiresReasoning = requiresReasoning || message.ReasoningContent != ""
		requiresTools = requiresTools || message.Role == "tool" || len(message.ToolCalls) > 0 || message.ToolCallID != ""
	}
	requiresStreamUsage := request.StreamOptions != nil && request.StreamOptions.IncludeUsage
	return slices.DeleteFunc(slices.Clone(targets), func(target provider.Target) bool {
		capabilities := target.Capabilities
		return (requiresTools && !capabilities.Tools) ||
			(requiresVision && !capabilities.Vision) ||
			(requiresJSON && !capabilities.JSONMode) ||
			(requiresDeveloper && !capabilities.DeveloperRole) ||
			(requiresReasoning && !capabilities.Reasoning) ||
			(requiresStreamUsage && !capabilities.StreamUsage)
	})
}

func filterTokenCapabilities(targets []provider.Target, inputTokens, outputTokens int64) []provider.Target {
	return slices.DeleteFunc(slices.Clone(targets), func(target provider.Target) bool {
		capabilities := target.Capabilities
		if capabilities.MaxOutputTokens > 0 && outputTokens > capabilities.MaxOutputTokens {
			return true
		}
		if capabilities.MaxContextTokens > 0 {
			total, err := addTokens(inputTokens, outputTokens)
			return err != nil || total > capabilities.MaxContextTokens
		}
		return false
	})
}

func requestUsesVision(request openaiapi.ChatCompletionRequest) bool {
	for _, message := range request.Messages {
		var parts []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(message.Content, &parts) != nil {
			continue
		}
		for _, part := range parts {
			switch part.Type {
			case "image", "image_url", "input_image":
				return true
			}
		}
	}
	return false
}

func requestUsesJSONMode(raw json.RawMessage) bool {
	if !hasJSONValue(raw) {
		return false
	}
	var format struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &format) != nil {
		return true
	}
	return format.Type == "json_object" || format.Type == "json_schema"
}

func hasJSONValue(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

func authorizeSource(ctx context.Context, project domain.Project) error {
	if len(project.AllowedCIDRs) == 0 {
		return nil
	}
	source, ok := requestmeta.SourceIP(ctx)
	if !ok {
		return gatewayError("source_not_allowed", "request source is not allowed for this project", 403, nil)
	}
	for _, prefix := range project.AllowedCIDRs {
		if prefix.Contains(source) {
			return nil
		}
	}
	return gatewayError("source_not_allowed", "request source is not allowed for this project", 403, nil)
}

func settlementForResult(
	response openaiapi.ChatCompletionResponse,
	providerErr error,
	estimatedInputTokens int64,
	estimatedOutputTokens int64,
	target provider.Target,
	reservationMicrosUSD int64,
) budget.Settlement {
	result := budget.Settlement{Outcome: "success"}
	if providerErr != nil {
		result.Outcome = "provider_error"
		var classified *provider.Error
		if !errors.As(providerErr, &classified) || !classified.Ambiguous {
			return result
		}
		result.ProviderInputTokens = estimatedInputTokens
		result.ProviderOutputTokens = estimatedOutputTokens
		result.PreparedOutputTokens = estimatedOutputTokens
		result.TokenEstimated = true
		result.CostEstimated = true
		setSettlementCost(&result, target, reservationMicrosUSD)
		return result
	}
	if !validUsage(response.Usage) {
		result.ProviderInputTokens = estimatedInputTokens
		result.ProviderOutputTokens = estimatedOutputTokens
		result.PreparedOutputTokens = estimatedOutputTokens
		result.TokenEstimated = true
		result.CostEstimated = true
	} else {
		result.ProviderInputTokens = response.Usage.PromptTokens
		result.ProviderOutputTokens = response.Usage.CompletionTokens
	}
	setSettlementCost(&result, target, reservationMicrosUSD)
	return result
}

func setSettlementCost(result *budget.Settlement, target provider.Target, reservationMicrosUSD int64) {
	cost, err := budget.EstimateCostMicros(
		result.ProviderInputTokens,
		result.ProviderOutputTokens,
		target.InputMicrosPerMillion,
		target.OutputMicrosPerMillion,
	)
	if err != nil {
		result.CommittedMicrosUSD = reservationMicrosUSD
		result.CostEstimated = true
		return
	}
	result.CommittedMicrosUSD = cost
}

func enrichSettlement(result *budget.Settlement, providerErr error, startedAt, completedAt time.Time) {
	if completedAt.After(startedAt) {
		result.LatencyMillis = completedAt.Sub(startedAt).Milliseconds()
	}
	if providerErr == nil {
		result.HTTPStatus = http.StatusOK
		return
	}
	var classified *provider.Error
	if errors.As(providerErr, &classified) {
		result.ErrorClass = string(classified.Class)
		result.HTTPStatus = classified.StatusCode
		return
	}
	switch {
	case errors.Is(providerErr, context.DeadlineExceeded):
		result.ErrorClass = string(provider.ErrorTimeout)
	case errors.Is(providerErr, context.Canceled):
		result.ErrorClass = "canceled"
	default:
		result.ErrorClass = string(provider.ErrorUnknown)
	}
}

func embeddingSettlement(
	response openaiapi.EmbeddingResponse,
	providerErr error,
	estimatedInputTokens int64,
	target provider.Target,
	reservationMicrosUSD int64,
) budget.Settlement {
	result := budget.Settlement{Outcome: "success"}
	if providerErr != nil {
		result.Outcome = "provider_error"
		var classified *provider.Error
		if !errors.As(providerErr, &classified) || !classified.Ambiguous {
			return result
		}
		result.ProviderInputTokens = estimatedInputTokens
		result.TokenEstimated = true
		result.CostEstimated = true
	} else if !validUsage(response.Usage) {
		result.ProviderInputTokens = estimatedInputTokens
		result.TokenEstimated = true
		result.CostEstimated = true
	} else {
		result.ProviderInputTokens = response.Usage.PromptTokens
	}
	setSettlementCost(&result, target, reservationMicrosUSD)
	return result
}

func streamSettlement(
	usage *openaiapi.Usage,
	providerErr error,
	emitted bool,
	estimatedInputTokens int64,
	estimatedOutputTokens int64,
	target provider.Target,
	reservationMicrosUSD int64,
) budget.Settlement {
	result := budget.Settlement{Outcome: "success"}
	if providerErr != nil {
		result.Outcome = "provider_error"
	}
	if validUsage(usage) {
		result.ProviderInputTokens = usage.PromptTokens
		result.ProviderOutputTokens = usage.CompletionTokens
	} else if providerErr == nil || emitted {
		result.ProviderInputTokens = estimatedInputTokens
		result.ProviderOutputTokens = estimatedOutputTokens
		result.PreparedOutputTokens = estimatedOutputTokens
		result.TokenEstimated = true
		result.CostEstimated = true
	} else {
		var classified *provider.Error
		if errors.As(providerErr, &classified) && classified.Ambiguous {
			result.ProviderInputTokens = estimatedInputTokens
			result.ProviderOutputTokens = estimatedOutputTokens
			result.PreparedOutputTokens = estimatedOutputTokens
			result.TokenEstimated = true
			result.CostEstimated = true
		}
	}
	setSettlementCost(&result, target, reservationMicrosUSD)
	return result
}

func validUsage(usage *openaiapi.Usage) bool {
	if usage == nil || usage.PromptTokens < 0 || usage.CompletionTokens < 0 || usage.TotalTokens < 0 {
		return false
	}
	if usage.PromptTokens > maxAccountableTokens ||
		usage.CompletionTokens > maxAccountableTokens ||
		usage.TotalTokens > maxAccountableTokens {
		return false
	}
	total, err := addTokens(usage.PromptTokens, usage.CompletionTokens)
	return err == nil && usage.TotalTokens >= total
}

func requestedOutputTokens(request openaiapi.ChatCompletionRequest, projectLimit int64) (int64, error) {
	requested := defaultMaximumOutputTokens
	if request.MaxTokens != nil {
		requested = *request.MaxTokens
	}
	if request.MaxCompletionTokens != nil {
		requested = *request.MaxCompletionTokens
	}
	if projectLimit > 0 {
		if requested > projectLimit {
			return 0, gatewayError("token_limit_exceeded", "requested output tokens exceed the project limit", 400, nil)
		}
		if request.MaxTokens == nil && request.MaxCompletionTokens == nil {
			requested = projectLimit
		}
	}
	return requested, nil
}

func estimateInputTokens(bytes int64) int64 {
	if bytes <= 0 {
		return 1
	}
	return (bytes + 3) / 4
}

func addTokens(left, right int64) (int64, error) {
	if left < 0 || right < 0 || left > math.MaxInt64-right {
		return 0, errors.New("token count overflow")
	}
	return left + right, nil
}

func mapProviderError(err error) error {
	var classified *provider.Error
	if !errors.As(err, &classified) {
		return gatewayError("provider_error", "provider request failed", 502, err)
	}
	switch classified.Class {
	case provider.ErrorBadRequest:
		return gatewayError("invalid_request_error", "provider rejected the request", 400, err)
	case provider.ErrorAuthentication:
		return gatewayError("provider_authentication_error", "provider authentication failed", 502, err)
	case provider.ErrorRateLimit:
		return gatewayError("provider_rate_limit", "provider rate limit exceeded", 429, err)
	case provider.ErrorTimeout:
		return gatewayError("provider_timeout", "provider timed out", 504, err)
	default:
		return gatewayError("provider_error", "provider request failed", 502, err)
	}
}

func (s *Service) mapLimitError(err error) error {
	mapped := gatewayError("policy_error", "request policy could not be evaluated", 503, err)
	switch {
	case errors.Is(err, limiter.ErrRPM):
		s.rejections.rpm.Add(1)
		mapped = gatewayError("rate_limit_exceeded", "project RPM limit exceeded", 429, err)
	case errors.Is(err, limiter.ErrTPM):
		s.rejections.tpm.Add(1)
		mapped = gatewayError("token_rate_limit_exceeded", "project TPM limit exceeded", 429, err)
	case errors.Is(err, limiter.ErrConcurrency):
		s.rejections.projectConcurrency.Add(1)
		mapped = gatewayError("concurrency_limit_exceeded", "project concurrency limit exceeded", 429, err)
	}
	var limitErr *limiter.Error
	if errors.As(err, &limitErr) {
		mapped.RetryAfter = limitErr.RetryAfter
	}
	return mapped
}

func gatewayError(code, message string, status int, cause error) *Error {
	return &Error{Code: code, Message: message, HTTPStatus: status, Cause: cause}
}
