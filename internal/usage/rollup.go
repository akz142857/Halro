package usage

import (
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

// The persisted row's histogram must stay the same shape as the one this
// package records into, or a stored day could not be compared against the
// live aggregate. Identical array types make that a compile error rather than
// a silently truncated tail.
var _ = [domain.RollupLatencyBuckets]uint64([latencyBucketCount]uint64{})

// rollupRow returns the pending increment for key, creating it stamped with
// the event's accounting identity. The period comes off the event — stamped at
// admission and inherited downstream — so the day a row belongs to is never
// re-derived here from a completion timestamp.
func (a *Aggregate) rollupRow(key domain.RollupKey, event ledger.Event, sequence uint64) *domain.DailyRollup {
	if existing := a.rollupDelta[key]; existing != nil {
		return existing
	}
	row := &domain.DailyRollup{FirstSequence: sequence}
	row.Identity(event.PeriodID, event.PeriodTimezoneVersion, event.PeriodTimezone,
		event.PeriodStartMicros, event.PeriodEndMicros)
	a.rollupDelta[key] = row
	return row
}

// rollupDimensions lists the rows one event contributes to. A dimension whose
// value the event does not carry is skipped rather than stored under an empty
// key: an empty key would collect unrelated traffic under one heading and then
// claim to be a provider.
func rollupDimensions(event ledger.Event, requestLevel bool) []domain.RollupKey {
	keys := []domain.RollupKey{{Dimension: domain.RollupDimensionTotal, DimensionKey: domain.RollupTotalKey}}
	candidates := []struct {
		dimension string
		value     string
	}{
		{domain.RollupDimensionProject, event.ProjectID},
		{domain.RollupDimensionRequestedModel, event.RequestedModel},
		{domain.RollupDimensionProvider, event.ProviderID},
		{domain.RollupDimensionDeployment, event.DeploymentID},
		{domain.RollupDimensionProviderModel, event.ProviderModel},
		{domain.RollupDimensionOffering, string(event.OfferingID)},
	}
	for _, candidate := range candidates {
		if candidate.value == "" {
			continue
		}
		if requestLevel && !domain.RequestMetricDimensions[candidate.dimension] {
			continue
		}
		keys = append(keys, domain.RollupKey{Dimension: candidate.dimension, DimensionKey: candidate.value})
	}
	for index := range keys {
		keys[index].PeriodID = event.PeriodID
		keys[index].TimezoneVersion = event.PeriodTimezoneVersion
	}
	return keys
}

// addAttemptRollup folds one settled attempt into every dimension it belongs
// to. The contribution is built once and merged repeatedly so a dimension row
// can never disagree with the total about the same attempt.
func (a *Aggregate) addAttemptRollup(event ledger.Event, sequence uint64, committedCost int64, costKnown bool) error {
	contribution := domain.DailyRollup{
		Attempts:                      1,
		InputTokens:                   event.ProviderInputTokens,
		OutputTokens:                  event.ProviderOutputTokens,
		CostMicrosUSD:                 committedCost,
		LatencyMillis:                 event.LatencyMillis,
		ProviderCachedInputTokens:     event.ProviderCachedInputTokens,
		ProviderCacheWriteInputTokens: event.ProviderCacheWriteInputTokens,
		ProviderReasoningTokens:       event.ProviderReasoningTokens,
		AttemptLatencySamples:         1,
	}
	contribution.Identity(event.PeriodID, event.PeriodTimezoneVersion, event.PeriodTimezone,
		event.PeriodStartMicros, event.PeriodEndMicros)
	if !costKnown {
		contribution.UnknownAttempts = 1
	}
	if event.Outcome != "success" {
		contribution.Errors = 1
	}
	// Estimated tokens and estimated cost are the parts of the totals above
	// that came from an upper bound rather than from Provider-reported usage.
	// Dashboard derives them at read time (query.go); a stored rollup has to
	// accumulate them at write time or the two views disagree.
	if event.TokenEstimated {
		contribution.EstimatedInputTokens = event.ProviderInputTokens
		contribution.EstimatedOutputTokens = event.ProviderOutputTokens
	}
	if event.CostEstimated && costKnown {
		contribution.EstimatedCostMicrosUSD = committedCost
	}
	if event.LatencyMillis > 0 {
		index, overflow := latencyBucketIndex(uint64(event.LatencyMillis))
		if overflow {
			contribution.AttemptLatencyOverflow = 1
		} else {
			contribution.AttemptLatencyBuckets[index] = 1
		}
	} else {
		contribution.AttemptLatencyBuckets[0] = 1
	}
	return a.mergeRollup(rollupDimensions(event, false), contribution, event, sequence)
}

// addRequestRollup folds one finalized request in. Request identity exists only
// on this event, which names the project and the requested model and nothing
// else, so the provider and deployment rows deliberately get no share of it.
func (a *Aggregate) addRequestRollup(event ledger.Event, sequence uint64, latencyMillis uint64, latencyKnown bool) error {
	contribution := domain.DailyRollup{Requests: 1}
	contribution.Identity(event.PeriodID, event.PeriodTimezoneVersion, event.PeriodTimezone,
		event.PeriodStartMicros, event.PeriodEndMicros)
	if event.Outcome != "success" {
		contribution.RequestErrors = 1
	}
	if latencyKnown {
		contribution.RequestLatencySamples = 1
		contribution.RequestLatencyMillis = int64(latencyMillis)
		index, overflow := latencyBucketIndex(latencyMillis)
		if overflow {
			contribution.RequestLatencyOverflow = 1
		} else {
			contribution.RequestLatencyBuckets[index] = 1
		}
	}
	return a.mergeRollup(rollupDimensions(event, true), contribution, event, sequence)
}

func (a *Aggregate) mergeRollup(
	keys []domain.RollupKey, contribution domain.DailyRollup, event ledger.Event, sequence uint64,
) error {
	// The contribution carries the sequence too, so a row that already exists in
	// the increment keeps the earliest sequence that reached it — the increment
	// may be merged into a stored row that was first written long before.
	contribution.FirstSequence = sequence
	for _, key := range keys {
		if err := key.Validate(); err != nil {
			return err
		}
		if err := a.rollupRow(key, event, sequence).Add(contribution); err != nil {
			return err
		}
	}
	return nil
}

// latencyBucketIndex reports which histogram bucket a sample lands in, and
// whether it exceeded the last bound. recordLatency drops such a sample
// entirely; a stored histogram cannot afford to, because its count is what a
// later percentile is computed against.
func latencyBucketIndex(latencyMillis uint64) (int, bool) {
	for index, upperBound := range LatencyBucketsMillis {
		if latencyMillis <= upperBound {
			return index, false
		}
	}
	return 0, true
}

// ApproximateLatencyPercentile reads a percentile off a stored histogram.
//
// It reports the upper bound of the bucket the percentile falls into, which is
// the honest answer a histogram can give: the true value is somewhere inside
// that bucket. The second result says the sample landed above the last bound,
// where the only truthful statement is "greater than that bound" — callers
// must present it that way rather than as a number.
func ApproximateLatencyPercentile(buckets [latencyBucketCount]uint64, overflow uint64, percent int) (int64, bool) {
	var total uint64
	for _, count := range buckets {
		total += count
	}
	total += overflow
	if total == 0 {
		return 0, false
	}
	// Ceiling, so the 95th percentile of 20 samples is the 19th, not the 18th.
	target := (total*uint64(percent) + 99) / 100
	var cumulative uint64
	for index, count := range buckets {
		cumulative += count
		if cumulative >= target {
			return int64(LatencyBucketsMillis[index]), false
		}
	}
	return int64(LatencyBucketsMillis[latencyBucketCount-1]), true
}
