package ledger

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
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
)

func (k EventKind) Valid() bool {
	return k >= EventRequestAccepted && k <= EventRequestFinalized
}

type Event struct {
	EventID                     string                             `json:"event_id"`
	Kind                        EventKind                          `json:"kind"`
	RequestID                   string                             `json:"request_id"`
	AttemptID                   string                             `json:"attempt_id,omitempty"`
	ProjectID                   string                             `json:"project_id"`
	KeyID                       string                             `json:"key_id,omitempty"`
	RouteID                     string                             `json:"route_id,omitempty"`
	DeploymentID                string                             `json:"deployment_id,omitempty"`
	ProviderID                  string                             `json:"provider_id,omitempty"`
	RequestedModel              string                             `json:"requested_model,omitempty"`
	ProviderModel               string                             `json:"provider_model,omitempty"`
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
	LatencyMillis                 int64            `json:"latency_millis,omitempty"`
}

func (e Event) Validate() error {
	var problems []error
	if e.EventID == "" {
		problems = append(problems, errors.New("event id is required"))
	}
	if !e.Kind.Valid() {
		problems = append(problems, errors.New("event kind is invalid"))
	}
	if e.RequestID == "" {
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

type Record struct {
	Sequence uint64
	Offset   int64
	Epoch    uint8
	Event    Event
}

type Watermark struct {
	Generation uint64 `json:"generation"`
	Offset     int64  `json:"offset"`
	Sequence   uint64 `json:"sequence"`
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

type SettledAttempt struct {
	Settlement       Event
	SettlementDigest string
	CostMicrosUSD    int64
	CostKnown        bool
}

type State struct {
	mu           sync.RWMutex
	balances     map[BalanceKey]Balance
	reservations map[string]attemptReservation
	leaseRecords map[string]Record
	settled      map[string]SettledAttempt
	eventDigests map[string][32]byte
	watermark    Watermark
}

func NewState() *State {
	return &State{
		balances:     make(map[BalanceKey]Balance),
		reservations: make(map[string]attemptReservation),
		leaseRecords: make(map[string]Record),
		settled:      make(map[string]SettledAttempt),
		eventDigests: make(map[string][32]byte),
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
			s.watermark = Watermark{Generation: 1, Offset: record.Offset, Sequence: record.Sequence}
		}
		return nil
	}
	if record.Sequence <= s.watermark.Sequence {
		return fmt.Errorf("sequence %d is not after watermark %d", record.Sequence, s.watermark.Sequence)
	}

	event := record.Event
	key := BalanceKey{ProjectID: event.ProjectID, PeriodID: event.PeriodID, TimezoneVersion: event.PeriodTimezoneVersion}
	balance := s.balances[key]

	switch event.Kind {
	case EventReservationCreated:
		amount := int64(0)
		if event.ReservationMicrosUSD != nil {
			amount = *event.ReservationMicrosUSD
		}
		if _, exists := s.reservations[event.AttemptID]; exists {
			return fmt.Errorf("attempt %q already has a reservation", event.AttemptID)
		}
		if _, exists := s.settled[event.AttemptID]; exists {
			return fmt.Errorf("attempt %q is already settled", event.AttemptID)
		}
		if event.LeaseMode != LeaseModeUnknownAllowed {
			balance.ReservedMicrosUSD, err = checkedAdd(balance.ReservedMicrosUSD, amount)
			if err != nil {
				return err
			}
		}
		s.reservations[event.AttemptID] = attemptReservation{Key: key, Amount: amount, Lease: event}
		s.leaseRecords[event.AttemptID] = record
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
		if _, exists := s.settled[event.AttemptID]; exists {
			return fmt.Errorf("attempt %q is already settled", event.AttemptID)
		}
		reservation, exists := s.reservations[event.AttemptID]
		if !exists {
			return fmt.Errorf("attempt %q has no reservation", event.AttemptID)
		}
		if reservation.Key != key {
			return fmt.Errorf("attempt %q settlement changed project or period", event.AttemptID)
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
		delete(s.reservations, event.AttemptID)
		settlementDigest := "sha256:" + fmt.Sprintf("%x", digest)
		base := int64(0)
		if event.CommittedMicrosUSD != nil {
			base = *event.CommittedMicrosUSD
		}
		s.settled[event.AttemptID] = SettledAttempt{Settlement: event, SettlementDigest: settlementDigest, CostMicrosUSD: base, CostKnown: event.CommittedMicrosUSD != nil}
	}

	s.balances[key] = balance
	s.eventDigests[event.EventID] = digest
	s.watermark = Watermark{
		Generation: 1,
		Offset:     record.Offset,
		Sequence:   record.Sequence,
	}
	return nil
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

func (s *State) AccountingLease(attemptID string) (Record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.leaseRecords[attemptID]
	return record, ok
}

func (s *State) SettledAttempt(attemptID string) (SettledAttempt, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.settled[attemptID]
	return value, ok
}

func (s *State) SettledAttempts() []SettledAttempt {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]SettledAttempt, 0, len(s.settled))
	for _, item := range s.settled {
		items = append(items, item)
	}
	return items
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
