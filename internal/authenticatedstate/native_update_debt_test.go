package authenticatedstate

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestNativeFrameRecoveryDebtActualPrewriteRefusalAndCloseUncertainty(t *testing.T) {
	for _, mode := range []string{"prewrite-pressure", "actual-close"} {
		t.Run(mode, func(t *testing.T) {
			s, o := testStoreWithoutCleanup(t, 3)
			ctx := context.Background()
			before, err := s.RootIdentity(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "prewrite-pressure" {
				if err := o.Budget.SetPressure(resourcebudget.Pressured); err != nil {
					t.Fatal(err)
				}
				if _, err := s.UpdateNativeFrame(ctx, nextCut(71), func(*Transaction) error { t.Fatal("unadmitted callback entered"); return nil }, func() error { return nil }); !errors.Is(err, resourcebudget.ErrPressure) {
					t.Fatal(err)
				}
				if err := s.NativeFrameRecoveryDebt(); err != nil {
					t.Fatal("failed prewrite acquired recovery debt", err)
				}
				if err := o.Budget.SetPressure(resourcebudget.Healthy); err != nil {
					t.Fatal(err)
				}
				after, err := s.RootIdentity(ctx)
				if err != nil || after != before {
					t.Fatal(after, before, err)
				}
				if _, err := s.UpdateNativeFrame(ctx, nextCut(71), func(tx *Transaction) error { return tx.Put([]byte("retry"), []byte("known")) }, func() error { return nil }); err != nil {
					t.Fatal(err)
				}
				if err := s.Discard(); err != nil {
					t.Fatal(err)
				}
				return
			}
			s.closeRootFile = func(f *os.File) error {
				if err := f.Close(); err != nil {
					return err
				}
				return f.Close()
			}
			fees := o.Budget.Snapshot().Used
			err = o.Budget.WithRAMWorkspace(ctx, []resourcebudget.WorkspaceRequirement{{Class: resourcebudget.Background, RAM: UpdateRAMBytes}}, func(ctx context.Context) error {
				_, err := s.UpdateNativeFrame(ctx, nextCut(72), func(tx *Transaction) error { return tx.Put([]byte("actual-appended"), []byte("known")) }, func() error { return nil })
				return err
			})
			var workspaceDebt *resourcebudget.WorkspaceRetirement
			if !errors.Is(err, ownedfile.ErrRetirement) || !errors.As(err, &workspaceDebt) {
				t.Fatal("actual unknown IO escaped prepared workspace", err)
			}
			if err := s.NativeFrameRecoveryDebt(); !errors.Is(err, ErrRecovery) || !errors.Is(err, ownedfile.ErrRetirement) {
				t.Fatal(err)
			}
			held := o.Budget.Snapshot().Used
			if held.RAM <= fees.RAM || s.retiredUpdate == (resourcebudget.Permit{}) {
				t.Fatal("actual pending Close discarded update fee", fees, held)
			}
			for range 2 {
				if err := s.Close(); !errors.Is(err, ownedfile.ErrRetirement) {
					t.Fatal(err)
				}
				if o.Budget.Snapshot().Used != held {
					t.Fatal("repeat Close erased debt")
				}
				if err := s.NativeFrameRecoveryDebt(); !errors.Is(err, ownedfile.ErrRetirement) {
					t.Fatal(err)
				}
			}
		})
	}
}
