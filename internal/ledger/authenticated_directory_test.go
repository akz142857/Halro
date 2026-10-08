package ledger

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

func TestAuthenticatedDirectoryCapturesCompleteOrdinalScopeAndReopensOldSelectedCut(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.wal")
	native := openChained(t, path, nil)
	events := append(projectionTestAttribution(), projectionTestLifecycle(1, true)...)
	for i, e := range events {
		if _, err := native.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
		if i == 2 || i == 5 {
			if _, err := native.Roll(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := native.Compact(1); err != nil {
		t.Fatal(err)
	}
	report, partial, err := InspectReplayAuthenticated(path, testChainKey, nil)
	if err != nil || partial {
		t.Fatal(partial, err)
	}
	opts := authenticatedLedgerTestOptions(t)
	owner, err := BuildAuthenticatedLedgerProjection(ctx, path, testChainKey, filepath.Join(t.TempDir(), "ledger-state"), report, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Discard()
	lease, err := native.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	var want []Segment
	if err := native.WalkSegments(ctx, func(s Segment) error { want = append(want, s); return nil }); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), "directory")
	o := AuthenticatedDirectoryOptions{Identity: opts.Identity, Budget: opts.Budget, MaxDiskBytes: 16 << 20, MaxAppendBytes: 4 << 20}
	candidate, err := BuildAuthenticatedDirectory(ctx, native, lease, owner, directory, o)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Discard()
	selected, err := candidate.SealReference(ctx)
	if err != nil || selected.Schema.Kind != authenticatedstate.SealRootState || selected.Schema.Component != authenticatedstate.SealComponentLogDirectory || selected.Root.Rows != uint64(len(want)+1) {
		t.Fatal(selected, err)
	}
	for _, w := range want {
		got, found, err := candidate.ReadSegment(ctx, w.Generation)
		if err != nil || !found || !reflect.DeepEqual(got, w) {
			t.Fatal(got, w, found, err)
		}
	}
	for _, g := range []uint64{0, report.Head.Generation, report.Head.Generation + 1} {
		if _, found, err := candidate.ReadSegment(ctx, g); err != nil || found {
			t.Fatal("non-sealed generation invented", g, found, err)
		}
	}
	if err := candidate.Close(); err != nil {
		t.Fatal(err)
	}
	credit, err := candidate.DiskReservation()
	if err != nil {
		t.Fatal(err)
	}
	defer credit.Release()
	o.DiskReservation = credit
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := native.Roll(); err != nil {
		t.Fatal(err)
	}
	if _, err := native.Append(ctx, projectionTestLifecycle(2, true)[0]); err != nil {
		t.Fatal(err)
	}
	lease, err = native.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	reopened, err := OpenAuthenticatedDirectory(ctx, native, lease, selected, directory, o)
	if err != nil {
		t.Fatal(err)
	}
	again, err := reopened.SealReference(ctx)
	if err != nil || again != selected {
		t.Fatal(again, err)
	}
	if _, found, err := reopened.ReadSegment(ctx, report.Head.Generation); err != nil || found {
		t.Fatal("newly sealed tail leaked into saved scope", found, err)
	}
	for _, w := range want {
		got, found, err := reopened.ReadSegment(ctx, w.Generation)
		if err != nil || !found || !reflect.DeepEqual(got, w) {
			t.Fatal(got, w, found, err)
		}
	}
	if _, err := reopened.AdvanceFromNative(ctx, native, lease, owner); !errors.Is(err, ErrProjectionSource) {
		t.Fatal("old accounting owner authorized a newer directory", err)
	}
	head, hash, _ := native.ChainHead()
	if _, err := owner.AdvanceFromNative(ctx, native, ProjectionPrefix{Head: head, Hash: hash}); err != nil {
		t.Fatal(err)
	}
	if changed, err := reopened.AdvanceFromNative(ctx, native, lease, owner); err != nil || changed != 1 {
		t.Fatal("new sealed generation was not installed atomically", changed, err)
	}
	current, err := reopened.SealReference(ctx)
	if err != nil || current.Position.Generation != head.Generation || current.Root.Rows != head.Generation {
		t.Fatal("advanced directory", current, err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := native.Compact(2); err != nil {
		t.Fatal(err)
	}
	lease, err = native.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if changed, err := reopened.AdvanceFromNative(ctx, native, lease, owner); err != nil || changed != 1 {
		t.Fatal("compacted descriptor was not refreshed at the same cut", changed, err)
	}
	if err := native.WalkSegments(ctx, func(w Segment) error {
		got, found, err := reopened.ReadSegment(ctx, w.Generation)
		if err != nil || !found || !reflect.DeepEqual(got, w) {
			return errors.Join(ErrProjectionCorrupt, err)
		}
		return nil
	}); err != nil {
		t.Fatal("refreshed directory differs from actual native physical manifest", err)
	}
	refreshed, err := reopened.SealReference(ctx)
	if err != nil || refreshed.Position != current.Position || refreshed.Root == current.Root {
		t.Fatal("physical refresh changed native cut or failed to replace root", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if err := candidate.AdoptClosedSuccessor(reopened); err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticatedDirectoryRejectsWrongSelectedRootBeforeOrphanCleanup(t *testing.T) {
	f := newAuthenticatedReopenFixture(t)
	f.maintain(t)
	ctx := context.Background()
	owner, _, err := OpenAuthenticatedLedgerProjection(ctx, f.native, f.directory, f.options)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	o := AuthenticatedDirectoryOptions{Identity: f.options.Identity, Budget: f.options.Budget, MaxDiskBytes: 16 << 20, MaxAppendBytes: 4 << 20}
	directory := filepath.Join(t.TempDir(), "directory")
	candidate, err := BuildAuthenticatedDirectory(ctx, f.native, f.lease, owner, directory, o)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Discard()
	ref, err := candidate.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := candidate.Close(); err != nil {
		t.Fatal(err)
	}
	credit, err := candidate.DiskReservation()
	if err != nil {
		t.Fatal(err)
	}
	defer credit.Release()
	o.DiskReservation = credit
	path := filepath.Join(directory, "pages")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	orphan := append(bytes.Clone(original), []byte("orphan")...)
	if err := os.WriteFile(path, orphan, 0600); err != nil {
		t.Fatal(err)
	}
	ref.Root.State.Digest[0] ^= 1
	baseline := o.Budget.Snapshot().Used
	if reopened, err := OpenAuthenticatedDirectory(ctx, f.native, f.lease, ref, directory, o); reopened != nil || !errors.Is(err, authenticatedstate.ErrRecovery) {
		t.Fatal(reopened, err)
	}
	now, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(now, orphan) || o.Budget.Snapshot().Used != baseline {
		t.Fatal("refused root mutated bytes or fees", err)
	}
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
}
