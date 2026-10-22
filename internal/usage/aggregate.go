package usage

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/provider"
)

// maxTrackedEventIDs bounds the dedup index, mirroring the audit log's. A
// ledger event ID only repeats within a short window of its first append — the
// crash-recovery path deliberately re-emits a deterministic event rather than
// inventing a new one — so a tail window answers every real duplicate, while
// retaining every ID ever seen would grow with the lifetime of the process and
// still be absent after a restart.
const maxTrackedEventIDs = 4096

const latencyBucketCount = 18

// LatencyBucketsMillis are the upper bounds a latency histogram counts into.
//
// The three above 30s are there because the shape below them was useless for
// the question they answer. The ladder used to step 30000 → 120000, one bucket
// covering a minute and a half, which is exactly the range a request spends
// when this instance's own deadlines are the thing cutting it: the shipped
// attempt_response_header_timeout is 1m0s and the shipped route_total_timeout
// is 2m0s. "Is 90 seconds enough for this deployment?" could not be answered —
// 55s and 90s landed in the same bucket — so during a triage the per-attempt
// latency column had to be read by eye instead. 60000 is the boundary that
// separates "under the attempt deadline" from "at it", and 90000 separates that
// from the request deadline; 45000 keeps the step from 30000 comparable to the
// rest of the ladder.
//
// The three above 120s follow the deadlines up. A reasoning model sends no
// header until it has finished reasoning, so attempt_response_header_timeout
// became 5m and route_total_timeout 10m; with the ladder ending at 120000,
// everything between two minutes and the request deadline fell into the
// overflow, the same blindness the 30s–120s bucket used to cause. 300000 is
// the header deadline, 600000 the request deadline, 180000 keeps the step
// from 120000 comparable.
//
// Changing this changes what a stored histogram means, so the derivatives
// that carry one are versioned past it: the usage history (historyFormatVersion,
// which holds the request rows and the daily rollups) and domain.RollupVersion.
// An existing instance rebuilds them from the Ledger, which is the authority;
// a Prometheus server keeps the old `le` series and starts a new set, the
// ordinary consequence of re-cutting a histogram.
var LatencyBucketsMillis = [latencyBucketCount]uint64{
	10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 45000, 60000, 90000, 120000,
	180000, 300000, 600000,
}

type AttemptEvent struct {
	EventID       string `json:"event_id"`
	RequestID     string `json:"request_id"`
	AttemptID     string `json:"attempt_id"`
	Sequence      uint64 `json:"sequence"`
	AttemptNumber int    `json:"attempt"`
	ProjectID     string `json:"project_id"`
	WorkUnitID    string `json:"work_unit_id,omitempty"`
	RunID         string `json:"run_id,omitempty"`
	KeyID         string `json:"key_id,omitempty"`
	RouteID       string `json:"route_id,omitempty"`
	DeploymentID  string `json:"deployment_id,omitempty"`
	// The accounting day this attempt was charged to, decided when its request
	// was admitted and inherited from there. It is not the completion instant
	// truncated to a date: a request accepted at 23:59 and settled at 00:02
	// belongs to the day it was admitted on, and every view has to agree on
	// that or the same call lands in two different days.
	PeriodID              string                    `json:"period_id"`
	PeriodTimezoneVersion uint64                    `json:"period_timezone_version,omitempty"`
	ProviderID            string                    `json:"provider_id,omitempty"`
	OfferingID            domain.ProviderOfferingID `json:"offering_id,omitempty"`
	ProfileID             domain.ProviderProfileID  `json:"profile_id,omitempty"`
	AccountRegionID       domain.ProviderRegionID   `json:"account_region_id,omitempty"`
	RequestedModel        string                    `json:"requested_model,omitempty"`
	ProviderModel         string                    `json:"provider_model,omitempty"`
	ProviderInputTokens   int64                     `json:"provider_input_tokens"`
	ProviderOutputTokens  int64                     `json:"provider_output_tokens"`
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
	ProviderCode             string                 `json:"provider_code,omitempty"`
	ProviderFailureReason    provider.FailureReason `json:"provider_failure_reason,omitempty"`
	ProviderRequestID        string                 `json:"provider_request_id,omitempty"`
	FailurePhase             string                 `json:"failure_phase,omitempty"`
	Retryable                bool                   `json:"retryable,omitempty"`
	Ambiguous                bool                   `json:"ambiguous,omitempty"`
	FailureSemanticsRecorded bool                   `json:"failure_semantics_recorded,omitempty"`
	LatencyMillis            int64                  `json:"latency_millis"`
	RetryCount               int                    `json:"retry_count"`
	FallbackCount            int                    `json:"fallback_count"`
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
	WorkUnitID     string `json:"work_unit_id,omitempty"`
	RunID          string `json:"run_id,omitempty"`
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

// Aggregate is the bounded, in-memory side of Usage: requests still in flight,
// the running counters, and the increment that has not reached history.db
// yet. Everything that grows with history — settled attempts, finished request
// summaries, hourly and daily rollups — is written to the history index in
// batches and read back from there. Memory therefore depends on how many
// requests are in flight and how much arrives between two flushes, not on how
// long the instance has been running.
type Aggregate struct {
	history *HistoryStore
	// flushMu serializes Flush, so batches reach the store in ledger order.
	flushMu sync.Mutex

	mu        sync.RWMutex
	watermark ledger.Watermark
	// committed is the watermark of the last batch the store accepted.
	committed    ledger.Watermark
	eventIDs     map[string]struct{}
	eventIDOrder []string
	started      map[string]time.Time
	requests     map[string]*pendingRequest
	totals       Bucket
	metrics      Metrics

	// The increment since the last flush. dirtyRequests and dirtyStarted name
	// the in-flight entries whose stored copy is stale; the flush writes their
	// current value, or deletes them if they have finished.
	batchAttempts  []AttemptEvent
	batchSummaries []RequestSummary
	hourly         map[int64]hourlyRow
	rollupDelta    map[domain.RollupKey]*domain.DailyRollup
	successes      map[string]time.Time
	dirtyRequests  map[string]struct{}
	dirtyStarted   map[string]struct{}
}

// NewAggregate starts an empty aggregate over an empty history store.
func NewAggregate(history *HistoryStore) *Aggregate {
	return &Aggregate{
		history:  history,
		eventIDs: make(map[string]struct{}), started: make(map[string]time.Time),
		requests: make(map[string]*pendingRequest), hourly: make(map[int64]hourlyRow),
		rollupDelta:   make(map[domain.RollupKey]*domain.DailyRollup),
		successes:     make(map[string]time.Time),
		dirtyRequests: make(map[string]struct{}), dirtyStarted: make(map[string]struct{}),
	}
}

// RestoreAggregate resumes from what history.db holds: its watermark, counters
// and in-flight state. The caller replays the Ledger from Watermark() on.
func RestoreAggregate(history *HistoryStore) (*Aggregate, error) {
	aggregate := NewAggregate(history)
	state, pending, started, err := history.loadState()
	if errors.Is(err, errHistoryStateMissing) {
		return aggregate, nil
	}
	if err != nil {
		return nil, err
	}
	aggregate.watermark, aggregate.totals, aggregate.metrics = state.Watermark, state.Totals, state.Metrics
	aggregate.committed = state.Watermark
	for _, eventID := range state.EventIDs {
		aggregate.rememberEventID(eventID)
	}
	for requestID, request := range pending {
		request := request
		aggregate.requests[requestID] = &request
	}
	aggregate.started = started
	return aggregate, nil
}

// History is the store this aggregate writes to, for readers.
func (a *Aggregate) History() *HistoryStore {
	return a.history
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
	if event.Kind >= ledger.EventWorkUnitCreated {
		a.rememberEventID(event.EventID)
		a.watermark = ledger.Watermark{Generation: record.Generation, Offset: record.Offset, Sequence: record.Sequence}
		return nil
	}
	accumulator := a.requests[event.RequestID]
	if accumulator == nil {
		accumulator = &pendingRequest{Summary: RequestSummary{
			RequestID: event.RequestID, ProjectID: event.ProjectID, KeyID: event.KeyID,
			WorkUnitID: event.WorkUnitID, RunID: event.RunID,
			RequestedModel:        event.RequestedModel,
			PeriodID:              event.PeriodID,
			PeriodTimezoneVersion: event.PeriodTimezoneVersion,
		}}
		a.requests[event.RequestID] = accumulator
	}
	a.dirtyRequests[event.RequestID] = struct{}{}
	switch event.Kind {
	case ledger.EventRequestAccepted:
		accumulator.Summary.AcceptedAt = event.OccurredAt
	case ledger.EventAttemptStarted:
		a.started[event.AttemptID] = event.OccurredAt
		a.dirtyStarted[event.AttemptID] = struct{}{}
	case ledger.EventAttemptSettled:
		if err := a.applySettled(record, accumulator); err != nil {
			return err
		}
	case ledger.EventRequestFinalized:
		if err := a.applyFinalized(record, accumulator); err != nil {
			return err
		}
	}
	a.rememberEventID(event.EventID)
	a.watermark = ledger.Watermark{Generation: record.Generation, Offset: record.Offset, Sequence: record.Sequence}
	return nil
}

func (a *Aggregate) applySettled(record ledger.Record, accumulator *pendingRequest) error {
	event := record.Event
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
	startedAt := a.started[event.AttemptID]
	if startedAt.IsZero() {
		startedAt = event.OccurredAt.Add(-time.Duration(event.LatencyMillis) * time.Millisecond)
	}
	attempt := AttemptEvent{
		EventID: event.EventID, RequestID: event.RequestID, AttemptID: event.AttemptID,
		Sequence: record.Sequence, AttemptNumber: event.AttemptNumber,
		ProjectID: event.ProjectID, KeyID: event.KeyID, RouteID: event.RouteID,
		WorkUnitID: event.WorkUnitID, RunID: event.RunID,
		DeploymentID:          event.DeploymentID,
		PeriodID:              event.PeriodID,
		PeriodTimezoneVersion: event.PeriodTimezoneVersion,
		ProviderID:            event.ProviderID, RequestedModel: event.RequestedModel,
		OfferingID: event.OfferingID, ProfileID: event.ProfileID, AccountRegionID: event.AccountRegionID,
		ProviderModel: event.ProviderModel, ProviderInputTokens: event.ProviderInputTokens,
		ProviderOutputTokens:          event.ProviderOutputTokens,
		ProviderCachedInputTokens:     event.ProviderCachedInputTokens,
		ProviderCacheWriteInputTokens: event.ProviderCacheWriteInputTokens,
		ProviderReasoningTokens:       event.ProviderReasoningTokens,
		PreparedOutputTokens:          event.PreparedOutputTokens, CostMicrosUSD: committedCostValue,
		LeaseMode: event.LeaseMode, PriceEvidenceStatus: evidenceStatus, CostValueStatus: costStatus,
		PriceSnapshot: priceSnapshot, InputCostMicrosUSD: inputCost, OutputCostMicrosUSD: outputCost, FixedCostMicrosUSD: fixedCost,
		Tags:          attemptCostTags(event, evidenceStatus),
		CostEstimated: event.CostEstimated, TokensEstimated: event.TokenEstimated,
		TokenUsageSource: event.TokenUsageSource,
		StartedAt:        startedAt, CompletedAt: event.OccurredAt, Status: event.Outcome,
		ErrorClass: event.ErrorClass, HTTPStatus: event.HTTPStatus,
		ProviderCode: event.ProviderCode, ProviderFailureReason: event.ProviderFailureReason,
		ProviderRequestID: event.ProviderRequestID, FailurePhase: event.FailurePhase,
		Retryable: event.Retryable, Ambiguous: event.Ambiguous,
		FailureSemanticsRecorded: event.FailureSemanticsRecorded,
		LatencyMillis:            event.LatencyMillis, RetryCount: event.RetryCount,
		FallbackCount: event.FallbackCount,
	}
	a.batchAttempts = append(a.batchAttempts, attempt)
	summary := &accumulator.Summary
	if err := addInt64(&summary.Attempts, 1); err != nil {
		return err
	}
	if err := addInt64(&summary.InputTokens, event.ProviderInputTokens); err != nil {
		return err
	}
	if err := addInt64(&summary.OutputTokens, event.ProviderOutputTokens); err != nil {
		return err
	}
	if err := addInt64(&summary.CostMicrosUSD, committedCost); err != nil {
		return err
	}
	if !costKnown {
		if err := addInt64(&summary.UnknownAttempts, 1); err != nil {
			return err
		}
	}
	if int64(event.FallbackCount) > summary.Fallbacks {
		summary.Fallbacks = int64(event.FallbackCount)
	}
	if event.Outcome == "success" {
		for _, key := range []string{
			successKey(successDeployment, event.DeploymentID),
			successKey(successRoute, event.RouteID),
			successKey(successProvider, event.ProviderID),
		} {
			if key == "" {
				continue
			}
			if accumulator.Successes == nil {
				accumulator.Successes = make(map[string]time.Time)
			}
			if event.OccurredAt.After(accumulator.Successes[key]) {
				accumulator.Successes[key] = event.OccurredAt
			}
		}
	}

	// The same contribution goes to the hour it finished in and to the running
	// totals, so the two can never disagree about one attempt.
	var contribution Bucket
	contribution.Attempts = 1
	contribution.InputTokens, contribution.OutputTokens = event.ProviderInputTokens, event.ProviderOutputTokens
	contribution.CostMicrosUSD, contribution.LatencyMillis = committedCost, event.LatencyMillis
	if !costKnown {
		contribution.UnknownAttempts = 1
	}
	if event.Outcome != "success" {
		contribution.Errors = 1
	}
	// Provider usage can be unavailable after an ambiguous failure. Such attempts
	// are settled against a conservative token upper bound; the estimated part is
	// carried separately so an upper bound is never mistaken for reported usage.
	if event.TokenEstimated {
		contribution.EstimatedInputTokens, contribution.EstimatedOutputTokens = event.ProviderInputTokens, event.ProviderOutputTokens
	}
	if event.CostEstimated && costKnown {
		contribution.EstimatedCostMicrosUSD = committedCost
	}
	hour := event.OccurredAt.UTC().Truncate(time.Hour)
	row := a.hourly[hour.Unix()]
	if err := row.add(hourlyRow{Bucket: contribution}); err != nil {
		return err
	}
	row.Bucket.Hour = hour
	a.hourly[hour.Unix()] = row
	if err := a.totals.add(contribution); err != nil {
		return err
	}

	if !costKnown {
		a.metrics.UnknownAttempts++
	}
	if event.Outcome != "success" {
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
	if _, ok := a.started[event.AttemptID]; ok {
		delete(a.started, event.AttemptID)
		a.dirtyStarted[event.AttemptID] = struct{}{}
	}
	return nil
}

func (a *Aggregate) applyFinalized(record ledger.Record, accumulator *pendingRequest) error {
	event := record.Event
	summary := accumulator.Summary
	summary.Outcome = event.Outcome
	summary.CompletedAt = event.OccurredAt
	summary.Sequence = record.Sequence
	a.batchSummaries = append(a.batchSummaries, summary)
	delete(a.requests, event.RequestID)
	if event.Outcome == "success" {
		for key, at := range accumulator.Successes {
			if at.After(a.successes[key]) {
				a.successes[key] = at
			}
		}
	}

	var latencyMillis uint64
	latencyKnown := !summary.AcceptedAt.IsZero() && event.OccurredAt.After(summary.AcceptedAt)
	if latencyKnown {
		latencyMillis = uint64(event.OccurredAt.Sub(summary.AcceptedAt).Milliseconds())
	}
	hour := event.OccurredAt.UTC().Truncate(time.Hour)
	row := a.hourly[hour.Unix()]
	row.Bucket.Hour = hour
	if err := addInt64(&row.Bucket.Requests, 1); err != nil {
		return err
	}
	if event.Outcome != "success" {
		if err := addInt64(&row.Bucket.RequestErrors, 1); err != nil {
			return err
		}
	}
	if latencyKnown {
		if err := addInt64(&row.Bucket.RequestLatencySamples, 1); err != nil {
			return err
		}
		if index, overflow := latencyBucketIndex(latencyMillis); overflow {
			row.RequestLatencyOverflow++
		} else {
			row.RequestLatencyBuckets[index]++
		}
	}
	a.hourly[hour.Unix()] = row
	if err := addInt64(&a.totals.Requests, 1); err != nil {
		return err
	}
	if event.Outcome == "success" {
		a.metrics.RequestsSuccess++
	} else {
		a.metrics.RequestsError++
	}
	if latencyKnown {
		if err := addUint64(&a.metrics.RequestLatencyMillis, latencyMillis); err != nil {
			return err
		}
		recordLatency(&a.metrics.RequestLatencyBuckets, latencyMillis)
	}
	if err := a.addRequestRollup(event, record.Sequence, latencyMillis, latencyKnown); err != nil {
		return err
	}
	return addUint64(&a.metrics.Fallbacks, uint64(summary.Fallbacks))
}

// PendingRecords reports how many rows wait for the next flush.
func (a *Aggregate) PendingRecords() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.batchAttempts) + len(a.batchSummaries)
}

// FlushIfFull writes the pending batch once it reaches the flush size. A
// caller replaying a long stretch of the Ledger calls it after each record so
// memory stays one batch deep.
func (a *Aggregate) FlushIfFull() error {
	if a.PendingRecords() < historyFlushRecords {
		return nil
	}
	return a.Flush()
}

// Flush writes everything applied since the previous flush, together with the
// watermark it reaches, in one transaction. A failed write hands the increment
// back, so the next flush writes it again rather than losing it.
func (a *Aggregate) Flush() error {
	a.flushMu.Lock()
	defer a.flushMu.Unlock()
	batch, empty := a.takeBatch()
	if empty {
		return nil
	}
	if err := a.history.commit(batch); err != nil {
		a.returnBatch(batch)
		return fmt.Errorf("write usage history: %w", err)
	}
	a.mu.Lock()
	a.committed = batch.state.Watermark
	a.mu.Unlock()
	return nil
}

// HistoryStats describes the index's progress for metrics.
type HistoryStats struct {
	PendingRecords int
	Applied        ledger.Watermark
	Committed      ledger.Watermark
}

func (a *Aggregate) HistoryStats() HistoryStats {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return HistoryStats{
		PendingRecords: len(a.batchAttempts) + len(a.batchSummaries),
		Applied:        a.watermark, Committed: a.committed,
	}
}

func (a *Aggregate) takeBatch() (*historyBatch, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.watermark.Sequence == 0 {
		return nil, true
	}
	batch := &historyBatch{
		state: HistoryState{
			Watermark: a.watermark, Totals: a.totals, Metrics: a.metrics,
			EventIDs: append([]string(nil), a.eventIDOrder...),
		},
		attempts: a.batchAttempts, summaries: a.batchSummaries, hourly: a.hourly,
		rollup: make(map[string]domain.DailyRollup, len(a.rollupDelta)), successes: a.successes,
		pendingPut: map[string]pendingRequest{}, pendingDelete: map[string]struct{}{},
		startedPut: map[string]time.Time{}, startedDelete: map[string]struct{}{},
	}
	for key, row := range a.rollupDelta {
		batch.rollup[key.Encode()] = *row
	}
	for requestID := range a.dirtyRequests {
		if request, ok := a.requests[requestID]; ok {
			batch.pendingPut[requestID] = clonePendingRequest(*request)
		} else {
			batch.pendingDelete[requestID] = struct{}{}
		}
	}
	for attemptID := range a.dirtyStarted {
		if at, ok := a.started[attemptID]; ok {
			batch.startedPut[attemptID] = at
		} else {
			batch.startedDelete[attemptID] = struct{}{}
		}
	}
	a.batchAttempts, a.batchSummaries = nil, nil
	a.hourly = make(map[int64]hourlyRow)
	a.rollupDelta = make(map[domain.RollupKey]*domain.DailyRollup)
	a.successes = make(map[string]time.Time)
	a.dirtyRequests, a.dirtyStarted = make(map[string]struct{}), make(map[string]struct{})
	return batch, false
}

// returnBatch puts an unwritten increment back in front of whatever arrived
// while the write was in flight. In-flight entries need only be marked dirty
// again: the next flush reads their current value.
func (a *Aggregate) returnBatch(batch *historyBatch) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.batchAttempts = append(batch.attempts, a.batchAttempts...)
	a.batchSummaries = append(batch.summaries, a.batchSummaries...)
	for hour, row := range batch.hourly {
		current := a.hourly[hour]
		if err := current.add(row); err == nil {
			current.Bucket.Hour = row.Bucket.Hour
			a.hourly[hour] = current
		}
	}
	for encoded, row := range batch.rollup {
		key, err := domain.DecodeRollupKey(encoded)
		if err != nil {
			continue
		}
		if existing := a.rollupDelta[key]; existing != nil {
			_ = existing.Add(row)
		} else {
			pending := row
			a.rollupDelta[key] = &pending
		}
	}
	for key, at := range batch.successes {
		if at.After(a.successes[key]) {
			a.successes[key] = at
		}
	}
	for requestID := range batch.pendingPut {
		a.dirtyRequests[requestID] = struct{}{}
	}
	for requestID := range batch.pendingDelete {
		a.dirtyRequests[requestID] = struct{}{}
	}
	for attemptID := range batch.startedPut {
		a.dirtyStarted[attemptID] = struct{}{}
	}
	for attemptID := range batch.startedDelete {
		a.dirtyStarted[attemptID] = struct{}{}
	}
}

func clonePendingRequest(request pendingRequest) pendingRequest {
	if request.Successes != nil {
		successes := make(map[string]time.Time, len(request.Successes))
		for key, value := range request.Successes {
			successes[key] = value
		}
		request.Successes = successes
	}
	return request
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

// Totals is every request and attempt this aggregate has applied.
func (a *Aggregate) Totals() Bucket {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.totals
}

func (a *Aggregate) activeRequest(requestID string) (RequestSummary, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	request, ok := a.requests[requestID]
	if !ok {
		return RequestSummary{}, false
	}
	return request.Summary, true
}

func (a *Aggregate) activeCount() uint64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return uint64(len(a.requests))
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

func containsTag(tags []string, wanted string) bool {
	for _, tag := range tags {
		if tag == wanted {
			return true
		}
	}
	return false
}
