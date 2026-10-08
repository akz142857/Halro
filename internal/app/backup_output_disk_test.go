package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestBackupOutputDistinctPhysicalFilePrepaysSameCommandManager(t *testing.T) {
	cfg := budgetedBackupOperationFixture(t)
	ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := prepareBackupStagingDisk(ctx, cfg, pressure)
	if err != nil {
		t.Fatal(err)
	}
	before := pressure.logicalBudget().Snapshot().Used
	output, err := prepareBackupOutputDisk(ctx, cfg, pressure)
	if err != nil {
		t.Fatal(err)
	}
	if output.quota == stage.quota {
		t.Fatal("distinct actual archive borrowed staged-payload claim")
	}
	if same, err := prepareBackupOutputDisk(ctx, cfg, pressure); err != nil || same != output {
		t.Fatal("nested output substituted original manager/owner", err)
	}
	after := pressure.logicalBudget().Snapshot().Used
	if after.Disk-before.Disk != cfg.ResourceBudget.BackupWorkspaceDiskBytes || after.RAM-before.RAM != backup.OutputWorkspaceRAMBytes+resourcebudget.ClaimRAMBytes {
		t.Fatal("actual output has no independent Disk/workspace claim", before, after)
	}
	if err := output.finish(nil); err != nil {
		t.Fatal(err)
	}
	final := pressure.logicalBudget().Snapshot().Used
	if final.Disk != after.Disk || final.RAM != after.RAM-backup.OutputWorkspaceRAMBytes {
		t.Fatal("known output exit refunded persistent file fee", final)
	}
}

func TestBackupOutputActualEncryptedCapacityFailureKeepsScopeAndRetiresOnlyOwnTemp(t *testing.T) {
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
	archive := filepath.Join(root, "refused.hmbk")
	_, err = createBackupWithWriter(ctx, cfg, configPath, archive, bytes.Repeat([]byte{0x69}, 32), CreateBackupOptions{}, func(options backup.CreateOptions) (backup.Manifest, error) {
		// Tighten the actual encrypted producer while retaining its original
		// scope provider/Manager. Even AEAD records must fit before each Write.
		options.OutputDiskBytes = 64
		return backup.Create(options)
	})
	if !errors.Is(err, backup.ErrOutputDiskLimit) {
		t.Fatal("unfunded ciphertext accepted", err)
	}
	var debt *backupOutputDiskFailure
	if !errors.As(err, &debt) || debt.owner == nil || !debt.owner.workRetired || !debt.owner.quota.closed || debt.owner.quota.consumed() != 0 {
		t.Fatal("known failed ciphertext lost original provider or reused live inode credit", err)
	}
	if used := pressure.logicalBudget().Snapshot().Used.Disk; used < 2*cfg.ResourceBudget.BackupWorkspaceDiskBytes {
		t.Fatal("archive failure erased independent aggregate obligations", used)
	}
	names, readErr := os.ReadDir(root)
	if readErr != nil || len(names) != 0 {
		t.Fatal("known failed ciphertext or staging still exists", names, readErr)
	}
}
