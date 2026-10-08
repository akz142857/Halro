package governance

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func typedGovernanceFixture(t *testing.T, native *Log) (*GovernanceProjection, TypedGovernanceReference, AuthenticatedGovernanceOptions) {
	t.Helper()
	budget := governanceSealBudget(t)
	credit, err := budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { credit.Release() })
	dir := t.TempDir()
	o := AuthenticatedGovernanceOptions{Native: native, Identity: [32]byte{1}, RootKey: bytes.Repeat([]byte{0x72}, 32), ProjectionPath: filepath.Join(dir, "scratch.db"), RootDirectory: filepath.Join(dir, "root"), ProjectionMaxDiskBytes: 16 << 20, RootMaxDiskBytes: 64 << 20, RootMaxAppendBytes: 8 << 20, ProjectionDiskReservation: credit, Budget: budget}
	p, ref, err := BuildTypedGovernanceProjection(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if p.db != nil || p.image != nil || p.cow == nil || ref.Projection.Schema.Codec != TypedGovernanceSealCodec {
		t.Fatal("typed owner retained ordinary image", ref)
	}
	if _, err := os.Stat(o.ProjectionPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("migration scratch survived publication", err)
	}
	return p, ref, o
}

func TestTypedGovernanceManagerReadsAndWritesWithoutMaterializedEngine(t *testing.T) {
	ctx := context.Background()
	old, native, _, _, _ := testManager(t)
	defer native.Close()
	in := reportInput("accepted", "", "first")
	first, _, err := old.Report(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	accounting, definitions, checkpoint, now := old.accounting, old.definitions, old.checkpoint, old.now
	if err := old.Retire(ctx); err != nil {
		t.Fatal(err)
	}
	p, _, _ := typedGovernanceFixture(t, native)
	defer p.Discard()
	m, err := NewProjectedManager(ctx, native, p, accounting, definitions, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	m.now = now
	defer m.Retire(ctx)
	if m.State() != nil {
		t.Fatal("typed Manager retained legacy State")
	}
	before := native.Summary()
	replayed, repeat, err := m.Report(ctx, in)
	if err != nil || !repeat || replayed.ID != first.ID || native.Summary() != before {
		t.Fatal(replayed, repeat, err)
	}
	second, repeat, err := m.Report(ctx, reportInput("rejected", first.ID, "second"))
	if err != nil || repeat || second.Revision != first.Revision+1 {
		t.Fatal(second, repeat, err)
	}
	current, found, err := p.ReadCurrent(ctx, first.ProjectID, first.WorkUnitID, first.DefinitionID, first.DefinitionVersion)
	if err != nil || !found || current.ID != second.ID {
		t.Fatal(current, found, err)
	}
	state := NewState()
	if _, err := native.Replay(state.Apply); err != nil {
		t.Fatal(err)
	}
	for _, row := range state.Snapshot().Records {
		got, found, err := p.ReadOutcome(ctx, row.Outcome.ID)
		if err != nil || !found || !reflect.DeepEqual(got, row.Outcome) {
			t.Fatal(got, row, found, err)
		}
	}
	ref, err := p.TypedSealReference(ctx)
	if err != nil || ref.Projection.Position.Sequence != native.Summary().Records || ref.Projection.Root.Rows <= 2 {
		t.Fatal(ref, err)
	}
}

func TestTypedGovernancePagesPreserveFilteredProgressAndFrozenCut(t *testing.T) {
	ctx := context.Background()
	native, err := Open(filepath.Join(t.TempDir(), "native.wal"), bytes.Repeat([]byte{0x43}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	for i := range 250 {
		e := projectionEvent(i)
		if i < 230 {
			e.ProjectID = "prj_other"
		}
		if _, err := native.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	p, _, _ := typedGovernanceFixture(t, native)
	defer p.Discard()
	cut, err := p.ReadHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	q := OutcomePageQuery{ProjectID: "prj_1", DefinitionID: "odef_result", Rows: 7, Bytes: 1 << 20}
	first, after, err := p.readOutcomePage(ctx, q, "", cut)
	if err != nil || len(first.Items) != 0 || after != "out_00000199" {
		t.Fatal(first, after, err)
	}
	late, err := native.Append(ctx, projectionEvent(999))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(ctx, late); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for n := 0; n < 10 && after != ""; n++ {
		page, next, err := p.readOutcomePage(ctx, q, after, cut)
		if err != nil || page.Head != cut || len(page.Items) > q.Rows || page.Bytes > q.Bytes || next == after {
			t.Fatal(page, next, err)
		}
		for _, item := range page.Items {
			if seen[item.ID] || item.GovernanceSequence > cut.Records {
				t.Fatal("duplicate or future fact", item)
			}
			seen[item.ID] = true
		}
		after = next
	}
	if len(seen) != 20 || after != "" || seen[late.Event.OutcomeID] {
		t.Fatal("incomplete frozen page traversal", len(seen), after)
	}
}

func TestTypedGovernanceReopenUsesSelectedRootAndPermittedDelta(t *testing.T) {
	ctx := context.Background()
	native, err := Open(filepath.Join(t.TempDir(), "native.wal"), bytes.Repeat([]byte{0x43}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	for i := range 3 {
		if _, err := native.Append(ctx, projectionEvent(i)); err != nil {
			t.Fatal(err)
		}
	}
	creator, ref, o := typedGovernanceFixture(t, native)
	if err := creator.Close(); err != nil {
		t.Fatal(err)
	}
	credit, err := creator.TypedDiskReservation()
	if err != nil {
		t.Fatal(err)
	}
	defer credit.Release()
	var permitted Summary
	for i := 3; i < 12; i++ {
		r, err := native.Append(ctx, projectionEvent(i))
		if err != nil {
			t.Fatal(err)
		}
		if i == 6 {
			permitted = advanceRecordCut(r)
		}
	}
	borrowed, err := OpenTypedGovernanceProjection(ctx, o, ref, credit)
	if err != nil {
		t.Fatal(err)
	}
	if borrowed.db != nil || borrowed.image != nil {
		t.Fatal("reopen materialized a DB")
	}
	receipt, err := borrowed.AdvanceAuthenticatedFromNative(ctx, native, permitted, o.Budget)
	if err != nil || receipt.AppliedRecords != 4 || receipt.From.Records != 3 || receipt.To != permitted || receipt.NativeHead.Records != 12 {
		t.Fatal(receipt, err)
	}
	if _, found, err := borrowed.ReadOutcome(ctx, projectionEvent(6).OutcomeID); err != nil || !found {
		t.Fatal(found, err)
	}
	if _, found, err := borrowed.ReadOutcome(ctx, projectionEvent(7).OutcomeID); err != nil || found {
		t.Fatal("unpermitted suffix exposed", found, err)
	}
	if err := borrowed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := creator.AdoptClosedTypedSuccessor(borrowed); err != nil {
		t.Fatal(err)
	}
	if err := creator.Discard(); err != nil {
		t.Fatal(err)
	}
}

func TestTypedGovernanceApplyRefusesForgedNativeRecordWithoutRootMutation(t *testing.T) {
	ctx := context.Background()
	native, err := Open(filepath.Join(t.TempDir(), "native.wal"), bytes.Repeat([]byte{0x43}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	p, _, _ := typedGovernanceFixture(t, native)
	defer p.Discard()
	before, err := p.cow.RootIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r, err := native.Append(ctx, projectionEvent(1))
	if err != nil {
		t.Fatal(err)
	}
	forged := r
	forged.Event.Value = "rejected"
	if err := p.Apply(ctx, forged); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
	after, err := p.cow.RootIdentity(ctx)
	if err != nil || after != before {
		t.Fatal("unproved facts mutated root", err)
	}
	if err := p.Apply(ctx, r); err != nil {
		t.Fatal(err)
	}
	if got, found, err := p.ReadOutcome(ctx, r.Event.OutcomeID); err != nil || !found || got.Value != "accepted" {
		t.Fatal(got, found, err)
	}
}

func TestTypedGovernanceNativeReaderCloseDebtKeepsPackAndWorkspace(t *testing.T) {
	ctx := context.Background()
	native, err := Open(filepath.Join(t.TempDir(), "native.wal"), bytes.Repeat([]byte{0x43}, 32))
	if err != nil {
		t.Fatal(err)
	}
	p, _, o := typedGovernanceFixture(t, native)
	r, err := native.Append(ctx, projectionEvent(1))
	if err != nil {
		t.Fatal(err)
	}
	native.closeReaderFile = func(f *os.File) error {
		if err := f.Close(); err != nil {
			return err
		}
		return f.Close()
	}
	receipt, err := p.AdvanceAuthenticatedFromNative(ctx, native, advanceRecordCut(r), o.Budget)
	if !errors.Is(err, ownedfile.ErrRetirement) || receipt != (AuthenticatedProjectionAdvance{}) {
		t.Fatal(receipt, err)
	}
	if _, _, err := p.ReadOutcome(ctx, r.Event.OutcomeID); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	first := p.Discard()
	if !errors.Is(first, ownedfile.ErrRetirement) || p.Discard() != first {
		t.Fatal("repeat cleanup erased debt", first)
	}
	fees := o.Budget.Snapshot().Used
	if fees.RAM < 2*governanceSealWorkspaceRAMBytes || fees.Disk < o.RootMaxDiskBytes {
		t.Fatal("unknown actual source refunded workspace or pack", fees)
	}
	if _, err := os.Stat(filepath.Join(o.RootDirectory, "pages")); err != nil {
		t.Fatal(err)
	}
	if err := native.Close(); !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal(err)
	}
}

// Compile assertions keep the concrete projected Manager contract intact.
var _ interface {
	ReadOutcome(context.Context, string) (domain.Outcome, bool, error)
} = (*GovernanceProjection)(nil)

func TestTypedGovernanceCloseCancelsAndJoinsWholeNativeJob(t *testing.T) {
	ctx := context.Background()
	native, err := Open(filepath.Join(t.TempDir(), "native.wal"), bytes.Repeat([]byte{0x43}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	p, _, o := typedGovernanceFixture(t, native)
	defer p.Discard()
	r, err := native.Append(ctx, projectionEvent(1))
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	advanced := make(chan error, 1)
	go func() {
		_, err := p.advanceAuthenticatedFromNative(ctx, native, advanceRecordCut(r), o.Budget, func(phase string) {
			if phase == "source-verified" {
				close(entered)
				<-release
			}
		})
		advanced <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("native job never entered")
	}
	closed := make(chan error, 1)
	go func() { closed <- p.Close() }()
	select {
	case err := <-closed:
		close(release)
		t.Fatal("Close did not join native job", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-advanced; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if _, err := p.ReadHead(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if native.terminalErr != nil {
		t.Fatal("cancellation poisoned native source", native.terminalErr)
	}
}

func TestTypedGovernanceSelectedSchemaAndCutRefusedBeforeRootRepair(t *testing.T) {
	ctx := context.Background()
	native, err := Open(filepath.Join(t.TempDir(), "native.wal"), bytes.Repeat([]byte{0x43}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	creator, selected, o := typedGovernanceFixture(t, native)
	defer creator.Discard()
	if err := creator.Close(); err != nil {
		t.Fatal(err)
	}
	credit, err := creator.TypedDiskReservation()
	if err != nil {
		t.Fatal(err)
	}
	defer credit.Release()
	pack := filepath.Join(o.RootDirectory, "pages")
	f, err := os.OpenFile(pack, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("uncommitted tail retained on rejection")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(pack)
	if err != nil {
		t.Fatal(err)
	}
	fees := o.Budget.Snapshot().Used
	for _, mutate := range []func(*TypedGovernanceReference){
		func(r *TypedGovernanceReference) { r.Projection.Schema.Codec = 1 },
		func(r *TypedGovernanceReference) { r.Projection.Scope[0] ^= 1 },
		func(r *TypedGovernanceReference) { r.Projection.Position.Hash[0] ^= 1 },
		func(r *TypedGovernanceReference) { r.Projection.Root.Digest[0] ^= 1 },
	} {
		ref := selected
		mutate(&ref)
		opened, err := OpenTypedGovernanceProjection(ctx, o, ref, credit)
		if err == nil || opened != nil {
			t.Fatal("ineligible selected root opened", opened, err)
		}
		after, err := os.ReadFile(pack)
		if err != nil || !bytes.Equal(before, after) || o.Budget.Snapshot().Used != fees {
			t.Fatal("rejection repaired bytes or changed fees", err)
		}
	}
}

func TestTypedGovernanceHistoricalSelectedRootKeepsFuturePhysicalPages(t *testing.T) {
	ctx := context.Background()
	native, err := Open(filepath.Join(t.TempDir(), "native.wal"), bytes.Repeat([]byte{0x43}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	first, err := native.Append(ctx, projectionEvent(1))
	if err != nil {
		t.Fatal(err)
	}
	creator, selected, o := typedGovernanceFixture(t, native)
	defer creator.Discard()
	late, err := native.Append(ctx, projectionEvent(2))
	if err != nil {
		t.Fatal(err)
	}
	if err := creator.Apply(ctx, late); err != nil {
		t.Fatal(err)
	}
	pack := filepath.Join(o.RootDirectory, "pages")
	before, err := os.Stat(pack)
	if err != nil {
		t.Fatal(err)
	}
	if err := creator.Close(); err != nil {
		t.Fatal(err)
	}
	credit, err := creator.TypedDiskReservation()
	if err != nil {
		t.Fatal(err)
	}
	defer credit.Release()
	borrowed, err := OpenTypedGovernanceProjection(ctx, o, selected, credit)
	if err != nil {
		t.Fatal(err)
	}
	defer borrowed.Close()
	if head, err := borrowed.ReadHead(ctx); err != nil || head != advanceRecordCut(first) {
		t.Fatal(head, err)
	}
	if _, found, err := borrowed.ReadOutcome(ctx, late.Event.OutcomeID); err != nil || found {
		t.Fatal(found, err)
	}
	after, err := os.Stat(pack)
	if err != nil || after.Size() != before.Size() {
		t.Fatal("opening old root discarded future pages", err)
	}
	receipt, err := borrowed.AdvanceAuthenticatedFromNative(ctx, native, advanceRecordCut(late), o.Budget)
	if err != nil || receipt.AppliedRecords != 1 {
		t.Fatal(receipt, err)
	}
	branched, err := os.Stat(pack)
	if err != nil || branched.Size() <= before.Size() {
		t.Fatal("new branch overwrote physical future extent", err)
	}
	if err := borrowed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := creator.AdoptClosedTypedSuccessor(borrowed); err != nil {
		t.Fatal(err)
	}
}

func TestTypedGovernanceChangedNativeNextFrameWithdrawsBothOwners(t *testing.T) {
	ctx := context.Background()
	native, err := Open(filepath.Join(t.TempDir(), "native.wal"), bytes.Repeat([]byte{0x43}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	p, _, _ := typedGovernanceFixture(t, native)
	defer p.Discard()
	row, err := native.Append(ctx, projectionEvent(1))
	if err != nil {
		t.Fatal(err)
	}
	before, err := p.cow.RootIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(native.path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	var byteAt [1]byte
	if _, err := f.ReadAt(byteAt[:], frameHeaderSize+10); err != nil {
		t.Fatal(err)
	}
	byteAt[0] ^= 1
	if _, err := f.WriteAt(byteAt[:], frameHeaderSize+10); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(ctx, row); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
	after, err := p.cow.RootIdentity(ctx)
	if err != nil || before != after {
		t.Fatal("corrupt source advanced authenticated rows", err)
	}
	if _, err := p.ReadHead(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if _, err := native.Append(ctx, projectionEvent(2)); err == nil {
		t.Fatal("corrupt native accepted another fact")
	}
}

func TestTypedGovernanceDirectBuildUsesFixedRAMAndPreservesForeignScratch(t *testing.T) {
	ctx := context.Background()
	native, err := Open(filepath.Join(t.TempDir(), "native.wal"), bytes.Repeat([]byte{0x43}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	for i := range 80 {
		if _, err := native.Append(ctx, projectionEvent(i)); err != nil {
			t.Fatal(err)
		}
	}
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 192 << 20, Disk: 128 << 20}, MaxClaims: 16, MaxOwners: 4, MaxPressureAge: time.Hour}
	for i := range limits.Classes {
		limits.Classes[i] = limits.Total
	}
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 16 * resourcebudget.ClaimRAMBytes}
	limits.Classes[resourcebudget.Recovery] = resourcebudget.Cost{}
	budget, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := budget.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	scratch := filepath.Join(dir, "foreign.db")
	bytesBefore := []byte("foreign materialized bytes stay untouched")
	if err := os.WriteFile(scratch, bytesBefore, 0600); err != nil {
		t.Fatal(err)
	}
	o := AuthenticatedGovernanceOptions{Native: native, Identity: [32]byte{1}, RootKey: bytes.Repeat([]byte{0x72}, 32), Budget: budget, RootDirectory: filepath.Join(dir, "root"), RootMaxDiskBytes: 64 << 20, RootMaxAppendBytes: 8 << 20, ProjectionPath: scratch, ProjectionMaxDiskBytes: 1 << 30}
	p, ref, err := BuildTypedGovernanceProjection(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if p.db != nil || p.image != nil || p.retiredScratch != nil || ref.Projection.Position.Sequence != 80 {
		t.Fatal("direct build retained scratch", ref)
	}
	after, err := os.ReadFile(scratch)
	if err != nil || !bytes.Equal(bytesBefore, after) {
		t.Fatal("direct constructor touched foreign scratch", err)
	}
	if err := p.Discard(); err != nil {
		t.Fatal(err)
	}
	if budget.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal("known direct owner did not retire", budget.Snapshot())
	}
}

func TestTypedGovernanceDirectBuildActualSourceCloseDebtKeepsCreatedRoot(t *testing.T) {
	ctx := context.Background()
	native, err := Open(filepath.Join(t.TempDir(), "native.wal"), bytes.Repeat([]byte{0x43}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := native.Append(ctx, projectionEvent(1)); err != nil {
		t.Fatal(err)
	}
	native.closeReaderFile = func(f *os.File) error {
		if err := f.Close(); err != nil {
			return err
		}
		return f.Close()
	}
	dir := t.TempDir()
	o := AuthenticatedGovernanceOptions{Native: native, Identity: [32]byte{1}, RootKey: bytes.Repeat([]byte{0x72}, 32), Budget: governanceSealBudget(t), RootDirectory: filepath.Join(dir, "root"), RootMaxDiskBytes: 64 << 20, RootMaxAppendBytes: 8 << 20}
	p, ref, err := BuildTypedGovernanceProjection(ctx, o)
	if p == nil || !errors.Is(err, ownedfile.ErrRetirement) || ref != (TypedGovernanceReference{}) {
		t.Fatal(p, ref, err)
	}
	before := o.Budget.Snapshot().Used
	if before.RAM < 2*governanceSealWorkspaceRAMBytes || before.Disk < o.RootMaxDiskBytes {
		t.Fatal("actual source debt refunded construction", before)
	}
	for range 2 {
		if err := p.Discard(); !errors.Is(err, ownedfile.ErrRetirement) || o.Budget.Snapshot().Used != before {
			t.Fatal(err, o.Budget.Snapshot())
		}
	}
	if _, err := p.ReadHead(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(o.RootDirectory, "pages")); err != nil {
		t.Fatal(err)
	}
	if err := native.Close(); !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal(err)
	}
}

func TestTypedGovernanceDirectBuildPostReductionMutationAndCancellationRetireOnlyCandidate(t *testing.T) {
	for _, shape := range []string{"old-byte-restored-time", "cancel-before-publication"} {
		t.Run(shape, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			native, err := Open(filepath.Join(t.TempDir(), "native.wal"), bytes.Repeat([]byte{0x43}, 32))
			if err != nil {
				t.Fatal(err)
			}
			defer native.Close()
			for i := range 9 {
				if _, err := native.Append(ctx, projectionEvent(i)); err != nil {
					t.Fatal(err)
				}
			}
			dir := t.TempDir()
			o := AuthenticatedGovernanceOptions{Native: native, Identity: [32]byte{1}, RootKey: bytes.Repeat([]byte{0x72}, 32), Budget: governanceSealBudget(t), RootDirectory: filepath.Join(dir, "root"), RootMaxDiskBytes: 64 << 20, RootMaxAppendBytes: 8 << 20}
			changed := false
			p, ref, err := buildTypedGovernanceProjection(ctx, o, func(phase string) {
				if changed {
					return
				}
				if shape == "cancel-before-publication" && phase == "before-publication" {
					changed = true
					cancel()
				}
				if shape == "old-byte-restored-time" && phase == "batch-applied" {
					changed = true
					info, err := os.Stat(native.path)
					if err != nil {
						t.Fatal(err)
					}
					f, err := os.OpenFile(native.path, os.O_RDWR, 0)
					if err != nil {
						t.Fatal(err)
					}
					var b [1]byte
					if _, err := f.ReadAt(b[:], frameHeaderSize+10); err != nil {
						t.Fatal(err)
					}
					b[0] ^= 1
					if _, err := f.WriteAt(b[:], frameHeaderSize+10); err != nil {
						t.Fatal(err)
					}
					if err := f.Close(); err != nil {
						t.Fatal(err)
					}
					if err := os.Chtimes(native.path, info.ModTime(), info.ModTime()); err != nil {
						t.Fatal(err)
					}
				}
			})
			wanted := ErrCorrupt
			if shape == "cancel-before-publication" {
				wanted = context.Canceled
			}
			if !changed || p != nil || !errors.Is(err, wanted) || ref != (TypedGovernanceReference{}) {
				t.Fatal(changed, p, ref, err)
			}
			if _, err := os.Stat(o.RootDirectory); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed direct candidate survived known retirement", err)
			}
			if o.Budget.Snapshot().Used != (resourcebudget.Cost{}) {
				t.Fatal("known direct cleanup retained fee", o.Budget.Snapshot())
			}
			if _, err := os.Stat(native.path); err != nil {
				t.Fatal("candidate cleanup removed native authority", err)
			}
		})
	}
}

func TestTypedGovernanceDirectBuildCancellationDuringActualSourceExitIsNotPublication(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	native, err := Open(filepath.Join(t.TempDir(), "native.wal"), bytes.Repeat([]byte{0x43}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	if _, err := native.Append(ctx, projectionEvent(1)); err != nil {
		t.Fatal(err)
	}
	calls := 0
	native.closeReaderFile = func(f *os.File) error { calls++; err := f.Close(); cancel(); return err }
	dir := t.TempDir()
	o := AuthenticatedGovernanceOptions{Native: native, Identity: [32]byte{1}, RootKey: bytes.Repeat([]byte{0x72}, 32), Budget: governanceSealBudget(t), RootDirectory: filepath.Join(dir, "root"), RootMaxDiskBytes: 64 << 20, RootMaxAppendBytes: 8 << 20}
	p, ref, err := BuildTypedGovernanceProjection(ctx, o)
	if p != nil || !errors.Is(err, context.Canceled) || ref != (TypedGovernanceReference{}) || calls != 1 {
		t.Fatal(p, ref, err, calls)
	}
	if _, err := os.Stat(o.RootDirectory); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled source exit published a root", err)
	}
	if o.Budget.Snapshot().Used != (resourcebudget.Cost{}) || native.readerRetirementErr != nil || native.closing {
		t.Fatal("known source exit lost fees or poisoned native", o.Budget.Snapshot(), native.readerRetirementErr)
	}
}
