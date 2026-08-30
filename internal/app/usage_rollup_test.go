package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"testing"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	bbolt "go.etcd.io/bbolt"
)

// stampForeignRollup makes the stored rollup look like one written by a build
// with a different row structure: the state names a version this build does not
// know, and the rows hold numbers this build's decoder does not agree with.
// Both halves matter — a version marker alone proves nothing, because rows that
// happen to still be correct stay correct whether or not they are trusted.
func stampForeignRollup(t *testing.T, metadataPath string, watermark ledger.Watermark, day domain.RollupKey) {
	t.Helper()
	db, err := bbolt.Open(metadataPath, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	state, err := json.Marshal(boltstore.UsageRollupState{
		Version: domain.RollupVersion + 1, Watermark: watermark,
	})
	if err != nil {
		t.Fatal(err)
	}
	garbled := domain.DailyRollup{
		Version: domain.RollupVersion + 1, PeriodID: day.PeriodID, TimezoneVersion: day.TimezoneVersion,
		Attempts: 99, Requests: 99, CostMicrosUSD: 9_900,
	}
	row, err := json.Marshal(garbled)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		if err := tx.Bucket([]byte("meta")).Put([]byte("usage_rollup_state"), state); err != nil {
			return err
		}
		return tx.Bucket([]byte("usage_daily_rollup")).Put([]byte(day.Encode()), row)
	}); err != nil {
		t.Fatal(err)
	}
}

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

// readOneRollupKey finds the stored total row's key, so a test can rewrite that
// exact row rather than guess at how the key was encoded.
func readOneRollupKey(t *testing.T, metadataPath string, out *domain.RollupKey) error {
	t.Helper()
	store, err := boltstore.Open(metadataPath)
	if err != nil {
		return err
	}
	defer store.Close()
	return store.UsageRollupRange("", "", func(key domain.RollupKey, _ domain.DailyRollup) error {
		if key.Dimension == domain.RollupDimensionTotal && out.PeriodID == "" {
			*out = key
		}
		return nil
	})
}

func storedRollupTotal(t *testing.T, metadataPath string) domain.DailyRollup {
	t.Helper()
	store, err := boltstore.Open(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var total domain.DailyRollup
	if err := store.UsageRollupRange("", "", func(key domain.RollupKey, row domain.DailyRollup) error {
		if key.Dimension != domain.RollupDimensionTotal {
			return nil
		}
		return total.Add(row)
	}); err != nil {
		t.Fatal(err)
	}
	return total
}

// The failure this guards is silent by construction: a discarded checkpoint
// replays the whole WAL from zero, and a rollup left on disk would count every
// stored row a second time without any error being reported. The two
// derivatives are therefore discarded together.
func TestRejectedUsageCheckpointDoesNotDoubleTheRollup(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	billOneRequest(t, runtime, "request_1")
	runtime.saveUsageCheckpoint()
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	single := storedRollupTotal(t, cfg.MetadataPath())
	if single.Attempts != 1 || single.Requests != 1 || single.CostMicrosUSD != 90 {
		t.Fatalf("first pass rollup=%#v", single)
	}

	// A checkpoint the next start cannot read: the payload no longer carries a
	// version it accepts. The rollup rows on disk are untouched and complete.
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	watermark, _, err := store.UsageCheckpoint()
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.PutUsageCheckpoint(watermark, []byte(`{"version":1}`), domain.RollupVersion, nil); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	reopened.saveUsageCheckpoint()
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	rebuilt := storedRollupTotal(t, cfg.MetadataPath())
	if rebuilt.Attempts != single.Attempts || rebuilt.Requests != single.Requests ||
		rebuilt.CostMicrosUSD != single.CostMicrosUSD {
		t.Fatalf("rebuilt rollup=%#v want=%#v", rebuilt, single)
	}
}

// The reverse gap: a rollup this build cannot read must not be repaired from
// the suffix of the WAL its still-valid checkpoint points at, because that
// prefix was never counted.
func TestUnreadableRollupForcesAFullRebuild(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	billOneRequest(t, runtime, "request_1")
	runtime.saveUsageCheckpoint()
	billOneRequest(t, runtime, "request_2")
	runtime.saveUsageCheckpoint()
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	expected := storedRollupTotal(t, cfg.MetadataPath())
	if expected.Attempts != 2 {
		t.Fatalf("fixture rollup=%#v", expected)
	}

	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.UsageRollupState()
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if state.Version != domain.RollupVersion || state.Watermark.Sequence == 0 {
		store.Close()
		t.Fatalf("rollup state=%#v", state)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// What an instance rolled back from a newer build finds on disk. There is
	// no API for writing a foreign version, which is the point: the file is
	// edited the way the other build would have written it.
	var day domain.RollupKey
	if err := readOneRollupKey(t, cfg.MetadataPath(), &day); err != nil {
		t.Fatal(err)
	}
	stampForeignRollup(t, cfg.MetadataPath(), state.Watermark, day)

	reopened, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	reopened.saveUsageCheckpoint()
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	rebuilt := storedRollupTotal(t, cfg.MetadataPath())
	if rebuilt.Attempts != expected.Attempts || rebuilt.CostMicrosUSD != expected.CostMicrosUSD {
		t.Fatalf("rebuilt rollup=%#v want=%#v", rebuilt, expected)
	}
}

func allRollupRows(t *testing.T, metadataPath string) map[domain.RollupKey]domain.DailyRollup {
	t.Helper()
	store, err := boltstore.Open(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rows := map[domain.RollupKey]domain.DailyRollup{}
	if err := store.UsageRollupRange("", "", func(key domain.RollupKey, row domain.DailyRollup) error {
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
		runtime.saveUsageCheckpoint()
	}
	billOneRequest(t, runtime, "request_3")
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	incremental := allRollupRows(t, cfg.MetadataPath())
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
	rebuilt := allRollupRows(t, cfg.MetadataPath())
	if !reflect.DeepEqual(rebuilt, incremental) {
		for key, row := range incremental {
			if other := rebuilt[key]; other != row {
				t.Errorf("%s/%s: incremental=%#v rebuilt=%#v", key.Dimension, key.DimensionKey, row, other)
			}
		}
		t.Fatalf("rebuild produced %d rows, incremental produced %d", len(rebuilt), len(incremental))
	}

	// A rebuild that left the two derivatives at different positions would be
	// undone by the next start, which discards both when they disagree.
	reopened, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	reopened.saveUsageCheckpoint()
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if after := allRollupRows(t, cfg.MetadataPath()); !reflect.DeepEqual(after, incremental) {
		t.Fatalf("the start after a rebuild changed the rollup: %#v", after)
	}
}
