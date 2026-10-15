package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
)

type EventKind uint8

type LeaseMode string

type TokenUsageSource string

const (
	LeaseModeMetered         LeaseMode        = "metered"
	LeaseModeFree            LeaseMode        = "free"
	LeaseModeUnknownAllowed  LeaseMode        = "unknown_allowed"
	TokenUsageSourceProvider TokenUsageSource = "provider_reported"
	TokenUsageSourceEstimate TokenUsageSource = "gateway_estimated"
	TokenUsageSourceNone     TokenUsageSource = "none"
)

func MicrosUSD(value int64) *int64 { return &value }

const (
	EventRequestAccepted EventKind = iota + 1
	EventReservationCreated
	EventAttemptStarted
	EventAttemptSettled
	EventRequestFinalized
	EventWorkUnitCreated
	EventWorkUnitClosed
	EventRunCreated
	EventRunClosed
)

func (k EventKind) Valid() bool {
	return k >= EventRequestAccepted && k <= EventRunClosed
}

type Event struct {
	EventID                     string                             `json:"event_id"`
	Kind                        EventKind                          `json:"kind"`
	RequestID                   string                             `json:"request_id"`
	WorkUnitID                  string                             `json:"work_unit_id,omitempty"`
	RunID                       string                             `json:"run_id,omitempty"`
	RunBudgetMicrosUSD          int64                              `json:"run_budget_micros_usd,omitempty"`
	RunExpiresAt                time.Time                          `json:"run_expires_at,omitzero"`
	OutcomeDefinitions          []domain.OutcomeDefinitionRef      `json:"outcome_definitions,omitempty"`
	CloseReason                 string                             `json:"close_reason,omitempty"`
	Operation                   string                             `json:"operation,omitempty"`
	IdempotencyKeyHash          string                             `json:"idempotency_key_hash,omitempty"`
	RequestFingerprint          string                             `json:"request_fingerprint,omitempty"`
	AttemptID                   string                             `json:"attempt_id,omitempty"`
	ProjectID                   string                             `json:"project_id"`
	KeyID                       string                             `json:"key_id,omitempty"`
	RouteID                     string                             `json:"route_id,omitempty"`
	DeploymentID                string                             `json:"deployment_id,omitempty"`
	ProviderID                  string                             `json:"provider_id,omitempty"`
	OfferingID                  domain.ProviderOfferingID          `json:"offering_id,omitempty"`
	ProfileID                   domain.ProviderProfileID           `json:"profile_id,omitempty"`
	AccountRegionID             domain.ProviderRegionID            `json:"account_region_id,omitempty"`
	RequestedModel              string                             `json:"requested_model,omitempty"`
	ProviderModel               string                             `json:"provider_model,omitempty"`
	ProviderPrimitive           provider.Primitive                 `json:"provider_primitive,omitempty"`
	AttemptNumber               int                                `json:"attempt_number,omitempty"`
	RetryCount                  int                                `json:"retry_count,omitempty"`
	FallbackCount               int                                `json:"fallback_count,omitempty"`
	PeriodID                    string                             `json:"period_id"`
	PeriodTimezone              string                             `json:"period_timezone,omitempty"`
	PeriodTimezoneVersion       uint64                             `json:"period_timezone_version,omitempty"`
	PeriodStartMicros           int64                              `json:"period_start_micros,omitempty"`
	PeriodEndMicros             int64                              `json:"period_end_micros,omitempty"`
	OccurredAt                  time.Time                          `json:"occurred_at"`
	ReservationMicrosUSD        *int64                             `json:"reservation_micros_usd"`
	CommittedMicrosUSD          *int64                             `json:"committed_micros_usd"`
	LeaseMode                   LeaseMode                          `json:"lease_mode,omitempty"`
	PriceSnapshot               *domain.PriceSnapshot              `json:"price_snapshot,omitempty"`
	PreparedInputTokens         int64                              `json:"prepared_input_tokens,omitempty"`
	PreparedOutputTokens        int64                              `json:"prepared_output_tokens,omitempty"`
	InputCostMicrosUSD          int64                              `json:"input_cost_micros_usd,omitempty"`
	OutputCostMicrosUSD         int64                              `json:"output_cost_micros_usd,omitempty"`
	FixedCostMicrosUSD          int64                              `json:"fixed_cost_micros_usd,omitempty"`
	RecoveryKey                 string                             `json:"recovery_key,omitempty"`
	UnknownPolicyEvidence       *domain.UnknownPricePolicyEvidence `json:"unknown_policy_evidence,omitempty"`
	TokenGuardPricingViewDigest string                             `json:"token_guard_pricing_view_digest,omitempty"`
	ProviderInputTokens         int64                              `json:"provider_input_tokens,omitempty"`
	ProviderOutputTokens        int64                              `json:"provider_output_tokens,omitempty"`
	// Cache tiers partition ProviderInputTokens; reasoning partitions
	// ProviderOutputTokens. They are recorded rather than folded into the totals
	// so a future pricing version can re-rate a span from the WAL alone, and so
	// an operator can tell an expensive request from a cache-served one. Events
	// written before this field existed decode to zero, which reads correctly as
	// "no cache tier reported".
	ProviderCachedInputTokens     int64            `json:"provider_cached_input_tokens,omitempty"`
	ProviderCacheWriteInputTokens int64            `json:"provider_cache_write_input_tokens,omitempty"`
	ProviderReasoningTokens       int64            `json:"provider_reasoning_tokens,omitempty"`
	CostEstimated                 bool             `json:"cost_estimated,omitempty"`
	TokenEstimated                bool             `json:"token_estimated,omitempty"`
	TokenUsageSource              TokenUsageSource `json:"token_usage_source,omitempty"`
	Outcome                       string           `json:"outcome,omitempty"`
	ErrorClass                    string           `json:"error_class,omitempty"`
	HTTPStatus                    int              `json:"http_status,omitempty"`
	// What an operator takes to the upstream's support desk, and where along
	// the request the failure happened. The class and the status say what kind
	// of failure it was; these say which one, in the upstream's own vocabulary,
	// and they are the only fields here an upstream chose — so they are bounded
	// and character-set narrowed by provider.SafeProviderIdentifier before they
	// reach this struct, and bounded again on the way into the WAL. A provider
	// response body must not become durable state.
	//
	// Absent on every event written before they existed, which reads correctly
	// as "not recorded" rather than as "the upstream named none". The console
	// says so in those words rather than showing an invented "unknown".
	ProviderCode             string                 `json:"provider_code,omitempty"`
	ProviderFailureReason    provider.FailureReason `json:"provider_failure_reason,omitempty"`
	ProviderRequestID        string                 `json:"provider_request_id,omitempty"`
	FailurePhase             string                 `json:"failure_phase,omitempty"`
	Retryable                bool                   `json:"retryable,omitempty"`
	Ambiguous                bool                   `json:"ambiguous,omitempty"`
	FailureSemanticsRecorded bool                   `json:"failure_semantics_recorded,omitempty"`
	LatencyMillis            int64                  `json:"latency_millis,omitempty"`
}

func (e Event) Validate() error {
	var problems []error
	if e.EventID == "" {
		problems = append(problems, errors.New("event id is required"))
	}
	if !e.Kind.Valid() {
		problems = append(problems, errors.New("event kind is invalid"))
	}
	if e.Kind <= EventRequestFinalized && e.RequestID == "" {
		problems = append(problems, errors.New("request id is required"))
	}
	if e.ProjectID == "" {
		problems = append(problems, errors.New("project id is required"))
	}
	if e.PeriodID == "" {
		problems = append(problems, errors.New("period id is required"))
	}
	if e.OccurredAt.IsZero() {
		problems = append(problems, errors.New("occurred_at is required"))
	}
	if e.Kind <= EventRequestFinalized {
		if (e.WorkUnitID == "") != (e.RunID == "") {
			problems = append(problems, errors.New("request attribution requires both work unit and run ids"))
		}
	} else {
		if e.WorkUnitID == "" {
			problems = append(problems, errors.New("work unit id is required for lifecycle events"))
		}
		switch e.Kind {
		case EventWorkUnitCreated, EventWorkUnitClosed:
			if e.RunID != "" || e.RunBudgetMicrosUSD != 0 || !e.RunExpiresAt.IsZero() {
				problems = append(problems, errors.New("work unit event cannot carry run fields"))
			}
			if e.Kind == EventWorkUnitClosed && len(e.OutcomeDefinitions) != 0 {
				problems = append(problems, errors.New("work unit close cannot carry outcome definitions"))
			}
		case EventRunCreated:
			if e.RunID == "" || e.RunBudgetMicrosUSD <= 0 || e.RunExpiresAt.IsZero() || !e.RunExpiresAt.After(e.OccurredAt) {
				problems = append(problems, errors.New("run creation fields are invalid"))
			}
		case EventRunClosed:
			if e.RunID == "" || strings.TrimSpace(e.CloseReason) == "" || len(e.CloseReason) > 64 {
				problems = append(problems, errors.New("run close fields are invalid"))
			}
		}
		if e.Operation == "" || !domain.ValidSHA256Label(e.IdempotencyKeyHash) || !domain.ValidSHA256Label(e.RequestFingerprint) {
			problems = append(problems, errors.New("lifecycle event requires operation and idempotency evidence"))
		}
	}
	if (e.ReservationMicrosUSD != nil && *e.ReservationMicrosUSD < 0) || (e.CommittedMicrosUSD != nil && *e.CommittedMicrosUSD < 0) {
		problems = append(problems, errors.New("amounts cannot be negative"))
	}
	if e.ProviderInputTokens < 0 || e.ProviderOutputTokens < 0 || e.PreparedOutputTokens < 0 || e.PreparedInputTokens < 0 ||
		e.InputCostMicrosUSD < 0 || e.OutputCostMicrosUSD < 0 || e.FixedCostMicrosUSD < 0 {
		problems = append(problems, errors.New("tokens cannot be negative"))
	}
	if e.AttemptNumber < 0 || e.RetryCount < 0 || e.FallbackCount < 0 ||
		e.HTTPStatus < 0 || e.LatencyMillis < 0 {
		problems = append(problems, errors.New("attempt counters, HTTP status, and latency cannot be negative"))
	}
	// The bound is enforced again here, at the durable boundary, rather than
	// trusted from the gateway that already narrowed it. The gateway is one
	// caller; the ledger is the record, and an event assembled by a recovery
	// path or a caller added later must not be able to widen what becomes
	// permanent. Rejected rather than truncated: a shortened identifier is a
	// different identifier, and one that would be quoted to an upstream that
	// never issued it.
	// Narrowed, not merely measured. The comment above claims the bound is
	// enforced again here so a caller added later cannot widen what becomes
	// permanent, and a length check alone did not deliver that: 128 bytes of
	// arbitrary text — spaces, newlines, a sentence out of a response body —
	// passed. SafeProviderIdentifier returns the value unchanged when it is
	// already an identifier, so comparing against it rejects exactly the values
	// the gateway would have dropped.
	if e.ProviderCode != provider.SafeProviderIdentifier(e.ProviderCode) ||
		e.ProviderRequestID != provider.SafeProviderIdentifier(e.ProviderRequestID) {
		problems = append(problems, errors.New("provider identifiers must be bounded identifiers"))
	}
	if !e.ProviderFailureReason.Valid() {
		problems = append(problems, errors.New("provider failure reason is invalid"))
	}
	if e.OfferingID != "" || e.ProfileID != "" {
		identity, ok := domain.IdentityForProfile(e.ProfileID)
		if !ok || e.OfferingID == "" || identity.Offering != e.OfferingID {
			problems = append(problems, errors.New("provider offering and profile attribution must match"))
		} else if e.AccountRegionID != domain.RegionNone &&
			!domain.AccountRegionBelongsToSurface(identity.Surface, e.AccountRegionID) {
			// Empty remains readable for records written before account-region
			// snapshots existed. Every non-empty value must still be one this
			// surface can produce; otherwise an arbitrary string becomes a durable
			// reporting dimension.
			problems = append(problems, errors.New("provider account region does not belong to the profile surface"))
		}
	} else if e.AccountRegionID != domain.RegionNone {
		problems = append(problems, errors.New("provider account region requires provider profile attribution"))
	}
	if e.PriceSnapshot != nil {
		if err := e.PriceSnapshot.Validate(); err != nil {
			problems = append(problems, err)
		}
	}
	switch e.Kind {
	case EventReservationCreated, EventAttemptStarted, EventAttemptSettled:
		if e.AttemptID == "" {
			problems = append(problems, errors.New("attempt id is required for attempt events"))
		}
	}
	if e.Kind == EventReservationCreated {
		amount := int64(0)
		if e.ReservationMicrosUSD != nil {
			amount = *e.ReservationMicrosUSD
		}
		switch e.LeaseMode {
		case "": // Legacy WAL compatibility: reservations before schema v2 were always metered.
			if e.ReservationMicrosUSD == nil || amount <= 0 {
				problems = append(problems, errors.New("legacy reservation amount must be positive"))
			}
		case LeaseModeMetered:
			if e.ReservationMicrosUSD == nil || amount <= 0 || e.PriceSnapshot == nil || e.PriceSnapshot.BillingMode != domain.BillingModeMetered {
				problems = append(problems, errors.New("metered lease requires a positive reservation and metered price snapshot"))
			}
		case LeaseModeFree:
			if e.ReservationMicrosUSD == nil || amount != 0 || e.PriceSnapshot == nil || e.PriceSnapshot.BillingMode != domain.BillingModeFree {
				problems = append(problems, errors.New("free lease requires a zero reservation and free price snapshot"))
			}
		case LeaseModeUnknownAllowed:
			if e.ReservationMicrosUSD != nil || e.PriceSnapshot == nil || e.PriceSnapshot.CostValueStatus != domain.CostValueUnknown {
				problems = append(problems, errors.New("unknown lease requires an unknown price snapshot"))
			}
			if e.UnknownPolicyEvidence == nil || e.UnknownPolicyEvidence.ProjectID != e.ProjectID {
				problems = append(problems, errors.New("unknown lease requires matching policy evidence"))
			} else if err := e.UnknownPolicyEvidence.Validate(); err != nil {
				problems = append(problems, err)
			}
		default:
			problems = append(problems, errors.New("lease mode is invalid"))
		}
		if e.LeaseMode != "" && !domain.ValidSHA256Label(e.TokenGuardPricingViewDigest) {
			problems = append(problems, errors.New("tagged lease requires token guard pricing view digest"))
		}
		if e.LeaseMode != LeaseModeUnknownAllowed && e.UnknownPolicyEvidence != nil {
			problems = append(problems, errors.New("known-price lease cannot contain unknown-price policy evidence"))
		}
	}
	if e.Kind == EventAttemptSettled {
		if e.LeaseMode == LeaseModeUnknownAllowed {
			if e.CommittedMicrosUSD != nil {
				problems = append(problems, errors.New("unknown settlement cost must be null"))
			}
		} else if e.CommittedMicrosUSD == nil {
			problems = append(problems, errors.New("known settlement cost is required"))
		}
		if e.TokenUsageSource != "" {
			switch e.TokenUsageSource {
			case TokenUsageSourceProvider:
				if e.TokenEstimated {
					problems = append(problems, errors.New("provider-reported usage cannot be estimated"))
				}
			case TokenUsageSourceEstimate:
				if !e.TokenEstimated {
					problems = append(problems, errors.New("gateway-estimated usage must be marked estimated"))
				}
			case TokenUsageSourceNone:
				if e.ProviderInputTokens != 0 || e.ProviderOutputTokens != 0 {
					problems = append(problems, errors.New("none token source cannot contain tokens"))
				}
			default:
				problems = append(problems, errors.New("token usage source is invalid"))
			}
		}
	}
	return errors.Join(problems...)
}

// validateForAppend applies invariants that every event written by this build
// must satisfy while Event.Validate remains able to read authenticated history
// written before the corresponding field existed. In particular, schema-era
// legacy events may omit the account-region snapshot for a fixed surface; a new
// append may not silently create another such record.
func (e Event) validateForAppend() error {
	if err := e.Validate(); err != nil {
		return err
	}
	// A primitive is meaningful only inside its profile contract. Settlements
	// are also checked against their reservation in State.Apply, but rejecting
	// an unbound primitive here keeps a bad settlement out of the durable log in
	// the first place. An authenticated pre-field reservation may legitimately
	// have no primitive, and recovery must be able to settle it without
	// inventing one. Every new reservation/start is stricter and must carry one.
	if e.ProviderPrimitive != "" && !provider.ProfileBindsPrimitive(e.ProfileID, e.ProviderPrimitive) {
		return errors.New("attempt event requires a primitive bound by its provider profile")
	}
	if (e.Kind == EventReservationCreated || e.Kind == EventAttemptStarted) &&
		e.ProfileID != "" && e.ProviderPrimitive == "" {
		return errors.New("attempt event requires a primitive bound by its provider profile")
	}
	if e.ProfileID != "" {
		identity, ok := domain.IdentityForProfile(e.ProfileID)
		if ok && identity.RegionScope == domain.RegionScopeFixed && e.AccountRegionID == domain.RegionNone {
			return errors.New("fixed provider surface requires account region attribution")
		}
	}
	if e.Kind != EventAttemptSettled {
		return nil
	}
	return e.validateSettlementSemanticsForAppend()
}

// validateSettlementSemanticsForAppend is intentionally stricter than
// Validate. Authenticated history predating the canonical fields remains
// readable, but every event written now must form one coherent state: a
// success has no failure attached, a failure names where it happened, and
// upstream-specific evidence cannot be attached to a local phase.
func (e Event) validateSettlementSemanticsForAppend() error {
	if e.Outcome == "" {
		return errors.New("settlement outcome is required")
	}
	validPhase := func(phase string) bool {
		switch phase {
		case "pre_provider", "provider", "response_render", "accounting", "client":
			return true
		default:
			return false
		}
	}
	validClass := func(class string) bool {
		switch provider.ErrorClass(class) {
		case provider.ErrorAuthentication, provider.ErrorRateLimit, provider.ErrorTimeout,
			provider.ErrorProvider5xx, provider.ErrorBadRequest, provider.ErrorConnect,
			provider.ErrorMalformed, provider.ErrorUnknown, provider.ErrorCanceled:
			return true
		default:
			return false
		}
	}
	providerDetails := e.ProviderCode != "" || e.ProviderFailureReason != "" || e.ProviderRequestID != ""
	if e.Outcome == "success" {
		if e.FailurePhase != "" || e.ErrorClass != "" || providerDetails || e.Retryable || e.Ambiguous || e.FailureSemanticsRecorded {
			return errors.New("successful settlement cannot carry failure semantics")
		}
		if e.HTTPStatus != 0 && (e.HTTPStatus < 200 || e.HTTPStatus >= 300) {
			return errors.New("successful settlement HTTP status must be 2xx")
		}
		return nil
	}
	if !validPhase(e.FailurePhase) {
		return errors.New("failed settlement requires a valid failure phase")
	}
	if e.ErrorClass != "" && !validClass(e.ErrorClass) {
		return errors.New("failed settlement error class is invalid")
	}
	if e.ErrorClass != "" && e.FailurePhase != "provider" && e.FailurePhase != "client" {
		return errors.New("provider error class requires provider or client phase")
	}
	if e.FailurePhase == "provider" && e.ErrorClass == "" {
		return errors.New("provider failure requires an error class")
	}
	if e.HTTPStatus != 0 && (e.FailurePhase != "provider" || e.HTTPStatus < 100 || e.HTTPStatus > 599) {
		return errors.New("provider HTTP status requires provider phase")
	}
	if providerDetails || e.Retryable || e.FailureSemanticsRecorded {
		if e.FailurePhase != "provider" || !e.FailureSemanticsRecorded {
			return errors.New("provider failure evidence requires recorded provider semantics")
		}
	}
	return nil
}

type Record struct {
	// Generation names the WAL file this record was read from. It is 1 for
	// every log that has never been sealed, and it is what makes a stored
	// watermark resolvable after sealing — a sequence says which record, the
	// generation and offset say where it sits.
	Generation uint64
	Sequence   uint64
	Offset     int64
	Epoch      uint8
	Event      Event
}

type Watermark struct {
	Generation uint64 `json:"generation"`
	Offset     int64  `json:"offset"`
	Sequence   uint64 `json:"sequence"`
}

// After reports whether this watermark names a position past head.
//
// It exists because the obvious comparison is wrong once the log has more than
// one generation. An offset only means anything inside the generation it was
// taken in: a roll leaves the sequence where it was and restarts the offset at
// zero in a fresh file, so a watermark held against the generation before the
// roll has a legitimately larger offset than the head does. Comparing the two
// numbers directly reads that as "ahead of the ledger" and condemns a position
// that is simply older — which, wherever such a check gates a rebuild, throws
// away a checkpoint and replays the whole WAL for exactly as long as it takes
// the new generation to grow past the old offset.
//
// Sequence is the total order and is compared first; generation breaks ties,
// because a roll keeps the sequence and moves to the next file; the offset is
// consulted only within one generation.
func (w Watermark) After(head Watermark) bool {
	if w.Sequence != head.Sequence {
		return w.Sequence > head.Sequence
	}
	if w.Generation != head.Generation {
		return w.Generation > head.Generation
	}
	return w.Offset > head.Offset
}

// BalanceKey identifies the balance a charge accumulates into.
//
// TimezoneVersion is part of the key, not decoration. The date string alone
// means different UTC intervals under different accounting zones, so without
// the version a zone change would let charges measured against the old
// boundary and the new one land in the same balance — overspending or wiping a
// budget with nothing in the ledger to show for it. Keyed this way, a change
// mints a fresh period instead of contaminating one.
//
// Events written before the accounting timezone became a governed setting carry
// version 0 and stay in their own keys.
type BalanceKey struct {
	ProjectID       string
	PeriodID        string
	TimezoneVersion uint64
}

type Balance struct {
	ReservedMicrosUSD  int64
	CommittedMicrosUSD int64
	InputTokens        int64
	OutputTokens       int64
	UnknownAttempts    int64
}

type attemptReservation struct {
	Key     BalanceKey
	Amount  int64
	Lease   Event
	Started bool
}

type PendingLease struct {
	Reservation Event
	Started     bool
}

// WorkUnitCost is what a Work Unit's settled attempts add up to. It is kept
// as running totals because the totals are all anything reads: holding every
// settled attempt to add them up again on each read is what made the state
// grow with the history.
//
// Overflowed records that a total passed the int64 range. The totals are
// derived, so an overflow is reported to whoever reads them and never refuses
// the settlement that caused it.
type WorkUnitCost struct {
	KnownMicrosUSD     int64
	EstimatedMicrosUSD int64
	UnknownAttempts    int64
	Overflowed         bool
}

func (c *WorkUnitCost) add(event Event) {
	var err error
	if event.CommittedMicrosUSD == nil {
		c.UnknownAttempts, err = checkedAdd(c.UnknownAttempts, 1)
	} else {
		c.KnownMicrosUSD, err = checkedAdd(c.KnownMicrosUSD, *event.CommittedMicrosUSD)
		if err == nil && event.CostEstimated {
			c.EstimatedMicrosUSD, err = checkedAdd(c.EstimatedMicrosUSD, *event.CommittedMicrosUSD)
		}
	}
	if err != nil {
		c.Overflowed = true
	}
}

// OutcomePinCommitFailed settles an attempt whose price pin could not be
// committed after its lease was written. The pin is left prepared, and the
// startup that resolves it needs the lease, so a settlement with this outcome
// keeps its lease in the state. It happens only when the metadata store fails a
// write, so the leases it keeps stay few.
const OutcomePinCommitFailed = "pin_commit_failed"

// recentEventDigests is how many event digests the state remembers. The only
// event written twice is a deterministic recovery settlement, and both copies
// come from one recovery pass at startup, a few records apart; a window this
// size covers that many times over without growing with the history.
const recentEventDigests = 1 << 16

type State struct {
	mu           sync.RWMutex
	balances     map[BalanceKey]Balance
	reservations map[string]attemptReservation
	// leaseRecords holds the lease of each attempt still in flight; a
	// settlement deletes it, except one settled as OutcomePinCommitFailed.
	leaseRecords map[string]Record
	// eventDigests remembers the digests of the most recent events, in the
	// order eventOrder records, so a duplicate is recognised without keeping
	// every event ever written.
	eventDigests          map[string][32]byte
	eventOrder            []string
	eventNext             int
	workUnits             map[string]domain.WorkUnit
	runs                  map[string]domain.Run
	requestRuns           map[string]requestAttribution
	runsByWorkUnit        map[string][]string
	activeByWorkUnit      map[string]map[string]struct{}
	pendingByWorkUnit     map[string]map[string]struct{}
	workUnitCosts         map[string]WorkUnitCost
	governanceIdempotency map[string]GovernanceIdempotency
	watermark             Watermark
}

type requestAttribution struct {
	WorkUnitID string
	RunID      string
}

type GovernanceIdempotency struct {
	ProjectID          string
	Operation          string
	KeyHash            string
	RequestFingerprint string
	WorkUnitID         string
	RunID              string
}

func NewState() *State {
	return &State{
		balances:              make(map[BalanceKey]Balance),
		reservations:          make(map[string]attemptReservation),
		leaseRecords:          make(map[string]Record),
		eventDigests:          make(map[string][32]byte),
		workUnits:             make(map[string]domain.WorkUnit),
		runs:                  make(map[string]domain.Run),
		requestRuns:           make(map[string]requestAttribution),
		runsByWorkUnit:        make(map[string][]string),
		activeByWorkUnit:      make(map[string]map[string]struct{}),
		pendingByWorkUnit:     make(map[string]map[string]struct{}),
		workUnitCosts:         make(map[string]WorkUnitCost),
		governanceIdempotency: make(map[string]GovernanceIdempotency),
	}
}

func (s *State) Apply(record Record) error {
	if err := record.Event.Validate(); err != nil {
		return fmt.Errorf("sequence %d: %w", record.Sequence, err)
	}
	digest, err := eventDigest(record.Event)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.eventDigests[record.Event.EventID]; ok {
		if existing != digest {
			return fmt.Errorf("event id %q was reused with different content", record.Event.EventID)
		}
		if record.Sequence < s.watermark.Sequence {
			return fmt.Errorf("sequence %d is before watermark %d", record.Sequence, s.watermark.Sequence)
		}
		// A crash may leave the same deterministic recovery event durably
		// appended more than once. Its effects are already reflected in the
		// read model, but replay must still consume the later physical record.
		if record.Sequence > s.watermark.Sequence {
			s.watermark = Watermark{Generation: record.Generation, Offset: record.Offset, Sequence: record.Sequence}
		}
		return nil
	}
	if record.Sequence <= s.watermark.Sequence {
		return fmt.Errorf("sequence %d is not after watermark %d", record.Sequence, s.watermark.Sequence)
	}

	event := record.Event
	if event.Kind >= EventWorkUnitCreated {
		idempotencyKey := governanceIdempotencyMapKey(event.ProjectID, event.Operation, event.IdempotencyKeyHash)
		if existing, ok := s.governanceIdempotency[idempotencyKey]; ok {
			if existing.RequestFingerprint != event.RequestFingerprint {
				return errors.New("idempotency key conflicts with another governance request")
			}
			s.rememberDigest(event.EventID, digest)
			s.watermark = Watermark{Generation: record.Generation, Offset: record.Offset, Sequence: record.Sequence}
			return nil
		}
	}
	key := BalanceKey{ProjectID: event.ProjectID, PeriodID: event.PeriodID, TimezoneVersion: event.PeriodTimezoneVersion}
	balance := s.balances[key]
	if event.Kind <= EventRequestFinalized && (len(s.requestRuns) != 0 || event.WorkUnitID != "" || event.RunID != "") {
		attribution, exists := s.requestRuns[event.RequestID]
		if event.Kind == EventRequestAccepted {
			if exists && (attribution.WorkUnitID != event.WorkUnitID || attribution.RunID != event.RunID) {
				return errors.New("accepted request changed run attribution")
			}
			if event.RunID != "" {
				run, runExists := s.runs[event.RunID]
				if !runExists || run.ProjectID != event.ProjectID || run.WorkUnitID != event.WorkUnitID ||
					run.Status != domain.RunActive || !event.OccurredAt.Before(run.ExpiresAt) {
					return errors.New("request run attribution is not active in this project")
				}
				s.requestRuns[event.RequestID] = requestAttribution{WorkUnitID: event.WorkUnitID, RunID: event.RunID}
				if s.activeByWorkUnit[event.WorkUnitID] == nil {
					s.activeByWorkUnit[event.WorkUnitID] = make(map[string]struct{})
				}
				s.activeByWorkUnit[event.WorkUnitID][event.RequestID] = struct{}{}
			}
		} else if exists {
			if attribution.WorkUnitID != event.WorkUnitID || attribution.RunID != event.RunID {
				return errors.New("request event changed run attribution")
			}
		} else if event.WorkUnitID != "" || event.RunID != "" {
			return errors.New("request event has attribution without an accepted request")
		}
	}

	switch event.Kind {
	case EventWorkUnitCreated:
		if _, exists := s.workUnits[event.WorkUnitID]; exists {
			return fmt.Errorf("work unit %q already exists", event.WorkUnitID)
		}
		workUnit := domain.WorkUnit{
			ID: event.WorkUnitID, ProjectID: event.ProjectID, Status: domain.WorkUnitOpen,
			CreatedByKeyID: event.KeyID, CreatedAt: event.OccurredAt,
			PeriodID: event.PeriodID, PeriodTimezoneVersion: event.PeriodTimezoneVersion,
			OutcomeDefinitions: slices.Clone(event.OutcomeDefinitions),
		}
		if err := workUnit.Validate(); err != nil {
			return err
		}
		s.workUnits[workUnit.ID] = workUnit
	case EventWorkUnitClosed:
		workUnit, exists := s.workUnits[event.WorkUnitID]
		if !exists || workUnit.ProjectID != event.ProjectID {
			return fmt.Errorf("work unit %q is not in project", event.WorkUnitID)
		}
		if workUnit.Status != domain.WorkUnitOpen {
			return fmt.Errorf("work unit %q is already closed", event.WorkUnitID)
		}
		closedAt := event.OccurredAt
		workUnit.Status, workUnit.ClosedAt = domain.WorkUnitClosed, &closedAt
		s.workUnits[workUnit.ID] = workUnit
	case EventRunCreated:
		if _, exists := s.runs[event.RunID]; exists {
			return fmt.Errorf("run %q already exists", event.RunID)
		}
		workUnit, exists := s.workUnits[event.WorkUnitID]
		if !exists || workUnit.ProjectID != event.ProjectID || workUnit.Status != domain.WorkUnitOpen {
			return fmt.Errorf("work unit %q is not open in project", event.WorkUnitID)
		}
		if len(s.runsByWorkUnit[event.WorkUnitID]) >= domain.MaxRunsPerWorkUnit {
			return errors.New("work unit run limit exceeded")
		}
		run := domain.Run{
			ID: event.RunID, ProjectID: event.ProjectID, WorkUnitID: event.WorkUnitID,
			BudgetMicrosUSD: event.RunBudgetMicrosUSD, Status: domain.RunActive,
			CreatedByKeyID: event.KeyID, CreatedAt: event.OccurredAt, ExpiresAt: event.RunExpiresAt,
		}
		if err := run.Validate(); err != nil {
			return err
		}
		s.runs[run.ID] = run
		s.runsByWorkUnit[run.WorkUnitID] = append(s.runsByWorkUnit[run.WorkUnitID], run.ID)
	case EventRunClosed:
		run, exists := s.runs[event.RunID]
		if !exists || run.ProjectID != event.ProjectID || run.WorkUnitID != event.WorkUnitID {
			return fmt.Errorf("run %q is not in work unit", event.RunID)
		}
		if run.Status != domain.RunActive {
			return fmt.Errorf("run %q is already closed", event.RunID)
		}
		closedAt := event.OccurredAt
		run.Status, run.ClosedAt, run.CloseReason = domain.RunClosed, &closedAt, event.CloseReason
		s.runs[run.ID] = run
	case EventReservationCreated:
		amount := int64(0)
		if event.ReservationMicrosUSD != nil {
			amount = *event.ReservationMicrosUSD
		}
		// An attempt settled earlier has left the state, so reusing its id is
		// not caught here. Attempt ids are 128 random bits and the WAL is
		// chained under a MAC, so a reused one could only be a forgery, and a
		// forgery does not get past the chain.
		if _, exists := s.reservations[event.AttemptID]; exists {
			return fmt.Errorf("attempt %q already has a reservation", event.AttemptID)
		}
		if event.LeaseMode != LeaseModeUnknownAllowed {
			balance.ReservedMicrosUSD, err = checkedAdd(balance.ReservedMicrosUSD, amount)
			if err != nil {
				return err
			}
		}
		s.reservations[event.AttemptID] = attemptReservation{Key: key, Amount: amount, Lease: event}
		if event.WorkUnitID != "" {
			if s.pendingByWorkUnit[event.WorkUnitID] == nil {
				s.pendingByWorkUnit[event.WorkUnitID] = make(map[string]struct{})
			}
			s.pendingByWorkUnit[event.WorkUnitID][event.AttemptID] = struct{}{}
		}
		s.leaseRecords[event.AttemptID] = record
		if event.RunID != "" && event.LeaseMode != LeaseModeUnknownAllowed {
			run := s.runs[event.RunID]
			run.ReservedMicrosUSD, err = checkedAdd(run.ReservedMicrosUSD, amount)
			if err != nil {
				return err
			}
			s.runs[event.RunID] = run
		}
	case EventAttemptStarted:
		reservation, exists := s.reservations[event.AttemptID]
		if !exists {
			return fmt.Errorf("attempt %q has no accounting lease", event.AttemptID)
		}
		if reservation.Started {
			return fmt.Errorf("attempt %q already started", event.AttemptID)
		}
		reservation.Started = true
		s.reservations[event.AttemptID] = reservation
	case EventAttemptSettled:
		reservation, exists := s.reservations[event.AttemptID]
		if !exists {
			return fmt.Errorf("attempt %q has no reservation", event.AttemptID)
		}
		if reservation.Key != key {
			return fmt.Errorf("attempt %q settlement changed project or period", event.AttemptID)
		}
		if event.WorkUnitID != reservation.Lease.WorkUnitID || event.RunID != reservation.Lease.RunID {
			return fmt.Errorf("attempt %q settlement changed run attribution", event.AttemptID)
		}
		if event.RouteID != reservation.Lease.RouteID ||
			event.DeploymentID != reservation.Lease.DeploymentID ||
			event.ProviderID != reservation.Lease.ProviderID ||
			event.OfferingID != reservation.Lease.OfferingID ||
			event.ProfileID != reservation.Lease.ProfileID ||
			event.AccountRegionID != reservation.Lease.AccountRegionID ||
			event.ProviderModel != reservation.Lease.ProviderModel ||
			event.ProviderPrimitive != reservation.Lease.ProviderPrimitive {
			return fmt.Errorf("attempt %q settlement changed provider attribution", event.AttemptID)
		}
		if reservation.Lease.PriceSnapshot != nil {
			if event.PriceSnapshot == nil || !samePriceSnapshot(*reservation.Lease.PriceSnapshot, *event.PriceSnapshot) {
				return fmt.Errorf("attempt %q settlement changed its price snapshot", event.AttemptID)
			}
			if event.LeaseMode != reservation.Lease.LeaseMode {
				return fmt.Errorf("attempt %q settlement changed its lease mode", event.AttemptID)
			}
		}
		if balance.ReservedMicrosUSD < reservation.Amount {
			return fmt.Errorf("attempt %q reservation exceeds project balance", event.AttemptID)
		}
		balance.ReservedMicrosUSD -= reservation.Amount
		if event.CommittedMicrosUSD != nil {
			balance.CommittedMicrosUSD, err = checkedAdd(balance.CommittedMicrosUSD, *event.CommittedMicrosUSD)
			if err != nil {
				return err
			}
		}
		balance.InputTokens, err = checkedAdd(balance.InputTokens, event.ProviderInputTokens)
		if err != nil {
			return err
		}
		balance.OutputTokens, err = checkedAdd(balance.OutputTokens, event.ProviderOutputTokens)
		if err != nil {
			return err
		}
		if reservation.Lease.LeaseMode == LeaseModeUnknownAllowed {
			balance.UnknownAttempts, err = checkedAdd(balance.UnknownAttempts, 1)
			if err != nil {
				return err
			}
		}
		if event.RunID != "" {
			run := s.runs[event.RunID]
			if reservation.Lease.LeaseMode != LeaseModeUnknownAllowed {
				if run.ReservedMicrosUSD < reservation.Amount {
					return fmt.Errorf("attempt %q reservation exceeds run balance", event.AttemptID)
				}
				run.ReservedMicrosUSD -= reservation.Amount
			}
			if event.CommittedMicrosUSD != nil {
				run.CommittedMicrosUSD, err = checkedAdd(run.CommittedMicrosUSD, *event.CommittedMicrosUSD)
				if err != nil {
					return err
				}
			}
			if reservation.Lease.LeaseMode == LeaseModeUnknownAllowed {
				run.UnknownAttempts, err = checkedAdd(run.UnknownAttempts, 1)
				if err != nil {
					return err
				}
			}
			s.runs[event.RunID] = run
		}
		delete(s.reservations, event.AttemptID)
		if event.Outcome != OutcomePinCommitFailed {
			delete(s.leaseRecords, event.AttemptID)
		}
		if event.WorkUnitID != "" {
			delete(s.pendingByWorkUnit[event.WorkUnitID], event.AttemptID)
			if len(s.pendingByWorkUnit[event.WorkUnitID]) == 0 {
				delete(s.pendingByWorkUnit, event.WorkUnitID)
			}
			cost := s.workUnitCosts[event.WorkUnitID]
			cost.add(event)
			s.workUnitCosts[event.WorkUnitID] = cost
		}
	case EventRequestFinalized:
		if attribution, ok := s.requestRuns[event.RequestID]; ok {
			delete(s.activeByWorkUnit[attribution.WorkUnitID], event.RequestID)
			if len(s.activeByWorkUnit[attribution.WorkUnitID]) == 0 {
				delete(s.activeByWorkUnit, attribution.WorkUnitID)
			}
		}
		delete(s.requestRuns, event.RequestID)
	}

	s.balances[key] = balance
	if event.Kind >= EventWorkUnitCreated {
		s.governanceIdempotency[governanceIdempotencyMapKey(event.ProjectID, event.Operation, event.IdempotencyKeyHash)] = GovernanceIdempotency{
			ProjectID: event.ProjectID, Operation: event.Operation, KeyHash: event.IdempotencyKeyHash,
			RequestFingerprint: event.RequestFingerprint, WorkUnitID: event.WorkUnitID, RunID: event.RunID,
		}
	}
	s.rememberDigest(event.EventID, digest)
	s.watermark = Watermark{
		Generation: record.Generation,
		Offset:     record.Offset,
		Sequence:   record.Sequence,
	}
	return nil
}

// rememberDigest records an event's digest, forgetting the oldest once the
// window is full.
func (s *State) rememberDigest(eventID string, digest [32]byte) {
	if len(s.eventOrder) < recentEventDigests {
		s.eventOrder = append(s.eventOrder, eventID)
	} else {
		delete(s.eventDigests, s.eventOrder[s.eventNext])
		s.eventOrder[s.eventNext] = eventID
		s.eventNext = (s.eventNext + 1) % recentEventDigests
	}
	s.eventDigests[eventID] = digest
}

var ErrGovernanceCohortLimit = errors.New("governance cohort work unit limit exceeded")

type GovernanceSnapshotOptions struct {
	ProjectID         string
	WorkUnitID        string
	DefinitionID      string
	DefinitionVersion uint64
	CohortStart       string
	CohortEnd         string
	MaxWorkUnits      int
	IncludeRuns       bool
	IncludeCosts      bool
}

// GovernanceSnapshot captures every accounting input used by one governance
// query under the State read lock. The watermark therefore names this exact
// accounting view, while the two journals remain intentionally independent.
type GovernanceSnapshot struct {
	WorkUnits        []domain.WorkUnit
	Runs             []domain.Run
	Costs            map[string]WorkUnitCost
	InflightWorkUnit map[string]bool
	Watermark        Watermark
}

func (s *State) GovernanceSnapshot(ctx context.Context, options GovernanceSnapshotOptions) (GovernanceSnapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := GovernanceSnapshot{InflightWorkUnit: make(map[string]bool), Costs: make(map[string]WorkUnitCost), Watermark: s.watermark}
	selected := make(map[string]struct{})
	for _, workUnit := range s.workUnits {
		if err := ctx.Err(); err != nil {
			return GovernanceSnapshot{}, err
		}
		if options.ProjectID != "" && workUnit.ProjectID != options.ProjectID {
			continue
		}
		if options.WorkUnitID != "" && workUnit.ID != options.WorkUnitID {
			continue
		}
		if options.CohortStart != "" && (workUnit.PeriodID < options.CohortStart || workUnit.PeriodID > options.CohortEnd) {
			continue
		}
		if options.DefinitionID != "" {
			eligible := false
			for _, reference := range workUnit.OutcomeDefinitions {
				if reference.ID == options.DefinitionID && reference.Version == options.DefinitionVersion {
					eligible = true
					break
				}
			}
			if !eligible {
				continue
			}
		}
		if options.MaxWorkUnits > 0 && len(result.WorkUnits) >= options.MaxWorkUnits {
			return GovernanceSnapshot{}, ErrGovernanceCohortLimit
		}
		workUnit.OutcomeDefinitions = slices.Clone(workUnit.OutcomeDefinitions)
		result.WorkUnits = append(result.WorkUnits, workUnit)
		selected[workUnit.ID] = struct{}{}
	}
	for workUnitID := range selected {
		if err := ctx.Err(); err != nil {
			return GovernanceSnapshot{}, err
		}
		if options.IncludeRuns {
			for _, runID := range s.runsByWorkUnit[workUnitID] {
				result.Runs = append(result.Runs, s.runs[runID])
			}
		}
		if len(s.activeByWorkUnit[workUnitID]) > 0 || len(s.pendingByWorkUnit[workUnitID]) > 0 {
			result.InflightWorkUnit[workUnitID] = true
		}
		if options.IncludeCosts {
			if cost, ok := s.workUnitCosts[workUnitID]; ok {
				result.Costs[workUnitID] = cost
			}
		}
	}
	sort.Slice(result.WorkUnits, func(i, j int) bool { return result.WorkUnits[i].ID < result.WorkUnits[j].ID })
	sort.Slice(result.Runs, func(i, j int) bool { return result.Runs[i].ID < result.Runs[j].ID })
	return result, nil
}

func (s *State) WorkUnit(id string) (domain.WorkUnit, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.workUnits[id]
	return value, ok
}

func (s *State) WorkUnits(projectID string) []domain.WorkUnit {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]domain.WorkUnit, 0)
	for _, item := range s.workUnits {
		if projectID == "" || item.ProjectID == projectID {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items
}

func (s *State) Run(id string) (domain.Run, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.runs[id]
	return value, ok
}

func (s *State) Runs(projectID, workUnitID string) []domain.Run {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]domain.Run, 0)
	for _, item := range s.runs {
		if (projectID == "" || item.ProjectID == projectID) && (workUnitID == "" || item.WorkUnitID == workUnitID) {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items
}

func governanceIdempotencyMapKey(projectID, operation, keyHash string) string {
	return projectID + "\x00" + operation + "\x00" + keyHash
}

func (s *State) GovernanceIdempotency(projectID, operation, keyHash string) (GovernanceIdempotency, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.governanceIdempotency[governanceIdempotencyMapKey(projectID, operation, keyHash)]
	return value, ok
}

func (e Event) KnownCommittedMicrosUSD() (int64, bool) {
	if e.CommittedMicrosUSD == nil {
		return 0, false
	}
	return *e.CommittedMicrosUSD, true
}

func (s *State) Balance(projectID, periodID string, timezoneVersion uint64) Balance {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.balances[BalanceKey{ProjectID: projectID, PeriodID: periodID, TimezoneVersion: timezoneVersion}]
}

// PeriodKey names one accounting day under one timezone generation, across
// every project.
type PeriodKey struct {
	PeriodID        string
	TimezoneVersion uint64
}

// PeriodTotals adds the balances up per accounting day across projects, and
// reports the days some lease is still open against: a settlement may yet land
// in those, so their totals are not final.
func (s *State) PeriodTotals() (totals map[PeriodKey]Balance, open map[PeriodKey]bool, watermark Watermark, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	totals, open = make(map[PeriodKey]Balance), make(map[PeriodKey]bool)
	for key, balance := range s.balances {
		period := PeriodKey{PeriodID: key.PeriodID, TimezoneVersion: key.TimezoneVersion}
		total := totals[period]
		for _, pair := range []struct {
			into  *int64
			value int64
		}{
			{&total.ReservedMicrosUSD, balance.ReservedMicrosUSD},
			{&total.CommittedMicrosUSD, balance.CommittedMicrosUSD},
			{&total.InputTokens, balance.InputTokens},
			{&total.OutputTokens, balance.OutputTokens},
			{&total.UnknownAttempts, balance.UnknownAttempts},
		} {
			if *pair.into, err = checkedAdd(*pair.into, pair.value); err != nil {
				return nil, nil, Watermark{}, err
			}
		}
		totals[period] = total
	}
	for _, reservation := range s.reservations {
		open[PeriodKey{PeriodID: reservation.Key.PeriodID, TimezoneVersion: reservation.Key.TimezoneVersion}] = true
	}
	return totals, open, s.watermark, nil
}

func (s *State) PendingReservations() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.reservations)
}

func (s *State) PendingLeases() []PendingLease {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]PendingLease, 0, len(s.reservations))
	for _, reservation := range s.reservations {
		items = append(items, PendingLease{Reservation: reservation.Lease, Started: reservation.Started})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Reservation.AttemptID < items[j].Reservation.AttemptID })
	return items
}

func (s *State) PendingLeaseStats(now time.Time) (int, time.Duration) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var oldest time.Time
	for _, reservation := range s.reservations {
		if oldest.IsZero() || reservation.Lease.OccurredAt.Before(oldest) {
			oldest = reservation.Lease.OccurredAt
		}
	}
	if oldest.IsZero() || now.Before(oldest) {
		return len(s.reservations), 0
	}
	return len(s.reservations), now.Sub(oldest)
}

// AccountingLease is the lease of an attempt still in flight, or of one settled
// as OutcomePinCommitFailed. Any other settled attempt's lease has left the
// state.
func (s *State) AccountingLease(attemptID string) (Record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.leaseRecords[attemptID]
	return record, ok
}

func (s *State) Watermark() Watermark {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.watermark
}

func eventDigest(event Event) ([32]byte, error) {
	encoded, err := json.Marshal(event)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256Sum(encoded), nil
}

func samePriceSnapshot(left, right domain.PriceSnapshot) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func checkedAdd(left, right int64) (int64, error) {
	if right > 0 && left > math.MaxInt64-right {
		return 0, errors.New("ledger integer overflow")
	}
	if right < 0 && left < math.MinInt64-right {
		return 0, errors.New("ledger integer underflow")
	}
	return left + right, nil
}
