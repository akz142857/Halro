package usage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

// The actual COW pin is kept by a job registered before the collector's own
// catchUpDone. Close must cancel and join that interval, not refund its live
// fee while the callback or actual pin remains active.
func TestUsagePhysicalJobEarlyLifetimeCloseJoinsActualPin(t *testing.T) {
	f := newTypedUsageFixtureAt(t, false, -1)
	p, _, err := BuildTypedUsageProjection(context.Background(), f.options, f.lease)
	if err != nil {
		t.Fatal(err)
	}
	op, finish, err := p.beginPhysicalUsageJob(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pin, err := p.engine.cow.Snapshot(op)
	if err != nil {
		t.Fatal(err)
	}
	before := f.options.Budget.Snapshot().Used
	closed := make(chan error, 1)
	go func() { closed <- p.Close() }()
	select {
	case <-op.Done():
	case <-time.After(time.Second):
		t.Fatal("Close failed to withdraw early job")
	}
	select {
	case err := <-closed:
		t.Fatal("Close returned while actual pin/job still active", err)
	case <-time.After(20 * time.Millisecond):
	}
	if f.options.Budget.Snapshot().Used != before {
		t.Fatal("Close refunded before actual pin joined", before, f.options.Budget.Snapshot())
	}
	if err := pin.Close(); err != nil {
		t.Fatal(err)
	}
	finish()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("joined Close stuck")
	}
	if _, _, err := p.beginPhysicalUsageJob(context.Background()); !errors.Is(err, ErrDiskUsageClosed) {
		t.Fatal("closed job owner was reborrowed", err)
	}
	if err := p.Discard(); err != nil {
		t.Fatal(err)
	}
}

func TestUsagePhysicalUngenuineOwnerRefusedAndDisabledRetirementNoop(t *testing.T) {
	f := newTypedUsageFixtureAt(t, false, -1)
	p, _, err := BuildTypedUsageProjection(context.Background(), f.options, f.lease)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := p.Discard(); err != nil {
			t.Error(err)
		}
	}()
	before := f.options.Budget.Snapshot().Used
	if err := p.EnablePhysicalCatchup(context.Background(), f.native, f.owner, [32]byte{1}); err == nil {
		t.Fatal("fresh nonselected owner acquired physical authority")
	}
	if p.physical != nil || f.options.Budget.Snapshot().Used != before {
		t.Fatal("refusal created an owner or fee")
	}
	if err := f.options.Budget.SetPressure(resourcebudget.Pressured); err != nil {
		t.Fatal(err)
	}
	if err := p.RetirePhysicalCatchupAfterCommon(context.Background(), nil, nil, nil, nil); err != nil {
		t.Fatal("disabled retirement borrowed owner/pressure authority", err)
	}
	if err := f.options.Budget.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
}
