package app

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/usage"
	"github.com/akz142857/Halro/internal/vault"
)

// billOneRequest drives one complete accounting cycle through the runtime, so
// the rollup is written by the same path production uses rather than by a
// hand-built event.
func billOneRequest(t *testing.T, runtime *Runtime, requestID string) {
	t.Helper()
	request, err := runtime.accounting.BeginRequestDetailed(
		context.Background(), "project_1", "key_1", requestID, "chat",
	)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := runtime.accounting.ReserveAttemptDetailed(
		context.Background(), request, 1_000, 100,
		budget.AttemptMetadata{
			RouteID: "route_1", ProviderID: "provider_1", ProviderModel: "model_1", AttemptNumber: 1,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.accounting.MarkStarted(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	if err := runtime.accounting.Settle(context.Background(), attempt, budget.Settlement{
		CommittedMicrosUSD: 90, ProviderInputTokens: 7, ProviderOutputTokens: 3,
		Outcome: "success", LatencyMillis: 12,
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.accounting.Finalize(context.Background(), request, "success"); err != nil {
		t.Fatal(err)
	}
}

func allRollupRows(t *testing.T, cfg config.Config) map[domain.RollupKey]domain.DailyRollup {
	t.Helper()
	ledgerKey := ledgerKeyForTest(t, cfg)
	defer clear(ledgerKey)
	key, err := vault.DeriveHistoryHMACKey(ledgerKey)
	if err != nil {
		t.Fatal(err)
	}
	history, _, err := usage.OpenHistory(cfg.HistoryPath(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()
	rows := map[domain.RollupKey]domain.DailyRollup{}
	if _, err := history.RollupRange("", "", func(key domain.RollupKey, row domain.DailyRollup) error {
		rows[key] = row
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return rows
}

// The claim the whole design rests on: the rows written a batch at a time as
// traffic arrives are the same rows a single pass over the Ledger produces.
// If the two ever differ, the stored summary is a second set of numbers with no
// way to say which one is the accounting truth.
func TestRebuildUsageSummaryReproducesTheIncrementalRollup(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	// Several checkpoint rounds, so the incremental path writes the day in
	// pieces rather than in one increment that trivially matches a single pass.
	for _, id := range []string{"request_1", "request_2"} {
		billOneRequest(t, runtime, id)
		syncUsageHistory(t, runtime)
	}
	billOneRequest(t, runtime, "request_3")
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	incremental := allRollupRows(t, cfg)
	if len(incremental) == 0 {
		t.Fatal("the incremental path wrote nothing to compare against")
	}

	report, err := RebuildUsageSummary(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if report.RollupRows != len(incremental) || report.AccountingDays != 1 ||
		report.RollupVersion != domain.RollupVersion || report.Watermark.Sequence == 0 {
		t.Fatalf("report=%#v incremental rows=%d", report, len(incremental))
	}
	rebuilt := allRollupRows(t, cfg)
	if !reflect.DeepEqual(rebuilt, incremental) {
		for key, row := range incremental {
			if other := rebuilt[key]; other != row {
				t.Errorf("%s/%s: incremental=%#v rebuilt=%#v", key.Dimension, key.DimensionKey, row, other)
			}
		}
		t.Fatalf("rebuild produced %d rows, incremental produced %d", len(rebuilt), len(incremental))
	}

	// And the next start resumes from the rebuilt index rather than adding to
	// it.
	reopened, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	syncUsageHistory(t, reopened)
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if after := allRollupRows(t, cfg); !reflect.DeepEqual(after, incremental) {
		t.Fatalf("the start after a rebuild changed the rollup: %#v", after)
	}
}
