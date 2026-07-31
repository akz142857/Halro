package budget

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/akz142857/Heimdall/internal/id"
	"github.com/akz142857/Heimdall/internal/ledger"
)

var (
	ErrExceeded      = errors.New("daily budget exceeded")
	ErrInvalidAmount = errors.New("invalid accounting amount")
)

type Attempt struct {
	RequestID            string
	AttemptID            string
	ProjectID            string
	PeriodID             string
	ReservationMicrosUSD int64
	KeyID                string
	RouteID              string
	DeploymentID         string
	ProviderID           string
	RequestedModel       string
	ProviderModel        string
	AttemptNumber        int
	RetryCount           int
	FallbackCount        int
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
}

type Manager struct {
	mu        sync.Mutex
	log       *ledger.Log
	state     *ledger.State
	location  *time.Location
	now       func() time.Time
	observers []func(ledger.Record)
}

func (m *Manager) AddObserver(observer func(ledger.Record)) {
	if observer == nil {
		return
	}
	m.mu.Lock()
	m.observers = append(m.observers, observer)
	m.mu.Unlock()
}

func New(log *ledger.Log, state *ledger.State, location *time.Location) (*Manager, error) {
	if log == nil || state == nil || location == nil {
		return nil, errors.New("ledger log, state, and location are required")
	}
	return &Manager{
		log:      log,
		state:    state,
		location: location,
		now:      time.Now,
	}, nil
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
	m.mu.Lock()
	defer m.mu.Unlock()
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
	if request.RequestID == "" || request.ProjectID == "" || request.PeriodID == "" {
		return Attempt{}, errors.New("request lease is invalid")
	}
	if dailyBudgetMicrosUSD < 0 || reservationMicrosUSD <= 0 {
		return Attempt{}, ErrInvalidAmount
	}
	attemptID, err := id.New("att")
	if err != nil {
		return Attempt{}, err
	}
	eventID, err := id.New("evt")
	if err != nil {
		return Attempt{}, err
	}
	attempt := Attempt{
		RequestID: request.RequestID, AttemptID: attemptID, ProjectID: request.ProjectID,
		PeriodID: request.PeriodID, ReservationMicrosUSD: reservationMicrosUSD,
		KeyID: request.KeyID, RequestedModel: request.RequestedModel,
		RouteID: metadata.RouteID, DeploymentID: metadata.DeploymentID,
		ProviderID: metadata.ProviderID, ProviderModel: metadata.ProviderModel,
		AttemptNumber: metadata.AttemptNumber, RetryCount: metadata.RetryCount,
		FallbackCount: metadata.FallbackCount,
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	balance := m.state.Balance(request.ProjectID, request.PeriodID)
	total, err := checkedAdd(balance.CommittedMicrosUSD, balance.ReservedMicrosUSD)
	if err != nil {
		return Attempt{}, err
	}
	total, err = checkedAdd(total, reservationMicrosUSD)
	if err != nil {
		return Attempt{}, err
	}
	if dailyBudgetMicrosUSD > 0 && total > dailyBudgetMicrosUSD {
		return Attempt{}, ErrExceeded
	}
	if err := m.appendApply(ctx, ledger.Event{
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
		ReservationMicrosUSD: reservationMicrosUSD,
	}); err != nil {
		return Attempt{}, err
	}
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
	m.mu.Lock()
	defer m.mu.Unlock()
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
	if settlement.CommittedMicrosUSD < 0 ||
		settlement.ProviderInputTokens < 0 ||
		settlement.ProviderOutputTokens < 0 ||
		settlement.PreparedOutputTokens < 0 {
		return ErrInvalidAmount
	}
	eventID, err := id.New("evt")
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.appendApply(ctx, ledger.Event{
		EventID:              eventID,
		Kind:                 ledger.EventAttemptSettled,
		RequestID:            attempt.RequestID,
		AttemptID:            attempt.AttemptID,
		ProjectID:            attempt.ProjectID,
		KeyID:                attempt.KeyID,
		RouteID:              attempt.RouteID,
		DeploymentID:         attempt.DeploymentID,
		ProviderID:           attempt.ProviderID,
		RequestedModel:       attempt.RequestedModel,
		ProviderModel:        attempt.ProviderModel,
		AttemptNumber:        attempt.AttemptNumber,
		RetryCount:           attempt.RetryCount,
		FallbackCount:        attempt.FallbackCount,
		PeriodID:             attempt.PeriodID,
		OccurredAt:           m.now().In(m.location),
		CommittedMicrosUSD:   settlement.CommittedMicrosUSD,
		ProviderInputTokens:  settlement.ProviderInputTokens,
		ProviderOutputTokens: settlement.ProviderOutputTokens,
		PreparedOutputTokens: settlement.PreparedOutputTokens,
		CostEstimated:        settlement.CostEstimated,
		TokenEstimated:       settlement.TokenEstimated,
		Outcome:              settlement.Outcome,
		ErrorClass:           settlement.ErrorClass,
		HTTPStatus:           settlement.HTTPStatus,
		LatencyMillis:        settlement.LatencyMillis,
	})
}

func (m *Manager) FinalizeRequest(ctx context.Context, attempt Attempt, outcome string) error {
	return m.Finalize(ctx, attempt.Request(), outcome)
}

func (m *Manager) Finalize(ctx context.Context, request Request, outcome string) error {
	eventID, err := id.New("evt")
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
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
	watermark, err := m.log.Append(ctx, event)
	if err != nil {
		return err
	}
	record := ledger.Record{
		Sequence: watermark.Sequence,
		Offset:   watermark.Offset,
		Event:    event,
	}
	if err := m.state.Apply(record); err != nil {
		return fmt.Errorf("apply durable accounting event: %w", err)
	}
	for _, observer := range m.observers {
		observer(record)
	}
	return nil
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
