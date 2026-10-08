package governance

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestAuthenticatedGovernanceRestoreManagerContinuesRevisionsAndIdempotency(t *testing.T) {
	ctx := context.Background()
	old, native, _, _, _ := testManager(t)
	defer native.Close()
	input := reportInput("accepted", "", "before-restore")
	first, _, err := old.Report(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	accounting, definitions, checkpoint, now := old.accounting, old.definitions, old.checkpoint, old.now
	if err := old.Retire(ctx); err != nil {
		t.Fatal(err)
	}
	budget := governanceSealBudget(t)
	credit, err := budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer credit.Release()
	dir := t.TempDir()
	root, err := BuildAuthenticatedGovernanceProjection(ctx, AuthenticatedGovernanceOptions{Native: native, Identity: [32]byte{1}, RootKey: bytes.Repeat([]byte{0x72}, 32), ProjectionPath: filepath.Join(dir, "scratch.db"), RootDirectory: filepath.Join(dir, "root"), ProjectionMaxDiskBytes: 16 << 20, RootMaxDiskBytes: 32 << 20, RootMaxAppendBytes: 4 << 20, ProjectionDiskReservation: credit, Budget: budget})
	if err != nil {
		t.Fatal(err)
	}
	defer root.Discard()
	p, err := root.RestoreProjection(ctx, filepath.Join(dir, "live.db"), native, 16<<20, credit, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Discard()
	m, err := NewProjectedManager(ctx, native, p, accounting, definitions, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	m.now = now
	defer m.Retire(ctx)
	before := native.Summary()
	replay, repeated, err := m.Report(ctx, input)
	if err != nil || !repeated || replay.ID != first.ID || native.Summary() != before {
		t.Fatalf("restored idempotency appended facts: %+v %t %v", replay, repeated, err)
	}
	second, repeated, err := m.Report(ctx, reportInput("rejected", first.ID, "after-restore"))
	if err != nil || repeated || second.Revision != first.Revision+1 {
		t.Fatalf("restored Manager failed next revision: %+v %t %v", second, repeated, err)
	}
	current, found, err := p.ReadCurrent(ctx, first.ProjectID, first.WorkUnitID, first.DefinitionID, first.DefinitionVersion)
	if err != nil || !found || current.ID != second.ID {
		t.Fatal(current, found, err)
	}
	if head, err := p.ReadHead(ctx); err != nil || head != native.Summary() {
		t.Fatal(head, err)
	}
	// Disk Close withdraws reads and retires mapping RAM, while still holding
	// the image's persistent fee until its own inode has actually been removed.
	if err := m.Retire(ctx); err != nil {
		t.Fatal(err)
	}
	ram := budget.Snapshot().Used.RAM
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if budget.Snapshot().Used.RAM >= ram {
		t.Fatal("physical Close retained live mapping RAM")
	}
	if err := credit.Release(); err != nil {
		t.Fatal(err)
	}
	if budget.Snapshot().Used.Disk < 16<<20 {
		t.Fatal("caller release refunded a live image")
	}
	if err := p.Discard(); err != nil {
		t.Fatal(err)
	}
	if err := root.Discard(); err != nil {
		t.Fatal(err)
	}
	if budget.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal("restore left live fees", budget.Snapshot())
	}
}
