package usage

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"sort"
	"time"
)

type AttemptQuery struct {
	BeforeSequence uint64
	Limit          int
	ProjectID      string
	WorkUnitID     string
	RunID          string
	ProviderID     string
	DeploymentID   string
	RequestID      string
	RequestedModel string
	ProviderModel  string
	Status         string
	Start          time.Time
	End            time.Time
}

type AttemptPage struct {
	Attempts   []AttemptEvent
	NextCursor string
}

type RequestDetail struct {
	Summary  RequestSummary `json:"summary"`
	Attempts []AttemptEvent `json:"attempts"`
}

type Dashboard struct {
	Today           Bucket                            `json:"today"`
	Hourly          []Bucket                          `json:"hourly"`
	Active          uint64                            `json:"active_requests"`
	Watermark       uint64                            `json:"watermark_sequence"`
	Breakdowns      map[string]map[string][]Breakdown `json:"breakdowns"`
	RecentAnomalies []Anomaly                         `json:"recent_anomalies"`
}

type Breakdown struct {
	Key                 string `json:"key"`
	Calls               int64  `json:"calls"`
	InputTokens         int64  `json:"input_tokens"`
	OutputTokens        int64  `json:"output_tokens"`
	CostMicrosUSD       int64  `json:"cost_micros_usd"`
	EstimatedCostMicros int64  `json:"estimated_cost_micros_usd,omitempty"`
	UnknownAttempts     int64  `json:"unknown_attempts"`
	Errors              int64  `json:"errors"`
}

type Anomaly struct {
	RequestID      string    `json:"request_id"`
	AttemptID      string    `json:"attempt_id"`
	CompletedAt    time.Time `json:"completed_at"`
	ProjectID      string    `json:"project_id"`
	DeploymentID   string    `json:"deployment_id,omitempty"`
	ProviderID     string    `json:"provider_id,omitempty"`
	RequestedModel string    `json:"requested_model,omitempty"`
	ProviderModel  string    `json:"provider_model,omitempty"`
	Status         string    `json:"status"`
	ErrorClass     string    `json:"error_class,omitempty"`
	HTTPStatus     int       `json:"http_status,omitempty"`
	RetryCount     int       `json:"retry_count"`
	FallbackCount  int       `json:"fallback_count"`
}

func (a *Aggregate) QueryAttempts(query AttemptQuery) (AttemptPage, error) {
	if query.Limit < 1 || query.Limit > 100 {
		return AttemptPage{}, errors.New("usage page limit must be between 1 and 100")
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	page := AttemptPage{Attempts: make([]AttemptEvent, 0, query.Limit+1)}
	for index := len(a.attempts) - 1; index >= 0; index-- {
		attempt := a.attempts[index]
		if query.BeforeSequence > 0 && attempt.Sequence >= query.BeforeSequence {
			continue
		}
		if query.ProjectID != "" && attempt.ProjectID != query.ProjectID ||
			query.WorkUnitID != "" && attempt.WorkUnitID != query.WorkUnitID ||
			query.RunID != "" && attempt.RunID != query.RunID ||
			query.ProviderID != "" && attempt.ProviderID != query.ProviderID ||
			query.DeploymentID != "" && attempt.DeploymentID != query.DeploymentID ||
			query.RequestID != "" && attempt.RequestID != query.RequestID ||
			query.RequestedModel != "" && attempt.RequestedModel != query.RequestedModel ||
			query.ProviderModel != "" && attempt.ProviderModel != query.ProviderModel ||
			query.Status != "" && attempt.Status != query.Status ||
			!query.Start.IsZero() && attempt.CompletedAt.Before(query.Start) ||
			!query.End.IsZero() && !attempt.CompletedAt.Before(query.End) {
			continue
		}
		page.Attempts = append(page.Attempts, attempt)
		if len(page.Attempts) == query.Limit+1 {
			page.Attempts = page.Attempts[:query.Limit]
			page.NextCursor = EncodeCursor(page.Attempts[len(page.Attempts)-1].Sequence)
			break
		}
	}
	return page, nil
}

func (a *Aggregate) RequestDetail(requestID string) (RequestDetail, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var result RequestDetail
	found := false
	for index := len(a.summaries) - 1; index >= 0; index-- {
		if a.summaries[index].RequestID == requestID {
			result.Summary = a.summaries[index]
			found = true
			break
		}
	}
	if active := a.requests[requestID]; active != nil {
		result.Summary = active.summary
		found = true
	}
	if !found {
		return RequestDetail{}, false
	}
	for _, attempt := range a.attempts {
		if attempt.RequestID == requestID {
			result.Attempts = append(result.Attempts, attempt)
		}
	}
	return result, true
}

// Period is the accounting day a dashboard reports on, as a half-open UTC
// interval.
//
// It is supplied by the caller rather than derived here. The boundary is a
// governed setting that only the caller can resolve, and the figures must cover
// exactly the interval the response advertises alongside them — deriving it
// twice is how the totals and the interval drift apart.
// Period is the accounting day a dashboard reports on. It carries the period's
// identity, not just its interval, because membership is decided by the stamp
// an event was admitted with rather than by when it finished — a request
// accepted at 23:59 and settled at 00:02 is charged to the day it was admitted
// on, and the summary rollup keys on that same stamp.
type Period struct {
	ID              string
	TimezoneVersion uint64
}

// Includes reports whether work stamped with this period identity belongs to
// it. There is deliberately no instant here: membership is the stamp, and a
// second interval-based rule beside it is how the dashboard and the summary
// would start disagreeing about which day a call belongs to.
func (p Period) Includes(periodID string, timezoneVersion uint64) bool {
	return periodID == p.ID && timezoneVersion == p.TimezoneVersion
}

func (a *Aggregate) Dashboard(now time.Time, period Period) Dashboard {
	a.mu.RLock()
	defer a.mu.RUnlock()
	since := now.Add(-7 * 24 * time.Hour).UTC().Truncate(time.Hour)
	result := Dashboard{
		Active: uint64(len(a.requests)), Watermark: a.watermark.Sequence,
		Hourly:          make([]Bucket, 0, 7*24),
		RecentAnomalies: make([]Anomaly, 0, 5),
		Breakdowns: map[string]map[string][]Breakdown{
			"project": {}, "provider": {}, "requested_model": {}, "provider_model": {},
		},
	}
	hourIndexes := make(map[int64]int, len(a.hourly))
	for _, bucket := range a.hourly {
		if !bucket.Hour.Before(since) {
			// Request quality is rebuilt from terminal request summaries below. That
			// keeps restored checkpoints from needing a schema migration and prevents
			// an hourly bucket from being reused as an accounting-period boundary.
			bucket.Requests = 0
			bucket.RequestErrors = 0
			bucket.RequestLatencySamples = 0
			bucket.RequestLatencyP50Millis = 0
			bucket.RequestLatencyP95Millis = 0
			hourIndexes[bucket.Hour.UTC().Truncate(time.Hour).Unix()] = len(result.Hourly)
			result.Hourly = append(result.Hourly, bucket)
		}
	}
	hourlyRequestLatencies := make(map[int64][]int64, len(result.Hourly))
	todayRequestLatencies := make([]int64, 0)
	for _, summary := range a.summaries {
		if summary.CompletedAt.IsZero() {
			continue
		}
		hour := summary.CompletedAt.UTC().Truncate(time.Hour)
		if bucketIndex, ok := hourIndexes[hour.Unix()]; ok {
			result.Hourly[bucketIndex].Requests++
			if summary.Outcome != "success" {
				result.Hourly[bucketIndex].RequestErrors++
			}
			if latency, ok := requestLatencyMillis(summary); ok {
				hourlyRequestLatencies[hour.Unix()] = append(hourlyRequestLatencies[hour.Unix()], latency)
			}
		}
		if period.Includes(summary.PeriodID, summary.PeriodTimezoneVersion) {
			result.Today.Requests++
			if summary.Outcome != "success" {
				result.Today.RequestErrors++
			}
			if latency, ok := requestLatencyMillis(summary); ok {
				todayRequestLatencies = append(todayRequestLatencies, latency)
			}
		}
	}
	// Provider usage can be unavailable after an ambiguous failure. Such attempts
	// are deliberately settled against a conservative token upper bound. Preserve
	// that accounting total while exposing the estimated portion separately so
	// operators do not mistake an upper bound for Provider-reported consumption.
	breakdowns := map[string]map[string]*Breakdown{
		"project": {}, "provider": {}, "requested_model": {}, "provider_model": {},
	}
	for index := len(a.attempts) - 1; index >= 0; index-- {
		attempt := a.attempts[index]
		hour := attempt.CompletedAt.UTC().Truncate(time.Hour)
		if attempt.TokensEstimated {
			if bucketIndex, ok := hourIndexes[hour.Unix()]; ok {
				result.Hourly[bucketIndex].EstimatedInputTokens += attempt.ProviderInputTokens
				result.Hourly[bucketIndex].EstimatedOutputTokens += attempt.ProviderOutputTokens
			}
		}
		if period.Includes(attempt.PeriodID, attempt.PeriodTimezoneVersion) {
			result.Today.Attempts++
			result.Today.InputTokens += attempt.ProviderInputTokens
			result.Today.OutputTokens += attempt.ProviderOutputTokens
			if cost, ok := attempt.KnownCostMicrosUSD(); ok {
				result.Today.CostMicrosUSD += cost
			} else {
				result.Today.UnknownAttempts++
			}
			if attempt.Status != "success" {
				result.Today.Errors++
			}
			result.Today.LatencyMillis += attempt.LatencyMillis
			if attempt.TokensEstimated {
				result.Today.EstimatedInputTokens += attempt.ProviderInputTokens
				result.Today.EstimatedOutputTokens += attempt.ProviderOutputTokens
			}
			if attempt.CostEstimated {
				if cost, ok := attempt.KnownCostMicrosUSD(); ok {
					result.Today.EstimatedCostMicrosUSD += cost
				}
			}
			addBreakdown(breakdowns["project"], attempt.ProjectID, attempt)
			addBreakdown(breakdowns["provider"], attempt.ProviderID, attempt)
			addBreakdown(breakdowns["requested_model"], attempt.RequestedModel, attempt)
			addBreakdown(breakdowns["provider_model"], attempt.ProviderModel, attempt)
			if len(result.RecentAnomalies) < 5 &&
				(attempt.Status != "success" || attempt.RetryCount > 0 || attempt.FallbackCount > 0) {
				result.RecentAnomalies = append(result.RecentAnomalies, Anomaly{
					RequestID: attempt.RequestID, AttemptID: attempt.AttemptID,
					CompletedAt: attempt.CompletedAt, ProjectID: attempt.ProjectID,
					DeploymentID: attempt.DeploymentID,
					ProviderID:   attempt.ProviderID, RequestedModel: attempt.RequestedModel,
					ProviderModel: attempt.ProviderModel, Status: attempt.Status,
					ErrorClass: attempt.ErrorClass, HTTPStatus: attempt.HTTPStatus,
					RetryCount: attempt.RetryCount, FallbackCount: attempt.FallbackCount,
				})
			}
		}
	}
	setRequestLatency(&result.Today, todayRequestLatencies)
	for hour, latencies := range hourlyRequestLatencies {
		if bucketIndex, ok := hourIndexes[hour]; ok {
			setRequestLatency(&result.Hourly[bucketIndex], latencies)
		}
	}
	for dimension, groups := range breakdowns {
		result.Breakdowns[dimension] = map[string][]Breakdown{
			"calls":  topBreakdowns(groups, 5, "calls"),
			"cost":   topBreakdowns(groups, 5, "cost"),
			"errors": topBreakdowns(groups, 5, "errors"),
		}
	}
	sortBuckets(result.Hourly)
	return result
}

func addBreakdown(groups map[string]*Breakdown, key string, attempt AttemptEvent) {
	if key == "" {
		return
	}
	item := groups[key]
	if item == nil {
		item = &Breakdown{Key: key}
		groups[key] = item
	}
	item.Calls++
	item.InputTokens += attempt.ProviderInputTokens
	item.OutputTokens += attempt.ProviderOutputTokens
	if cost, ok := attempt.KnownCostMicrosUSD(); ok {
		item.CostMicrosUSD += cost
		if attempt.CostEstimated {
			item.EstimatedCostMicros += cost
		}
	} else {
		item.UnknownAttempts++
	}
	if attempt.Status != "success" {
		item.Errors++
	}
}

func topBreakdowns(groups map[string]*Breakdown, limit int, metric string) []Breakdown {
	items := make([]Breakdown, 0, len(groups))
	for _, item := range groups {
		items = append(items, *item)
	}
	sort.Slice(items, func(i, j int) bool {
		left, right := items[i].Calls, items[j].Calls
		switch metric {
		case "cost":
			left, right = items[i].CostMicrosUSD, items[j].CostMicrosUSD
		case "errors":
			left, right = items[i].Errors, items[j].Errors
		}
		if left != right {
			return left > right
		}
		return items[i].Key < items[j].Key
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items
}

func requestLatencyMillis(summary RequestSummary) (int64, bool) {
	if summary.AcceptedAt.IsZero() || !summary.CompletedAt.After(summary.AcceptedAt) {
		return 0, false
	}
	return summary.CompletedAt.Sub(summary.AcceptedAt).Milliseconds(), true
}

func setRequestLatency(bucket *Bucket, latencies []int64) {
	if bucket == nil || len(latencies) == 0 {
		return
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	bucket.RequestLatencySamples = int64(len(latencies))
	bucket.RequestLatencyP50Millis = percentile(latencies, 50)
	bucket.RequestLatencyP95Millis = percentile(latencies, 95)
}

func percentile(sorted []int64, percent int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	index := (len(sorted)*percent + 99) / 100
	return sorted[index-1]
}

func EncodeCursor(sequence uint64) string {
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], sequence)
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

func DecodeCursor(value string) (uint64, error) {
	if value == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) != 8 {
		return 0, errors.New("invalid usage cursor")
	}
	sequence := binary.BigEndian.Uint64(raw)
	if sequence == 0 {
		return 0, errors.New("invalid usage cursor")
	}
	return sequence, nil
}

func sortBuckets(buckets []Bucket) {
	for index := 1; index < len(buckets); index++ {
		for current := index; current > 0 &&
			buckets[current].Hour.Before(buckets[current-1].Hour); current-- {
			buckets[current], buckets[current-1] = buckets[current-1], buckets[current]
		}
	}
}
