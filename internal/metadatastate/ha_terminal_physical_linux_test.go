//go:build linux

package metadatastate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/replicationlimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

func haPhysicalSelectedFixture(t *testing.T, replica bool) (*Baseline, selectedMetadataNativeFixture) {
	t.Helper()
	requireMetadataPhysicalExt4(t)
	role := authenticatedstate.SealPrimary
	if replica {
		role = authenticatedstate.SealReplica
	}
	f := selectedMetadataNativeCountRole(t, 0, true, 64<<20, role)
	ctx := context.Background()
	old := f.baseline
	a := f.options.Authority
	p := metadatajournal.BoundReplicationAuthority{Baseline: f.head.Baseline, FileRevision: f.head.FileRevision, MaxFileBytes: f.options.MaxBytes}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	rc, jc, err := old.DiskReservations()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := errors.Join(rc.Release(), jc.Release()); err != nil {
			t.Error(err)
		}
	})
	o := HASelectedSourceOpenOptions{PhysicalTerminalReserve: true, OpenOptions: OpenOptions{BaselineOptions: baselineOptionsForSelectedFixture(f), Evidence: old.evidence, RootDiskReservation: rc, JournalDiskReservation: jc, MaxJournalBytes: p.MaxFileBytes}, Publisher: f.options.Publisher, Guard: f.options.Guard, WithAuthority: func(ctx context.Context, visit func(context.Context, authenticatedstate.SealEligibility, HASelectedMetadataPosition, metadatajournal.BoundReplicationAuthority) error) error {
		return visit(ctx, a, HASelectedMetadataPosition{Epoch: f.head.Epoch, Sequence: f.head.Sequence, Hash: f.head.Hash}, p)
	}}
	b, _, err := OpenHASelectedSourceRoot(ctx, metadataTestKey(), old.output, f.path, o)
	if err != nil {
		t.Fatal(err)
	}
	if replica {
		if _, err := b.ConfigureHAReplicaJournal(ctx, metadataTestKey(), p); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := b.ConfigureHAPrimaryJournal(ctx, p, func(metadatajournal.DurableBatch) (metadatajournal.AppendAdmission, error) { return nil, nil }, func(metadatajournal.DurableBatch) (uint64, error) { return 1, nil }, func(context.Context, uint64) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	*f.rootReplaced = true
	t.Cleanup(func() {
		err := b.Close()
		if err != nil && !errors.Is(err, authenticatedstate.ErrRecovery) {
			t.Error(err)
		}
	})
	return b, f
}

// Component fixture: real selected root/format2 native writer and native
// admission/confirmation hooks. Actual Member/Ordering/TLS is an App gate.
func TestHAMetadataPhysicalPrimaryActualSelectedAppendConsumesPrepaidRoot(t *testing.T) {
	b, f := haPhysicalSelectedFixture(t, false)
	ctx := context.Background()
	if err := b.EnableHAPhysicalTerminals(ctx); err != nil {
		t.Fatal("actual HA enable", err)
	}
	if err := b.EnableQualifiedPhysicalTerminals(ctx, f.options.Publisher, f.options.Guard, f.options.Authority); err == nil {
		t.Fatal("Standalone guard admitted HA")
	}
	w, err := b.ReservePhysicalTerminals(ctx, 1)
	if err != nil {
		t.Fatal("original HA prepaid window", err)
	}
	defer w.Close()
	// A generic authoritative write pays above terminal debt; original native
	// hooks still run and actual COW commits before confirmation returns.
	if _, err := b.AppendFrame(ctx, []metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("physical-ha"), Value: []byte("v")}}); err != nil {
		t.Fatal("actual bound native/COW", err)
	}
	if b.position.Sequence != 1 || b.journal.Head().Sequence != 1 || w.remaining != 1 {
		t.Fatal("semantic write consumed unrelated terminal reservation", b.position, w.remaining)
	}
	if err := b.View(ctx, func(tx *Transaction) error {
		v, found, err := tx.Get([]string{"projects"}, []byte("physical-ha"))
		if !found || string(v) != "v" {
			return ErrSource
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestHAMetadataPhysicalReplicaActualReceiveEscrowAndOneFrameCatchup(t *testing.T) {
	b, f := haPhysicalSelectedFixture(t, true)
	ctx := context.Background()
	if err := b.EnableHAPhysicalTerminals(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.PrepareHAReplicaPhysicalEscrow(ctx, 3); err != nil {
		t.Fatal("prepaid original Replica vector", err)
	}
	w := b.haPhysicalSource.receiveWindow
	header, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "source.journal")
	if err := os.WriteFile(path, header, 0600); err != nil {
		t.Fatal(err)
	}
	source, err := metadatajournal.OpenBoundContext(ctx, path, f.key, f.head.Baseline, f.head.FileRevision, f.options.MaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	for i := range 4 {
		if _, err := source.Append([]metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte{byte(i)}, Value: []byte("actual-replicated")}}); err != nil {
			t.Fatal(err)
		}
	}
	p := *b.haReplicaPrepared
	r, err := metadatajournal.NewBoundReplicationRangeReader(path, f.key, 1<<20, p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	used := b.budget.Snapshot().Used
	if err := b.budget.SetPressure(resourcebudget.Pressured); err != nil {
		t.Fatal(err)
	}
	for seq := uint64(1); seq <= 3; seq++ {
		frames, err := r.Read(p.Baseline.Epoch, seq, seq)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.journal.AppendReplicatedContext(ctx, p.Baseline.Epoch, seq, seq, frames); err != nil {
			t.Fatal("accepted receive under pressure", seq, err)
		}
		if err := b.journal.AppendReplicatedContext(ctx, p.Baseline.Epoch, seq, seq, frames); err != nil {
			t.Fatal("durable duplicate must not charge a new target", err)
		}
	}
	frames, err := r.Read(p.Baseline.Epoch, 4, 4)
	if err != nil {
		t.Fatal(err)
	}
	before := b.journal.Head()
	if err := b.journal.AppendReplicatedContext(ctx, p.Baseline.Epoch, 4, 4, frames); !errors.Is(err, resourcebudget.ErrPhysicalAllocation) {
		t.Fatal("unfunded fourth frame admitted", err)
	}
	if b.journal.Head() != before || w.remaining != 3 || b.position.Sequence != 0 || b.budget.Snapshot().Used != used {
		t.Fatal("preaccept refusal changed original cursor/target/fees")
	}
	if _, err := b.ReplayTo(ctx, before, 1); err == nil {
		t.Fatal("unconfirmed native head authorized physical Applied")
	}
	confirmed, err := metadatajournal.BoundReplicationPrefixHead(ctx, path, f.key, p, p.Baseline.Epoch, 3)
	if err != nil || confirmed != before {
		t.Fatal("separate authenticated source confirmation changed", err)
	}
	b.haPhysicalSource.withConfirmedApply = testHAMetadataPhysicalConfirmedSource(confirmed)
	for seq := uint64(1); seq <= 3; seq++ {
		if _, err := b.ReplayTo(ctx, before, seq); err != nil {
			t.Fatal("original confirmed source reduction", seq, err)
		}
	}
	if w.remaining != 0 || b.physicalDebt != 0 || b.position.Sequence != 3 || b.budget.Snapshot().Used != used {
		t.Fatal("one-frame jobs did not retain/consume the original window", w.remaining, b.physicalDebt, b.position)
	}
	if err := b.budget.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	if err := b.View(ctx, func(tx *Transaction) error {
		for i := range 3 {
			v, found, err := tx.Get([]string{"projects"}, []byte{byte(i)})
			if err != nil || !found || string(v) != "actual-replicated" {
				return errors.Join(ErrSource, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestHAMetadataPhysicalFreshReplicaRecoveryAuthenticatesOriginalNativeBeforeReduce(t *testing.T) {
	testHAMetadataPhysicalFreshReplicaPermittedPrefix(t, 2)
}

func TestHAMetadataPhysicalFreshReplicaExcludesAuthenticatedUnappliedTail(t *testing.T) {
	testHAMetadataPhysicalFreshReplicaPermittedPrefix(t, 1)
}

func testHAMetadataPhysicalFreshReplicaPermittedPrefix(t *testing.T, permitted uint64) {
	t.Helper()
	b, f := haPhysicalSelectedFixture(t, true)
	ctx := context.Background()
	if err := b.EnableHAPhysicalTerminals(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.PrepareHAReplicaPhysicalEscrow(ctx, 2); err != nil {
		t.Fatal(err)
	}
	header, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "authoritative-source.journal")
	if err := os.WriteFile(path, header, 0600); err != nil {
		t.Fatal(err)
	}
	p := *b.haReplicaPrepared
	source, err := metadatajournal.OpenBoundContext(ctx, path, f.key, p.Baseline, p.FileRevision, p.MaxFileBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	for i := range 2 {
		if _, err := source.Append([]metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte{byte(i)}, Value: []byte("before-crash")}}); err != nil {
			t.Fatal(err)
		}
	}
	fullHead := source.Head()
	independentHead, err := metadatajournal.BoundReplicationPrefixHead(ctx, path, f.key, p, p.Baseline.Epoch, permitted)
	if err != nil {
		t.Fatal(err)
	}
	r, err := metadatajournal.NewBoundReplicationRangeReader(path, f.key, 1<<20, p)
	if err != nil {
		t.Fatal(err)
	}
	frames, err := r.Read(p.Baseline.Epoch, 1, 2)
	err = errors.Join(err, r.Close())
	if err != nil {
		t.Fatal(err)
	}
	if err := b.journal.AppendReplicated(p.Baseline.Epoch, 1, 2, frames); err != nil {
		t.Fatal(err)
	}
	if b.journal.Head() != fullHead || b.position.Sequence != 0 {
		t.Fatal("fixture lost the independently authored source cut")
	}
	if err := b.Close(); !errors.Is(err, authenticatedstate.ErrRecovery) {
		t.Fatal("old borrowed owners must retain unselected targets", err)
	}
	common, found, err := f.options.Publisher.Current()
	if err != nil || !found {
		t.Fatal(err)
	}
	if err := f.options.Publisher.Close(); err != nil {
		t.Fatal(err)
	}
	m := metadataPhysicalBudget(t)
	rc, err := m.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Release()
	jc, err := m.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: p.MaxFileBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer jc.Release()
	pc, err := m.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 32 << 10})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Release()
	hard := authenticatedstate.SealHardLimits{MaxPageBytes: common.MaxPageBytes, MaxValueBytes: common.MaxValueBytes, MaxTreeHeight: common.MaxTreeHeight, RequiredRoots: []authenticatedstate.SealRootSchema{{Kind: authenticatedstate.SealRootState, Component: authenticatedstate.SealComponentMetadata, IndexID: AuthenticatedMetadataSealIndexID, Codec: AuthenticatedMetadataSealCodec}}}
	pub, err := authenticatedstate.OpenSealCandidatePublisher(filepath.Dir(f.path), f.options.Guard, f.key, hard, m, pc)
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	bo := baselineOptionsForSelectedFixture(f)
	bo.Budget = m
	o := HASelectedSourceOpenOptions{PhysicalTerminalReserve: true, OpenOptions: OpenOptions{BaselineOptions: bo, Evidence: b.evidence, RootDiskReservation: rc, JournalDiskReservation: jc, MaxJournalBytes: p.MaxFileBytes}, Publisher: pub, Guard: f.options.Guard, WithAuthority: func(ctx context.Context, visit func(context.Context, authenticatedstate.SealEligibility, HASelectedMetadataPosition, metadatajournal.BoundReplicationAuthority) error) error {
		return visit(ctx, f.options.Authority, HASelectedMetadataPosition{Epoch: independentHead.Epoch, Sequence: independentHead.Sequence, Hash: independentHead.Hash}, p)
	}}
	fresh, receipt, err := OpenHASelectedSourceRoot(ctx, metadataTestKey(), b.output, f.path, o)
	if err != nil {
		t.Fatal("actual fresh original-native/COW restore", err)
	}
	defer fresh.Close()
	actualNative := fresh.journal
	if _, err := fresh.ConfigureHAReplicaJournal(ctx, metadataTestKey(), p); err != nil {
		t.Fatal(err)
	}
	if fresh.journal != actualNative || fresh.position != independentHead || receipt.AppliedFrames != permitted || receipt.NativeHead != fullHead || fresh.physicalDebt != int(2-permitted) || fresh.haPhysicalSource.receiveWindow == nil {
		t.Fatal("factory changed original FD/permission/window", receipt, fresh.position)
	}
	if err := fresh.View(ctx, func(tx *Transaction) error {
		for i := range permitted {
			v, found, err := tx.Get([]string{"projects"}, []byte{byte(i)})
			if err != nil || !found || string(v) != "before-crash" {
				return errors.Join(ErrSource, err)
			}
		}
		if permitted < 2 {
			_, found, err := tx.Get([]string{"projects"}, []byte{1})
			if err != nil || found {
				return errors.Join(ErrSource, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestHAMetadataPhysicalOriginalFacadeTerminalPermissionAndExpiredScope(t *testing.T) {
	b, _ := haPhysicalSelectedFixture(t, false)
	ctx := context.Background()
	if err := b.EnableHAPhysicalTerminals(ctx); err != nil {
		t.Fatal(err)
	}
	pin := physicalPin(t)
	raw, err := json.Marshal(pin)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := b.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var escaped context.Context
	permission := func(ctx context.Context, visit func(context.Context, authenticatedstate.SealPosition) error) error {
		member, err := b.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: replicationlimits.MemberAuthorityRAMBytes})
		if err != nil {
			return err
		}
		defer member.Release()
		ordering, err := b.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: replicationlimits.OrderingFrozenReadRAMBytes})
		if err != nil {
			return err
		}
		defer ordering.Release()
		escaped = ctx
		return visit(ctx, metadataSealPosition(b.position))
	}
	backend, err := b.HAFacade(ctx, b.budget, b.evidence.Identity, b.evidence.InstanceID, b.evidence.KeyGeneration, ref, authenticatedstate.SealPrimary, permission)
	if err != nil {
		t.Fatal(err)
	}
	store, err := boltstore.NewFacadeStore(ctx, backend)
	if err != nil {
		t.Fatal(err)
	}
	w, err := b.ReservePhysicalTerminals(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Update(w.Bind(ctx), func(tx *Transaction) error {
		return tx.Put([]string{"deployment_price_pin_intents"}, []byte(pin.AttemptID), raw)
	}); err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := b.budget.SetPressure(resourcebudget.Pressured); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitDeploymentPricePin(w.Bind(ctx), pin.AttemptID, pin.SnapshotSHA256, 7, time.Now().UTC()); err != nil {
		t.Fatal("actual same-window HA permission/native/COW terminal", err)
	}
	before := b.journal.Head()
	if w.remaining != 0 || b.physicalDebt != 0 {
		t.Fatal("terminal did not consume original prepaid target")
	}
	if _, err := backend.Update(escaped, func(boltstore.FacadeTransaction) error { t.Fatal("expired scope invoked DAO"); return nil }); !errors.Is(err, resourcebudget.ErrClosed) {
		t.Fatal("private completed workspace scope revived", err)
	}
	if b.journal.Head() != before {
		t.Fatal("expired scope appended a fact")
	}
}

func TestHAMetadataPhysicalCommonRetirementRenewsOnlyOriginalReceiveOwner(t *testing.T) {
	b, f := haPhysicalSelectedFixture(t, true)
	ctx := context.Background()
	if err := b.EnableHAPhysicalTerminals(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.PrepareHAReplicaPhysicalEscrow(ctx, 3); err != nil {
		t.Fatal(err)
	}
	oldWindow := b.haPhysicalSource.receiveWindow
	registration, native := b.haPhysicalSource.receiveRegistration, b.journal
	caller, err := b.acquirePhysicalCompletion()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := errors.Join(caller.Close(), caller.Active.Release()); err != nil {
			t.Error(err)
		}
	}()
	publishMetadataPhysicalActual(t, b, f.options.Publisher)
	if err := b.budget.SetPressure(resourcebudget.Pressured); err != nil {
		t.Fatal(err)
	}
	if err := resourcebudget.WithCompletionWorkspace(caller.Bind(ctx), func(ctx context.Context) error {
		if err := b.RetireHAPhysicalPreparationAfterCommon(ctx); err != nil {
			return err
		}
		return b.BindHAPhysicalCommonSelection(ctx)
	}); err != nil {
		t.Fatal("actual selected common retire/bind under original paid work", err)
	}
	if !oldWindow.closed.Load() || oldWindow.remaining != 0 || b.physicalDebt != 0 || b.haPhysicalSource.receiveWindow != nil || b.journal != native {
		t.Fatal("retirement lost or guessed original window/native lifecycle")
	}
	// New admission is refused while pressure persists. The already retired
	// original receiver registration cannot manufacture a replacement pool.
	used := b.budget.Snapshot().Used
	if err := b.PrepareHAReplicaPhysicalEscrow(ctx, 2); !errors.Is(err, resourcebudget.ErrPressure) {
		t.Fatal("new receive work borrowed completed admission", err)
	}
	if b.budget.Snapshot().Used != used || b.haPhysicalSource.receiveWindow != nil {
		t.Fatal("refused renewal retained guessed targets")
	}
	if err := b.budget.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	if err := b.PrepareHAReplicaPhysicalEscrow(ctx, 2); err != nil {
		t.Fatal("actual fresh admission renewal", err)
	}
	if b.haPhysicalSource.receiveRegistration != registration || b.haPhysicalSource.receiveWindow == oldWindow || b.journal != native || b.physicalDebt != 2 {
		t.Fatal("renewal changed original receiver owner or reused retired work")
	}
	if err := b.RetireHAPhysicalPreparationAfterCommon(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.BindHAPhysicalCommonSelection(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal("actual common-qualified HA shutdown", err)
	}
}

// Component-only independent source receipt. Actual Runtime uses the original
// receiver's opaque confirmed Member/Ordering/four-source scope instead.
func testHAMetadataPhysicalConfirmedSource(confirmed metadatajournal.Head) func(context.Context, *Baseline, metadatajournal.Head, uint64, func(context.Context) error) error {
	return func(ctx context.Context, _ *Baseline, expected metadatajournal.Head, through uint64, visit func(context.Context) error) error {
		if expected.Epoch != confirmed.Epoch || expected.Baseline != confirmed.Baseline || expected.FileRevision != confirmed.FileRevision || through > confirmed.Sequence {
			return ErrSource
		}
		return visit(ctx)
	}
}
