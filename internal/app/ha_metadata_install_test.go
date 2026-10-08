package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

func prepareHAMetadataInstallSource(t *testing.T) (haMetadataMaintenanceFixture, string) {
	t.Helper()
	f := prepareHAMetadataMaintenanceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cfgPath, archivePath := filepath.Join(t.TempDir(), "config.yaml"), filepath.Join(t.TempDir(), "source.hmbk")
	if err := os.WriteFile(cfgPath, []byte("version: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	backupKey := bytes.Repeat([]byte{0x53}, 32)
	if _, err := CreateBackupWithOptions(ctx, configWithoutBudget(f.configs[1]), cfgPath, archivePath, backupKey, CreateBackupOptions{Replica: true}); err != nil {
		t.Fatal(err)
	}
	archive, err := backup.OpenVerified(ctx, archivePath, backupKey)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	guard, err := lock.Acquire(f.configs[0].Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	source := filepath.Join(f.configs[0].Storage.DataDir, HAMetadataBaselineDirectory)
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := f.configs[0]
	preparation, err := PrepareHAMetadataBaseline(ctx, cfg, f.keys, archive, source, guard, metadatastate.BaselineOptions{Identity: sha256.Sum256([]byte("actual-ha-install-source")), InstanceID: f.instance, KeyGeneration: 1, Budget: functionalHAMetadataManager(t, cfg), MaxSourceBytes: cfg.ResourceBudget.MetadataStoreDiskBytes, MaxDiskBytes: cfg.ResourceBudget.MetadataStoreDiskBytes, MaxAppendBytes: min(8<<20, cfg.ResourceBudget.MetadataStoreDiskBytes/2)})
	if err != nil {
		t.Fatal(err)
	}
	if err := preparation.Close(); err != nil {
		t.Fatal(err)
	}
	return f, source
}

func cloneHAMetadataInstallTarget(t *testing.T, cfg config.Config) config.Config {
	t.Helper()
	root := t.TempDir()
	clone := cfg
	clone.Storage.DataDir = filepath.Join(root, "data")
	clone.Storage.MasterKey.File = filepath.Join(root, "master.key")
	copyTestTree(t, cfg.Storage.DataDir, clone.Storage.DataDir)
	copyTestRegularFile(t, cfg.Storage.MasterKey.File, clone.Storage.MasterKey.File)
	return clone
}

func TestHAMetadataInstallActualQualificationAndRefusals(t *testing.T) {
	f, source := prepareHAMetadataInstallSource(t)
	ctx := context.Background()
	t.Run("actual-copied-root-and-same-manager-loan", func(t *testing.T) {
		cfg := cloneHAMetadataInstallTarget(t, f.configs[1])
		guard, err := lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		defer guard.Close()
		manager := functionalHAMetadataManager(t, cfg)
		installed, err := InstallHAMetadataBaseline(ctx, cfg, f.keys, source, guard, manager)
		if err != nil {
			t.Fatal(err)
		}
		defer installed.Close()
		if got := manager.Snapshot(); got.Classes[resourcebudget.Recovery].RAM != 0 || got.Used.Disk != cfg.ResourceBudget.MetadataStoreDiskBytes+cfg.ResourceBudget.MetadataJournalDiskBytes+maxHAMetadataCarrierBytes {
			t.Fatal("installer fees do not match persistent copied capacity", got)
		}
		for i := 0; i < 5; i++ {
			want, err := os.ReadFile(haMetadataInstalledFile(source, i))
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(haMetadataInstalledFile(installed.Path, i))
			if err != nil || !bytes.Equal(want, got) {
				t.Fatal("copied file differs", i, err)
			}
		}
		before := manager.Snapshot().Used
		if _, err := InstallHAMetadataBaseline(ctx, cfg, f.keys, source, guard, manager); !errors.Is(err, os.ErrExist) {
			t.Fatal("existing published namespace was not refused", err)
		}
		if manager.Snapshot().Used != before {
			t.Fatal("refusal leaked fees")
		}
		if err := installed.Discard(); err == nil {
			t.Fatal("published borrowed files could be discarded")
		}
		rootFee, journalFee, err := installed.DiskReservations()
		if err != nil {
			t.Fatal(err)
		}
		if manager.Snapshot().Used.Disk != before.Disk {
			t.Fatal("retained loan double charged capacity")
		}
		maintenance, err := openHAMetadataMaintenance(ctx, cfg, f.keys, guard, manager, slog.New(slog.NewTextHandler(io.Discard, nil)), rootFee, journalFee)
		if err != nil {
			t.Fatal("actual maintenance cannot open installed qualified root", err)
		}
		if err := maintenance.Close(); err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(rootFee.Release(), journalFee.Release()); err != nil {
			t.Fatal(err)
		}
	})
	for _, mutation := range []string{"carrier-tamper", "root-page-tamper", "journal-symlink", "foreign-stage", "cancel-after-copy", "cancel-after-qualification"} {
		t.Run(mutation, func(t *testing.T) {
			cfg := cloneHAMetadataInstallTarget(t, f.configs[1])
			guard, err := lock.Acquire(cfg.Storage.DataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			manager := functionalHAMetadataManager(t, cfg)
			candidate := filepath.Join(t.TempDir(), "source")
			copyTestTree(t, source, candidate)
			var phase func(string) error
			switch mutation {
			case "carrier-tamper", "root-page-tamper":
				index := 0
				if mutation == "root-page-tamper" {
					index = 2
				}
				path := haMetadataInstalledFile(candidate, index)
				raw, err := os.ReadFile(path)
				if err != nil || len(raw) == 0 {
					t.Fatal(err)
				}
				raw[len(raw)/2] ^= 0x40
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "journal-symlink":
				path := haMetadataInstalledFile(candidate, 1)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(haMetadataInstalledFile(source, 1), path); err != nil {
					t.Fatal(err)
				}
			case "foreign-stage":
				stage := filepath.Join(cfg.Storage.DataDir, HAMetadataBaselineDirectory) + ".preparing"
				if err := os.Mkdir(stage, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(stage, "foreign"), []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			case "cancel-after-copy", "cancel-after-qualification":
				at := "install-file-2-synced"
				if mutation == "cancel-after-qualification" {
					at = "install-qualified-closed"
				}
				phase = func(s string) error {
					if s == at {
						return context.Canceled
					}
					return nil
				}
			}
			installation, err := installHAMetadataBaseline(ctx, cfg, f.keys, candidate, guard, manager, phase)
			if err == nil {
				t.Fatal("unqualified/cancelled install succeeded")
			}
			if installation != nil {
				t.Fatal("known joined refusal retained cleanup owner", err)
			}
			if got := manager.Snapshot(); got.Used.Disk != 0 || got.Used.RAM != 0 {
				t.Fatal("known joined refusal leaked resources", got)
			}
			final := filepath.Join(cfg.Storage.DataDir, HAMetadataBaselineDirectory)
			if _, err := os.Lstat(final); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("refusal published target", err)
			}
			stage := final + ".preparing"
			if mutation == "foreign-stage" {
				raw, err := os.ReadFile(filepath.Join(stage, "foreign"))
				if err != nil || string(raw) != "preserve" {
					t.Fatal("foreign namespace changed", err)
				}
			} else if _, err := os.Lstat(stage); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("known created staging was not retired", err)
			}
		})
	}
}
