package app

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/usage"
)

// historyOf builds a usage history from the fixture's Ledger, up to and
// including sequence through (all of it when zero).
func historyOf(t *testing.T, f *accountingFixture, through uint64) *usage.HistoryStore {
	t.Helper()
	history, aggregate, err := openFreshHistory(filepath.Join(t.TempDir(), "history.db"), ledgerStateTestKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { history.Close() })
	if _, err := f.log.Replay(ledger.Watermark{}, func(record ledger.Record) error {
		if through != 0 && record.Sequence > through {
			return nil
		}
		return aggregate.Apply(record)
	}); err != nil {
		t.Fatal(err)
	}
	if err := aggregate.Flush(); err != nil {
		t.Fatal(err)
	}
	return history
}

// The history's daily totals agree with the accounting totals when it holds
// every settlement, and disagree when one is missing — which is what a deleted
// row looks like, and what its MAC cannot show.
func TestReconciliationFindsAMissingSettlement(t *testing.T) {
	f := newAccountingFixture(t)
	defer func() { f.log.Close() }()
	f.bill(4)
	totals, open, watermark, err := f.state.PeriodTotals()
	if err != nil {
		t.Fatal(err)
	}
	checked, mismatched, err := reconcilePeriods(totals, open, historyOf(t, f, 0), "")
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 || len(mismatched) != 0 {
		t.Fatalf("complete history: checked=%d mismatched=%v", checked, mismatched)
	}
	// Each billed request is five events; leaving out the last three drops
	// the final settlement from the history.
	_, mismatched, err = reconcilePeriods(totals, open, historyOf(t, f, watermark.Sequence-3), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(mismatched) != 1 {
		t.Fatalf("history missing a settlement: mismatched=%v", mismatched)
	}
}

// A day a lease is still open against is not compared: its settlement has
// reached neither side yet, and may reach one before the other.
func TestReconciliationSkipsADayWithAnOpenLease(t *testing.T) {
	f := newAccountingFixture(t)
	defer func() { f.log.Close() }()
	f.bill(2)
	ctx := context.Background()
	request, err := f.manager.BeginRequestDetailed(ctx, "project_1", "key_1", "req_open", "chat")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.ReserveAttemptDetailed(ctx, request, 1_000_000, 100, budget.AttemptMetadata{
		RouteID: "route_1", ProviderID: "provider_1", ProviderModel: "model_1", AttemptNumber: 1,
	}); err != nil {
		t.Fatal(err)
	}
	totals, open, _, err := f.state.PeriodTotals()
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("open periods = %v", open)
	}
	checked, mismatched, err := reconcilePeriods(totals, open, historyOf(t, f, 1), "")
	if err != nil {
		t.Fatal(err)
	}
	if checked != 0 || len(mismatched) != 0 {
		t.Fatalf("open day was compared: checked=%d mismatched=%v", checked, mismatched)
	}
}
