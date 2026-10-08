package usage

import (
	"context"
	"errors"
	"testing"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func liveWorkspaceFixture(t *testing.T) (*typedUsageFixture, *LedgerDerivative, *DiskProjection) {
	t.Helper()
	f := newTypedUsageFixtureAt(t, false, -1)
	d, p, _, err := BuildTypedDiskLedgerDerivative(context.Background(), f.options, f.lease)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d.Close()
		if err := p.Discard(); err != nil {
			t.Error(err)
		}
	})
	if err := p.ActivateTypedLive(context.Background(), f.native, f.options.Budget); err != nil {
		t.Fatal(err)
	}
	if err := f.lease.Close(); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range finalizationFixture(false, false) {
		if _, _, err := f.native.AppendApplyRecordWithReceipt(context.Background(), fixture.Event, func(ctx context.Context, r ledger.Record) error {
			_, err := f.owner.AdvanceOneFromNative(ctx, f.native, ledger.ProjectionPrefix{Head: typedUsageRecordAnchor(r).Head, Hash: r.Hash})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	return f, d, p
}

func TestTypedLiveWorkspacePressureAfterFirstAcceptedReduceFinishesOwnedIO(t *testing.T) {
	ctx := context.Background()
	f, d, p := liveWorkspaceFixture(t)
	before := f.options.Budget.Snapshot().Used
	pressured := false
	receipt, err := d.catchUpTypedSourceWithPhase(ctx, f.native, f.options.Budget, nil, func(stage string) {
		if stage == "record-applied" && !pressured {
			pressured = true
			if err := f.options.Budget.SetPressure(resourcebudget.Pressured); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err != nil || !pressured || receipt.AppliedRecords == 0 || receipt.To.Head != receipt.NativeHead || p.retired.Load() || p.engine.cow.NativeFrameRecoveryDebt() != nil {
		t.Fatal("accepted job lost prepared completion IO", receipt, pressured, p.retired.Load(), err)
	}
	if f.options.Budget.Snapshot().Used != before {
		t.Fatal("known joined workspace leaked fees", before, f.options.Budget.Snapshot())
	}
	if err := f.options.Budget.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ReadStats(ctx); err != nil {
		t.Fatal("healthy owner did not remain readable", err)
	}
	if retry, err := d.CatchUpTypedLive(ctx, f.native, f.options.Budget); err != nil || retry.AppliedRecords != 0 {
		t.Fatal(retry, err)
	}
}

func TestTypedLiveWorkspacePrewriteRefusalKeepsActualRootAndRetry(t *testing.T) {
	f, d, p := liveWorkspaceFixture(t)
	ctx := context.Background()
	before, err := p.engine.cow.RootIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fees := f.options.Budget.Snapshot().Used
	if err := f.options.Budget.SetPressure(resourcebudget.Pressured); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CatchUpTypedLive(ctx, f.native, f.options.Budget); !errors.Is(err, resourcebudget.ErrPressure) {
		t.Fatal(err)
	}
	if p.retired.Load() || p.engine.cow.NativeFrameRecoveryDebt() != nil || f.options.Budget.Snapshot().Used != fees {
		t.Fatal("pre-admission refusal poisoned or refunded actual owner", f.options.Budget.Snapshot())
	}
	if err := f.options.Budget.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	after, err := p.engine.cow.RootIdentity(ctx)
	if err != nil || before != after {
		t.Fatal(before, after, err)
	}
	if receipt, err := d.CatchUpTypedLive(ctx, f.native, f.options.Budget); err != nil || receipt.AppliedRecords == 0 {
		t.Fatal(receipt, err)
	}
}

func TestTypedLiveActualPartialCOWWriteWithdrawsOwnerAndPreservesCause(t *testing.T) {
	f, d, p := liveWorkspaceFixture(t)
	ctx := context.Background()
	current, err := p.engine.cow.RootIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("actual callback failed after page append")
	if _, err := p.engine.cow.UpdateDerived(ctx, current.Cut, func(tx *authenticatedstate.Transaction) error {
		if err := tx.Put([]byte("actual-private-partial"), []byte("written")); err != nil {
			return err
		}
		return injected
	}, func() error { return nil }); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	if err := p.engine.cow.NativeFrameRecoveryDebt(); !errors.Is(err, authenticatedstate.ErrRecovery) || !errors.Is(err, injected) {
		t.Fatal("actual partial write debt absent", err)
	}
	if _, err := d.CatchUpTypedLive(ctx, f.native, f.options.Budget); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	if !p.retired.Load() {
		t.Fatal("poisoned real COW resumed")
	}
	if _, err := p.ReadStats(ctx); !errors.Is(err, ErrDiskUsageClosed) || !errors.Is(err, injected) {
		t.Fatal("actual retirement cause erased", err)
	}
}

func TestHASelectedStartupCatchUpRefusesStandaloneLeaseWithoutClaims(t *testing.T) {
	f := newTypedUsageFixtureAt(t, false, -1)
	d, p, _, err := BuildTypedDiskLedgerDerivative(context.Background(), f.options, f.lease)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	defer p.Discard()
	before := f.options.Budget.Snapshot().Used
	if _, err := d.CatchUpHASelectedStartup(context.Background(), f.native, f.lease, f.options.Budget); err == nil {
		t.Fatal("Standalone lease impersonated actual HA startup")
	}
	if _, err := d.CatchUpHASelectedStartup(context.Background(), f.native, nil, f.options.Budget); err == nil {
		t.Fatal("absent lease granted actual HA startup")
	}
	if f.options.Budget.Snapshot().Used != before || p.retired.Load() {
		t.Fatal("refusal changed valid source lifetime")
	}
}
