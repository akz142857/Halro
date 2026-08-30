package domain

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// RollupVersion is the structure version of a persisted daily rollup row. The
// rollup is a derivative of the Ledger, so a version it does not recognise is
// refused and rebuilt rather than migrated — the same posture the Usage
// checkpoint takes.
const RollupVersion = 1

// RollupLatencyBuckets is the number of latency histogram buckets a rollup row
// carries. It mirrors usage.LatencyBucketsMillis; usage asserts the two agree
// at compile time.
const RollupLatencyBuckets = 12

// Rollup dimensions. A row holds one dimension's one value — the rollup is a
// marginal aggregate and deliberately stores no cross terms (D7-a), so a
// question like "this project's model mix" is answered by drilling into the
// attempt detail, not by a rollup read.
const (
	RollupDimensionTotal          = "total"
	RollupDimensionProject        = "project"
	RollupDimensionRequestedModel = "requested_model"
	RollupDimensionProvider       = "provider"
	RollupDimensionDeployment     = "deployment"
	RollupDimensionProviderModel  = "provider_model"
)

// RollupTotalKey is the dimension key the total row carries. The total is
// stored rather than summed on read so a query that only wants the total does
// not walk every dimension, and so the two can never drift apart.
const RollupTotalKey = "-"

// RollupOtherKey collects the tail beyond MaxRollupKeysPerDimension.
const RollupOtherKey = "__other__"

// MaxRollupKeysPerDimension bounds how many distinct values one accounting day
// stores for one dimension. provider_model is the worst case, and an unbounded
// day would grow the rollup — and every backup that carries it — with the
// caller's naming habits rather than with traffic.
//
// The tail is chosen by LEDGER ORDER, not by size: the first N distinct values
// to appear in the day are kept and everything after them is folded into
// RollupOtherKey. Size would be the more useful cut, but it cannot be taken
// incrementally. A day is not finished when its last accounting hour passes —
// a request admitted at 23:59 and settled at 00:02 is charged to the day it was
// admitted on — so "fold the smallest once the day closes" has no moment it can
// safely run, and folding twice does not produce the partition a single pass
// would. Ledger order has no such moment: both the incremental path and a full
// rebuild see the same events in the same order, so they admit the same keys.
const MaxRollupKeysPerDimension = 200

// RollupKeySeparator is NUL because a dimension key legitimately contains "/":
// Gemini public models are "models/...", Bedrock inference profiles are ARNs,
// and the domain only requires them to be non-blank.
const RollupKeySeparator = "\x00"

// RequestMetricDimensions are the dimensions whose rows carry request-level
// counts. Request identity only exists on EventRequestFinalized, which names
// the project and the requested model and nothing else — a single request can
// span providers through retry and fallback, so a provider row cannot claim a
// share of it (D9-a).
var RequestMetricDimensions = map[string]bool{
	RollupDimensionTotal:          true,
	RollupDimensionProject:        true,
	RollupDimensionRequestedModel: true,
}

var rollupDimensions = map[string]bool{
	RollupDimensionTotal:          true,
	RollupDimensionProject:        true,
	RollupDimensionRequestedModel: true,
	RollupDimensionProvider:       true,
	RollupDimensionDeployment:     true,
	RollupDimensionProviderModel:  true,
}

// IsRollupDimension reports whether name is a dimension the rollup stores.
func IsRollupDimension(name string) bool { return rollupDimensions[name] }

// RollupKey identifies one rollup row.
//
// The accounting day is the event's own PeriodID — stamped at admission and
// inherited by every downstream event (budget.Period.Stamp) — never a
// re-derived truncation of a completion timestamp. TimezoneVersion is part of
// the identity because the same local date under two generations of the
// accounting timezone denotes two different UTC intervals, and adding them
// together would be adding two different days.
type RollupKey struct {
	PeriodID        string
	TimezoneVersion uint64
	Dimension       string
	DimensionKey    string
}

// Encode renders the key in its stored form. Ordering is by period first, so a
// range query over one dimension is a bounded cursor scan.
func (k RollupKey) Encode() string {
	return k.PeriodID + RollupKeySeparator +
		strconv.FormatUint(k.TimezoneVersion, 10) + RollupKeySeparator +
		k.Dimension + RollupKeySeparator + k.DimensionKey
}

// RollupDimensionPrefix is the encoded prefix every row of one day's one
// dimension shares. The key cap counts within exactly this prefix.
func RollupDimensionPrefix(periodID string, timezoneVersion uint64, dimension string) string {
	return RollupDayPrefix(periodID, timezoneVersion) + dimension + RollupKeySeparator
}

// RollupDayPrefix is the encoded prefix every row of one accounting day shares.
func RollupDayPrefix(periodID string, timezoneVersion uint64) string {
	return periodID + RollupKeySeparator +
		strconv.FormatUint(timezoneVersion, 10) + RollupKeySeparator
}

// DecodeRollupKey parses a stored key. The dimension key is the remainder
// rather than a field, because it may itself contain any byte but NUL.
func DecodeRollupKey(encoded string) (RollupKey, error) {
	parts := strings.SplitN(encoded, RollupKeySeparator, 4)
	if len(parts) != 4 {
		return RollupKey{}, fmt.Errorf("usage rollup key has %d segments, want 4", len(parts))
	}
	version, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return RollupKey{}, fmt.Errorf("usage rollup key timezone version: %w", err)
	}
	key := RollupKey{PeriodID: parts[0], TimezoneVersion: version, Dimension: parts[2], DimensionKey: parts[3]}
	if err := key.Validate(); err != nil {
		return RollupKey{}, err
	}
	return key, nil
}

// Validate rejects a key that could not have been produced by a stamped event.
func (k RollupKey) Validate() error {
	if strings.TrimSpace(k.PeriodID) == "" {
		return errors.New("usage rollup key requires a period id")
	}
	if strings.Contains(k.PeriodID, RollupKeySeparator) || strings.Contains(k.Dimension, RollupKeySeparator) {
		return errors.New("usage rollup key segments cannot contain the separator")
	}
	if !rollupDimensions[k.Dimension] {
		return fmt.Errorf("usage rollup dimension %q is not known", k.Dimension)
	}
	if k.Dimension == RollupDimensionTotal {
		if k.DimensionKey != RollupTotalKey {
			return fmt.Errorf("usage rollup total row must use key %q", RollupTotalKey)
		}
		return nil
	}
	if k.DimensionKey == "" {
		return errors.New("usage rollup key requires a dimension value")
	}
	return nil
}

// DailyRollup is one accounting day's totals for one dimension value.
//
// Field names follow usage.Bucket wherever the two overlap so an operator
// comparing the summary against the dashboard is reading the same words.
// Two families deliberately differ:
//
//   - Estimated* and the Provider* token columns are SUBSETS of the totals
//     above them, not additions to them. EstimatedCostMicrosUSD is the part of
//     CostMicrosUSD that came from an estimate; the cache and reasoning tokens
//     partition Input/OutputTokens. Summing a subset into its own total is the
//     reconciliation error this schema exists to prevent.
//   - Latency is a histogram rather than a percentile, because a P95 cannot be
//     summed across days. Overflow counts samples above the last bucket bound,
//     which recordLatency otherwise drops on the floor.
type DailyRollup struct {
	Version int `json:"version"`
	// FirstSequence is the ledger sequence that first contributed to this row.
	// It is what makes the key cap deterministic: new values are admitted in
	// this order, so a day written in a hundred increments admits the same
	// keys as the same day rebuilt in one pass.
	FirstSequence     uint64 `json:"first_sequence,omitempty"`
	PeriodID          string `json:"period_id"`
	TimezoneVersion   uint64 `json:"timezone_version"`
	Timezone          string `json:"timezone,omitempty"`
	PeriodStartMicros int64  `json:"period_start_micros,omitempty"`
	PeriodEndMicros   int64  `json:"period_end_micros,omitempty"`

	// Request-level counts. Only populated on RequestMetricDimensions rows.
	Requests      int64 `json:"requests"`
	RequestErrors int64 `json:"request_errors"`

	Attempts               int64 `json:"attempts"`
	Errors                 int64 `json:"errors"`
	InputTokens            int64 `json:"input_tokens"`
	OutputTokens           int64 `json:"output_tokens"`
	EstimatedInputTokens   int64 `json:"estimated_input_tokens"`
	EstimatedOutputTokens  int64 `json:"estimated_output_tokens"`
	CostMicrosUSD          int64 `json:"cost_micros_usd"`
	EstimatedCostMicrosUSD int64 `json:"estimated_cost_micros_usd"`
	UnknownAttempts        int64 `json:"unknown_attempts"`
	LatencyMillis          int64 `json:"latency_millis"`

	ProviderCachedInputTokens     int64 `json:"provider_cached_input_tokens"`
	ProviderCacheWriteInputTokens int64 `json:"provider_cache_write_input_tokens"`
	ProviderReasoningTokens       int64 `json:"provider_reasoning_tokens"`

	RequestLatencyBuckets  [RollupLatencyBuckets]uint64 `json:"request_latency_buckets"`
	RequestLatencyOverflow uint64                       `json:"request_latency_overflow"`
	RequestLatencySamples  int64                        `json:"request_latency_samples"`
	RequestLatencyMillis   int64                        `json:"request_latency_millis"`

	AttemptLatencyBuckets  [RollupLatencyBuckets]uint64 `json:"attempt_latency_buckets"`
	AttemptLatencyOverflow uint64                       `json:"attempt_latency_overflow"`
	AttemptLatencySamples  int64                        `json:"attempt_latency_samples"`
}

// Identity stamps the accounting period a row belongs to. It is idempotent:
// two events of the same period carry the same interval, and a row that
// already knows its period is left alone.
func (d *DailyRollup) Identity(periodID string, timezoneVersion uint64, timezone string, startMicros, endMicros int64) {
	d.Version = RollupVersion
	d.PeriodID = periodID
	d.TimezoneVersion = timezoneVersion
	if timezone != "" {
		d.Timezone = timezone
	}
	if startMicros != 0 {
		d.PeriodStartMicros = startMicros
	}
	if endMicros != 0 {
		d.PeriodEndMicros = endMicros
	}
}

// Add merges other into d. Every column is additive, which is what lets an
// increment be applied to a stored row, a stored row be merged into a range
// total, and a rebuild reach the same numbers as the incremental path.
//
// Overflow is an error rather than a wrap: a silently negative cost would be a
// wrong number presented as a right one.
func (d *DailyRollup) Add(other DailyRollup) error {
	if d.Version == 0 {
		d.Version = other.Version
	}
	if d.FirstSequence == 0 || (other.FirstSequence != 0 && other.FirstSequence < d.FirstSequence) {
		d.FirstSequence = other.FirstSequence
	}
	if d.PeriodID == "" {
		d.PeriodID = other.PeriodID
		d.TimezoneVersion = other.TimezoneVersion
	}
	if d.PeriodID != other.PeriodID || d.TimezoneVersion != other.TimezoneVersion {
		return fmt.Errorf("usage rollup cannot merge period %s/v%d into %s/v%d",
			other.PeriodID, other.TimezoneVersion, d.PeriodID, d.TimezoneVersion)
	}
	if d.Timezone == "" {
		d.Timezone = other.Timezone
	}
	if d.PeriodStartMicros == 0 {
		d.PeriodStartMicros = other.PeriodStartMicros
	}
	if d.PeriodEndMicros == 0 {
		d.PeriodEndMicros = other.PeriodEndMicros
	}
	pairs := []struct {
		target *int64
		value  int64
	}{
		{&d.Requests, other.Requests}, {&d.RequestErrors, other.RequestErrors},
		{&d.Attempts, other.Attempts}, {&d.Errors, other.Errors},
		{&d.InputTokens, other.InputTokens}, {&d.OutputTokens, other.OutputTokens},
		{&d.EstimatedInputTokens, other.EstimatedInputTokens},
		{&d.EstimatedOutputTokens, other.EstimatedOutputTokens},
		{&d.CostMicrosUSD, other.CostMicrosUSD},
		{&d.EstimatedCostMicrosUSD, other.EstimatedCostMicrosUSD},
		{&d.UnknownAttempts, other.UnknownAttempts},
		{&d.LatencyMillis, other.LatencyMillis},
		{&d.ProviderCachedInputTokens, other.ProviderCachedInputTokens},
		{&d.ProviderCacheWriteInputTokens, other.ProviderCacheWriteInputTokens},
		{&d.ProviderReasoningTokens, other.ProviderReasoningTokens},
		{&d.RequestLatencySamples, other.RequestLatencySamples},
		{&d.RequestLatencyMillis, other.RequestLatencyMillis},
		{&d.AttemptLatencySamples, other.AttemptLatencySamples},
	}
	for _, pair := range pairs {
		sum, err := addRollupInt64(*pair.target, pair.value)
		if err != nil {
			return err
		}
		*pair.target = sum
	}
	for index := range d.RequestLatencyBuckets {
		sum, err := addRollupUint64(d.RequestLatencyBuckets[index], other.RequestLatencyBuckets[index])
		if err != nil {
			return err
		}
		d.RequestLatencyBuckets[index] = sum
	}
	for index := range d.AttemptLatencyBuckets {
		sum, err := addRollupUint64(d.AttemptLatencyBuckets[index], other.AttemptLatencyBuckets[index])
		if err != nil {
			return err
		}
		d.AttemptLatencyBuckets[index] = sum
	}
	overflow, err := addRollupUint64(d.RequestLatencyOverflow, other.RequestLatencyOverflow)
	if err != nil {
		return err
	}
	d.RequestLatencyOverflow = overflow
	overflow, err = addRollupUint64(d.AttemptLatencyOverflow, other.AttemptLatencyOverflow)
	if err != nil {
		return err
	}
	d.AttemptLatencyOverflow = overflow
	return nil
}

// DropRequestMetrics clears the request-level columns. A dimension with no
// request identity reports nothing rather than zero, because zero reads as
// "no requests" instead of "not answerable here".
func (d *DailyRollup) DropRequestMetrics() {
	d.Requests = 0
	d.RequestErrors = 0
	d.RequestLatencyBuckets = [RollupLatencyBuckets]uint64{}
	d.RequestLatencyOverflow = 0
	d.RequestLatencySamples = 0
	d.RequestLatencyMillis = 0
}

func addRollupInt64(current, delta int64) (int64, error) {
	if delta > 0 && current > math.MaxInt64-delta {
		return 0, errors.New("usage rollup counter overflow")
	}
	if delta < 0 && current < math.MinInt64-delta {
		return 0, errors.New("usage rollup counter underflow")
	}
	return current + delta, nil
}

func addRollupUint64(current, delta uint64) (uint64, error) {
	if current > math.MaxUint64-delta {
		return 0, errors.New("usage rollup counter overflow")
	}
	return current + delta, nil
}
