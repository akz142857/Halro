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
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/akz142857/Halro/internal/anthropicapi"
	"github.com/akz142857/Halro/internal/auth"
	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/circuit"
	"github.com/akz142857/Halro/internal/compatibility"
	anthropicwire "github.com/akz142857/Halro/internal/compatibility/anthropic"
	openaiwire "github.com/akz142857/Halro/internal/compatibility/openai"
	"github.com/akz142857/Halro/internal/contentscan"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/id"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/limiter"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/redaction"
	"github.com/akz142857/Halro/internal/requestmeta"
	"github.com/akz142857/Halro/internal/semantic"
	"github.com/akz142857/Halro/internal/tokenguard"
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
	// instanceID identifies this process. It is what tells a reservation left
	// behind by a crash apart from one another request is holding right now.
	instanceID                    string
	now                           func() time.Time
	resources                     InferenceResourcesResourceStore
	resourceObjectDir             string
	contentScanner                contentscan.Scanner
	pricing                       PriceSelector
	pricingClockRollbackTolerance time.Duration
	pricingClockForwardTolerance  time.Duration
	pricingUnknownPolicy          string
	logger                        *slog.Logger
}

type PriceSelector interface {
	SelectDeploymentPriceVersion(context.Context, string, time.Time) (domain.DeploymentPriceVersion, error)
}

type PricePinStore interface {
	PriceSelector
	LockDeploymentPricingShared(string) func()
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
	Resources                     InferenceResourcesResourceStore
	ResourceObjectDir             string
	ContentScanner                contentscan.Scanner
	Pricing                       PriceSelector
	PricingClockRollbackTolerance time.Duration
	PricingClockForwardTolerance  time.Duration
	PricingUnknownPolicy          string

	// Logger records provider attempts that failed. A gateway answers its caller
	// with a deliberately opaque envelope — no upstream status, code or sentence,
	// because that is the caller's side of a trust boundary — which left the
	// operator with a 502 and nothing at all to read. Nil selects a discarding
	// logger so tests and embedders need not supply one.
	Logger *slog.Logger

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
	finalized                   bool
	tokenGuardPricingViewDigest string
}

// finalize closes out the request's accounting exactly once. Every exit path
// that knows why the request ended calls this; close() covers the paths that
// return before reaching one. Without the guard the two would double-append a
// RequestFinalized event for the same request.
func (run *requestRun) finalize(outcome string) error {
	if run.finalized {
		return nil
	}
	run.finalized = true
	return run.service.finalizeRequest(run.requestLease, outcome)
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

// assertPolicySnapshotsCoverProject refuses a request whose Project names a
// redaction or Token Guard policy the live snapshot does not hold.
//
// Both engines answer a lookup miss with "no policy", and for two controls
// whose whole job is to constrain traffic that is the fail-open direction: the
// project's PII rules simply do not run, and Token Guard admits unconditionally.
// The Admin guards make a miss unreachable through the supported paths — a
// policy an enabled Project references cannot be disabled or deleted — so
// arriving here means the live snapshot is behind the durable store or one of
// those guards regressed. Both are refusal cases. An empty ID is not a miss:
// it means the Project deliberately has no policy.
func (s *Service) assertPolicySnapshotsCoverProject(principal auth.AuthResult) error {
	if id := principal.Project.RedactionPolicyID; id != "" && !s.redactor.HasPolicy(id) {
		return gatewayError(
			"configuration_stale",
			"the redaction policy this project requires is not loaded; retry shortly",
			503,
			nil,
		)
	}
	if id := principal.Project.TokenGuardPolicyID; id != "" && !s.tokenGuard.HasPolicy(id) {
		return gatewayError(
			"configuration_stale",
			"the Token Guard policy this project requires is not loaded; retry shortly",
			503,
			nil,
		)
	}
	return nil
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
	if !slices.Contains(principal.Project.AllowedModels, model) {
		return auth.AuthResult{}, nil, gatewayError("model_not_allowed", "model is not allowed for this project", 403, nil)
	}
	if err := authorizeSource(ctx, principal.Project); err != nil {
		return auth.AuthResult{}, nil, err
	}
	if err := s.assertPolicySnapshotsCoverProject(principal); err != nil {
		return auth.AuthResult{}, nil, err
	}
	targets := s.registry.ResolveCandidatesFor(model, operation)
	if len(targets) == 0 {
		// Order matters: candidate resolution drops probe-unhealthy targets
		// before the operation filter, so an alias whose every deployment is
		// unhealthy is empty for every operation. Reporting that as 400
		// "unsupported" blames the request for an upstream state — the caller
		// rewrites their payload, the operator audits capabilities, and neither
		// finds anything. It is the same condition an open circuit reports, so
		// it gets the same shape.
		if s.registry.SupportsOperation(model, operation, "") {
			return auth.AuthResult{}, nil, gatewayError("provider_unavailable", "no healthy deployment is available for this model; retry shortly", 503, nil)
		}
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
	// A request that was accepted into the ledger but never finalized stays
	// "in flight" forever: the usage aggregate keeps its entry, the entry is
	// checkpointed into bbolt, and halro_active_requests never comes back
	// down. Rejections that never reach a provider — budget exhausted, breaker
	// open, concurrency full — are the paths most likely to return before any
	// caller thinks to finalize, and they are also the cheapest to trigger in
	// bulk. Finalizing here makes "the run ended" the thing that closes the
	// accounting, rather than every individual return statement remembering to.
	if !run.finalized {
		_ = run.finalize("rejected")
	}
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
	var fatal *Error
	switch {
	case errors.As(lastErr, &fatal):
		// startAttempt's fatal failures — token_guard_blocked, accounting
		// unavailability — arrive already mapped, with the status and code the
		// policy decision chose. Re-mapping them through mapProviderError would
		// report a policy refusal as a 502 provider outage, inviting clients to
		// retry requests that must not be retried. The multi-target loops check
		// this themselves before retrying; the single-target native paths rely
		// on this case.
		return fatal
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
		pricingUnlock = pinStore.LockDeploymentPricingShared(target.DeploymentID)
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
		finalizeErr := run.finalize("accounting_error")
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
		finalizeErr := run.finalize("token_guard_rejected")
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
		finalizeErr := run.finalize("accounting_error")
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
			finalizeErr := run.finalize("accounting_error")
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
		finalizeErr := run.finalize("accounting_error")
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
	cost, err := snapshot.Calculate(inputTokens, 0, outputTokens)
	if err != nil {
		return 0, "", nil, target, gatewayError("accounting_error", "unable to estimate request cost", http.StatusServiceUnavailable, err)
	}
	mode := ledger.LeaseModeMetered
	if price.BillingMode == domain.BillingModeFree {
		mode = ledger.LeaseModeFree
	}
	priced := target
	priced.InputMicrosPerMillion = *snapshot.InputMicrosPerMillion
	priced.CachedInputMicrosPerMillion = *snapshot.CachedInputMicrosPerMillion
	priced.OutputMicrosPerMillion = *snapshot.OutputMicrosPerMillion
	priced.FixedRequestMicrosUSD = *snapshot.FixedRequestMicrosUSD
	return cost.TotalCostMicrosUSD, mode, &snapshot, priced, nil
}

func (s *Service) prepareAccountingLease(ctx context.Context, target provider.Target, inputTokens, outputTokens int64) (int64, ledger.LeaseMode, *domain.PriceSnapshot, provider.Target, error) {
	if s.pricing == nil {
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
	cost, err := snapshot.Calculate(inputTokens, 0, outputTokens)
	if err != nil {
		return 0, "", nil, target, gatewayError("accounting_error", "unable to estimate request cost", http.StatusServiceUnavailable, err)
	}
	mode := ledger.LeaseModeMetered
	if snapshot.BillingMode == domain.BillingModeFree {
		mode = ledger.LeaseModeFree
	}
	priced := target
	priced.InputMicrosPerMillion = *snapshot.InputMicrosPerMillion
	priced.CachedInputMicrosPerMillion = *snapshot.CachedInputMicrosPerMillion
	priced.OutputMicrosPerMillion = *snapshot.OutputMicrosPerMillion
	priced.FixedRequestMicrosUSD = *snapshot.FixedRequestMicrosUSD
	return cost.TotalCostMicrosUSD, mode, &snapshot, priced, nil
}

func (attempt *activeAttempt) finish(providerErr error, settlement budget.Settlement) error {
	attempt.logProviderFailure(providerErr)
	if attempt.accounting.LeaseMode == ledger.LeaseModeUnknownAllowed {
		settlement.CommittedMicrosUSD = 0
		settlement.CostEstimated = false
	}
	attempt.concurrency.Release()
	attempt.run.recordProviderResult(providerErr, settlement)
	enrichSettlement(&settlement, providerErr, attempt.startedAt, attempt.service.now())
	if err := attempt.service.settleAttempt(attempt.accounting, settlement); err != nil {
		attempt.reportBreaker(providerErr)
		finalizeErr := attempt.run.finalize("accounting_error")
		return gatewayError(
			"accounting_unavailable", "request accounting could not be finalized", 503,
			errors.Join(err, finalizeErr),
		)
	}
	attempt.reportBreaker(providerErr)
	return nil
}

// logProviderFailure records an attempt the upstream did not complete.
//
// What the caller is told and what the operator is told are different answers on
// purpose. The response carries a fixed sentence and no upstream detail, because
// the caller is on the other side of a trust boundary. The log is on this side,
// so it names the route that failed and how the failure was classified — which
// is what decides whether the attempt was retried, whether the breaker counted
// it, and whether a fallback target was tried.
//
// The upstream's own sentence is not written. It is a provider response body,
// and the one thing an upstream is most likely to quote back is the credential
// it just refused; a pattern denylist only knows the formats it was told about.
// A refusal Halro produced itself — a transport policy rejection, a response it
// would not decode — carries no provider body and is the case an operator most
// needs, so that text is logged, on the same rule the connection tests use: an
// error with no upstream status is one Halro wrote.
func (attempt *activeAttempt) logProviderFailure(providerErr error) {
	if providerErr == nil {
		return
	}
	target := attempt.pricingTarget
	attributes := []any{
		"request_id", attempt.run.requestID,
		"public_model", target.PublicModel,
		"deployment_id", target.DeploymentID,
		"provider_id", target.ProviderID,
		"binding_id", target.BindingID,
	}
	var classified *provider.Error
	if errors.As(providerErr, &classified) {
		attributes = append(attributes, "error_class", string(classified.Class), "retryable", classified.Retryable)
		if classified.Ambiguous {
			attributes = append(attributes, "ambiguous", true)
		}
		if classified.StatusCode > 0 {
			attributes = append(attributes, "provider_status", classified.StatusCode)
		}
		// The adapter separates the upstream's identifier from its prose for
		// exactly this line: the sentence is a response body and stays inside the
		// error, while `code` — and the parameter it refused, joined to it — is
		// what an operator can act on. Logging status without it says a request
		// was refused without saying for what, and leaves them bisecting a body
		// they did not write.
		if classified.ProviderCode != "" {
			attributes = append(attributes, "provider_code", classified.ProviderCode)
		}
		if classified.ProviderRequestID != "" {
			attributes = append(attributes, "provider_request_id", classified.ProviderRequestID)
		}
		if classified.StatusCode == 0 {
			attributes = append(attributes, "reason", providerFailureReason(classified))
		}
	} else if errors.Is(providerErr, context.Canceled) || errors.Is(providerErr, context.DeadlineExceeded) {
		attributes = append(attributes, "error_class", "client_disconnected_or_timed_out")
	} else {
		attributes = append(attributes, "error_class", string(provider.ErrorUnknown), "reason", providerErr.Error())
	}
	attempt.service.logger.Warn("provider attempt failed", attributes...)
}

// providerFailureReason unwraps the cause a provider error carries. Error stops
// at its own headline, so a transport refusal reads as a bare "provider request
// failed" — losing the address or allowlist entry that actually refused it.
func providerFailureReason(classified *provider.Error) string {
	if classified.Cause == nil {
		return classified.Message
	}
	cause := classified.Cause.Error()
	if classified.Message == "" || strings.Contains(cause, classified.Message) {
		return cause
	}
	return classified.Message + ": " + cause
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
	finalizeErr := attempt.run.finalize(outcome)
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
	instanceID, err := id.New("inst")
	if err != nil {
		return nil, errors.New("generate service instance ID")
	}
	clock := options.Now
	if clock == nil {
		clock = time.Now
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Service{
		logger:                        logger,
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
		instanceID:                    instanceID,
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
	candidates := targets
	targets = filterSemanticCapabilities(targets, canonical.Requirements)
	targets = filterGenerateProfileCompatibility(targets, canonical)
	targets = filterPrimitiveTargets(targets, provider.OperationChat)
	if len(targets) == 0 {
		return openaiapi.ChatCompletionResponse{}, unservableError(
			"model route does not support the requested chat capabilities",
			unservableReasons(candidates, canonical, provider.OperationChat),
		)
	}
	request, err = s.redactor.ProcessInboundChat(principal.Project.RedactionPolicyID, request)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, gatewayError("sensitive_data_detected", "request contains secret material", 400, err)
	}
	canonical, err = openaiwire.DecodeGenerate(request)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, gatewayError("invalid_request_error", "redacted request cannot be represented safely", 400, err)
	}
	inputTokens := estimateGenerateInputTokens(request.EstimatedInputBytes(), canonical)
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
	requestID := run.requestID
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
				if finalizeErr := run.finalize(outcome); finalizeErr != nil {
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
				if err := run.finalize("provider_error"); err != nil {
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
	if err := run.finalize("provider_error"); err != nil {
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
		return openaiapi.Response{}, s.returnFailure(ctx, "responses", "provider response cannot be represented safely", err)
	}
	response, err := openaiwire.RenderResponseResult(result, request)
	if err != nil {
		return openaiapi.Response{}, s.returnFailure(ctx, "responses", "provider response cannot be rendered safely", err)
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
		return s.returnFailure(ctx, "responses", "provider stream cannot be completed safely", err)
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
		return anthropicapi.Message{}, s.returnFailure(ctx, "messages", "provider response cannot be represented safely", err)
	}
	message, err := anthropicwire.RenderResult(result, request.Model)
	if err != nil {
		return anthropicapi.Message{}, s.returnFailure(ctx, "messages", "provider response cannot be rendered safely", err)
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
		return s.returnFailure(ctx, "messages", "provider stream cannot be completed safely", err)
	}
	for _, event := range events {
		if err := emit(event); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) MessagesNative(ctx context.Context, plaintextKey, version string, betas []string, request anthropicapi.MessageRequest) (anthropicapi.Message, error) {
	if request.Stream {
		return anthropicapi.Message{}, gatewayError("invalid_request_error", "stream must be false", 400, nil)
	}
	principal, target, envelope, inputTokens, outputTokens, err := s.prepareNativeMessages(ctx, plaintextKey, version, betas, request, provider.OperationMessages)
	if err != nil {
		return anthropicapi.Message{}, err
	}
	// Inspect the bytes the envelope will hand the provider, not the decoded
	// struct: the struct is a lossy view of them.
	payload, err := envelope.PayloadFor(target.ProfileID, 1, compatibility.NativeRequest)
	if err != nil {
		return anthropicapi.Message{}, gatewayError("internal_error", "native request payload is unavailable", 500, err)
	}
	if err := s.checkNativeInboundRedaction(principal, payload); err != nil {
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
	result, providerErr := adapter.MessagesNative(ctx, provider.NativeMessageCall{RequestID: run.requestID, ProviderModel: target.ProviderModel, Version: version, Betas: betas, Payload: payload})
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
				providerErr = s.checkNativeOutboundRedaction(principal, safePayload)
			}
		}
	}
	if message.ID != "" {
		usage := nativeAnthropicUsage(message.Usage)
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
	if err := run.finalize(outcome); err != nil {
		return anthropicapi.Message{}, gatewayError("accounting_unavailable", "request accounting could not be finalized", 503, err)
	}
	if providerErr != nil {
		return anthropicapi.Message{}, terminalProviderError(providerErr)
	}
	message.Model = request.Model
	return message, nil
}

// MessagesCountTokens serves Anthropic's count_tokens. Anthropic does not bill
// it, and Halro settles it at zero cost — but it still runs the whole admission
// path: authentication, project policy, routing, redaction, and a real ledger
// attempt. The endpoint sends the caller's prompt to the provider on the
// operator's credential, so leaving it off the ledger would put a class of
// provider calls outside the record that every other call is held to. What the
// entry says is that it happened and cost nothing.
func (s *Service) MessagesCountTokens(ctx context.Context, plaintextKey, version string, betas []string, request anthropicapi.MessageRequest) (anthropicapi.TokenCount, error) {
	if request.Stream {
		return anthropicapi.TokenCount{}, gatewayError("invalid_request_error", "count_tokens does not stream", 400, nil)
	}
	principal, target, envelope, inputTokens, _, err := s.prepareNativeMessages(ctx, plaintextKey, version, betas, request, provider.OperationMessages)
	if err != nil {
		return anthropicapi.TokenCount{}, err
	}
	if target.ProfileID != domain.ProfileAnthropicMessages {
		// Only the direct Anthropic profile has a proven count_tokens surface.
		// Bedrock Mantle shares the Messages wire format, but whether it serves
		// this path is not established, and guessing would send the operator's
		// prompt at an endpoint nobody verified.
		return anthropicapi.TokenCount{}, gatewayError("unsupported_feature", "count_tokens requires a direct Anthropic Messages provider profile", 400, nil)
	}
	payload, err := envelope.PayloadFor(target.ProfileID, 1, compatibility.NativeRequest)
	if err != nil {
		return anthropicapi.TokenCount{}, gatewayError("internal_error", "native request payload is unavailable", 500, err)
	}
	if err := s.checkNativeInboundRedaction(principal, payload); err != nil {
		return anthropicapi.TokenCount{}, err
	}
	run, err := s.beginRequestRun(ctx, principal, request.Model, []provider.Target{target}, inputTokens, inputTokens, 0)
	if err != nil {
		return anthropicapi.TokenCount{}, err
	}
	defer run.close()
	// Zero prepared tokens is what makes the reservation zero: nothing is
	// generated, so there is no span to price.
	attempt, err := s.startAttempt(ctx, run, target, 0, 0, 0, 0, 1)
	if err != nil {
		return anthropicapi.TokenCount{}, s.exhaustedAttemptsError(err)
	}
	adapter, err := target.NativeTokenCount()
	if err != nil {
		abortErr := attempt.abort("unsupported_feature")
		return anthropicapi.TokenCount{}, gatewayError("unsupported_feature", "native count_tokens primitive is unavailable", 400, errors.Join(err, abortErr))
	}
	result, providerErr := adapter.CountTokensNative(ctx, provider.NativeMessageCall{RequestID: run.requestID, ProviderModel: target.ProviderModel, Version: version, Betas: betas, Payload: payload})
	var count anthropicapi.TokenCount
	if providerErr == nil {
		count, providerErr = anthropicapi.DecodeTokenCount(result.Payload)
		if providerErr == nil {
			providerErr = s.checkNativeOutboundRedaction(principal, result.Payload)
		}
	}
	settlement := budget.Settlement{Outcome: "success", ProviderInputTokens: count.InputTokens}
	if providerErr != nil {
		settlement = budget.Settlement{Outcome: "provider_error"}
	}
	if err := attempt.finish(providerErr, settlement); err != nil {
		return anthropicapi.TokenCount{}, err
	}
	if err := run.finalize(settlement.Outcome); err != nil {
		return anthropicapi.TokenCount{}, gatewayError("accounting_unavailable", "request accounting could not be finalized", 503, err)
	}
	if providerErr != nil {
		return anthropicapi.TokenCount{}, terminalProviderError(providerErr)
	}
	return count, nil
}

func (s *Service) MessagesNativeStream(ctx context.Context, plaintextKey, version string, betas []string, request anthropicapi.MessageRequest, emit func(anthropicapi.RawStreamEvent) error) error {
	if !request.Stream {
		return gatewayError("invalid_request_error", "stream must be true", 400, nil)
	}
	principal, target, envelope, inputTokens, outputTokens, err := s.prepareNativeMessages(ctx, plaintextKey, version, betas, request, provider.OperationMessagesStream)
	if err != nil {
		return err
	}
	payload, err := envelope.PayloadFor(target.ProfileID, 1, compatibility.NativeRequest)
	if err != nil {
		return gatewayError("internal_error", "native request payload is unavailable", 500, err)
	}
	if err := s.checkNativeInboundRedaction(principal, payload); err != nil {
		return err
	}
	if !s.redactor.AllowsStreaming(principal.Project.RedactionPolicyID) {
		return gatewayError("streaming_redaction_incompatible", "streaming is disabled by the Project redaction policy", 400, nil)
	}
	streamInspector, err := s.redactor.NewStreamInspector(principal.Project.RedactionPolicyID)
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
	registry, _ := anthropicwire.NewNativeSchemaRegistry()
	identity := nativeIdentity(principal, target, request.Model)
	gate := newNativeStreamGate(s, principal.Project.RedactionPolicyID, streamInspector, emit)
	providerErrorEvent := false
	usage, providerErr := adapter.MessagesNativeStream(ctx, provider.NativeMessageCall{RequestID: run.requestID, ProviderModel: target.ProviderModel, Version: version, Betas: betas, Payload: payload}, func(event anthropicapi.RawStreamEvent) error {
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
		providerErrorEvent = providerErrorEvent || event.Type == "error"
		return gate.Accept(event, safePayload)
	})
	if providerErr == nil {
		// Closing the inspector is what confirms the suffix each channel was still
		// withholding; the events waiting on it are released here or never.
		providerErr = gate.Finish()
	}
	emitted := gate.emitted
	deliveredBytes := gate.delivered
	semanticUsage := (*semantic.Usage)(nil)
	if usage != nil {
		converted := nativeAnthropicUsage(*usage)
		semanticUsage = &converted
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
	if err := run.finalize(outcome); err != nil {
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

// allowsAnthropicBetas fails closed on any token a connection has not been
// configured to forward.
func allowsAnthropicBetas(target provider.Target, betas []string) bool {
	for _, beta := range betas {
		if !slices.Contains(target.AllowedAnthropicBetas, beta) {
			return false
		}
	}
	return true
}

func (s *Service) prepareNativeMessages(ctx context.Context, plaintextKey, version string, betas []string, request anthropicapi.MessageRequest, operation provider.Operation) (auth.AuthResult, provider.Target, *compatibility.NativeEnvelope, int64, int64, error) {
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
	// Native requests carry requirements too — a schema-backed output_config
	// needs JSON mode, a web_search tool needs the connection to have accepted
	// upstream egress. Filtering here is what makes those requirements load
	// bearing; deriving them only inside the governance envelope, which is built
	// for an already-chosen target, left them as decoration.
	requirements := anthropicwire.NativeRequirements(request)
	nativeCandidates := targets
	targets = filterSemanticCapabilities(targets, requirements)
	if len(targets) == 0 {
		return auth.AuthResult{}, provider.Target{}, nil, 0, 0, unservableError(
			"model route does not support the requested Anthropic Messages capabilities",
			missingCapabilities(nativeCandidates, requirements),
		)
	}
	// The beta allowlist is per connection, so it is checked once a target is
	// chosen and before any provider work — an unaccepted beta costs nothing —
	// and before the envelope, which has to record the headers actually sent.
	targets = slices.DeleteFunc(targets, func(target provider.Target) bool {
		return !allowsAnthropicBetas(target, betas)
	})
	if len(targets) == 0 {
		return auth.AuthResult{}, provider.Target{}, nil, 0, 0, gatewayError("unsupported_feature", "anthropic-beta is not enabled for any connection behind this model", 400, nil)
	}
	target := targets[0]
	registry, err := anthropicwire.NewNativeSchemaRegistry()
	if err != nil {
		return auth.AuthResult{}, provider.Target{}, nil, 0, 0, gatewayError("internal_error", "native schema is unavailable", 500, err)
	}
	envelope, err := compatibility.NewNativeEnvelope(registry, target.ProfileID, 1, anthropicwire.NativeHeaders(version, betas), request.Raw, nativeIdentity(principal, target, request.Model))
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

// checkNativeInboundRedaction inspects the exact bytes that will travel
// upstream. It walks the raw payload rather than a portable projection because
// a projection can only carry what the canonical model models: every field
// outside it — metadata, service_tier, top_k, and anything a newer Anthropic
// release adds — reached the provider uninspected. Walking the payload keeps
// the inspection surface equal to the accepted surface, which is the invariant
// that lets the accepted surface grow without opening a redaction hole.
func (s *Service) checkNativeInboundRedaction(principal auth.AuthResult, payload json.RawMessage) error {
	err := s.redactor.InspectJSON(principal.Project.RedactionPolicyID, "inbound", payload)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, redaction.ErrMalformedJSON):
		return gatewayError("invalid_request_error", "native request cannot be inspected safely", 400, err)
	case errors.Is(err, redaction.ErrRewriteRequired):
		return gatewayError("native_redaction_incompatible", "native payload would require rewriting", 400, err)
	default:
		return gatewayError("sensitive_data_detected", "request contains secret material", 400, err)
	}
}

// checkNativeOutboundRedaction mirrors the inbound walk over the response the
// client is about to receive. Thinking blocks are inspected here rather than
// projected away: the old exclusion existed because the portable result model
// had nowhere to put them, not because model reasoning is exempt from the
// outbound secret baseline.
func (s *Service) checkNativeOutboundRedaction(principal auth.AuthResult, payload json.RawMessage) error {
	err := s.inspectOutboundJSON(principal.Project.RedactionPolicyID, payload)
	if errors.Is(err, redaction.ErrMalformedJSON) {
		return &provider.Error{Class: provider.ErrorMalformed, Ambiguous: true, Message: "native response cannot be inspected safely", Cause: err}
	}
	return err
}

// inspectOutboundJSON is the stateless half of outbound inspection, shared by
// the unary response and by whole stream events. It deliberately walks opaque
// values too (thinking signatures, redacted_thinking data): exempting a field
// would require the schema knowledge this design removes, and a uniform walk is
// what keeps the inspected surface equal to the accepted surface.
func (s *Service) inspectOutboundJSON(policyID string, payload json.RawMessage) error {
	err := s.redactor.InspectJSON(policyID, "outbound", payload)
	if err == nil || errors.Is(err, redaction.ErrMalformedJSON) {
		return err
	}
	return errors.Join(redaction.ErrPolicyRejected, err)
}

// nativeStreamGate is the outbound baseline for a stream Halro forwards byte
// for byte.
//
// It exists because the ordinary stream redactor cannot answer the question
// native mode has to ask. That redactor withholds the trailing max-match-width
// bytes of each channel so a pattern split across two provider chunks cannot be
// delivered half-redacted, and it returns the redacted prefix; comparing that
// prefix against the fragment that produced it always differs, which is not a
// verdict about the policy. Native streaming was gated on exactly that
// comparison, so every delta carrying text was refused — the mode has never
// worked on real payloads, and the repository's only native streaming test used
// signature_delta, which carries none.
//
// The gate inverts the arrangement: the inspector reports how many bytes of each
// channel are confirmed unchanged, and an event is released only once every byte
// of text it carries falls inside that confirmed span. Events therefore queue
// briefly — bounded by the widest rule, not by the block — and an event whose
// text turns out to need rewriting is never emitted at all.
type nativeStreamGate struct {
	inspector *redaction.StreamInspector
	policyID  string
	service   *Service
	emit      func(anthropicapi.RawStreamEvent) error
	queue     []gatedStreamEvent
	pushed    map[string]int64
	confirmed map[string]int64
	delivered int64
	emitted   bool
}

type gatedStreamEvent struct {
	event   anthropicapi.RawStreamEvent
	channel string
	need    int64
}

type nativeStreamDelta struct {
	Index *int `json:"index"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		Thinking    string `json:"thinking"`
	} `json:"delta"`
}

func newNativeStreamGate(service *Service, policyID string, inspector *redaction.StreamInspector, emit func(anthropicapi.RawStreamEvent) error) *nativeStreamGate {
	return &nativeStreamGate{
		inspector: inspector, policyID: policyID, service: service, emit: emit,
		pushed: make(map[string]int64), confirmed: make(map[string]int64),
	}
}

// Accept inspects one event and releases whatever the inspection has now
// confirmed. Order is preserved: a queued event never overtakes one in front of
// it, so a content_block_stop cannot reach the client before the deltas it ends.
func (g *nativeStreamGate) Accept(event anthropicapi.RawStreamEvent, payload json.RawMessage) error {
	channel, text, err := nativeDeltaChannel(event, payload)
	if err != nil {
		return err
	}
	switch {
	case channel != "":
		confirmed, err := g.inspector.Push(channel, strings.HasSuffix(channel, ":input_json_delta"), text)
		if err != nil {
			return err
		}
		g.pushed[channel] += int64(len(text))
		g.confirmed[channel] = confirmed
		g.queue = append(g.queue, gatedStreamEvent{event: anthropicapi.RawStreamEvent{Type: event.Type, Data: payload}, channel: channel, need: g.pushed[channel]})
	default:
		// Every event that is not incremental text arrives whole, so it gets the
		// same raw-JSON walk the unary path uses. Without it a content_block_start
		// carrying a block type Halro does not model would reach the client
		// uninspected — the hole that widening the accepted block set would open.
		if err := g.service.inspectOutboundJSON(g.policyID, payload); err != nil {
			return err
		}
		if event.Type == "content_block_stop" {
			// The block is complete, so its channels can be closed and their
			// withheld suffixes confirmed. Doing it before queuing this event is
			// what lets the deltas ahead of it drain in the same pass.
			if err := g.closeBlock(payload); err != nil {
				return err
			}
		}
		g.queue = append(g.queue, gatedStreamEvent{event: anthropicapi.RawStreamEvent{Type: event.Type, Data: payload}})
	}
	return g.drain()
}

// Finish closes every open channel and releases what that confirms. A stream
// that ends without it leaves each channel's withheld suffix uninspected, which
// is the same leak as not inspecting at all.
func (g *nativeStreamGate) Finish() error {
	if err := g.inspector.Finish(); err != nil {
		return err
	}
	for channel := range g.pushed {
		g.confirmed[channel] = g.inspector.Confirmed(channel)
	}
	if err := g.drain(); err != nil {
		return err
	}
	if len(g.queue) > 0 {
		return redaction.ErrPolicyRejected
	}
	return nil
}

func (g *nativeStreamGate) closeBlock(payload json.RawMessage) error {
	var event struct {
		Index *int `json:"index"`
	}
	if json.Unmarshal(payload, &event) != nil || event.Index == nil {
		return nil
	}
	prefix := strconv.Itoa(*event.Index) + ":"
	closed, err := g.inspector.CloseGroup(prefix)
	if err != nil {
		return err
	}
	for _, channel := range closed {
		g.confirmed[channel] = g.inspector.Confirmed(channel)
	}
	return nil
}

func (g *nativeStreamGate) drain() error {
	for len(g.queue) > 0 {
		head := g.queue[0]
		if head.channel != "" && g.confirmed[head.channel] < head.need {
			return nil
		}
		g.queue = g.queue[1:]
		if err := g.emit(head.event); err != nil {
			return err
		}
		g.delivered += int64(len(head.event.Data))
		g.emitted = true
	}
	return nil
}

// nativeDeltaChannel names the logical text stream an event contributes to, or
// returns an empty name for an event that carries no incremental text. The name
// is scoped by content block index because two blocks stream independently and a
// pattern cannot span them.
func nativeDeltaChannel(event anthropicapi.RawStreamEvent, payload json.RawMessage) (string, string, error) {
	if event.Type != "content_block_delta" {
		return "", "", nil
	}
	var value nativeStreamDelta
	if json.Unmarshal(payload, &value) != nil || value.Index == nil {
		return "", "", redaction.ErrPolicyRejected
	}
	var text string
	switch value.Delta.Type {
	case "text_delta":
		text = value.Delta.Text
	case "input_json_delta":
		text = value.Delta.PartialJSON
	case "thinking_delta":
		text = value.Delta.Thinking
	default:
		// signature_delta and any delta shape a later release adds carry no text
		// Halro can inspect incrementally; they take the whole-event walk instead
		// of passing through unlooked-at.
		return "", "", nil
	}
	if text == "" {
		return "", "", nil
	}
	return strconv.Itoa(*value.Index) + ":" + value.Delta.Type, text, nil
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
	candidates := targets
	targets = filterSemanticCapabilities(targets, canonical.Requirements)
	targets = filterGenerateProfileCompatibility(targets, canonical)
	targets = filterPrimitiveTargets(targets, provider.OperationChatStream)
	if len(targets) == 0 {
		return unservableError(
			"model route does not support the requested chat capabilities",
			unservableReasons(candidates, canonical, provider.OperationChatStream),
		)
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
	inputTokens := estimateGenerateInputTokens(request.EstimatedInputBytes(), canonical)
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
	requestID := run.requestID
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
				if err := run.finalize("success"); err != nil {
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
				if err := run.finalize(outcome); err != nil {
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
	if err := run.finalize("provider_error"); err != nil {
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
	requestID := run.requestID
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
				if err := run.finalize("success"); err != nil {
					return openaiapi.EmbeddingResponse{}, gatewayError(
						"accounting_unavailable", "request accounting could not be finalized", 503, err,
					)
				}
				response.Model = request.Model
				return response, nil
			}
			lastErr = providerErr
			if !retryable(providerErr) {
				if err := run.finalize("provider_error"); err != nil {
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
	if err := run.finalize("provider_error"); err != nil {
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
	// A reservation is taken before the provider answers, so no cache read has
	// been reported yet and the whole prompt reserves at the ordinary input rate.
	reservation, err := budget.EstimateCostMicros(budget.TokenCost{
		InputTokens: inputTokens, OutputTokens: outputTokens,
		InputMicrosPerMillion: target.InputMicrosPerMillion, OutputMicrosPerMillion: target.OutputMicrosPerMillion,
	})
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
			// Token Guard bounds what a request may cost before it runs, so it
			// prices every candidate as if nothing were served from cache.
			cost, err := snapshot.Calculate(inputTokens, 0, outputTokens)
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

// unservableError names what a route could not serve. The filters compute every
// reason and used to discard all of them, which left a refusal that says a
// request was turned away without saying what about it — the operator's next
// move is a bisect of a body they may not have written.
//
// The names are safe to return because none of them is caller data: capability
// keys are the same vocabulary the console shows, and the field names are the
// ones docs/compatibility/endpoint-manifests.json already publishes.
func unservableError(message string, reasons []string) *Error {
	if len(reasons) > 0 {
		message += ": " + strings.Join(reasons, ", ")
	}
	return gatewayError("unsupported_feature", message, 400, nil)
}

// unservableReasons collects, across every candidate the route offered, the
// capability each one lacked, the request member its profile cannot carry, and
// the operation it does not implement. Every candidate is reported because every
// candidate was dropped: naming only the first would send an operator to fix one
// deployment while the rest of the route fails for other reasons.
func unservableReasons(candidates []provider.Target, request semantic.GenerateRequest, operation provider.Operation) []string {
	reasons := newReasonSet()
	for _, target := range candidates {
		reasons.addAll(missingCapabilities([]provider.Target{target}, request.Requirements))
		reasons.addAll(compatibility.UnsupportedGenerateFields(target.ProfileID, request))
		if _, ok := target.ResolveOperation(operation); !ok {
			reasons.add(string(operation))
		}
	}
	return reasons.values
}

func missingCapabilities(candidates []provider.Target, requirements semantic.Requirements) []string {
	reasons := newReasonSet()
	for _, target := range candidates {
		// Table order, not map order: a refusal that lists its reasons
		// differently each time is a refusal an operator cannot diff.
		for _, pairing := range capabilityRequirements {
			if pairing.required(requirements) && !pairing.served(target.Capabilities) {
				reasons.add(pairing.name)
			}
		}
	}
	return reasons.values
}

type reasonSet struct {
	seen   map[string]struct{}
	values []string
}

func newReasonSet() *reasonSet { return &reasonSet{seen: map[string]struct{}{}} }

func (r *reasonSet) add(value string) {
	if _, exists := r.seen[value]; exists || value == "" {
		return
	}
	r.seen[value] = struct{}{}
	r.values = append(r.values, value)
}

func (r *reasonSet) addAll(values []string) {
	for _, value := range values {
		r.add(value)
	}
}

// capabilityRequirements pairs what a request needs with the capability that
// serves it, under the name the console and the model catalogue already use.
//
// It is one table because it used to be two: the filter matched requirement to
// capability in a boolean expression, and the refusal that names what a route
// could not serve rebuilt the same pairing in its own list. Two copies of a
// mapping is a mapping that drifts, and the drift is silent — a requirement
// paired in one place and forgotten in the other either refuses without saying
// why or does not refuse at all.
//
// The two sides also used to be spelled differently, requirement InputImage
// against capability vision and StructuredJSON against json_mode, so the table
// was the only place the reader could learn they were the same thing. They share
// the dictionary's names now, and TestEveryCapabilityShapedRequirementIsPaired
// holds a new one to being either paired here or listed as deliberately not.
var capabilityRequirements = []struct {
	name     string
	required func(semantic.Requirements) bool
	served   func(provider.Capabilities) bool
}{
	{"tools", func(r semantic.Requirements) bool { return r.Tools }, func(c provider.Capabilities) bool { return c.Tools }},
	{"vision", func(r semantic.Requirements) bool { return r.Vision }, func(c provider.Capabilities) bool { return c.Vision }},
	{"fetched_image", func(r semantic.Requirements) bool { return r.FetchedImage }, func(c provider.Capabilities) bool { return c.FetchedImage }},
	{"json_mode", func(r semantic.Requirements) bool { return r.JSONMode }, func(c provider.Capabilities) bool { return c.JSONMode }},
	{"developer_role", func(r semantic.Requirements) bool { return r.DeveloperRole }, func(c provider.Capabilities) bool { return c.DeveloperRole }},
	{"reasoning", func(r semantic.Requirements) bool { return r.Reasoning }, func(c provider.Capabilities) bool { return c.Reasoning }},
	{"stream_usage", func(r semantic.Requirements) bool { return r.StreamUsage }, func(c provider.Capabilities) bool { return c.StreamUsage }},
	{"provider_executed_tools", func(r semantic.Requirements) bool { return r.ProviderExecutedTools }, func(c provider.Capabilities) bool { return c.ProviderExecutedTools }},
}

func filterSemanticCapabilities(targets []provider.Target, requirements semantic.Requirements) []provider.Target {
	return slices.DeleteFunc(slices.Clone(targets), func(target provider.Target) bool {
		for _, pairing := range capabilityRequirements {
			if pairing.required(requirements) && !pairing.served(target.Capabilities) {
				return true
			}
		}
		return false
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
			recordUsageTiers(&result, *response.Usage)
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
		recordUsageTiers(&result, *response.Usage)
	}
	setSettlementCost(&result, target, reservationMicrosUSD)
	return result
}

// nativeAnthropicUsage translates a Messages usage block into the semantic
// convention, which the native path has to do for itself because nothing
// translates its payload.
//
// Anthropic reports input_tokens net of both cache tiers. Copying that field
// straight across under-reports the prompt by whatever the cache served — most
// of it on an agent workload — and leaves the tiers empty, so the cache-read
// rate never applies and the ledger records a prompt that never existed. The
// compatibility mapping already recovers the full span this way; the native path
// was reading the same field with the other meaning.
func nativeAnthropicUsage(usage anthropicapi.Usage) semantic.Usage {
	promptTokens := usage.PromptTokens()
	return semantic.Usage{
		InputTokens:           promptTokens,
		CachedInputTokens:     usage.CacheReadInputTokens,
		CacheWriteInputTokens: usage.CacheCreationInputTokens,
		OutputTokens:          usage.OutputTokens,
		ReasoningTokens:       usage.ThinkingTokens,
		TotalTokens:           promptTokens + usage.OutputTokens,
		Source:                semantic.UsageProviderReported,
	}
}

// recordUsageTiers copies the provider's token breakdown onto the settlement and
// marks the cost estimated whenever a tier is reported that the price cannot
// express.
//
// A cache read now has its own rate on the price version, so a cached span is
// charged at what it actually costs and needs no such flag. A cache *write*
// still does: upstreams bill it at a premium over the input rate — 1.25x on
// Anthropic's table — and a Deployment has no term for it, so those tokens are
// charged at the ordinary input rate and the row is marked estimated. That is an
// under-charge rather than an upper bound, which is exactly why it must stay
// visible: CostEstimated is the signal that identifies the rows worth re-rating
// once a price version can express the write tier.
func recordUsageTiers(result *budget.Settlement, usage semantic.Usage) {
	result.ProviderCachedInputTokens = usage.CachedInputTokens
	result.ProviderCacheWriteInputTokens = usage.CacheWriteInputTokens
	result.ProviderReasoningTokens = usage.ReasoningTokens
	if usage.CacheWriteInputTokens > 0 {
		result.CostEstimated = true
	}
}

func setSettlementCost(result *budget.Settlement, target provider.Target, reservationMicrosUSD int64) {
	cost, err := budget.EstimateCostMicros(budget.TokenCost{
		InputTokens:                 result.ProviderInputTokens,
		CachedInputTokens:           result.ProviderCachedInputTokens,
		OutputTokens:                result.ProviderOutputTokens,
		InputMicrosPerMillion:       target.InputMicrosPerMillion,
		CachedInputMicrosPerMillion: target.CachedInputMicrosPerMillion,
		OutputMicrosPerMillion:      target.OutputMicrosPerMillion,
	})
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
		result.ErrorClass = string(provider.ErrorCanceled)
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
		recordUsageTiers(&result, *usage)
	} else if providerErr == nil || emitted {
		estimate := cappedOutputEstimate(estimatedOutputTokens, deliveredOutputTokens)
		result.ProviderInputTokens = estimatedInputTokens
		result.ProviderOutputTokens = estimate
		result.PreparedOutputTokens = estimate
		result.TokenEstimated = true
		result.CostEstimated = true
	} else {
		var classified *provider.Error
		if !errors.As(providerErr, &classified) || !classified.Ambiguous {
			// A definitive failure with nothing delivered and no usage means
			// the provider never served this attempt, so there is no cost to
			// commit — not even the fixed per-request fee, which pays for a
			// request the provider actually received. This mirrors how
			// settlementForResult treats the same outcome.
			return result
		}
		// Nothing was delivered, so there is nothing to bound the estimate
		// with: an ambiguous failure means the request may have been served
		// in full upstream, and that is what the reservation covers.
		result.ProviderInputTokens = estimatedInputTokens
		result.ProviderOutputTokens = estimatedOutputTokens
		result.PreparedOutputTokens = estimatedOutputTokens
		result.TokenEstimated = true
		result.CostEstimated = true
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

// estimateGenerateInputTokens is estimateInputTokens for a request that may carry
// images. The wire bytes include each image's URL — a data URL is the whole
// picture in base64 — and counting that as text is how a 400 KB photograph became
// a six-figure token estimate. Take those bytes back out and charge each image the
// ceiling in semantic instead.
func estimateGenerateInputTokens(bytes int64, request semantic.GenerateRequest) int64 {
	images := int64(0)
	for _, message := range request.Messages {
		for _, part := range message.Content {
			if part.Kind != semantic.ContentInputImage {
				continue
			}
			images++
			bytes -= int64(len(part.URL))
		}
	}
	if images == 0 {
		return estimateInputTokens(bytes)
	}
	return estimateInputTokens(bytes) + images*semantic.ImageInputTokenCeiling
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

// returnFailure is gatewayError for the failures on the way back: the upstream
// answered, the attempt settled, and the answer could not be turned into the
// shape this surface returns.
//
// These are the only 502s the attempt log never sees — logProviderFailure runs
// where a provider error is recorded, and here there is no provider error. That
// left the operator with a 502 whose sentence names a stage and nothing else,
// and it is the failure most likely to be Halro's own: a termination, a content
// block or a usage shape one side of the translation cannot express.
//
// The cause is written because these sentences are Halro's, produced by its own
// decoders and renderers. No provider body reaches them.
func (s *Service) returnFailure(ctx context.Context, surface, message string, cause error) *Error {
	requestID, _ := requestmeta.RequestID(ctx)
	attributes := []any{"request_id", requestID, "surface", surface, "stage", message}
	if cause != nil {
		attributes = append(attributes, "reason", cause.Error())
	}
	s.logger.Warn("provider response could not be returned to the caller", attributes...)
	return gatewayError("provider_error", message, 502, cause)
}
