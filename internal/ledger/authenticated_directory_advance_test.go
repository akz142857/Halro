package ledger

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestAuthenticatedDirectoryPartialFinitePageCannotPublishAndReopensSavedRoot(t *testing.T) {
	ctx := context.Background()
	owner, native, _ := authenticatedAdvanceFixture(t, 0)
	lease, err := native.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	opts := AuthenticatedDirectoryOptions{Identity: authenticatedLedgerTestOptions(t).Identity, Budget: owner.budget, MaxDiskBytes: 16 << 20, MaxAppendBytes: 4 << 20}
	path := filepath.Join(t.TempDir(), "directory")
	d, err := BuildAuthenticatedDirectory(ctx, native, lease, owner, path, opts)
	if err != nil {
		t.Fatal(err)
	}
	// This fixture owns an unpublished private root. The original creation
	// receipt can retire it after the independently selected successor joins.
	defer func() {
		if err := d.store.Discard(); err != nil {
			t.Error(err)
		}
	}()
	saved, err := d.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 70; i++ {
		if _, err := native.Append(ctx, projectionTestLifecycle(i+2, true)[0]); err != nil {
			t.Fatal(err)
		}
		if _, err := native.Roll(); err != nil {
			t.Fatal(err)
		}
	}
	lease, err = native.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	head, hash, _ := native.ChainHead()
	if _, err := owner.AdvanceFromNative(ctx, native, ProjectionPrefix{Head: head, Hash: hash}); err != nil {
		t.Fatal(err)
	}
	called := 0
	changed, err := d.advanceFromNative(ctx, native, lease, owner, func(string) error { called++; return context.Canceled })
	if called != 1 || changed != 0 || !errors.Is(err, context.Canceled) || !errors.Is(err, ErrDirectoryPartialAdvance) {
		t.Fatal("actual first finite page did not withdraw publication", called, changed, err)
	}
	if _, err := d.SealReference(ctx); !errors.Is(err, ErrDirectoryPartialAdvance) {
		t.Fatal("partial rows were eligible for common publication", err)
	}
	if _, _, err := d.ReadSegment(ctx, 1); !errors.Is(err, ErrDirectoryPartialAdvance) {
		t.Fatal("partial owner kept read admission", err)
	}
	if err := d.Close(); !errors.Is(err, ErrDirectoryPartialAdvance) {
		t.Fatal("partial fault was erased by Close", err)
	}
	credit, err := d.DiskReservation()
	if err != nil {
		t.Fatal(err)
	}
	defer credit.Release()
	opts.DiskReservation = credit
	opened, err := OpenAuthenticatedDirectory(ctx, native, lease, saved, path, opts)
	if err != nil {
		t.Fatal("saved independently qualified root cannot reopen", err)
	}
	defer func() {
		if err := opened.Close(); err != nil {
			t.Error(err)
		}
		if err := d.AdoptClosedSuccessor(opened); err != nil {
			t.Error(err)
		}
	}()
	if ref, err := opened.SealReference(ctx); err != nil || ref != saved {
		t.Fatal("unpublished page became authority", ref, err)
	}
	if changed, err := opened.AdvanceFromNative(ctx, native, lease, owner); err != nil || changed != 70 {
		t.Fatal("fresh bounded maintenance could not complete", changed, err)
	}
	if ref, err := opened.SealReference(ctx); err != nil || ref.Root.Rows != 71 || ref.Position.Generation != 71 {
		t.Fatal("complete directory differs", ref, err)
	}
}
