package ledger

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

type EventKind uint8

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
	EventID              string    `json:"event_id"`
	Kind                 EventKind `json:"kind"`
	RequestID            string    `json:"request_id"`
	AttemptID            string    `json:"attempt_id,omitempty"`
	ProjectID            string    `json:"project_id"`
	KeyID                string    `json:"key_id,omitempty"`
	RouteID              string    `json:"route_id,omitempty"`
	DeploymentID         string    `json:"deployment_id,omitempty"`
	ProviderID           string    `json:"provider_id,omitempty"`
	RequestedModel       string    `json:"requested_model,omitempty"`
	ProviderModel        string    `json:"provider_model,omitempty"`
	AttemptNumber        int       `json:"attempt_number,omitempty"`
	RetryCount           int       `json:"retry_count,omitempty"`
	FallbackCount        int       `json:"fallback_count,omitempty"`
	PeriodID             string    `json:"period_id"`
	OccurredAt           time.Time `json:"occurred_at"`
	ReservationMicrosUSD int64     `json:"reservation_micros_usd,omitempty"`
	CommittedMicrosUSD   int64     `json:"committed_micros_usd,omitempty"`
	ProviderInputTokens  int64     `json:"provider_input_tokens,omitempty"`
	ProviderOutputTokens int64     `json:"provider_output_tokens,omitempty"`
	PreparedOutputTokens int64     `json:"prepared_output_tokens,omitempty"`
	CostEstimated        bool      `json:"cost_estimated,omitempty"`
	TokenEstimated       bool      `json:"token_estimated,omitempty"`
	Outcome              string    `json:"outcome,omitempty"`
	ErrorClass           string    `json:"error_class,omitempty"`
	HTTPStatus           int       `json:"http_status,omitempty"`
	LatencyMillis        int64     `json:"latency_millis,omitempty"`
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
	if e.ReservationMicrosUSD < 0 || e.CommittedMicrosUSD < 0 {
		problems = append(problems, errors.New("amounts cannot be negative"))
	}
	if e.ProviderInputTokens < 0 || e.ProviderOutputTokens < 0 || e.PreparedOutputTokens < 0 {
		problems = append(problems, errors.New("tokens cannot be negative"))
	}
	if e.AttemptNumber < 0 || e.RetryCount < 0 || e.FallbackCount < 0 ||
		e.HTTPStatus < 0 || e.LatencyMillis < 0 {
		problems = append(problems, errors.New("attempt counters, HTTP status, and latency cannot be negative"))
	}
	switch e.Kind {
	case EventReservationCreated, EventAttemptStarted, EventAttemptSettled:
		if e.AttemptID == "" {
			problems = append(problems, errors.New("attempt id is required for attempt events"))
		}
	}
	if e.Kind == EventReservationCreated && e.ReservationMicrosUSD == 0 {
		problems = append(problems, errors.New("reservation amount must be positive"))
	}
	return errors.Join(problems...)
}

type Record struct {
	Sequence uint64
	Offset   int64
	Event    Event
}

type Watermark struct {
	Generation uint64 `json:"generation"`
	Offset     int64  `json:"offset"`
	Sequence   uint64 `json:"sequence"`
}

type BalanceKey struct {
	ProjectID string
	PeriodID  string
}

type Balance struct {
	ReservedMicrosUSD  int64
	CommittedMicrosUSD int64
	InputTokens        int64
	OutputTokens       int64
}

type attemptReservation struct {
	Key    BalanceKey
	Amount int64
}

type State struct {
	mu           sync.RWMutex
	balances     map[BalanceKey]Balance
	reservations map[string]attemptReservation
	settled      map[string]struct{}
	eventDigests map[string][32]byte
	watermark    Watermark
}

func NewState() *State {
	return &State{
		balances:     make(map[BalanceKey]Balance),
		reservations: make(map[string]attemptReservation),
		settled:      make(map[string]struct{}),
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
		return nil
	}
	if record.Sequence <= s.watermark.Sequence {
		return fmt.Errorf("sequence %d is not after watermark %d", record.Sequence, s.watermark.Sequence)
	}

	event := record.Event
	key := BalanceKey{ProjectID: event.ProjectID, PeriodID: event.PeriodID}
	balance := s.balances[key]

	switch event.Kind {
	case EventReservationCreated:
		if _, exists := s.reservations[event.AttemptID]; exists {
			return fmt.Errorf("attempt %q already has a reservation", event.AttemptID)
		}
		if _, exists := s.settled[event.AttemptID]; exists {
			return fmt.Errorf("attempt %q is already settled", event.AttemptID)
		}
		balance.ReservedMicrosUSD, err = checkedAdd(balance.ReservedMicrosUSD, event.ReservationMicrosUSD)
		if err != nil {
			return err
		}
		s.reservations[event.AttemptID] = attemptReservation{Key: key, Amount: event.ReservationMicrosUSD}
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
		if balance.ReservedMicrosUSD < reservation.Amount {
			return fmt.Errorf("attempt %q reservation exceeds project balance", event.AttemptID)
		}
		balance.ReservedMicrosUSD -= reservation.Amount
		balance.CommittedMicrosUSD, err = checkedAdd(balance.CommittedMicrosUSD, event.CommittedMicrosUSD)
		if err != nil {
			return err
		}
		balance.InputTokens, err = checkedAdd(balance.InputTokens, event.ProviderInputTokens)
		if err != nil {
			return err
		}
		balance.OutputTokens, err = checkedAdd(balance.OutputTokens, event.ProviderOutputTokens)
		if err != nil {
			return err
		}
		delete(s.reservations, event.AttemptID)
		s.settled[event.AttemptID] = struct{}{}
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

func (s *State) Balance(projectID, periodID string) Balance {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.balances[BalanceKey{ProjectID: projectID, PeriodID: periodID}]
}

func (s *State) PendingReservations() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.reservations)
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

func checkedAdd(left, right int64) (int64, error) {
	if right > 0 && left > math.MaxInt64-right {
		return 0, errors.New("ledger integer overflow")
	}
	if right < 0 && left < math.MinInt64-right {
		return 0, errors.New("ledger integer underflow")
	}
	return left + right, nil
}
