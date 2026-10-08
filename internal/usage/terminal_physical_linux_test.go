//go:build linux

package usage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
	"golang.org/x/sys/unix"
)

type usagePhysicalCapacity struct {
	mu           sync.Mutex
	used, unused int64
}

func (c *usagePhysicalCapacity) reserve(n int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n <= 0 || n > (64<<20)-c.used-c.unused {
		return resourcebudget.ErrExhausted
	}
	c.used += n
	return nil
}
func (c *usagePhysicalCapacity) ReservePhysicalRange(n int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n <= 0 || n > (64<<20)-c.used-c.unused {
		return resourcebudget.ErrExhausted
	}
	c.unused += n
	return nil
}
func (c *usagePhysicalCapacity) ConsumePhysicalRange(n int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n <= 0 || n > c.unused {
		return resourcebudget.ErrInvalid
	}
	c.unused -= n
	c.used += n
	return nil
}
func (c *usagePhysicalCapacity) RetirePhysicalRange(n int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n <= 0 || n > c.unused {
		return resourcebudget.ErrInvalid
	}
	c.unused -= n
	return nil
}

type usagePhysicalFixture struct {
	root          string
	p             *DiskProjection
	d             *LedgerDerivative
	native        *ledger.Log
	owner         *ledger.AuthenticatedLedgerProjection
	lease         *ledger.MaintenanceLease
	publisher     *authenticatedstate.SealCandidatePublisher
	seal          authenticatedstate.CandidateSeal
	guard         *lock.Lock
	manager       *resourcebudget.Manager
	creator       *DiskProjection
	ledgerCreator *ledger.AuthenticatedLedgerProjection
	directory     *ledger.AuthenticatedDirectory
	directoryLive *ledger.AuthenticatedDirectory
	credits       []resourcebudget.Permit
}

func newUsagePhysicalFixture(t *testing.T) *usagePhysicalFixture {
	ctx := context.Background()
	dir := os.Getenv("HALRO_USAGE_PHYSICAL_ROOT")
	if dir == "" {
		dir = t.TempDir()
	} else if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Type != unix.EXT4_SUPER_MAGIC {
		t.Skip("persistent physical terminal recovery requires ext4; set TMPDIR or HALRO_USAGE_PHYSICAL_ROOT to a qualified ext4 fixture")
	}
	path := filepath.Join(dir, "ledger.wal")
	key := bytes.Repeat([]byte{0x41}, 32)
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 1536 << 20, Disk: 512 << 20}, MaxClaims: 96, MaxOwners: 16, MaxPressureAge: time.Hour}
	for i := range limits.Classes {
		limits.Classes[i] = limits.Total
	}
	limits.Classes[resourcebudget.Recovery] = resourcebudget.Cost{}
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 96 * resourcebudget.ClaimRAMBytes}
	manager, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	native, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: key})
	if err != nil {
		t.Fatal(err)
	}
	report, partial, err := ledger.InspectReplayAuthenticated(path, key, nil)
	if err != nil || partial {
		t.Fatal(partial, err)
	}
	identity := sha256.Sum256([]byte("actual-selected-usage-physical"))
	lo := ledger.AuthenticatedLedgerOptions{Identity: identity, MaxDiskBytes: 32 << 20, MaxAppendBytes: 2 << 20, Budget: manager}
	owner, err := ledger.BuildAuthenticatedLedgerProjection(ctx, path, key, filepath.Join(dir, "ledger-root"), report, lo)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := native.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	directoryOptions := ledger.AuthenticatedDirectoryOptions{Identity: identity, Budget: manager, MaxDiskBytes: 16 << 20, MaxAppendBytes: 4 << 20}
	directory, err := ledger.BuildAuthenticatedDirectory(ctx, native, lease, owner, filepath.Join(dir, "directory"), directoryOptions)
	if err != nil {
		t.Fatal(err)
	}
	options := AuthenticatedUsageOptions{Native: native, Owner: owner, Identity: identity, UsageKey: bytes.Repeat([]byte{0x42}, 32), Budget: manager, RootDirectory: filepath.Join(dir, "usage"), RootMaxDiskBytes: 256 << 20, RootMaxAppendBytes: 4 << 20}
	creator, ur, err := BuildTypedUsageProjection(ctx, options, lease)
	if err != nil {
		t.Fatal(err)
	}
	lr, err := owner.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dr, err := directory.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := creator.Close(); err != nil {
		t.Fatal(err)
	}
	uf, err := creator.TypedDiskReservation()
	if err != nil {
		t.Fatal(err)
	}
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
	df, err := directory.DiskReservation()
	if err != nil {
		t.Fatal(err)
	}
	directoryOptions.DiskReservation = df
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	lf, err := owner.DiskReservation()
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := native.Close(); err != nil {
		t.Fatal(err)
	}
	guard, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	seal := authenticatedstate.CandidateSeal{SchemaVersion: 1, ApplyVersion: 1, IndexVersion: 1, KeyDerivation: 1, Instance: identity, Incarnation: [16]byte{1}, Generation: 1, Revision: 1, Role: authenticatedstate.SealStandalone, Coverage: authenticatedstate.SealCoverageAuthenticated, MaxPageBytes: 4 << 20, MaxValueBytes: 3 << 20, MaxTreeHeight: 32, PositionCount: 5, RootCount: 3}
	seal.Roots[0], seal.Roots[1], seal.Roots[2] = lr.Root, ur.Projection.Root, dr.Root
	seal.Positions[0] = dr.Position
	seal.Positions[1] = authenticatedstate.SealPosition{Component: authenticatedstate.SealComponentMetadata, Epoch: 1, Revision: 1, MetadataBaseline: [32]byte{2}}
	seal.Positions[2].Component = authenticatedstate.SealComponentGovernance
	seal.Positions[3].Component = authenticatedstate.SealComponentAudit
	seal.Positions[4] = ur.Projection.Position
	hard := authenticatedstate.SealHardLimits{MaxPageBytes: 4 << 20, MaxValueBytes: 3 << 20, MaxTreeHeight: 32, RequiredRoots: []authenticatedstate.SealRootSchema{lr.Schema, ur.Projection.Schema, dr.Schema}}
	pubFee, err := manager.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 32 << 10})
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := authenticatedstate.OpenSealCandidatePublisher(dir, guard, key, hard, manager, pubFee)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Publish(ctx, seal); err != nil {
		t.Fatal(err)
	}
	authority := authenticatedstate.SealEligibility{SchemaVersion: 1, ApplyVersion: 1, IndexVersion: 1, KeyDerivation: 1, Instance: identity, Incarnation: seal.Incarnation, Role: seal.Role, Coverage: seal.Coverage}
	copy(authority.Positions[:], seal.Positions[:])
	capacity := &usagePhysicalCapacity{}
	so := ledger.SelectedSealOpenOptions{Publisher: publisher, Guard: guard, Authority: authority, Directory: filepath.Join(dir, "directory"), DirectoryOptions: directoryOptions}
	native, _, err = ledger.OpenWithSelectedSeal(ctx, path, ledger.NewStatus(), ledger.Options{ChainKey: key, ReserveAppendDisk: capacity.reserve, MaxBatch: 1}, so)
	if err != nil {
		t.Fatal(err)
	}
	if err := native.AuditSelectedStartup(ctx, manager); err != nil {
		t.Fatal(err)
	}
	lease, err = native.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	head, hash, _ := native.ChainHead()
	lo.Prefix = &ledger.ProjectionPrefix{Head: head, Hash: hash}
	source := ledger.SelectedLedgerProjectionOpenOptions{Publisher: publisher, Guard: guard, Authority: authority}
	next, _, err := ledger.OpenSelectedAuthenticatedLedgerProjection(ctx, native, filepath.Join(dir, "ledger-root"), ledger.AuthenticatedLedgerOpenOptions{AuthenticatedLedgerOptions: lo, SavedAnchor: lr.Anchor, DiskReservation: lf}, source)
	if err != nil {
		t.Fatal(err)
	}
	growth, err := manager.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(seal)
	selection := sha256.Sum256(raw)
	if err := next.EnableQualifiedPhysicalTerminals(ctx, native, 64<<20, growth, capacity, selection); err != nil {
		t.Fatal(err)
	}
	if err := native.BindPhysicalSelectionDigestContext(ctx, selection); err != nil {
		t.Fatal(err)
	}
	source.PhysicalRecovery = &ledger.PhysicalStartupRecoveryOptions{}
	directoryLive, err := ledger.OpenSelectedAuthenticatedDirectory(ctx, native, dr, filepath.Join(dir, "directory"), directoryOptions, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := directoryLive.EnablePhysicalCatchup(ctx, native, next, selection); err != nil {
		t.Fatal(err)
	}
	if err := directoryLive.InstallPhysicalCatchupCoverage(ctx, native, next); err != nil {
		t.Fatal(err)
	}
	options.Native, options.Owner, options.PhysicalTerminalReserve = native, next, true
	d, p, err := OpenSelectedTypedDiskLedgerDerivative(ctx, options, lease, ur, uf, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnablePhysicalCatchup(ctx, native, next, selection); err != nil {
		t.Fatal(err)
	}
	if err := native.RequirePhysicalDerivedCoverage(ctx, next); err != nil {
		t.Fatal(err)
	}
	f := &usagePhysicalFixture{root: dir, p: p, d: d, native: native, owner: next, lease: lease, publisher: publisher, seal: seal, guard: guard, manager: manager, creator: creator, ledgerCreator: owner, directory: directory, directoryLive: directoryLive, credits: []resourcebudget.Permit{uf, lf, df, pubFee, growth}}
	t.Cleanup(func() {
		d.Close()
		_ = p.Close()
		_ = lease.Close()
		_ = directoryLive.Close()
		_ = native.Close()
		_ = next.Close()
		_ = publisher.Close()
		_ = guard.Close()
	})
	return f
}
func (f *usagePhysicalFixture) append(ctx context.Context, e ledger.Event) error {
	ctx = ledger.WithPhysicalAppendKind(ctx, e.Kind)
	return f.owner.WithPreparedPhysicalAppend(ctx, func(ctx context.Context) error {
		return f.owner.WithNativeApplyWorkspace(ctx, func(ctx context.Context) error {
			_, _, err := f.native.AppendApplyRecordWithReceipt(ctx, e, func(ctx context.Context, r ledger.Record) error {
				_, err := f.owner.AdvanceOneFromNative(ctx, f.native, ledger.ProjectionPrefix{Head: typedUsageRecordAnchor(r).Head, Hash: r.Hash})
				return err
			})
			return err
		})
	})
}
func (f *usagePhysicalFixture) publishAndRetire(t *testing.T) {
	ctx := context.Background()
	lease, err := f.native.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.lease = lease
	if err := f.p.BindTypedMaintenance(ctx, f.native, lease, f.owner, f.manager); err != nil {
		t.Fatal(err)
	}
	if _, err := f.directoryLive.AdvanceFromNative(ctx, f.native, lease, f.owner); err != nil {
		t.Fatal(err)
	}
	dr, err := f.directoryLive.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lr, err := f.owner.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ur, err := f.p.TypedSealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seal := f.seal
	seal.Revision++
	seal.Roots[0], seal.Roots[1], seal.Roots[2] = lr.Root, ur.Projection.Root, dr.Root
	seal.Positions[0] = authenticatedstate.SealPosition{Component: authenticatedstate.SealComponentLedger, Epoch: uint64(lr.Anchor.Epoch), Generation: lr.Anchor.Head.Generation, Sequence: lr.Anchor.Head.Sequence, Offset: uint64(lr.Anchor.Head.Offset), Hash: lr.Anchor.Hash}
	seal.Positions[4] = ur.Projection.Position
	if _, err := f.publisher.Publish(ctx, seal); err != nil {
		t.Fatal(err)
	}
	if err := f.p.RetirePhysicalCatchupAfterCommon(ctx, f.native, f.owner, lease, f.publisher); err != nil {
		t.Fatal(err)
	}
	if err := f.directoryLive.RetirePhysicalCatchupAfterCommon(ctx, f.native, f.owner, lease, f.publisher); err != nil {
		t.Fatal(err)
	}
	if err := f.native.RetirePhysicalPreparationForMaintenance(ctx, lease); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(seal)
	digest := sha256.Sum256(raw)
	if err := f.native.BindPhysicalSelectionDigestContext(ctx, digest); err != nil {
		t.Fatal(err)
	}
	if err := f.p.BindPhysicalCommonSelection(ctx, f.native, f.owner, digest); err != nil {
		t.Fatal(err)
	}
	if err := f.directoryLive.BindPhysicalCommonSelection(ctx, f.native, f.owner, digest); err != nil {
		t.Fatal(err)
	}
}

func TestUsagePhysicalSelectedEachFrameCatchupAndKnownRetirement(t *testing.T) {
	f := newUsagePhysicalFixture(t)
	ctx := context.Background()
	if err := f.p.ActivateTypedLive(ctx, f.native, f.manager); err != nil {
		t.Fatal(err)
	}
	if err := f.lease.Close(); err != nil {
		t.Fatal(err)
	}
	rows := finalizationFixture(false, false)
	for _, r := range rows {
		if err := f.append(ctx, r.Event); err != nil {
			t.Fatal(err)
		}
	}
	if f.p.physical.remaining != len(rows) {
		t.Fatal("each native frame lacked its own COW target", f.p.physical.remaining, len(rows))
	}
	before := f.manager.Snapshot().Used
	if err := f.manager.SetPressure(resourcebudget.Pressured); err != nil {
		t.Fatal(err)
	}
	receipt, err := f.d.CatchUpTypedLive(ctx, f.native, f.manager)
	if err != nil || receipt.AppliedRecords != uint64(len(rows)) || f.p.physical.remaining != 0 {
		t.Fatal("prepaid derived continuation failed under pressure", receipt, f.p.physical.remaining, err)
	}
	if f.manager.Snapshot().Used != before {
		t.Fatal("joined prepaid continuation changed fixed fees", before, f.manager.Snapshot())
	}
	if err := f.manager.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	f.publishAndRetire(t)
	if err := f.p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.directoryLive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.native.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.owner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.creator.AdoptClosedTypedSuccessor(f.p); err != nil {
		t.Fatal(err)
	}
	if err := f.ledgerCreator.AdoptClosedSuccessor(f.owner); err != nil {
		t.Fatal(err)
	}
	if err := f.creator.Discard(); err != nil {
		t.Fatal(err)
	}
	if err := f.ledgerCreator.Discard(); err != nil {
		t.Fatal(err)
	}
	if err := f.directoryLive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.directory.AdoptClosedSuccessor(f.directoryLive); err != nil {
		t.Fatal(err)
	}
	if err := f.directory.Discard(); err != nil {
		t.Fatal(err)
	}
	if err := f.publisher.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.publisher.Retire(); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.credits {
		if err := c.Release(); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.manager.Snapshot().Used; got != (resourcebudget.Cost{}) {
		t.Fatal("actual joined component owners leaked fees", got)
	}
}

func TestUsagePhysicalPoolBoundStopsBeforeNativeAcceptance(t *testing.T) {
	f := newUsagePhysicalFixture(t)
	ctx := context.Background()
	if err := f.p.ActivateTypedLive(ctx, f.native, f.manager); err != nil {
		t.Fatal(err)
	}
	if err := f.lease.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < resourcebudget.MaxCompletionBatch; i++ {
		e := finalizationFixture(false, false)[0].Event
		e.RequestID = fmt.Sprintf("pending-%02d", i)
		e.EventID = fmt.Sprintf("accepted-%02d", i)
		if err := f.append(ctx, e); err != nil {
			t.Fatal("real fixed pool did not cover admitted frame", i, err)
		}
	}
	before, _, _ := f.native.ChainHead()
	e := finalizationFixture(false, false)[0].Event
	e.RequestID = "overflow"
	e.EventID = "overflow"
	err := f.append(ctx, e)
	after, _, _ := f.native.ChainHead()
	if !errors.Is(err, resourcebudget.ErrExhausted) || after != before || f.p.physical.remaining != 32 {
		t.Fatal("fixed 32 actual targets failed preaccept bound", before, after, f.p.physical.remaining, err)
	}
	if err := f.p.Close(); !errors.Is(err, authenticatedstate.ErrRecovery) {
		t.Fatal("unqualified target cleanup was refunded", err)
	}
}

func TestUsagePhysicalActualCollectorPrepaidWatermarksAtPressure(t *testing.T) {
	f := newUsagePhysicalFixture(t)
	ctx := context.Background()
	if err := f.p.ActivateTypedLive(ctx, f.native, f.manager); err != nil {
		t.Fatal(err)
	}
	if err := f.lease.Close(); err != nil {
		t.Fatal(err)
	}
	collector, err := NewTypedLiveCollector(f.d, f.native, f.manager, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := collector.Retire(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	for _, r := range finalizationFixture(false, false) {
		if err := f.append(ctx, r.Event); err != nil {
			t.Fatal(err)
		}
	}
	before := f.manager.Snapshot().Used
	if err := f.manager.SetPressure(resourcebudget.Pressured); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.ReadWatermark(ctx); !errors.Is(err, resourcebudget.ErrPressure) {
		t.Fatal("ordinary watermark borrowed physical credits", err)
	}
	if err := collector.CatchUp(ctx); err != nil || f.p.physical.remaining != 0 {
		t.Fatal("actual Collector watermark/replay/watermark continuation failed", f.p.physical.remaining, err)
	}
	if got := f.manager.Snapshot().Used; got != before {
		t.Fatal("Collector escaped or refunded its original work", before, got)
	}
	if collector.Stats().Lagging {
		t.Fatal("completed actual Collector stayed lagging")
	}
}

func TestUsagePhysicalActualExt4PrepaidCatchupAtENOSPC(t *testing.T) {
	root := os.Getenv("HALRO_EXT4_ROOT")
	if root == "" {
		t.Skip("requires owned isolated ext4 fixture")
	}
	var st unix.Statfs_t
	if err := unix.Statfs(root, &st); err != nil || st.Type != unix.EXT4_SUPER_MAGIC {
		t.Fatal("actual ext4 required", err)
	}
	t.Setenv("TMPDIR", root)
	f := newUsagePhysicalFixture(t)
	ctx := context.Background()
	if err := f.p.ActivateTypedLive(ctx, f.native, f.manager); err != nil {
		t.Fatal(err)
	}
	if err := f.lease.Close(); err != nil {
		t.Fatal(err)
	}
	for _, r := range finalizationFixture(false, false) {
		if err := f.append(ctx, r.Event); err != nil {
			t.Fatal(err)
		}
	}
	filler, err := os.OpenFile(filepath.Join(root, "usage-owned-filler"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = filler.Close(); _ = os.Remove(filler.Name()) }()
	for offset := int64(0); ; offset += 4096 {
		err = unix.Fallocate(int(filler.Fd()), 0, offset, 4096)
		if errors.Is(err, syscall.ENOSPC) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := filler.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := f.manager.SetPressure(resourcebudget.Pressured); err != nil {
		t.Fatal(err)
	}
	receipt, err := f.d.CatchUpTypedLive(ctx, f.native, f.manager)
	if err != nil || receipt.AppliedRecords != 5 || f.p.physical.remaining != 0 {
		t.Fatal("actual prepaid per-frame COW failed on full ext4", receipt, err)
	}
	if err := f.p.Close(); !errors.Is(err, authenticatedstate.ErrRecovery) {
		t.Fatal("unpublished current root incorrectly retired", err)
	}
}

func TestUsagePhysicalReservedTerminalVectorAtActualENOSPC(t *testing.T) {
	root := os.Getenv("HALRO_EXT4_ROOT")
	if root == "" {
		t.Skip("requires owned isolated ext4 fixture")
	}
	var st unix.Statfs_t
	if err := unix.Statfs(root, &st); err != nil || st.Type != unix.EXT4_SUPER_MAGIC {
		t.Fatal("actual ext4 required", err)
	}
	t.Setenv("TMPDIR", root)
	f := newUsagePhysicalFixture(t)
	ctx := context.Background()
	if err := f.p.ActivateTypedLive(ctx, f.native, f.manager); err != nil {
		t.Fatal(err)
	}
	if err := f.lease.Close(); err != nil {
		t.Fatal(err)
	}
	profile := ledger.CompletionWorkspaceRequirements()
	var completionRAM int64
	for _, r := range profile {
		completionRAM += r.RAM
	}
	work, err := f.manager.TryAcquireCompletableWork(resourcebudget.IO, resourcebudget.Cost{RAM: 4096}, resourcebudget.Cost{RAM: completionRAM}, profile)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = work.Close(); _ = work.Active.Release() }()
	w, err := f.native.ReservePhysicalTerminals(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if f.p.physical.remaining != 2 || f.native.PhysicalTerminalDebt() != 2 {
		t.Fatal("terminal vector was not prepaid before request acceptance", f.p.physical.remaining, f.native.PhysicalTerminalDebt())
	}
	rows := finalizationFixture(false, false)
	for _, r := range rows[:3] {
		if err := f.append(ctx, r.Event); err != nil {
			t.Fatal(err)
		}
	}
	if f.p.physical.remaining != len(rows) {
		t.Fatal("ordinary facts consumed future terminal coverage", f.p.physical.remaining)
	}
	filler, err := os.OpenFile(filepath.Join(root, "usage-terminal-owned-filler"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = filler.Close(); _ = os.Remove(filler.Name()) }()
	for offset := int64(0); ; offset += 4096 {
		err = unix.Fallocate(int(filler.Fd()), 0, offset, 4096)
		if errors.Is(err, syscall.ENOSPC) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := filler.Sync(); err != nil {
		t.Fatal(err)
	}
	before := f.manager.Snapshot().Used
	if err := f.manager.SetPressure(resourcebudget.Pressured); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows[3:] {
		if err := resourcebudget.WithCompletionWorkspace(work.Bind(w.Bind(ctx)), func(ctx context.Context) error {
			return f.append(ctx, r.Event)
		}); err != nil {
			t.Fatal("accepted request lost prepaid native terminal", r.Event.Kind, err)
		}
	}
	if f.native.PhysicalTerminalDebt() != 0 || f.p.physical.remaining != len(rows) {
		t.Fatal("terminal coverage failed exact native/derived debt", f.native.PhysicalTerminalDebt(), f.p.physical.remaining)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	receipt, err := f.d.CatchUpTypedLive(ctx, f.native, f.manager)
	if err != nil || receipt.AppliedRecords != uint64(len(rows)) || f.p.physical.remaining != 0 {
		t.Fatal("accepted terminal vector did not complete derived COW", receipt, err)
	}
	if got := f.manager.Snapshot().Used; got != before {
		t.Fatal("original terminal work did not preserve its prepaid fees", before, got)
	}
	if err := resourcebudget.WithCompletionWorkspace(f.p.physical.work.Bind(ctx), func(ctx context.Context) error {
		stats, err := f.p.ReadStats(ctx)
		if err != nil {
			return err
		}
		if stats.Watermark.Sequence != 5 || stats.Totals.Requests != 1 || stats.Totals.Attempts != 1 || stats.Metrics.CostMicrosUSD != 7 || stats.Metrics.ActiveRequests != 0 {
			return fmt.Errorf("completed vector has incorrect Usage balances: %+v", stats)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.p.Close(); !errors.Is(err, authenticatedstate.ErrRecovery) {
		t.Fatal("unpublished vector was falsely retired", err)
	}
}
