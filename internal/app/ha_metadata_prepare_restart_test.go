package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/store/lock"
)

func haPrepareRestartArchive(t *testing.T, f haMetadataMaintenanceFixture) (string, []byte) {
	t.Helper()
	cfgPath, archive := filepath.Join(t.TempDir(), "config.yaml"), filepath.Join(t.TempDir(), "source.hmbk")
	if err := os.WriteFile(cfgPath, []byte("version: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x61}, 32)
	if _, err := CreateBackupWithOptions(context.Background(), configWithoutBudget(f.configs[1]), cfgPath, archive, key, CreateBackupOptions{Replica: true}); err != nil {
		t.Fatal(err)
	}
	return archive, key
}

func haPrepareRestartOptions(cfg config.Config, f haMetadataMaintenanceFixture, t *testing.T) metadatastate.BaselineOptions {
	return metadatastate.BaselineOptions{Identity: sha256.Sum256([]byte("actual-ha-restart-source")), InstanceID: f.instance, KeyGeneration: 1, Budget: functionalHAMetadataManager(t, cfg), MaxSourceBytes: cfg.ResourceBudget.MetadataStoreDiskBytes, MaxDiskBytes: cfg.ResourceBudget.MetadataStoreDiskBytes, MaxAppendBytes: min(8<<20, cfg.ResourceBudget.MetadataStoreDiskBytes/2)}
}

func TestHAMetadataPrepareRestartAuthenticatesBeforePreserving(t *testing.T) {
	f := prepareHAMetadataMaintenanceFixture(t)
	archivePath, backupKey := haPrepareRestartArchive(t, f)
	for _, scenario := range []string{"foreign-opaque", "wrong-key", "wrong-member", "closed-backup", "exhaustion", "default-refusal"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := cloneHAMetadataInstallTarget(t, f.configs[0])
			workspace := filepath.Join(cfg.Storage.DataDir, HAMetadataBaselineDirectory)
			if err := os.Mkdir(workspace, 0700); err != nil {
				t.Fatal(err)
			}
			foreign := filepath.Join(workspace, "unknown")
			if err := os.WriteFile(foreign, []byte("opaque creator unknown"), 0600); err != nil {
				t.Fatal(err)
			}
			oldInfo, err := os.Lstat(workspace)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "exhaustion" {
				for i := 0; i < 4; i++ {
					if err := os.Mkdir(fmt.Sprintf("%s.source-unqualified-%d", workspace, i), 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			guard, err := lock.Acquire(cfg.Storage.DataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			archive, err := backup.OpenVerified(context.Background(), archivePath, backupKey)
			if err != nil {
				t.Fatal(err)
			}
			defer archive.Close()
			keys := f.keys
			if scenario == "wrong-key" {
				keys.Metadata = bytes.Repeat([]byte{0x77}, 32)
			}
			if scenario == "wrong-member" {
				replicationCfg := *cfg.Replication
				cfg.Replication = &replicationCfg
				cfg.Replication.NodeID = "foreign-node"
			}
			if scenario == "closed-backup" {
				if err := archive.Close(); err != nil {
					t.Fatal(err)
				}
			}
			o := haPrepareRestartOptions(cfg, f, t)
			p, err := PrepareHAMetadataBaselineWithOptions(context.Background(), cfg, keys, archive, workspace, guard, o, HAMetadataPrepareOptions{RestartSource: scenario != "default-refusal"})
			if scenario != "foreign-opaque" {
				if err == nil {
					t.Fatal("unqualified restart accepted")
				}
				if p != nil {
					if cleanup := p.Discard(); cleanup != nil {
						t.Fatal(cleanup)
					}
				}
				info, statErr := os.Lstat(workspace)
				if statErr != nil || !os.SameFile(oldInfo, info) {
					t.Fatal("refusal moved existing namespace", statErr)
				}
				if raw, err := os.ReadFile(foreign); err != nil || string(raw) != "opaque creator unknown" {
					t.Fatal("refusal changed opaque content", err)
				}
				if o.Budget.Snapshot().Used.Disk != 0 {
					t.Fatal("pre-transition refusal retained new ownership fees", o.Budget.Snapshot())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Close(); err != nil {
				t.Fatal(err)
			}
			preserved := workspace + ".source-unqualified-0"
			info, err := os.Lstat(preserved)
			if err != nil || !os.SameFile(oldInfo, info) {
				t.Fatal("old actual directory inode was not preserved", err)
			}
			if raw, err := os.ReadFile(filepath.Join(preserved, "unknown")); err != nil || string(raw) != "opaque creator unknown" {
				t.Fatal("opaque bytes changed", err)
			}
			if _, err := os.Lstat(filepath.Join(workspace, "unknown")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("old content entered fresh candidate", err)
			}
			member, key, err := readMemberState(cfg, f.keys.Master)
			if err != nil {
				t.Fatal(err)
			}
			defer clear(key[:])
			if _, _, err := readHAMetadataCarrier(context.Background(), p.CarrierPath, key[:], member); err != nil {
				t.Fatal("fresh independent carrier unqualified", err)
			}
			if o.Budget.Snapshot().Used.Disk <= cfg.ResourceBudget.MetadataStoreDiskBytes+cfg.ResourceBudget.MetadataJournalDiskBytes {
				t.Fatal("retained bytes and fresh namespace were not inventoried")
			}
		})
	}
}
