//go:build linux

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/statecut"
	"github.com/akz142857/Halro/internal/store/lock"
	"golang.org/x/sys/unix"
)

// Genuine TLS/Member/Ordering factory component evidence only. No transport
// admission, HA completion/confirmation, or full-vector Ready is granted.
func TestHASelectedPhysicalLedgerDirectoryActualBothRoles(t *testing.T) {
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
			limits.LedgerAppendDiskBytes = 64 << 20
			cfg.ResourceBudget = &limits
			// An independent first process owns real preparation, then SIGKILL
			// leaves its authenticated controls/targets for this fresh factory.
			killHAPhysicalLedgerDirectoryPreparation(t, cfg, fixture.keys)
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
			c := f.reopened.Owners.Components
			raw, err := json.Marshal(before.Seal)
			if err != nil {
				t.Fatal(err)
			}
			selection := sha256.Sum256(raw)
			if err = c.Ledger.EnableHAQualifiedPhysicalTerminals(ctx, c.NativeLedger, c.Lease, limits.LedgerAppendDiskBytes, f.growth.credit, f.growth, selection); err != nil {
				t.Fatal("genuine Ledger preparation", err)
			}
			if err = c.Directory.EnableHAPhysicalCatchup(ctx, c.NativeLedger, c.Ledger, c.Lease, selection); err != nil {
				t.Fatal("genuine Directory preparation", err)
			}
			if err = c.Directory.InstallHAPhysicalCatchupCoverage(ctx, c.NativeLedger, c.Ledger, c.Lease); err != nil {
				t.Fatal("genuine captured coverage", err)
			}
			if err = c.Usage.EnableHAPhysicalCatchup(ctx, c.NativeLedger, c.Ledger, c.Lease, selection); err != nil {
				t.Fatal("genuine Usage preparation", err)
			}
			cost := manager.Snapshot().Used
			wrong := selection
			wrong[0] ^= 1
			if err = c.NativeLedger.BindHAPhysicalSelectionDigestContext(ctx, c.Lease, wrong); !errors.Is(err, authenticatedstate.ErrSealIneligible) {
				t.Fatal("caller digest granted HA selection", err)
			}
			if manager.Snapshot().Used != cost {
				t.Fatal("wrong digest retired actual owners")
			}
			if err = c.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
				if err := c.Directory.RetireHAPhysicalCatchupAfterCommon(ctx, c.NativeLedger, c.Ledger, c.Lease, c.Publisher); err != nil {
					return err
				}
				if err := c.Usage.RetireHAPhysicalCatchupAfterCommon(ctx, c.NativeLedger, c.Ledger, c.Lease, c.Publisher); err != nil {
					return err
				}
				if err := c.NativeLedger.RetireHAPhysicalPreparationAfterCommon(ctx, c.Lease); err != nil {
					return err
				}
				if err := c.NativeLedger.BindHAPhysicalSelectionDigestContext(ctx, c.Lease, selection); err != nil {
					return err
				}
				if err := c.Usage.BindHAPhysicalCommonSelection(ctx, c.NativeLedger, c.Ledger, selection); err != nil {
					return err
				}
				return c.Directory.BindHAPhysicalCommonSelection(ctx, c.NativeLedger, c.Ledger, c.Lease, selection)
			}); err != nil {
				t.Fatal("genuine post-common physical handoff", err)
			}
			if err = c.NativeLedger.CloseHAQualifiedPhysicalContext(ctx, c.Lease); err != nil {
				t.Fatal("genuine HA actual native Close", err)
			}
			if err = f.Close(); err != nil {
				t.Fatal("actual joined native/derived close", err)
			}
		})
	}
}

func TestHAPhysicalLedgerDirectoryPreparationSIGKILLHelper(t *testing.T) {
	path := os.Getenv("HALRO_HA_PHYSICAL_LD_CRASH_REQUEST")
	if path == "" {
		t.Skip("actual subprocess helper")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var request haMetadataCrashRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	guard, err := lock.Acquire(request.Config.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	f, err := openHASelectedRuntimeFoundation(ctx, request.Config, request.Keys.Master, guard, functionalHAMetadataManager(t, request.Config), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.prepareSelectedRoots(ctx); err != nil {
		t.Fatal(err)
	}
	c := f.reopened.Owners.Components
	seal, found, err := c.Publisher.CurrentContext(ctx)
	if err != nil || !found {
		t.Fatal(err)
	}
	raw, err = json.Marshal(seal)
	if err != nil {
		t.Fatal(err)
	}
	selection := sha256.Sum256(raw)
	if err = c.Ledger.EnableHAQualifiedPhysicalTerminals(ctx, c.NativeLedger, c.Lease, request.Config.ResourceBudget.LedgerAppendDiskBytes, f.growth.credit, f.growth, selection); err != nil {
		t.Fatal(err)
	}
	if err = c.Directory.EnableHAPhysicalCatchup(ctx, c.NativeLedger, c.Ledger, c.Lease, selection); err != nil {
		t.Fatal(err)
	}
	if err = c.Directory.InstallHAPhysicalCatchupCoverage(ctx, c.NativeLedger, c.Ledger, c.Lease); err != nil {
		t.Fatal(err)
	}
	if err = c.Usage.EnableHAPhysicalCatchup(ctx, c.NativeLedger, c.Ledger, c.Lease, selection); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(request.Ready, []byte("prepared"), 0600); err != nil {
		t.Fatal(err)
	}
	for {
		time.Sleep(100 * time.Millisecond)
	}
}

func killHAPhysicalLedgerDirectoryPreparation(t *testing.T, cfg config.Config, keys HAMetadataKeys) {
	t.Helper()
	directory := t.TempDir()
	requestPath, ready := filepath.Join(directory, "request.json"), filepath.Join(directory, "prepared")
	raw, err := json.Marshal(haMetadataCrashRequest{Config: cfg, Keys: keys, Ready: ready})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(requestPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestHAPhysicalLedgerDirectoryPreparationSIGKILLHelper$", "-test.count=1")
	command.Env = append(os.Environ(), "HALRO_HA_PHYSICAL_LD_CRASH_REQUEST="+requestPath)
	output := &bytes.Buffer{}
	command.Stdout, command.Stderr = output, output
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.ProcessState == nil {
			command.Process.Kill()
			command.Wait()
		}
	})
	deadline := time.Now().Add(30 * time.Second)
	for {
		raw, err := os.ReadFile(ready)
		if err == nil && string(raw) == "prepared" {
			break
		}
		if time.Now().After(deadline) {
			command.Process.Kill()
			command.Wait()
			t.Fatal("child actual preparation failed", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	killHAMetadataChild(t, command, output)
}
