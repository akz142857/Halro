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
	"github.com/akz142857/Heimdall/internal/compatibility"
	openaiwire "github.com/akz142857/Heimdall/internal/compatibility/openai"
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

type requestRun struct {
	service         *Service
	principal       auth.AuthResult
	policyLease     *limiter.Lease
	requestLease    budget.Request
	requestID       string
	actualTPMTokens int64
	providerCalled  bool
	providerFailed  bool
}

type activeAttempt struct {
	service     *Service
	run         *requestRun
	accounting  budget.Attempt
	breaker     *circuit.Lease
	concurrency *targetConcurrencyLease
	startedAt   time.Time
}

func (s *Service) resolveRequest(
	ctx context.Context,
	plaintextKey, model string,
	operation provider.Operation,
	unsupportedMessage string,
) (auth.AuthResult, []provider.Target, error) {
	principal, err := s.auth.Authenticate(plaintextKey, s.now())
	if err != nil {
		return auth.AuthResult{}, nil, gatewayError("invalid_api_key", "invalid API key", 401, err)
	}
	if !slices.Contains(principal.Project.AllowedRoutes, model) {
		return auth.AuthResult{}, nil, gatewayError("model_not_allowed", "model is not allowed for this project", 403, nil)
	}
	if err := authorizeSource(ctx, principal.Project); err != nil {
		return auth.AuthResult{}, nil, err
	}
	targets := s.registry.ResolveCandidatesFor(model, operation)
	if len(targets) == 0 {
		if len(s.registry.ResolveAll(model)) > 0 {
			return auth.AuthResult{}, nil, gatewayError("unsupported_feature", unsupportedMessage, 400, nil)
		}
		return auth.AuthResult{}, nil, gatewayError("model_not_found", "model route was not found", 404, nil)
	}
	return principal, targets, nil
}

func (s *Service) beginRequestRun(
	ctx context.Context,
	principal auth.AuthResult,
	model string,
	targets []provider.Target,
	totalTokens, inputTokens, outputTokens int64,
) (*requestRun, error) {
	if err := s.admitTokenGuard(ctx, principal, targets, totalTokens, inputTokens, outputTokens); err != nil {
		return nil, err
	}
	policyLease, err := s.limiter.Acquire(principal.Project, totalTokens, s.now())
	if err != nil {
		return nil, s.mapLimitError(err)
	}
	requestID, err := id.New("req")
	if err != nil {
		policyLease.Release()
		return nil, gatewayError("internal_error", "unable to create request ID", 500, err)
	}
	requestLease, err := s.accounting.BeginRequestDetailed(
		ctx, principal.Project.ID, principal.Key.ID, requestID, model,
	)
	if err != nil {
		policyLease.Release()
		return nil, gatewayError("accounting_unavailable", "accounting is unavailable", 503, err)
	}
	return &requestRun{
		service: s, principal: principal, policyLease: policyLease,
		requestLease: requestLease, requestID: requestID,
	}, nil
}

func (run *requestRun) close() {
	_ = run.policyLease.Reconcile(run.actualTPMTokens, run.service.now())
	run.policyLease.Release()
	if run.providerCalled {
		run.service.tokenGuard.Complete(
			run.principal.Project.TokenGuardPolicyID,
			run.principal.Project.ID,
			run.principal.Key.ID,
			run.service.now(),
			run.providerFailed,
		)
	}
}

func (run *requestRun) recordProviderResult(providerErr error, settlement budget.Settlement) {
	run.providerCalled = true
	run.providerFailed = providerErr != nil
	run.actualTPMTokens = accumulateTPMTokens(run.actualTPMTokens, settlement)
}

func (s *Service) exhaustedAttemptsError(lastErr error) error {
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

func terminalProviderError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return mapProviderError(err)
}

func (s *Service) startAttempt(
	ctx context.Context,
	run *requestRun,
	target provider.Target,
	inputTokens, outputTokens int64,
	targetIndex, targetTry, attemptNumber int,
) (*activeAttempt, error) {
	breakerLease, err := s.breakers.Acquire(target.ID, s.now())
	if err != nil {
		return nil, err
	}
	providerLease, err := s.acquireTargetConcurrency(target)
	if err != nil {
		breakerLease.Done(nil, s.now())
		if errors.Is(err, errDeploymentConcurrency) {
			s.rejections.deploymentConcurrency.Add(1)
		} else {
			s.rejections.providerConcurrency.Add(1)
		}
		return nil, err
	}
	reservation, err := estimateReservation(inputTokens, outputTokens, target)
	if err != nil {
		providerLease.Release()
		breakerLease.Done(nil, s.now())
		finalizeErr := s.finalizeRequest(run.requestLease, "accounting_error")
		return nil, gatewayError(
			"accounting_unavailable", "accounting is unavailable", 503,
			errors.Join(err, finalizeErr),
		)
	}
	attempt, err := s.accounting.ReserveAttemptDetailed(
		ctx, run.requestLease, run.principal.Project.DailyBudgetMicrosUSD, reservation,
		budget.AttemptMetadata{
			RouteID: target.ID, DeploymentID: target.DeploymentID,
			ProviderID: target.ProviderID, ProviderModel: target.ProviderModel,
			AttemptNumber: attemptNumber, RetryCount: targetTry, FallbackCount: targetIndex,
		},
	)
	if err != nil {
		providerLease.Release()
		breakerLease.Done(nil, s.now())
		if errors.Is(err, budget.ErrExceeded) {
			return nil, err
		}
		finalizeErr := s.finalizeRequest(run.requestLease, "accounting_error")
		return nil, gatewayError(
			"accounting_unavailable", "accounting is unavailable", 503,
			errors.Join(err, finalizeErr),
		)
	}
	if err := s.accounting.MarkStarted(ctx, attempt); err != nil {
		providerLease.Release()
		breakerLease.Done(nil, s.now())
		cleanupErr := s.settleAttempt(attempt, budget.Settlement{Outcome: "start_failed"})
		finalizeErr := s.finalizeRequest(run.requestLease, "accounting_error")
		return nil, gatewayError(
			"accounting_unavailable", "accounting is unavailable", 503,
			errors.Join(err, cleanupErr, finalizeErr),
		)
	}
	return &activeAttempt{
		service: s, run: run, accounting: attempt, breaker: breakerLease,
		concurrency: providerLease, startedAt: s.now(),
	}, nil
}

func (attempt *activeAttempt) finish(providerErr error, settlement budget.Settlement) error {
	attempt.concurrency.Release()
	attempt.run.recordProviderResult(providerErr, settlement)
	enrichSettlement(&settlement, providerErr, attempt.startedAt, attempt.service.now())
	if err := attempt.service.settleAttempt(attempt.accounting, settlement); err != nil {
		attempt.breaker.Done(availabilityFailure(providerErr), attempt.service.now())
		finalizeErr := attempt.service.finalizeRequest(attempt.run.requestLease, "accounting_error")
		return gatewayError(
			"accounting_unavailable", "request accounting could not be finalized", 503,
			errors.Join(err, finalizeErr),
		)
	}
	attempt.breaker.Done(availabilityFailure(providerErr), attempt.service.now())
	return nil
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
	principal, targets, err := s.resolveRequest(
		ctx, plaintextKey, request.Model, provider.OperationChat,
		"model route does not support chat completions",
	)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, err
	}
	canonical, err := openaiwire.DecodeGenerate(request)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, gatewayError("invalid_request_error", "request cannot be represented safely", 400, err)
	}
	targets = filterSemanticCapabilities(targets, canonical.Requirements)
	targets = filterGenerateProfileCompatibility(targets, canonical)
	targets = filterPrimitiveTargets(targets, provider.OperationChat)
	if len(targets) == 0 {
		return openaiapi.ChatCompletionResponse{}, gatewayError("unsupported_feature", "model route does not support the requested chat capabilities", 400, nil)
	}
	request, err = s.redactor.ProcessInboundChat(principal.Project.RedactionPolicyID, request)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, gatewayError("sensitive_data_detected", "request contains secret material", 400, err)
	}
	canonical, err = openaiwire.DecodeGenerate(request)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, gatewayError("invalid_request_error", "redacted request cannot be represented safely", 400, err)
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
	run, err := s.beginRequestRun(ctx, principal, request.Model, targets, totalTokens, inputTokens, outputTokens)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, err
	}
	defer run.close()
	requestID, requestLease := run.requestID, run.requestLease
	var lastErr error
	attemptCount := 0
	for targetIndex, target := range targets {
		for targetTry := 0; targetTry < s.maxAttemptsPerTarget && attemptCount < s.maxAttempts; targetTry++ {
			if ctx.Err() != nil {
				lastErr = ctx.Err()
				break
			}
			if attemptCount > 0 {
				if err := s.waitRetry(ctx, attemptCount-1, lastErr); err != nil {
					lastErr = err
					break
				}
			}
			attempt, err := s.startAttempt(
				ctx, run, target, inputTokens, outputTokens,
				targetIndex, targetTry, attemptCount+1,
			)
			if err != nil {
				var fatal *Error
				if errors.As(err, &fatal) {
					return openaiapi.ChatCompletionResponse{}, fatal
				}
				lastErr = err
				break
			}
			attemptCount++
			generation, resolveErr := target.Generation(provider.OperationChat)
			if resolveErr != nil {
				return openaiapi.ChatCompletionResponse{}, gatewayError("unsupported_feature", "generation primitive is unavailable", 400, resolveErr)
			}
			semanticResponse, providerErr := generation.Generate(ctx, provider.GenerateCall{RequestID: requestID, ProviderModel: target.ProviderModel, Request: canonical})
			response := openaiapi.ChatCompletionResponse{}
			if providerErr == nil {
				response, err = openaiwire.RenderGenerateResult(semanticResponse)
				if err != nil {
					providerErr = &provider.Error{Class: provider.ErrorMalformed, Ambiguous: true, Message: "render canonical generate result", Cause: err}
				}
			}
			settlement := settlementForResult(
				semanticResponse, providerErr, inputTokens, outputTokens, target, attempt.accounting.ReservationMicrosUSD,
			)
			if err := attempt.finish(providerErr, settlement); err != nil {
				return openaiapi.ChatCompletionResponse{}, err
			}
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
				return openaiapi.ChatCompletionResponse{}, terminalProviderError(providerErr)
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
	return openaiapi.ChatCompletionResponse{}, s.exhaustedAttemptsError(lastErr)
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
	principal, targets, err := s.resolveRequest(
		ctx, plaintextKey, request.Model, provider.OperationChatStream,
		"model route does not support streaming",
	)
	if err != nil {
		return err
	}
	canonical, err := openaiwire.DecodeGenerate(request)
	if err != nil {
		return gatewayError("invalid_request_error", "request cannot be represented safely", 400, err)
	}
	targets = filterSemanticCapabilities(targets, canonical.Requirements)
	targets = filterGenerateProfileCompatibility(targets, canonical)
	targets = filterPrimitiveTargets(targets, provider.OperationChatStream)
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
	canonical, err = openaiwire.DecodeGenerate(request)
	if err != nil {
		return gatewayError("invalid_request_error", "redacted request cannot be represented safely", 400, err)
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
	run, err := s.beginRequestRun(ctx, principal, request.Model, targets, totalTokens, inputTokens, outputTokens)
	if err != nil {
		return err
	}
	defer run.close()
	requestID, requestLease := run.requestID, run.requestLease
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
				if err := s.waitRetry(ctx, totalAttempts-1, lastErr); err != nil {
					lastErr = err
					break
				}
			}
			attempt, err := s.startAttempt(
				ctx, run, target, inputTokens, outputTokens,
				targetIndex, targetTry, totalAttempts+1,
			)
			if err != nil {
				var fatal *Error
				if errors.As(err, &fatal) {
					return fatal
				}
				lastErr = err
				break
			}
			totalAttempts++
			attemptEmitted := false
			streamRedactor, streamErr := s.redactor.NewStream(
				principal.Project.RedactionPolicyID,
			)
			if streamErr != nil {
				attempt.concurrency.Release()
				attempt.breaker.Done(nil, s.now())
				cleanupErr := s.settleAttempt(attempt.accounting, budget.Settlement{Outcome: "policy_rejected"})
				finalizeErr := s.finalizeRequest(requestLease, "policy_rejected")
				return gatewayError(
					"streaming_redaction_incompatible",
					"streaming is disabled by the Project redaction policy",
					400, errors.Join(streamErr, cleanupErr, finalizeErr),
				)
			}
			semanticStream := semantic.NewStreamValidator()
			generation, resolveErr := target.Generation(provider.OperationChatStream)
			if resolveErr != nil {
				return gatewayError("unsupported_feature", "generation primitive is unavailable", 400, resolveErr)
			}
			semanticUsage, providerErr := generation.GenerateStream(ctx, provider.GenerateCall{
				RequestID: requestID, ProviderModel: target.ProviderModel, Request: canonical,
			}, func(event semantic.Event) error {
				if validationErr := semanticStream.Accept(event); validationErr != nil {
					return validationErr
				}
				chunk, conversionErr := openaiwire.RenderEvent(event)
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
				providerErr = semanticStream.Finalize(canonical.IncludeUsage)
			}
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
			settlement := streamSettlement(
				semanticUsage, providerErr, attemptEmitted, inputTokens, outputTokens,
				target, attempt.accounting.ReservationMicrosUSD,
			)
			if err := attempt.finish(providerErr, settlement); err != nil {
				return err
			}
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
				return terminalProviderError(providerErr)
			}
		}
		if totalAttempts >= s.maxAttempts || emitted || ctx.Err() != nil {
			break
		}
	}
	if err := s.finalizeRequest(requestLease, "provider_error"); err != nil {
		return gatewayError("accounting_unavailable", "request accounting could not be finalized", 503, err)
	}
	return s.exhaustedAttemptsError(lastErr)
}

func (s *Service) Embeddings(
	ctx context.Context,
	plaintextKey string,
	request openaiapi.EmbeddingRequest,
) (openaiapi.EmbeddingResponse, error) {
	principal, targets, err := s.resolveRequest(
		ctx, plaintextKey, request.Model, provider.OperationEmbeddings,
		"model route does not support embeddings",
	)
	if err != nil {
		return openaiapi.EmbeddingResponse{}, err
	}
	request, err = s.redactor.ProcessInboundEmbedding(
		principal.Project.RedactionPolicyID, request,
	)
	if err != nil {
		return openaiapi.EmbeddingResponse{}, gatewayError("sensitive_data_detected", "request contains secret material", 400, err)
	}
	canonical, err := openaiwire.DecodeEmbedding(request)
	if err != nil {
		return openaiapi.EmbeddingResponse{}, gatewayError("invalid_request_error", "embedding request cannot be represented safely", 400, err)
	}
	targets = filterPrimitiveTargets(targets, provider.OperationEmbeddings)
	targets = filterEmbeddingProfileCompatibility(targets, canonical)
	if len(targets) == 0 {
		return openaiapi.EmbeddingResponse{}, gatewayError("unsupported_feature", "model route cannot represent the requested embedding fields", 400, nil)
	}
	inputTokens := request.EstimatedInputTokens()
	if principal.Project.MaxInputTokens > 0 && inputTokens > principal.Project.MaxInputTokens {
		return openaiapi.EmbeddingResponse{}, gatewayError("token_limit_exceeded", "estimated input tokens exceed the project limit", 400, nil)
	}
	targets = filterTokenCapabilities(targets, inputTokens, 0)
	if len(targets) == 0 {
		return openaiapi.EmbeddingResponse{}, gatewayError("token_limit_exceeded", "request exceeds the model deployment token limits", 400, nil)
	}
	run, err := s.beginRequestRun(ctx, principal, request.Model, targets, inputTokens, inputTokens, 0)
	if err != nil {
		return openaiapi.EmbeddingResponse{}, err
	}
	defer run.close()
	requestID, requestLease := run.requestID, run.requestLease
	var lastErr error
	attemptCount := 0
	for targetIndex, target := range targets {
		for targetTry := 0; targetTry < s.maxAttemptsPerTarget && attemptCount < s.maxAttempts; targetTry++ {
			if ctx.Err() != nil {
				lastErr = ctx.Err()
				break
			}
			if attemptCount > 0 {
				if err := s.waitRetry(ctx, attemptCount-1, lastErr); err != nil {
					lastErr = err
					break
				}
			}
			attempt, err := s.startAttempt(
				ctx, run, target, inputTokens, 0,
				targetIndex, targetTry, attemptCount+1,
			)
			if err != nil {
				var fatal *Error
				if errors.As(err, &fatal) {
					return openaiapi.EmbeddingResponse{}, fatal
				}
				lastErr = err
				break
			}
			attemptCount++
			embedding, resolveErr := target.Embedding()
			if resolveErr != nil {
				return openaiapi.EmbeddingResponse{}, gatewayError("unsupported_feature", "embedding primitive is unavailable", 400, resolveErr)
			}
			semanticResponse, providerErr := embedding.EmbedSemantic(ctx, provider.EmbedCall{RequestID: requestID, ProviderModel: target.ProviderModel, Request: canonical})
			response := openaiapi.EmbeddingResponse{}
			if providerErr == nil {
				response, err = openaiwire.RenderEmbeddingResult(semanticResponse)
				if err != nil {
					providerErr = &provider.Error{Class: provider.ErrorMalformed, Ambiguous: true, Message: "render canonical embedding result", Cause: err}
				}
			}
			settlement := embeddingSettlement(
				semanticResponse, providerErr, inputTokens, target, attempt.accounting.ReservationMicrosUSD,
			)
			if err := attempt.finish(providerErr, settlement); err != nil {
				return openaiapi.EmbeddingResponse{}, err
			}
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
				return openaiapi.EmbeddingResponse{}, terminalProviderError(providerErr)
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
	return openaiapi.EmbeddingResponse{}, s.exhaustedAttemptsError(lastErr)
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

func (s *Service) waitRetry(ctx context.Context, retryIndex int, previous error) error {
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
	var classified *provider.Error
	if errors.As(previous, &classified) && classified.RetryAfter > delay {
		delay = min(classified.RetryAfter, s.retryMaxDelay)
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

func filterSemanticCapabilities(targets []provider.Target, requirements semantic.Requirements) []provider.Target {
	return slices.DeleteFunc(slices.Clone(targets), func(target provider.Target) bool {
		capabilities := target.Capabilities
		if target.LegacyUnprofiled && (requirements.Tools || requirements.ParallelTools ||
			requirements.InputImage || requirements.StructuredJSON || requirements.DeveloperRole ||
			requirements.Reasoning || requirements.StreamUsage || requirements.Seed ||
			requirements.MultipleCandidates || requirements.EndUserReference) {
			return true
		}
		return (requirements.Tools && !capabilities.Tools) ||
			(requirements.InputImage && !capabilities.Vision) ||
			(requirements.StructuredJSON && !capabilities.JSONMode) ||
			(requirements.DeveloperRole && !capabilities.DeveloperRole) ||
			(requirements.Reasoning && !capabilities.Reasoning) ||
			(requirements.StreamUsage && !capabilities.StreamUsage)
	})
}

func filterGenerateProfileCompatibility(targets []provider.Target, request semantic.GenerateRequest) []provider.Target {
	return slices.DeleteFunc(slices.Clone(targets), func(target provider.Target) bool {
		return len(compatibility.UnsupportedGenerateFields(target.ProfileID, request)) > 0
	})
}

func filterEmbeddingProfileCompatibility(targets []provider.Target, request semantic.EmbeddingRequest) []provider.Target {
	return slices.DeleteFunc(slices.Clone(targets), func(target provider.Target) bool {
		return len(compatibility.UnsupportedEmbeddingFields(target.ProfileID, request)) > 0
	})
}

func filterPrimitiveTargets(targets []provider.Target, operation provider.Operation) []provider.Target {
	return slices.DeleteFunc(slices.Clone(targets), func(target provider.Target) bool {
		_, ok := target.ResolveOperation(operation)
		return !ok
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
	response semantic.GenerateResult,
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
		if validSemanticUsage(response.Usage) {
			result.ProviderInputTokens = response.Usage.InputTokens
			result.ProviderOutputTokens = response.Usage.OutputTokens
			setSettlementCost(&result, target, reservationMicrosUSD)
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
	if !validSemanticUsage(response.Usage) {
		result.ProviderInputTokens = estimatedInputTokens
		result.ProviderOutputTokens = estimatedOutputTokens
		result.PreparedOutputTokens = estimatedOutputTokens
		result.TokenEstimated = true
		result.CostEstimated = true
	} else {
		result.ProviderInputTokens = response.Usage.InputTokens
		result.ProviderOutputTokens = response.Usage.OutputTokens
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
	response semantic.EmbeddingResult,
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
		if validSemanticUsage(response.Usage) {
			result.ProviderInputTokens = response.Usage.InputTokens
			setSettlementCost(&result, target, reservationMicrosUSD)
			return result
		}
		result.ProviderInputTokens = estimatedInputTokens
		result.TokenEstimated = true
		result.CostEstimated = true
	} else if !validSemanticUsage(response.Usage) {
		result.ProviderInputTokens = estimatedInputTokens
		result.TokenEstimated = true
		result.CostEstimated = true
	} else {
		result.ProviderInputTokens = response.Usage.InputTokens
	}
	setSettlementCost(&result, target, reservationMicrosUSD)
	return result
}

func streamSettlement(
	usage *semantic.Usage,
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
	if validSemanticUsage(usage) {
		result.ProviderInputTokens = usage.InputTokens
		result.ProviderOutputTokens = usage.OutputTokens
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

func validSemanticUsage(usage *semantic.Usage) bool {
	if usage == nil || usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.TotalTokens < 0 {
		return false
	}
	if usage.InputTokens > maxAccountableTokens ||
		usage.OutputTokens > maxAccountableTokens ||
		usage.TotalTokens > maxAccountableTokens {
		return false
	}
	total, err := addTokens(usage.InputTokens, usage.OutputTokens)
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
	var mapped *Error
	switch classified.Class {
	case provider.ErrorBadRequest:
		mapped = gatewayError("invalid_request_error", "provider rejected the request", 400, err)
	case provider.ErrorAuthentication:
		mapped = gatewayError("provider_authentication_error", "provider authentication failed", 502, err)
	case provider.ErrorRateLimit:
		mapped = gatewayError("provider_rate_limit", "provider rate limit exceeded", 429, err)
	case provider.ErrorTimeout:
		mapped = gatewayError("provider_timeout", "provider timed out", 504, err)
	default:
		mapped = gatewayError("provider_error", "provider request failed", 502, err)
	}
	mapped.RetryAfter = classified.RetryAfter
	return mapped
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
