package usage

import (
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

const (
	rollupDay     = "2026-08-30"
	rollupVersion = uint64(2)
)

func rollupRows(t *testing.T, aggregate *Aggregate) map[domain.RollupKey]domain.DailyRollup {
	t.Helper()
	snapshot, err := aggregate.TakeCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	rows := make(map[domain.RollupKey]domain.DailyRollup, len(snapshot.Rollup))
	for encoded, row := range snapshot.Rollup {
		key, err := domain.DecodeRollupKey(encoded)
		if err != nil {
			t.Fatal(err)
		}
		rows[key] = row
	}
	return rows
}

func rollupRow(t *testing.T, rows map[domain.RollupKey]domain.DailyRollup, dimension, key string) domain.DailyRollup {
	t.Helper()
	row, exists := rows[domain.RollupKey{
		PeriodID: rollupDay, TimezoneVersion: rollupVersion, Dimension: dimension, DimensionKey: key,
	}]
	if !exists {
		t.Fatalf("no rollup row for %s=%s", dimension, key)
	}
	return row
}

// mixedTraffic is one accounting day with every shape the cost columns have to
// keep apart: a plain billed call, one whose tokens and cost are an estimate,
// one that could not be priced at all, and one that failed.
func mixedTraffic(t *testing.T, aggregate *Aggregate) time.Time {
	t.Helper()
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	unknown := domain.NewUnknownPriceSnapshot(now)
	stamp := func(event ledger.Event) ledger.Event {
		event.PeriodID = rollupDay
		event.PeriodTimezoneVersion = rollupVersion
		event.PeriodTimezone = "Asia/Shanghai"
		return event
	}
	events := []ledger.Event{
		stamp(ledger.Event{EventID: "a1_accepted", Kind: ledger.EventRequestAccepted, RequestID: "req_1",
			ProjectID: "prj_a", RequestedModel: "chat", OccurredAt: now}),
		stamp(ledger.Event{EventID: "a1_settled", Kind: ledger.EventAttemptSettled, RequestID: "req_1",
			AttemptID: "att_1", ProjectID: "prj_a", RequestedModel: "chat", ProviderID: "prov_a",
			DeploymentID: "dep_a", ProviderModel: "vendor/chat-1", OccurredAt: now.Add(time.Second),
			ProviderInputTokens: 100, ProviderOutputTokens: 20, ProviderCachedInputTokens: 40,
			ProviderReasoningTokens: 5, LatencyMillis: 900,
			CommittedMicrosUSD: ledger.MicrosUSD(120), Outcome: "success"}),
		stamp(ledger.Event{EventID: "a1_final", Kind: ledger.EventRequestFinalized, RequestID: "req_1",
			ProjectID: "prj_a", RequestedModel: "chat", OccurredAt: now.Add(2 * time.Second), Outcome: "success"}),

		stamp(ledger.Event{EventID: "a2_accepted", Kind: ledger.EventRequestAccepted, RequestID: "req_2",
			ProjectID: "prj_b", RequestedModel: "chat", OccurredAt: now.Add(time.Minute)}),
		stamp(ledger.Event{EventID: "a2_settled", Kind: ledger.EventAttemptSettled, RequestID: "req_2",
			AttemptID: "att_2", ProjectID: "prj_b", RequestedModel: "chat", ProviderID: "prov_b",
			DeploymentID: "dep_b", ProviderModel: "vendor/chat-2", OccurredAt: now.Add(time.Minute + time.Second),
			ProviderInputTokens: 70, ProviderOutputTokens: 30, LatencyMillis: 2500,
			CommittedMicrosUSD: ledger.MicrosUSD(80), CostEstimated: true, TokenEstimated: true,
			TokenUsageSource: ledger.TokenUsageSourceEstimate, Outcome: "error", ErrorClass: "upstream"}),
		stamp(ledger.Event{EventID: "a2_final", Kind: ledger.EventRequestFinalized, RequestID: "req_2",
			ProjectID: "prj_b", RequestedModel: "chat", OccurredAt: now.Add(time.Minute + 2*time.Second),
			Outcome: "error"}),

		stamp(ledger.Event{EventID: "a3_accepted", Kind: ledger.EventRequestAccepted, RequestID: "req_3",
			ProjectID: "prj_a", RequestedModel: "chat", OccurredAt: now.Add(2 * time.Minute)}),
		stamp(ledger.Event{EventID: "a3_settled", Kind: ledger.EventAttemptSettled, RequestID: "req_3",
			AttemptID: "att_3", ProjectID: "prj_a", RequestedModel: "chat", ProviderID: "prov_a",
			DeploymentID: "dep_a", ProviderModel: "vendor/chat-1", OccurredAt: now.Add(2*time.Minute + time.Second),
			ProviderInputTokens: 10, ProviderOutputTokens: 5, LatencyMillis: 400,
			LeaseMode: ledger.LeaseModeUnknownAllowed, PriceSnapshot: &unknown,
			TokenUsageSource: ledger.TokenUsageSourceProvider, Outcome: "success"}),
		stamp(ledger.Event{EventID: "a3_final", Kind: ledger.EventRequestFinalized, RequestID: "req_3",
			ProjectID: "prj_a", RequestedModel: "chat", OccurredAt: now.Add(2*time.Minute + 2*time.Second),
			Outcome: "success"}),
	}
	for index, event := range events {
		if err := aggregate.Apply(ledger.Record{
			Sequence: uint64(index + 1), Offset: int64(index+1) * 100, Event: event,
		}); err != nil {
			t.Fatalf("event %s: %v", event.EventID, err)
		}
	}
	return now
}

// The cross-check the whole design rests on: the stored rollup and the live
// dashboard accumulate the same events by two independent paths, so if they
// ever disagree one of them is wrong and nothing else in the product can tell
// which. Latency percentiles are excluded on purpose — the rollup keeps a
// histogram, because a P95 cannot be summed across days.
func TestRollupTotalMatchesDashboardToday(t *testing.T) {
	aggregate := NewAggregate()
	now := mixedTraffic(t, aggregate)
	today := aggregate.Dashboard(now.Add(time.Hour), Period{ID: rollupDay, TimezoneVersion: rollupVersion}).Today
	total := rollupRow(t, rollupRows(t, aggregate), domain.RollupDimensionTotal, domain.RollupTotalKey)

	for _, field := range []struct {
		name     string
		rollup   int64
		dashboad int64
	}{
		{"requests", total.Requests, today.Requests},
		{"request_errors", total.RequestErrors, today.RequestErrors},
		{"attempts", total.Attempts, today.Attempts},
		{"errors", total.Errors, today.Errors},
		{"input_tokens", total.InputTokens, today.InputTokens},
		{"output_tokens", total.OutputTokens, today.OutputTokens},
		{"estimated_input_tokens", total.EstimatedInputTokens, today.EstimatedInputTokens},
		{"estimated_output_tokens", total.EstimatedOutputTokens, today.EstimatedOutputTokens},
		{"cost_micros_usd", total.CostMicrosUSD, today.CostMicrosUSD},
		{"estimated_cost_micros_usd", total.EstimatedCostMicrosUSD, today.EstimatedCostMicrosUSD},
		{"unknown_attempts", total.UnknownAttempts, today.UnknownAttempts},
		{"latency_millis", total.LatencyMillis, today.LatencyMillis},
	} {
		if field.rollup != field.dashboad {
			t.Errorf("%s: rollup=%d dashboard=%d", field.name, field.rollup, field.dashboad)
		}
	}
	// The fixture has to actually exercise the columns, or the comparison above
	// passes by both sides being zero.
	if total.Attempts != 3 || total.Requests != 3 || total.UnknownAttempts != 1 ||
		total.EstimatedCostMicrosUSD != 80 || total.Errors != 1 {
		t.Fatalf("fixture did not exercise the columns: %#v", total)
	}
}

// Estimated tokens and estimated cost are parts of the totals above them, not
// additions to them. Presenting them as a second column that gets added is the
// reconciliation error this schema exists to prevent.
func TestRollupEstimatedColumnsAreSubsets(t *testing.T) {
	aggregate := NewAggregate()
	mixedTraffic(t, aggregate)
	total := rollupRow(t, rollupRows(t, aggregate), domain.RollupDimensionTotal, domain.RollupTotalKey)
	if total.EstimatedInputTokens > total.InputTokens ||
		total.EstimatedOutputTokens > total.OutputTokens ||
		total.EstimatedCostMicrosUSD > total.CostMicrosUSD {
		t.Fatalf("estimated columns are not subsets: %#v", total)
	}
	if total.ProviderCachedInputTokens > total.InputTokens ||
		total.ProviderReasoningTokens > total.OutputTokens+total.InputTokens {
		t.Fatalf("provider token columns are not subsets: %#v", total)
	}
}

// Every dimension partitions the same attempts, so each one has to add back up
// to the total. A dimension that quietly dropped an attempt would show a
// smaller bill than the total and give two answers to one question.
func TestRollupDimensionsSumToTotal(t *testing.T) {
	aggregate := NewAggregate()
	mixedTraffic(t, aggregate)
	rows := rollupRows(t, aggregate)
	total := rollupRow(t, rows, domain.RollupDimensionTotal, domain.RollupTotalKey)

	for _, dimension := range []string{
		domain.RollupDimensionProject, domain.RollupDimensionRequestedModel,
		domain.RollupDimensionProvider, domain.RollupDimensionDeployment,
		domain.RollupDimensionProviderModel,
	} {
		var attempts, cost int64
		for key, row := range rows {
			if key.Dimension == dimension {
				attempts += row.Attempts
				cost += row.CostMicrosUSD
			}
		}
		if attempts != total.Attempts || cost != total.CostMicrosUSD {
			t.Errorf("%s: attempts=%d cost=%d, total attempts=%d cost=%d",
				dimension, attempts, cost, total.Attempts, total.CostMicrosUSD)
		}
	}
}

// Request identity exists only on the finalize event, which names the project
// and the requested model. A provider row claiming a share of a request would
// be inventing one, since a single request can span providers.
func TestRollupRequestMetricsStayOnRequestDimensions(t *testing.T) {
	aggregate := NewAggregate()
	mixedTraffic(t, aggregate)
	rows := rollupRows(t, aggregate)

	if got := rollupRow(t, rows, domain.RollupDimensionProject, "prj_a").Requests; got != 2 {
		t.Fatalf("project requests=%d want 2", got)
	}
	for _, dimension := range []string{
		domain.RollupDimensionProvider, domain.RollupDimensionDeployment, domain.RollupDimensionProviderModel,
	} {
		for key, row := range rows {
			if key.Dimension != dimension {
				continue
			}
			if row.Requests != 0 || row.RequestErrors != 0 || row.RequestLatencySamples != 0 {
				t.Fatalf("%s=%s carries request metrics: %#v", dimension, key.DimensionKey, row)
			}
		}
	}
}

// Crash recovery re-emits a settled event under its original, deterministic
// event id. Apply already refuses the duplicate; the rollup increment lives
// inside Apply so that it is refused with it, rather than being counted by a
// caller that cannot tell a duplicate from a fresh event.
func TestRollupRefusesDuplicateEventIDs(t *testing.T) {
	aggregate := NewAggregate()
	mixedTraffic(t, aggregate)
	before := rollupRow(t, rollupRows(t, aggregate), domain.RollupDimensionTotal, domain.RollupTotalKey)

	replay := ledger.Event{EventID: "a1_settled", Kind: ledger.EventAttemptSettled, RequestID: "req_1",
		AttemptID: "att_1", ProjectID: "prj_a", RequestedModel: "chat", ProviderID: "prov_a",
		PeriodID: rollupDay, PeriodTimezoneVersion: rollupVersion,
		OccurredAt: time.Date(2026, 8, 30, 13, 0, 0, 0, time.UTC), ProviderInputTokens: 100,
		ProviderOutputTokens: 20, CommittedMicrosUSD: ledger.MicrosUSD(120), Outcome: "success"}
	if err := aggregate.Apply(ledger.Record{Sequence: 99, Offset: 9900, Event: replay}); err != nil {
		t.Fatal(err)
	}
	after := rollupRows(t, aggregate)
	if len(after) != 0 {
		t.Fatalf("a duplicate produced a rollup increment: %#v", after)
	}
	if before.Attempts != 3 {
		t.Fatalf("fixture attempts=%d", before.Attempts)
	}
}

// recordLatency drops a sample above the last bucket bound. A stored histogram
// cannot: the count is what a later percentile is computed against, so an
// unbounded tail has to be counted somewhere.
func TestRollupCountsLatencyAboveTheLastBucket(t *testing.T) {
	aggregate := NewAggregate()
	slow := ledger.Event{EventID: "slow", Kind: ledger.EventAttemptSettled, RequestID: "req_slow",
		AttemptID: "att_slow", ProjectID: "prj_a", PeriodID: rollupDay, PeriodTimezoneVersion: rollupVersion,
		OccurredAt: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC),
		// Above LatencyBucketsMillis' last bound of 120s.
		LatencyMillis: 200_000, CommittedMicrosUSD: ledger.MicrosUSD(1), Outcome: "success"}
	if err := aggregate.Apply(ledger.Record{Sequence: 1, Offset: 100, Event: slow}); err != nil {
		t.Fatal(err)
	}
	total := rollupRow(t, rollupRows(t, aggregate), domain.RollupDimensionTotal, domain.RollupTotalKey)
	if total.AttemptLatencyOverflow != 1 || total.AttemptLatencySamples != 1 {
		t.Fatalf("overflow=%d samples=%d", total.AttemptLatencyOverflow, total.AttemptLatencySamples)
	}
	for index, count := range total.AttemptLatencyBuckets {
		if count != 0 {
			t.Fatalf("bucket %d counted an overflowing sample", index)
		}
	}
}

// A failed durable write must not lose the events it covered. The increment is
// handed back and merged with whatever arrived while the write was in flight,
// so the next round persists both.
func TestReturnCheckpointMergesTheUnwrittenIncrement(t *testing.T) {
	aggregate := NewAggregate()
	now := mixedTraffic(t, aggregate)
	snapshot, err := aggregate.TakeCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	later := ledger.Event{EventID: "late", Kind: ledger.EventAttemptSettled, RequestID: "req_late",
		AttemptID: "att_late", ProjectID: "prj_a", PeriodID: rollupDay, PeriodTimezoneVersion: rollupVersion,
		OccurredAt: now.Add(time.Hour), ProviderInputTokens: 1, LatencyMillis: 10,
		CommittedMicrosUSD: ledger.MicrosUSD(5), Outcome: "success"}
	if err := aggregate.Apply(ledger.Record{Sequence: 500, Offset: 50000, Event: later}); err != nil {
		t.Fatal(err)
	}
	if err := aggregate.ReturnCheckpoint(snapshot); err != nil {
		t.Fatal(err)
	}
	total := rollupRow(t, rollupRows(t, aggregate), domain.RollupDimensionTotal, domain.RollupTotalKey)
	if total.Attempts != 4 || total.CostMicrosUSD != 205 {
		t.Fatalf("merged increment lost or duplicated work: %#v", total)
	}
}
