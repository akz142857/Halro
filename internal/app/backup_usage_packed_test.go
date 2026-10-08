package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/usage"
)

// Functional ownership tests drive the real Monitor with deterministic sampled
// signals. Linux capacity acceptance separately uses the actual OS sampler.
func TestPackedBackupReferencesVerifyActualArchiveAndRetainForeignReplacement(t *testing.T) {
	for _, fault := range []string{"clean", "cancel", "foreign-root"} {
		t.Run(fault, func(t *testing.T) {
			cfg := budgetedBackupOperationFixture(t)
			cfg.ResourceBudget.MemoryStateRequired = true
			pressure, err := newResourcePressureWithSource(cfg, pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() }))
			if err != nil {
				t.Fatal(err)
			}
			base, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			ctx := context.WithValue(base, commandResourceScopeKey{}, &commandResourceScope{pressure: pressure, budget: *cfg.ResourceBudget, policy: *cfg.ResourcePressure})
			log, metadata, key := openSealingLedger(t, cfg)
			defer metadata.Close()
			defer clear(key)
			if err := log.Close(); err != nil {
				t.Fatal(err)
			}
			expected, partial, err := ledger.InspectReplayAuthenticated(cfg.LedgerPath(), key, nil)
			if err != nil || partial {
				t.Fatal(expected, partial, err)
			}
			before, err := os.ReadFile(cfg.LedgerPath())
			if err != nil {
				t.Fatal(err)
			}
			parent := t.TempDir()
			t.Setenv("TMPDIR", parent)
			var foreign string
			position, err := withBackupUsageReference(ctx, cfg, cfg.UsagePath(), cfg.LedgerPath(), key, expected, cfg.ResourceBudget.UsageArchiveDiskBytes, pressure, func(e *usage.Exporter, p usage.ArchivePosition) error {
				if p.Files == 0 || p.LastSequence == 0 {
					t.Fatal("fixture did not verify real published archive", p)
				}
				if snapshot := pressure.logicalBudget().Snapshot(); snapshot.Pressure != resourcebudget.Healthy || snapshot.Classes[resourcebudget.Background].RAM < 32<<20 || snapshot.Classes[resourcebudget.Background].Disk == 0 {
					t.Fatal("packed roots escaped actual sampler/fees", snapshot)
				}
				if fault == "cancel" {
					cancel()
					return nil
				}
				if fault == "foreign-root" {
					paths, err := filepath.Glob(filepath.Join(parent, "halro-backup-reference-packed-*", "usage"))
					if err != nil || len(paths) != 1 {
						t.Fatal(paths, err)
					}
					foreign = paths[0]
					if err := os.Rename(foreign, foreign+".original"); err != nil {
						return err
					}
					return os.WriteFile(foreign, []byte("foreign root"), 0600)
				}
				return e.WalkArchiveBackupFiles(ctx, expected.Head, func(string) error { return nil })
			})
			after, readErr := os.ReadFile(cfg.LedgerPath())
			if readErr != nil || !bytes.Equal(before, after) {
				t.Fatal("read-only reference mutated native", readErr)
			}
			if fault == "clean" {
				if err != nil || position.LastSequence == 0 {
					filepath.WalkDir(parent, func(path string, entry os.DirEntry, walkErr error) error {
						t.Log("retained reference path", path, walkErr)
						return nil
					})
					t.Fatal(position, err)
				}
				entries, err := os.ReadDir(parent)
				if err != nil || len(entries) != 0 {
					t.Fatal("owned roots survived", entries, err)
				}
				if snapshot := pressure.logicalBudget().Snapshot(); snapshot.Used != (resourcebudget.Cost{}) {
					t.Fatal("known retirement retained fees", snapshot)
				}
			} else {
				if err == nil || position != (usage.ArchivePosition{}) {
					t.Fatal("failed packed job returned archive authority", position, err)
				}
				if fault == "cancel" && !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				if fault == "foreign-root" {
					raw, readErr := os.ReadFile(foreign)
					if readErr != nil || string(raw) != "foreign root" || pressure.logicalBudget().Snapshot().Used == (resourcebudget.Cost{}) {
						t.Fatal("foreign replacement removed or physical fees refunded", string(raw), readErr, err)
					}
				}
			}
		})
	}
}

func TestPackedBackupCreateAndStagedRestoreReuseOneActualSamplingOwner(t *testing.T) {
	cfg := budgetedBackupOperationFixture(t)
	// The real capacity profile separates its 8 GiB payload/staging ceilings
	// from the fixed catalog mapping. This caught the former 8 GiB RAM claim.
	limits := selectedRuntimeBudgetFixture()
	limits.MemoryStateRequired = true
	limits.TotalRAMBytes, limits.RecoveryRAMBytes, limits.RecoveryReserveRAMBytes = 2<<30, 1<<30, 1<<30
	limits.BackgroundRAMBytes = 1 << 30
	limits.TotalDiskBytes, limits.BackgroundDiskBytes = 96<<30, 48<<30
	limits.RecoveryDiskBytes, limits.RecoveryReserveDiskBytes = 48<<30, 48<<30
	limits.IODiskBytes, limits.BackupWorkspaceDiskBytes = 8<<30, 8<<30
	limits.PersistentInventoryDiskBytes, limits.LedgerAppendDiskBytes = 8<<30, 8<<30
	limits.LedgerProjectionDiskBytes, limits.UsageProjectionDiskBytes = 8<<30, 8<<30
	cfg.ResourceBudget = &limits
	pressure, err := newResourcePressureWithSource(cfg, pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() }))
	if err != nil {
		t.Fatal(err)
	}
	base, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ctx := context.WithValue(base, commandResourceScopeKey{}, &commandResourceScope{pressure: pressure, budget: *cfg.ResourceBudget, policy: *cfg.ResourcePressure})
	configPath, archive := filepath.Join(t.TempDir(), "config.yaml"), filepath.Join(t.TempDir(), "actual-packed.hmbk")
	if err := os.WriteFile(configPath, []byte("version: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x51}, 32)
	manifest, err := CreateBackup(ctx, cfg, configPath, archive, key)
	if err != nil {
		t.Fatal("actual bounded backup", err, pressure.logicalBudget().Snapshot())
	}
	if snapshot := pressure.logicalBudget().Snapshot(); snapshot.Classes[resourcebudget.Recovery].RAM >= 1<<20 || snapshot.Classes[resourcebudget.Background].RAM != 0 || snapshot.Classes[resourcebudget.IO].RAM != 0 {
		t.Fatal("backup retained reducer/sampler work", snapshot)
	}
	firstSamples := pressure.monitor.Snapshot().Samples
	out, err := RestoreBackup(ctx, cfg, archive, key, manifest.BackupID)
	if err != nil || out.BackupID != manifest.BackupID {
		t.Fatal("actual bounded staged restore", out, err, pressure.logicalBudget().Snapshot())
	}
	if pressure.monitor.Snapshot().Samples <= firstSamples {
		t.Fatal("restore did not earn fresh measured pressure", pressure.monitor.Snapshot())
	}
	if snapshot := pressure.logicalBudget().Snapshot(); snapshot.Classes[resourcebudget.Recovery].RAM >= 1<<20 || snapshot.Classes[resourcebudget.Background].RAM != 0 || snapshot.Classes[resourcebudget.IO].RAM != 0 {
		t.Fatal("restore retained reducer/sampler work", snapshot)
	}
}
