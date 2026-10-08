//go:build linux

package ledger

import (
	"context"
	"errors"
	"testing"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestPhysicalRollRecoveryIndexesRejectAcceptedFactsWithZeroCounter(t *testing.T) {
	p, native, _ := physicalNativeFixture(t)
	ctx := context.Background()
	if native.PhysicalTerminalDebt() != 0 {
		t.Fatal("fixture should have zero physical counter")
	}
	before := p.budget.Snapshot().Used
	native.mu.Lock()
	err := native.verifyPhysicalRollRecoveryIndexesLocked(ctx)
	native.mu.Unlock()
	if !errors.Is(err, resourcebudget.ErrPhysicalAllocation) || native.status.Load() != AccountingHealthy || p.budget.Snapshot().Used != before {
		t.Fatal("accepted pending facts were erased by zero physical counter", err, native.status.Load())
	}
	events := projectionTestLifecycle(1, false)
	for _, e := range events[3:] {
		if err := appendPhysicalFixture(p, native, ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	native.mu.Lock()
	err = native.verifyPhysicalRollRecoveryIndexesLocked(ctx)
	native.mu.Unlock()
	if err != nil {
		t.Fatal("actual settled and finalized indexes did not prove NoDebt", err)
	}
}
