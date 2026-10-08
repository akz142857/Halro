package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/usage"
	"os"
	"path/filepath"
	"testing"
)

func enableBackupReferenceBudget(cfg config.Config) config.Config {
	cfg = enableLedgerSegmentIndexBudget(cfg)
	b := cfg.ResourceBudget
	b.LedgerProjectionDiskBytes, b.UsageProjectionDiskBytes, b.GovernanceProjectionDiskBytes = 8<<20, 8<<20, 8<<20
	// Three published mappings coexist with later private backup references.
	b.TotalRAMBytes += 240 << 20
	b.BackgroundRAMBytes += 120 << 20
	b.RecoveryRAMBytes += 120 << 20
	b.RecoveryReserveRAMBytes += 120 << 20
	b.BackupWorkspaceDiskBytes = 32 << 20
	b.TotalRAMBytes += 512 << 20
	b.RecoveryRAMBytes += 512 << 20
	b.RecoveryReserveRAMBytes += 512 << 20
	b.RecoveryClaims, b.MaxClaims = 24, 48
	b.ControlRAMBytes = 48 * resourcebudget.ClaimRAMBytes
	return cfg
}

func budgetedBackupOperationFixture(t *testing.T) config.Config {
	t.Helper()
	cfg := usageReferenceBudgetFixture(t)
	b := cfg.ResourceBudget
	// Deliberately generous logical test capacities exercise ownership rather
	// than a production capacity promise. The native fixture remains tiny.
	b.TotalRAMBytes = 4 << 30
	b.BackgroundRAMBytes, b.ActiveRAMBytes, b.IORAMBytes = 1<<30, 1<<30, 1<<30
	b.RecoveryRAMBytes, b.RecoveryReserveRAMBytes = 2<<30, 2<<30
	b.TotalDiskBytes = 16 << 30
	b.BackgroundDiskBytes, b.IODiskBytes = 8<<30, 8<<30
	b.RecoveryDiskBytes, b.RecoveryReserveDiskBytes = 8<<30, 8<<30
	b.UsageArchiveDiskBytes = 2 << 30
	b.MetadataStoreDiskBytes = 16 << 20
	b.GovernanceProjectionDiskBytes = 8 << 20
	b.BackupWorkspaceDiskBytes, b.RestoreWorkspaceDiskBytes = 32<<20, 32<<20
	b.RecoveryClaims, b.MaxClaims = 64, 128
	b.ControlRAMBytes = 128 * resourcebudget.ClaimRAMBytes
	return cfg
}

func TestBudgetedBackupRestoreUsesSharedReferencesEndToEnd(t *testing.T) {
	cfg := budgetedBackupOperationFixture(t)
	ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "shared.hmbk")
	key := bytes.Repeat([]byte{0x69}, 32)
	manifest, err := CreateBackup(ctx, cfg, configPath, archive, key)
	if err != nil {
		t.Fatal("budgeted create", err, pressure.logicalBudget().Snapshot())
	}
	if s := pressure.logicalBudget().Snapshot(); s.Classes[resourcebudget.Recovery].RAM >= 1<<20 {
		t.Fatal("create retained closed mappings or reference workspaces", s)
	}
	// Restore inherits the root scope; every stage mapping and reference must
	// contend with the original job's conservative fixed path-owner charges.
	out, err := RestoreBackup(ctx, cfg, archive, key, manifest.BackupID)
	if err != nil || out.BackupID != manifest.BackupID {
		t.Fatal("budgeted staged restore", out, err, pressure.logicalBudget().Snapshot())
	}
	if s := pressure.logicalBudget().Snapshot(); s.Classes[resourcebudget.Recovery].RAM >= 1<<20 || s.RecoveryClaims > commandMetadataFileSlots {
		t.Fatal("restore retained closed mappings or private references", s)
	}
}

func TestBackupUsageReferenceSharedBudgetAndReplacementPreservation(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned-removal", true: "foreign-replacement"}[replace], func(t *testing.T) {
			cfg := usageReferenceBudgetFixture(t)
			// The archive's bounded catalog builder can overlap the references;
			// unlike online verification, this helper also walks the backup set.
			// Archive verification reserves a 256 MiB index and spool, inside
			// the archive tree ceiling as well as the shared Recovery pool.
			// These logical test ceilings do not preallocate those bytes.
			cfg.ResourceBudget.TotalRAMBytes += 512 << 20
			cfg.ResourceBudget.RecoveryRAMBytes += 512 << 20
			cfg.ResourceBudget.RecoveryReserveRAMBytes += 512 << 20
			cfg.ResourceBudget.TotalDiskBytes = 4 << 30
			cfg.ResourceBudget.UsageArchiveDiskBytes = 1 << 30
			cfg.ResourceBudget.BackgroundDiskBytes = 2 << 30
			cfg.ResourceBudget.RecoveryDiskBytes, cfg.ResourceBudget.RecoveryReserveDiskBytes = 2<<30, 2<<30
			ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			log, store, key := openSealingLedger(t, cfg)
			defer log.Close()
			defer store.Close()
			defer clear(key)
			report, partial, err := ledger.InspectReplayAuthenticated(cfg.LedgerPath(), key, nil)
			if err != nil || partial {
				t.Fatal(report, partial, err)
			}
			parent := t.TempDir()
			t.Setenv("TMPDIR", parent)
			var replaced string
			position, err := withBackupUsageReference(ctx, cfg, cfg.UsagePath(), cfg.LedgerPath(), key, report, cfg.ResourceBudget.UsageArchiveDiskBytes, pressure, func(_ *usage.Exporter, _ usage.ArchivePosition) error {
				s := pressure.logicalBudget().Snapshot()
				if s.Classes[resourcebudget.Recovery].RAM < 208<<20 || s.Classes[resourcebudget.Recovery].Disk < (1<<30)+(16<<20) {
					t.Fatal("backup references escaped shared pool", s)
				}
				if replace {
					paths, err := filepath.Glob(filepath.Join(parent, "halro-backup-usage-*", "usage.db"))
					if err != nil || len(paths) != 1 {
						t.Fatal(paths, err)
					}
					replaced = paths[0]
					if err := os.Rename(replaced, replaced+".owned"); err != nil {
						return err
					}
					return os.WriteFile(replaced, []byte("foreign bytes"), 0600)
				}
				return nil
			})
			s := pressure.logicalBudget().Snapshot()
			if replace {
				if err == nil || position != (usage.ArchivePosition{}) || s.Classes[resourcebudget.Recovery].RAM != 208<<20 || s.Classes[resourcebudget.Recovery].Disk != 16<<20 {
					t.Fatal("failed retirement refunded the job", position, err, s)
				}
				data, readErr := os.ReadFile(replaced)
				if readErr != nil || string(data) != "foreign bytes" {
					t.Fatal("foreign file removed", readErr)
				}
			} else {
				if err != nil || s.Used != (resourcebudget.Cost{}) {
					t.Fatal(err, s)
				}
				entries, readErr := os.ReadDir(parent)
				if readErr != nil || len(entries) != 0 {
					t.Fatal("owned scratch survived", entries, readErr)
				}
			}
		})
	}
}

func TestBackupGovernanceReferenceSharedBudgetAndNativePreservation(t *testing.T) {
	cfg := commandBudgetFixture(t)
	b := cfg.ResourceBudget
	b.GovernanceProjectionDiskBytes = 8 << 20
	b.RecoveryClaims++
	addProjectionMappingFixtureRAM(t, b)
	b.MaxClaims += 2
	b.ControlRAMBytes += 2 * resourcebudget.ClaimRAMBytes
	b.RecoveryReserveRAMBytes += resourcebudget.ClaimRAMBytes
	b.TotalRAMBytes += 2 * resourcebudget.ClaimRAMBytes
	ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	store, log, source, key := openGovernanceRecoveryFixture(t)
	defer store.Close()
	defer log.Close()
	first := governanceRecoveryEvent("out_budget", "accepted", "", "key-budget", 1)
	if _, err := log.AppendBatch(ctx, []governance.Event{first}); err != nil {
		t.Fatal(err)
	}
	head, auth := log.AuthenticatedHead()
	if err := store.PutGovernanceJournalAnchor(head.Records, head.Bytes, head.LastHash, auth); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	if err := validateBackupGovernanceProjection(ctx, cfg, store, log, source, key, head, parent); err != nil {
		t.Fatal(err)
	}
	if s := pressure.logicalBudget().Snapshot(); s.Used != (resourcebudget.Cost{}) {
		t.Fatal("owned Governance job survived", s)
	}
	occupied, err := pressure.logicalBudget().TryAcquireRecovery(resourcebudget.Cost{RAM: 140 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateBackupGovernanceProjection(ctx, cfg, store, log, source, key, head, parent); !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatal("unadmitted Governance read entered IO", err)
	}
	if err := occupied.Release(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(source)
	if err != nil || string(after) != string(before) {
		t.Fatal("reference changed native source", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
}

func TestBackupPricingBudgetRefusesBeforePrivateDirectory(t *testing.T) {
	cfg := usageReferenceBudgetFixture(t)
	ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	log, store, key := openSealingLedger(t, cfg)
	defer log.Close()
	defer store.Close()
	defer clear(key)
	report, partial, err := ledger.InspectReplayAuthenticated(cfg.LedgerPath(), key, nil)
	if err != nil || partial {
		t.Fatal(err)
	}
	occupied, err := pressure.logicalBudget().TryAcquireRecovery(resourcebudget.Cost{RAM: 100 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Release()
	parent := t.TempDir()
	if err := validateBackupPricingProjection(ctx, cfg, store, cfg.LedgerPath(), key, report, parent); !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatal("unadmitted backup scratch entered IO", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
}

func TestReferenceDirectoryReplacementAndUnknownEntryArePreserved(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "reference")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	identity, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(dir, "foreign")
	if err := os.WriteFile(unknown, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := removeOwnedReferenceDirectory(dir, identity); err == nil {
		t.Fatal("recursive removal accepted unknown entry")
	}
	if err := os.Rename(dir, dir+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := removeOwnedReferenceDirectory(dir, identity); err == nil {
		t.Fatal("replacement directory removed")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
}

func TestBudgetedBackupCreateReportVerifyAndRestoreShareOneRootManager(t *testing.T) {
	cfg := budgetedBackupOperationFixture(t)
	ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "report.hmbk")
	key := bytes.Repeat([]byte{0x48}, 32)
	var report bytes.Buffer
	if err := WriteCreateBackup(ctx, cfg, configPath, path, key, CreateBackupOptions{}, &report); err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		backup.ArchiveSummary
		Files []backup.File `json:"files"`
	}
	if err := json.Unmarshal(report.Bytes(), &parsed); err != nil || parsed.FormatVersion != 5 || uint64(len(parsed.Files)) != parsed.Catalog.Files || len(parsed.Files) < 5 {
		t.Fatal("created report lost complete inventory", err, parsed)
	}
	before := pressure.logicalBudget().Snapshot()
	if before.Classes[resourcebudget.Recovery].RAM >= 1<<20 {
		t.Fatal("report retained live scratch", before)
	}
	report.Reset()
	if err := WriteVerifyBackupWithConfig(ctx, cfg, path, key, &report); err != nil {
		t.Fatal(err)
	}
	after := pressure.logicalBudget().Snapshot()
	if after.Acquired[resourcebudget.Recovery] < before.Acquired[resourcebudget.Recovery]+3 || after.Classes[resourcebudget.Recovery].RAM >= 1<<20 {
		t.Fatal("later report used another manager or leaked scratch", before, after)
	}
	out, err := RestoreBackup(ctx, cfg, path, key, parsed.BackupID)
	if err != nil || out.BackupID != parsed.BackupID {
		t.Fatal("paged budgeted restore", out, err)
	}
	if s := pressure.logicalBudget().Snapshot(); s.Classes[resourcebudget.Recovery].RAM >= 1<<20 {
		t.Fatal("restore retained live reference/index RAM", s)
	}
}
