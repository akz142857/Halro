//go:build linux

package authenticatedstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestPhysicalPreparedObservationOriginalBankAndForeignTargets(t *testing.T) {
	requirePhysicalReceiptExt4(t)
	s, options := testStoreWithoutCleanup(t, 3)
	ctx := context.Background()
	nativePath := filepath.Join(filepath.Dir(s.dir), "observation-native")
	if err := os.WriteFile(nativePath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	native, verify := receiptNativeFixture(t, nativePath)
	if _, err := s.EnablePhysicalTerminalQualified(ctx, nextCut(99), native, verify); err != nil {
		t.Fatal(err)
	}
	w, err := s.ReservePhysicalTerminals(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	fees := options.Budget.Snapshot().Used
	first, err := w.PreparedObservation(ctx)
	if err != nil || first.UnusedRoots != 2 || first.Native != native || first.Selection != nextCut(99) {
		t.Fatal(first, err)
	}
	if again, err := w.PreparedObservation(ctx); err != nil || again != first || options.Budget.Snapshot().Used != fees {
		t.Fatal("read changed original owner", again, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := w.PreparedObservation(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	alias := *w
	if _, err := alias.PreparedObservation(ctx); !errors.Is(err, ErrRecovery) {
		t.Fatal("copied window acquired original bank", err)
	}

	// Byte-identical replacement is still a different owner and physical birth.
	path := w.slots[0].file.Name()
	saved := path + ".original-observation"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.PreparedObservation(ctx); err == nil {
		t.Fatal("foreign identical future root acquired credit")
	}
	if options.Budget.Snapshot().Used != fees {
		t.Fatal("foreign root refunded a fee")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(saved, path); err != nil {
		t.Fatal(err)
	}

	// The inactive control is also authenticated; corrupt bytes cannot supply
	// credit even when the active bank and every public counter still match.
	inactive := s.physical.receipt.files[1-s.physical.receipt.bank]
	control := make([]byte, physicalReceiptBytes)
	if _, err := inactive.ReadAt(control, 0); err != nil {
		t.Fatal(err)
	}
	bad := append([]byte(nil), control...)
	bad[len(bad)-1] ^= 1
	if _, err := inactive.WriteAt(bad, 0); err != nil {
		t.Fatal(err)
	}
	if err := inactive.Sync(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.PreparedObservation(ctx); err == nil {
		t.Fatal("tampered inactive control acquired credit")
	}
	if _, err := inactive.WriteAt(control, 0); err != nil {
		t.Fatal(err)
	}
	if err := inactive.Sync(); err != nil {
		t.Fatal(err)
	}

	// Removing real future blocks without changing length/control bytes must
	// revoke observation. Restore only this fixture's independently owned tail.
	// Truncation back to the original EOF removes KEEP_SIZE extents beyond EOF;
	// Linux PUNCH_HOLE past EOF would succeed without removing those extents.
	if err := s.pack.Truncate(first.COWEnd); err != nil {
		t.Fatal(err)
	}
	if err := s.pack.Truncate(int64(s.physicalExtent)); err != nil {
		t.Fatal(err)
	}
	if err := s.pack.Sync(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.PreparedObservation(ctx); err == nil {
		t.Fatal("unallocated future pack range acquired credit")
	}
	if err := resourcebudget.AllocatePhysicalRange(s.pack, int64(s.physicalExtent), first.COWEnd-int64(s.physicalExtent)); err != nil {
		t.Fatal(err)
	}
	if restored, err := w.PreparedObservation(ctx); err != nil || restored != first {
		t.Fatal("restored original bank differs", restored, err)
	}

	u, err := s.PreparePhysicalUpdate(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	_, updateErr := s.Update(u.Bind(ctx), nextCut(1), func(tx *Transaction) error { return tx.Put([]byte("key_000000"), []byte("actual accepted update")) })
	if err := u.Finish(true, updateErr); err != nil {
		t.Fatal(err)
	}
	after, err := w.PreparedObservation(ctx)
	if err != nil || after.UnusedRoots != 1 || after.Selection != first.Selection || after.Controls != first.Controls || after.NativeEnd != first.NativeEnd || after.COWEnd != first.COWEnd || after.Pack != first.Pack {
		t.Fatal("consumption changed bank identity or lost debt", after, err)
	}
	if options.Budget.Snapshot().Used != fees {
		t.Fatal("observation changed actual fees")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.PreparedObservation(ctx); !errors.Is(err, ErrRecovery) {
		t.Fatal("retired bank returned credit", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Discard(); err != nil {
		t.Fatal(err)
	}
}
