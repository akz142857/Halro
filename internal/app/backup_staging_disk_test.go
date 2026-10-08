package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestBackupStagingOwnerUsesOneCommandManagerAndKeepsPublishedTreeFee(t *testing.T) {
	cfg := budgetedBackupOperationFixture(t)
	ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := prepareBackupStagingDisk(ctx, cfg, pressure)
	if err != nil {
		t.Fatal(err)
	}
	if same, err := prepareBackupStagingDisk(ctx, cfg, pressure); err != nil || same != owner {
		t.Fatal("staging replaced original aggregate manager/owner", same, err)
	}
	manager := pressure.logicalBudget()
	before := manager.Snapshot()
	if before.Used != (resourcebudget.Cost{RAM: backupStagingRAMBytes + resourcebudget.ClaimRAMBytes, Disk: cfg.ResourceBudget.BackupWorkspaceDiskBytes}) || before.RecoveryClaims != 1 {
		t.Fatal("staging did not prepay fixed shared capacity", before)
	}
	source, destination := filepath.Join(t.TempDir(), "source.journal"), filepath.Join(t.TempDir(), "staged.journal")
	data := bytes.Repeat([]byte("actual private journal"), 4096)
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := copyFileForBackupWithDisk(ctx, source, destination, owner); err != nil {
		t.Fatal(err)
	}
	consumed := owner.quota.consumed()
	if consumed <= int64(len(data)) {
		t.Fatal("actual whole output envelope not charged")
	}
	actual, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatal("actual bounded journal differs", err)
	}
	if err := os.Remove(destination); err != nil {
		t.Fatal(err)
	}
	if err := owner.finish(nil); err != nil {
		t.Fatal(err)
	}
	if owner.quota.consumed() != consumed || manager.Snapshot().Used != (resourcebudget.Cost{RAM: resourcebudget.ClaimRAMBytes, Disk: cfg.ResourceBudget.BackupWorkspaceDiskBytes}) {
		t.Fatal("missing output name refunded persistent staging obligation", manager.Snapshot(), owner.quota.consumed())
	}
	if _, err := owner.reserve(1); !errors.Is(err, resourcebudget.ErrClosed) {
		t.Fatal("completed command staging admitted another IO", err)
	}
}

func TestBackupStagingQuotaRejectsActualCopyBeforeFileCreation(t *testing.T) {
	cfg := budgetedBackupOperationFixture(t)
	cfg.ResourceBudget.BackupWorkspaceDiskBytes = 64 << 10
	ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := prepareBackupStagingDisk(ctx, cfg, pressure)
	if err != nil {
		t.Fatal(err)
	}
	source, destination := filepath.Join(t.TempDir(), "source.journal"), filepath.Join(t.TempDir(), "staged.journal")
	data := bytes.Repeat([]byte("x"), 64<<10)
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := copyFileForBackupWithDisk(ctx, source, destination, owner); !errors.Is(err, errBackupStagingDiskLimit) {
		t.Fatal("unfunded journal output accepted", err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatal("unfunded copy created file", err)
	}
	if actual, err := os.ReadFile(source); err != nil || !bytes.Equal(actual, data) {
		t.Fatal("quota refusal changed real source", err)
	}
	if owner.quota.consumed() != 0 {
		t.Fatal("definite precreate refusal consumed counter")
	}
}

func TestBackupStagingActualCreateSharesCounterAndRetainsFeeAfterCleanup(t *testing.T) {
	cfg := budgetedBackupOperationFixture(t)
	ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	archive := filepath.Join(root, "shared-staging.hmbk")
	manifest, err := CreateBackup(ctx, cfg, configPath, archive, bytes.Repeat([]byte{0x69}, 32))
	if err != nil {
		t.Fatal("actual command backup", err)
	}
	if manifest.BackupID == "" {
		t.Fatal("missing actual authenticated backup identity")
	}
	scope := ctx.Value(commandResourceScopeKey{}).(*commandResourceScope)
	owner := scope.backupStaging
	if owner == nil || owner.quota.consumed() < 4*(64<<10) || !owner.quota.closed || !owner.workRetired {
		t.Fatal("actual Metadata/journal/Ledger/root bypassed bounded staging aggregate", owner)
	}
	output := scope.backupOutput
	if output == nil || output.quota == owner.quota || output.quota.consumed() != cfg.ResourceBudget.BackupWorkspaceDiskBytes || !output.quota.closed || !output.workRetired {
		t.Fatal("actual encrypted archive borrowed staging capacity or lost published owner", output)
	}
	if snapshot := pressure.logicalBudget().Snapshot(); snapshot.Used.Disk < cfg.ResourceBudget.BackupWorkspaceDiskBytes {
		t.Fatal("successful RemoveAll refunded root staging claim", snapshot)
	}
	names, err := os.ReadDir(root)
	if err != nil || len(names) != 1 || names[0].Name() != filepath.Base(archive) {
		t.Fatal("actual stage was not cleaned", names, err)
	}
}

func TestBackupStagingActualCapacityFailurePreservesNativeAndArchiveAbsence(t *testing.T) {
	cfg := budgetedBackupOperationFixture(t)
	cfg.ResourceBudget.BackupWorkspaceDiskBytes = 64 << 10
	ctx, _, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(cfg.LedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "refused.hmbk")
	_, err = CreateBackup(ctx, cfg, configPath, archive, bytes.Repeat([]byte{0x69}, 32))
	if !errors.Is(err, errBackupStagingDiskLimit) {
		t.Fatal("unfunded complete staging command accepted", err)
	}
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Fatal("unfunded staging published archive", err)
	}
	if actual, err := os.ReadFile(cfg.LedgerPath()); err != nil || !bytes.Equal(actual, before) {
		t.Fatal("staging refusal changed native Ledger", err)
	}
	var debt *backupStagingDebt
	if !errors.As(err, &debt) || debt.owner == nil || debt.owner.quota.consumed() != 64<<10 {
		t.Fatal("actual command failed to preserve root directory capacity owner", err)
	}
}

func TestBackupStagingReplacedRootPreservesForeignTreeAndUnknownFee(t *testing.T) {
	cfg := budgetedBackupOperationFixture(t)
	ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	log, store, key := openSealingLedger(t, cfg)
	defer log.Close()
	defer store.Close()
	defer clear(key)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	var original, retained string
	injected := errors.New("writer failed after private-root replacement")
	writer := func(options backup.CreateOptions) (backup.Manifest, error) {
		if err := options.SourceCatalog.Walk(ctx, func(source backup.SourceFile) error {
			if source.ArchivePath == "data/metadata.db" {
				original = filepath.Dir(source.LocalPath)
			}
			return nil
		}); err != nil {
			return backup.Manifest{}, err
		}
		if original == "" {
			return backup.Manifest{}, errors.New("actual Metadata stage absent")
		}
		retained = original + ".retained"
		if err := os.Rename(original, retained); err != nil {
			return backup.Manifest{}, err
		}
		if err := os.Mkdir(original, 0700); err != nil {
			return backup.Manifest{}, err
		}
		if err := os.WriteFile(filepath.Join(original, "foreign"), []byte("preserve"), 0600); err != nil {
			return backup.Manifest{}, err
		}
		return backup.Manifest{}, injected
	}
	_, err = createBackupSnapshotWithLedgerWriter(ctx, cfg, configPath, filepath.Join(root, "refused.hmbk"), bytes.Repeat([]byte{0x69}, 32), store, "test-fingerprint", log, key, nil, nil, writer, pressure)
	if !errors.Is(err, injected) || !errors.Is(err, ownedfile.ErrRetirement) || !errors.Is(err, ownedfile.ErrReplaced) {
		t.Fatal("private-root replacement did not retain retirement debt", err)
	}
	if actual, err := os.ReadFile(filepath.Join(original, "foreign")); err != nil || string(actual) != "preserve" {
		t.Fatal("cleanup removed foreign tree", err)
	}
	if _, err := os.Stat(filepath.Join(retained, "metadata.db")); err != nil {
		t.Fatal("cleanup discarded original actual staging inode", err)
	}
	var debt *backupStagingDebt
	if !errors.As(err, &debt) || debt.owner == nil || debt.owner.workRetired || !debt.owner.quota.closed {
		t.Fatal("unknown namespace released its actual fixed workspace", err)
	}
	if pressure.logicalBudget().Snapshot().Used.Disk < cfg.ResourceBudget.BackupWorkspaceDiskBytes {
		t.Fatal("unknown stage refunded fixed persistent disk claim")
	}
}

func TestBackupStagingAuthenticatingSealedSnapshotChargesDerivedSidecar(t *testing.T) {
	cfg := budgetedBackupOperationFixture(t)
	log, store, key := openSealingLedger(t, cfg)
	if result, err := log.Roll(); err != nil || !result.Rolled {
		t.Fatal("seal real source fixture", result, err)
	}
	if err := errors.Join(log.Close(), store.Close()); err != nil {
		t.Fatal(err)
	}
	clear(key)
	ctx, _, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = CreateBackup(ctx, cfg, configPath, filepath.Join(t.TempDir(), "sealed.hmbk"), bytes.Repeat([]byte{0x69}, 32))
	if err != nil {
		t.Fatal("actual sealed command backup", err)
	}
	owner := ctx.Value(commandResourceScopeKey{}).(*commandResourceScope).backupStaging
	// Root, Metadata, Journal, active snapshot, sealed payload, manifest and
	// the authenticated snapshot Log's derived persistent locator are all
	// distinct actual products. The locator is not exported in the archive.
	if owner == nil || owner.quota.consumed() < 7*(64<<10) || !owner.quota.closed {
		t.Fatal("snapshot authenticator bypassed staging sidecar admission", owner)
	}
}
