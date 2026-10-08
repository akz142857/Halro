//go:build linux

package authenticatedstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestPhysicalTerminalRootPoolActualOwnerLifecycle(t *testing.T) {
	s, o := testStoreWithoutCleanup(t, 3)
	ctx := context.Background()
	if err := s.EnablePhysicalTerminal(ctx); err != nil {
		t.Fatal(err)
	}
	w, err := s.ReservePhysicalTerminals(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	candidate := w.slots[0].owner.Identity()
	u, err := s.PreparePhysicalUpdate(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	_, commitErr := s.Update(u.Bind(ctx), nextCut(1), func(tx *Transaction) error { return tx.Put([]byte("key_000000"), []byte("terminal-published")) })
	if err := u.Finish(true, commitErr); err != nil {
		t.Fatal(err)
	}
	active, err := os.Stat(s.slotPath(s.packSlot, "active.root"))
	if err != nil || !os.SameFile(candidate, active) {
		t.Fatal("prepared inode was not publisher destination", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if s.physical.debt != 0 || s.physical.windows != 0 {
		t.Fatal("known children left debt", s.physical.debt, s.physical.windows)
	}
	fee, err := s.physical.fee.Cost()
	if err != nil || fee.RAM != 32<<10 || fee.Disk != (maxPhysicalTerminalSlots+2)*4096 {
		t.Fatal("whole pool missing fee", fee, err)
	}
	beforeClose := o.Budget.Snapshot().Used
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if !s.physical.feeRetired || o.Budget.Snapshot().Used.Disk != beforeClose.Disk-fee.Disk {
		t.Fatal("Close retained fee after all actual extra targets exited", o.Budget.Snapshot())
	}
	if err := s.Discard(); err != nil || o.Budget.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal("known namespace retirement leaked pool fee", err, o.Budget.Snapshot())
	}
}

func TestPhysicalHoldCloseJoinsDescriptorsPreservesUnselectedTargetsAndFee(t *testing.T) {
	requirePhysicalReceiptExt4(t)
	s, _ := testStoreWithoutCleanup(t, 3)
	ctx := context.Background()
	nativePath := filepath.Join(filepath.Dir(s.dir), "native")
	if err := os.WriteFile(nativePath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	native, verify := receiptNativeFixture(t, nativePath)
	if _, err := s.EnablePhysicalTerminalQualified(ctx, nextCut(99), native, verify); err != nil {
		t.Fatal(err)
	}
	w, err := s.ReservePhysicalTerminals(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	slot := w.slots[0]
	control := s.physical.receipt.files[0]
	beforeFee, err := s.physical.fee.Cost()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.HoldPhysicalPreparationForRecovery(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.Close(); !errors.Is(err, ErrRecovery) {
			t.Fatal("hold lost sticky Close debt", err)
		}
		fee, err := s.physical.fee.Cost()
		if err != nil || fee != beforeFee {
			t.Fatal("held physical fee refunded", fee, err)
		}
	}
	if _, err := slot.file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("held slot descriptor not joined", err)
	}
	if _, err := control.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("held control descriptor not joined", err)
	}
	if err := slot.owner.VerifyName(); err != nil {
		t.Fatal("held target inode retired", err)
	}
	if _, err := os.Stat(s.receiptPath(0)); err != nil {
		t.Fatal("held receipt retired", err)
	}
	if len(s.key) == 0 || s.key[0] == 0 {
		t.Fatal("held authentication owner discarded")
	}
}

func TestPhysicalTerminalPoolForeignReplacementRetainsWholeDebt(t *testing.T) {
	s, o := testStoreWithoutCleanup(t, 3)
	ctx := context.Background()
	if err := s.EnablePhysicalTerminal(ctx); err != nil {
		t.Fatal(err)
	}
	w, err := s.ReservePhysicalTerminals(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	path := s.slotPath(s.packSlot, "terminal-root-00")
	moved := filepath.Join(s.dir, "actual-original-spare")
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	foreign := []byte("foreign must remain")
	if err := os.WriteFile(path, foreign, 0600); err != nil {
		t.Fatal(err)
	}
	before := o.Budget.Snapshot().Used
	if _, err := s.PreparePhysicalUpdate(ctx, w); !errors.Is(err, ownedfile.ErrReplaced) {
		t.Fatal("foreign inode acquired physical content qualification", err)
	}
	for i := 0; i < 2; i++ {
		if err := w.Close(); !errors.Is(err, ownedfile.ErrReplaced) || o.Budget.Snapshot().Used != before || s.physical.debt != s.appendLimit {
			t.Fatal("unknown retirement lost debt/fee", err, o.Budget.Snapshot())
		}
	}
	if err := s.Close(); !errors.Is(err, ownedfile.ErrReplaced) {
		t.Fatal(err)
	}
	fee, err := s.physical.fee.Cost()
	if err != nil || fee.RAM != 32<<10 || fee.Disk != (maxPhysicalTerminalSlots+2)*4096 {
		t.Fatal("unknown Close refunded pool fee", fee, err)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != string(foreign) {
		t.Fatal("foreign inode changed", string(raw), err)
	}
	if _, err := os.Stat(moved); err != nil {
		t.Fatal("original unqualified allocation deleted", err)
	}
}

func TestPhysicalTerminalPublisherCloseDebtIsSticky(t *testing.T) {
	s, o := testStoreWithoutCleanup(t, 3)
	ctx := context.Background()
	if err := s.EnablePhysicalTerminal(ctx); err != nil {
		t.Fatal(err)
	}
	w, err := s.ReservePhysicalTerminals(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.PreparePhysicalUpdate(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	closeFailure := errors.New("injected uncertain actual root Close")
	s.closeRootFile = func(f *os.File) error { return errors.Join(f.Close(), closeFailure) }
	before := o.Budget.Snapshot().Used
	_, commitErr := s.Update(u.Bind(ctx), nextCut(2), func(tx *Transaction) error {
		return tx.Put([]byte("key_000000"), []byte("native fact already accepted"))
	})
	if !errors.Is(commitErr, closeFailure) || !errors.Is(commitErr, ownedfile.ErrRetirement) {
		t.Fatal("Close uncertainty not surfaced", commitErr)
	}
	if err := u.Finish(true, commitErr); !errors.Is(err, ErrRecovery) {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := w.Close(); !errors.Is(err, ErrRecovery) || s.physical.debt != s.appendLimit || o.Budget.Snapshot().Used.Disk != before.Disk {
			t.Fatal("unknown publisher exit lost debt", err, o.Budget.Snapshot())
		}
	}
	if _, err := s.PreparePhysicalUpdate(ctx, nil); !errors.Is(err, ErrRecovery) {
		t.Fatal("uncertain owner admitted next ordinary fact", err)
	}
	if err := s.Close(); !errors.Is(err, closeFailure) {
		t.Fatal(err)
	}
	fee, err := s.physical.fee.Cost()
	if err != nil || fee.RAM != 32<<10 {
		t.Fatal("unknown child returned its RAM", fee, err)
	}
	pending, err := os.Stat(s.slotPath(s.packSlot, "root.pending"))
	if err != nil || !os.SameFile(pending, u.slot.owner.Identity()) {
		t.Fatal("unknown original candidate deleted/replaced", err)
	}
	if _, err := os.Stat(s.slotPath(s.packSlot, "terminal-root-00")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rename retained a second candidate name", err)
	}
}
