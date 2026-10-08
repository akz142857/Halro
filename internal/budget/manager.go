package budget

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

var (
	ErrExceeded            = errors.New("daily budget exceeded")
	ErrRunExceeded         = errors.New("run budget exceeded")
	ErrRunPriceUnavailable = errors.New("run budget requires a known price")
	ErrInvalidAmount       = errors.New("invalid accounting amount")
	ErrRunNotFound         = errors.New("run was not found")
	ErrRunNotActive        = errors.New("run is not active")
	ErrWorkUnitClosed      = errors.New("work unit is closed")
	ErrWorkUnitNotFound    = errors.New("work unit was not found")
	ErrResourceLimit       = errors.New("run governance resource limit exceeded")
	ErrIdempotencyConflict = errors.New("idempotency key conflicts with another request")
)

type Attempt struct {
	RequestID                   string
	AttemptID                   string
	ProjectID                   string
	WorkUnitID                  string
	RunID                       string
	Period                      Period
	ReservationMicrosUSD        int64
	KeyID                       string
	RouteID                     string
	DeploymentID                string
	ProviderID                  string
	OfferingID                  domain.ProviderOfferingID
	ProfileID                   domain.ProviderProfileID
	AccountRegionID             domain.ProviderRegionID
	RequestedModel              string
	ProviderModel               string
	ProviderPrimitive           provider.Primitive
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
	RequestID  string
	ProjectID  string
	WorkUnitID string
	RunID      string
	// Period is fixed when the request is accepted and inherited by every
	// event that follows it. A request that outlives a period boundary settles
	// in the period it began in, matching how its price snapshot is pinned.
	Period         Period
	KeyID          string
	RequestedModel string
}

type AttemptMetadata struct {
	RouteID           string
	DeploymentID      string
	ProviderID        string
	OfferingID        domain.ProviderOfferingID
	ProfileID         domain.ProviderProfileID
	AccountRegionID   domain.ProviderRegionID
	ProviderModel     string
	ProviderPrimitive provider.Primitive
	AttemptNumber     int
	RetryCount        int
	FallbackCount     int
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
	// The upstream's own identifiers for this failure, and the phase it
	// happened in. They are carried through to the ledger so a support ticket
	// raised days later can still name the request the upstream saw; the
	// gateway narrows them before they get here.
	ProviderCode             string
	ProviderFailureReason    provider.FailureReason
	Retryable                bool
	Ambiguous                bool
	FailureSemanticsRecorded bool
	ProviderRequestID        string
	FailurePhase             string
	LatencyMillis            int64
	// OccurredAt is reserved for deterministic recovery events. Normal
	// callers leave it zero and the manager captures its clock at commit.
	OccurredAt time.Time
}

type GovernanceIntent struct {
	Operation          string
	IdempotencyKeyHash string
	RequestFingerprint string
}

func (i GovernanceIntent) validate() error {
	if i.Operation == "" || !domain.ValidSHA256Label(i.IdempotencyKeyHash) || !domain.ValidSHA256Label(i.RequestFingerprint) {
		return errors.New("governance idempotency evidence is invalid")
	}
	return nil
}

type Manager struct {
	operationMu      sync.Mutex
	retiring         bool
	paused           *accountingPause
	activeOperations uint64
	operationDrain   chan struct{}
	projectLocks     projectAdmissionRegistry
	applyMu          sync.Mutex
	applyCond        *sync.Cond
	applyErr         error
	observerMu       sync.RWMutex
	log              *ledger.Log
	owner            accountingOwner
	// Default recovery retains its legacy full collection until Runtime
	// selects exclusive two-level recovery. Projection owners reject it.
	legacyPending func() []ledger.PendingLease
	reads         ledger.AccountingAdmissionReader
	periods       *PeriodResolver
	now           func() time.Time
	observers     []func(ledger.Record)
	recoveryMu    sync.RWMutex
	recovery      RecoveryStats

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

// Observers read frozen durable evidence synchronously. They must not mutate
// it or reenter Ledger append, maintenance, Close or Manager.Retire here.
func (m *Manager) AddObserver(observer func(ledger.Record)) error {
	_, release, err := m.enterOperation(context.Background())
	if err != nil {
		return err
	}
	defer release()
	if observer == nil {
		return nil
	}
	m.observerMu.Lock()
	m.observers = append(m.observers, observer)
	m.observerMu.Unlock()
	return nil
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
	manager := newWithOwner(log, stateAccountingOwner{state}, periods, options)
	manager.legacyPending = state.PendingLeases
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

func (m *Manager) governanceLock(projectID string) func() {
	admission := m.projectLocks.retain(projectID)
	admission.lifecycle.Lock()
	return func() { admission.lifecycle.Unlock(); m.projectLocks.release(projectID, admission) }
}

func (m *Manager) governanceReadLock(projectID string) func() {
	admission := m.projectLocks.retain(projectID)
	admission.lifecycle.RLock()
	return func() { admission.lifecycle.RUnlock(); m.projectLocks.release(projectID, admission) }
}

func (m *Manager) CreateWorkUnit(ctx context.Context, projectID, keyID string, maxOpen int64, intent GovernanceIntent) (domain.WorkUnit, bool, error) {
	return m.CreateWorkUnitWithDefinitions(ctx, projectID, keyID, maxOpen, nil, intent)
}

func (m *Manager) CreateWorkUnitWithDefinitions(ctx context.Context, projectID, keyID string, maxOpen int64, definitions []domain.OutcomeDefinitionRef, intent GovernanceIntent) (domain.WorkUnit, bool, error) {
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return domain.WorkUnit{}, false, err
	}
	defer release()
	if projectID == "" || keyID == "" || maxOpen <= 0 || maxOpen > domain.MaxOpenWorkUnits {
		return domain.WorkUnit{}, false, ErrResourceLimit
	}
	if err := intent.validate(); err != nil {
		return domain.WorkUnit{}, false, err
	}
	unlock := m.governanceLock(projectID)
	defer unlock()
	previous, ok, err := m.reads.ReadGovernanceIdempotency(ctx, projectID, intent.Operation, intent.IdempotencyKeyHash)
	if err != nil {
		return domain.WorkUnit{}, false, err
	}
	if ok {
		if previous.RequestFingerprint != intent.RequestFingerprint {
			return domain.WorkUnit{}, false, ErrIdempotencyConflict
		}
		workUnit, found, err := m.ReadWorkUnit(ctx, projectID, previous.WorkUnitID)
		if err != nil {
			return domain.WorkUnit{}, false, err
		}
		if !found {
			return domain.WorkUnit{}, false, errors.New("idempotent work unit is absent from accounting state")
		}
		return workUnit, true, nil
	}
	open, err := m.reads.ReadOpenWorkUnitCount(ctx, projectID)
	if err != nil {
		return domain.WorkUnit{}, false, err
	}
	if open >= maxOpen {
		return domain.WorkUnit{}, false, ErrResourceLimit
	}
	workUnitID, err := id.New("wku")
	if err != nil {
		return domain.WorkUnit{}, false, err
	}
	eventID, err := id.New("evt")
	if err != nil {
		return domain.WorkUnit{}, false, err
	}
	now := m.localNow()
	period, err := m.periods.PeriodAt(now)
	if err != nil {
		return domain.WorkUnit{}, false, err
	}
	event := ledger.Event{
		EventID: eventID, Kind: ledger.EventWorkUnitCreated,
		ProjectID: projectID, KeyID: keyID, WorkUnitID: workUnitID, OccurredAt: now,
		Operation: intent.Operation, IdempotencyKeyHash: intent.IdempotencyKeyHash, RequestFingerprint: intent.RequestFingerprint,
		OutcomeDefinitions: slices.Clone(definitions),
	}
	period.Stamp(&event)
	if err := m.appendApply(ctx, event); err != nil {
		return domain.WorkUnit{}, false, err
	}
	workUnit, ok, err := m.ReadWorkUnit(ctx, projectID, workUnitID)
	if err != nil {
		return domain.WorkUnit{}, false, err
	}
	if !ok {
		return domain.WorkUnit{}, false, errors.New("created work unit is absent from accounting state")
	}
	return workUnit, false, nil
}

func (m *Manager) CloseWorkUnit(ctx context.Context, projectID, keyID, workUnitID string, intent GovernanceIntent) (domain.WorkUnit, bool, error) {
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return domain.WorkUnit{}, false, err
	}
	defer release()
	if projectID == "" || keyID == "" || workUnitID == "" {
		return domain.WorkUnit{}, false, ErrWorkUnitNotFound
	}
	if err := intent.validate(); err != nil {
		return domain.WorkUnit{}, false, err
	}
	unlock := m.governanceLock(projectID)
	defer unlock()
	previous, ok, err := m.reads.ReadGovernanceIdempotency(ctx, projectID, intent.Operation, intent.IdempotencyKeyHash)
	if err != nil {
		return domain.WorkUnit{}, false, err
	}
	if ok {
		if previous.RequestFingerprint != intent.RequestFingerprint {
			return domain.WorkUnit{}, false, ErrIdempotencyConflict
		}
		workUnit, found, err := m.ReadWorkUnit(ctx, projectID, previous.WorkUnitID)
		if err != nil {
			return domain.WorkUnit{}, false, err
		}
		if !found {
			return domain.WorkUnit{}, false, errors.New("idempotent work unit is absent from accounting state")
		}
		return workUnit, true, nil
	}
	workUnit, ok, err := m.ReadWorkUnit(ctx, projectID, workUnitID)
	if err != nil {
		return domain.WorkUnit{}, false, err
	}
	if !ok || workUnit.ProjectID != projectID {
		return domain.WorkUnit{}, false, ErrWorkUnitNotFound
	}
	if workUnit.Status == domain.WorkUnitClosed {
		return workUnit, true, nil
	}
	eventID, err := id.New("evt")
	if err != nil {
		return domain.WorkUnit{}, false, err
	}
	now := m.localNow()
	period, err := m.periods.PeriodAt(now)
	if err != nil {
		return domain.WorkUnit{}, false, err
	}
	event := ledger.Event{
		EventID: eventID, Kind: ledger.EventWorkUnitClosed,
		ProjectID: projectID, KeyID: keyID, WorkUnitID: workUnitID, OccurredAt: now,
		Operation: intent.Operation, IdempotencyKeyHash: intent.IdempotencyKeyHash, RequestFingerprint: intent.RequestFingerprint,
	}
	period.Stamp(&event)
	if err := m.appendApply(ctx, event); err != nil {
		return domain.WorkUnit{}, false, err
	}
	closed, found, err := m.ReadWorkUnit(ctx, projectID, workUnitID)
	if err != nil {
		return domain.WorkUnit{}, false, err
	}
	if !found {
		return domain.WorkUnit{}, false, errors.New("closed work unit is absent from accounting view")
	}
	return closed, false, nil
}

func (m *Manager) CreateRun(
	ctx context.Context,
	projectID, keyID, workUnitID string,
	budgetMicrosUSD int64,
	ttl time.Duration,
	maxActive int64,
	intent GovernanceIntent,
) (domain.Run, bool, error) {
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return domain.Run{}, false, err
	}
	defer release()
	if projectID == "" || keyID == "" || workUnitID == "" || budgetMicrosUSD <= 0 ||
		ttl <= 0 || ttl > time.Duration(domain.MaxRunTTLSeconds)*time.Second ||
		maxActive <= 0 || maxActive > domain.MaxActiveRuns {
		return domain.Run{}, false, ErrResourceLimit
	}
	if err := intent.validate(); err != nil {
		return domain.Run{}, false, err
	}
	unlock := m.governanceLock(projectID)
	defer unlock()
	previous, ok, err := m.reads.ReadGovernanceIdempotency(ctx, projectID, intent.Operation, intent.IdempotencyKeyHash)
	if err != nil {
		return domain.Run{}, false, err
	}
	if ok {
		if previous.RequestFingerprint != intent.RequestFingerprint {
			return domain.Run{}, false, ErrIdempotencyConflict
		}
		run, found, err := m.ReadRun(ctx, projectID, previous.RunID)
		if err != nil {
			return domain.Run{}, false, err
		}
		if !found {
			return domain.Run{}, false, errors.New("idempotent run is absent from accounting state")
		}
		return run, true, nil
	}
	workUnit, ok, err := m.ReadWorkUnit(ctx, projectID, workUnitID)
	if err != nil {
		return domain.Run{}, false, err
	}
	if !ok || workUnit.ProjectID != projectID || workUnit.Status != domain.WorkUnitOpen {
		return domain.Run{}, false, ErrWorkUnitClosed
	}
	now := m.localNow()
	capacity, err := m.reads.ReadRunCapacity(ctx, projectID, workUnitID, now)
	if err != nil {
		return domain.Run{}, false, err
	}
	if capacity.ActiveRuns >= maxActive || capacity.WorkUnitRuns >= domain.MaxRunsPerWorkUnit {
		return domain.Run{}, false, ErrResourceLimit
	}
	runID, err := id.New("run")
	if err != nil {
		return domain.Run{}, false, err
	}
	eventID, err := id.New("evt")
	if err != nil {
		return domain.Run{}, false, err
	}
	period, err := m.periods.PeriodAt(now)
	if err != nil {
		return domain.Run{}, false, err
	}
	event := ledger.Event{
		EventID: eventID, Kind: ledger.EventRunCreated,
		ProjectID: projectID, KeyID: keyID, WorkUnitID: workUnitID, RunID: runID,
		RunBudgetMicrosUSD: budgetMicrosUSD, RunExpiresAt: now.Add(ttl), OccurredAt: now,
		Operation: intent.Operation, IdempotencyKeyHash: intent.IdempotencyKeyHash, RequestFingerprint: intent.RequestFingerprint,
	}
	period.Stamp(&event)
	if err := m.appendApply(ctx, event); err != nil {
		return domain.Run{}, false, err
	}
	run, ok, err := m.ReadRun(ctx, projectID, runID)
	if err != nil {
		return domain.Run{}, false, err
	}
	if !ok {
		return domain.Run{}, false, errors.New("created run is absent from accounting state")
	}
	return run, false, nil
}

func (m *Manager) CloseRun(ctx context.Context, projectID, keyID, workUnitID, runID, reason string, intent GovernanceIntent) (domain.Run, bool, error) {
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return domain.Run{}, false, err
	}
	defer release()
	if projectID == "" || keyID == "" || workUnitID == "" || runID == "" || len(reason) == 0 || len(reason) > 64 {
		return domain.Run{}, false, ErrRunNotFound
	}
	if err := intent.validate(); err != nil {
		return domain.Run{}, false, err
	}
	unlock := m.governanceLock(projectID)
	defer unlock()
	previous, ok, err := m.reads.ReadGovernanceIdempotency(ctx, projectID, intent.Operation, intent.IdempotencyKeyHash)
	if err != nil {
		return domain.Run{}, false, err
	}
	if ok {
		if previous.RequestFingerprint != intent.RequestFingerprint {
			return domain.Run{}, false, ErrIdempotencyConflict
		}
		run, found, err := m.ReadRun(ctx, projectID, previous.RunID)
		if err != nil {
			return domain.Run{}, false, err
		}
		if !found {
			return domain.Run{}, false, errors.New("idempotent run is absent from accounting state")
		}
		return run, true, nil
	}
	run, ok, err := m.ReadRun(ctx, projectID, runID)
	if err != nil {
		return domain.Run{}, false, err
	}
	if !ok || run.ProjectID != projectID || run.WorkUnitID != workUnitID {
		return domain.Run{}, false, ErrRunNotFound
	}
	if run.Status == domain.RunClosed {
		return run, true, nil
	}
	// Establish the close/admission order before appending. An admission that
	// already won is allowed to make its reservation durable; once closing is
	// visible here, later admissions fail without writing or reaching Provider.
	finishClose := m.beginRunClose(projectID, runID)
	defer finishClose()
	eventID, err := id.New("evt")
	if err != nil {
		return domain.Run{}, false, err
	}
	now := m.localNow()
	period, err := m.periods.PeriodAt(now)
	if err != nil {
		return domain.Run{}, false, err
	}
	event := ledger.Event{
		EventID: eventID, Kind: ledger.EventRunClosed,
		ProjectID: projectID, KeyID: keyID, WorkUnitID: workUnitID, RunID: runID,
		CloseReason: reason, OccurredAt: now,
		Operation: intent.Operation, IdempotencyKeyHash: intent.IdempotencyKeyHash, RequestFingerprint: intent.RequestFingerprint,
	}
	period.Stamp(&event)
	if err := m.appendApply(ctx, event); err != nil {
		return domain.Run{}, false, err
	}
	closed, found, err := m.ReadRun(ctx, projectID, runID)
	if err != nil {
		return domain.Run{}, false, err
	}
	if !found {
		return domain.Run{}, false, errors.New("closed run is absent from accounting view")
	}
	return closed, false, nil
}

// ReadWorkUnit and ReadRun keep storage errors distinct from successful absence.
// Public list readers remain on the legacy State until their pagination migration.
// These observation reads hold owner lifetime for the entire query. Errors
// must not become zero financial or pending values.
func (m *Manager) ReadBalance(ctx context.Context, key ledger.BalanceKey) (ledger.Balance, error) {
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return ledger.Balance{}, err
	}
	defer release()
	v, err := m.reads.ReadBalance(ctx, key)
	if err != nil {
		return ledger.Balance{}, err
	}
	return v, nil
}

// ReadWorkUnitMaturityPage owns the whole finite, coherent observation.
func (m *Manager) ReadWorkUnitMaturityPage(ctx context.Context, refs []ledger.WorkUnitReference) (ledger.WorkUnitMaturityPage, error) {
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return ledger.WorkUnitMaturityPage{}, err
	}
	defer release()
	page, err := m.reads.ReadWorkUnitMaturityPage(ctx, refs)
	if err != nil {
		return ledger.WorkUnitMaturityPage{}, err
	}
	return page, nil
}

// VisitGovernanceCohort owns the full query, including visitors between pages.
// Errors invalidate every visited row; no partial accounting head may escape.
func (m *Manager) VisitGovernanceCohort(ctx context.Context, q ledger.GovernanceCohortQuery, visit func(ledger.GovernanceCohortRow) error) (ledger.Watermark, error) {
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return ledger.Watermark{}, err
	}
	defer release()
	head, err := m.reads.VisitGovernanceCohort(ctx, q, visit)
	if err != nil {
		return ledger.Watermark{}, err
	}
	return head, nil
}

func (m *Manager) ReadPendingLeaseStats(ctx context.Context, now time.Time) (ledger.PendingLeaseStatistics, error) {
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return ledger.PendingLeaseStatistics{}, err
	}
	defer release()
	v, err := m.reads.ReadPendingLeaseStats(ctx, now)
	if err != nil {
		return ledger.PendingLeaseStatistics{}, err
	}
	return v, nil
}

// ReadWorkUnitAccounting keeps lifetime admission around the entire coherent
// read. The selected owner serves the WorkUnit, activity and bounded Runs.
func (m *Manager) ReadWorkUnitAccounting(ctx context.Context, projectID, workUnitID string, includeRuns bool) (ledger.WorkUnitAccountingView, bool, error) {
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return ledger.WorkUnitAccountingView{}, false, err
	}
	defer release()
	return m.reads.ReadWorkUnitAccounting(ctx, projectID, workUnitID, includeRuns)
}

func (m *Manager) ReadWorkUnit(ctx context.Context, projectID, workUnitID string) (domain.WorkUnit, bool, error) {
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return domain.WorkUnit{}, false, err
	}
	defer release()
	workUnit, found, err := m.reads.ReadWorkUnit(ctx, workUnitID)
	if err != nil {
		return domain.WorkUnit{}, false, err
	}
	if !found || (projectID != "" && workUnit.ProjectID != projectID) {
		return domain.WorkUnit{}, false, nil
	}
	return workUnit, true, nil
}

func (m *Manager) ReadWorkUnitPage(ctx context.Context, query ledger.WorkUnitListQuery) (ledger.GovernanceListPage[domain.WorkUnit], error) {
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return ledger.GovernanceListPage[domain.WorkUnit]{}, err
	}
	defer release()
	page, err := m.reads.ReadWorkUnitPage(ctx, query)
	if err != nil {
		return ledger.GovernanceListPage[domain.WorkUnit]{}, err
	}
	return page, nil
}

func (m *Manager) ReadActiveRunCount(ctx context.Context, projectID string, at time.Time) (int64, error) {
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	capacity, err := m.reads.ReadRunCapacity(ctx, projectID, "", at)
	if err != nil {
		return 0, err
	}
	return capacity.ActiveRuns, err
}

func (m *Manager) ReadRun(ctx context.Context, projectID, runID string) (domain.Run, bool, error) {
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return domain.Run{}, false, err
	}
	defer release()
	run, found, err := m.reads.ReadRun(ctx, runID)
	if err != nil {
		return domain.Run{}, false, err
	}
	if !found || (projectID != "" && run.ProjectID != projectID) {
		return domain.Run{}, false, nil
	}
	lockedProject := run.ProjectID
	admission, unlock := m.lockProject(lockedProject)
	defer unlock()
	// Re-read under the project's money lock before combining in-flight spend.
	run, found, err = m.reads.ReadRun(ctx, runID)
	if err != nil {
		return domain.Run{}, false, err
	}
	if !found || (projectID != "" && run.ProjectID != projectID) {
		return domain.Run{}, false, nil
	}
	if run.ProjectID != lockedProject {
		return domain.Run{}, false, errors.New("accounting run project changed during read")
	}
	return domain.WithRunBudgetState(run, admission.runPending[runID]), true, nil
}

func (m *Manager) ActiveRun(ctx context.Context, projectID, runID string) (domain.Run, error) {
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return domain.Run{}, err
	}
	defer release()
	unlock := m.governanceLock(projectID)
	defer unlock()
	run, found, err := m.reads.ReadRun(ctx, runID)
	if err != nil {
		return domain.Run{}, err
	}
	if !found || run.ProjectID != projectID {
		return domain.Run{}, ErrRunNotFound
	}
	if run.Status != domain.RunActive || !m.localNow().Before(run.ExpiresAt) {
		return domain.Run{}, ErrRunNotActive
	}
	return run, nil
}

func (m *Manager) ReadRunPage(ctx context.Context, query ledger.RunListQuery) (ledger.GovernanceListPage[domain.Run], error) {
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return ledger.GovernanceListPage[domain.Run]{}, err
	}
	defer release()
	page, err := m.reads.ReadRunPage(ctx, query)
	if err != nil {
		return ledger.GovernanceListPage[domain.Run]{}, err
	}
	output := make([]domain.Run, 0, len(page.Items))
	size := 0
	for _, row := range page.Items {
		run, found, err := m.ReadRun(ctx, query.ProjectID, row.ID)
		if err != nil {
			return ledger.GovernanceListPage[domain.Run]{}, err
		}
		if !found {
			return ledger.GovernanceListPage[domain.Run]{}, errors.New("listed run is absent from accounting view")
		}
		if (query.WorkUnitID != "" && run.WorkUnitID != query.WorkUnitID) || (query.Status != "" && domain.EffectiveRunStatus(run, query.At) != query.Status) {
			continue
		}
		raw, err := json.Marshal(run)
		if err != nil {
			return ledger.GovernanceListPage[domain.Run]{}, err
		}
		if len(raw) > query.Limits.Bytes-size {
			if len(output) == 0 {
				return ledger.GovernanceListPage[domain.Run]{}, ledger.ErrProjectionPageLimit
			}
			page.NextID = output[len(output)-1].ID
			break
		}
		output = append(output, run)
		size += len(raw)
	}
	page.Items = output
	page.Bytes = size
	if err := ctx.Err(); err != nil {
		return ledger.GovernanceListPage[domain.Run]{}, err
	}
	return page, nil
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
	// Protected by the registry, including callers waiting on either lock.
	users int
	// lifecycle orders Work Unit/Run create/close against attached request
	// acceptance. Requests share the read side so their durable acceptance can
	// batch; control-plane mutations take the write side and wait until every
	// acceptance that won the order is visible in the Ledger.
	lifecycle sync.RWMutex
	// pending is spend admitted but not yet visible in the Ledger's own balance,
	// keyed by period so a request near midnight counts against its own day.
	pending map[ledger.BalanceKey]int64
	// runPending is the lifetime Run counterpart to pending. runAdmissions
	// keeps a close operation behind reservations that won the decision first;
	// closingRuns makes a close that won first reject later reservations while
	// its event is appended outside this lock.
	runPending    map[string]int64
	runAdmissions map[string]int
	closingRuns   map[string]bool
	changed       *sync.Cond
}

func newProjectAdmission() *projectAdmission {
	admission := &projectAdmission{}
	admission.changed = sync.NewCond(&admission.mu)
	return admission
}

func (m *Manager) beginRunClose(projectID, runID string) func() {
	admission, unlock := m.lockProject(projectID)
	if admission.closingRuns == nil {
		admission.closingRuns = make(map[string]bool)
	}
	admission.closingRuns[runID] = true
	for admission.runAdmissions[runID] > 0 {
		admission.changed.Wait()
	}
	unlock()
	return func() {
		admission, unlock := m.lockProject(projectID)
		delete(admission.closingRuns, runID)
		admission.changed.Broadcast()
		unlock()
	}
}

func (m *Manager) lockProject(projectID string) (*projectAdmission, func()) {
	admission := m.projectLocks.retain(projectID)
	waitStarted := time.Now()
	admission.mu.Lock()
	held := time.Now()
	m.lockWaitNanos.Add(int64(held.Sub(waitStarted)))
	m.lockAcquisitions.Add(1)
	return admission, func() {
		m.lockHeldNanos.Add(int64(time.Since(held)))
		admission.mu.Unlock()
		m.projectLocks.release(projectID, admission)
	}
}

// ProjectLockStats reports contention on the per-project accounting lock.
// Acquisitions is the denominator for both durations.
type ProjectLockStats struct {
	Acquisitions uint64
	WaitDuration time.Duration
	HeldDuration time.Duration
	LiveProjects int
}

func (m *Manager) ProjectLockStats() ProjectLockStats {
	return ProjectLockStats{
		Acquisitions: m.lockAcquisitions.Load(),
		WaitDuration: time.Duration(m.lockWaitNanos.Load()),
		HeldDuration: time.Duration(m.lockHeldNanos.Load()),
		LiveProjects: m.projectLocks.count(),
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
	return m.admitForRun(context.Background(), key, dailyBudgetMicrosUSD, reservationMicrosUSD, counted, "", "", time.Time{})
}

func (m *Manager) admitForRun(ctx context.Context, key ledger.BalanceKey, dailyBudgetMicrosUSD, reservationMicrosUSD int64, counted bool, workUnitID, runID string, now time.Time) error {
	admission, unlock := m.lockProject(key.ProjectID)
	defer unlock()
	balance, err := m.reads.ReadBalance(ctx, key)
	if err != nil {
		return err
	}
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
	if runID != "" {
		run, ok, err := m.reads.ReadRun(ctx, runID)
		if err != nil {
			return err
		}
		if !ok || run.ProjectID != key.ProjectID || run.WorkUnitID != workUnitID {
			return ErrRunNotFound
		}
		if admission.closingRuns[runID] || run.Status != domain.RunActive || !now.Before(run.ExpiresAt) {
			return ErrRunNotActive
		}
		runTotal, err := checkedAdd(run.CommittedMicrosUSD, run.ReservedMicrosUSD)
		if err != nil {
			return err
		}
		if runTotal, err = checkedAdd(runTotal, admission.runPending[runID]); err != nil {
			return err
		}
		if counted {
			if runTotal, err = checkedAdd(runTotal, reservationMicrosUSD); err != nil {
				return err
			}
		}
		if runTotal > run.BudgetMicrosUSD {
			return ErrRunExceeded
		}
	}
	if counted {
		if admission.pending == nil {
			admission.pending = make(map[ledger.BalanceKey]int64)
		}
		admission.pending[key] += reservationMicrosUSD
		if runID != "" {
			if admission.runPending == nil {
				admission.runPending = make(map[string]int64)
			}
			admission.runPending[runID] += reservationMicrosUSD
		}
	}
	if runID != "" {
		if admission.runAdmissions == nil {
			admission.runAdmissions = make(map[string]int)
		}
		admission.runAdmissions[runID]++
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
	m.releaseAdmittedForRun(key, reservationMicrosUSD, "")
}

func (m *Manager) releaseAdmittedForRun(key ledger.BalanceKey, reservationMicrosUSD int64, runID string) {
	admission, unlock := m.lockProject(key.ProjectID)
	defer unlock()
	if remaining := admission.pending[key] - reservationMicrosUSD; remaining > 0 {
		admission.pending[key] = remaining
	} else {
		// Keyed by period, so entries would otherwise accumulate one per day.
		delete(admission.pending, key)
	}
	if runID != "" {
		if remaining := admission.runPending[runID] - reservationMicrosUSD; remaining > 0 {
			admission.runPending[runID] = remaining
		} else {
			delete(admission.runPending, runID)
		}
		if remaining := admission.runAdmissions[runID] - 1; remaining > 0 {
			admission.runAdmissions[runID] = remaining
		} else {
			delete(admission.runAdmissions, runID)
		}
		admission.changed.Broadcast()
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
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return Attempt{}, err
	}
	defer release()
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
	return m.BeginRequestAttributed(ctx, projectID, keyID, requestID, requestedModel, "", "")
}

func (m *Manager) BeginRequestAttributed(
	ctx context.Context,
	projectID, keyID, requestID, requestedModel, workUnitID, runID string,
) (Request, error) {
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return Request{}, err
	}
	defer release()
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
		KeyID: keyID, RequestedModel: requestedModel, WorkUnitID: workUnitID, RunID: runID,
	}
	var unlockLifecycle func()
	if runID != "" || workUnitID != "" {
		if runID == "" || workUnitID == "" {
			return Request{}, ErrRunNotFound
		}
		unlockLifecycle = m.governanceReadLock(projectID)
		defer unlockLifecycle()
		run, ok, err := m.reads.ReadRun(ctx, runID)
		if err != nil {
			return Request{}, err
		}
		if !ok || run.ProjectID != projectID || run.WorkUnitID != workUnitID {
			return Request{}, ErrRunNotFound
		}
		if run.Status != domain.RunActive || !now.Before(run.ExpiresAt) {
			return Request{}, ErrRunNotActive
		}
	}
	event := ledger.Event{
		EventID: eventID, Kind: ledger.EventRequestAccepted,
		RequestID: request.RequestID, ProjectID: request.ProjectID,
		KeyID: request.KeyID, RequestedModel: request.RequestedModel,
		WorkUnitID: request.WorkUnitID, RunID: request.RunID,
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
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return Attempt{}, err
	}
	defer release()
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
		if request.RunID != "" {
			return Attempt{}, ErrRunPriceUnavailable
		}
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
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return Attempt{}, err
	}
	defer release()
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
		WorkUnitID: request.WorkUnitID, RunID: request.RunID,
		RouteID: metadata.RouteID, DeploymentID: metadata.DeploymentID,
		ProviderID: metadata.ProviderID, ProviderModel: metadata.ProviderModel,
		ProviderPrimitive: metadata.ProviderPrimitive,
		OfferingID:        metadata.OfferingID, ProfileID: metadata.ProfileID,
		AccountRegionID: metadata.AccountRegionID,
		AttemptNumber:   metadata.AttemptNumber, RetryCount: metadata.RetryCount,
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
	if err := m.admitForRun(ctx, key, dailyBudgetMicrosUSD, spec.ReservationMicrosUSD, counted, request.WorkUnitID, request.RunID, m.localNow()); err != nil {
		return Attempt{}, err
	}
	if counted || request.RunID != "" {
		// Released on every path out of here, so a reservation that never became
		// durable does not hold budget headroom for the rest of the period.
		defer m.releaseAdmittedForRun(key, spec.ReservationMicrosUSD, request.RunID)
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
		WorkUnitID:           request.WorkUnitID,
		RunID:                request.RunID,
		KeyID:                request.KeyID,
		RouteID:              metadata.RouteID,
		DeploymentID:         metadata.DeploymentID,
		ProviderID:           metadata.ProviderID,
		OfferingID:           metadata.OfferingID,
		ProfileID:            metadata.ProfileID,
		AccountRegionID:      metadata.AccountRegionID,
		RequestedModel:       request.RequestedModel,
		ProviderModel:        metadata.ProviderModel,
		ProviderPrimitive:    metadata.ProviderPrimitive,
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
		KeyID: a.KeyID, RequestedModel: a.RequestedModel, WorkUnitID: a.WorkUnitID, RunID: a.RunID,
	}
}

func (m *Manager) MarkStarted(ctx context.Context, attempt Attempt) error {
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return err
	}
	defer release()
	eventID, err := id.New("evt")
	if err != nil {
		return err
	}
	started := ledger.Event{
		EventID:    eventID,
		Kind:       ledger.EventAttemptStarted,
		RequestID:  attempt.RequestID,
		AttemptID:  attempt.AttemptID,
		ProjectID:  attempt.ProjectID,
		WorkUnitID: attempt.WorkUnitID, RunID: attempt.RunID,
		KeyID: attempt.KeyID, RouteID: attempt.RouteID, DeploymentID: attempt.DeploymentID, ProviderID: attempt.ProviderID,
		OfferingID: attempt.OfferingID, ProfileID: attempt.ProfileID,
		AccountRegionID: attempt.AccountRegionID,
		RequestedModel:  attempt.RequestedModel, ProviderModel: attempt.ProviderModel,
		ProviderPrimitive: attempt.ProviderPrimitive,
		AttemptNumber:     attempt.AttemptNumber, RetryCount: attempt.RetryCount,
		FallbackCount: attempt.FallbackCount,
		OccurredAt:    m.localNow(),
	}
	attempt.Period.Stamp(&started)
	return m.appendApply(ctx, started)
}

// Settle releases the reservation and commits usage and cost in one durable
// event. Callers should use a cleanup context that is independent of the client.
func (m *Manager) Settle(ctx context.Context, attempt Attempt, settlement Settlement) error {
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return err
	}
	defer release()
	eventID, err := id.New("evt")
	if err != nil {
		return err
	}
	return m.settle(ctx, eventID, attempt, settlement)
}

// unservedSettlement reports an attempt that produced nothing: no tokens, no
// cost, and an outcome that is not a success. Such an attempt owes nothing, and
// that includes the fixed per-request fee — the fee pays for a request an
// upstream actually received.
//
// The price-snapshot check below has to skip it rather than re-derive a cost.
// Re-deriving one made every deterministic failure unsettleable the moment a
// price version carried a fixed fee: the reservation stayed pending for the
// life of the process, the caller was told accounting was unavailable instead of
// what the provider said, and restart recovery then charged the whole prepared
// estimate for an attempt the gateway had already established produced nothing.
// A settlement claiming success still has to match the frozen formula, so a
// served request cannot dodge the fee by reporting no tokens.
func unservedSettlement(settlement Settlement) bool {
	return settlement.CommittedMicrosUSD == 0 &&
		settlement.ProviderInputTokens == 0 &&
		settlement.ProviderOutputTokens == 0 &&
		settlement.ProviderCachedInputTokens == 0 &&
		settlement.ProviderCacheWriteInputTokens == 0 &&
		settlement.Outcome != "success"
}

// settle deliberately does not bound CommittedMicrosUSD by the attempt's
// reservation. The reservation is an admission-time estimate; the provider
// bills for the tokens it actually served, and when it reports more billable
// output than PreparedOutputTokens estimated, the true cost is what the ledger
// must record — capping it at the reservation would silently refund real
// spend, and the price-snapshot check below would reject the capped value as
// inconsistent with the reported tokens. The settling write may therefore push
// a project past its daily ceiling; the overshoot is bounded by one attempt's
// worth of under-estimation, and admission refuses further attempts once the
// balance reflects it.
func (m *Manager) settle(ctx context.Context, eventID string, attempt Attempt, settlement Settlement) error {
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return err
	}
	defer release()
	occurredAt := settlement.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = m.localNow()
	} else {
		occurredAt = m.inAccountingZone(occurredAt)
	}
	event, err := buildSettlementEvent(eventID, attempt, settlement, occurredAt)
	if err != nil {
		return err
	}
	return m.appendApply(ctx, event)
}

// buildSettlementEvent shares frozen-price and attribution semantics between
// normal accounting and maintenance recovery. It does not read mutable clocks,
// admission limits or state, and never appends a fact.
func buildSettlementEvent(eventID string, attempt Attempt, settlement Settlement, occurredAt time.Time) (ledger.Event, error) {
	if settlement.CommittedMicrosUSD < 0 ||
		settlement.ProviderInputTokens < 0 ||
		settlement.ProviderOutputTokens < 0 ||
		settlement.ProviderCachedInputTokens < 0 ||
		settlement.ProviderCacheWriteInputTokens < 0 ||
		settlement.ProviderReasoningTokens < 0 ||
		settlement.PreparedOutputTokens < 0 {
		return ledger.Event{}, ErrInvalidAmount
	}
	// The tiers partition the input total. A sum that overruns it means a caller
	// added them on top instead of reporting a breakdown, which would let a
	// later re-rating charge the cached span twice.
	if settlement.ProviderCachedInputTokens+settlement.ProviderCacheWriteInputTokens > settlement.ProviderInputTokens {
		return ledger.Event{}, ErrInvalidAmount
	}
	var priceSnapshot *domain.PriceSnapshot
	inputCost, outputCost, fixedCost := int64(0), int64(0), int64(0)
	if attempt.PriceSnapshot != nil {
		copy := attempt.PriceSnapshot.Clone()
		priceSnapshot = &copy
		if copy.CostValueStatus == domain.CostValueKnown && !unservedSettlement(settlement) {
			cost, costErr := copy.Calculate(settlement.ProviderInputTokens, settlement.ProviderCachedInputTokens, settlement.ProviderOutputTokens)
			if costErr != nil {
				return ledger.Event{}, costErr
			}
			if cost.TotalCostMicrosUSD != settlement.CommittedMicrosUSD {
				return ledger.Event{}, fmt.Errorf("settlement cost does not match immutable price snapshot: calculated=%d committed=%d", cost.TotalCostMicrosUSD, settlement.CommittedMicrosUSD)
			}
			inputCost, outputCost, fixedCost = cost.InputCostMicrosUSD, cost.OutputCostMicrosUSD, cost.FixedCostMicrosUSD
		}
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
		WorkUnitID:         attempt.WorkUnitID,
		RunID:              attempt.RunID,
		KeyID:              attempt.KeyID,
		RouteID:            attempt.RouteID,
		DeploymentID:       attempt.DeploymentID,
		ProviderID:         attempt.ProviderID,
		OfferingID:         attempt.OfferingID,
		ProfileID:          attempt.ProfileID,
		AccountRegionID:    attempt.AccountRegionID,
		RequestedModel:     attempt.RequestedModel,
		ProviderModel:      attempt.ProviderModel,
		ProviderPrimitive:  attempt.ProviderPrimitive,
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
		ProviderCode:                  settlement.ProviderCode,
		ProviderFailureReason:         settlement.ProviderFailureReason,
		Retryable:                     settlement.Retryable,
		Ambiguous:                     settlement.Ambiguous,
		FailureSemanticsRecorded:      settlement.FailureSemanticsRecorded,
		ProviderRequestID:             settlement.ProviderRequestID,
		FailurePhase:                  settlement.FailurePhase,
		LatencyMillis:                 settlement.LatencyMillis,
	}
	attempt.Period.Stamp(&settled)
	return settled, nil
}

// RecoverPendingLeases resolves every durable lease before listeners become
// ready. Leases without AttemptStarted are released; started leases are
// conservatively settled from their prepared token bounds and frozen price.
func (m *Manager) RecoverPendingLeases(ctx context.Context) error {
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return err
	}
	defer release()
	if m.legacyPending == nil {
		return errors.New("projected accounting requires exclusive two-level request/attempt recovery")
	}
	pendingLeases := m.legacyPending()
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
		attempt, settlement, eventID, err := recoverySettlement(pending)
		if err != nil {
			return err
		}
		if err := m.settle(ctx, eventID, attempt, settlement); err != nil {
			m.recoveryMu.Lock()
			m.recovery.Failures++
			m.recoveryMu.Unlock()
			return fmt.Errorf("recover attempt %q: %w", attempt.AttemptID, err)
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

// recoverySettlement retains the v1 attempt recovery identity and frozen
// provider/price decision shared by both recovery paths.
func recoverySettlement(pending ledger.PendingLease) (Attempt, Settlement, string, error) {
	event := pending.Reservation
	reservation := int64(0)
	if event.ReservationMicrosUSD != nil {
		reservation = *event.ReservationMicrosUSD
	}
	attempt := Attempt{
		RequestID: event.RequestID, AttemptID: event.AttemptID, ProjectID: event.ProjectID,
		WorkUnitID: event.WorkUnitID, RunID: event.RunID,
		Period: PeriodFromEvent(event), ReservationMicrosUSD: reservation,
		KeyID: event.KeyID, RouteID: event.RouteID, DeploymentID: event.DeploymentID,
		ProviderID: event.ProviderID, OfferingID: event.OfferingID, ProfileID: event.ProfileID,
		AccountRegionID: event.AccountRegionID,
		RequestedModel:  event.RequestedModel, ProviderModel: event.ProviderModel,
		ProviderPrimitive: event.ProviderPrimitive,
		AttemptNumber:     event.AttemptNumber, RetryCount: event.RetryCount, FallbackCount: event.FallbackCount,
		LeaseMode: event.LeaseMode, PriceSnapshot: event.PriceSnapshot,
		PreparedInputTokens: event.PreparedInputTokens, PreparedOutputTokens: event.PreparedOutputTokens,
		RecoveryKey: event.RecoveryKey, UnknownPolicyEvidence: event.UnknownPolicyEvidence,
		TokenGuardPricingViewDigest: event.TokenGuardPricingViewDigest,
	}
	settlement := Settlement{Outcome: "recovered_not_started", FailurePhase: "accounting", OccurredAt: event.OccurredAt}
	if pending.Started {
		settlement = Settlement{
			ProviderInputTokens: event.PreparedInputTokens, ProviderOutputTokens: event.PreparedOutputTokens,
			PreparedOutputTokens: event.PreparedOutputTokens, TokenEstimated: true, CostEstimated: true,
			Outcome: "recovered_started_unknown_result", FailurePhase: "accounting", Ambiguous: true,
			OccurredAt: event.OccurredAt,
		}
		if event.PriceSnapshot != nil && event.PriceSnapshot.CostValueStatus == domain.CostValueKnown {
			// Nothing is known about how much of a recovered attempt's prompt
			// the provider served from cache, so none of it is: the whole
			// prompt is charged at the ordinary input rate, which is the
			// conservative reading of an ambiguous outcome.
			cost, err := event.PriceSnapshot.Calculate(event.PreparedInputTokens, 0, event.PreparedOutputTokens)
			if err != nil {
				return Attempt{}, Settlement{}, "", fmt.Errorf("recover attempt %q: %w", event.AttemptID, err)
			}
			settlement.CommittedMicrosUSD = cost.TotalCostMicrosUSD
		}
	}
	digest := sha256.Sum256([]byte("halro:accounting-recovery:v1\x00" + event.AttemptID + "\x00" + settlement.Outcome))
	eventID := "evt_recovery_" + hex.EncodeToString(digest[:12])
	return attempt, settlement, eventID, nil
}

func (m *Manager) FinalizeRequest(ctx context.Context, attempt Attempt, outcome string) error {
	return m.Finalize(ctx, attempt.Request(), outcome)
}

func (m *Manager) Finalize(ctx context.Context, request Request, outcome string) error {
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return err
	}
	defer release()
	eventID, err := id.New("evt")
	if err != nil {
		return err
	}
	finalized := ledger.Event{
		EventID:    eventID,
		Kind:       ledger.EventRequestFinalized,
		RequestID:  request.RequestID,
		ProjectID:  request.ProjectID,
		WorkUnitID: request.WorkUnitID,
		RunID:      request.RunID,
		KeyID:      request.KeyID, RequestedModel: request.RequestedModel,
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
	ctx = ledger.WithPhysicalAppendKind(ctx, event.Kind)
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return ledger.Record{}, err
	}
	defer release()
	// A failed apply is terminal: the state machine advances in sequence order
	// and can never get past the event it choked on. Appending anyway would
	// fsync a record that will never be applied, and leave it in the log for
	// replay to choke on again at the next start — with a longer tail of
	// unappliable records behind it every time. Refuse before the write.
	if err := m.applyFailure(); err != nil {
		return ledger.Record{}, err
	}
	var record ledger.Record
	var receipt ledger.AppendReceipt
	appendRecord := func(appendCtx context.Context) error {
		var err error
		record, receipt, err = m.log.AppendApplyRecordWithReceipt(appendCtx, event, m.applyDurableRecord)
		return err
	}
	if owner, ok := m.owner.(interface {
		withAppendWorkspace(context.Context, func(context.Context) error) error
	}); ok {
		err = owner.withAppendWorkspace(ctx, appendRecord)
	} else {
		err = appendRecord(ctx)
	}
	if err != nil {
		return ledger.Record{}, err
	}
	// The local state must observe ReservationCreated/AttemptStarted before the
	// request waits for a Replica. Waiting inside the Ledger writer would hold
	// its lock and serialize unrelated appends behind network latency.
	if err := m.log.WaitConfirmed(ctx, receipt); err != nil {
		return ledger.Record{}, err
	}
	if owner, ok := m.owner.(accountingOperationValidator); ok {
		if err := owner.validateAccountingOperation(ctx); err != nil {
			return ledger.Record{}, err
		}
	}
	return record, nil
}

// applyDurableRecord is part of the native mutation lifetime. Global sequence
// Apply/observers remain ordered, but no native writer lock is held here.
// Close wakes an unresolved predecessor wait and fails closed; a record whose
// turn is ready still applies even when an accepted caller canceled or Close
// has started. Accepted durable facts are never reported as absent.
func (m *Manager) applyDurableRecord(ctx context.Context, record ledger.Record) error {
	m.applyMu.Lock()
	var stopWake func() bool
	var wakeDone chan struct{}
	defer func() {
		m.applyMu.Unlock()
		// Stop the subscription or join its signaler outside the mutex. The
		// native mutation must also outlive this callback-owned goroutine.
		if stopWake != nil && !stopWake() {
			<-wakeDone
		}
	}()
	position := func() ledger.Watermark {
		head, err := m.owner.appliedPosition(ctx)
		if err != nil && m.applyErr == nil {
			m.applyErr = fmt.Errorf("read durable accounting apply position: %w", err)
			m.applyCond.Broadcast()
			m.log.Status().RequireRecovery()
		}
		return head
	}
	head := position()
	if m.applyErr == nil && record.Sequence > 0 && head.Sequence < record.Sequence-1 {
		wakeDone = make(chan struct{})
		stopWake = context.AfterFunc(ctx, func() {
			defer close(wakeDone)
			m.applyMu.Lock()
			m.applyCond.Broadcast()
			m.applyMu.Unlock()
		})
	}
	for m.applyErr == nil && record.Sequence > 0 && head.Sequence < record.Sequence-1 {
		if err := ctx.Err(); err != nil {
			m.applyErr = fmt.Errorf("durable accounting predecessor wait interrupted: %w", err)
			m.applyCond.Broadcast()
			break
		}
		m.applyCond.Wait()
		head = position()
	}
	if m.applyErr != nil {
		return m.applyErr
	}
	if err := m.owner.applyRecord(ctx, record); err != nil {
		m.applyErr = fmt.Errorf("apply durable accounting event: %w", err)
		m.applyCond.Broadcast()
		m.log.Status().RequireRecovery()
		return m.applyErr
	}
	if err := resourcebudget.RetireRAMWorkspace(ctx); err != nil {
		m.applyErr = fmt.Errorf("retire durable accounting workspace: %w", err)
		m.applyCond.Broadcast()
		m.log.Status().RequireRecovery()
		return m.applyErr
	}
	m.observerMu.RLock()
	observers := slices.Clone(m.observers)
	m.observerMu.RUnlock()
	for _, observer := range observers {
		observer(record)
	}
	m.applyCond.Broadcast()
	return nil
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

// WithGovernanceExport retains the selected accounting owner and its operation
// ticket through bounded reads and external publication. There is no State
// fallback: the legacy State has no immutable as-of lifecycle positions.
func (m *Manager) WithGovernanceExport(ctx context.Context, visit func(ledger.GovernanceExportView) error) error {
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return err
	}
	defer release()
	reader, ok := m.reads.(ledger.AccountingExportReader)
	if !ok {
		return ledger.ErrGovernanceExportBackend
	}
	return reader.WithGovernanceExport(ctx, visit)
}

// Transitional State export only: the full accounting snapshot still grows
// with history. Select the SAME reader used by this Manager and hold its ticket
// through external publication; never borrow Runtime's potentially old alias.
// A projected owner cannot silently use this compatibility path.
func (m *Manager) WithLegacyGovernanceSnapshot(ctx context.Context, visit func(context.Context, ledger.GovernanceSnapshot) error) error {
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return err
	}
	defer release()
	state, ok := m.reads.(stateAccountingOwner)
	if !ok || state.State == nil {
		return ledger.ErrGovernanceExportBackend
	}
	if visit == nil {
		return ledger.ErrGovernanceExportIncomplete
	}
	snapshot, err := state.GovernanceSnapshot(ctx, ledger.GovernanceSnapshotOptions{IncludeRuns: true})
	if err != nil {
		return err
	}
	return visit(ctx, snapshot)
}
