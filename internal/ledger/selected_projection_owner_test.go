package ledger

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

func TestSelectedLedgerProjectionOpensActualSelectedRootAndOnlyDelta(t *testing.T) {
	f := newSelectedStartupFixture(t, true)
	if _, err := f.native.Append(context.Background(), projectionTestLifecycle(2, true)[0]); err != nil {
		t.Fatal(err)
	}
	f.reopen(t)
	lease, err := f.native.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	head, hash, _ := f.native.ChainHead()
	prefix := ProjectionPrefix{Head: head, Hash: hash}
	o := f.ledgerOptions
	o.Prefix = &prefix
	p, receipt, err := OpenSelectedAuthenticatedLedgerProjection(context.Background(), f.native, f.ledgerDirectory, o, SelectedLedgerProjectionOpenOptions{Publisher: f.o.Publisher, Guard: f.o.Guard, Authority: f.o.Authority})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.SourceVerified != 1 || receipt.AppliedRecords != 1 || receipt.DeltaVerified != 1 || receipt.To.Head != head || receipt.To.Hash != hash {
		t.Fatal(receipt)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.ledgerOwner.AdoptClosedSuccessor(p); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedLedgerProjectionNoDeltaDoesNotReadOldNativeBytes(t *testing.T) {
	f := newSelectedStartupFixture(t, false)
	f.reopen(t)
	// Deliberate old native damage is still caught by the separate historical
	// auditor. This selected root opener must not silently turn into genesis IO.
	file, err := os.OpenFile(f.path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	var byteAt [1]byte
	if _, err := file.ReadAt(byteAt[:], frameHeaderSize+chainPreviousHashSize); err != nil {
		t.Fatal(err)
	}
	byteAt[0] ^= 1
	if _, err := file.WriteAt(byteAt[:], frameHeaderSize+chainPreviousHashSize); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	lease, err := f.native.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	o := f.ledgerOptions
	prefix := ProjectionPrefix{Head: o.SavedAnchor.Head, Hash: o.SavedAnchor.Hash}
	o.Prefix = &prefix
	p, r, err := OpenSelectedAuthenticatedLedgerProjection(context.Background(), f.native, f.ledgerDirectory, o, SelectedLedgerProjectionOpenOptions{Publisher: f.o.Publisher, Guard: f.o.Guard, Authority: f.o.Authority})
	if err != nil {
		t.Fatal(err)
	}
	if r.SourceVerified != 0 || r.AppliedRecords != 0 {
		t.Fatal(r)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if f.native.SelectedStartupReady() {
		t.Fatal("root loan bypassed historical readiness")
	}
	if err := f.native.AuditSelectedStartup(context.Background(), f.o.DirectoryOptions.Budget); err == nil {
		t.Fatal("actual historical damage hidden")
	}
}

func TestSelectedLedgerProjectionRejectsForgedCallerRootOrOrdinaryNative(t *testing.T) {
	f := newSelectedStartupFixture(t, false)
	lease, err := f.native.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	o := f.ledgerOptions
	prefix := ProjectionPrefix{Head: o.SavedAnchor.Head, Hash: o.SavedAnchor.Hash}
	o.Prefix = &prefix
	fake := authenticatedstate.SealRoot{Kind: authenticatedstate.SealRootState, Component: authenticatedstate.SealComponentLedger}
	o.SelectedSealRoot = &fake
	selected := SelectedLedgerProjectionOpenOptions{Publisher: f.o.Publisher, Guard: f.o.Guard, Authority: f.o.Authority}
	if p, _, err := OpenSelectedAuthenticatedLedgerProjection(context.Background(), f.native, f.ledgerDirectory, o, selected); p != nil || !errors.Is(err, ErrProjectionSource) {
		t.Fatal(p, err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	f.reopen(t)
	lease, err = f.native.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	selected.Authority.Positions[0].Sequence++
	if p, _, err := OpenSelectedAuthenticatedLedgerProjection(context.Background(), f.native, f.ledgerDirectory, o, selected); p != nil || err == nil {
		t.Fatal(p, err)
	}
}
