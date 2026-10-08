package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/vault"
)

func TestBackupPricingProjectionUsesFrozenReportAndRemovesTemporaryTree(t *testing.T) {
	for _, name := range []string{"empty", "rolled empty active", "wide identities", "changed report", "partial source", "canceled", "closed metadata"} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(t)
			if err := Initialize(cfg); err != nil {
				t.Fatal(err)
			}
			log, metadata, key := openSealingLedger(t, cfg)
			defer clear(key)
			defer metadata.Close()
			if name != "empty" {
				now := time.Now().UTC()
				event := ledger.Event{Kind: ledger.EventRequestAccepted, EventID: "accepted_backup_projection", RequestID: "req_backup_projection", ProjectID: "prj_backup_projection", PeriodID: "2026-10-03", PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, OccurredAt: now}
				if name == "wide identities" {
					event.EventID = strings.Repeat("e", 32769)
					event.RequestID = strings.Repeat("r", 32769)
					event.ProjectID = strings.Repeat("p", 32769)
				}
				if _, err := log.Append(context.Background(), event); err != nil {
					t.Fatal(err)
				}
				if _, err := log.Roll(); err != nil {
					t.Fatal(err)
				}
			}
			if err := log.Close(); err != nil {
				t.Fatal(err)
			}
			report, partial, err := ledger.InspectReplayAuthenticated(cfg.LedgerPath(), key, nil)
			if err != nil || partial {
				t.Fatal(report, partial, err)
			}
			parent := t.TempDir()
			ctx := context.Background()
			switch name {
			case "changed report":
				report.Head.Sequence++
			case "partial source":
				f, err := os.OpenFile(cfg.LedgerPath(), os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, writeErr := f.Write([]byte{0x05})
				if err := errors.Join(writeErr, f.Close()); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "closed metadata":
				if err := metadata.Close(); err != nil {
					t.Fatal(err)
				}
			}
			err = validateBackupPricingProjection(ctx, cfg, metadata, cfg.LedgerPath(), key, report, parent)
			if name == "empty" || name == "rolled empty active" || name == "wide identities" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("invalid staged cut accepted")
			}
			if name == "changed report" && !errors.Is(err, ledger.ErrProjectionCorrupt) {
				t.Fatal(err)
			}
			if name == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			entries, readErr := os.ReadDir(parent)
			if readErr != nil {
				t.Fatal(readErr)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".halro-pricing-check-") {
					t.Fatal("temporary projection retained", entry.Name(), err)
				}
			}
			if len(entries) != 0 {
				t.Fatal("temporary files survived validation", entries)
			}
			if _, statErr := os.Stat(filepath.Join(cfg.Storage.DataDir, "ledger.db")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("derived cache published into data tree", statErr)
			}
		})
	}
}

func TestBackupRestorePreservesWideNativeLedgerIdentities(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	log, metadata, key := openSealingLedger(t, cfg)
	root := filepath.Dir(cfg.Storage.DataDir)
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	event := ledger.Event{Kind: ledger.EventRequestAccepted, EventID: strings.Repeat("e", 32769), RequestID: strings.Repeat("r", 32769), ProjectID: strings.Repeat("p", 32769), PeriodID: "2026-10-03", PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, OccurredAt: now}
	if _, err := log.Append(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Roll(); err != nil {
		t.Fatal(err)
	}
	masterKey, err := vault.LoadMasterKey(cfg.Storage.MasterKey.File)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := keyFingerprint(masterKey)
	clear(masterKey)
	archive := filepath.Join(root, "wide.hmbk")
	backupKey := bytes.Repeat([]byte{0x62}, 32)
	manifest, backupErr := createBackupSnapshotWithLedger(context.Background(), cfg, configPath, archive, backupKey, metadata, fingerprint, log, key, nil, nil)
	clear(key)
	if err := errors.Join(backupErr, log.Close(), metadata.Close()); err != nil {
		t.Fatal(err)
	}
	for _, file := range manifest.Files {
		if strings.Contains(file.Path, "pricing-check") || strings.HasSuffix(file.Path, "ledger.db") {
			t.Fatal("derived projection archived", file.Path)
		}
	}
	if _, err := RestoreBackup(context.Background(), cfg, archive, backupKey, manifest.BackupID); err != nil {
		t.Fatal(err)
	}
	ids := replayEventIDs(t, cfg.LedgerPath())
	if len(ids) != 1 || ids[0] != event.EventID {
		t.Fatal("wide native identity not preserved")
	}
	if err := filepath.WalkDir(cfg.Storage.DataDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.Contains(entry.Name(), "pricing-check") || entry.Name() == "ledger.db" {
			return errors.New("derived pricing projection published")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
