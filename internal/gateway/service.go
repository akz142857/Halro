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
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"time"

	"github.com/akz142857/Heimdall/internal/anthropicapi"
	"github.com/akz142857/Heimdall/internal/auth"
	"github.com/akz142857/Heimdall/internal/budget"
	"github.com/akz142857/Heimdall/internal/circuit"
	"github.com/akz142857/Heimdall/internal/compatibility"
	anthropicwire "github.com/akz142857/Heimdall/internal/compatibility/anthropic"
	openaiwire "github.com/akz142857/Heimdall/internal/compatibility/openai"
	"github.com/akz142857/Heimdall/internal/contentscan"
	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/id"
	"github.com/akz142857/Heimdall/internal/ledger"
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
	auth                          *auth.Snapshot
	registry                      *provider.Registry
	accounting                    *budget.Manager
	limiter                       *limiter.Manager
	redactor                      *redaction.Engine
	breakers                      *circuit.Manager
	maxAttempts                   int
	maxAttemptsPerTarget          int
	retryBaseDelay                time.Duration
	retryMaxDelay                 time.Duration
	retryJitter                   bool
	tokenGuard                    *tokenguard.Manager
	providerConcurrency           *provider.ConcurrencyManager
	deploymentConcurrency         *provider.ConcurrencyManager
	rejections                    rejectionCounters
	sourceHashKey                 [32]byte
	now                           func() time.Time
	resources                     Phase2ResourceStore
	resourceObjectDir             string
	contentScanner                contentscan.Scanner
	pricing                       PriceSelector
	pricingClockRollbackTolerance time.Duration
	pricingClockForwardTolerance  time.Duration
	pricingUnknownPolicy          string
}

type PriceSelector interface {
	SelectDeploymentPriceVersion(context.Context, string, time.Time) (domain.DeploymentPriceVersion, error)
}

type PricePinStore interface {
	PriceSelector
	LockDeploymentPricing(string) func()
	PrepareDeploymentPricePin(context.Context, string, string, time.Time, time.Duration, time.Duration) (domain.DeploymentPriceVersion, domain.PriceSnapshot, domain.PricePinIntent, error)
	CommitDeploymentPricePin(context.Context, string, string, uint64, time.Time) (domain.PricePinIntent, error)
	DeletePreparedDeploymentPricePin(context.Context, string) error
}

func NewService(authSnapshot *auth.Snapshot, registry *provider.Registry, accounting *budget.Manager) (*Service, error) {
	return NewServiceWithOptions(authSnapshot, registry, accounting, ServiceOptions{})
}

type ServiceOptions struct {
	MaxAttempts                   int
	CircuitFailureThreshold       int
	CircuitOpenDuration           time.Duration
	CircuitHalfOpenMaxRequests    int
	MaxAttemptsPerTarget          int
	RetryBaseDelay                time.Duration
	RetryMaxDelay                 time.Duration
	RetryJitter                   bool
	TokenGuard                    *tokenguard.Manager
	Redactor                      *redaction.Engine
	Resources                     Phase2ResourceStore
	ResourceObjectDir             string
	ContentScanner                contentscan.Scanner
	Pricing                       PriceSelector
	PricingClockRollbackTolerance time.Duration
	PricingClockForwardTolerance  time.Duration
	PricingUnknownPolicy          string

	// Now overrides the clock the whole service reads: authentication
	// timestamps, price selection, rate-limit buckets and Token Guard windows
	// all come from it. Without it a test in another package cannot fix the
	// gateway's idea of the time, which is how a "passes today, fails tomorrow"
	// test gets written. Nil selects time.Now.
	Now func() time.Time
}

type requestRun struct {
	service                     *Service
	principal                   auth.AuthResult
	policyLease                 *limiter.Lease
	tokenGuardLease             *tokenguard.Lease
	requestLease                budget.Request
	requestID                   string
	actualTPMTokens             int64
	providerCalled              bool
	providerFailed              bool
	tokenGuardPricingViewDigest string
}

type activeAttempt struct {
	service       *Service
	run           *requestRun
	accounting    budget.Attempt
	breaker       *circuit.Lease
	concurrency   *targetConcurrencyLease
	startedAt     time.Time
	pricingTarget provider.Target
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
	tokenGuardLease, pricingViewDigest, err := s.admitTokenGuard(ctx, principal, targets, totalTokens, inputTokens, outputTokens)
	if err != nil {
		return nil, err
	}
	policyLease, err := s.limiter.Acquire(principal.Project, totalTokens, s.now())
	if err != nil {
		tokenGuardLease.Release()
		return nil, s.mapLimitError(err)
	}
	requestID, supplied := requestmeta.RequestID(ctx)
	if !supplied {
		requestID, err = id.New("req")
		if err != nil {
			policyLease.Release()
			tokenGuardLease.Release()
			return nil, gatewayError("internal_error", "unable to create request ID", 500, err)
		}
	}
	requestLease, err := s.accounting.BeginRequestDetailed(
		ctx, principal.Project.ID, principal.Key.ID, requestID, model,
	)
	if err != nil {
		policyLease.Release()
		tokenGuardLease.Release()
		return nil, gatewayError("accounting_unavailable", "accounting is unavailable", 503, err)
	}
	return &requestRun{
		service: s, principal: principal, policyLease: policyLease, tokenGuardLease: tokenGuardLease,
		requestLease: requestLease, requestID: requestID, tokenGuardPricingViewDigest: pricingViewDigest,
	}, nil
}

func (run *requestRun) close() {
	run.tokenGuardLease.Release()
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

// exhaustedAttemptsError maps whatever stopped the last attempt onto a response.
//
// One of its cases never has to wait for the attempts to be exhausted: the daily
// budget belongs to the project, not to a target, so no other candidate can
// change the answer. Walking the rest of them re-runs price selection, takes the
// pricing lock, writes a pin, fails the same check and deletes the pin again —
// once per candidate, on every request, for as long as the budget stays spent.
// The callers return on budget.ErrExceeded rather than continuing.
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
		breakerLease.Abandon()
		if errors.Is(err, errDeploymentConcurrency) {
			s.rejections.deploymentConcurrency.Add(1)
		} else {
			s.rejections.providerConcurrency.Add(1)
		}
		return nil, err
	}
	var pricingUnlock func()
	var pinStore PricePinStore
	var pinIntent *domain.PricePinIntent
	var unknownPolicyEvidence *domain.UnknownPricePolicyEvidence
	forcedAttemptID := ""
	reservation, leaseMode, snapshot, pricedTarget := int64(0), ledger.LeaseMode(""), (*domain.PriceSnapshot)(nil), target
	if candidate, ok := s.pricing.(PricePinStore); ok && target.DeploymentID != "" {
		pinStore = candidate
		pricingUnlock = pinStore.LockDeploymentPricing(target.DeploymentID)
		forcedAttemptID, err = id.New("att")
		if err == nil {
			pricingSelectedAt := s.now().UTC()
			var price domain.DeploymentPriceVersion
			var captured domain.PriceSnapshot
			var intent domain.PricePinIntent
			price, captured, intent, err = pinStore.PrepareDeploymentPricePin(ctx, target.DeploymentID, forcedAttemptID, pricingSelectedAt, s.pricingClockRollbackTolerance, s.pricingClockForwardTolerance)
			if err == nil {
				reservation, leaseMode, snapshot, pricedTarget, err = accountingTermsFromSnapshot(price, captured, target, inputTokens, outputTokens)
				pinIntent = &intent
			} else if errors.Is(err, domain.ErrPriceUnavailable) {
				unknownPolicyEvidence, err = s.unknownPricePolicyEvidence(run.principal)
				if err == nil {
					captured = domain.NewUnknownPriceSnapshot(pricingSelectedAt)
					reservation, leaseMode, snapshot, pricedTarget = 0, ledger.LeaseModeUnknownAllowed, &captured, target
				}
			}
		}
	} else {
		reservation, leaseMode, snapshot, pricedTarget, err = s.prepareAccountingLease(ctx, target, inputTokens, outputTokens)
	}
	if err != nil {
		if pinIntent != nil {
			_ = pinStore.DeletePreparedDeploymentPricePin(context.Background(), pinIntent.AttemptID)
		}
		if pricingUnlock != nil {
			pricingUnlock()
		}
		providerLease.Release()
		breakerLease.Abandon()
		finalizeErr := s.finalizeRequest(run.requestLease, "accounting_error")
		return nil, gatewayError(
			"accounting_unavailable", "accounting is unavailable", 503,
			errors.Join(err, finalizeErr),
		)
	}
	if decision := s.tokenGuard.RecheckCost(run.tokenGuardLease, reservation, s.now()); !decision.Allowed {
		if pinIntent != nil {
			_ = pinStore.DeletePreparedDeploymentPricePin(context.Background(), pinIntent.AttemptID)
		}
		if pricingUnlock != nil {
			pricingUnlock()
		}
		providerLease.Release()
		breakerLease.Abandon()
		s.rejections.tokenGuard.Add(1)
		finalizeErr := s.finalizeRequest(run.requestLease, "token_guard_rejected")
		return nil, gatewayError("token_guard_blocked", "the current attempt price exceeds Token Guard cost limits", http.StatusForbidden, finalizeErr)
	}
	metadata := budget.AttemptMetadata{
		RouteID: target.ID, DeploymentID: target.DeploymentID,
		ProviderID: target.ProviderID, ProviderModel: target.ProviderModel,
		AttemptNumber: attemptNumber, RetryCount: targetTry, FallbackCount: targetIndex,
	}
	var attempt budget.Attempt
	if snapshot == nil {
		attempt, err = s.accounting.ReserveAttemptDetailed(ctx, run.requestLease, run.principal.Project.DailyBudgetMicrosUSD, reservation, metadata)
	} else {
		attempt, err = s.accounting.ReserveLeaseDetailed(ctx, run.requestLease, run.principal.Project.DailyBudgetMicrosUSD, budget.LeaseSpec{
			AttemptID: forcedAttemptID,
			Mode:      leaseMode, ReservationMicrosUSD: reservation, PriceSnapshot: snapshot,
			PreparedInputTokens: inputTokens, PreparedOutputTokens: outputTokens,
			RecoveryKey:                 "accounting-recovery-v1",
			UnknownPolicyEvidence:       unknownPolicyEvidence,
			TokenGuardPricingViewDigest: run.tokenGuardPricingViewDigest,
		}, metadata)
	}
	if err != nil {
		if pinIntent != nil {
			_ = pinStore.DeletePreparedDeploymentPricePin(context.Background(), pinIntent.AttemptID)
		}
		if pricingUnlock != nil {
			pricingUnlock()
		}
		providerLease.Release()
		breakerLease.Abandon()
		if errors.Is(err, budget.ErrExceeded) {
			return nil, err
		}
		finalizeErr := s.finalizeRequest(run.requestLease, "accounting_error")
		return nil, gatewayError(
			"accounting_unavailable", "accounting is unavailable", 503,
			errors.Join(err, finalizeErr),
		)
	}
	if pinIntent != nil {
		if _, err := pinStore.CommitDeploymentPricePin(ctx, attempt.AttemptID, pinIntent.SnapshotSHA256, attempt.ReservationSequence, s.now().UTC()); err != nil {
			pricingUnlock()
			providerLease.Release()
			breakerLease.Abandon()
			cleanupErr := s.settleAttempt(attempt, budget.Settlement{Outcome: "pin_commit_failed"})
			finalizeErr := s.finalizeRequest(run.requestLease, "accounting_error")
			return nil, gatewayError("accounting_unavailable", "accounting price pin could not be committed", 503, errors.Join(err, cleanupErr, finalizeErr))
		}
	}
	if pricingUnlock != nil {
		pricingUnlock()
	}
	if err := s.accounting.MarkStarted(ctx, attempt); err != nil {
		providerLease.Release()
		breakerLease.Abandon()
		cleanupErr := s.settleAttempt(attempt, budget.Settlement{Outcome: "start_failed"})
		finalizeErr := s.finalizeRequest(run.requestLease, "accounting_error")
		return nil, gatewayError(
			"accounting_unavailable", "accounting is unavailable", 503,
			errors.Join(err, cleanupErr, finalizeErr),
		)
	}
	return &activeAttempt{
		service: s, run: run, accounting: attempt, breaker: breakerLease,
		concurrency: providerLease, startedAt: s.now(), pricingTarget: pricedTarget,
	}, nil
}

func accountingTermsFromSnapshot(price domain.DeploymentPriceVersion, snapshot domain.PriceSnapshot, target provider.Target, inputTokens, outputTokens int64) (int64, ledger.LeaseMode, *domain.PriceSnapshot, provider.Target, error) {
	cost, err := snapshot.Calculate(inputTokens, outputTokens)
	if err != nil {
		return 0, "", nil, target, gatewayError("accounting_error", "unable to estimate request cost", http.StatusServiceUnavailable, err)
	}
	mode := ledger.LeaseModeMetered
	if price.BillingMode == domain.BillingModeFree {
		mode = ledger.LeaseModeFree
	}
	priced := target
	priced.InputMicrosPerMillion = *snapshot.InputMicrosPerMillion
	priced.OutputMicrosPerMillion = *snapshot.OutputMicrosPerMillion
	priced.FixedRequestMicrosUSD = *snapshot.FixedRequestMicrosUSD
	return cost.TotalCostMicrosUSD, mode, &snapshot, priced, nil
}

func (s *Service) prepareAccountingLease(ctx context.Context, target provider.Target, inputTokens, outputTokens int64) (int64, ledger.LeaseMode, *domain.PriceSnapshot, provider.Target, error) {
	if s.pricing == nil || target.DeploymentID == "" {
		reservation, err := estimateReservation(inputTokens, outputTokens, target)
		return reservation, "", nil, target, err
	}
	selectedAt := s.now().UTC()
	price, err := s.pricing.SelectDeploymentPriceVersion(ctx, target.DeploymentID, selectedAt)
	if err != nil {
		return 0, "", nil, target, gatewayError("price_unavailable", "an effective price is unavailable", http.StatusServiceUnavailable, err)
	}
	snapshot, err := domain.NewVersionedPriceSnapshot(price, selectedAt)
	if err != nil {
		return 0, "", nil, target, gatewayError("accounting_error", "unable to snapshot request price", http.StatusServiceUnavailable, err)
	}
	cost, err := snapshot.Calculate(inputTokens, outputTokens)
	if err != nil {
		return 0, "", nil, target, gatewayError("accounting_error", "unable to estimate request cost", http.StatusServiceUnavailable, err)
	}
	mode := ledger.LeaseModeMetered
	if snapshot.BillingMode == domain.BillingModeFree {
		mode = ledger.LeaseModeFree
	}
	priced := target
	priced.InputMicrosPerMillion = *snapshot.InputMicrosPerMillion
	priced.OutputMicrosPerMillion = *snapshot.OutputMicrosPerMillion
	priced.FixedRequestMicrosUSD = *snapshot.FixedRequestMicrosUSD
	return cost.TotalCostMicrosUSD, mode, &snapshot, priced, nil
}

func (attempt *activeAttempt) finish(providerErr error, settlement budget.Settlement) error {
	if attempt.accounting.LeaseMode == ledger.LeaseModeUnknownAllowed {
		settlement.CommittedMicrosUSD = 0
		settlement.CostEstimated = false
	}
	attempt.concurrency.Release()
	attempt.run.recordProviderResult(providerErr, settlement)
	enrichSettlement(&settlement, providerErr, attempt.startedAt, attempt.service.now())
	if err := attempt.service.settleAttempt(attempt.accounting, settlement); err != nil {
		attempt.reportBreaker(providerErr)
		finalizeErr := attempt.service.finalizeRequest(attempt.run.requestLease, "accounting_error")
		return gatewayError(
			"accounting_unavailable", "request accounting could not be finalized", 503,
			errors.Join(err, finalizeErr),
		)
	}
	attempt.reportBreaker(providerErr)
	return nil
}

// abort releases everything startAttempt took, for a request that fails after
// the attempt exists but before the provider is ever called. finish is the wrong
// tool there: it judges the target, and a local failure says nothing about the
// upstream's health — so the breaker is abandoned, which returns a half-open
// probe slot without counting as either a success or a failure.
//
// Every caller of startAttempt needs this on its local-failure paths. Writing it
// out five times is how three of them came to be missing it.
func (attempt *activeAttempt) abort(outcome string) error {
	attempt.concurrency.Release()
	attempt.breaker.Abandon()
	cleanupErr := attempt.service.settleAttempt(attempt.accounting, budget.Settlement{Outcome: outcome})
	finalizeErr := attempt.service.finalizeRequest(attempt.run.requestLease, outcome)
	return errors.Join(cleanupErr, finalizeErr)
}

// reportBreaker judges the target on the attempt's outcome, except when the
// caller went away. A cancelled read surfaces from the transport as a truncated
// response, which is indistinguishable from the provider cutting the stream, so
// without this a wave of client disconnects — a frontend deploy, a gateway
// restart — would open circuits on providers that never faltered.
func (attempt *activeAttempt) reportBreaker(providerErr error) {
	if providerErr != nil && errors.Is(providerErr, context.Canceled) {
		attempt.breaker.Abandon()
		return
	}
	attempt.breaker.Done(availabilityFailure(providerErr), attempt.service.now())
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
	if options.PricingClockRollbackTolerance <= 0 {
		options.PricingClockRollbackTolerance = 2 * time.Second
	}
	if options.PricingClockForwardTolerance <= 0 {
		options.PricingClockForwardTolerance = 30 * time.Second
	}
	if options.PricingUnknownPolicy == "" {
		options.PricingUnknownPolicy = "reject"
	}
	if options.PricingUnknownPolicy != "reject" && options.PricingUnknownPolicy != "allow_without_cost_governance" {
		return nil, errors.New("pricing unknown policy must be reject or allow_without_cost_governance")
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
	if options.ContentScanner == nil {
		options.ContentScanner = contentscan.Builtin{}
	}
	if options.ResourceObjectDir != "" {
		options.ResourceObjectDir = filepath.Clean(options.ResourceObjectDir)
		if !filepath.IsAbs(options.ResourceObjectDir) {
			return nil, errors.New("resource object directory must be absolute")
		}
		if err := os.MkdirAll(options.ResourceObjectDir, 0o700); err != nil {
			return nil, fmt.Errorf("create resource object directory: %w", err)
		}
		if err := os.Chmod(options.ResourceObjectDir, 0o700); err != nil {
			return nil, fmt.Errorf("secure resource object directory: %w", err)
		}
	}
	var sourceHashKey [32]byte
	if _, err := cryptorand.Read(sourceHashKey[:]); err != nil {
		return nil, errors.New("generate source hashing key")
	}
	clock := options.Now
	if clock == nil {
		clock = time.Now
	}
	return &Service{
		auth:                          authSnapshot,
		registry:                      registry,
		accounting:                    accounting,
		limiter:                       limiter.New(),
		redactor:                      options.Redactor,
		breakers:                      breakers,
		maxAttempts:                   options.MaxAttempts,
		maxAttemptsPerTarget:          options.MaxAttemptsPerTarget,
		retryBaseDelay:                options.RetryBaseDelay,
		retryMaxDelay:                 options.RetryMaxDelay,
		retryJitter:                   options.RetryJitter,
		tokenGuard:                    options.TokenGuard,
		providerConcurrency:           provider.NewConcurrencyManager(),
		deploymentConcurrency:         provider.NewConcurrencyManager(),
		sourceHashKey:                 sourceHashKey,
		now:                           clock,
		resources:                     options.Resources,
		resourceObjectDir:             options.ResourceObjectDir,
		contentScanner:                options.ContentScanner,
		pricing:                       options.Pricing,
		pricingClockRollbackTolerance: options.PricingClockRollbackTolerance,
		pricingClockForwardTolerance:  options.PricingClockForwardTolerance,
		pricingUnknownPolicy:          options.PricingUnknownPolicy,
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
				if errors.Is(err, budget.ErrExceeded) {
					return openaiapi.ChatCompletionResponse{}, s.exhaustedAttemptsError(err)
				}
				lastErr = err
				break
			}
			attemptCount++
			generation, resolveErr := target.Generation(provider.OperationChat)
			if resolveErr != nil {
				abortErr := attempt.abort("unsupported_feature")
				return openaiapi.ChatCompletionResponse{}, gatewayError("unsupported_feature", "generation primitive is unavailable", 400, errors.Join(resolveErr, abortErr))
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
				semanticResponse, providerErr, inputTokens, outputTokens, attempt.pricingTarget, attempt.accounting.ReservationMicrosUSD,
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

// Responses implements the Phase 1A stateless Responses facade by translating
// the declared portable subset to the existing semantic generation hot path.
func (s *Service) Responses(
	ctx context.Context,
	plaintextKey string,
	request openaiapi.ResponseRequest,
) (openaiapi.Response, error) {
	if request.Stream {
		return openaiapi.Response{}, gatewayError("invalid_request_error", "stream must be false", 400, nil)
	}
	canonical, err := openaiwire.DecodeResponseGenerate(request)
	if err != nil {
		return openaiapi.Response{}, gatewayError("invalid_request_error", "request cannot be represented safely", 400, err)
	}
	chatRequest, err := openaiwire.RenderGenerateRequest(canonical, request.Model)
	if err != nil {
		return openaiapi.Response{}, gatewayError("invalid_request_error", "request cannot be translated safely", 400, err)
	}
	chatResponse, err := s.Chat(ctx, plaintextKey, chatRequest)
	if err != nil {
		return openaiapi.Response{}, err
	}
	result, err := openaiwire.DecodeGenerateResult(chatResponse)
	if err != nil {
		return openaiapi.Response{}, gatewayError("provider_error", "provider response cannot be represented safely", 502, err)
	}
	response, err := openaiwire.RenderResponseResult(result, request)
	if err != nil {
		return openaiapi.Response{}, gatewayError("provider_error", "provider response cannot be rendered safely", 502, err)
	}
	return response, nil
}

func (s *Service) ResponsesStream(
	ctx context.Context,
	plaintextKey string,
	request openaiapi.ResponseRequest,
	emit func(openaiapi.ResponseStreamEvent) error,
) error {
	if !request.Stream {
		return gatewayError("invalid_request_error", "stream must be true", 400, nil)
	}
	canonical, err := openaiwire.DecodeResponseGenerate(request)
	if err != nil {
		return gatewayError("invalid_request_error", "request cannot be represented safely", 400, err)
	}
	chatRequest, err := openaiwire.RenderGenerateRequest(canonical, request.Model)
	if err != nil {
		return gatewayError("invalid_request_error", "request cannot be translated safely", 400, err)
	}
	renderer := openaiwire.NewResponseStreamRenderer(request)
	emittedResponseEvent := false
	streamTranslationError := func(err error) error {
		if err == nil || !emittedResponseEvent {
			return err
		}
		return &provider.Error{Class: provider.ErrorMalformed, Ambiguous: true, Message: "Responses stream failed after payload", Cause: err}
	}
	err = s.ChatStream(ctx, plaintextKey, chatRequest, func(chunk openaiapi.ChatCompletionResponse) error {
		event, decodeErr := openaiwire.DecodeEvent(chunk)
		if decodeErr != nil {
			return streamTranslationError(decodeErr)
		}
		events, renderErr := renderer.Accept(event)
		if renderErr != nil {
			return streamTranslationError(renderErr)
		}
		for _, responseEvent := range events {
			if emitErr := emit(responseEvent); emitErr != nil {
				return streamTranslationError(emitErr)
			}
			emittedResponseEvent = true
		}
		return nil
	})
	if err != nil {
		return err
	}
	events, err := renderer.Complete()
	if err != nil {
		return gatewayError("provider_error", "provider stream cannot be completed safely", 502, err)
	}
	for _, event := range events {
		if err := emit(event); err != nil {
			return err
		}
	}
	return nil
}

// Messages implements the portable tier of the Anthropic Messages facade.
// Native mode has a separate, profile-pinned hot path so it cannot
// accidentally inherit portable fallback behavior.
func (s *Service) Messages(
	ctx context.Context,
	plaintextKey string,
	request anthropicapi.MessageRequest,
) (anthropicapi.Message, error) {
	if request.Stream {
		return anthropicapi.Message{}, gatewayError("invalid_request_error", "stream must be false", 400, nil)
	}
	canonical, err := anthropicwire.DecodePortable(request)
	if err != nil {
		return anthropicapi.Message{}, gatewayError("invalid_request_error", "request is not portable; use native mode with an Anthropic route", 400, err)
	}
	chatRequest, err := openaiwire.RenderGenerateRequest(canonical, request.Model)
	if err != nil {
		return anthropicapi.Message{}, gatewayError("invalid_request_error", "request cannot be translated safely", 400, err)
	}
	chatResponse, err := s.Chat(ctx, plaintextKey, chatRequest)
	if err != nil {
		return anthropicapi.Message{}, err
	}
	result, err := openaiwire.DecodeGenerateResult(chatResponse)
	if err != nil {
		return anthropicapi.Message{}, gatewayError("provider_error", "provider response cannot be represented safely", 502, err)
	}
	message, err := anthropicwire.RenderResult(result, request.Model)
	if err != nil {
		return anthropicapi.Message{}, gatewayError("provider_error", "provider response cannot be rendered safely", 502, err)
	}
	return message, nil
}

func (s *Service) MessagesStream(
	ctx context.Context,
	plaintextKey string,
	request anthropicapi.MessageRequest,
	emit func(anthropicapi.StreamEvent) error,
) error {
	if !request.Stream {
		return gatewayError("invalid_request_error", "stream must be true", 400, nil)
	}
	canonical, err := anthropicwire.DecodePortable(request)
	if err != nil {
		return gatewayError("invalid_request_error", "request is not portable; use native mode with an Anthropic route", 400, err)
	}
	chatRequest, err := openaiwire.RenderGenerateRequest(canonical, request.Model)
	if err != nil {
		return gatewayError("invalid_request_error", "request cannot be translated safely", 400, err)
	}
	renderer := anthropicwire.NewStreamRenderer(request.Model)
	emitted := false
	err = s.ChatStream(ctx, plaintextKey, chatRequest, func(chunk openaiapi.ChatCompletionResponse) error {
		event, decodeErr := openaiwire.DecodeEvent(chunk)
		if decodeErr != nil {
			return decodeErr
		}
		events, renderErr := renderer.Accept(event)
		if renderErr != nil {
			return renderErr
		}
		for _, event := range events {
			if emitErr := emit(event); emitErr != nil {
				return emitErr
			}
			emitted = true
		}
		return nil
	})
	if err != nil {
		if emitted {
			return &provider.Error{Class: provider.ErrorMalformed, Ambiguous: true, Message: "Anthropic stream failed after payload", Cause: err}
		}
		return err
	}
	events, err := renderer.Complete()
	if err != nil {
		return gatewayError("provider_error", "provider stream cannot be completed safely", 502, err)
	}
	for _, event := range events {
		if err := emit(event); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) MessagesNative(ctx context.Context, plaintextKey, version string, request anthropicapi.MessageRequest) (anthropicapi.Message, error) {
	if request.Stream {
		return anthropicapi.Message{}, gatewayError("invalid_request_error", "stream must be false", 400, nil)
	}
	principal, target, envelope, inputTokens, outputTokens, err := s.prepareNativeMessages(ctx, plaintextKey, version, request, provider.OperationMessages)
	if err != nil {
		return anthropicapi.Message{}, err
	}
	if err := s.checkNativeInboundRedaction(principal, request); err != nil {
		return anthropicapi.Message{}, err
	}
	totalTokens, err := addTokens(inputTokens, outputTokens)
	if err != nil {
		return anthropicapi.Message{}, gatewayError("token_limit_exceeded", "requested token count is too large", 400, err)
	}
	run, err := s.beginRequestRun(ctx, principal, request.Model, []provider.Target{target}, totalTokens, inputTokens, outputTokens)
	if err != nil {
		return anthropicapi.Message{}, err
	}
	defer run.close()
	attempt, err := s.startAttempt(ctx, run, target, inputTokens, outputTokens, 0, 0, 1)
	if err != nil {
		return anthropicapi.Message{}, s.exhaustedAttemptsError(err)
	}
	adapter, err := target.NativeMessages(false)
	if err != nil {
		abortErr := attempt.abort("unsupported_feature")
		return anthropicapi.Message{}, gatewayError("unsupported_feature", "native Messages primitive is unavailable", 400, errors.Join(err, abortErr))
	}
	payload, _ := envelope.PayloadFor(target.ProfileID, 1, compatibility.NativeRequest)
	result, providerErr := adapter.MessagesNative(ctx, provider.NativeMessageCall{RequestID: run.requestID, ProviderModel: target.ProviderModel, Version: version, Payload: payload})
	var message anthropicapi.Message
	var semanticResult semantic.GenerateResult
	if providerErr == nil {
		registry, _ := anthropicwire.NewNativeSchemaRegistry()
		identity := nativeIdentity(principal, target, request.Model)
		responseEnvelope, envelopeErr := compatibility.NewNativeResponseEnvelope(registry, target.ProfileID, 1, http.Header{}, result.Payload, identity)
		if envelopeErr != nil {
			providerErr = &provider.Error{Class: provider.ErrorMalformed, Ambiguous: true, Message: "validate native Anthropic response", Cause: envelopeErr}
		} else {
			safePayload, _ := responseEnvelope.PayloadFor(target.ProfileID, 1, compatibility.NativeResponse)
			message, providerErr = anthropicapi.DecodeMessage(safePayload)
			if providerErr == nil {
				providerErr = s.checkNativeOutboundRedaction(principal, message)
			}
		}
	}
	if message.ID != "" {
		usage := semantic.Usage{InputTokens: message.Usage.InputTokens, OutputTokens: message.Usage.OutputTokens, TotalTokens: message.Usage.InputTokens + message.Usage.OutputTokens, Source: semantic.UsageProviderReported}
		semanticResult.Usage = &usage
	}
	settlement := settlementForResult(semanticResult, providerErr, inputTokens, outputTokens, attempt.pricingTarget, attempt.accounting.ReservationMicrosUSD)
	if err := attempt.finish(providerErr, settlement); err != nil {
		return anthropicapi.Message{}, err
	}
	outcome := "success"
	if providerErr != nil {
		outcome = "provider_error"
	}
	if err := s.finalizeRequest(run.requestLease, outcome); err != nil {
		return anthropicapi.Message{}, gatewayError("accounting_unavailable", "request accounting could not be finalized", 503, err)
	}
	if providerErr != nil {
		return anthropicapi.Message{}, terminalProviderError(providerErr)
	}
	message.Model = request.Model
	return message, nil
}

func (s *Service) MessagesNativeStream(ctx context.Context, plaintextKey, version string, request anthropicapi.MessageRequest, emit func(anthropicapi.RawStreamEvent) error) error {
	if !request.Stream {
		return gatewayError("invalid_request_error", "stream must be true", 400, nil)
	}
	principal, target, envelope, inputTokens, outputTokens, err := s.prepareNativeMessages(ctx, plaintextKey, version, request, provider.OperationMessagesStream)
	if err != nil {
		return err
	}
	if err := s.checkNativeInboundRedaction(principal, request); err != nil {
		return err
	}
	if !s.redactor.AllowsStreaming(principal.Project.RedactionPolicyID) {
		return gatewayError("streaming_redaction_incompatible", "streaming is disabled by the Project redaction policy", 400, nil)
	}
	streamRedactor, err := s.redactor.NewStream(principal.Project.RedactionPolicyID)
	if err != nil {
		return gatewayError("streaming_redaction_incompatible", "streaming is disabled by the Project redaction policy", 400, err)
	}
	totalTokens, err := addTokens(inputTokens, outputTokens)
	if err != nil {
		return gatewayError("token_limit_exceeded", "requested token count is too large", 400, err)
	}
	run, err := s.beginRequestRun(ctx, principal, request.Model, []provider.Target{target}, totalTokens, inputTokens, outputTokens)
	if err != nil {
		return err
	}
	defer run.close()
	attempt, err := s.startAttempt(ctx, run, target, inputTokens, outputTokens, 0, 0, 1)
	if err != nil {
		return s.exhaustedAttemptsError(err)
	}
	adapter, err := target.NativeMessages(true)
	if err != nil {
		abortErr := attempt.abort("unsupported_feature")
		return gatewayError("unsupported_feature", "native Messages stream primitive is unavailable", 400, errors.Join(err, abortErr))
	}
	payload, _ := envelope.PayloadFor(target.ProfileID, 1, compatibility.NativeRequest)
	registry, _ := anthropicwire.NewNativeSchemaRegistry()
	identity := nativeIdentity(principal, target, request.Model)
	emitted := false
	deliveredBytes := int64(0)
	providerErrorEvent := false
	usage, providerErr := adapter.MessagesNativeStream(ctx, provider.NativeMessageCall{RequestID: run.requestID, ProviderModel: target.ProviderModel, Version: version, Payload: payload}, func(event anthropicapi.RawStreamEvent) error {
		eventEnvelope, envelopeErr := compatibility.NewNativeEventEnvelope(registry, target.ProfileID, 1, http.Header{}, event.Data, identity)
		if envelopeErr != nil {
			return envelopeErr
		}
		safePayload, _ := eventEnvelope.PayloadFor(target.ProfileID, 1, compatibility.NativeEvent)
		if event.Type == "message_start" {
			safePayload, envelopeErr = rewriteAnthropicStreamModel(safePayload, request.Model)
			if envelopeErr != nil {
				return envelopeErr
			}
		}
		if redactionErr := verifyNativeStreamRedaction(streamRedactor, event.Type, safePayload); redactionErr != nil {
			return redactionErr
		}
		if emitErr := emit(anthropicapi.RawStreamEvent{Type: event.Type, Data: safePayload}); emitErr != nil {
			return emitErr
		}
		deliveredBytes += int64(len(safePayload))
		emitted = true
		providerErrorEvent = providerErrorEvent || event.Type == "error"
		return nil
	})
	semanticUsage := (*semantic.Usage)(nil)
	if usage != nil {
		semanticUsage = &semantic.Usage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, TotalTokens: usage.InputTokens + usage.OutputTokens, Source: semantic.UsageProviderReported}
	}
	settlement := streamSettlement(semanticUsage, providerErr, emitted, inputTokens, outputTokens,
		estimateInputTokens(deliveredBytes), attempt.pricingTarget, attempt.accounting.ReservationMicrosUSD)
	if err := attempt.finish(providerErr, settlement); err != nil {
		return err
	}
	outcome := "success"
	if providerErr != nil {
		outcome = "provider_error"
	}
	if errors.Is(providerErr, redaction.ErrPolicyRejected) {
		outcome = "policy_rejected"
	}
	if err := s.finalizeRequest(run.requestLease, outcome); err != nil {
		return gatewayError("accounting_unavailable", "request accounting could not be finalized", 503, err)
	}
	if providerErr != nil {
		if providerErrorEvent {
			// The provider-native error event has already been delivered using
			// Anthropic's schema; returning another error would duplicate it.
			return nil
		}
		if emitted {
			return &provider.Error{Class: provider.ErrorMalformed, Ambiguous: true, Message: "native Anthropic stream failed after payload", Cause: providerErr}
		}
		return terminalProviderError(providerErr)
	}
	return nil
}

func rewriteAnthropicStreamModel(payload json.RawMessage, publicModel string) (json.RawMessage, error) {
	var event map[string]json.RawMessage
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, err
	}
	var message map[string]json.RawMessage
	if err := json.Unmarshal(event["message"], &message); err != nil {
		return nil, err
	}
	encodedModel, _ := json.Marshal(publicModel)
	message["model"] = encodedModel
	encodedMessage, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	event["message"] = encodedMessage
	return json.Marshal(event)
}

func (s *Service) prepareNativeMessages(ctx context.Context, plaintextKey, version string, request anthropicapi.MessageRequest, operation provider.Operation) (auth.AuthResult, provider.Target, *compatibility.NativeEnvelope, int64, int64, error) {
	principal, targets, err := s.resolveRequest(ctx, plaintextKey, request.Model, operation, "model route does not support native Anthropic Messages")
	if err != nil {
		return auth.AuthResult{}, provider.Target{}, nil, 0, 0, err
	}
	targets = slices.DeleteFunc(targets, func(target provider.Target) bool {
		return !isNativeAnthropicProfile(target.ProfileID, target.AccessSurface)
	})
	if len(targets) == 0 {
		return auth.AuthResult{}, provider.Target{}, nil, 0, 0, gatewayError("unsupported_feature", "native mode requires an Anthropic Messages provider profile", 400, nil)
	}
	target := targets[0]
	registry, err := anthropicwire.NewNativeSchemaRegistry()
	if err != nil {
		return auth.AuthResult{}, provider.Target{}, nil, 0, 0, gatewayError("internal_error", "native schema is unavailable", 500, err)
	}
	envelope, err := compatibility.NewNativeEnvelope(registry, target.ProfileID, 1, anthropicwire.NativeHeaders(version), request.Raw, nativeIdentity(principal, target, request.Model))
	if err != nil {
		return auth.AuthResult{}, provider.Target{}, nil, 0, 0, gatewayError("invalid_request_error", "native request failed schema validation", 400, err)
	}
	governance := envelope.Governance()
	inputTokens, outputTokens := governance.EstimatedInputTokens, governance.EstimatedOutputTokens
	if principal.Project.MaxInputTokens > 0 && inputTokens > principal.Project.MaxInputTokens {
		return auth.AuthResult{}, provider.Target{}, nil, 0, 0, gatewayError("token_limit_exceeded", "estimated input tokens exceed the project limit", 400, nil)
	}
	if principal.Project.MaxOutputTokens > 0 && outputTokens > principal.Project.MaxOutputTokens {
		return auth.AuthResult{}, provider.Target{}, nil, 0, 0, gatewayError("token_limit_exceeded", "requested output tokens exceed the project limit", 400, nil)
	}
	if len(filterTokenCapabilities([]provider.Target{target}, inputTokens, outputTokens)) == 0 {
		return auth.AuthResult{}, provider.Target{}, nil, 0, 0, gatewayError("token_limit_exceeded", "request exceeds the model deployment token limits", 400, nil)
	}
	return principal, target, envelope, inputTokens, outputTokens, nil
}

func isNativeAnthropicProfile(profileID domain.ProviderProfileID, surface domain.AccessSurface) bool {
	return profileID == domain.ProfileAnthropicMessages && surface == domain.SurfaceAnthropic ||
		profileID == domain.ProfileBedrockMantleAnthropicMessages && surface == domain.SurfaceBedrockMantle
}

func nativeIdentity(principal auth.AuthResult, target provider.Target, model string) compatibility.NativeIdentity {
	return compatibility.NativeIdentity{ProjectID: principal.Project.ID, PrincipalID: principal.Key.ID, CredentialRef: "cred_" + target.ProviderID, RouteID: target.ID, RequestedModel: model}
}

func (s *Service) checkNativeInboundRedaction(principal auth.AuthResult, request anthropicapi.MessageRequest) error {
	projection := request
	projection.Thinking = nil
	projection.Metadata = nil
	projection.ServiceTier = ""
	projection.TopK = nil
	projection.Raw = nil
	for messageIndex := range projection.Messages {
		blocks := projection.Messages[messageIndex].Content[:0]
		for _, block := range projection.Messages[messageIndex].Content {
			if block.Type != "thinking" && block.Type != "redacted_thinking" {
				blocks = append(blocks, block)
			}
		}
		projection.Messages[messageIndex].Content = blocks
	}
	canonical, err := anthropicwire.DecodePortable(projection)
	if err != nil {
		return gatewayError("invalid_request_error", "native request cannot be inspected safely", 400, err)
	}
	chat, err := openaiwire.RenderGenerateRequest(canonical, request.Model)
	if err != nil {
		return gatewayError("invalid_request_error", "native request cannot be inspected safely", 400, err)
	}
	processed, err := s.redactor.ProcessInboundChat(principal.Project.RedactionPolicyID, chat)
	if err != nil {
		return gatewayError("sensitive_data_detected", "request contains secret material", 400, err)
	}
	left, _ := json.Marshal(chat)
	right, _ := json.Marshal(processed)
	if !bytes.Equal(left, right) {
		return gatewayError("native_redaction_incompatible", "native payload would require rewriting", 400, redaction.ErrPolicyRejected)
	}
	return nil
}

func (s *Service) checkNativeOutboundRedaction(principal auth.AuthResult, message anthropicapi.Message) error {
	projection := message
	blocks := projection.Content[:0]
	for _, block := range projection.Content {
		if block.Type != "thinking" && block.Type != "redacted_thinking" {
			blocks = append(blocks, block)
		}
	}
	projection.Content = blocks
	result, err := anthropicwire.DecodeResult(projection)
	if err != nil {
		return &provider.Error{Class: provider.ErrorMalformed, Ambiguous: true, Message: "native response cannot be inspected safely", Cause: err}
	}
	chat, err := openaiwire.RenderGenerateResult(result)
	if err != nil {
		return err
	}
	processed, err := s.redactor.ProcessOutboundChat(principal.Project.RedactionPolicyID, chat)
	if err != nil {
		return redaction.ErrPolicyRejected
	}
	left, _ := json.Marshal(chat)
	right, _ := json.Marshal(processed)
	if !bytes.Equal(left, right) {
		return redaction.ErrPolicyRejected
	}
	return nil
}

func verifyNativeStreamRedaction(stream *redaction.Stream, eventType string, payload []byte) error {
	if eventType != "content_block_delta" {
		return nil
	}
	var value struct {
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
		} `json:"delta"`
	}
	if json.Unmarshal(payload, &value) != nil {
		return redaction.ErrPolicyRejected
	}
	text := value.Delta.Text
	if value.Delta.Type == "input_json_delta" {
		text = value.Delta.PartialJSON
	}
	if text == "" {
		return nil
	}
	chunk := openaiapi.ChatCompletionResponse{ID: "native-redaction", Object: "chat.completion.chunk", Model: "native", Choices: []openaiapi.Choice{{Index: 0, Delta: &openaiapi.Message{Role: "assistant", Content: openaiapi.TextContent(text)}}}}
	processed, err := stream.Process(chunk)
	if err != nil {
		return err
	}
	if len(processed) != 1 {
		return redaction.ErrPolicyRejected
	}
	original, _ := json.Marshal(chunk)
	actual, _ := json.Marshal(processed[0])
	if !bytes.Equal(original, actual) {
		return redaction.ErrPolicyRejected
	}
	return nil
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
				if errors.Is(err, budget.ErrExceeded) {
					return s.exhaustedAttemptsError(err)
				}
				lastErr = err
				break
			}
			totalAttempts++
			attemptEmitted := false
			attemptDeliveredBytes := int64(0)
			streamRedactor, streamErr := s.redactor.NewStream(
				principal.Project.RedactionPolicyID,
			)
			if streamErr != nil {
				abortErr := attempt.abort("policy_rejected")
				return gatewayError(
					"streaming_redaction_incompatible",
					"streaming is disabled by the Project redaction policy",
					400, errors.Join(streamErr, abortErr),
				)
			}
			semanticStream := semantic.NewStreamValidator()
			generation, resolveErr := target.Generation(provider.OperationChatStream)
			if resolveErr != nil {
				abortErr := attempt.abort("unsupported_feature")
				return gatewayError("unsupported_feature", "generation primitive is unavailable", 400, errors.Join(resolveErr, abortErr))
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
					attemptDeliveredBytes += deliveredChunkBytes(safeChunk)
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
						attemptDeliveredBytes += deliveredChunkBytes(safeChunk)
						attemptEmitted = true
						emitted = true
					}
				}
			}
			settlement := streamSettlement(
				semanticUsage, providerErr, attemptEmitted, inputTokens, outputTokens,
				estimateInputTokens(attemptDeliveredBytes),
				attempt.pricingTarget, attempt.accounting.ReservationMicrosUSD,
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
				if errors.Is(err, budget.ErrExceeded) {
					return openaiapi.EmbeddingResponse{}, s.exhaustedAttemptsError(err)
				}
				lastErr = err
				break
			}
			attemptCount++
			embedding, resolveErr := target.Embedding()
			if resolveErr != nil {
				abortErr := attempt.abort("unsupported_feature")
				return openaiapi.EmbeddingResponse{}, gatewayError("unsupported_feature", "embedding primitive is unavailable", 400, errors.Join(resolveErr, abortErr))
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
				semanticResponse, providerErr, inputTokens, attempt.pricingTarget, attempt.accounting.ReservationMicrosUSD,
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
	if target.FixedRequestMicrosUSD > math.MaxInt64-reservation {
		return 0, gatewayError("accounting_error", "fixed request price overflows", 503, nil)
	}
	reservation += target.FixedRequestMicrosUSD
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
) (*tokenguard.Lease, string, error) {
	selectedAt := s.now().UTC()
	maximumCost, pricingViewDigest, err := s.captureTokenGuardPricingView(ctx, principal, targets, inputTokens, outputTokens, selectedAt)
	if err != nil {
		return nil, "", err
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
	decision, lease := s.tokenGuard.Acquire(input)
	if !decision.Allowed {
		s.rejections.tokenGuard.Add(1)
		return nil, "", gatewayError(
			"token_guard_blocked",
			"gateway key is temporarily blocked due to anomalous token usage",
			403,
			nil,
		)
	}
	return lease, pricingViewDigest, nil
}

type tokenGuardPriceViewEntry struct {
	TargetID       string `json:"target_id"`
	DeploymentID   string `json:"deployment_id,omitempty"`
	EvidenceStatus string `json:"evidence_status"`
	SnapshotSHA256 string `json:"snapshot_sha256,omitempty"`
	LegacyCost     *int64 `json:"legacy_cost_micros_usd,omitempty"`
}

func (s *Service) captureTokenGuardPricingView(
	ctx context.Context,
	principal auth.AuthResult,
	targets []provider.Target,
	inputTokens, outputTokens int64,
	selectedAt time.Time,
) (int64, string, error) {
	entries := make([]tokenGuardPriceViewEntry, 0, len(targets))
	var maximumCost int64
	for _, target := range targets {
		entry := tokenGuardPriceViewEntry{TargetID: target.ID, DeploymentID: target.DeploymentID}
		if s.pricing != nil && target.DeploymentID != "" {
			price, err := s.pricing.SelectDeploymentPriceVersion(ctx, target.DeploymentID, selectedAt)
			if errors.Is(err, domain.ErrPriceUnavailable) {
				entry.EvidenceStatus = string(domain.PriceEvidenceUnknown)
				entries = append(entries, entry)
				if _, policyErr := s.unknownPricePolicyEvidence(principal); policyErr != nil {
					return 0, "", policyErr
				}
				continue
			}
			if err != nil {
				return 0, "", gatewayError("price_unavailable", "candidate pricing could not be selected", http.StatusConflict, err)
			}
			snapshot, err := domain.NewVersionedPriceSnapshot(price, selectedAt)
			if err != nil {
				return 0, "", gatewayError("accounting_error", "candidate pricing snapshot is invalid", http.StatusServiceUnavailable, err)
			}
			cost, err := snapshot.Calculate(inputTokens, outputTokens)
			if err != nil {
				return 0, "", gatewayError("accounting_error", "candidate cost could not be calculated", http.StatusServiceUnavailable, err)
			}
			digest, err := snapshot.Digest()
			if err != nil {
				return 0, "", err
			}
			entry.EvidenceStatus, entry.SnapshotSHA256 = string(domain.PriceEvidenceVersioned), digest
			entries = append(entries, entry)
			if cost.TotalCostMicrosUSD > maximumCost {
				maximumCost = cost.TotalCostMicrosUSD
			}
			continue
		}
		cost, err := estimateReservation(inputTokens, outputTokens, target)
		if err != nil {
			return 0, "", err
		}
		entry.EvidenceStatus, entry.LegacyCost = "legacy_unversioned", &cost
		entries = append(entries, entry)
		if cost > maximumCost {
			maximumCost = cost
		}
	}
	payload := struct {
		PricingSelectedAt time.Time                  `json:"pricing_selected_at"`
		Entries           []tokenGuardPriceViewEntry `json:"entries"`
	}{PricingSelectedAt: selectedAt, Entries: entries}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return 0, "", err
	}
	digest := sha256.Sum256(encoded)
	return maximumCost, fmt.Sprintf("sha256:%x", digest[:]), nil
}

func (s *Service) unknownPricePolicyEvidence(principal auth.AuthResult) (*domain.UnknownPricePolicyEvidence, error) {
	if s.pricingUnknownPolicy != "allow_without_cost_governance" || principal.Project.DailyBudgetMicrosUSD != 0 ||
		s.tokenGuard.HasCostDimension(principal.Project.TokenGuardPolicyID) {
		return nil, gatewayError("price_unavailable", "known pricing is required by instance or project cost governance", http.StatusConflict, domain.ErrPriceUnavailable)
	}
	status := "none"
	if principal.Project.TokenGuardPolicyID != "" {
		status = "non_cost_dimensions_only"
	}
	evidence := &domain.UnknownPricePolicyEvidence{
		PolicyVersion: "pricing-unknown-policy-v1", ProjectID: principal.Project.ID,
		TokenGuardStatus: status, ReasonCode: "cost_governance_disabled",
		InstanceExplicitOptIn: true, CostGovernanceDisabled: true,
	}
	return evidence, evidence.Validate()
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
	if target.FixedRequestMicrosUSD > math.MaxInt64-cost {
		result.CommittedMicrosUSD = reservationMicrosUSD
		result.CostEstimated = true
		return
	}
	result.CommittedMicrosUSD = cost + target.FixedRequestMicrosUSD
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
	deliveredOutputTokens int64,
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
		estimate := cappedOutputEstimate(estimatedOutputTokens, deliveredOutputTokens)
		result.ProviderInputTokens = estimatedInputTokens
		result.ProviderOutputTokens = estimate
		result.PreparedOutputTokens = estimate
		result.TokenEstimated = true
		result.CostEstimated = true
	} else {
		var classified *provider.Error
		if errors.As(providerErr, &classified) && classified.Ambiguous {
			// Nothing was delivered, so there is nothing to bound the estimate
			// with: an ambiguous failure means the request may have been served
			// in full upstream, and that is what the reservation covers.
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

// deliveredChunkBytes measures the assistant text a chunk carried. Everything
// else in the frame is protocol the provider was not asked to generate.
func deliveredChunkBytes(chunk openaiapi.ChatCompletionResponse) int64 {
	total := int64(0)
	for _, choice := range chunk.Choices {
		if choice.Delta == nil {
			continue
		}
		if text, ok := openaiapi.DecodeTextContent(choice.Delta.Content); ok {
			total += int64(len(text))
			continue
		}
		total += int64(len(choice.Delta.Content))
	}
	return total
}

// cappedOutputEstimate bounds an output estimate by what the gateway actually
// wrote to the caller. Without max_tokens the estimate is the project's output
// ceiling, which can be tens of thousands of tokens, so a stream that delivered
// twenty and then broke was billed for the ceiling. The delivered byte count is
// a real upper bound on what the provider produced for this request, and the
// gateway is the one holding it.
func cappedOutputEstimate(estimated, delivered int64) int64 {
	if delivered > 0 && delivered < estimated {
		return delivered
	}
	return estimated
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
