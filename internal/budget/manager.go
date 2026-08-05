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
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/id"
	"github.com/akz142857/Heimdall/internal/ledger"
)

var (
	ErrExceeded                    = errors.New("daily budget exceeded")
	ErrInvalidAmount               = errors.New("invalid accounting amount")
	ErrAdjustmentHardLimitExceeded = errors.New("adjustment exceeds rolling online hard limit")
)

type Attempt struct {
	RequestID                   string
	AttemptID                   string
	ProjectID                   string
	PeriodID                    string
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
	RequestID      string
	ProjectID      string
	PeriodID       string
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
	PreparedOutputTokens int64
	CostEstimated        bool
	TokenEstimated       bool
	Outcome              string
	ErrorClass           string
	HTTPStatus           int
	LatencyMillis        int64
	// OccurredAt is reserved for deterministic recovery events. Normal
	// callers leave it zero and the manager captures its clock at commit.
	OccurredAt time.Time
}

type AdjustmentSpec struct {
	AttemptID                string
	Mode                     ledger.AdjustmentMode
	ExplicitDeltaMicrosUSD   int64
	CorrectionPriceSnapshot  *domain.PriceSnapshot
	ExpectedSequence         uint64
	ExpectedNetCostMicrosUSD int64
	IdempotencyKeyDigest     string
	RequestDigest            string
	ReasonCode               string
	Reason                   string
	EvidenceDigest           string
	CreatedBy                string
}

type AdjustmentPreview struct {
	Event                  ledger.Event `json:"event"`
	BudgetOverageMicrosUSD int64        `json:"budget_overage_micros_usd"`
	SoftLimitExceeded      bool         `json:"soft_limit_exceeded"`
	HardLimitExceeded      bool         `json:"hard_limit_exceeded"`
}

type PersistAdjustmentIntent func(ledger.Event) error

type Manager struct {
	projectLocks sync.Map
	applyMu      sync.Mutex
	applyCond    *sync.Cond
	applyErr     error
	observerMu   sync.RWMutex
	log          *ledger.Log
	state        *ledger.State
	location     *time.Location
	now          func() time.Time
	observers    []func(ledger.Record)
	recoveryMu   sync.RWMutex
	recovery     RecoveryStats
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

func New(log *ledger.Log, state *ledger.State, location *time.Location) (*Manager, error) {
	return NewWithOptions(log, state, location, Options{})
}

func NewWithOptions(
	log *ledger.Log,
	state *ledger.State,
	location *time.Location,
	options Options,
) (*Manager, error) {
	if log == nil || state == nil || location == nil {
		return nil, errors.New("ledger log, state, and location are required")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	manager := &Manager{
		log:      log,
		state:    state,
		location: location,
		now:      options.Now,
	}
	manager.applyCond = sync.NewCond(&manager.applyMu)
	return manager, nil
}

func (m *Manager) lockProject(projectID string) func() {
	value, _ := m.projectLocks.LoadOrStore(projectID, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

func (m *Manager) PreviewAdjustment(spec AdjustmentSpec, postedAt time.Time, dailyBudgetMicrosUSD int64) (AdjustmentPreview, error) {
	settled, ok := m.state.SettledAttempt(spec.AttemptID)
	if !ok {
		return AdjustmentPreview{}, errors.New("settled attempt not found")
	}
	if !settled.CostKnown && spec.Mode != ledger.AdjustmentModeReprice {
		return AdjustmentPreview{}, errors.New("unknown original cost requires a correction price snapshot")
	}
	if spec.ExpectedSequence != settled.AdjustmentSequence || settled.CostKnown && spec.ExpectedNetCostMicrosUSD != settled.NetCostMicrosUSD {
		return AdjustmentPreview{}, errors.New("adjustment sequence or expected net cost conflicts")
	}
	if !domain.ValidSHA256Label(spec.IdempotencyKeyDigest) || !domain.ValidSHA256Label(spec.RequestDigest) || !domain.ValidSHA256Label(spec.EvidenceDigest) || spec.CreatedBy == "" || spec.ReasonCode == "" || len(spec.Reason) > 1024 {
		return AdjustmentPreview{}, errors.New("adjustment evidence is incomplete")
	}
	postedAt = postedAt.In(m.location)
	if postedAt.IsZero() {
		return AdjustmentPreview{}, errors.New("adjustment posting time is required")
	}
	base, before := settled.BaseCostMicrosUSD, settled.NetCostMicrosUSD
	after, delta := int64(0), spec.ExplicitDeltaMicrosUSD
	inputCost, outputCost, fixedCost := int64(0), int64(0), int64(0)
	switch spec.Mode {
	case ledger.AdjustmentModeReprice:
		if spec.CorrectionPriceSnapshot == nil {
			return AdjustmentPreview{}, errors.New("correction price snapshot is required")
		}
		breakdown, err := spec.CorrectionPriceSnapshot.Calculate(settled.Settlement.ProviderInputTokens, settled.Settlement.ProviderOutputTokens)
		if err != nil {
			return AdjustmentPreview{}, err
		}
		after, inputCost, outputCost, fixedCost = breakdown.TotalCostMicrosUSD, breakdown.InputCostMicrosUSD, breakdown.OutputCostMicrosUSD, breakdown.FixedCostMicrosUSD
		if settled.CostKnown {
			delta = after - before
		} else {
			delta = after
		}
	case ledger.AdjustmentModeExplicit:
		var err error
		after, err = checkedSignedAdd(before, delta)
		if err != nil {
			return AdjustmentPreview{}, err
		}
	default:
		return AdjustmentPreview{}, errors.New("invalid adjustment mode")
	}
	if after < 0 {
		return AdjustmentPreview{}, errors.New("adjustment cannot make attempt cost negative")
	}
	eventID := "evt_adj_" + spec.IdempotencyKeyDigest[len("sha256:"):len("sha256:")+24]
	event := ledger.Event{
		EventID: eventID, Kind: ledger.EventCostAdjusted, RequestID: settled.Settlement.RequestID, AttemptID: spec.AttemptID,
		ProjectID: settled.Settlement.ProjectID, KeyID: settled.Settlement.KeyID, RouteID: settled.Settlement.RouteID, DeploymentID: settled.Settlement.DeploymentID,
		ProviderID: settled.Settlement.ProviderID, RequestedModel: settled.Settlement.RequestedModel, ProviderModel: settled.Settlement.ProviderModel,
		PeriodID: settled.Settlement.PeriodID, OccurredAt: postedAt, OriginalSettlementEventID: settled.Settlement.EventID, OriginalSettlementDigest: settled.SettlementDigest,
		AdjustmentMode: spec.Mode, AdjustmentSequence: settled.AdjustmentSequence + 1, IdempotencyKeyDigest: spec.IdempotencyKeyDigest, AdjustmentRequestDigest: spec.RequestDigest,
		BaseSettlementMicrosUSD: func() *int64 {
			if settled.CostKnown {
				return ledger.MicrosUSD(base)
			}
			return nil
		}(),
		NetCostBeforeMicrosUSD: func() *int64 {
			if settled.CostKnown {
				return ledger.MicrosUSD(before)
			}
			return nil
		}(), AdjustmentDeltaMicrosUSD: delta, NetCostAfterMicrosUSD: ledger.MicrosUSD(after),
		ServicePeriodID: settled.Settlement.PeriodID, OriginalCompletedAt: settled.Settlement.OccurredAt, PostedPeriodID: postedAt.Format("2006-01-02"), PostedAt: postedAt,
		CorrectionPriceSnapshot: spec.CorrectionPriceSnapshot, AdjustmentInputCostMicrosUSD: inputCost, AdjustmentOutputCostMicrosUSD: outputCost, AdjustmentFixedCostMicrosUSD: fixedCost,
		AdjustmentReasonCode: spec.ReasonCode, AdjustmentReason: spec.Reason, AdjustmentEvidenceDigest: spec.EvidenceDigest, AdjustmentCreatedBy: spec.CreatedBy,
	}
	if spec.CorrectionPriceSnapshot != nil {
		snapshot := spec.CorrectionPriceSnapshot.Clone()
		event.CorrectionPriceSnapshot = &snapshot
	}
	if err := event.Validate(); err != nil {
		return AdjustmentPreview{}, err
	}
	overage := int64(0)
	if dailyBudgetMicrosUSD > 0 && event.ServicePeriodID == postedAt.Format("2006-01-02") {
		balance := m.state.Balance(event.ProjectID, event.ServicePeriodID)
		current, err := checkedSignedAdd(balance.CommittedMicrosUSD, balance.ReservedMicrosUSD)
		if err != nil {
			return AdjustmentPreview{}, err
		}
		adjusted, err := checkedSignedAdd(current, delta)
		if err != nil {
			return AdjustmentPreview{}, err
		}
		if adjusted > dailyBudgetMicrosUSD {
			overage = adjusted - dailyBudgetMicrosUSD
		}
	}
	return AdjustmentPreview{Event: event, BudgetOverageMicrosUSD: overage}, nil
}

// CommitAdjustmentWithIntent serializes the sequence check, rolling hard-limit
// check, durable intent write and Ledger append under the project lock.
func (m *Manager) CommitAdjustmentWithIntent(ctx context.Context, spec AdjustmentSpec, dailyBudgetMicrosUSD, rollingHardLimitMicrosUSD int64, persist PersistAdjustmentIntent) (AdjustmentPreview, bool, error) {
	settled, ok := m.state.SettledAttempt(spec.AttemptID)
	if !ok {
		return AdjustmentPreview{}, false, errors.New("settled attempt not found")
	}
	defer m.lockProject(settled.Settlement.ProjectID)()
	if prior, ok := m.state.AdjustmentByIdempotencyDigest(spec.IdempotencyKeyDigest); ok {
		if prior.RequestDigest != spec.RequestDigest {
			return AdjustmentPreview{}, false, errors.New("adjustment idempotency key conflict")
		}
		return AdjustmentPreview{Event: prior.Event}, true, nil
	}
	preview, err := m.PreviewAdjustment(spec, m.now().In(m.location), dailyBudgetMicrosUSD)
	if err != nil {
		return AdjustmentPreview{}, false, err
	}
	magnitude := preview.Event.AdjustmentDeltaMicrosUSD
	if magnitude == math.MinInt64 {
		return AdjustmentPreview{}, false, errors.New("adjustment magnitude overflows int64")
	}
	if magnitude < 0 {
		magnitude = -magnitude
	}
	if rollingHardLimitMicrosUSD > 0 {
		posted, err := m.state.PostedAdjustmentAbsoluteSince(preview.Event.ProjectID, m.now().Add(-24*time.Hour))
		if err != nil {
			return AdjustmentPreview{}, false, err
		}
		if magnitude > rollingHardLimitMicrosUSD || posted > rollingHardLimitMicrosUSD-magnitude {
			return AdjustmentPreview{}, false, ErrAdjustmentHardLimitExceeded
		}
	}
	if persist == nil {
		return AdjustmentPreview{}, false, errors.New("durable adjustment intent writer is required")
	}
	if err := persist(preview.Event); err != nil {
		return AdjustmentPreview{}, false, err
	}
	if err := m.appendApply(ctx, preview.Event); err != nil {
		return AdjustmentPreview{}, false, err
	}
	return preview, false, nil
}

func (m *Manager) AdjustCost(ctx context.Context, spec AdjustmentSpec, dailyBudgetMicrosUSD int64) (ledger.Event, bool, error) {
	if prior, ok := m.state.AdjustmentByIdempotencyDigest(spec.IdempotencyKeyDigest); ok {
		if prior.RequestDigest != spec.RequestDigest {
			return ledger.Event{}, false, errors.New("adjustment idempotency key conflict")
		}
		return prior.Event, true, nil
	}
	settled, ok := m.state.SettledAttempt(spec.AttemptID)
	if !ok {
		return ledger.Event{}, false, errors.New("settled attempt not found")
	}
	defer m.lockProject(settled.Settlement.ProjectID)()
	if prior, ok := m.state.AdjustmentByIdempotencyDigest(spec.IdempotencyKeyDigest); ok {
		if prior.RequestDigest != spec.RequestDigest {
			return ledger.Event{}, false, errors.New("adjustment idempotency key conflict")
		}
		return prior.Event, true, nil
	}
	preview, err := m.PreviewAdjustment(spec, m.now(), dailyBudgetMicrosUSD)
	if err != nil {
		return ledger.Event{}, false, err
	}
	if err := m.appendApply(ctx, preview.Event); err != nil {
		return ledger.Event{}, false, err
	}
	return preview.Event, false, nil
}

// CommitPreparedAdjustment is the recovery-safe half of the durable Admin
// intent protocol. The caller must persist the exact event before calling it.
func (m *Manager) CommitPreparedAdjustment(ctx context.Context, event ledger.Event) (bool, error) {
	if err := event.Validate(); err != nil || event.Kind != ledger.EventCostAdjusted {
		if err != nil {
			return false, err
		}
		return false, errors.New("prepared event is not a cost adjustment")
	}
	defer m.lockProject(event.ProjectID)()
	if prior, ok := m.state.AdjustmentByIdempotencyDigest(event.IdempotencyKeyDigest); ok {
		left, _ := json.Marshal(prior.Event)
		right, _ := json.Marshal(event)
		if string(left) != string(right) {
			return false, errors.New("prepared adjustment conflicts with authoritative Ledger")
		}
		return true, nil
	}
	if err := m.appendApply(ctx, event); err != nil {
		return false, err
	}
	return false, nil
}

func checkedSignedAdd(left, right int64) (int64, error) {
	if right > 0 && left > math.MaxInt64-right {
		return 0, errors.New("accounting integer overflow")
	}
	if right < 0 && left < math.MinInt64-right {
		return 0, errors.New("accounting integer underflow")
	}
	return left + right, nil
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
	now := m.now().In(m.location)
	request := Request{
		RequestID: requestID, ProjectID: projectID, PeriodID: now.Format("2006-01-02"),
		KeyID: keyID, RequestedModel: requestedModel,
	}
	defer m.lockProject(projectID)()
	if err := m.appendApply(ctx, ledger.Event{
		EventID: eventID, Kind: ledger.EventRequestAccepted,
		RequestID: request.RequestID, ProjectID: request.ProjectID,
		KeyID: request.KeyID, RequestedModel: request.RequestedModel,
		PeriodID: request.PeriodID, OccurredAt: now,
	}); err != nil {
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
	if request.RequestID == "" || request.ProjectID == "" || request.PeriodID == "" {
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
		PeriodID: request.PeriodID, ReservationMicrosUSD: spec.ReservationMicrosUSD,
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
	defer m.lockProject(request.ProjectID)()
	balance := m.state.Balance(request.ProjectID, request.PeriodID)
	total, err := checkedAdd(balance.CommittedMicrosUSD, balance.ReservedMicrosUSD)
	if err != nil {
		return Attempt{}, err
	}
	if spec.Mode != ledger.LeaseModeUnknownAllowed {
		total, err = checkedAdd(total, spec.ReservationMicrosUSD)
	}
	if err != nil {
		return Attempt{}, err
	}
	if dailyBudgetMicrosUSD > 0 && total > dailyBudgetMicrosUSD {
		return Attempt{}, ErrExceeded
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
	record, err := m.appendApplyRecord(ctx, ledger.Event{
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
		PeriodID:             request.PeriodID,
		OccurredAt:           m.now().In(m.location),
		ReservationMicrosUSD: reservationValue,
		LeaseMode:            spec.Mode, PriceSnapshot: eventPriceSnapshot,
		PreparedInputTokens: spec.PreparedInputTokens, PreparedOutputTokens: spec.PreparedOutputTokens,
		RecoveryKey: spec.RecoveryKey, UnknownPolicyEvidence: spec.UnknownPolicyEvidence,
		TokenGuardPricingViewDigest: spec.TokenGuardPricingViewDigest,
	})
	if err != nil {
		return Attempt{}, err
	}
	attempt.ReservationSequence = record.Sequence
	return attempt, nil
}

func (a Attempt) Request() Request {
	return Request{
		RequestID: a.RequestID, ProjectID: a.ProjectID, PeriodID: a.PeriodID,
		KeyID: a.KeyID, RequestedModel: a.RequestedModel,
	}
}

func (m *Manager) MarkStarted(ctx context.Context, attempt Attempt) error {
	eventID, err := id.New("evt")
	if err != nil {
		return err
	}
	defer m.lockProject(attempt.ProjectID)()
	return m.appendApply(ctx, ledger.Event{
		EventID:   eventID,
		Kind:      ledger.EventAttemptStarted,
		RequestID: attempt.RequestID,
		AttemptID: attempt.AttemptID,
		ProjectID: attempt.ProjectID,
		KeyID:     attempt.KeyID, RouteID: attempt.RouteID, DeploymentID: attempt.DeploymentID, ProviderID: attempt.ProviderID,
		RequestedModel: attempt.RequestedModel, ProviderModel: attempt.ProviderModel,
		AttemptNumber: attempt.AttemptNumber, RetryCount: attempt.RetryCount,
		FallbackCount: attempt.FallbackCount,
		PeriodID:      attempt.PeriodID,
		OccurredAt:    m.now().In(m.location),
	})
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
		settlement.PreparedOutputTokens < 0 {
		return ErrInvalidAmount
	}
	var priceSnapshot *domain.PriceSnapshot
	inputCost, outputCost, fixedCost := int64(0), int64(0), int64(0)
	if attempt.PriceSnapshot != nil {
		copy := attempt.PriceSnapshot.Clone()
		priceSnapshot = &copy
		if copy.CostValueStatus == domain.CostValueKnown && settlement.Outcome != "recovered_not_started" {
			cost, costErr := copy.Calculate(settlement.ProviderInputTokens, settlement.ProviderOutputTokens)
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
		occurredAt = m.now().In(m.location)
	} else {
		occurredAt = occurredAt.In(m.location)
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
	defer m.lockProject(attempt.ProjectID)()
	return m.appendApply(ctx, ledger.Event{
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
		PeriodID:           attempt.PeriodID,
		OccurredAt:         occurredAt,
		CommittedMicrosUSD: committedValue,
		LeaseMode:          attempt.LeaseMode, PriceSnapshot: priceSnapshot,
		InputCostMicrosUSD: inputCost, OutputCostMicrosUSD: outputCost, FixedCostMicrosUSD: fixedCost,
		ProviderInputTokens:  settlement.ProviderInputTokens,
		ProviderOutputTokens: settlement.ProviderOutputTokens,
		PreparedOutputTokens: settlement.PreparedOutputTokens,
		CostEstimated:        settlement.CostEstimated,
		TokenEstimated:       settlement.TokenEstimated,
		TokenUsageSource:     tokenSource,
		Outcome:              settlement.Outcome,
		ErrorClass:           settlement.ErrorClass,
		HTTPStatus:           settlement.HTTPStatus,
		LatencyMillis:        settlement.LatencyMillis,
	})
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
			PeriodID: event.PeriodID, ReservationMicrosUSD: reservation,
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
				cost, err := event.PriceSnapshot.Calculate(event.PreparedInputTokens, event.PreparedOutputTokens)
				if err != nil {
					return fmt.Errorf("recover attempt %q: %w", event.AttemptID, err)
				}
				settlement.CommittedMicrosUSD = cost.TotalCostMicrosUSD
			}
		}
		digest := sha256.Sum256([]byte("heimdall:accounting-recovery:v1\x00" + event.AttemptID + "\x00" + settlement.Outcome))
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
	defer m.lockProject(request.ProjectID)()
	return m.appendApply(ctx, ledger.Event{
		EventID:   eventID,
		Kind:      ledger.EventRequestFinalized,
		RequestID: request.RequestID,
		ProjectID: request.ProjectID,
		KeyID:     request.KeyID, RequestedModel: request.RequestedModel,
		PeriodID:   request.PeriodID,
		OccurredAt: m.now().In(m.location),
		Outcome:    outcome,
	})
}

func (m *Manager) appendApply(ctx context.Context, event ledger.Event) error {
	_, err := m.appendApplyRecord(ctx, event)
	return err
}

func (m *Manager) appendApplyRecord(ctx context.Context, event ledger.Event) (ledger.Record, error) {
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

func EstimateCostMicros(inputTokens, outputTokens, inputMicrosPerMillion, outputMicrosPerMillion int64) (int64, error) {
	if inputTokens < 0 || outputTokens < 0 || inputMicrosPerMillion < 0 || outputMicrosPerMillion < 0 {
		return 0, ErrInvalidAmount
	}
	inputCost, err := multiplyDivideCeil(inputTokens, inputMicrosPerMillion, 1_000_000)
	if err != nil {
		return 0, err
	}
	outputCost, err := multiplyDivideCeil(outputTokens, outputMicrosPerMillion, 1_000_000)
	if err != nil {
		return 0, err
	}
	return checkedAdd(inputCost, outputCost)
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
