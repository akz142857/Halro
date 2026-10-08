package governance

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/auditlimits"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

type selectedTypedGovFixture struct {
	native   *Log
	creator  *GovernanceProjection
	options  AuthenticatedGovernanceOptions
	selected SelectedTypedGovernanceOpenOptions
	cache    *StartupCache
	key      []byte
	path     string
	debt     bool
}

func selectedTypedGovFixtureForTest(t *testing.T, delta bool) *selectedTypedGovFixture {
	return selectedTypedGovFixtureWithRAMForTest(t, delta, 256<<20)
}

func selectedTypedGovFixtureWithRAMForTest(t *testing.T, delta bool, ram int64) *selectedTypedGovFixture {
	t.Helper()
	ctx := context.Background()
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: ram, Disk: 128 << 20}, RecoveryReserve: resourcebudget.Cost{RAM: 8 << 20}, RecoveryClaims: 4, MaxClaims: 64, MaxOwners: 16, MaxPressureAge: time.Hour}
	for i := range limits.Classes {
		limits.Classes[i] = limits.Total
	}
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 64 * resourcebudget.ClaimRAMBytes}
	limits.Classes[resourcebudget.Recovery] = resourcebudget.Cost{RAM: (8 << 20) - 4*resourcebudget.ClaimRAMBytes}
	budget, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := budget.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if fixed := os.Getenv("HALRO_GOV_PHYSICAL_CHILD_DIR"); fixed != "" {
		dir = fixed
	}
	path := filepath.Join(dir, "governance.wal")
	key := bytes.Repeat([]byte{0x61}, 32)
	nativeOptions := Options{CacheBudget: budget, CacheRAMBytes: auditlimits.MinCacheRAMBytes}
	native, err := OpenWithOptions(path, key, nativeOptions)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 6; i++ {
		if _, err := native.Append(ctx, projectionEvent(i)); err != nil {
			t.Fatal(err)
		}
	}
	identity := [32]byte{9}
	o := AuthenticatedGovernanceOptions{Native: native, Identity: identity, RootKey: key, RootDirectory: filepath.Join(dir, "typed-root"), RootMaxDiskBytes: 16 << 20, RootMaxAppendBytes: 4 << 20, Budget: budget}
	p, ref, err := BuildTypedGovernanceProjection(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := native.CaptureStartupCache(ctx, budget, identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	disk, err := p.TypedDiskReservation()
	if err != nil {
		t.Fatal(err)
	}
	guard, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	seal := authenticatedstate.CandidateSeal{SchemaVersion: 1, ApplyVersion: 1, IndexVersion: 1, KeyDerivation: 1, Instance: [32]byte{1}, Incarnation: [16]byte{1}, Generation: 1, Revision: 1, Role: authenticatedstate.SealStandalone, Coverage: authenticatedstate.SealCoverageAuthenticated, MaxPageBytes: 4 << 20, MaxValueBytes: 3 << 20, MaxTreeHeight: 32, PositionCount: 5, RootCount: 2}
	seal.Positions[0] = authenticatedstate.SealPosition{Component: authenticatedstate.SealComponentLedger, Generation: 1}
	seal.Positions[1] = authenticatedstate.SealPosition{Component: authenticatedstate.SealComponentMetadata, Epoch: 1, Revision: 1, MetadataBaseline: [32]byte{2}}
	seal.Positions[2] = ref.Journal.Position
	seal.Positions[3].Component = authenticatedstate.SealComponentAudit
	seal.Positions[4].Component = authenticatedstate.SealComponentUsage
	seal.Roots[0], seal.Roots[1] = ref.Journal.Root, ref.Projection.Root
	hard := authenticatedstate.SealHardLimits{MaxPageBytes: seal.MaxPageBytes, MaxValueBytes: seal.MaxValueBytes, MaxTreeHeight: seal.MaxTreeHeight, RequiredRoots: []authenticatedstate.SealRootSchema{ref.Journal.Schema, ref.Projection.Schema}}
	fee, err := budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 32 << 10})
	if err != nil {
		t.Fatal(err)
	}
	pub, err := authenticatedstate.OpenSealCandidatePublisher(dir, guard, key, hard, budget, fee)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pub.Publish(ctx, seal); err != nil {
		t.Fatal(err)
	}
	authority := authenticatedstate.SealEligibility{SchemaVersion: 1, ApplyVersion: 1, IndexVersion: 1, KeyDerivation: 1, Instance: seal.Instance, Incarnation: seal.Incarnation, Role: seal.Role, Coverage: seal.Coverage}
	copy(authority.Positions[:], seal.Positions[:])
	if delta {
		if _, err := native.Append(ctx, projectionEvent(7)); err != nil {
			t.Fatal(err)
		}
	}
	if err := native.Close(); err != nil {
		t.Fatal(err)
	}
	native, _, err = OpenWithSelectedSeal(ctx, path, key, nativeOptions, SelectedSealOpenOptions{Publisher: pub, Guard: guard, Authority: authority, Cache: cache, Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
	o.Native = native
	f := &selectedTypedGovFixture{native: native, creator: p, options: o, selected: SelectedTypedGovernanceOpenOptions{Publisher: pub, Guard: guard, Authority: authority, Selected: ref, DiskReservation: disk, Permitted: native.Summary()}, cache: cache, key: key, path: path}
	t.Cleanup(func() {
		if err := f.native.Close(); err != nil && !(f.debt && errors.Is(err, ownedfile.ErrRetirement)) {
			t.Error(err)
		}
		if err := p.Discard(); err != nil {
			t.Error(err)
		}
		if err := disk.Release(); err != nil {
			t.Error(err)
		}
		if err := cache.Close(); err != nil {
			t.Error(err)
		}
		if err := pub.Close(); err != nil {
			t.Error(err)
		}
		if err := pub.Retire(); err != nil {
			t.Error(err)
		}
		if err := fee.Release(); err != nil {
			t.Error(err)
		}
		if err := guard.Close(); err != nil {
			t.Error(err)
		}
		used := budget.Snapshot().Used
		if !f.debt && used != (resourcebudget.Cost{}) || f.debt && used.RAM < governanceSealWorkspaceRAMBytes {
			t.Error("selected typed lifetime", used)
		}
	})
	return f
}

func TestSelectedTypedGovernanceActualRootsAndOnlyDelta(t *testing.T) {
	f := selectedTypedGovFixtureForTest(t, true)
	p, r, err := OpenSelectedTypedGovernanceProjection(context.Background(), f.options, f.selected)
	if err != nil {
		t.Fatal(err)
	}
	if r.SourceVerified != 1 || r.AppliedRecords != 1 || r.From.Records != 6 || r.To.Records != 7 {
		t.Fatal(r)
	}
	if _, err := p.ReadHead(context.Background()); !errors.Is(err, ErrStartupAuditPending) {
		t.Fatal("pending query granted", err)
	}
	entered, leave := make(chan struct{}), make(chan struct{})
	a, err := f.native.startSelectedStartupAudit(context.Background(), f.options.Budget, StartupAuditOptions{Deadline: time.Second, ProgressTimeout: time.Second}, func(seq uint64, _ *os.File) {
		if seq == 1 {
			close(entered)
			<-leave
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	if head, err := p.ReadHead(context.Background()); err != nil || head.Records != 7 {
		t.Fatal(head, err)
	}
	if _, err := p.TypedSealReference(context.Background()); !errors.Is(err, ErrStartupAuditPending) {
		t.Fatal("partial audit published", err)
	}
	close(leave)
	if err := a.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	actual, err := p.TypedSealReference(context.Background())
	if err != nil || actual != r.Reference {
		t.Fatal(actual, r.Reference, err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.creator.AdoptClosedTypedSuccessor(p); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedTypedGovernanceNoDeltaSkipsOldDamageAndGrantWithdraws(t *testing.T) {
	f := selectedTypedGovFixtureForTest(t, false)
	file, err := os.OpenFile(f.path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := file.ReadAt(b[:], frameHeaderSize); err != nil {
		t.Fatal(err)
	}
	b[0] ^= 1
	if _, err := file.WriteAt(b[:], frameHeaderSize); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	p, r, err := OpenSelectedTypedGovernanceProjection(context.Background(), f.options, f.selected)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if r.SourceVerified != 0 || r.AppliedRecords != 0 {
		t.Fatal(r)
	}
	a, err := f.native.StartSelectedStartupAudit(context.Background(), f.options.Budget, StartupAuditOptions{Deadline: time.Second, ProgressTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Wait(context.Background()); err == nil {
		t.Fatal("damaged history accepted")
	}
	if _, err := p.ReadHead(context.Background()); err == nil {
		t.Fatal("damaged native grant returned query")
	}
}

func TestSelectedTypedGovernanceRejectsCallerRootAndAuthority(t *testing.T) {
	f := selectedTypedGovFixtureForTest(t, false)
	s := f.selected
	s.Selected.Projection.Root.Digest[0] ^= 1
	if p, _, err := OpenSelectedTypedGovernanceProjection(context.Background(), f.options, s); p != nil || !errors.Is(err, ErrStartupCache) {
		t.Fatal(p, err)
	}
	s = f.selected
	s.Authority.Positions[2].Sequence++
	if p, _, err := OpenSelectedTypedGovernanceProjection(context.Background(), f.options, s); p != nil || err == nil {
		t.Fatal(p, err)
	}
}

func TestSelectedTypedGovernanceActualSourceCloseKeepsWholeFee(t *testing.T) {
	f := selectedTypedGovFixtureForTest(t, false)
	f.native.mu.Lock()
	f.native.closeReaderFile = func(file *os.File) error {
		if err := file.Close(); err != nil {
			return err
		}
		return file.Close()
	}
	f.native.mu.Unlock()
	p, r, err := OpenSelectedTypedGovernanceProjection(context.Background(), f.options, f.selected)
	if p == nil || !errors.Is(err, ownedfile.ErrRetirement) || r != (TypedGovernanceReopen{}) {
		t.Fatal(p, r, err)
	}
	var debt *selectedGovernanceSourceDebt
	if !errors.As(err, &debt) || debt.source == nil || len(debt.key) != 32 {
		t.Fatal("actual reader/key/job owner lost", err)
	}
	f.debt = true
}
