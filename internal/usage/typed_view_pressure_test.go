package usage

import (
	"context"
	"errors"
	"testing"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

// Board §40.4/§40.6: pressure that arrives after a View pinned its snapshot
// refuses the page read. A refused read is not evidence about the rows, so the
// View reports the refusal and not ErrDiskUsageCorrupt; the owner stays usable.
func TestTypedViewReportsRefusedReadNotCorruption(t *testing.T) {
	f, _, p := liveWorkspaceFixture(t)
	ctx := context.Background()
	p.engine.pinned = func() {
		if err := f.options.Budget.SetPressure(resourcebudget.Pressured); err != nil {
			t.Fatal(err)
		}
	}
	err := p.engine.scoped(ctx).View(func(*usageTx) error { return nil })
	p.engine.pinned = nil
	if !errors.Is(err, resourcebudget.ErrPressure) || errors.Is(err, ErrDiskUsageCorrupt) {
		t.Fatal("refused read reported as corruption", err)
	}
	if err := f.options.Budget.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ReadStats(ctx); err != nil || p.retired.Load() {
		t.Fatal("owner unusable after a refused read", p.retired.Load(), err)
	}
}
