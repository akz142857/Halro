package ledger

import (
	"context"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"path/filepath"
	"testing"
)

func TestHotScratchLedgerInterleavedEvictionAllRowsMatchOrdinary(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "native.wal")
	native := openChained(t, path, nil)
	// A step-wide schedule holds far more mutable request/pending keys than the
	// fixed pool. Settlement deletion and finalization cross builder batches.
	for step := 0; step < 5; step++ {
		for i := 1; i <= 256; i++ {
			event := projectionTestLifecycle(i, false)[step]
			if i%3 == 0 && step == 3 {
				event = projectionTestLifecycle(i, false)[4]
			}
			if i%3 == 0 && step == 4 {
				event = projectionTestLifecycle(i, false)[3]
			}
			if _, err := native.Append(ctx, event); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := native.Close(); err != nil {
		t.Fatal(err)
	}
	report, partial, err := InspectReplayAuthenticated(path, testChainKey, nil)
	if err != nil || partial {
		t.Fatal(err, partial)
	}
	opts := authenticatedLedgerTestOptions(t)
	before := opts.Budget.Snapshot().Used
	p, err := buildFixedPackedAuthenticatedLedgerProjection(ctx, path, testChainKey, filepath.Join(t.TempDir(), "fixed"), report, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Discard()
	reference, err := BuildLedgerProjection(ctx, path, testChainKey, filepath.Join(t.TempDir(), "reference.db"), report)
	if err != nil {
		t.Fatal(err)
	}
	defer reference.Discard()
	if rows := compareAuthenticatedLedgerRows(t, p, reference); rows < 3000 {
		t.Fatal("missing historical rows", rows)
	}
	if err := p.Discard(); err != nil {
		t.Fatal(err)
	}
	if got := opts.Budget.Snapshot().Used; got != before {
		t.Fatal("workspace survived actual retirement", before, got)
	}
	if got := opts.Budget.Snapshot().Classes[resourcebudget.IO].RAM; got != 0 {
		t.Fatal("unreturned workspace", got)
	}
}
