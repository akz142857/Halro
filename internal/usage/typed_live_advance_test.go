package usage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ledger"
)

func TestTypedUsageLiveCatchUpAfterMaintenanceReleasePreservesBothOrders(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "late"}[late], func(t *testing.T) {
			ctx := context.Background()
			f := newTypedUsageFixtureAt(t, late, -1)
			d, p, _, err := BuildTypedDiskLedgerDerivative(ctx, f.options, f.lease)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				d.Close()
				if err := p.Discard(); err != nil {
					t.Error(err)
				}
			})
			ordinary, db, err := NewDiskLedgerDerivativeWithLimit(f.owner, filepath.Join(t.TempDir(), "ordinary.db"), 32<<20)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ordinary.Close()
				if err := db.Discard(); err != nil {
					t.Error(err)
				}
			})
			if err := p.ActivateTypedLive(ctx, f.native, f.options.Budget); err != nil {
				t.Fatal(err)
			}
			if err := f.lease.Close(); err != nil {
				t.Fatal(err)
			}
			collector, err := NewTypedLiveCollector(d, f.native, f.options.Budget, 4)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := collector.Retire(ctx); err != nil {
					t.Error(err)
				}
			})
			for _, fixture := range finalizationFixture(late, false) {
				r, _, err := f.native.AppendApplyRecordWithReceipt(ctx, fixture.Event, func(ctx context.Context, r ledger.Record) error {
					_, err := f.owner.AdvanceOneFromNative(ctx, f.native, ledger.ProjectionPrefix{Head: typedUsageRecordAnchor(r).Head, Hash: r.Hash})
					return err
				})
				if err != nil {
					t.Fatal("ordinary durable Apply", err)
				}
				if err := ordinary.ApplyContext(ctx, r); err != nil {
					t.Fatal(err)
				}
				advance, err := d.CatchUpTypedLive(ctx, f.native, f.options.Budget)
				if err != nil || advance.AppliedRecords != 1 || advance.To.Head.Sequence != r.Sequence {
					t.Fatal("live async catch-up", advance, err)
				}
			}
			compareTypedUsageRows(t, p, db)
			before, err := p.TypedSealReference(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if retry, err := d.CatchUpTypedLive(ctx, f.native, f.options.Budget); err != nil || retry.AppliedRecords != 0 {
				t.Fatal("live retry", retry, err)
			}
			if after, err := p.TypedSealReference(ctx); err != nil || after != before {
				t.Fatal("empty live retry changed root", err)
			}
			collector.Observe(ledger.Record{})
			if err := collector.CatchUp(ctx); err != nil || collector.Stats().Lagging {
				t.Fatal("actual typed collector could not join coherent job", err)
			}
		})
	}
}

func TestTypedUsageLiveCatchUpStopsAtActualLedgerOwnerNotDurableTail(t *testing.T) {
	ctx := context.Background()
	f := newTypedUsageFixtureAt(t, false, -1)
	d, p, _, err := BuildTypedDiskLedgerDerivative(ctx, f.options, f.lease)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d.Close()
		if err := p.Discard(); err != nil {
			t.Error(err)
		}
	})
	if err := p.ActivateTypedLive(ctx, f.native, f.options.Budget); err != nil {
		t.Fatal(err)
	}
	if err := f.lease.Close(); err != nil {
		t.Fatal(err)
	}
	for i, fixture := range finalizationFixture(false, false) {
		r, _, err := f.native.AppendApplyRecordWithReceipt(ctx, fixture.Event, func(ctx context.Context, r ledger.Record) error {
			if i >= 2 {
				return nil
			}
			_, err := f.owner.AdvanceOneFromNative(ctx, f.native, ledger.ProjectionPrefix{Head: typedUsageRecordAnchor(r).Head, Hash: r.Hash})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		f.records = append(f.records, r)
	}
	if receipt, err := d.CatchUpTypedLive(ctx, f.native, f.options.Budget); err != nil || receipt.AppliedRecords != 2 || receipt.To.Head.Sequence != 2 || receipt.NativeHead.Sequence <= 2 {
		t.Fatal("live source credited unselected durable facts", receipt, err)
	}
	var escaped ledger.AuthenticatedUsageSource
	ref, err := f.owner.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.owner.WithFrozenUsageSource(ctx, f.native, f.options.Budget, ref.Scope, func(_ context.Context, source ledger.AuthenticatedUsageSource) error { escaped = source; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, _, err := escaped.ReadEventPosition(ctx, f.records[0].Event.EventID); !errors.Is(err, ledger.ErrProjectionUnavailable) {
		t.Fatal("escaped dependency read survived actual job", err)
	}
	if _, err := escaped.ReplayAuthenticated(ctx, ledger.ReplayAnchor{}, nil); !errors.Is(err, ledger.ErrProjectionUnavailable) {
		t.Fatal("escaped actual native replay survived job", err)
	}
}
