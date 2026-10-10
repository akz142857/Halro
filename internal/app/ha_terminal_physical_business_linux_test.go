//go:build linux

package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
	"github.com/akz142857/Halro/internal/store/lock"
	"golang.org/x/sys/unix"
)

func TestHASelectedPhysicalBusinessOriginalWindowActualTLS(t *testing.T) {
	exerciseHASelectedPhysicalBusinessActualTLS(t, false)
}

func TestHASelectedPhysicalBusinessOriginalWindowAtENOSPCActualTLS(t *testing.T) {
	exerciseHASelectedPhysicalBusinessActualTLS(t, true)
}

func TestHASelectedPhysicalReplicaSameGenerationDoesNotConsumeCheckpointWindows(t *testing.T) {
	exerciseHASelectedPhysicalBusinessActualTLSMode(t, false, true)
}

func exerciseHASelectedPhysicalBusinessActualTLS(t *testing.T, pressure bool) {
	exerciseHASelectedPhysicalBusinessActualTLSMode(t, pressure, false)
}

func exerciseHASelectedPhysicalBusinessActualTLSMode(t *testing.T, pressure, repeat bool) {
	root := os.Getenv("HALRO_EXT4_ROOT")
	if root == "" {
		t.Skip("requires explicitly isolated ext4 runner")
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(root, &fs); err != nil || fs.Type != unix.EXT4_SUPER_MAGIC {
		t.Fatal("actual ext4 fixture", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	owners := startHAPhysicalBusinessPair(t, ctx, nil)
	primary := owners[0]
	admitted, retire, err := primary.physicalTerminalAdmission(ctx, 2)
	if err != nil {
		t.Fatal("original confirmed/Metadata/Ledger physical admission", err)
	}
	c := primary.reopened.Owners.Components
	profile := ledger.HASelectedCompletionWorkspaceRequirements()
	var completionRAM int64
	for _, slot := range profile {
		completionRAM += slot.RAM
	}
	work, err := c.Budget.TryAcquireCompletableWork(resourcebudget.Active, resourcebudget.Cost{RAM: 4096}, resourcebudget.Cost{RAM: completionRAM}, profile)
	if err != nil {
		t.Fatal("actual pre-admitted request completion RAM", err)
	}
	request, err := primary.business.accounting.BeginRequestDetailed(work.Bind(admitted), "prj_physical", "key_physical", "actual-physical-ha-request", "model")
	if err != nil {
		t.Fatal("actual ordinary RequestAccepted", err, c.Budget.Snapshot(), primary.replication.publisher.Snapshot())
	}
	if pressure {
		accepted := primary.replication.publisher.Snapshot().DurableIndex
		if err := primary.replication.coordinator.WaitConfirmed(ctx, accepted); err != nil {
			t.Fatal("actual accepted prefix confirmation before foreign fill", err)
		}
		for owners[1].replication.publisher.Snapshot().AppliedIndex < accepted {
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(10 * time.Millisecond):
			}
		}
		if err := exhaustHAPhysicalFixture(filepath.Join(root, "foreign-ha-business-fill"), 1536<<20); err != nil {
			t.Fatal(err)
		}
		for _, f := range owners {
			if err := f.reopened.Owners.Components.Budget.SetPressure(resourcebudget.Pressured); err != nil {
				t.Fatal(err)
			}
			if _, err := f.reopened.Owners.Components.Budget.TryAcquire(resourcebudget.IO, resourcebudget.Cost{RAM: 1}); !errors.Is(err, resourcebudget.ErrPressure) {
				t.Fatal("ordinary new IO was not refused at actual ENOSPC", err)
			}
		}
	}
	err = work.WithWorkspace(admitted, func(ctx context.Context) error {
		if err := primary.business.accounting.Finalize(ctx, request, "success"); err != nil {
			return fmt.Errorf("actual RequestFinalized: %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal("actual original physical business facts", err)
	}
	if err := work.Close(); err != nil {
		t.Fatal("actual request RAM retirement", err)
	}
	if err := work.Active.Release(); err != nil {
		t.Fatal("actual request active RAM retirement", err)
	}
	// Native accounting completion is distinct from quorum confirmation. Wait
	// for the actual final durable Ordering frame before withdrawing transport.
	through := primary.replication.publisher.Snapshot().DurableIndex
	if err := primary.replication.coordinator.WaitConfirmed(ctx, through); err != nil {
		t.Fatal("actual final durable frame confirmation", err)
	}
	for owners[1].replication.publisher.Snapshot().AppliedIndex < through {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := retire(); err != nil {
		t.Fatal("actual original completion owners", err)
	}
	if repeat {
		second, closeSecond, err := primary.physicalTerminalAdmission(ctx, 2)
		if err != nil {
			t.Fatal("second original same-generation window", err)
		}
		secondWork, err := c.Budget.TryAcquireCompletableWork(resourcebudget.Active, resourcebudget.Cost{RAM: 4096}, resourcebudget.Cost{RAM: completionRAM}, profile)
		if err != nil {
			t.Fatal(err)
		}
		request, err := primary.business.accounting.BeginRequestDetailed(secondWork.Bind(second), "prj_physical", "key_physical", "actual-physical-ha-second-request", "model")
		if err != nil {
			t.Fatal("third actual same-generation fact", err)
		}
		if err := secondWork.WithWorkspace(second, func(ctx context.Context) error { return primary.business.accounting.Finalize(ctx, request, "success") }); err != nil {
			t.Fatal("fourth actual same-generation fact", err)
		}
		if err := secondWork.Close(); err != nil {
			t.Fatal(err)
		}
		if err := secondWork.Active.Release(); err != nil {
			t.Fatal(err)
		}
		if err := closeSecond(); err != nil {
			t.Fatal(err)
		}
		through := primary.replication.publisher.Snapshot().DurableIndex
		if err := primary.replication.coordinator.WaitConfirmed(ctx, through); err != nil {
			t.Fatal(err)
		}
		for owners[1].replication.publisher.Snapshot().AppliedIndex < through {
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(10 * time.Millisecond):
			}
		}
		replica := owners[1].reopened.Owners.Components
		if _, found, err := replica.Ledger.ReadFinalization(ctx, request.RequestID); err != nil || !found {
			t.Fatal("four original facts actually Replica applied", found, err)
		}
		cancelled, stop := context.WithCancel(ctx)
		stop()
		if _, err := replica.Directory.AdvanceHASelectedReplicaDirectoryFromNative(cancelled, replica.NativeLedger, replica.Ledger); !errors.Is(err, context.Canceled) {
			t.Fatal("cancelled directory observation accepted", err)
		}
		if _, err := replica.Directory.AdvanceHASelectedReplicaDirectoryFromNative(ctx, c.NativeLedger, c.Ledger); err == nil {
			t.Fatal("foreign Primary source accepted by Replica directory")
		}
	}
	for i, f := range owners {
		finishCtx := ctx
		if pressure {
			// Retirement may return genuine owned blocks. Fill those again
			// before each actual publication so another member cannot borrow
			// the first member's reclaimed capacity as a hidden prerequisite.
			if err := exhaustHAPhysicalFixture(filepath.Join(root, fmt.Sprintf("foreign-ha-common-fill-%d", i)), 1536<<20); err != nil {
				t.Fatal(err)
			}
			finishCtx = context.WithValue(ctx, haPhysicalFinishObserverKey{}, func(stage string) error {
				if stage == "common-ready" {
					// Actual collector COW may retire its previous owned root.
					// Let the independent foreign consumer take those blocks
					// too before the common publisher begins its source loan.
					if err := exhaustHAPhysicalFixture(filepath.Join(root, fmt.Sprintf("foreign-ha-publication-fill-%d", i)), 1536<<20); err != nil {
						return err
					}
				}
				if err := unix.Statfs(root, &fs); err != nil || stage == "common-ready" && fs.Bavail != 0 {
					return fmt.Errorf("whole common began with freed filesystem capacity: stage=%s bavail=%d %v", stage, fs.Bavail, err)
				}
				t.Log("actual independently owned common publication filesystem", i, stage, fs.Bavail)
				return nil
			})
		}
		if err := f.finishHAPhysicalBusiness(finishCtx); err != nil {
			t.Fatal("actual confirmed terminal whole common", i, err)
		}
		if err := f.reopened.Owners.Components.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
			return statecut.ValidateHASelectedMaintenance(ctx, f.reopened.Owners)
		}); err != nil {
			t.Fatal("actual all-seven current cut", i, err)
		}
		if err := f.closeHAPhysicalNative(ctx); err != nil {
			t.Fatal("actual complete physical native close", i, err)
		}
		if err := f.Close(); err != nil {
			t.Fatal("actual physical business whole close", i, err)
		}
		if pressure {
			if err := unix.Statfs(root, &fs); err != nil {
				t.Fatal("actual post-retirement filesystem measurement", i, err)
			}
			t.Log("actual post-common owned retirement returned blocks", i, fs.Bavail)
		}
	}
}

// startHAPhysicalBusinessPair activates a Primary and a Replica on the
// isolated ext4 with the physical terminal reserve and serves their actual
// TLS transport until the Primary is startup ready. Cleanup stops transport
// before closing the foundations. adjust, when set, edits each member's
// budget after the fixed physical profile is applied.
func startHAPhysicalBusinessPair(t *testing.T, ctx context.Context, adjust func(member int, limits *config.ResourceBudget)) [2]*haSelectedRuntimeFoundation {
	t.Helper()
	fixture := activateHAPhysicalBusinessFixture(t, ctx, false)
	return [2]*haSelectedRuntimeFoundation(openHAPhysicalBusinessGroup(t, ctx, fixture.configs[:], fixture.keys.Master, false, nil, adjust).owners)
}

// haPhysicalBusinessGroup is an open Primary (member 0) and its Replicas.
// release stops the transport, then closes each foundation and its directory
// lock; it runs once, at the latest on cleanup, so a test can reopen the same
// directories.
type haPhysicalBusinessGroup struct {
	owners  []*haSelectedRuntimeFoundation
	release func()
}

// activateHAPhysicalBusinessFixture prepares the two members' directories
// and completes their memory-state activation, leaving both closed. A strict
// pair's Metadata journal ceiling is bound by the independent baseline, so it
// is set here, before publication.
func activateHAPhysicalBusinessFixture(t *testing.T, ctx context.Context, strict bool) haMetadataMaintenanceFixture {
	t.Helper()
	fixture := prepareHAMetadataMaintenanceFixture(t)
	if strict {
		for i, cfg := range fixture.configs {
			limits := *cfg.ResourceBudget
			limits.MetadataJournalDiskBytes = 576 << 20
			cfg.ResourceBudget = &limits
			fixture.configs[i] = cfg
		}
	}
	prepareHAMetadataMaintenanceArtifacts(t, fixture)
	completeHAMemoryActivationBoundary(t, fixture)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	for _, cfg := range fixture.configs {
		guard, err := lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		activation, err := ActivateHAMemoryRuntime(ctx, cfg, fixture.keys, guard, functionalHAMetadataManager(t, cfg), logger)
		if err != nil {
			t.Fatal(err)
		}
		if err := activation.Close(); err != nil {
			t.Fatal(err)
		}
		if err := guard.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return fixture
}

// openHAPhysicalBusinessGroup opens activated members (member 0 the Primary)
// with the fixed physical profile, each from a fresh Manager, and serves their
// TLS transport until the Primary is startup ready. strict serves the
// transport the Runtime uses with the physical reserve, with the peer credit
// controller that admission and online checkpoints go through, and waits for
// every Replica's first credit window; it carries the lifecycle fixture's
// Metadata and inventory ceilings. With sampled set, each member's Manager is
// owned by a real sampler on its data directory, wired as the Runtime wires
// it, and pressure follows the OS rather than the test; the samplers stop
// after the members close.
func openHAPhysicalBusinessGroup(t *testing.T, ctx context.Context, configs []config.Config, master []byte, strict bool, sampled *config.ResourcePressure, adjust func(member int, limits *config.ResourceBudget)) haPhysicalBusinessGroup {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	owners := make([]*haSelectedRuntimeFoundation, len(configs))
	guards := make([]*lock.Lock, len(configs))
	var stop context.CancelFunc
	var done chan error
	var once sync.Once
	var samplers []func()
	release := func() {
		once.Do(func() {
			defer func() {
				for _, stopSampler := range samplers {
					stopSampler()
				}
			}()
			if stop != nil {
				stop()
				for range owners {
					if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
						t.Error(err)
					}
				}
			}
			for i := len(owners) - 1; i >= 0; i-- {
				if owners[i] == nil {
					continue
				}
				if err := owners[i].Close(); err != nil {
					t.Error("original foundation cleanup", i, err)
				}
				if err := guards[i].Close(); err != nil {
					t.Error(err)
				}
			}
		})
	}
	t.Cleanup(release)
	for i, cfg := range configs {
		limits := *cfg.ResourceBudget
		limits.TotalRAMBytes = 2 << 30
		limits.RecoveryRAMBytes, limits.RecoveryReserveRAMBytes = 896<<20, 896<<20
		limits.CompletionRAMBytes = 1 << 30
		limits.TotalUnconfirmedBytes = 64 << 20
		limits.IOUnconfirmedBytes = 32 << 20
		limits.CompletionUnconfirmedBytes = 32 << 20
		limits.RecoveryUnconfirmedBytes = 16 << 20
		limits.RecoveryReserveUnconfirmedBytes = 16 << 20
		limits.UnconfirmedMaxEntries = 64
		// The actual fixed Directory catch-up pool remains resident while the
		// independently paid confirmation, Metadata and Ledger request slots live.
		// The nonphysical fixture's 96 normal claims cannot fund that overlap.
		limits.MaxClaims += 32
		limits.ControlRAMBytes += 32 * resourcebudget.ClaimRAMBytes
		limits.PhysicalTerminalReserve = true
		limits.LedgerProjectionDiskBytes = 80 << 20
		limits.LedgerSegmentIndexDiskBytes = 48 << 20
		limits.UsageProjectionDiskBytes = 128 << 20
		if i > 0 {
			// Each of the finite 32 received records owes its own worst-case
			// 16 MiB Ledger and Usage COW extent before native acceptance.
			limits.LedgerProjectionDiskBytes = 576 << 20
			limits.UsageProjectionDiskBytes = 576 << 20
		}
		if strict {
			limits.MetadataStoreDiskBytes = 576 << 20
			limits.MetadataJournalDiskBytes = 576 << 20
		}
		if adjust != nil {
			adjust(i, &limits)
		}
		if strict {
			limits.PersistentInventoryDiskBytes = limits.MinimumPersistentInventoryDiskBytes() + 512<<20
		}
		cfg.ResourceBudget = &limits
		guard, err := lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		manager := functionalHAMetadataManager(t, cfg)
		if sampled != nil {
			var stopSampler func()
			manager, stopSampler = sampledHAPhysicalManager(t, ctx, cfg, *sampled)
			samplers = append(samplers, stopSampler)
		}
		f, err := openHASelectedRuntimeFoundation(ctx, cfg, master, guard, manager, logger)
		if err != nil {
			_ = guard.Close()
			t.Fatal("actual physical business foundation", i, err)
		}
		owners[i], guards[i] = f, guard
		if err := f.prepareSelectedServicesWithLease(ctx, i == 0); err != nil {
			t.Fatal("actual whole physical service preparation", i, err, f.owners.Components.Budget.Snapshot())
		}
		if err := f.installHASelectedGovernanceService(ctx); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err := f.prepareHASelectedPrimaryStartup(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if !strict {
			if err := f.prepareSelectedTransport(logger); err != nil {
				t.Fatal("actual whole physical transport", i, err)
			}
			continue
		}
		if i > 0 {
			if err := f.reopened.Owners.Components.Metadata.PrepareHAReplicaPhysicalEscrow(ctx, resourcebudget.MaxCompletionBatch); err != nil {
				t.Fatal("Replica Metadata receive window", err)
			}
		}
		if err := f.prepareSelectedTransportWithPhysicalCredit(logger); err != nil {
			t.Fatal("actual strict physical transport", i, err)
		}
	}
	runCtx, cancelRun := context.WithCancel(ctx)
	stop, done = cancelRun, make(chan error, len(owners))
	for _, f := range owners {
		go func(f *haSelectedRuntimeFoundation) { done <- f.serveSelectedTransport(runCtx) }(f)
	}
	primary := owners[0]
	for !primary.replication.startupReady.Load() {
		select {
		case err := <-primary.replication.fatal:
			t.Fatal("actual physical Primary startup", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if strict {
		waitHAPhysicalCreditWindow(t, ctx, primary, len(owners)-1)
	}
	return haPhysicalBusinessGroup{owners, release}
}

// exhaustHAPhysicalFixture fills the isolated runner ext4 to actual ENOSPC
// with a foreign file at path. bound refuses a filesystem larger than the
// fixture expects, so a misdirected root is never filled.
func exhaustHAPhysicalFixture(path string, bound int64) error {
	var fs unix.Statfs_t
	root := os.Getenv("HALRO_EXT4_ROOT")
	if root == "" || filepath.Dir(path) != root {
		return errors.New("foreign fill requires original isolated runner root")
	}
	if err := unix.Statfs(root, &fs); err != nil || fs.Type != unix.EXT4_SUPER_MAGIC || fs.Bsize != 4096 || uint64(fs.Blocks)*uint64(fs.Bsize) > uint64(bound) {
		return errors.Join(errors.New("foreign fill filesystem exceeds fixed private bound"), err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR|syscall.O_SYNC, 0600)
	if err != nil {
		return err
	}
	// Members still running can return blocks while the file grows (a
	// Replica retires its receive window when the Primary's session ends), so
	// a probe write that finds room starts another pass. Only a write that
	// actually meets ENOSPC ends the fill.
	var offset int64
	var info os.FileInfo
	for pass := 1; ; pass++ {
		if pass > 8 {
			return errors.Join(errors.New("actual foreign write did not observe ENOSPC"), f.Close())
		}
		for step := int64(64 << 20); step >= fs.Bsize; {
			if offset+step > bound {
				return errors.Join(errors.New("foreign allocation fixed bound exceeded"), f.Close())
			}
			err := unix.Fallocate(int(f.Fd()), 0, offset, step)
			if errors.Is(err, syscall.ENOSPC) {
				step /= 2
				continue
			}
			if err != nil {
				return errors.Join(err, f.Close())
			}
			offset += step
		}
		if info, err = f.Stat(); err != nil {
			return errors.Join(err, f.Close())
		}
		_, writeErr := f.WriteAt(make([]byte, 4096), info.Size()+4096)
		if errors.Is(writeErr, syscall.ENOSPC) {
			break
		}
		if writeErr != nil {
			return errors.Join(writeErr, f.Close())
		}
		fmt.Printf("actual HA business ext4 fill pass %d found returned blocks\n", pass)
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	if err := unix.Statfs(root, &fs); err != nil || fs.Bavail != 0 {
		return fmt.Errorf("actual isolated HA ENOSPC unproved: bavail=%d %v", fs.Bavail, err)
	}
	fmt.Printf("actual HA business ext4 ENOSPC foreign_extent_bytes=%d bavail=%d\n", info.Size(), fs.Bavail)
	return nil
}

// waitHAPhysicalCreditWindow waits until each of the Primary's replicas has
// sent it a fresh physical credit window.
func waitHAPhysicalCreditWindow(t *testing.T, ctx context.Context, primary *haSelectedRuntimeFoundation, replicas int) {
	t.Helper()
	q := primary.replication.peerPhysical.Load()
	if q == nil {
		t.Fatal("strict Primary credit controller absent")
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		q.mu.Lock()
		ready := replicas <= len(q.peers)
		for p := 0; ready && p < replicas; p++ {
			ready = q.peers[p].window.Operation == replication.PhysicalCreditWindow
		}
		q.mu.Unlock()
		if ready {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("Replica credit window absent")
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// sampledHAPhysicalManager builds a member's Manager owned by a real sampler
// on its data directory, as newResourcePressureWithSource does, runs the
// monitor and waits for its first Healthy decision. stop cancels and joins
// the monitor; the group's release calls it after the members close.
func sampledHAPhysicalManager(t *testing.T, ctx context.Context, cfg config.Config, policy config.ResourcePressure) (*resourcebudget.Manager, func()) {
	t.Helper()
	if err := policy.Validate(); err != nil {
		t.Fatal("sampled pressure policy", err)
	}
	manager, err := resourcebudget.New(cfg.ResourceBudget.Limits(policy.MaxAge.Value()))
	if err != nil {
		t.Fatal(err)
	}
	sampler, err := resourcebudget.NewSampler(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	monitor, err := resourcebudget.NewMonitor(sampler, policy.Policy(), policy.SampleInterval.Value())
	if err != nil {
		t.Fatal(err)
	}
	if err := monitor.AttachManager(manager); err != nil {
		t.Fatal(err)
	}
	monitorCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- monitor.Run(monitorCtx, nil) }()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				t.Error("sampler", err)
			}
		})
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for manager.Snapshot().Pressure != resourcebudget.Healthy {
		select {
		case <-deadline.C:
			stop()
			t.Fatal("sampler never reported Healthy", monitor.Snapshot())
		case <-ctx.Done():
			stop()
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	return manager, stop
}
