package usage

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

// testChainKey is a fixed 32-byte Ledger HMAC key: every event the ledger
// package writes is promoted to epoch 4 (ADR 0016), which requires a key.
var testChainKey = bytes.Repeat([]byte{0x24}, 32)

func TestCollectorQueueSaturationDoesNotBlockLedgerAndCatchesUpExactly(t *testing.T) {
	log, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "ledger.wal"), nil, ledger.Options{MaxBatch: 1, ChainKey: testChainKey})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	state := ledger.NewState()
	accounting, err := budget.New(log, state, mustResolver(t, "UTC"))
	if err != nil {
		t.Fatal(err)
	}
	aggregate := NewAggregate()
	collector, err := NewCollector(aggregate, log, 1)
	if err != nil {
		t.Fatal(err)
	}
	accounting.AddObserver(collector.Observe)
	for index := 0; index < 100; index++ {
		if _, err := accounting.BeginRequestDetailed(
			context.Background(), "project_1", "key_1",
			fmt.Sprintf("request_%03d", index), "chat",
		); err != nil {
			t.Fatalf("durable append %d was blocked by derivative saturation: %v", index, err)
		}
	}
	before := collector.Stats()
	if before.Dropped == 0 || !before.Lagging || log.Stats().Records != 100 || state.Watermark().Sequence != 100 {
		t.Fatalf("before catch-up collector=%#v ledger=%#v watermark=%#v", before, log.Stats(), state.Watermark())
	}
	if err := collector.CatchUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := collector.Stats()
	if after.QueueDepth != 0 || after.Lagging {
		t.Fatalf("after catch-up=%#v", after)
	}
	expected := NewAggregate()
	if _, err := log.Replay(ledger.Watermark{}, expected.Apply); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(aggregate.Snapshot(), expected.Snapshot()) || aggregate.Metrics() != expected.Metrics() {
		t.Fatalf("catch-up differs from authoritative replay\nactual=%#v\nexpected=%#v", aggregate.Snapshot(), expected.Snapshot())
	}
}

// mustResolver pins the accounting timezone for a test. Period boundaries are
// the subject of the assertions here, so the zone is stated rather than
// inherited from whatever the host is set to.
func mustResolver(t *testing.T, timezone string) *budget.PeriodResolver {
	t.Helper()
	resolver, err := budget.NewFixedPeriodResolver(timezone)
	if err != nil {
		t.Fatalf("resolver for %s: %v", timezone, err)
	}
	return resolver
}

// The rollup is accumulated inside Apply, and CatchUp is one of the paths that
// reaches Apply without going through the collector at all. If a saturated
// queue could drop increments the way it drops notifications, a busy minute
// would leave its cost in no stored row and in no later increment either.
func TestCollectorCatchUpRebuildsTheRollupExactly(t *testing.T) {
	log, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "ledger.wal"), nil, ledger.Options{MaxBatch: 1, ChainKey: testChainKey})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	state := ledger.NewState()
	accounting, err := budget.New(log, state, mustResolver(t, "Asia/Shanghai"))
	if err != nil {
		t.Fatal(err)
	}
	aggregate := NewAggregate()
	// One slot, so the queue saturates and the collector starts dropping
	// notifications rather than blocking the durable path.
	collector, err := NewCollector(aggregate, log, 1)
	if err != nil {
		t.Fatal(err)
	}
	accounting.AddObserver(collector.Observe)
	for index := 0; index < 40; index++ {
		billOneRequestThrough(t, accounting, fmt.Sprintf("request_%03d", index), index)
	}
	if stats := collector.Stats(); stats.Dropped == 0 || !stats.Lagging {
		t.Fatalf("the fixture did not saturate the queue: %#v", stats)
	}
	if err := collector.CatchUp(context.Background()); err != nil {
		t.Fatal(err)
	}

	expected := NewAggregate()
	if _, err := log.Replay(ledger.Watermark{}, expected.Apply); err != nil {
		t.Fatal(err)
	}
	caught, err := aggregate.TakeCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := expected.TakeCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(caught.Rollup, replayed.Rollup) {
		t.Fatalf("catch-up rollup has %d rows, replay has %d", len(caught.Rollup), len(replayed.Rollup))
	}
	if len(caught.Rollup) == 0 {
		t.Fatal("the fixture produced no rollup rows to compare")
	}

	// And the row the whole thing is keyed on agrees with the aggregate it was
	// accumulated beside.
	totals := aggregate.Snapshot().Totals
	var rolledUp domain.DailyRollup
	for encoded, row := range caught.Rollup {
		key, decodeErr := domain.DecodeRollupKey(encoded)
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		if key.Dimension != domain.RollupDimensionTotal {
			continue
		}
		if err := rolledUp.Add(row); err != nil {
			t.Fatal(err)
		}
	}
	if rolledUp.Attempts != totals.Attempts || rolledUp.Requests != totals.Requests ||
		rolledUp.CostMicrosUSD != totals.CostMicrosUSD || rolledUp.InputTokens != totals.InputTokens {
		t.Fatalf("rollup=%#v aggregate totals=%#v", rolledUp, totals)
	}
}

func billOneRequestThrough(t *testing.T, accounting *budget.Manager, requestID string, index int) {
	t.Helper()
	ctx := context.Background()
	request, err := accounting.BeginRequestDetailed(ctx, "project_1", "key_1", requestID, "chat")
	if err != nil {
		t.Fatal(err)
	}
	// The daily budget has to cover every attempt the fixture bills, or the
	// reservations start being refused and the queue never saturates.
	attempt, err := accounting.ReserveAttemptDetailed(ctx, request, 1_000_000, 100, budget.AttemptMetadata{
		RouteID: "route_1", ProviderID: fmt.Sprintf("provider_%d", index%3),
		ProviderModel: "model_1", AttemptNumber: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := accounting.MarkStarted(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	if err := accounting.Settle(ctx, attempt, budget.Settlement{
		CommittedMicrosUSD: int64(10 + index), ProviderInputTokens: 7, ProviderOutputTokens: 3,
		Outcome: "success", LatencyMillis: int64(5 + index),
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounting.Finalize(ctx, request, "success"); err != nil {
		t.Fatal(err)
	}
}
