package ledger

import (
	"context"
	"errors"
	"testing"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// This verifies only actual authenticated indexes and pinned lifecycle. It
// grants no physical allocation or selected Runtime qualification on this OS.
func TestPhysicalRollRecoveryProofKeepsActualSourcePositionAndBusyLifetime(t *testing.T) {
	ctx := context.Background()
	p, native, records := authenticatedAdvanceFixture(t, 0)
	initialCount := len(records)
	before := p.budget.Snapshot().Used
	check := func() error {
		native.mu.Lock()
		defer native.mu.Unlock()
		return p.verifyPhysicalRollRecoveryIndexesLocked(ctx, native)
	}
	if err := check(); !errors.Is(err, resourcebudget.ErrPhysicalAllocation) {
		t.Fatal("actual accepted request treated as NoDebt", err)
	}
	p.mu.Lock()
	if err := check(); !errors.Is(err, authenticatedstate.ErrBusy) {
		t.Fatal("Roll waited behind the actual projection owner", err)
	}
	p.mu.Unlock()
	for _, e := range projectionTestLifecycle(1, true)[1:] {
		r, _, err := native.AppendRecordWithReceipt(ctx, e)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	if err := check(); !errors.Is(err, ErrProjectionSource) {
		t.Fatal("stale authenticated source position qualified later native", err)
	}
	for _, r := range records[initialCount:] {
		if _, err := p.AdvanceOneFromNative(ctx, native, nativeRecordPrefix(r)); err != nil {
			t.Fatal(err)
		}
	}
	if err := check(); err != nil {
		t.Fatal("actual finalized indexes refused", err)
	}
	if got := p.budget.Snapshot().Used; got != before {
		t.Fatal("bounded proof leaked its pinned lifecycle", before, got)
	}
}
