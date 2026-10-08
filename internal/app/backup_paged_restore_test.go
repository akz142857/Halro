package app

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/config"
)

// Repackage a real App-created native snapshot without altering its data/cuts.
// This is a test converter, not the default writer or a migration command.
func nativePagedBackupForTest(t *testing.T, path string, key []byte, output string) backup.PagedManifest {
	t.Helper()
	stage := filepath.Join(t.TempDir(), "extract")
	m, err := backup.Extract(path, key, stage)
	if err != nil {
		t.Fatal(err)
	}
	options := backup.CreateOptions{
		OutputPath: output, BackupKey: key, Metadata: m.Metadata, LedgerWatermark: m.LedgerWatermark, CheckpointWatermark: m.CheckpointWatermark, UsageManifestVersion: m.UsageManifestVersion,
		LedgerFeatureEpoch: m.LedgerFeatureEpoch, MinimumLedgerReaderVersion: m.MinimumLedgerReaderVersion,
		LedgerChainHeadSequence: m.LedgerChainHeadSequence, LedgerChainHeadOffset: m.LedgerChainHeadOffset, LedgerChainHeadHash: m.LedgerChainHeadHash, LedgerChainVerified: m.LedgerChainVerified,
		GovernanceSequence: m.GovernanceSequence, GovernanceOffset: m.GovernanceOffset, GovernanceHeadHash: m.GovernanceHeadHash, GovernanceFormatVersion: m.GovernanceFormatVersion,
		PricingStateSHA256: m.PricingStateSHA256, PendingIntentSHA256: m.PendingIntentSHA256, PendingIntents: m.PendingIntents, MasterKeyFingerprint: m.MasterKeyFingerprint, KeySlotDescriptorSHA256: m.KeySlotDescriptorSHA256,
		ClusterID: m.ClusterID, ClusterIncarnation: m.ClusterIncarnation, SourceNodeID: m.SourceNodeID, SourceRole: m.SourceRole, Term: m.Term, AppliedIndex: m.AppliedIndex, MetadataJournalEpoch: m.MetadataJournalEpoch, MetadataJournalSequence: m.MetadataJournalSequence, PerStoreHead: m.PerStoreHead, Build: m.Build, Now: func() time.Time { return m.CreatedAt },
	}
	for _, file := range m.Files {
		options.Files = append(options.Files, backup.SourceFile{ArchivePath: file.Path, LocalPath: filepath.Join(stage, filepath.FromSlash(file.Path))})
	}
	root, err := backup.CreatePaged(options)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestDefaultRestoreReadsPagedNativeBackupAndPreservesRollback(t *testing.T) {
	cfg := enableBackupReferenceBudget(testConfig(t))
	if err := cfg.Validate(config.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte("correct horse battery staple")); err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(cfg.Storage.DataDir)
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{94}, 32)
	pagedPath := filepath.Join(root, "paged.hmbk")
	manifest, err := CreateBackupSummary(context.Background(), cfg, configPath, pagedPath, key, CreateBackupOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if manifest.FormatVersion != 5 || manifest.CompatibilityVersion != 3 {
		t.Fatal(manifest.FormatVersion, manifest.CompatibilityVersion)
	}
	sentinel := filepath.Join(cfg.Storage.DataDir, "after-backup")
	if err := os.WriteFile(sentinel, []byte("rollback proof"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreBackup(context.Background(), cfg, pagedPath, key, "wrong-id"); err == nil {
		t.Fatal("incorrect confirmation accepted")
	}
	if raw, err := os.ReadFile(sentinel); err != nil || string(raw) != "rollback proof" {
		t.Fatal("failed confirmation changed live data", err)
	}
	result, err := RestoreBackup(context.Background(), cfg, pagedPath, key, manifest.BackupID)
	if err != nil {
		t.Fatal(err)
	}
	if result.BackupID != manifest.BackupID || result.PreviousDataDir == "" {
		t.Fatal(result)
	}
	if raw, err := os.ReadFile(filepath.Join(result.PreviousDataDir, "after-backup")); err != nil || string(raw) != "rollback proof" {
		t.Fatal("old live data not preserved", err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal("native staged restore failed Runtime open", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	audit, err := VerifyAudit(context.Background(), cfg)
	if err != nil || audit.Records < 3 {
		t.Fatal(audit, err)
	}
	stages, err := filepath.Glob(filepath.Join(root, ".halro-restore-stage-*"))
	if err != nil || len(stages) != 0 {
		t.Fatal("restore stage leaked", stages, err)
	}
}
