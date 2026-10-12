package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/ownedfile"
)

// F1 (board §40.15, §40.16): a backup that fails with retirement debt keeps
// its staging directory by design. This process keeps it; the next backup of
// the same data directory from another process reclaims it, and a staging
// directory of another data directory, or without an owner record, is kept.
func TestBackupReclaimsStagingRetainedByAnExitedProcess(t *testing.T) {
	cfg := budgetedBackupOperationFixture(t)
	ctx, pressure, err := commandBudgetContext(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	dataDir, err := filepath.Abs(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backupKey := bytes.Repeat([]byte{0x69}, 32)
	root := t.TempDir()

	log, store, key := openSealingLedger(t, cfg)
	var staging string
	injected := errors.Join(errors.New("injected bbolt Open failure"), ownedfile.ErrRetirement)
	writer := func(options backup.CreateOptions) (backup.Manifest, error) {
		if err := options.SourceCatalog.Walk(ctx, func(source backup.SourceFile) error {
			if source.ArchivePath == "data/metadata.db" {
				staging = filepath.Dir(source.LocalPath)
			}
			return nil
		}); err != nil {
			return backup.Manifest{}, err
		}
		return backup.Manifest{}, injected
	}
	_, err = createBackupSnapshotWithLedgerWriter(ctx, cfg, configPath, filepath.Join(root, "failed.hmbk"), backupKey, store, "test-fingerprint", log, key, nil, nil, writer, pressure)
	clear(key)
	if err := errors.Join(log.Close(), store.Close()); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(err, ownedfile.ErrRetirement) || staging == "" {
		t.Fatal("injected failure did not retain staging", staging, err)
	}
	if _, err := os.Stat(filepath.Join(staging, "metadata.db")); err != nil {
		t.Fatal("retained staging is absent", err)
	}

	if removed, err := reclaimBackupStaging(root, dataDir); err != nil || removed != 0 {
		t.Fatal("this process reclaimed its own retained staging", removed, err)
	}
	if _, err := os.Stat(staging); err != nil {
		t.Fatal(err)
	}

	// Kept: another data directory's staging, and one with no owner record.
	foreign := filepath.Join(root, ".halro-backup-stage-1")
	unowned := filepath.Join(root, ".halro-backup-stage-2")
	for _, directory := range []string{foreign, unowned} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.Marshal(backupStagingOwner{DataDir: filepath.Join(root, "other-data"), Process: "exited"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreign, backupStagingOwnerName), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	// A later process has a different token: rewrite the record as it would see it.
	marker := filepath.Join(staging, backupStagingOwnerName)
	raw, err = os.ReadFile(marker)
	if err != nil {
		t.Fatal("staging has no owner record", err)
	}
	var owner backupStagingOwner
	if err := json.Unmarshal(raw, &owner); err != nil || owner.DataDir != dataDir || owner.Process == "" {
		t.Fatal("staging owner record", string(raw), err)
	}
	owner.Process = "exited"
	if raw, err = json.Marshal(owner); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	archive := filepath.Join(root, "after.hmbk")
	if _, err := CreateBackup(t.Context(), cfg, configPath, archive, backupKey); err != nil {
		t.Fatal("backup after a retained staging", err)
	}
	if _, err := os.Lstat(staging); !os.IsNotExist(err) {
		t.Fatal("next backup did not reclaim the exited process's staging", err)
	}
	for _, directory := range []string{foreign, unowned} {
		if _, err := os.Stat(directory); err != nil {
			t.Fatal("reclaim removed staging it cannot prove", directory, err)
		}
	}
	if _, err := VerifyBackup(archive, backupKey); err != nil {
		t.Fatal(err)
	}
}
