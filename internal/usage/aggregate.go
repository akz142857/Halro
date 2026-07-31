package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/akz142857/Heimdall/internal/ledger"
)

const checkpointVersion = 2

type AttemptEvent struct {
	EventID              string    `json:"event_id"`
	RequestID            string    `json:"request_id"`
	AttemptID            string    `json:"attempt_id"`
	Sequence             uint64    `json:"sequence"`
	AttemptNumber        int       `json:"attempt"`
	ProjectID            string    `json:"project_id"`
	KeyID                string    `json:"key_id,omitempty"`
	RouteID              string    `json:"route_id,omitempty"`
	DeploymentID         string    `json:"deployment_id,omitempty"`
	ProviderID           string    `json:"provider_id,omitempty"`
	RequestedModel       string    `json:"requested_model,omitempty"`
	ProviderModel        string    `json:"provider_model,omitempty"`
	ProviderInputTokens  int64     `json:"provider_input_tokens"`
	ProviderOutputTokens int64     `json:"provider_output_tokens"`
	PreparedOutputTokens int64     `json:"prepared_output_tokens"`
	CostMicrosUSD        int64     `json:"cost_micros_usd"`
	CostEstimated        bool      `json:"cost_estimated"`
	TokensEstimated      bool      `json:"tokens_estimated"`
	StartedAt            time.Time `json:"started_at"`
	CompletedAt          time.Time `json:"completed_at"`
	Status               string    `json:"status"`
	ErrorClass           string    `json:"error_class,omitempty"`
	HTTPStatus           int       `json:"http_status,omitempty"`
	LatencyMillis        int64     `json:"latency_millis"`
	RetryCount           int       `json:"retry_count"`
	FallbackCount        int       `json:"fallback_count"`
}

type RequestSummary struct {
	RequestID      string    `json:"request_id"`
	ProjectID      string    `json:"project_id"`
	KeyID          string    `json:"key_id,omitempty"`
	RequestedModel string    `json:"requested_model,omitempty"`
	Attempts       int64     `json:"attempts"`
	InputTokens    int64     `json:"input_tokens"`
	OutputTokens   int64     `json:"output_tokens"`
	CostMicrosUSD  int64     `json:"cost_micros_usd"`
	Fallbacks      int64     `json:"fallbacks"`
	Outcome        string    `json:"outcome"`
	AcceptedAt     time.Time `json:"accepted_at"`
	CompletedAt    time.Time `json:"completed_at"`
}

type Bucket struct {
	Hour                  time.Time `json:"hour"`
	Requests              int64     `json:"requests"`
	Attempts              int64     `json:"attempts"`
	InputTokens           int64     `json:"input_tokens"`
	OutputTokens          int64     `json:"output_tokens"`
	EstimatedInputTokens  int64     `json:"estimated_input_tokens,omitempty"`
	EstimatedOutputTokens int64     `json:"estimated_output_tokens,omitempty"`
	CostMicrosUSD         int64     `json:"cost_micros_usd"`
	Errors                int64     `json:"errors"`
	LatencyMillis         int64     `json:"latency_millis"`
}

type Snapshot struct {
	Watermark ledger.Watermark `json:"watermark"`
	Totals    Bucket           `json:"totals"`
	Hourly    []Bucket         `json:"hourly"`
	Attempts  []AttemptEvent   `json:"attempts"`
	Requests  []RequestSummary `json:"requests"`
}

type Metrics struct {
	RequestsSuccess      uint64
	RequestsError        uint64
	AttemptsSuccess      uint64
	AttemptsError        uint64
	InputTokens          uint64
	OutputTokens         uint64
	CostMicrosUSD        uint64
	AttemptLatencyMillis uint64
	RequestLatencyMillis uint64
	Fallbacks            uint64
	ActiveRequests       uint64
}

type requestAccumulator struct {
	summary RequestSummary
}

type checkpoint struct {
	Version   int                       `json:"version"`
	Watermark ledger.Watermark          `json:"watermark"`
	Started   map[string]time.Time      `json:"started"`
	Active    map[string]RequestSummary `json:"active_requests"`
	Attempts  []AttemptEvent            `json:"attempts"`
	Summaries []RequestSummary          `json:"request_summaries"`
	Hourly    map[int64]Bucket          `json:"hourly"`
	Totals    Bucket                    `json:"totals"`
	Metrics   Metrics                   `json:"metrics"`
}

type Aggregate struct {
	mu        sync.RWMutex
	watermark ledger.Watermark
	eventIDs  map[string]struct{}
	started   map[string]time.Time
	requests  map[string]*requestAccumulator
	attempts  []AttemptEvent
	summaries []RequestSummary
	hourly    map[int64]Bucket
	totals    Bucket
	metrics   Metrics
}

func NewAggregate() *Aggregate {
	return &Aggregate{
		eventIDs: make(map[string]struct{}), started: make(map[string]time.Time),
		requests: make(map[string]*requestAccumulator), hourly: make(map[int64]Bucket),
	}
}

func RestoreCheckpoint(payload []byte) (*Aggregate, error) {
	if len(payload) == 0 {
		return nil, errors.New("usage checkpoint is empty")
	}
	var saved checkpoint
	if err := json.Unmarshal(payload, &saved); err != nil {
		return nil, fmt.Errorf("decode usage checkpoint: %w", err)
	}
	if saved.Version != checkpointVersion {
		return nil, fmt.Errorf("usage checkpoint version %d is not supported", saved.Version)
	}
	if saved.Watermark.Sequence == 0 && (saved.Watermark.Offset != 0 || saved.Watermark.Generation != 0) {
		return nil, errors.New("usage checkpoint has an invalid empty watermark")
	}
	if saved.Watermark.Sequence > 0 && (saved.Watermark.Offset <= 0 || saved.Watermark.Generation != 1) {
		return nil, errors.New("usage checkpoint has an invalid watermark")
	}
	aggregate := NewAggregate()
	aggregate.watermark = saved.Watermark
	aggregate.started = cloneStarted(saved.Started)
	aggregate.attempts = append([]AttemptEvent(nil), saved.Attempts...)
	aggregate.summaries = append([]RequestSummary(nil), saved.Summaries...)
	aggregate.hourly = cloneHourly(saved.Hourly)
	aggregate.totals = saved.Totals
	aggregate.metrics = saved.Metrics
	for requestID, summary := range saved.Active {
		if requestID == "" || summary.RequestID != requestID {
			return nil, errors.New("usage checkpoint has an invalid active request")
		}
		aggregate.requests[requestID] = &requestAccumulator{summary: summary}
	}
	return aggregate, nil
}

func (a *Aggregate) MarshalCheckpoint() (ledger.Watermark, []byte, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	active := make(map[string]RequestSummary, len(a.requests))
	for requestID, accumulator := range a.requests {
		active[requestID] = accumulator.summary
	}
	saved := checkpoint{
		Version: checkpointVersion, Watermark: a.watermark,
		Started: cloneStarted(a.started), Active: active,
		Attempts:  append([]AttemptEvent(nil), a.attempts...),
		Summaries: append([]RequestSummary(nil), a.summaries...),
		Hourly:    cloneHourly(a.hourly), Totals: a.totals, Metrics: a.metrics,
	}
	payload, err := json.Marshal(saved)
	if err != nil {
		return ledger.Watermark{}, nil, fmt.Errorf("encode usage checkpoint: %w", err)
	}
	return a.watermark, payload, nil
}

func (a *Aggregate) Apply(record ledger.Record) error {
	if err := record.Event.Validate(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, exists := a.eventIDs[record.Event.EventID]; exists {
		return nil
	}
	if record.Sequence <= a.watermark.Sequence {
		return errors.New("usage record sequence is not monotonic")
	}
	event := record.Event
	accumulator := a.requests[event.RequestID]
	if accumulator == nil {
		accumulator = &requestAccumulator{summary: RequestSummary{
			RequestID: event.RequestID, ProjectID: event.ProjectID, KeyID: event.KeyID,
			RequestedModel: event.RequestedModel,
		}}
		a.requests[event.RequestID] = accumulator
	}
	switch event.Kind {
	case ledger.EventRequestAccepted:
		accumulator.summary.AcceptedAt = event.OccurredAt
	case ledger.EventAttemptStarted:
		a.started[event.AttemptID] = event.OccurredAt
	case ledger.EventAttemptSettled:
		startedAt := a.started[event.AttemptID]
		if startedAt.IsZero() {
			startedAt = event.OccurredAt.Add(-time.Duration(event.LatencyMillis) * time.Millisecond)
		}
		attempt := AttemptEvent{
			EventID: event.EventID, RequestID: event.RequestID, AttemptID: event.AttemptID,
			Sequence: record.Sequence, AttemptNumber: event.AttemptNumber,
			ProjectID: event.ProjectID, KeyID: event.KeyID, RouteID: event.RouteID,
			DeploymentID: event.DeploymentID,
			ProviderID:   event.ProviderID, RequestedModel: event.RequestedModel,
			ProviderModel: event.ProviderModel, ProviderInputTokens: event.ProviderInputTokens,
			ProviderOutputTokens: event.ProviderOutputTokens,
			PreparedOutputTokens: event.PreparedOutputTokens, CostMicrosUSD: event.CommittedMicrosUSD,
			CostEstimated: event.CostEstimated, TokensEstimated: event.TokenEstimated,
			StartedAt: startedAt, CompletedAt: event.OccurredAt, Status: event.Outcome,
			ErrorClass: event.ErrorClass, HTTPStatus: event.HTTPStatus,
			LatencyMillis: event.LatencyMillis, RetryCount: event.RetryCount,
			FallbackCount: event.FallbackCount,
		}
		a.attempts = append(a.attempts, attempt)
		accumulator.summary.Attempts++
		accumulator.summary.InputTokens += event.ProviderInputTokens
		accumulator.summary.OutputTokens += event.ProviderOutputTokens
		accumulator.summary.CostMicrosUSD += event.CommittedMicrosUSD
		if int64(event.FallbackCount) > accumulator.summary.Fallbacks {
			accumulator.summary.Fallbacks = int64(event.FallbackCount)
		}
		hour := event.OccurredAt.UTC().Truncate(time.Hour)
		bucket := a.hourly[hour.Unix()]
		bucket.Hour = hour
		bucket.Attempts++
		bucket.InputTokens += event.ProviderInputTokens
		bucket.OutputTokens += event.ProviderOutputTokens
		bucket.CostMicrosUSD += event.CommittedMicrosUSD
		bucket.LatencyMillis += event.LatencyMillis
		if event.Outcome != "success" {
			bucket.Errors++
		}
		a.hourly[hour.Unix()] = bucket
		a.totals.Attempts++
		a.totals.InputTokens += event.ProviderInputTokens
		a.totals.OutputTokens += event.ProviderOutputTokens
		a.totals.CostMicrosUSD += event.CommittedMicrosUSD
		a.totals.LatencyMillis += event.LatencyMillis
		if event.Outcome != "success" {
			a.totals.Errors++
			a.metrics.AttemptsError++
		} else {
			a.metrics.AttemptsSuccess++
		}
		a.metrics.InputTokens += uint64(event.ProviderInputTokens)
		a.metrics.OutputTokens += uint64(event.ProviderOutputTokens)
		a.metrics.CostMicrosUSD += uint64(event.CommittedMicrosUSD)
		a.metrics.AttemptLatencyMillis += uint64(event.LatencyMillis)
		delete(a.started, event.AttemptID)
	case ledger.EventRequestFinalized:
		accumulator.summary.Outcome = event.Outcome
		accumulator.summary.CompletedAt = event.OccurredAt
		a.summaries = append(a.summaries, accumulator.summary)
		delete(a.requests, event.RequestID)
		hour := event.OccurredAt.UTC().Truncate(time.Hour)
		bucket := a.hourly[hour.Unix()]
		bucket.Hour = hour
		bucket.Requests++
		a.hourly[hour.Unix()] = bucket
		a.totals.Requests++
		if event.Outcome == "success" {
			a.metrics.RequestsSuccess++
		} else {
			a.metrics.RequestsError++
		}
		if !accumulator.summary.AcceptedAt.IsZero() &&
			event.OccurredAt.After(accumulator.summary.AcceptedAt) {
			a.metrics.RequestLatencyMillis += uint64(event.OccurredAt.Sub(
				accumulator.summary.AcceptedAt,
			).Milliseconds())
		}
		a.metrics.Fallbacks += uint64(accumulator.summary.Fallbacks)
	}
	a.eventIDs[event.EventID] = struct{}{}
	a.watermark = ledger.Watermark{Generation: 1, Offset: record.Offset, Sequence: record.Sequence}
	return nil
}

func (a *Aggregate) Metrics() Metrics {
	a.mu.RLock()
	defer a.mu.RUnlock()
	result := a.metrics
	result.ActiveRequests = uint64(len(a.requests))
	return result
}

func (a *Aggregate) Watermark() ledger.Watermark {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.watermark
}

func (a *Aggregate) Snapshot() Snapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()
	result := Snapshot{
		Watermark: a.watermark, Totals: a.totals,
		Attempts: append([]AttemptEvent(nil), a.attempts...),
		Requests: append([]RequestSummary(nil), a.summaries...),
		Hourly:   make([]Bucket, 0, len(a.hourly)),
	}
	for _, bucket := range a.hourly {
		result.Hourly = append(result.Hourly, bucket)
	}
	sort.Slice(result.Hourly, func(i, j int) bool {
		return result.Hourly[i].Hour.Before(result.Hourly[j].Hour)
	})
	return result
}

func cloneStarted(source map[string]time.Time) map[string]time.Time {
	result := make(map[string]time.Time, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneHourly(source map[int64]Bucket) map[int64]Bucket {
	result := make(map[int64]Bucket, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
