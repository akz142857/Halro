package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/backup"
)

func TestRestoreRejectsNewAuthenticatedArchiveAfterConfirmation(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{32}, 32)
	firstPath, secondPath := filepath.Join(root, "first.hmbk"), filepath.Join(root, "second.hmbk")
	first, err := CreateBackup(context.Background(), cfg, configPath, firstPath, key)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CreateBackup(context.Background(), cfg, configPath, secondPath, key)
	if err != nil {
		t.Fatal(err)
	}
	if first.BackupID == second.BackupID {
		t.Fatal("fixture IDs must differ")
	}
	before, err := os.Stat(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	result, err := restoreBackupWithExtract(context.Background(), cfg, firstPath, key, first.BackupID, RestoreOptions{}, defaultKMSWrapperFactory, func(path string, key []byte, destination string) (backup.Manifest, error) {
		if err := os.Rename(secondPath, path); err != nil {
			return backup.Manifest{}, err
		}
		return backup.Extract(path, key, destination)
	})
	if err == nil || !strings.Contains(err.Error(), "changed after restore confirmation") || result.BackupID != "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	after, err := os.Stat(cfg.Storage.DataDir)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("live data directory changed", err)
	}
	stages, err := filepath.Glob(filepath.Join(filepath.Dir(cfg.Storage.DataDir), ".halro-restore-stage-*"))
	if err != nil || len(stages) != 0 {
		t.Fatal("failed restore leaked staging", stages, err)
	}
}
