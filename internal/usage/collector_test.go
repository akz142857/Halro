package usage

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/akz142857/Heimdall/internal/budget"
	"github.com/akz142857/Heimdall/internal/ledger"
)

func TestCollectorQueueSaturationDoesNotBlockLedgerAndCatchesUpExactly(t *testing.T) {
	log, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "ledger.wal"), nil, ledger.Options{MaxBatch: 1})
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
