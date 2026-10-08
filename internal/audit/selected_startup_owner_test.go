package audit

import (
	"context"
	"testing"
)

func TestSelectedStartupOwnerRequiresActualConstructionPublisherAndIdentity(t *testing.T) {
	f := newStartupCacheFixture(t, 3)
	check := func(identity [32]byte) error {
		return f.native.ValidateSelectedStartupOwner(context.Background(), f.options.CacheBudget, f.selected.Publisher, f.selected.Guard, f.selected.Authority, identity)
	}
	if err := check(f.selected.Identity); err == nil {
		t.Fatal("ordinary full native qualified as selected")
	}
	reopenSelectedForAudit(t, f)
	if err := check(f.selected.Identity); err != nil {
		t.Fatal(err)
	}
	if f.native.SelectedStartupReady() {
		t.Fatal("owner validation granted readiness")
	}
	if err := check([32]byte{8}); err == nil {
		t.Fatal("identity copied head qualified")
	}
	bad := f.selected.Authority
	bad.Positions[3].Sequence++
	if err := f.native.ValidateSelectedStartupOwner(context.Background(), f.options.CacheBudget, f.selected.Publisher, f.selected.Guard, bad, f.selected.Identity); err == nil {
		t.Fatal("unselected authority qualified")
	}
	if err := f.native.Close(); err != nil {
		t.Fatal(err)
	}
	if err := check(f.selected.Identity); err == nil {
		t.Fatal("closed actual owner qualified")
	}
}
