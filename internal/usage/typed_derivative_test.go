package usage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
)

func TestTypedUsageActualMaintenanceDerivativeBeforeLedgerApplyPreservesBothOrders(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "late"}[late], func(t *testing.T) {
			f := newTypedUsageFixtureAt(t, late, -1)
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
			for _, record := range finalizationFixture(late, false) {
				actual, _, err := f.lease.AppendApply(context.Background(), record.Event, func(ctx context.Context, r ledger.Record) error {
					before, _, err := f.owner.ReadPosition(ctx)
					if err != nil {
						return err
					}
					if r.Sequence != before.Sequence+1 {
						t.Fatal("not derivative-before-Ledger-Apply", before, r.Sequence)
					}
					if err := d.ApplyContext(ctx, r); err != nil {
						return err
					}
					root, err := p.engine.cow.RootIdentity(ctx)
					if err != nil {
						return err
					}
					if err := d.ApplyContext(ctx, r); err != nil {
						return err
					}
					retry, err := p.engine.cow.RootIdentity(ctx)
					if err != nil || retry != root {
						t.Fatal("exact current retry republished root", root, retry, err)
					}
					if err := ordinary.ApplyContext(ctx, r); err != nil {
						return err
					}
					_, err = f.owner.AdvanceOneFromNative(ctx, f.native, ledger.ProjectionPrefix{Head: typedUsageRecordAnchor(r).Head, Hash: r.Hash})
					return err
				})
				if err != nil {
					t.Fatal("live actual maintenance fact", actual.Sequence, err)
				}
			}
			compareTypedUsageRows(t, p, db)
			ref, err := p.TypedSealReference(context.Background())
			if err != nil {
				t.Fatal("completed cut cannot seal", err)
			}
			head, hash, err := f.owner.ReadPosition(context.Background())
			if err != nil || ref.Ledger.Anchor.Head != head || ref.Ledger.Anchor.Hash != hash {
				t.Fatal("Usage/Ledger split final cut", ref, head, err)
			}
		})
	}
}

func TestTypedUsageAcceptedScopeStillRejectsCopiedHashForgedPayload(t *testing.T) {
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
	_, _, err = f.lease.AppendApply(context.Background(), finalizationFixture(false, false)[0].Event, func(ctx context.Context, r ledger.Record) error {
		root, err := p.engine.cow.RootIdentity(ctx)
		if err != nil {
			return err
		}
		forged := r
		forged.Event.ProjectID = "forged-project"
		if err := d.ApplyContext(ctx, forged); !errors.Is(err, ErrDiskUsageCorrupt) {
			t.Fatal("opaque current boundary authorized caller payload", err)
		}
		after, err := p.engine.cow.RootIdentity(ctx)
		if err != nil || after != root || p.retired.Load() {
			t.Fatal("forgery changed/retired real candidate", after, root, err)
		}
		if err := d.ApplyContext(ctx, r); err != nil {
			return err
		}
		_, err = f.owner.AdvanceOneFromNative(ctx, f.native, ledger.ProjectionPrefix{Head: typedUsageRecordAnchor(r).Head, Hash: r.Hash})
		return err
	})
	if err != nil || f.native.Status().Load() != ledger.AccountingHealthy {
		t.Fatal("valid actual fact could not complete", err)
	}
}

func TestTypedUsageWithoutActualAppendScopeCannotLeadSelectedOwner(t *testing.T) {
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
	actual, _, err := f.lease.AppendApply(context.Background(), finalizationFixture(false, false)[0].Event, func(context.Context, ledger.Record) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	before, err := p.engine.cow.RootIdentity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ApplyContext(context.Background(), actual); !errors.Is(err, authenticatedstate.ErrRecovery) {
		t.Fatal("raw native hash granted pre-Apply permission", err)
	}
	after, err := p.engine.cow.RootIdentity(context.Background())
	if err != nil || before != after || p.retired.Load() {
		t.Fatal("input rejection changed root", before, after, err)
	}
	if _, err := f.owner.AdvanceOneFromNative(context.Background(), f.native, ledger.ProjectionPrefix{Head: typedUsageRecordAnchor(actual).Head, Hash: actual.Hash}); err != nil {
		t.Fatal(err)
	}
	if err := d.ApplyContext(context.Background(), actual); err != nil {
		t.Fatal("already selected native proof cannot replay", err)
	}
}
