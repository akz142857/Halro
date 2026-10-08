//go:build linux

package governance

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/auditlimits"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/nativephysical"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
	"golang.org/x/sys/unix"
)

// Component-only scope: the original native/COW files and physical fees are
// real. Actual Member/TLS and seven-root Runtime admission are covered by app.
func TestHASelectedPhysicalGovernanceCOWActualFullFilesystem(t *testing.T) {
	if os.Getenv("HALRO_EXT4_ROOT") == "" {
		t.Skip("isolated actual ext4 fixture required")
	}
	ctx := context.Background()
	f := selectedTypedGovFixtureWithRAMForTest(t, false, 512<<20)
	if err := f.native.Close(); err != nil {
		t.Fatal(err)
	}
	seal, found, err := f.selected.Publisher.Current()
	if err != nil || !found {
		t.Fatal(err)
	}
	seal.Generation++
	seal.Role, seal.Cluster, seal.Member = authenticatedstate.SealPrimary, [32]byte{11}, [32]byte{12}
	seal.Term, seal.OrderingTerm, seal.OrderingIndex, seal.OrderingOffset, seal.OrderingMAC = 1, 1, 9, 256, [32]byte{13}
	if _, err = f.selected.Publisher.Publish(ctx, seal); err != nil {
		t.Fatal(err)
	}
	a := f.selected.Authority
	a.Role, a.Cluster, a.Member, a.Term, a.OrderingIndex, a.OrderingOffset, a.OrderingMAC = seal.Role, seal.Cluster, seal.Member, seal.Term, seal.OrderingIndex, seal.OrderingOffset, seal.OrderingMAC
	f.selected.Authority = a
	head := f.cache.Head()
	options := Options{CacheBudget: f.options.Budget, CacheRAMBytes: auditlimits.MinCacheRAMBytes, MaxDiskBytes: 64 << 20, BeforeDurable: func(DurableBatch) (AppendAdmission, error) { return nil, nil }, AfterDurable: func(DurableBatch) error { return nil }}
	n, _, err := OpenWithHASelectedSeal(ctx, f.path, f.key, options, HASelectedSealOpenOptions{SelectedSealOpenOptions: SelectedSealOpenOptions{Publisher: f.selected.Publisher, Guard: f.selected.Guard, Cache: f.cache, Identity: f.options.Identity}, WithAuthority: func(ctx context.Context, visit func(context.Context, authenticatedstate.SealEligibility, HASelectedNativePosition) error) error {
		return visit(ctx, a, HASelectedNativePosition{Sequence: head.Records, Hash: head.LastHash})
	}})
	if err != nil {
		t.Fatal(err)
	}
	f.native, f.options.Native = n, n
	job, err := n.StartSelectedStartupAudit(ctx, f.options.Budget, StartupAuditOptions{Deadline: 2 * time.Second, ProgressTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err = job.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	nativeFee, err := f.options.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer nativeFee.Release()
	controls, err := f.options.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: nativephysical.ControlDiskBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer controls.Release()
	if err = n.PrepareHAPhysicalNative(ctx, f.options.Budget, HAPhysicalOptions{NativeDisk: nativeFee, ControlDisk: controls, WindowBytes: 16 << 20}); err != nil {
		t.Fatal(err)
	}
	p, _, err := OpenHASelectedTypedGovernanceProjection(ctx, f.options, f.selected)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
		if err := f.creator.AdoptClosedTypedSuccessor(p); err != nil {
			t.Error(err)
		}
	}()
	if err = p.EnableHAPhysicalProjection(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if child := os.Getenv("HALRO_GOV_PHYSICAL_CHILD_PHASE"); child == "" {
		original, work := p.physical, p.physical.work
		if err = p.EnableHAPhysicalProjection(ctx, 2); !errors.Is(err, ErrStartupCache) {
			t.Fatal("duplicate installer replaced original owner", err)
		}
		if err = p.PrepareHAPhysicalProjection(ctx, 2); err != nil {
			t.Fatal("actual same-cut original owner preparation", err)
		}
		if p.physical != original || p.physical.work != work || n.physicalProjection != original || original.remaining != 2 {
			t.Fatal("same-cut preparation replaced original owner or fee")
		}
	}
	if child := os.Getenv("HALRO_GOV_PHYSICAL_CHILD_PHASE"); child != "" {
		cacheFile, err := os.OpenFile(filepath.Join(filepath.Dir(f.path), "independent.cache"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if err = f.cache.WriteTo(ctx, cacheFile); err != nil {
			t.Fatal(err)
		}
		if err = cacheFile.Sync(); err != nil {
			t.Fatal(err)
		}
		if err = cacheFile.Close(); err != nil {
			t.Fatal(err)
		}
		artifact := governancePhysicalChildArtifact{Selected: f.selected.Selected, Authority: a, Saved: head}
		r, err := n.Append(ctx, projectionEvent(7))
		if err != nil {
			t.Fatal(err)
		}
		artifact.Full = n.Summary()
		if child == "cow-durable" {
			head = artifact.Full
			if err = p.Apply(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		raw, err := json.Marshal(artifact)
		if err != nil {
			t.Fatal(err)
		}
		control, err := os.OpenFile(filepath.Join(filepath.Dir(f.path), "independent.fixture.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = control.Write(raw); err != nil {
			t.Fatal(err)
		}
		if err = control.Sync(); err != nil {
			t.Fatal(err)
		}
		if err = control.Close(); err != nil {
			t.Fatal(err)
		}
		parent, err := os.Open(filepath.Dir(f.path))
		if err != nil {
			t.Fatal(err)
		}
		if err = parent.Sync(); err != nil {
			t.Fatal(err)
		}
		if err = parent.Close(); err != nil {
			t.Fatal(err)
		}
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		panic("SIGKILL returned")
	}
	before := f.options.Budget.Snapshot()
	lastBlock, err := os.OpenFile(filepath.Join(filepath.Dir(f.path), "foreign-last-block"), os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_SYNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lastBlock.Close()
	filler, err := os.OpenFile(filepath.Join(filepath.Dir(f.path), "foreign-full-cow"), os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_SYNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 1<<20)
	size := len(chunk)
	retries := 0
	for {
		_, err = filler.Write(chunk[:size])
		if err == nil {
			continue
		}
		if !errors.Is(err, syscall.ENOSPC) {
			t.Fatal(err)
		}
		if size > 4096 {
			size = 4096
			continue
		}
		if err = unix.Syncfs(int(filler.Fd())); err != nil {
			t.Fatal(err)
		}
		var fs unix.Statfs_t
		if err = unix.Statfs(f.path, &fs); err != nil {
			t.Fatal(err)
		}
		if fs.Bavail > 0 && retries < 32 {
			// A new extent in the large filler can require another metadata
			// block. The already-created second inode consumes a genuine last
			// data block without weakening the zero-Bavail acceptance condition.
			if _, err := lastBlock.Write(chunk[:4096]); err != nil && !errors.Is(err, syscall.ENOSPC) {
				t.Fatal(err)
			}
			retries++
			continue
		}
		if fs.Bavail != 0 {
			t.Fatal("not actual full filesystem", fs.Bavail)
		}
		break
	}
	if err = filler.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 7; i <= 8; i++ {
		r, err := n.Append(ctx, projectionEvent(i))
		if err != nil {
			t.Fatal("accepted native", err)
		}
		head = n.Summary()
		if err = p.Apply(ctx, r); err != nil {
			t.Fatal("prepaid COW", err)
		}
	}
	if _, err = n.Append(ctx, projectionEvent(9)); !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatal("exhausted COW wrote native", err)
	}
	if n.Summary().Records != 8 {
		t.Fatal("refused native changed head")
	}
	if after := f.options.Budget.Snapshot(); after.Used != before.Used || after.NormalClaims != before.NormalClaims || after.RecoveryClaims != before.RecoveryClaims {
		t.Fatal("new fee under full filesystem", before, after)
	}
	// Free only this fixture's foreign filler, then actually publish the new
	// common root before any original prepared tail is retired.
	if err = os.Remove(filler.Name()); err != nil {
		t.Fatal(err)
	}
	if err = lastBlock.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(lastBlock.Name()); err != nil {
		t.Fatal(err)
	}
	var ref TypedGovernanceReference
	err = p.withPhysicalProjectionWork(ctx, func(ctx context.Context) error { var err error; ref, err = p.TypedSealReference(ctx); return err })
	if err != nil {
		t.Fatal(err)
	}
	seal.Generation++
	seal.Positions[2] = ref.Journal.Position
	seal.Roots[0], seal.Roots[1] = ref.Journal.Root, ref.Projection.Root
	if _, err = f.selected.Publisher.Publish(ctx, seal); err != nil {
		t.Fatal(err)
	}
	copy(a.Positions[:], seal.Positions[:])
	err = p.withPhysicalProjectionWork(ctx, func(ctx context.Context) error { return p.RetireHAPhysicalProjectionAfterCommon(ctx) })
	if err != nil {
		t.Fatal(err)
	}
	if err = p.ReplenishHAPhysicalProjection(ctx, 2); err != nil {
		t.Fatal("actual postcommon renewal", err)
	}
	r, err := n.Append(ctx, projectionEvent(9))
	if err != nil {
		t.Fatal(err)
	}
	head = n.Summary()
	if err = p.Apply(ctx, r); err != nil {
		t.Fatal(err)
	}
	err = p.withPhysicalProjectionWork(ctx, func(ctx context.Context) error { var err error; ref, err = p.TypedSealReference(ctx); return err })
	if err != nil {
		t.Fatal(err)
	}
	seal.Generation++
	seal.Positions[2] = ref.Journal.Position
	seal.Roots[0], seal.Roots[1] = ref.Journal.Root, ref.Projection.Root
	if _, err = f.selected.Publisher.Publish(ctx, seal); err != nil {
		t.Fatal(err)
	}
	copy(a.Positions[:], seal.Positions[:])
	err = p.withPhysicalProjectionWork(ctx, func(ctx context.Context) error { return p.RetireHAPhysicalProjectionAfterCommon(ctx) })
	if err != nil {
		t.Fatal(err)
	}
}

type governancePhysicalChildArtifact struct {
	Selected    TypedGovernanceReference
	Authority   authenticatedstate.SealEligibility
	Saved, Full Summary
}

// The independent synthetic fixture cut is recorded before killing the real
// writer. A fresh process reconstructs original native births, MAC chains and
// COW targets; fixture fields are explicitly not actual Member confirmation.
func TestHASelectedPhysicalGovernanceRealSIGKILLRecovery(t *testing.T) {
	if os.Getenv("HALRO_EXT4_ROOT") == "" {
		t.Skip("isolated actual ext4 fixture required")
	}
	for _, phase := range []string{"native-durable", "cow-durable"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			child := exec.Command(os.Args[0], "-test.run", "^TestHASelectedPhysicalGovernanceCOWActualFullFilesystem$", "-test.count=1", "-test.timeout=30s")
			child.Env = append(os.Environ(), "HALRO_GOV_PHYSICAL_CHILD_DIR="+dir, "HALRO_GOV_PHYSICAL_CHILD_PHASE="+phase)
			output, err := child.CombinedOutput()
			exit, ok := err.(*exec.ExitError)
			if !ok || !exit.ProcessState.Sys().(syscall.WaitStatus).Signaled() || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("actual SIGKILL absent: %v %s", err, output)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "independent.fixture.json"))
			if err != nil {
				t.Fatal(err)
			}
			var artifact governancePhysicalChildArtifact
			if err = json.Unmarshal(raw, &artifact); err != nil {
				t.Fatal(err)
			}
			limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 512 << 20, Disk: 128 << 20}, RecoveryReserve: resourcebudget.Cost{RAM: 8 << 20}, RecoveryClaims: 4, MaxClaims: 64, MaxOwners: 16, MaxPressureAge: time.Hour}
			for i := range limits.Classes {
				limits.Classes[i] = limits.Total
			}
			limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 64 * resourcebudget.ClaimRAMBytes}
			limits.Classes[resourcebudget.Recovery] = resourcebudget.Cost{RAM: (8 << 20) - 4*resourcebudget.ClaimRAMBytes}
			budget, err := resourcebudget.New(limits)
			if err != nil {
				t.Fatal(err)
			}
			if err = budget.SetPressure(resourcebudget.Healthy); err != nil {
				t.Fatal(err)
			}
			key := make([]byte, 32)
			for i := range key {
				key[i] = 0x61
			}
			guard, err := lock.Acquire(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			acquire := func(size int64) resourcebudget.Permit {
				v, err := budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: size})
				if err != nil {
					t.Fatal(err)
				}
				return v
			}
			pubFee := acquire(32 << 10)
			defer pubFee.Release()
			hard := authenticatedstate.SealHardLimits{MaxPageBytes: 4 << 20, MaxValueBytes: 3 << 20, MaxTreeHeight: 32, RequiredRoots: []authenticatedstate.SealRootSchema{artifact.Selected.Journal.Schema, typedGovernanceSchema()}}
			pub, err := authenticatedstate.OpenSealCandidatePublisher(dir, guard, key, hard, budget, pubFee)
			if err != nil {
				t.Fatal(err)
			}
			defer pub.Close()
			cacheFD, err := os.Open(filepath.Join(dir, "independent.cache"))
			if err != nil {
				t.Fatal(err)
			}
			info, err := cacheFD.Stat()
			if err != nil {
				t.Fatal(err)
			}
			cache, err := ReadStartupCache(ctx, cacheFD, info.Size(), key, [32]byte{9}, budget)
			if err != nil {
				t.Fatal(err)
			}
			if err = cacheFD.Close(); err != nil {
				t.Fatal(err)
			}
			defer cache.Close()
			options := Options{CacheBudget: budget, CacheRAMBytes: auditlimits.MinCacheRAMBytes, MaxDiskBytes: 64 << 20, BeforeDurable: func(DurableBatch) (AppendAdmission, error) { return nil, nil }, AfterDurable: func(DurableBatch) error { return nil }}
			selected := SelectedSealOpenOptions{Publisher: pub, Guard: guard, Cache: cache, Identity: [32]byte{9}}
			n, _, err := OpenWithHASelectedSeal(ctx, filepath.Join(dir, "governance.wal"), key, options, HASelectedSealOpenOptions{SelectedSealOpenOptions: selected, WithAuthority: func(ctx context.Context, visit func(context.Context, authenticatedstate.SealEligibility, HASelectedNativePosition) error) error {
				return visit(ctx, artifact.Authority, HASelectedNativePosition{Sequence: artifact.Full.Records, Hash: artifact.Full.LastHash})
			}})
			if err != nil {
				t.Fatal(err)
			}
			defer n.Close()
			job, err := n.StartSelectedStartupAudit(ctx, budget, StartupAuditOptions{Deadline: 2 * time.Second, ProgressTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			if err = job.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			nativeFee, controlFee := acquire(64<<20), acquire(nativephysical.ControlDiskBytes)
			defer nativeFee.Release()
			defer controlFee.Release()
			if err = n.PrepareHAPhysicalNative(ctx, budget, HAPhysicalOptions{NativeDisk: nativeFee, ControlDisk: controlFee, WindowBytes: 16 << 20, Restart: true}); err != nil {
				t.Fatal(err)
			}
			rootFee := acquire(16 << 20)
			defer rootFee.Release()
			p, reopened, err := OpenHASelectedTypedGovernanceProjection(ctx, AuthenticatedGovernanceOptions{Native: n, Identity: [32]byte{9}, RootKey: key, RootDirectory: filepath.Join(dir, "typed-root"), RootMaxDiskBytes: 16 << 20, RootMaxAppendBytes: 4 << 20, Budget: budget}, SelectedTypedGovernanceOpenOptions{Publisher: pub, Guard: guard, Selected: artifact.Selected, DiskReservation: rootFee})
			if err != nil {
				t.Fatal(err)
			}
			if reopened.To != artifact.Full || reopened.AppliedRecords != map[string]uint64{"native-durable": 1, "cow-durable": 0}[phase] {
				t.Fatal("unexpected recovered reducer", reopened)
			}
			// Only an actual newly published fixture common root may close normally.
			seal, found, err := pub.Current()
			if err != nil || !found {
				t.Fatal(err)
			}
			var ref TypedGovernanceReference
			err = p.withPhysicalProjectionWork(ctx, func(ctx context.Context) error {
				return n.startupProvenance.ha.withScope(ctx, func(ctx context.Context, _ authenticatedstate.SealEligibility, _ HASelectedNativePosition) error {
					var err error
					ref, err = p.TypedSealReference(ctx)
					return err
				})
			})
			if err != nil {
				t.Fatal(err)
			}
			seal.Generation++
			seal.Positions[2] = ref.Journal.Position
			seal.Roots[0], seal.Roots[1] = ref.Journal.Root, ref.Projection.Root
			if _, err = pub.Publish(ctx, seal); err != nil {
				t.Fatal(err)
			}
			copy(artifact.Authority.Positions[:], seal.Positions[:])
			err = p.withPhysicalProjectionWork(ctx, func(ctx context.Context) error { return p.RetireHAPhysicalProjectionAfterCommon(ctx) })
			if err != nil {
				t.Fatal(err)
			}
			original, work := p.physical, p.physical.work
			if err = p.PrepareHAPhysicalProjection(ctx, 2); err != nil {
				t.Fatal("fresh original recovered owner preparation", err)
			}
			if p.physical != original || p.physical.work != work || n.physicalProjection != original {
				t.Fatal("recovered preparation replaced actual owner or fee")
			}
			err = p.withPhysicalProjectionWork(ctx, func(ctx context.Context) error { return p.RetireHAPhysicalProjectionAfterCommon(ctx) })
			if err != nil {
				t.Fatal(err)
			}
			if err = p.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
