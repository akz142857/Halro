//go:build linux

package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
	"github.com/akz142857/Halro/internal/store/lock"
	"golang.org/x/sys/unix"
)

// The actual TLS/Member/Ordering fixture prepares every physical source and
// derived owner, then retires an unused vector under the exact common cut.
// Accepted HA terminals, confirmation and crash recovery remain separate tests.
func TestHASelectedPhysicalDerivedVectorActualBothRoles(t *testing.T) {
	exerciseHASelectedPhysicalDerivedVector(t, false)
}

func TestHASelectedPhysicalDerivedVectorActualCommonAtENOSPCBothRoles(t *testing.T) {
	exerciseHASelectedPhysicalDerivedVector(t, true)
}

func exerciseHASelectedPhysicalDerivedVector(t *testing.T, publishAtENOSPC bool) {
	t.Helper()
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
			limits.LedgerProjectionDiskBytes = 56 << 20
			limits.LedgerSegmentIndexDiskBytes = 48 << 20
			limits.UsageProjectionDiskBytes = 128 << 20
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
			if err = f.prepareHAPhysicalDerivedVector(ctx); err != nil {
				t.Fatal("genuine all-source/derived physical vector", err, "actual same-manager snapshot", manager.Snapshot())
			}
			if publishAtENOSPC {
				filler := filepath.Join(cfg.Storage.DataDir, "foreign-full-vector-fill")
				defer os.Remove(filler)
				if err := exhaustPhysicalAcceptedFixture(filler); err != nil {
					t.Fatal(err)
				}
				if err := unix.Statfs(root, &fs); err != nil || fs.Bavail != 0 {
					t.Fatal("actual private ext4 not exhausted", err, fs.Bavail)
				}
				if err := manager.SetPressure(resourcebudget.Pressured); err != nil {
					t.Fatal(err)
				}
				paid := manager.Snapshot()
				base := f.reopened.Seal
				base.Revision++
				c := f.reopened.Owners.Components
				if err := c.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
					publication, err := statecut.PublishHASelectedMaintenanceCapturedStartupCaches(ctx, f.reopened.Owners, base, func(ctx context.Context) (statecut.StartupCacheCandidates, error) {
						return f.prepareHAPhysicalCheckpointCaches(ctx, before.Seal)
					})
					if err != nil {
						return err
					}
					f.reopened.Seal = publication.Seal
					return publication.CleanupError()
				}); err != nil {
					t.Fatal("actual complete-vector common publication at ENOSPC", err)
				}
				after := manager.Snapshot()
				if paid.Used != after.Used || paid.NormalClaims != after.NormalClaims || paid.RecoveryClaims != after.RecoveryClaims {
					t.Fatal("full-vector common publication used new claims", paid, after)
				}
				if err := unix.Statfs(root, &fs); err != nil || fs.Bavail != 0 {
					t.Fatal("publication did not retain original extent ownership", err, fs.Bavail)
				}
			}
			if err = f.retireHAPhysicalDerivedVectorAfterCommon(ctx); err != nil {
				t.Fatal("genuine unused vector retirement after exact common", err)
			}
			c := f.reopened.Owners.Components
			if !publishAtENOSPC {
				// Renewal keeps the original native/Directory coverage callback.
				// Its retired window is replaced only under a fresh genuine whole
				// maintenance loan, never by a public role or copied digest.
				for cycle := 0; cycle < 2; cycle++ {
					if err := f.prepareHAPhysicalDerivedVector(ctx); err != nil {
						t.Fatal("same-owner complete-vector reprepare", cycle, err)
					}
					if err := c.Directory.InstallHAPhysicalCatchupCoverage(ctx, c.NativeLedger, c.Ledger, c.Lease); err != nil {
						t.Fatal("same original Directory coverage registration", cycle, err)
					}
					canceled, cancel := context.WithCancel(ctx)
					cancel()
					if err := c.Directory.InstallHAPhysicalCatchupCoverage(canceled, c.NativeLedger, c.Ledger, c.Lease); err == nil {
						t.Fatal("canceled maintenance retained registration authority")
					}
					if err := f.retireHAPhysicalDerivedVectorAfterCommon(ctx); err != nil {
						t.Fatal("same-owner complete-vector retirement", cycle, err)
					}
				}
			}
			if err = c.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error { return c.NativeLedger.CloseHAQualifiedPhysicalContext(ctx, c.Lease) }); err != nil {
				t.Fatal("actual HA native qualified close", err)
			}
			if err = f.Close(); err != nil {
				t.Fatal("actual joined seven-root/native/target close", err)
			}
		})
	}
}
