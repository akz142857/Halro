//go:build linux

package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
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
	fixture := prepareHAMetadataMaintenanceFixture(t)
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
	var owners [2]*haSelectedRuntimeFoundation
	for i, cfg := range fixture.configs {
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
		if i == 1 {
			// Each of the finite 32 received records owes its own worst-case
			// 16 MiB Ledger and Usage COW extent before native acceptance.
			limits.LedgerProjectionDiskBytes = 576 << 20
			limits.UsageProjectionDiskBytes = 576 << 20
		}
		cfg.ResourceBudget = &limits
		fixture.configs[i] = cfg
		guard, err := lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		f, err := openHASelectedRuntimeFoundation(ctx, cfg, fixture.keys.Master, guard, functionalHAMetadataManager(t, cfg), logger)
		if err != nil {
			t.Fatal("actual physical business foundation", i, err)
		}
		owners[i] = f
		t.Cleanup(func() {
			if err := f.Close(); err != nil {
				t.Error("original foundation cleanup", i, err)
			}
			if err := guard.Close(); err != nil {
				t.Error(err)
			}
		})
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
		if err := f.prepareSelectedTransport(logger); err != nil {
			t.Fatal("actual whole physical transport", i, err)
		}
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 2)
	for _, f := range owners {
		go func(f *haSelectedRuntimeFoundation) { done <- f.serveSelectedTransport(runCtx) }(f)
	}
	defer func() {
		stop()
		for range 2 {
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		}
	}()
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
	var offset int64
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
	info, err := f.Stat()
	if err != nil {
		return errors.Join(err, f.Close())
	}
	_, writeErr := f.WriteAt(make([]byte, 4096), info.Size()+4096)
	if !errors.Is(writeErr, syscall.ENOSPC) {
		return errors.Join(errors.New("actual foreign write did not observe ENOSPC"), writeErr, f.Close())
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
