package budget

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/id"
	"github.com/akz142857/Halro/internal/ledger"
)

var (
	ErrExceeded      = errors.New("daily budget exceeded")
	ErrInvalidAmount = errors.New("invalid accounting amount")
)

type Attempt struct {
	RequestID                   string
	AttemptID                   string
	ProjectID                   string
	Period                      Period
	ReservationMicrosUSD        int64
	KeyID                       string
	RouteID                     string
	DeploymentID                string
	ProviderID                  string
	RequestedModel              string
	ProviderModel               string
	AttemptNumber               int
	RetryCount                  int
	FallbackCount               int
	LeaseMode                   ledger.LeaseMode
	PriceSnapshot               *domain.PriceSnapshot
	PreparedInputTokens         int64
	PreparedOutputTokens        int64
	RecoveryKey                 string
	ReservationSequence         uint64
	UnknownPolicyEvidence       *domain.UnknownPricePolicyEvidence
	TokenGuardPricingViewDigest string
}

type Request struct {
	RequestID string
	ProjectID string
	// Period is fixed when the request is accepted and inherited by every
	// event that follows it. A request that outlives a period boundary settles
	// in the period it began in, matching how its price snapshot is pinned.
	Period         Period
	KeyID          string
	RequestedModel string
}

type AttemptMetadata struct {
	RouteID       string
	DeploymentID  string
	ProviderID    string
	ProviderModel string
	AttemptNumber int
	RetryCount    int
	FallbackCount int
}

type LeaseSpec struct {
	AttemptID                   string
	Mode                        ledger.LeaseMode
	ReservationMicrosUSD        int64
	PriceSnapshot               *domain.PriceSnapshot
	PreparedInputTokens         int64
	PreparedOutputTokens        int64
	RecoveryKey                 string
	UnknownPolicyEvidence       *domain.UnknownPricePolicyEvidence
	TokenGuardPricingViewDigest string
}

type Settlement struct {
	CommittedMicrosUSD   int64
	ProviderInputTokens  int64
	ProviderOutputTokens int64
	// The cache tiers are breakdown subsets of ProviderInputTokens, recorded so
	// the ledger can show why a request cost what it did and so a later pricing
	// version can re-rate the span without replaying provider traffic.
	ProviderCachedInputTokens     int64
	ProviderCacheWriteInputTokens int64
	ProviderReasoningTokens       int64
	PreparedOutputTokens          int64
	CostEstimated                 bool
	TokenEstimated                bool
	Outcome                       string
	ErrorClass                    string
	HTTPStatus                    int
	LatencyMillis                 int64
	// OccurredAt is reserved for deterministic recovery events. Normal
	// callers leave it zero and the manager captures its clock at commit.
	OccurredAt time.Time
}

type Manager struct {
	projectLocks sync.Map
	applyMu      sync.Mutex
	applyCond    *sync.Cond
	applyErr     error
	observerMu   sync.RWMutex
	log          *ledger.Log
	state        *ledger.State
	periods      *PeriodResolver
	now          func() time.Time
	observers    []func(ledger.Record)
	recoveryMu   sync.RWMutex
	recovery     RecoveryStats

	lockAcquisitions atomic.Uint64
	lockWaitNanos    atomic.Int64
	lockHeldNanos    atomic.Int64
}

type RecoveryStats struct {
	PendingObserved       uint64
	ReleasedNotStarted    uint64
	ConservativelySettled uint64
	Failures              uint64
	OldestObservedAge     time.Duration
}

func (m *Manager) RecoveryStats() RecoveryStats {
	m.recoveryMu.RLock()
	defer m.recoveryMu.RUnlock()
	return m.recovery
}

func (m *Manager) AddObserver(observer func(ledger.Record)) {
	if observer == nil {
		return
	}
	m.observerMu.Lock()
	m.observers = append(m.observers, observer)
	m.observerMu.Unlock()
}

// Options carries the dependencies a caller may substitute. Everything here has a working
// default, so New remains the normal entry point.
type Options struct {
	// Now supplies the clock that buckets a request into its accounting period. Tests that
	// assert on a specific period must set it: the manager derives PeriodID itself, so a
	// clock injected anywhere else — the gateway service, for instance — does not move the
	// bucket, and an assertion written against a fixed date silently starts failing the
	// next day.
	Now func() time.Time
}

func New(log *ledger.Log, state *ledger.State, periods *PeriodResolver) (*Manager, error) {
	return NewWithOptions(log, state, periods, Options{})
}

func NewWithOptions(
	log *ledger.Log,
	state *ledger.State,
	periods *PeriodResolver,
	options Options,
) (*Manager, error) {
	if log == nil || state == nil || periods == nil {
		return nil, errors.New("ledger log, state, and period resolver are required")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	manager := &Manager{
		log:     log,
		state:   state,
		periods: periods,
		now:     options.Now,
	}
	manager.applyCond = sync.NewCond(&manager.applyMu)
	return manager, nil
}

// Periods exposes the resolver so callers that must report a period — the
// Admin API, diagnostics — read the same one the ledger is written with.
func (m *Manager) Periods() *PeriodResolver { return m.periods }

// inAccountingZone renders an instant in the accounting zone. The instant is
// unchanged; only the offset a ledger event serializes with is. Falling back to
// the value as given is deliberate: a zone that cannot be loaded must not
// silently reinterpret a timestamp.
func (m *Manager) inAccountingZone(instant time.Time) time.Time {
	location, err := m.periods.LocationAt(instant)
	if err != nil {
		return instant
	}
	return instant.In(location)
}

func (m *Manager) localNow() time.Time {
	return m.inAccountingZone(m.now())
}

// lockProject serializes one project's admission decisions — reading its balance
// and deciding whether one more reservation fits — and nothing else.
//
// It is never held across a Ledger append. That is a protocol rule, not an
// accident of the current code: holding it across the append made a project's
// request rate a function of fsync latency, since its events could then never
// share a WAL batch (ADR 0018). A durable step added inside this lock would grow
// the ceiling straight back.
//
// The events that carry no decision — request accepted, attempt started,
// settled, finalized — take no lock at all. Ordering does not come from here:
// ledger.State applies records in WAL sequence order globally, and per-attempt
// causality comes from callers awaiting each step before the next, which
// Service.startAttempt does. A caller that fired an attempt's events
// concurrently would break that, and no lock in this package would save it.
//
// Wait and hold times are accumulated because the remaining contention is
// otherwise invisible in production: an operator sees latency rise with nothing
// to attribute it to. Deliberately aggregate, with no project label, since
// project count is unbounded and high-cardinality labels are not allowed.
// projectAdmission is one project's admission state. The pending amounts live
// here rather than in a map shared by every project, because the lock guarding
// them is per-project: a shared map would be written under different locks by
// different projects, which is a data race and not a subtle one — the
// multi-project benchmark hit it on the first run.
type projectAdmission struct {
	mu sync.Mutex
	// pending is spend admitted but not yet visible in the Ledger's own balance,
	// keyed by period so a request near midnight counts against its own day.
	pending map[ledger.BalanceKey]int64
}

func (m *Manager) lockProject(projectID string) (*projectAdmission, func()) {
	value, loaded := m.projectLocks.Load(projectID)
	if !loaded {
		value, _ = m.projectLocks.LoadOrStore(projectID, &projectAdmission{})
	}
	admission := value.(*projectAdmission)
	waitStarted := time.Now()
	admission.mu.Lock()
	held := time.Now()
	m.lockWaitNanos.Add(int64(held.Sub(waitStarted)))
	m.lockAcquisitions.Add(1)
	return admission, func() {
		m.lockHeldNanos.Add(int64(time.Since(held)))
		admission.mu.Unlock()
	}
}

// ProjectLockStats reports contention on the per-project accounting lock.
// Acquisitions is the denominator for both durations.
type ProjectLockStats struct {
	Acquisitions uint64
	WaitDuration time.Duration
	HeldDuration time.Duration
}

func (m *Manager) ProjectLockStats() ProjectLockStats {
	return ProjectLockStats{
		Acquisitions: m.lockAcquisitions.Load(),
		WaitDuration: time.Duration(m.lockWaitNanos.Load()),
		HeldDuration: time.Duration(m.lockHeldNanos.Load()),
	}
}

// admit decides whether one more reservation fits the project's daily budget,
// and records it as spent until the Ledger says so itself.
//
// The count has to include reservations that were admitted but whose events are
// still in flight, or two concurrent requests would both read the same balance,
// both pass the same check, and both be let through. pendingAdmitted holds them
// for exactly that window: an amount is added here, under the lock, and removed
// only after Apply has made it visible in state.Balance. So it is counted in one
// of the two places at every instant and in neither at none, which is the only
// interval that could admit the same headroom twice.
//
// The reverse overlap does exist — between Apply and the release it is counted
// in both — and that direction is safe: it can only refuse at the very edge of
// a budget, never overspend it.
func (m *Manager) admit(key ledger.BalanceKey, dailyBudgetMicrosUSD, reservationMicrosUSD int64, counted bool) error {
	admission, unlock := m.lockProject(key.ProjectID)
	defer unlock()
	balance := m.state.Balance(key.ProjectID, key.PeriodID, key.TimezoneVersion)
	total, err := checkedAdd(balance.CommittedMicrosUSD, balance.ReservedMicrosUSD)
	if err != nil {
		return err
	}
	if total, err = checkedAdd(total, admission.pending[key]); err != nil {
		return err
	}
	if counted {
		if total, err = checkedAdd(total, reservationMicrosUSD); err != nil {
			return err
		}
	}
	if dailyBudgetMicrosUSD > 0 && total > dailyBudgetMicrosUSD {
		return ErrExceeded
	}
	if counted {
		if admission.pending == nil {
			admission.pending = make(map[ledger.BalanceKey]int64)
		}
		admission.pending[key] += reservationMicrosUSD
	}
	return nil
}

// admittedForTest reports spend admitted but not yet handed to the Ledger. Tests
// assert it drains to zero: an amount left here would hold budget headroom for
// the rest of the period with nothing to release it.
func (m *Manager) admittedForTest(key ledger.BalanceKey) int64 {
	admission, unlock := m.lockProject(key.ProjectID)
	defer unlock()
	return admission.pending[key]
}

// releaseAdmitted hands the amount over to the Ledger's own accounting. It runs
// after the append has either applied or failed, on the same goroutine that
// admitted it, so nothing has to record whose amount to remove.
func (m *Manager) releaseAdmitted(key ledger.BalanceKey, reservationMicrosUSD int64) {
	admission, unlock := m.lockProject(key.ProjectID)
	defer unlock()
	if remaining := admission.pending[key] - reservationMicrosUSD; remaining > 0 {
		admission.pending[key] = remaining
	} else {
		// Keyed by period, so entries would otherwise accumulate one per day.
		delete(admission.pending, key)
	}
}

// BeginAttempt checks the project budget and durably reserves spend before an
// upstream provider can receive the request. A zero budget means unlimited.
func (m *Manager) BeginAttempt(
	ctx context.Context,
	projectID string,
	dailyBudgetMicrosUSD int64,
	requestID string,
	reservationMicrosUSD int64,
) (Attempt, error) {
	if projectID == "" || requestID == "" {
		return Attempt{}, errors.New("project and request IDs are required")
	}
	request, err := m.BeginRequest(ctx, projectID, requestID)
	if err != nil {
		return Attempt{}, err
	}
	return m.ReserveAttempt(ctx, request, dailyBudgetMicrosUSD, reservationMicrosUSD)
}

func (m *Manager) BeginRequest(ctx context.Context, projectID, requestID string) (Request, error) {
	return m.BeginRequestDetailed(ctx, projectID, "", requestID, "")
}

func (m *Manager) BeginRequestDetailed(
	ctx context.Context,
	projectID, keyID, requestID, requestedModel string,
) (Request, error) {
	if projectID == "" || requestID == "" {
		return Request{}, errors.New("project and request IDs are required")
	}
	eventID, err := id.New("evt")
	if err != nil {
		return Request{}, err
	}
	now := m.localNow()
	period, err := m.periods.PeriodAt(now)
	if err != nil {
		return Request{}, err
	}
	request := Request{
		RequestID: requestID, ProjectID: projectID, Period: period,
		KeyID: keyID, RequestedModel: requestedModel,
	}
	event := ledger.Event{
		EventID: eventID, Kind: ledger.EventRequestAccepted,
		RequestID: request.RequestID, ProjectID: request.ProjectID,
		KeyID: request.KeyID, RequestedModel: request.RequestedModel,
		OccurredAt: now,
	}
	period.Stamp(&event)
	if err := m.appendApply(ctx, event); err != nil {
		return Request{}, err
	}
	return request, nil
}

func (m *Manager) ReserveAttempt(
	ctx context.Context,
	request Request,
	dailyBudgetMicrosUSD int64,
	reservationMicrosUSD int64,
) (Attempt, error) {
	return m.ReserveAttemptDetailed(ctx, request, dailyBudgetMicrosUSD, reservationMicrosUSD, AttemptMetadata{})
}

func (m *Manager) ReserveAttemptDetailed(
	ctx context.Context,
	request Request,
	dailyBudgetMicrosUSD int64,
	reservationMicrosUSD int64,
	metadata AttemptMetadata,
) (Attempt, error) {
	return m.reserveAttemptDetailed(ctx, request, dailyBudgetMicrosUSD, LeaseSpec{ReservationMicrosUSD: reservationMicrosUSD}, metadata)
}

func (m *Manager) ReserveLeaseDetailed(ctx context.Context, request Request, dailyBudgetMicrosUSD int64, spec LeaseSpec, metadata AttemptMetadata) (Attempt, error) {
	if spec.Mode == "" || spec.PriceSnapshot == nil || spec.PreparedInputTokens < 0 || spec.PreparedOutputTokens < 0 {
		return Attempt{}, ErrInvalidAmount
	}
	if !domain.ValidSHA256Label(spec.TokenGuardPricingViewDigest) {
		return Attempt{}, errors.New("token guard pricing view digest is required for a tagged lease")
	}
	if err := spec.PriceSnapshot.Validate(); err != nil {
		return Attempt{}, err
	}
	if spec.Mode == ledger.LeaseModeUnknownAllowed {
		if dailyBudgetMicrosUSD != 0 || spec.UnknownPolicyEvidence == nil || spec.UnknownPolicyEvidence.ProjectID != request.ProjectID {
			return Attempt{}, ErrInvalidAmount
		}
		if err := spec.UnknownPolicyEvidence.Validate(); err != nil {
			return Attempt{}, err
		}
	}
	return m.reserveAttemptDetailed(ctx, request, dailyBudgetMicrosUSD, spec, metadata)
}

func (m *Manager) reserveAttemptDetailed(
	ctx context.Context,
	request Request,
	dailyBudgetMicrosUSD int64,
	spec LeaseSpec,
	metadata AttemptMetadata,
) (Attempt, error) {
	if request.RequestID == "" || request.ProjectID == "" || request.Period.ID == "" {
		return Attempt{}, errors.New("request lease is invalid")
	}
	if dailyBudgetMicrosUSD < 0 || spec.ReservationMicrosUSD < 0 || (spec.Mode == "" && spec.ReservationMicrosUSD <= 0) {
		return Attempt{}, ErrInvalidAmount
	}
	attemptID := spec.AttemptID
	if attemptID == "" {
		var err error
		attemptID, err = id.New("att")
		if err != nil {
			return Attempt{}, err
		}
	}
	eventID, err := id.New("evt")
	if err != nil {
		return Attempt{}, err
	}
	attempt := Attempt{
		RequestID: request.RequestID, AttemptID: attemptID, ProjectID: request.ProjectID,
		Period: request.Period, ReservationMicrosUSD: spec.ReservationMicrosUSD,
		KeyID: request.KeyID, RequestedModel: request.RequestedModel,
		RouteID: metadata.RouteID, DeploymentID: metadata.DeploymentID,
		ProviderID: metadata.ProviderID, ProviderModel: metadata.ProviderModel,
		AttemptNumber: metadata.AttemptNumber, RetryCount: metadata.RetryCount,
		FallbackCount:       metadata.FallbackCount,
		LeaseMode:           spec.Mode,
		PreparedInputTokens: spec.PreparedInputTokens, PreparedOutputTokens: spec.PreparedOutputTokens,
		RecoveryKey:                 spec.RecoveryKey,
		UnknownPolicyEvidence:       spec.UnknownPolicyEvidence,
		TokenGuardPricingViewDigest: spec.TokenGuardPricingViewDigest,
	}
	if spec.PriceSnapshot != nil {
		snapshot := spec.PriceSnapshot.Clone()
		attempt.PriceSnapshot = &snapshot
	}
	// Admission is a decision about money and has to be atomic against other
	// requests in the same project; the durable write does not. Holding the lock
	// across the append is what made a project's request rate a function of fsync
	// latency (ADR 0018), so the lock ends here and the append happens without
	// it.
	key := ledger.BalanceKey{ProjectID: request.ProjectID, PeriodID: request.Period.ID, TimezoneVersion: request.Period.TimezoneVersion}
	counted := spec.Mode != ledger.LeaseModeUnknownAllowed
	if err := m.admit(key, dailyBudgetMicrosUSD, spec.ReservationMicrosUSD, counted); err != nil {
		return Attempt{}, err
	}
	if counted {
		// Released on every path out of here, so a reservation that never became
		// durable does not hold budget headroom for the rest of the period.
		defer m.releaseAdmitted(key, spec.ReservationMicrosUSD)
	}
	var reservationValue *int64
	if spec.Mode != ledger.LeaseModeUnknownAllowed {
		value := spec.ReservationMicrosUSD
		reservationValue = &value
	}
	var eventPriceSnapshot *domain.PriceSnapshot
	if attempt.PriceSnapshot != nil {
		snapshot := attempt.PriceSnapshot.Clone()
		eventPriceSnapshot = &snapshot
	}
	reserved := ledger.Event{
		EventID:              eventID,
		Kind:                 ledger.EventReservationCreated,
		RequestID:            request.RequestID,
		AttemptID:            attemptID,
		ProjectID:            request.ProjectID,
		KeyID:                request.KeyID,
		RouteID:              metadata.RouteID,
		DeploymentID:         metadata.DeploymentID,
		ProviderID:           metadata.ProviderID,
		RequestedModel:       request.RequestedModel,
		ProviderModel:        metadata.ProviderModel,
		AttemptNumber:        metadata.AttemptNumber,
		RetryCount:           metadata.RetryCount,
		FallbackCount:        metadata.FallbackCount,
		OccurredAt:           m.localNow(),
		ReservationMicrosUSD: reservationValue,
		LeaseMode:            spec.Mode, PriceSnapshot: eventPriceSnapshot,
		PreparedInputTokens: spec.PreparedInputTokens, PreparedOutputTokens: spec.PreparedOutputTokens,
		RecoveryKey: spec.RecoveryKey, UnknownPolicyEvidence: spec.UnknownPolicyEvidence,
		TokenGuardPricingViewDigest: spec.TokenGuardPricingViewDigest,
	}
	request.Period.Stamp(&reserved)
	record, err := m.appendApplyRecord(ctx, reserved)
	if err != nil {
		return Attempt{}, err
	}
	attempt.ReservationSequence = record.Sequence
	return attempt, nil
}

func (a Attempt) Request() Request {
	return Request{
		RequestID: a.RequestID, ProjectID: a.ProjectID, Period: a.Period,
		KeyID: a.KeyID, RequestedModel: a.RequestedModel,
	}
}

func (m *Manager) MarkStarted(ctx context.Context, attempt Attempt) error {
	eventID, err := id.New("evt")
	if err != nil {
		return err
	}
	started := ledger.Event{
		EventID:   eventID,
		Kind:      ledger.EventAttemptStarted,
		RequestID: attempt.RequestID,
		AttemptID: attempt.AttemptID,
		ProjectID: attempt.ProjectID,
		KeyID:     attempt.KeyID, RouteID: attempt.RouteID, DeploymentID: attempt.DeploymentID, ProviderID: attempt.ProviderID,
		RequestedModel: attempt.RequestedModel, ProviderModel: attempt.ProviderModel,
		AttemptNumber: attempt.AttemptNumber, RetryCount: attempt.RetryCount,
		FallbackCount: attempt.FallbackCount,
		OccurredAt:    m.localNow(),
	}
	attempt.Period.Stamp(&started)
	return m.appendApply(ctx, started)
}

// Settle releases the reservation and commits usage and cost in one durable
// event. Callers should use a cleanup context that is independent of the client.
func (m *Manager) Settle(ctx context.Context, attempt Attempt, settlement Settlement) error {
	eventID, err := id.New("evt")
	if err != nil {
		return err
	}
	return m.settle(ctx, eventID, attempt, settlement)
}

func (m *Manager) settle(ctx context.Context, eventID string, attempt Attempt, settlement Settlement) error {
	if settlement.CommittedMicrosUSD < 0 ||
		settlement.ProviderInputTokens < 0 ||
		settlement.ProviderOutputTokens < 0 ||
		settlement.ProviderCachedInputTokens < 0 ||
		settlement.ProviderCacheWriteInputTokens < 0 ||
		settlement.ProviderReasoningTokens < 0 ||
		settlement.PreparedOutputTokens < 0 {
		return ErrInvalidAmount
	}
	// The tiers partition the input total. A sum that overruns it means a caller
	// added them on top instead of reporting a breakdown, which would let a
	// later re-rating charge the cached span twice.
	if settlement.ProviderCachedInputTokens+settlement.ProviderCacheWriteInputTokens > settlement.ProviderInputTokens {
		return ErrInvalidAmount
	}
	var priceSnapshot *domain.PriceSnapshot
	inputCost, outputCost, fixedCost := int64(0), int64(0), int64(0)
	if attempt.PriceSnapshot != nil {
		copy := attempt.PriceSnapshot.Clone()
		priceSnapshot = &copy
		if copy.CostValueStatus == domain.CostValueKnown && settlement.Outcome != "recovered_not_started" {
			cost, costErr := copy.Calculate(settlement.ProviderInputTokens, settlement.ProviderCachedInputTokens, settlement.ProviderOutputTokens)
			if costErr != nil {
				return costErr
			}
			if cost.TotalCostMicrosUSD != settlement.CommittedMicrosUSD {
				return fmt.Errorf("settlement cost does not match immutable price snapshot: calculated=%d committed=%d", cost.TotalCostMicrosUSD, settlement.CommittedMicrosUSD)
			}
			inputCost, outputCost, fixedCost = cost.InputCostMicrosUSD, cost.OutputCostMicrosUSD, cost.FixedCostMicrosUSD
		}
	}
	occurredAt := settlement.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = m.localNow()
	} else {
		occurredAt = m.inAccountingZone(occurredAt)
	}
	var committedValue *int64
	if attempt.LeaseMode != ledger.LeaseModeUnknownAllowed {
		value := settlement.CommittedMicrosUSD
		committedValue = &value
	}
	tokenSource := ledger.TokenUsageSourceNone
	if settlement.ProviderInputTokens != 0 || settlement.ProviderOutputTokens != 0 {
		tokenSource = ledger.TokenUsageSourceProvider
		if settlement.TokenEstimated {
			tokenSource = ledger.TokenUsageSourceEstimate
		}
	}
	settled := ledger.Event{
		EventID:            eventID,
		Kind:               ledger.EventAttemptSettled,
		RequestID:          attempt.RequestID,
		AttemptID:          attempt.AttemptID,
		ProjectID:          attempt.ProjectID,
		KeyID:              attempt.KeyID,
		RouteID:            attempt.RouteID,
		DeploymentID:       attempt.DeploymentID,
		ProviderID:         attempt.ProviderID,
		RequestedModel:     attempt.RequestedModel,
		ProviderModel:      attempt.ProviderModel,
		AttemptNumber:      attempt.AttemptNumber,
		RetryCount:         attempt.RetryCount,
		FallbackCount:      attempt.FallbackCount,
		OccurredAt:         occurredAt,
		CommittedMicrosUSD: committedValue,
		LeaseMode:          attempt.LeaseMode, PriceSnapshot: priceSnapshot,
		InputCostMicrosUSD: inputCost, OutputCostMicrosUSD: outputCost, FixedCostMicrosUSD: fixedCost,
		ProviderInputTokens:           settlement.ProviderInputTokens,
		ProviderOutputTokens:          settlement.ProviderOutputTokens,
		ProviderCachedInputTokens:     settlement.ProviderCachedInputTokens,
		ProviderCacheWriteInputTokens: settlement.ProviderCacheWriteInputTokens,
		ProviderReasoningTokens:       settlement.ProviderReasoningTokens,
		PreparedOutputTokens:          settlement.PreparedOutputTokens,
		CostEstimated:                 settlement.CostEstimated,
		TokenEstimated:                settlement.TokenEstimated,
		TokenUsageSource:              tokenSource,
		Outcome:                       settlement.Outcome,
		ErrorClass:                    settlement.ErrorClass,
		HTTPStatus:                    settlement.HTTPStatus,
		LatencyMillis:                 settlement.LatencyMillis,
	}
	attempt.Period.Stamp(&settled)
	return m.appendApply(ctx, settled)
}

// RecoverPendingLeases resolves every durable lease before listeners become
// ready. Leases without AttemptStarted are released; started leases are
// conservatively settled from their prepared token bounds and frozen price.
func (m *Manager) RecoverPendingLeases(ctx context.Context) error {
	pendingLeases := m.state.PendingLeases()
	now := m.now()
	m.recoveryMu.Lock()
	m.recovery.PendingObserved += uint64(len(pendingLeases))
	for _, pending := range pendingLeases {
		age := now.Sub(pending.Reservation.OccurredAt)
		if age > m.recovery.OldestObservedAge {
			m.recovery.OldestObservedAge = age
		}
	}
	m.recoveryMu.Unlock()
	for _, pending := range pendingLeases {
		event := pending.Reservation
		reservation := int64(0)
		if event.ReservationMicrosUSD != nil {
			reservation = *event.ReservationMicrosUSD
		}
		attempt := Attempt{
			RequestID: event.RequestID, AttemptID: event.AttemptID, ProjectID: event.ProjectID,
			Period: PeriodFromEvent(event), ReservationMicrosUSD: reservation,
			KeyID: event.KeyID, RouteID: event.RouteID, DeploymentID: event.DeploymentID,
			ProviderID: event.ProviderID, RequestedModel: event.RequestedModel, ProviderModel: event.ProviderModel,
			AttemptNumber: event.AttemptNumber, RetryCount: event.RetryCount, FallbackCount: event.FallbackCount,
			LeaseMode: event.LeaseMode, PriceSnapshot: event.PriceSnapshot,
			PreparedInputTokens: event.PreparedInputTokens, PreparedOutputTokens: event.PreparedOutputTokens,
			RecoveryKey: event.RecoveryKey, UnknownPolicyEvidence: event.UnknownPolicyEvidence,
			TokenGuardPricingViewDigest: event.TokenGuardPricingViewDigest,
		}
		settlement := Settlement{Outcome: "recovered_not_started", OccurredAt: event.OccurredAt}
		if pending.Started {
			settlement = Settlement{
				ProviderInputTokens: event.PreparedInputTokens, ProviderOutputTokens: event.PreparedOutputTokens,
				PreparedOutputTokens: event.PreparedOutputTokens, TokenEstimated: true, CostEstimated: true,
				Outcome: "recovered_started_unknown_result", OccurredAt: event.OccurredAt,
			}
			if event.PriceSnapshot != nil && event.PriceSnapshot.CostValueStatus == domain.CostValueKnown {
				// Nothing is known about how much of a recovered attempt's prompt
				// the provider served from cache, so none of it is: the whole
				// prompt is charged at the ordinary input rate, which is the
				// conservative reading of an ambiguous outcome.
				cost, err := event.PriceSnapshot.Calculate(event.PreparedInputTokens, 0, event.PreparedOutputTokens)
				if err != nil {
					return fmt.Errorf("recover attempt %q: %w", event.AttemptID, err)
				}
				settlement.CommittedMicrosUSD = cost.TotalCostMicrosUSD
			}
		}
		digest := sha256.Sum256([]byte("halro:accounting-recovery:v1\x00" + event.AttemptID + "\x00" + settlement.Outcome))
		eventID := "evt_recovery_" + hex.EncodeToString(digest[:12])
		if err := m.settle(ctx, eventID, attempt, settlement); err != nil {
			m.recoveryMu.Lock()
			m.recovery.Failures++
			m.recoveryMu.Unlock()
			return fmt.Errorf("recover attempt %q: %w", event.AttemptID, err)
		}
		m.recoveryMu.Lock()
		if pending.Started {
			m.recovery.ConservativelySettled++
		} else {
			m.recovery.ReleasedNotStarted++
		}
		m.recoveryMu.Unlock()
	}
	return nil
}

func (m *Manager) FinalizeRequest(ctx context.Context, attempt Attempt, outcome string) error {
	return m.Finalize(ctx, attempt.Request(), outcome)
}

func (m *Manager) Finalize(ctx context.Context, request Request, outcome string) error {
	eventID, err := id.New("evt")
	if err != nil {
		return err
	}
	finalized := ledger.Event{
		EventID:   eventID,
		Kind:      ledger.EventRequestFinalized,
		RequestID: request.RequestID,
		ProjectID: request.ProjectID,
		KeyID:     request.KeyID, RequestedModel: request.RequestedModel,
		OccurredAt: m.localNow(),
		Outcome:    outcome,
	}
	request.Period.Stamp(&finalized)
	return m.appendApply(ctx, finalized)
}

func (m *Manager) appendApply(ctx context.Context, event ledger.Event) error {
	_, err := m.appendApplyRecord(ctx, event)
	return err
}

// applyFailure reports the terminal apply error, if the state machine has hit
// one.
func (m *Manager) applyFailure() error {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	return m.applyErr
}

func (m *Manager) appendApplyRecord(ctx context.Context, event ledger.Event) (ledger.Record, error) {
	// A failed apply is terminal: the state machine advances in sequence order
	// and can never get past the event it choked on. Appending anyway would
	// fsync a record that will never be applied, and leave it in the log for
	// replay to choke on again at the next start — with a longer tail of
	// unappliable records behind it every time. Refuse before the write.
	if err := m.applyFailure(); err != nil {
		return ledger.Record{}, err
	}
	watermark, err := m.log.Append(ctx, event)
	if err != nil {
		return ledger.Record{}, err
	}
	record := ledger.Record{
		Sequence: watermark.Sequence,
		Offset:   watermark.Offset,
		Event:    event,
	}
	m.applyMu.Lock()
	for m.applyErr == nil && m.state.Watermark().Sequence+1 < record.Sequence {
		m.applyCond.Wait()
	}
	if m.applyErr != nil {
		err := m.applyErr
		m.applyMu.Unlock()
		return ledger.Record{}, err
	}
	if err := m.state.Apply(record); err != nil {
		m.applyErr = fmt.Errorf("apply durable accounting event: %w", err)
		m.applyCond.Broadcast()
		m.applyMu.Unlock()
		// Accounting is the gateway's fail-closed gate, and it reads the
		// ledger's status. Leaving that healthy while this manager can no
		// longer apply anything would give two answers to one question.
		m.log.Status().MarkUnavailable()
		return ledger.Record{}, fmt.Errorf("apply durable accounting event: %w", err)
	}
	m.observerMu.RLock()
	observers := slices.Clone(m.observers)
	m.observerMu.RUnlock()
	for _, observer := range observers {
		observer(record)
	}
	m.applyCond.Broadcast()
	m.applyMu.Unlock()
	return record, nil
}

// TokenCost is one attempt's priced tokens together with the rates they are
// billed at. CachedInputTokens is a subset of InputTokens — the span the
// provider served from its own cache — and is billed at
// CachedInputMicrosPerMillion instead of the ordinary input rate. Callers that
// do not know the split, such as a reservation taken before the provider
// answers, leave it zero and pay the ordinary rate for the whole prompt.
type TokenCost struct {
	InputTokens                 int64
	CachedInputTokens           int64
	OutputTokens                int64
	InputMicrosPerMillion       int64
	CachedInputMicrosPerMillion int64
	OutputMicrosPerMillion      int64
}

// EstimateCostMicros rounds each priced span up independently, matching
// domain.PriceSnapshot.Calculate exactly: a settlement whose committed amount
// disagrees with the snapshot by even one micro-USD is rejected as not matching
// its frozen price.
func EstimateCostMicros(cost TokenCost) (int64, error) {
	if cost.InputTokens < 0 || cost.CachedInputTokens < 0 || cost.OutputTokens < 0 ||
		cost.InputMicrosPerMillion < 0 || cost.CachedInputMicrosPerMillion < 0 || cost.OutputMicrosPerMillion < 0 ||
		cost.CachedInputTokens > cost.InputTokens {
		return 0, ErrInvalidAmount
	}
	inputCost, err := multiplyDivideCeil(cost.InputTokens-cost.CachedInputTokens, cost.InputMicrosPerMillion, 1_000_000)
	if err != nil {
		return 0, err
	}
	cachedCost, err := multiplyDivideCeil(cost.CachedInputTokens, cost.CachedInputMicrosPerMillion, 1_000_000)
	if err != nil {
		return 0, err
	}
	promptCost, err := checkedAdd(inputCost, cachedCost)
	if err != nil {
		return 0, err
	}
	outputCost, err := multiplyDivideCeil(cost.OutputTokens, cost.OutputMicrosPerMillion, 1_000_000)
	if err != nil {
		return 0, err
	}
	return checkedAdd(promptCost, outputCost)
}

func multiplyDivideCeil(left, right, divisor int64) (int64, error) {
	if left == 0 || right == 0 {
		return 0, nil
	}
	if left > math.MaxInt64/right {
		return 0, errors.New("accounting integer overflow")
	}
	product := left * right
	quotient := product / divisor
	if product%divisor != 0 {
		quotient++
	}
	return quotient, nil
}

func checkedAdd(left, right int64) (int64, error) {
	if right > 0 && left > math.MaxInt64-right {
		return 0, errors.New("accounting integer overflow")
	}
	return left + right, nil
}
