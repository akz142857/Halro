package app

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/accountinglimits"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/usage"
)

func TestLedgerSegmentIndexDiskOwnerChargesAggregateAndAppliesRoleShare(t *testing.T) {
	b := runtimeRAMBudgetFixture()
	b.TotalDiskBytes, b.BackgroundDiskBytes = 256<<20, 256<<20
	b.RecoveryDiskBytes, b.RecoveryReserveDiskBytes = 128<<20, 128<<20
	b.IODiskBytes, b.UsageArchiveDiskBytes = 64, 64
	b.LedgerSegmentIndexDiskBytes = 96
	limits := b.Limits(time.Hour)
	manager, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{ResourceBudget: &b}
	owner, share, err := acquireLedgerSegmentIndexDiskFromManager(cfg, manager)
	if err != nil {
		t.Fatal(err)
	}
	if got := manager.Snapshot(); got.RecoveryClaims != 1 || got.Classes[resourcebudget.Recovery].Disk != b.LedgerSegmentIndexDiskBytes {
		t.Fatalf("aggregate index ceiling not claimed before open: %+v", got)
	}
	if share != b.LedgerSegmentIndexDiskBytes {
		t.Fatalf("standalone index share=%d want=%d", share, b.LedgerSegmentIndexDiskBytes)
	}
	pressure := &resourcePressureRuntime{budget: manager}
	archive, releaseArchive, err := newOneShotUsageArchiveDiskBudgetWithPressure(context.Background(), cfg, pressure)
	if err != nil || archive == nil {
		t.Fatal("one-shot archive did not reuse the command Manager", err)
	}
	if got := manager.Snapshot(); got.RecoveryClaims != 2 || got.Classes[resourcebudget.Recovery].Disk != b.LedgerSegmentIndexDiskBytes+b.UsageArchiveDiskBytes {
		t.Fatalf("Ledger and archive claims were not aggregated: %+v", got)
	}
	if err := releaseArchive(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Release(); err != nil {
		t.Fatal(err)
	}

	cfg.Replication = &config.Replication{}
	owner, share, err = acquireLedgerSegmentIndexDiskFromManager(cfg, manager)
	if err != nil {
		t.Fatal(err)
	}
	if share != b.LedgerSegmentIndexDiskBytes/6 {
		t.Fatalf("HA per-index share=%d want=%d", share, b.LedgerSegmentIndexDiskBytes/6)
	}
	if err := owner.Release(); err != nil {
		t.Fatal(err)
	}
	if got := manager.Snapshot(); got.Used.Disk != 0 || got.Owners != 0 {
		t.Fatalf("one-shot index owner leaked: %+v", got)
	}
}

func TestBackupWorkspaceDiskOwnerUsesCommandRecoveryManager(t *testing.T) {
	b := runtimeRAMBudgetFixture()
	b.TotalDiskBytes, b.RecoveryDiskBytes, b.RecoveryReserveDiskBytes = 128, 128, 128
	b.BackupWorkspaceDiskBytes = 96
	b.RecoveryClaims = 6
	manager, err := resourcebudget.New(b.Limits(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{ResourceBudget: &b}
	pressure := &resourcePressureRuntime{budget: manager}
	owner, limit, err := acquireBackupWorkspaceDisk(context.Background(), cfg, pressure)
	if err != nil || owner == nil || limit != b.BackupWorkspaceDiskBytes {
		t.Fatal(owner, limit, err)
	}
	ram, _ := accountinglimits.ProjectionMappingRAMBytes(b.BackupWorkspaceDiskBytes)
	if got := manager.Snapshot(); got.RecoveryClaims != 1 || got.Classes[resourcebudget.Recovery].RAM != ram || got.Classes[resourcebudget.Recovery].Disk != b.BackupWorkspaceDiskBytes {
		t.Fatalf("backup workspace was not charged before catalog creation: %+v", got)
	}
	if err := owner.Release(); err != nil {
		t.Fatal(err)
	}
	if got := manager.Snapshot(); got.Used.Disk != 0 || got.RecoveryClaims != 0 {
		t.Fatalf("backup workspace claim leaked: %+v", got)
	}
}

func TestRestoreWorkspaceDiskOwnerRejectsOversizeAndChargesVerifiedBytes(t *testing.T) {
	b := runtimeRAMBudgetFixture()
	b.TotalDiskBytes, b.RecoveryDiskBytes, b.RecoveryReserveDiskBytes = 128, 128, 128
	b.RestoreWorkspaceDiskBytes = 96
	b.RecoveryClaims = 6
	manager, err := resourcebudget.New(b.Limits(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{ResourceBudget: &b}
	pressure := &resourcePressureRuntime{budget: manager}
	if owner, err := acquireRestoreWorkspaceDisk(context.Background(), cfg, pressure, 97); err == nil || owner != nil {
		t.Fatal("oversized authenticated payload was admitted", owner, err)
	}
	if got := manager.Snapshot(); got.Used.Disk != 0 || got.RecoveryClaims != 0 {
		t.Fatalf("oversized restore changed budget state: %+v", got)
	}
	owner, err := acquireRestoreWorkspaceDisk(context.Background(), cfg, pressure, 73)
	if err != nil || owner == nil {
		t.Fatal(owner, err)
	}
	if got := manager.Snapshot(); got.Classes[resourcebudget.Recovery].Disk != 73 || got.RecoveryClaims != 1 {
		t.Fatalf("restore did not reserve verified payload bytes: %+v", got)
	}
	if err := owner.Release(); err != nil {
		t.Fatal(err)
	}
	if got := manager.Snapshot(); got.Used.Disk != 0 || got.RecoveryClaims != 0 {
		t.Fatalf("restore workspace claim leaked: %+v", got)
	}
}

func TestAccountingWorkspaceConstructionRejectsBeforeSavedFileMutation(t *testing.T) {
	for _, role := range []string{"standalone", "replica"} {
		for _, shape := range []string{"missing_manager", "exhausted", "closed", "cancelled"} {
			t.Run(role+"/"+shape, func(t *testing.T) {
				budgetCfg, pressure := recoveryResourceFixture(t)
				manager := pressure.logicalBudget()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var held resourcebudget.Permit
				switch shape {
				case "missing_manager":
					pressure = nil
				case "exhausted":
					var err error
					held, err = manager.TryAcquireRecovery(resourcebudget.Cost{RAM: budgetCfg.ResourceBudget.RecoveryRAMBytes})
					if err != nil {
						t.Fatal(err)
					}
					defer held.Release()
				case "closed":
					manager.CloseAdmission()
				case "cancelled":
					cancel()
				}
				used := manager.Snapshot().Used
				old := []byte("untrusted prior derivative; quota refusal must preserve it")
				if role == "standalone" {
					native, _, _ := projectedWriterLog(t, []ledger.Event{projectedWriterAcceptance("build")}, ledger.Options{})
					cfg := testConfig(t)
					cfg.ResourceBudget = budgetCfg.ResourceBudget
					if err := os.MkdirAll(cfg.Storage.DataDir, 0700); err != nil {
						t.Fatal(err)
					}
					path := standaloneProjectionPath(cfg)
					if err := os.WriteFile(path, old, 0600); err != nil {
						t.Fatal(err)
					}
					owner, lease, _, err := prepareRuntimeAccountingState(ctx, cfg, native, pressure)
					if err == nil || lease != nil || owner.reader != nil || native.Status().Load() != ledger.AccountingHealthy {
						t.Fatal("refused build changed native/owner", err, owner, lease)
					}
					got, err := os.ReadFile(path)
					if err != nil || string(got) != string(old) {
						t.Fatal("refused build removed prior derivative", err)
					}
				} else {
					cfg, member, native, metadata := replicaRuntimeOwnerFixture(t)
					cfg.ResourceBudget = budgetCfg.ResourceBudget
					for _, path := range []string{replicaRuntimeProjectionPath(cfg), replicaRuntimeProjectionPath(cfg) + ".usage"} {
						if err := os.WriteFile(path, old, 0600); err != nil {
							t.Fatal(err)
						}
					}
					g, err := prepareReplicaRuntimeOwner(ctx, cfg, member, native, metadata, pressure)
					if err == nil || g != nil || native.Status().Load() != ledger.AccountingHealthy {
						t.Fatal("refused Replica build published an owner", g, err)
					}
					for _, path := range []string{replicaRuntimeProjectionPath(cfg), replicaRuntimeProjectionPath(cfg) + ".usage"} {
						got, err := os.ReadFile(path)
						if err != nil || string(got) != string(old) {
							t.Fatal("refused Replica build removed prior derivative", err)
						}
					}
				}
				if manager.Snapshot().Used != used {
					t.Fatal("refused build leaked workspace", manager.Snapshot())
				}
			})
		}
	}
}

func TestStandaloneLedgerDiskCeilingIsChargedThroughOwnedRemoval(t *testing.T) {
	b := runtimeRAMBudgetFixture()
	b.TotalDiskBytes = 1 << 30
	b.BackgroundDiskBytes = 512 << 20
	b.RecoveryDiskBytes = 512 << 20
	b.RecoveryReserveDiskBytes = 512 << 20
	b.LedgerProjectionDiskBytes = 256 << 20
	b.RecoveryClaims = 6
	addProjectionMappingFixtureRAM(t, &b)
	manager, err := resourcebudget.New(b.Limits(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	pressure := &resourcePressureRuntime{budget: manager}
	cfg := testConfig(t)
	cfg.ResourceBudget = &b
	if err := os.MkdirAll(cfg.Storage.DataDir, 0700); err != nil {
		t.Fatal(err)
	}
	native, _, _ := projectedWriterLog(t, []ledger.Event{projectedWriterAcceptance("bounded")}, ledger.Options{})
	defer native.Close()
	b.LedgerProjectionDiskBytes = 4 << 10
	cfg.ResourceBudget = &b
	if owner, lease, _, err := prepareRuntimeAccountingState(context.Background(), cfg, native, pressure); !errors.Is(err, ledger.ErrProjectionDiskLimit) || lease != nil || owner.reader != nil {
		t.Fatal("under-sized runtime Ledger ceiling did not fail closed", owner, lease, err)
	}
	if _, err := os.Lstat(standaloneProjectionPath(cfg)); !errors.Is(err, os.ErrNotExist) || native.Status().Load() != ledger.AccountingHealthy || manager.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal("failed runtime candidate leaked file/charge or poisoned native Ledger", err, native.Status().Load(), manager.Snapshot())
	}
	b.LedgerProjectionDiskBytes = 256 << 20
	cfg.ResourceBudget = &b
	owner, lease, _, err := prepareRuntimeAccountingState(context.Background(), cfg, native, pressure)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := manager.Snapshot()
	ram, _ := accountinglimits.ProjectionMappingRAMBytes(b.LedgerProjectionDiskBytes)
	if snapshot.Classes[resourcebudget.Background].RAM != ram || snapshot.Classes[resourcebudget.Background].Disk != b.LedgerProjectionDiskBytes || snapshot.Classes[resourcebudget.Recovery].Disk != 0 || snapshot.RecoveryClaims != 0 || snapshot.NormalClaims != 1 {
		t.Fatal("published Standalone Ledger did not retain its persistent Background ceiling", snapshot)
	}
	if err := owner.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(standaloneProjectionPath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned Ledger projection remains after retirement", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if snapshot := manager.Snapshot(); snapshot.Used != (resourcebudget.Cost{}) || snapshot.Owners != 0 {
		t.Fatal("Ledger owner removal did not refund its ceiling", snapshot)
	}
}

func TestStandaloneUsageDiskCeilingIsChargedThroughOwnedRemoval(t *testing.T) {
	b := runtimeRAMBudgetFixture()
	b.TotalDiskBytes = 1 << 30
	b.BackgroundDiskBytes = 512 << 20
	b.RecoveryDiskBytes = 512 << 20
	b.RecoveryReserveDiskBytes = 512 << 20
	b.UsageProjectionDiskBytes = 256 << 20
	b.RecoveryClaims = 6
	addProjectionMappingFixtureRAM(t, &b)
	manager, err := resourcebudget.New(b.Limits(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	pressure := &resourcePressureRuntime{budget: manager}
	cfg := testConfig(t)
	cfg.ResourceBudget = &b
	if err := os.MkdirAll(cfg.Storage.DataDir, 0700); err != nil {
		t.Fatal(err)
	}
	native, _, _ := projectedWriterLog(t, []ledger.Event{projectedWriterAcceptance("usage-bounded")}, ledger.Options{})
	defer native.Close()
	accounting, lease, _, err := prepareRuntimeAccountingState(context.Background(), cfg, native, pressure)
	if err != nil {
		t.Fatal(err)
	}

	b.UsageProjectionDiskBytes = 4 << 10
	cfg.ResourceBudget = &b
	if view, _, err := prepareStandaloneUsage(context.Background(), cfg, native, accounting.projection, lease, pressure); view != nil || !errors.Is(err, ledger.ErrProjectionDiskLimit) || !errors.Is(err, usage.ErrDiskUsageDiskLimit) {
		t.Fatal("undersized Standalone Usage ceiling did not fail closed without poisoning Ledger", view, err)
	}
	if _, err := os.Lstat(standaloneUsageProjectionPath(cfg)); !errors.Is(err, os.ErrNotExist) || native.Status().Load() != ledger.AccountingHealthy || manager.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal("failed Usage candidate leaked file/charge or poisoned native Ledger", err, native.Status().Load(), manager.Snapshot())
	}

	b.UsageProjectionDiskBytes = 256 << 20
	cfg.ResourceBudget = &b
	view, _, err := prepareStandaloneUsage(context.Background(), cfg, native, accounting.projection, lease, pressure)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := manager.Snapshot()
	ram, _ := accountinglimits.ProjectionMappingRAMBytes(b.UsageProjectionDiskBytes)
	if snapshot.Classes[resourcebudget.Background].RAM != ram || snapshot.Classes[resourcebudget.Background].Disk != b.UsageProjectionDiskBytes || snapshot.Classes[resourcebudget.Recovery].Disk != 0 {
		t.Fatal("published Usage projection did not retain its Background ceiling", snapshot)
	}
	if err := view.closeOwner(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(standaloneUsageProjectionPath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned Usage projection remains after retirement", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := accounting.close(); err != nil {
		t.Fatal(err)
	}
	if snapshot := manager.Snapshot(); snapshot.Used != (resourcebudget.Cost{}) || snapshot.Owners != 0 {
		t.Fatal("Usage owner removal did not refund its ceiling", snapshot)
	}
}

func TestReplicaLedgerDiskCeilingIsChargedThroughGenerationClose(t *testing.T) {
	cfg, member, native, metadata := replicaRuntimeOwnerFixture(t)
	b := runtimeRAMBudgetFixture()
	b.TotalDiskBytes = 1 << 30
	b.BackgroundDiskBytes = 512 << 20
	b.RecoveryDiskBytes = 512 << 20
	b.RecoveryReserveDiskBytes = 512 << 20
	b.LedgerProjectionDiskBytes, b.UsageProjectionDiskBytes = 256<<20, 256<<20
	addProjectionMappingFixtureRAM(t, &b)
	b.LedgerProjectionDiskBytes = 4 << 10
	b.UsageProjectionDiskBytes = 4 << 10
	b.RecoveryClaims = 7
	manager, err := resourcebudget.New(b.Limits(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	pressure := &resourcePressureRuntime{budget: manager}
	cfg.ResourceBudget = &b
	if g, err := prepareReplicaRuntimeOwner(context.Background(), cfg, member, native, metadata, pressure); g != nil || !errors.Is(err, ledger.ErrProjectionDiskLimit) {
		t.Fatal("undersized Replica ceiling did not fail closed", g, err)
	}
	for _, path := range []string{replicaRuntimeProjectionPath(cfg), replicaRuntimeProjectionPath(cfg) + ".usage"} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("failed Replica candidate remains", path, err)
		}
	}
	if native.Status().Load() != ledger.AccountingHealthy || manager.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal("failed Replica ceiling poisoned native Ledger or leaked budget", native.Status().Load(), manager.Snapshot())
	}
	b.LedgerProjectionDiskBytes = 256 << 20
	cfg.ResourceBudget = &b
	if g, err := prepareReplicaRuntimeOwner(context.Background(), cfg, member, native, metadata, pressure); g != nil || !errors.Is(err, usage.ErrDiskUsageDiskLimit) {
		t.Fatal("undersized Replica Usage ceiling did not fail closed", g, err)
	}
	for _, path := range []string{replicaRuntimeProjectionPath(cfg), replicaRuntimeProjectionPath(cfg) + ".usage"} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("failed Replica Usage candidate remains", path, err)
		}
	}
	if manager.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal("failed Replica Usage build leaked disk permits", manager.Snapshot())
	}
	b.UsageProjectionDiskBytes = 256 << 20
	cfg.ResourceBudget = &b
	g, err := prepareReplicaRuntimeOwner(context.Background(), cfg, member, native, metadata, pressure)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := manager.Snapshot()
	ram, _ := accountinglimits.ProjectionMappingRAMBytes(b.LedgerProjectionDiskBytes, b.UsageProjectionDiskBytes)
	if snapshot.Classes[resourcebudget.Background].RAM != ram || snapshot.Classes[resourcebudget.Background].Disk != b.LedgerProjectionDiskBytes+b.UsageProjectionDiskBytes || snapshot.Classes[resourcebudget.Recovery].Disk != 0 || snapshot.RecoveryClaims != 0 {
		t.Fatal("published Replica Ledger/Usage did not retain their Background ceilings", snapshot)
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(replicaRuntimeProjectionPath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Replica Ledger remains after generation close", err)
	}
	if snapshot := manager.Snapshot(); snapshot.Used != (resourcebudget.Cost{}) || snapshot.Owners != 0 {
		t.Fatal("Replica generation close did not refund Ledger ceiling", snapshot)
	}
}

func TestAccountingWorkspaceBuildCancellationRetainsFeeUntilCandidateCleanup(t *testing.T) {
	for _, role := range []string{"standalone", "replica"} {
		t.Run(role, func(t *testing.T) {
			budgetCfg, pressure := recoveryResourceFixture(t)
			manager := pressure.logicalBudget()
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(resume) }) }
			defer unblock()
			var seen atomic.Bool
			var paths []string
			probe := generationProbeContext{Context: base, probe: func() {
				if len(paths) == 0 {
					return
				}
				if _, err := os.Stat(paths[0]); err != nil || !seen.CompareAndSwap(false, true) {
					return
				}
				close(entered)
				<-resume // actual second native pass/DB construction is still on stack
			}}
			done := make(chan error, 1)
			if role == "standalone" {
				native, _, _ := projectedWriterLog(t, []ledger.Event{projectedWriterAcceptance("build")}, ledger.Options{})
				cfg := testConfig(t)
				cfg.ResourceBudget = budgetCfg.ResourceBudget
				if err := os.MkdirAll(cfg.Storage.DataDir, 0700); err != nil {
					t.Fatal(err)
				}
				paths = []string{standaloneProjectionPath(cfg)}
				go func() { _, _, _, err := prepareRuntimeAccountingState(probe, cfg, native, pressure); done <- err }()
			} else {
				cfg, member, native, metadata := replicaRuntimeOwnerFixture(t)
				cfg.ResourceBudget = budgetCfg.ResourceBudget
				paths = []string{replicaRuntimeProjectionPath(cfg), replicaRuntimeProjectionPath(cfg) + ".usage"}
				go func() {
					_, err := prepareReplicaRuntimeOwner(probe, cfg, member, native, metadata, pressure)
					done <- err
				}()
			}
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("actual candidate construction not reached")
			}
			cancel()
			select {
			case err := <-done:
				t.Fatal("cancel returned before construction stack joined", err)
			case <-time.After(10 * time.Millisecond):
			}
			if s := manager.Snapshot(); s.RecoveryClaims != 1 || s.Classes[resourcebudget.Recovery].RAM != budgetCfg.ResourceBudget.AccountingRecoveryRAMBytes || s.Pressure != resourcebudget.Unknown {
				t.Fatal("construction dropped its reserved workspace", s)
			}
			unblock()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			for _, path := range paths {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("failed candidate remains after retirement", path, err)
				}
			}
			if s := manager.Snapshot(); s.Used != (resourcebudget.Cost{}) || s.Owners != 0 {
				t.Fatal("construction cleanup leaked workspace", s)
			}
		})
	}
}

func TestAccountingWorkspaceReplicaSuccessReleasesTransientClaimKeepsAppliedCut(t *testing.T) {
	cfg, member, native, metadata := replicaRuntimeOwnerFixture(t)
	b, pressure := recoveryResourceFixture(t)
	cfg.ResourceBudget = b.ResourceBudget
	g, err := prepareReplicaRuntimeOwner(context.Background(), cfg, member, native, metadata, pressure)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	head, _, err := g.ledger.ReadPosition(context.Background())
	stats, statErr := g.usageDisk.ReadStats(context.Background())
	if err != nil || statErr != nil || head.Sequence != 4 || stats.Watermark.Sequence != 4 || stats.Totals.Attempts != 0 {
		t.Fatal("excluded durable suffix acquired applied credit", head, stats, err, statErr)
	}
	if s := pressure.logicalBudget().Snapshot(); s.Used != (resourcebudget.Cost{}) || s.Acquired[resourcebudget.Recovery] != 1 {
		t.Fatal("live candidate retained transient construction credit", s)
	}
}

func TestAccountingWorkspacePanicClosesUnpublishedCandidatesAndReleasesNativeLease(t *testing.T) {
	for _, role := range []string{"standalone", "replica"} {
		t.Run(role, func(t *testing.T) {
			b, pressure := recoveryResourceFixture(t)
			cfg := testConfig(t)
			cfg.ResourceBudget = b.ResourceBudget
			var paths []string
			var native *ledger.Log
			var run func(context.Context)
			if role == "standalone" {
				native, _, _ = projectedWriterLog(t, []ledger.Event{projectedWriterAcceptance("build")}, ledger.Options{})
				if err := os.MkdirAll(cfg.Storage.DataDir, 0700); err != nil {
					t.Fatal(err)
				}
				paths = []string{standaloneProjectionPath(cfg)}
				run = func(ctx context.Context) { _, _, _, _ = prepareRuntimeAccountingState(ctx, cfg, native, pressure) }
			} else {
				c, member, n, metadata := replicaRuntimeOwnerFixture(t)
				c.ResourceBudget = b.ResourceBudget
				native = n
				paths = []string{replicaRuntimeProjectionPath(c), replicaRuntimeProjectionPath(c) + ".usage"}
				run = func(ctx context.Context) { _, _ = prepareReplicaRuntimeOwner(ctx, c, member, n, metadata, pressure) }
			}
			var triggered bool
			probe := generationProbeContext{Context: context.Background(), probe: func() {
				// Replica must already own BOTH DBs, beyond Ledger's builder
				// defer, to prove generation cleanup handles later panics too.
				if _, err := os.Stat(paths[len(paths)-1]); err != nil || triggered {
					return
				}
				triggered = true
				panic("construction probe")
			}}
			func() {
				defer func() {
					if got := recover(); got != "construction probe" {
						t.Fatal("panic swallowed or changed", got)
					}
				}()
				run(probe)
			}()
			if !triggered {
				t.Fatal("construction panic boundary not reached")
			}
			for _, path := range paths {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("panic leaked unpublished derivative", path, err)
				}
			}
			if s := pressure.logicalBudget().Snapshot(); s.Used != (resourcebudget.Cost{}) || s.Owners != 0 {
				t.Fatal("panic leaked workspace", s)
			}
			// Acquire a new actual native lease, proving panic did not strand
			// the failed Standalone constructor's maintenance exclusivity.
			lease, err := native.BeginMaintenance(context.Background())
			if role == "standalone" {
				if err != nil {
					t.Fatal("panic retained native lease", err)
				}
				if err := lease.Close(); err != nil {
					t.Fatal(err)
				}
			} else if err == nil || lease != nil || native.Status().Load() != ledger.AccountingHealthy {
				t.Fatal("Replica panic granted semantic recovery or poisoned native", lease, err)
			}
		})
	}
}

func addProjectionMappingFixtureRAM(t *testing.T, b *config.ResourceBudget) {
	t.Helper()
	ram, err := accountinglimits.ProjectionMappingRAMBytes(b.LedgerProjectionDiskBytes, b.UsageProjectionDiskBytes, b.GovernanceProjectionDiskBytes)
	if err != nil {
		t.Fatal(err)
	}
	b.TotalRAMBytes += 2 * ram
	b.BackgroundRAMBytes += ram
	b.RecoveryRAMBytes += ram
	b.RecoveryReserveRAMBytes += ram
}

func TestProjectionMappingAdmissionPrecedesFileMutationAndRetirementPreservesForeignFile(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	if err := os.MkdirAll(cfg.Storage.DataDir, 0700); err != nil {
		t.Fatal(err)
	}
	path := standaloneProjectionPath(cfg)
	const old = "untouched derived file before failed RAM admission"
	if err := os.WriteFile(path, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	native, _, _ := projectedWriterLog(t, []ledger.Event{projectedWriterAcceptance("mapping")}, ledger.Options{})
	defer native.Close()
	b := runtimeRAMBudgetFixture()
	b.TotalDiskBytes, b.BackgroundDiskBytes = 1<<30, 512<<20
	b.RecoveryDiskBytes, b.RecoveryReserveDiskBytes = 512<<20, 512<<20
	b.LedgerProjectionDiskBytes = 256 << 20
	b.RecoveryClaims = 6
	manager, err := resourcebudget.New(b.Limits(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ResourceBudget = &b
	pressure := &resourcePressureRuntime{budget: manager}
	if _, lease, _, err := prepareRuntimeAccountingState(ctx, cfg, native, pressure); lease != nil || !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatal("unpaid mapping reached candidate IO", lease, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != old || manager.Snapshot().Used != (resourcebudget.Cost{}) || native.Status().Load() != ledger.AccountingHealthy {
		t.Fatal("mapping refusal changed existing files, native health or fees", err, manager.Snapshot())
	}
	b.LedgerProjectionDiskBytes = 1 << 20
	addProjectionMappingFixtureRAM(t, &b)
	manager, err = resourcebudget.New(b.Limits(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	pressure = &resourcePressureRuntime{budget: manager}
	owner, lease, _, err := prepareRuntimeAccountingState(ctx, cfg, native, pressure)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := os.Rename(path, path+".owned"); err != nil {
		t.Fatal(err)
	}
	if err := owner.close(); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("renamed projection was mistaken for retired inode", err)
	}
	ram, _ := accountinglimits.ProjectionMappingRAMBytes(b.LedgerProjectionDiskBytes)
	if s := manager.Snapshot(); s.Classes[resourcebudget.Background].RAM != ram || s.Classes[resourcebudget.Background].Disk != b.LedgerProjectionDiskBytes || s.NormalClaims != 1 {
		t.Fatal("missing projection name refunded live inode", s)
	}
	const foreign = "replacement must survive a stale projection owner"
	if err := os.WriteFile(path, []byte(foreign), 0600); err != nil {
		t.Fatal(err)
	}
	if err := owner.close(); err == nil {
		t.Fatal("failed inode retirement reported success")
	}
	if s := manager.Snapshot(); s.Classes[resourcebudget.Background].RAM != ram || s.Classes[resourcebudget.Background].Disk != b.LedgerProjectionDiskBytes || s.NormalClaims != 1 {
		t.Fatal("failed retirement refunded the owned file/mapping fee", s)
	}
	raw, err = os.ReadFile(path)
	if err != nil || string(raw) != foreign {
		t.Fatal("stale owner removed the replacement", err)
	}
}
