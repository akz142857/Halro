package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/vault"
)

// Sealing, seen from outside the ledger package: what an operator's tools say
// about an instance whose accounting history is no longer one file.
//
// The unit tests in internal/ledger prove the chain, the replay and the roll
// recovery. These prove the two paths that would silently lose that history
// instead of failing on it — the backup, and the verify command that is meant
// to certify the backup was worth taking.

func openSealingLedger(t *testing.T, cfg config.Config) (*ledger.Log, *boltstore.Store, []byte) {
	t.Helper()
	metadata, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	masterKey, err := vault.LoadMasterKey(cfg.Storage.MasterKey.File)
	if err != nil {
		metadata.Close()
		t.Fatal(err)
	}
	secretVault, err := vault.New(masterKey)
	if err != nil {
		clear(masterKey)
		metadata.Close()
		t.Fatal(err)
	}
	ledgerKey, err := loadLedgerHMACKey(metadata, secretVault, masterKey)
	secretVault.Close()
	clear(masterKey)
	if err != nil {
		metadata.Close()
		t.Fatal(err)
	}
	log, err := ledger.OpenWithOptions(cfg.LedgerPath(), ledger.NewStatus(), ledger.Options{
		QueueCapacity: 256, MaxBatch: 1, ChainKey: ledgerKey,
	})
	if err != nil {
		clear(ledgerKey)
		metadata.Close()
		t.Fatal(err)
	}
	return log, metadata, ledgerKey
}

func appendSealingEvents(t *testing.T, log *ledger.Log, from, count int) {
	t.Helper()
	for index := range count {
		if _, err := log.Append(context.Background(), ledger.Event{
			EventID: fmt.Sprintf("seal_evt_%03d", from+index), Kind: ledger.EventRequestAccepted,
			RequestID: fmt.Sprintf("seal_req_%03d", from+index), ProjectID: "project_seal",
			PeriodID: "project_seal:2026-09-01:UTC", OccurredAt: time.Now().UTC(),
			PeriodTimezone: "UTC", PeriodTimezoneVersion: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func replayEventIDs(t *testing.T, path string) []string {
	t.Helper()
	log, err := ledger.Open(path, ledger.NewStatus())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	_, replayErr := log.Replay(ledger.Watermark{}, func(record ledger.Record) error {
		ids = append(ids, record.Event.EventID)
		return nil
	})
	closeErr := log.Close()
	if err := errors.Join(replayErr, closeErr); err != nil {
		t.Fatal(err)
	}
	return ids
}

// A backup taken after a roll restores the whole history, and `ledger verify`
// still authenticates all of it.
//
// Both halves matter. Staging only the active file would produce an archive
// that passes every check it currently has and restores an instance whose
// balances begin at the last roll; verifying only the active file would then
// certify that instance as sound.
func TestABackupTakenAfterASealRestoresEveryGeneration(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(cfg.Storage.DataDir)
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	log, metadata, ledgerKey := openSealingLedger(t, cfg)

	appendSealingEvents(t, log, 1, 5)
	if _, err := log.Roll(); err != nil {
		t.Fatal(err)
	}
	appendSealingEvents(t, log, 6, 4)
	if _, err := log.Roll(); err != nil {
		t.Fatal(err)
	}
	appendSealingEvents(t, log, 10, 2)
	// One generation compressed and one not, so the archive has to carry both
	// storage forms.
	if _, err := log.Compact(1); err != nil {
		t.Fatal(err)
	}
	source := replayEventIDs(t, cfg.LedgerPath())
	if len(source) != 11 {
		t.Fatalf("the source ledger replays %d records, want 11", len(source))
	}

	masterKey, err := vault.LoadMasterKey(cfg.Storage.MasterKey.File)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := keyFingerprint(masterKey)
	clear(masterKey)
	archivePath := filepath.Join(root, "sealed.hmbk")
	backupKey := bytes.Repeat([]byte{0x5e}, 32)
	manifest, err := createBackupSnapshotWithLedger(
		context.Background(), cfg, configPath, archivePath, backupKey,
		metadata, fingerprint, log, ledgerKey,
	)
	clear(ledgerKey)
	closeErr := errors.Join(log.Close(), metadata.Close())
	if err := errors.Join(err, closeErr); err != nil {
		t.Fatal(err)
	}
	if manifest.LedgerWatermark.Generation != 3 || manifest.LedgerWatermark.Sequence != 11 {
		t.Fatalf("manifest watermark = %#v", manifest.LedgerWatermark)
	}

	if _, err := RestoreBackup(
		context.Background(), cfg, archivePath, backupKey, manifest.BackupID,
	); err != nil {
		t.Fatal(err)
	}
	ledgerDirectory := filepath.Dir(cfg.LedgerPath())
	for _, name := range []string{"ledger-1.seg.gz", "ledger-2.wal", "segments.json"} {
		if _, err := os.Stat(filepath.Join(ledgerDirectory, name)); err != nil {
			t.Fatalf("the restored data directory is missing %s: %v", name, err)
		}
	}
	restored := replayEventIDs(t, cfg.LedgerPath())
	if !slices.Equal(restored, source) {
		t.Fatalf("the restored ledger replays differently:\nsource   %v\nrestored %v", source, restored)
	}

	// And the command an operator would run to be told the restore is sound
	// counts the sealed generations rather than skipping them.
	report, err := VerifyLedger(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if report.SealedGenerations != 2 || report.SealedAuthenticated != 9 || report.Authenticated != 2 {
		t.Fatalf("verify report = %#v", report)
	}
	if !report.ChainVerified {
		t.Fatal("a restored sealed ledger reports its chain as unverified")
	}
}

// A generation removed from a restored directory is caught rather than read
// past. This is the failure that looks like housekeeping — the archive is
// intact, the active file verifies, and nine records of accounting are gone.
func TestVerifyRefusesALedgerWithAGenerationRemoved(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	log, metadata, ledgerKey := openSealingLedger(t, cfg)
	appendSealingEvents(t, log, 1, 3)
	if _, err := log.Roll(); err != nil {
		t.Fatal(err)
	}
	appendSealingEvents(t, log, 4, 2)
	clear(ledgerKey)
	if err := errors.Join(log.Close(), metadata.Close()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(filepath.Dir(cfg.LedgerPath()), "ledger-1.wal")); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyLedger(context.Background(), cfg); !errors.Is(err, ledger.ErrSegmentMissing) {
		t.Fatalf("verify with a removed generation: err=%v, want ErrSegmentMissing", err)
	}
	// And the instance refuses to open rather than starting with a short
	// history, because a missing generation is not something a start can repair.
	if _, err := ledger.Open(cfg.LedgerPath(), ledger.NewStatus()); err == nil {
		t.Fatal("a ledger with a missing generation opened")
	}
}
