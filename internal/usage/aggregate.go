package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

// maxTrackedEventIDs bounds the dedup index, mirroring the audit log's. A
// ledger event ID only repeats within a short window of its first append — the
// crash-recovery path deliberately re-emits a deterministic event rather than
// inventing a new one — so a tail window answers every real duplicate, while
// retaining every ID ever seen would grow with the lifetime of the process and
// still be absent after a restart.
const maxTrackedEventIDs = 4096

// Version 10 carries the upstream's own identifiers for a failed attempt — the
// provider code, the provider request ID, and the phase the failure happened in
// — so a support ticket raised days after the fact can still name the request
// the upstream saw. Version 9 stamped each request summary with the ledger
// sequence that finalized it, so the failed-request list can page the way the
// attempt list does: a cursor over a slice position would not survive a
// restart, and one over the completion instant cannot separate two requests
// that finished in the same millisecond. Version 8 recorded the accounting period on attempts and request
// summaries — without it the aggregate could say when a call finished but not
// which accounting day it was charged to, and the daily rollup, which keys on
// the period stamped at admission, had nothing to key on. Version 7 persisted
// the dedup window; version 6 dropped the duplicate cost columns. A checkpoint
// written before this is refused rather than migrated: it is a derivative, and
// rebuilding it from the Ledger is cheap.
const checkpointVersion = 10

const latencyBucketCount = 12

var LatencyBucketsMillis = [latencyBucketCount]uint64{
	10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 120000,
}

type AttemptEvent struct {
	EventID       string `json:"event_id"`
	RequestID     string `json:"request_id"`
	AttemptID     string `json:"attempt_id"`
	Sequence      uint64 `json:"sequence"`
	AttemptNumber int    `json:"attempt"`
	ProjectID     string `json:"project_id"`
	KeyID         string `json:"key_id,omitempty"`
	RouteID       string `json:"route_id,omitempty"`
	DeploymentID  string `json:"deployment_id,omitempty"`
	// The accounting day this attempt was charged to, decided when its request
	// was admitted and inherited from there. It is not the completion instant
	// truncated to a date: a request accepted at 23:59 and settled at 00:02
	// belongs to the day it was admitted on, and every view has to agree on
	// that or the same call lands in two different days.
	PeriodID              string `json:"period_id"`
	PeriodTimezoneVersion uint64 `json:"period_timezone_version,omitempty"`
	ProviderID            string `json:"provider_id,omitempty"`
	RequestedModel        string `json:"requested_model,omitempty"`
	ProviderModel         string `json:"provider_model,omitempty"`
	ProviderInputTokens   int64  `json:"provider_input_tokens"`
	ProviderOutputTokens  int64  `json:"provider_output_tokens"`
	// Breakdown subsets of the two totals above, not additions to them.
	ProviderCachedInputTokens     int64                      `json:"provider_cached_input_tokens,omitempty"`
	ProviderCacheWriteInputTokens int64                      `json:"provider_cache_write_input_tokens,omitempty"`
	ProviderReasoningTokens       int64                      `json:"provider_reasoning_tokens,omitempty"`
	PreparedOutputTokens          int64                      `json:"prepared_output_tokens"`
	CostMicrosUSD                 *int64                     `json:"cost_micros_usd"`
	LeaseMode                     ledger.LeaseMode           `json:"lease_mode,omitempty"`
	PriceEvidenceStatus           domain.PriceEvidenceStatus `json:"price_evidence_status"`
	CostValueStatus               domain.CostValueStatus     `json:"cost_value_status"`
	PriceSnapshot                 *domain.PriceSnapshot      `json:"price_snapshot,omitempty"`
	InputCostMicrosUSD            *int64                     `json:"input_cost_micros_usd"`
	OutputCostMicrosUSD           *int64                     `json:"output_cost_micros_usd"`
	FixedCostMicrosUSD            *int64                     `json:"fixed_cost_micros_usd"`
	Tags                          []string                   `json:"tags,omitempty"`
	CostEstimated                 bool                       `json:"cost_estimated"`
	TokensEstimated               bool                       `json:"tokens_estimated"`
	TokenUsageSource              ledger.TokenUsageSource    `json:"token_usage_source,omitempty"`
	StartedAt                     time.Time                  `json:"started_at"`
	CompletedAt                   time.Time                  `json:"completed_at"`
	Status                        string                     `json:"status"`
	ErrorClass                    string                     `json:"error_class,omitempty"`
	HTTPStatus                    int                        `json:"http_status,omitempty"`
	// The upstream's own identifiers for this failure, and where along the
	// request it happened. Absent on every attempt recorded before they were
	// carried, which the console renders as "this record predates the field"
	// rather than as an invented "unknown" — a value nobody could act on
	// looks exactly like an upstream that named none.
	ProviderCode      string `json:"provider_code,omitempty"`
	ProviderRequestID string `json:"provider_request_id,omitempty"`
	FailurePhase      string `json:"failure_phase,omitempty"`
	LatencyMillis     int64  `json:"latency_millis"`
	RetryCount        int    `json:"retry_count"`
	FallbackCount     int    `json:"fallback_count"`
}

func (a AttemptEvent) KnownCostMicrosUSD() (int64, bool) {
	if a.CostMicrosUSD != nil {
		return *a.CostMicrosUSD, true
	}
	return 0, false
}

type RequestSummary struct {
	RequestID      string `json:"request_id"`
	ProjectID      string `json:"project_id"`
	KeyID          string `json:"key_id,omitempty"`
	RequestedModel string `json:"requested_model,omitempty"`
	// Sequence of the RequestFinalized event, and therefore this summary's
	// position in the ledger's total order. It is what the failed-request list
	// pages on: an index into the in-memory slice would not survive a restart,
	// and CompletedAt cannot separate two requests that ended in the same
	// millisecond. Zero while the request is still in flight.
	Sequence uint64 `json:"sequence,omitempty"`
	// The accounting day the request was admitted into; see AttemptEvent.
	PeriodID              string    `json:"period_id"`
	PeriodTimezoneVersion uint64    `json:"period_timezone_version,omitempty"`
	Attempts              int64     `json:"attempts"`
	InputTokens           int64     `json:"input_tokens"`
	OutputTokens          int64     `json:"output_tokens"`
	CostMicrosUSD         int64     `json:"cost_micros_usd"`
	UnknownAttempts       int64     `json:"unknown_attempts"`
	Fallbacks             int64     `json:"fallbacks"`
	Outcome               string    `json:"outcome"`
	AcceptedAt            time.Time `json:"accepted_at"`
	CompletedAt           time.Time `json:"completed_at"`
}

type Bucket struct {
	Hour                    time.Time `json:"hour"`
	Requests                int64     `json:"requests"`
	RequestErrors           int64     `json:"request_errors"`
	RequestLatencySamples   int64     `json:"request_latency_samples"`
	RequestLatencyP50Millis int64     `json:"request_latency_p50_millis"`
	RequestLatencyP95Millis int64     `json:"request_latency_p95_millis"`
	Attempts                int64     `json:"attempts"`
	InputTokens             int64     `json:"input_tokens"`
	OutputTokens            int64     `json:"output_tokens"`
	EstimatedInputTokens    int64     `json:"estimated_input_tokens,omitempty"`
	EstimatedOutputTokens   int64     `json:"estimated_output_tokens,omitempty"`
	CostMicrosUSD           int64     `json:"cost_micros_usd"`
	EstimatedCostMicrosUSD  int64     `json:"estimated_cost_micros_usd,omitempty"`
	UnknownAttempts         int64     `json:"unknown_attempts"`
	Errors                  int64     `json:"errors"`
	LatencyMillis           int64     `json:"latency_millis"`
}

type Snapshot struct {
	Watermark ledger.Watermark `json:"watermark"`
	Totals    Bucket           `json:"totals"`
	Hourly    []Bucket         `json:"hourly"`
	Attempts  []AttemptEvent   `json:"attempts"`
	Requests  []RequestSummary `json:"requests"`
}

type Metrics struct {
	RequestsSuccess       uint64
	RequestsError         uint64
	AttemptsSuccess       uint64
	AttemptsError         uint64
	InputTokens           uint64
	OutputTokens          uint64
	CostMicrosUSD         uint64
	AttemptLatencyMillis  uint64
	RequestLatencyMillis  uint64
	Fallbacks             uint64
	ActiveRequests        uint64
	AttemptLatencyBuckets [latencyBucketCount]uint64
	RequestLatencyBuckets [latencyBucketCount]uint64
	UnknownAttempts       uint64
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
	// EventIDs is the dedup window. Without it a checkpoint taken between the
	// two physical frames of a re-emitted event resumed with an empty index and
	// counted the second copy again — the aggregate then disagreed with a full
	// replay of the same WAL.
	EventIDs []string `json:"event_ids,omitempty"`
}

type Aggregate struct {
	mu           sync.RWMutex
	watermark    ledger.Watermark
	eventIDs     map[string]struct{}
	eventIDOrder []string
	started      map[string]time.Time
	requests     map[string]*requestAccumulator
	attempts     []AttemptEvent
	summaries    []RequestSummary
	hourly       map[int64]Bucket
	// rollupDelta is the daily-rollup increment that has not been persisted
	// yet. It lives here, behind the same lock as the watermark, because Apply
	// is the only place that knows an event was neither a duplicate nor a
	// replay — the Collector cannot tell, and startup replay and CatchUp both
	// bypass it entirely.
	rollupDelta  map[domain.RollupKey]*domain.DailyRollup
	attemptIndex map[string]int
	summaryIndex map[string]int
	totals       Bucket
	metrics      Metrics
}

func NewAggregate() *Aggregate {
	return &Aggregate{
		eventIDs: make(map[string]struct{}), started: make(map[string]time.Time),
		requests: make(map[string]*requestAccumulator), hourly: make(map[int64]Bucket),
		rollupDelta:  make(map[domain.RollupKey]*domain.DailyRollup),
		attemptIndex: make(map[string]int), summaryIndex: make(map[string]int),
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
	for index, attempt := range aggregate.attempts {
		aggregate.attemptIndex[attempt.AttemptID] = index
	}
	for index, summary := range aggregate.summaries {
		aggregate.summaryIndex[summary.RequestID] = index
	}
	for _, eventID := range saved.EventIDs {
		aggregate.rememberEventID(eventID)
	}
	for requestID, summary := range saved.Active {
		if requestID == "" || summary.RequestID != requestID {
			return nil, errors.New("usage checkpoint has an invalid active request")
		}
		aggregate.requests[requestID] = &requestAccumulator{summary: summary}
	}
	return aggregate, nil
}

// TakeCheckpoint captures the aggregate's state and drains the pending rollup
// increment in one critical section, so the two describe exactly the same
// prefix of the Ledger. The caller writes both in a single transaction and, if
// that write fails, must hand the increment back with ReturnCheckpoint.
func (a *Aggregate) TakeCheckpoint() (CheckpointSnapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
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
		EventIDs: append([]string(nil), a.eventIDOrder...),
	}
	payload, err := json.Marshal(saved)
	if err != nil {
		return CheckpointSnapshot{}, fmt.Errorf("encode usage checkpoint: %w", err)
	}
	return CheckpointSnapshot{Watermark: a.watermark, Payload: payload, Rollup: a.takeRollupDelta()}, nil
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
			RequestedModel:        event.RequestedModel,
			PeriodID:              event.PeriodID,
			PeriodTimezoneVersion: event.PeriodTimezoneVersion,
		}}
		a.requests[event.RequestID] = accumulator
	}
	switch event.Kind {
	case ledger.EventRequestAccepted:
		accumulator.summary.AcceptedAt = event.OccurredAt
	case ledger.EventAttemptStarted:
		a.started[event.AttemptID] = event.OccurredAt
	case ledger.EventAttemptSettled:
		committedCost, costKnown := event.KnownCommittedMicrosUSD()
		var committedCostValue *int64
		if costKnown {
			value := committedCost
			committedCostValue = &value
		}
		evidenceStatus, costStatus := domain.PriceEvidenceLegacyUnversioned, domain.CostValueKnown
		var priceSnapshot *domain.PriceSnapshot
		var inputCost, outputCost, fixedCost *int64
		if event.PriceSnapshot != nil {
			copy := event.PriceSnapshot.Clone()
			priceSnapshot = &copy
			evidenceStatus, costStatus = copy.PriceEvidenceStatus, copy.CostValueStatus
			if copy.CostValueStatus == domain.CostValueKnown {
				inputValue, outputValue, fixedValue := event.InputCostMicrosUSD, event.OutputCostMicrosUSD, event.FixedCostMicrosUSD
				inputCost, outputCost, fixedCost = &inputValue, &outputValue, &fixedValue
			}
		}
		tags := attemptCostTags(event, evidenceStatus)
		startedAt := a.started[event.AttemptID]
		if startedAt.IsZero() {
			startedAt = event.OccurredAt.Add(-time.Duration(event.LatencyMillis) * time.Millisecond)
		}
		attempt := AttemptEvent{
			EventID: event.EventID, RequestID: event.RequestID, AttemptID: event.AttemptID,
			Sequence: record.Sequence, AttemptNumber: event.AttemptNumber,
			ProjectID: event.ProjectID, KeyID: event.KeyID, RouteID: event.RouteID,
			DeploymentID:          event.DeploymentID,
			PeriodID:              event.PeriodID,
			PeriodTimezoneVersion: event.PeriodTimezoneVersion,
			ProviderID:            event.ProviderID, RequestedModel: event.RequestedModel,
			ProviderModel: event.ProviderModel, ProviderInputTokens: event.ProviderInputTokens,
			ProviderOutputTokens:          event.ProviderOutputTokens,
			ProviderCachedInputTokens:     event.ProviderCachedInputTokens,
			ProviderCacheWriteInputTokens: event.ProviderCacheWriteInputTokens,
			ProviderReasoningTokens:       event.ProviderReasoningTokens,
			PreparedOutputTokens:          event.PreparedOutputTokens, CostMicrosUSD: committedCostValue,
			LeaseMode: event.LeaseMode, PriceEvidenceStatus: evidenceStatus, CostValueStatus: costStatus,
			PriceSnapshot: priceSnapshot, InputCostMicrosUSD: inputCost, OutputCostMicrosUSD: outputCost, FixedCostMicrosUSD: fixedCost,
			Tags:          tags,
			CostEstimated: event.CostEstimated, TokensEstimated: event.TokenEstimated,
			TokenUsageSource: event.TokenUsageSource,
			StartedAt:        startedAt, CompletedAt: event.OccurredAt, Status: event.Outcome,
			ErrorClass: event.ErrorClass, HTTPStatus: event.HTTPStatus,
			ProviderCode: event.ProviderCode, ProviderRequestID: event.ProviderRequestID,
			FailurePhase:  event.FailurePhase,
			LatencyMillis: event.LatencyMillis, RetryCount: event.RetryCount,
			FallbackCount: event.FallbackCount,
		}
		a.attempts = append(a.attempts, attempt)
		a.attemptIndex[event.AttemptID] = len(a.attempts) - 1
		if err := addInt64(&accumulator.summary.Attempts, 1); err != nil {
			return err
		}
		if err := addInt64(&accumulator.summary.InputTokens, event.ProviderInputTokens); err != nil {
			return err
		}
		if err := addInt64(&accumulator.summary.OutputTokens, event.ProviderOutputTokens); err != nil {
			return err
		}
		if err := addInt64(&accumulator.summary.CostMicrosUSD, committedCost); err != nil {
			return err
		}
		if !costKnown {
			if err := addInt64(&accumulator.summary.UnknownAttempts, 1); err != nil {
				return err
			}
		}
		if int64(event.FallbackCount) > accumulator.summary.Fallbacks {
			accumulator.summary.Fallbacks = int64(event.FallbackCount)
		}
		hour := event.OccurredAt.UTC().Truncate(time.Hour)
		bucket := a.hourly[hour.Unix()]
		bucket.Hour = hour
		if err := addInt64(&bucket.Attempts, 1); err != nil {
			return err
		}
		if err := addInt64(&bucket.InputTokens, event.ProviderInputTokens); err != nil {
			return err
		}
		if err := addInt64(&bucket.OutputTokens, event.ProviderOutputTokens); err != nil {
			return err
		}
		if err := addInt64(&bucket.CostMicrosUSD, committedCost); err != nil {
			return err
		}
		if !costKnown {
			if err := addInt64(&bucket.UnknownAttempts, 1); err != nil {
				return err
			}
		}
		if err := addInt64(&bucket.LatencyMillis, event.LatencyMillis); err != nil {
			return err
		}
		if event.Outcome != "success" {
			if err := addInt64(&bucket.Errors, 1); err != nil {
				return err
			}
		}
		a.hourly[hour.Unix()] = bucket
		if err := addInt64(&a.totals.Attempts, 1); err != nil {
			return err
		}
		if err := addInt64(&a.totals.InputTokens, event.ProviderInputTokens); err != nil {
			return err
		}
		if err := addInt64(&a.totals.OutputTokens, event.ProviderOutputTokens); err != nil {
			return err
		}
		if err := addInt64(&a.totals.CostMicrosUSD, committedCost); err != nil {
			return err
		}
		if !costKnown {
			if err := addInt64(&a.totals.UnknownAttempts, 1); err != nil {
				return err
			}
			a.metrics.UnknownAttempts++
		}
		if err := addInt64(&a.totals.LatencyMillis, event.LatencyMillis); err != nil {
			return err
		}
		if event.Outcome != "success" {
			if err := addInt64(&a.totals.Errors, 1); err != nil {
				return err
			}
			a.metrics.AttemptsError++
		} else {
			a.metrics.AttemptsSuccess++
		}
		if err := addUint64(&a.metrics.InputTokens, uint64(event.ProviderInputTokens)); err != nil {
			return err
		}
		if err := addUint64(&a.metrics.OutputTokens, uint64(event.ProviderOutputTokens)); err != nil {
			return err
		}
		if err := addUint64(&a.metrics.CostMicrosUSD, uint64(committedCost)); err != nil {
			return err
		}
		if err := addUint64(&a.metrics.AttemptLatencyMillis, uint64(event.LatencyMillis)); err != nil {
			return err
		}
		recordLatency(&a.metrics.AttemptLatencyBuckets, uint64(event.LatencyMillis))
		if err := a.addAttemptRollup(event, record.Sequence, committedCost, costKnown); err != nil {
			return err
		}
		delete(a.started, event.AttemptID)
	case ledger.EventRequestFinalized:
		accumulator.summary.Outcome = event.Outcome
		accumulator.summary.CompletedAt = event.OccurredAt
		accumulator.summary.Sequence = record.Sequence
		a.summaries = append(a.summaries, accumulator.summary)
		a.summaryIndex[event.RequestID] = len(a.summaries) - 1
		delete(a.requests, event.RequestID)
		hour := event.OccurredAt.UTC().Truncate(time.Hour)
		bucket := a.hourly[hour.Unix()]
		bucket.Hour = hour
		if err := addInt64(&bucket.Requests, 1); err != nil {
			return err
		}
		a.hourly[hour.Unix()] = bucket
		if err := addInt64(&a.totals.Requests, 1); err != nil {
			return err
		}
		if event.Outcome == "success" {
			a.metrics.RequestsSuccess++
		} else {
			a.metrics.RequestsError++
		}
		var latencyMillis uint64
		latencyKnown := !accumulator.summary.AcceptedAt.IsZero() &&
			event.OccurredAt.After(accumulator.summary.AcceptedAt)
		if latencyKnown {
			latencyMillis = uint64(event.OccurredAt.Sub(
				accumulator.summary.AcceptedAt,
			).Milliseconds())
			if err := addUint64(&a.metrics.RequestLatencyMillis, latencyMillis); err != nil {
				return err
			}
			recordLatency(&a.metrics.RequestLatencyBuckets, latencyMillis)
		}
		if err := a.addRequestRollup(event, record.Sequence, latencyMillis, latencyKnown); err != nil {
			return err
		}
		if err := addUint64(&a.metrics.Fallbacks, uint64(accumulator.summary.Fallbacks)); err != nil {
			return err
		}
	}
	a.rememberEventID(event.EventID)
	a.watermark = ledger.Watermark{Generation: 1, Offset: record.Offset, Sequence: record.Sequence}
	return nil
}

func (a *Aggregate) rememberEventID(eventID string) {
	if _, exists := a.eventIDs[eventID]; exists {
		return
	}
	a.eventIDs[eventID] = struct{}{}
	a.eventIDOrder = append(a.eventIDOrder, eventID)
	if len(a.eventIDOrder) > maxTrackedEventIDs {
		delete(a.eventIDs, a.eventIDOrder[0])
		a.eventIDOrder = a.eventIDOrder[1:]
	}
}

func addInt64(target *int64, delta int64) error {
	if delta > 0 && *target > math.MaxInt64-delta {
		return errors.New("usage aggregate integer overflow")
	}
	if delta < 0 && *target < math.MinInt64-delta {
		return errors.New("usage aggregate integer underflow")
	}
	*target += delta
	return nil
}

func addUint64(target *uint64, delta uint64) error {
	if *target > math.MaxUint64-delta {
		return errors.New("usage aggregate unsigned integer overflow")
	}
	*target += delta
	return nil
}

func recordLatency(buckets *[latencyBucketCount]uint64, latencyMillis uint64) {
	for index, upperBound := range LatencyBucketsMillis {
		if latencyMillis <= upperBound {
			buckets[index]++
			return
		}
	}
}

func attemptCostTags(event ledger.Event, evidence domain.PriceEvidenceStatus) []string {
	var tags []string
	if event.TokenEstimated || event.CostEstimated {
		tags = append(tags, "EST")
	}
	switch evidence {
	case domain.PriceEvidenceUnknown:
		tags = append(tags, "UNKNOWN")
	case domain.PriceEvidenceLegacyUnversioned:
		tags = append(tags, "LEGACY")
	case domain.PriceEvidenceVersioned:
		if event.PriceSnapshot != nil && event.PriceSnapshot.BillingMode == domain.BillingModeFree {
			tags = append(tags, "FREE")
		}
	}
	return tags
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

func containsTag(tags []string, wanted string) bool {
	for _, tag := range tags {
		if tag == wanted {
			return true
		}
	}
	return false
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
