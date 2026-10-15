package app

import (
	"context"
	"github.com/akz142857/Halro/internal/usage"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/akz142857/Halro/internal/ledger"
)

// seedLedgerActivity writes one accepted request so the WAL holds at least one
// epoch-4 frame and the startup reconciliation has a non-zero chain head to
// checkpoint.
func seedLedgerActivity(t *testing.T, runtime *Runtime) {
	t.Helper()
	request, err := runtime.accounting.BeginRequestDetailed(
		context.Background(), "project_1", "key_1", "request_1", "chat",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.accounting.Finalize(context.Background(), request, "success"); err != nil {
		t.Fatal(err)
	}
}

// TestDeletedLedgerIsRejectedByChainCheckpoint is the ledger's version of
// TestAuditCheckpointDetectsDeletedSuffix, for the most complete truncation
// there is. Deleting the WAL outright is easier than editing it and pays
// better: the file is recreated empty on the next open, every spent
// reservation disappears with it, and the budget starts over. Per-frame MACs
// cannot see this — there are no frames left to check. Only the checkpoint in
// bbolt still remembers that a chain existed, so the reconciliation has to
// consult it even when the chain it is comparing against is empty.
func TestDeletedLedgerIsRejectedByChainCheckpoint(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	seedLedgerActivity(t, runtime)
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopen once so startup reconciliation persists the chain head; this is
	// what a real instance does on every restart.
	runtime, err = Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(cfg.LedgerPath()); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		reopened.Close()
		t.Fatal("a deleted ledger must not start against a non-zero chain checkpoint")
	}

	// The operator-facing command has to agree with the startup gate. A
	// verify that reports success on a wiped ledger is worse than no verify at
	// all, because it is the command someone runs precisely when they suspect
	// this.
	if _, err := VerifyLedger(context.Background(), cfg); err == nil {
		t.Fatal("halro ledger verify must fail on a deleted ledger with a non-zero checkpoint")
	}
}

// TestUsageHistoryAheadOfLedgerHeadIsDiscarded covers the second-order damage
// from the same wipe. The usage history index keeps its own watermark, and
// resuming from an offset the shortened WAL no longer reaches makes the next
// replay land in the middle of whatever gets written next — surfacing minutes
// later as corruption, far away from the deletion that caused it. The index
// is a derivative, so discarding it and rebuilding is always available and
// always correct.
func TestUsageHistoryAheadOfLedgerHeadIsDiscarded(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	request, err := runtime.accounting.BeginRequestDetailed(
		context.Background(), "project_1", "key_1", "request_1", "chat",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.accounting.Finalize(context.Background(), request, "success"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.usageCollector.CatchUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runtime.usage.HistoryStats().Committed.Offset == 0 {
		t.Fatal("expected a non-zero usage watermark to have been stored")
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	// An empty head is what a wiped WAL replays to. The index recorded a real
	// offset a moment ago, so it is now describing bytes that are gone.
	ledgerKey := ledgerKeyForTest(t, cfg)
	defer clear(ledgerKey)
	history, restored, err := openUsageHistory(cfg, ledgerKey, ledger.Watermark{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()
	if restored.Watermark().Offset != 0 {
		t.Fatalf("usage history ahead of the ledger head must be discarded: watermark=%+v", restored.Watermark())
	}
	if _, err := history.State(); !usage.IsHistoryEmpty(err) {
		t.Fatalf("the discarded index still holds a position: %v", err)
	}
}
