//go:build linux

package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/statecut"
	"github.com/akz142857/Halro/internal/store/lock"
	"golang.org/x/sys/unix"
)

// Actual TLS/Member/Ordering qualification and the genuine seven-root publisher
// consume prepaid Seal and capsule targets at real ENOSPC. This remains an
// offline common-publication slice; no full terminal-vector Ready is claimed.
func TestHASelectedPhysicalCommonCachesActualENOSPCAndFreshBothRoles(t *testing.T) {
	root := os.Getenv("HALRO_EXT4_ROOT")
	if root == "" {
		t.Skip("requires explicitly isolated ext4 runner")
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(root, &fs); err != nil || fs.Type != unix.EXT4_SUPER_MAGIC {
		t.Fatal("actual ext4 fixture", err)
	}
	fixture := prepareHAMetadataMaintenanceFixture(t)
	prepareHAMetadataMaintenanceArtifacts(t, fixture)
	completeHAMemoryActivationBoundary(t, fixture)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, cfg := range fixture.configs {
		guard, err := lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		a, err := ActivateHAMemoryRuntime(ctx, cfg, fixture.keys, guard, functionalHAMetadataManager(t, cfg), logger)
		if err != nil {
			t.Fatal(err)
		}
		if err = a.Close(); err != nil {
			t.Fatal(err)
		}
		if err = guard.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for _, cfg := range fixture.configs {
		t.Run(cfg.Replication.NodeID, func(t *testing.T) {
			limits := *cfg.ResourceBudget
			limits.TotalRAMBytes = 2 << 30
			limits.RecoveryRAMBytes, limits.RecoveryReserveRAMBytes = 896<<20, 896<<20
			limits.CompletionRAMBytes = 1 << 30
			limits.PhysicalTerminalReserve = true
			cfg.ResourceBudget = &limits
			guard, err := lock.Acquire(cfg.Storage.DataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			manager := functionalHAMetadataManager(t, cfg)
			f, err := openHASelectedRuntimeFoundation(ctx, cfg, fixture.keys.Master, guard, manager, logger)
			if err != nil {
				t.Fatal("physical HA source factories", err)
			}
			defer func() {
				if err := f.Close(); err != nil {
					t.Error("actual foundation close", err)
				}
			}()
			var before statecut.HASelectedCut
			if err = statecut.WithHASelectedCut(ctx, f.owners, func(ctx context.Context, cut statecut.HASelectedCut) error { before = cut; return ctx.Err() }); err != nil {
				t.Fatal(err)
			}
			if err = f.prepareSelectedRoots(ctx); err != nil {
				t.Fatal("actual joined audit/native preparation/reopen", err)
			}
			if f.reopened == nil || f.reopened.Owners.Components.Directory == nil || f.reopened.Owners.Components.Governance == nil {
				t.Fatal("actual derived owners missing")
			}
			if err = statecut.WithHASelectedCut(ctx, f.owners, func(ctx context.Context, cut statecut.HASelectedCut) error {
				if cut.Seal != before.Seal || cut.Authority != before.Authority || cut.Allowed != before.Allowed || cut.Heads != before.Heads || cut.ActualMember.DurableIndex != before.ActualMember.DurableIndex || cut.ActualMember.AppliedIndex != before.ActualMember.AppliedIndex {
					t.Fatal("physical preparation changed independently allowed cut")
				}
				if cut.Authority.Role == authenticatedstate.SealStandalone {
					t.Fatal("physical preparation substituted role")
				}
				return ctx.Err()
			}); err != nil {
				t.Fatal(err)
			}
			if f.replication.startupReady.Load() || f.business != nil {
				t.Fatal("native preparation granted business/HA readiness")
			}
			if err = f.prepareHAPhysicalPublicationTargets(ctx); err != nil {
				t.Fatal("actual physical Seal/capsule preparation", err)
			}
			profile := ledger.HAPhysicalCheckpointWorkspaceRequirements()
			var ram int64
			for _, slot := range profile {
				ram += slot.RAM
			}
			work, err := manager.TryAcquireCompletableWork(resourcebudget.IO, resourcebudget.Cost{RAM: 4096}, resourcebudget.Cost{RAM: ram}, profile)
			if err != nil {
				t.Fatal("original publication prepayment", err)
			}
			defer work.Active.Release()
			defer work.Close()
			filler := filepath.Join(cfg.Storage.DataDir, "foreign-common-fill")
			defer os.Remove(filler)
			if err = exhaustPhysicalAcceptedFixture(filler); err != nil {
				t.Fatal(err)
			}
			if err = manager.SetPressure(resourcebudget.Pressured); err != nil {
				t.Fatal(err)
			}
			paid := manager.Snapshot()
			base := before.Seal
			base.Revision++
			err = work.WithWorkspace(ctx, func(ctx context.Context) error {
				publication, err := statecut.PublishHASelectedMaintenanceCapturedStartupCaches(ctx, f.reopened.Owners, base, func(ctx context.Context) (statecut.StartupCacheCandidates, error) {
					return f.prepareHAPhysicalCheckpointCaches(ctx, before.Seal)
				})
				if err != nil {
					return err
				}
				f.reopened.Seal = publication.Seal
				return publication.CleanupError()
			})
			if err != nil {
				t.Fatal("actual prepaid seven-root publication at ENOSPC", err)
			}
			if after := manager.Snapshot(); after.NormalClaims != paid.NormalClaims || after.RecoveryClaims != paid.RecoveryClaims || after.Used != paid.Used {
				t.Fatal("publication escaped original workspace", paid.Used, after.Used)
			}
			if err = unix.Statfs(root, &fs); err != nil || fs.Bavail != 0 {
				t.Fatal("publication released blocks", fs.Bavail, err)
			}
			if err = work.WithWorkspace(ctx, func(ctx context.Context) error { return f.Close() }); err != nil {
				t.Fatal("actual full-pressure source/target join", err)
			}
			if err = os.Remove(filler); err != nil {
				t.Fatal(err)
			}
			fresh, err := openHASelectedRuntimeFoundation(ctx, cfg, fixture.keys.Master, guard, functionalHAMetadataManager(t, cfg), logger)
			if err != nil {
				t.Fatal("independent physical common reopen", err)
			}
			defer fresh.Close()
			if err = fresh.prepareSelectedRoots(ctx); err != nil {
				t.Fatal("fresh source/receipt/derived qualification", err)
			}
			if err = fresh.prepareHAPhysicalPublicationTargets(ctx); err != nil {
				t.Fatal("fresh actual same-inode capsule restore", err)
			}
			if fresh.reopened.Seal != base {
				t.Fatal("fresh generation did not select exact published common")
			}
			if err = fresh.Close(); err != nil {
				t.Fatal("fresh actual source/target join", err)
			}
		})
	}
}
