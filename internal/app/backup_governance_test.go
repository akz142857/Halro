package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
)

func TestBackupGovernanceProjectionValidatesNativeDomainAndCleansScratch(t *testing.T) {
	for _, mode := range []string{"valid", "missing-anchor", "invalid-anchor", "revision-gap", "wrong-supersedes", "duplicate-outcome", "changed-head", "canceled", "canceled-during-build", "closed-metadata"} {
		t.Run(mode, func(t *testing.T) {
			store, log, source, key := openGovernanceRecoveryFixture(t)
			defer store.Close()
			defer log.Close()
			first := governanceRecoveryEvent("out_first", "accepted", "", "first", 1)
			second := governanceRecoveryEvent("out_second", "rejected", first.OutcomeID, "second", 2)
			switch mode {
			case "revision-gap":
				second.Revision = 3
			case "wrong-supersedes":
				second.SupersedesOutcomeID = "out_missing"
			case "duplicate-outcome":
				second.OutcomeID = first.OutcomeID
			}
			if _, err := log.AppendBatch(context.Background(), []governance.Event{first, second}); err != nil {
				t.Fatal(err)
			}
			head, auth := log.AuthenticatedHead()
			if mode == "invalid-anchor" {
				auth[0] ^= 1
			}
			if mode != "missing-anchor" {
				if err := store.PutGovernanceJournalAnchor(head.Records, head.Bytes, head.LastHash, auth); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "changed-head" {
				head.Records++
			}
			parent := t.TempDir()
			ctx := context.Background()
			if mode == "canceled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			enteredBuild := false
			if mode == "canceled-during-build" {
				base, cancel := context.WithCancel(ctx)
				defer cancel()
				ctx = generationProbeContext{Context: base, probe: func() {
					entries, err := os.ReadDir(parent)
					if err != nil {
						t.Fatal(err)
					}
					for _, entry := range entries {
						if _, err := os.Stat(filepath.Join(parent, entry.Name(), "governance.db")); err == nil {
							enteredBuild = true
							cancel()
							return
						}
					}
				}}
			}
			if mode == "closed-metadata" {
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
			}
			err := validateBackupGovernanceProjection(ctx, config.Config{}, store, log, source, key, head, parent)
			if (mode == "valid") != (err == nil) {
				t.Fatal("native domain validation result", mode, err)
			}
			if (mode == "missing-anchor" || mode == "invalid-anchor") && !errors.Is(err, errGovernanceAnchorMismatch) {
				t.Fatal("anchor refusal lost", err)
			}
			if (mode == "canceled" || mode == "canceled-during-build") && !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation lost", err)
			}
			if mode == "canceled-during-build" && !enteredBuild {
				t.Fatal("fixture did not cancel a constructed physical candidate")
			}
			entries, err := os.ReadDir(parent)
			if err != nil || len(entries) != 0 {
				t.Fatal("temporary Governance DB survived validation", entries, err)
			}
		})
	}
}

func TestBackupGovernanceDiskRoundTripPreservesHistoryAndLegacyCheckpointBytes(t *testing.T) {
	configPath, cfg, store, log, key := governanceBackupFixture(t)
	ctx := context.Background()
	var final governance.Event
	for i := 0; i < 128; i++ {
		first := governanceRecoveryEvent(fmt.Sprintf("out_f%04d", i), "accepted", "", fmt.Sprintf("first-%d", i), 1)
		first.WorkUnitID = fmt.Sprintf("wku_%04d", i)
		second := governanceRecoveryEvent(fmt.Sprintf("out_s%04d", i), "rejected", first.OutcomeID, fmt.Sprintf("second-%d", i), 2)
		second.WorkUnitID = first.WorkUnitID
		if _, err := log.AppendBatch(ctx, []governance.Event{first, second}); err != nil {
			t.Fatal(err)
		}
		final = second
	}
	head, auth := log.AuthenticatedHead()
	if err := store.PutGovernanceJournalAnchor(head.Records, head.Bytes, head.LastHash, auth); err != nil {
		t.Fatal(err)
	}
	// A sizeable old derivative remains in Metadata. Backup copies it as opaque
	// bytes instead of assembling/decoding State or deleting an unreadable blob.
	legacy := bytes.Repeat([]byte("legacy derivative\n"), 1<<17)
	if err := store.SaveGovernanceCheckpoint(head.Records, head.Bytes, head.LastHash, sha256.Sum256(legacy), [32]byte{1}, legacy); err != nil {
		t.Fatal(err)
	}
	if err := store.PutUsageCheckpoint(ledger.Watermark{Generation: 1, Sequence: 99999, Offset: 99999}, legacy, nil, nil, domain.RollupVersion, nil); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(log.Close(), store.Close()); err != nil {
		t.Fatal(err)
	}
	archiveKey := bytes.Repeat([]byte{0x47}, 32)
	archivePath := filepath.Join(t.TempDir(), "governance.hmbk")
	manifest, err := CreateBackup(ctx, cfg, configPath, archivePath, archiveKey)
	if err != nil || manifest.GovernanceSequence != head.Records || manifest.CheckpointWatermark != (ledger.Watermark{}) {
		t.Fatal("backup selected an untrusted derivative", manifest.GovernanceSequence, manifest.CheckpointWatermark, err)
	}
	for _, file := range manifest.Files {
		if strings.Contains(file.Path, "governance-check") || strings.HasSuffix(file.Path, "governance.db") || strings.HasSuffix(file.Path, "governance-projection.db") {
			t.Fatal("backup published derived scratch", file.Path)
		}
	}
	metadata, err := openMetadataForTest(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, checkpointErr := metadata.LoadGovernanceCheckpoint()
	if err := errors.Join(checkpointErr, metadata.Close()); err != nil || !bytes.Equal(checkpoint.Payload, legacy) {
		t.Fatal("backup changed opaque legacy checkpoint", err)
	}
	if _, err := RestoreBackup(ctx, cfg, archivePath, archiveKey, manifest.BackupID); err != nil {
		t.Fatal(err)
	}
	next, err := Open(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	current, found, err := next.governance.owner.projection.ReadCurrent(ctx, final.ProjectID, final.WorkUnitID, final.DefinitionID, final.DefinitionVersion)
	if err != nil || !found || current.ID != final.OutcomeID || current.Revision != 2 || next.governance.manager.State() != nil {
		t.Fatal("restore changed native Governance history", current, found, err)
	}
	clear(key)
}

func TestBackupGovernanceRestoreRejectsAuthenticatedInvalidRevisionBeforePublish(t *testing.T) {
	configPath, cfg, store, log, key := governanceBackupFixture(t)
	ctx := context.Background()
	first := governanceRecoveryEvent("out_first", "accepted", "", "first", 1)
	if _, err := log.Append(ctx, first); err != nil {
		t.Fatal(err)
	}
	head, auth := log.AuthenticatedHead()
	if err := store.PutGovernanceJournalAnchor(head.Records, head.Bytes, head.LastHash, auth); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(log.Close(), store.Close()); err != nil {
		t.Fatal(err)
	}
	archiveKey := bytes.Repeat([]byte{0x48}, 32)
	archivePath := filepath.Join(t.TempDir(), "valid.hmbk")
	manifest, err := CreateBackup(ctx, cfg, configPath, archivePath, archiveKey)
	if err != nil {
		t.Fatal(err)
	}
	extracted := t.TempDir()
	if _, err := backup.Extract(archivePath, archiveKey, extracted); err != nil {
		t.Fatal(err)
	}
	stagedNative, err := governance.Open(filepath.Join(extracted, "data", "governance", "governance.journal"), key)
	if err != nil {
		t.Fatal(err)
	}
	invalid := governanceRecoveryEvent("out_gap", "rejected", first.OutcomeID, "gap", 3)
	if _, err := stagedNative.Append(ctx, invalid); err != nil {
		t.Fatal(err)
	}
	badHead := stagedNative.Summary()
	if err := stagedNative.Close(); err != nil {
		t.Fatal(err)
	}
	files := make([]backup.SourceFile, 0, len(manifest.Files))
	for _, file := range manifest.Files {
		files = append(files, backup.SourceFile{ArchivePath: file.Path, LocalPath: filepath.Join(extracted, filepath.FromSlash(file.Path))})
	}
	badPath := filepath.Join(t.TempDir(), "signed-invalid-domain.hmbk")
	badManifest, err := backup.Create(backup.CreateOptions{
		OutputPath: badPath, BackupKey: archiveKey, Files: files, Metadata: manifest.Metadata, LedgerWatermark: manifest.LedgerWatermark,
		LedgerChainHeadSequence: manifest.LedgerChainHeadSequence, LedgerChainHeadOffset: manifest.LedgerChainHeadOffset,
		LedgerChainHeadHash: manifest.LedgerChainHeadHash, LedgerChainVerified: manifest.LedgerChainVerified,
		GovernanceSequence: badHead.Records, GovernanceOffset: badHead.Bytes, GovernanceHeadHash: badHead.LastHash, GovernanceFormatVersion: 1,
		LedgerFeatureEpoch: manifest.LedgerFeatureEpoch, MinimumLedgerReaderVersion: manifest.MinimumLedgerReaderVersion,
		PricingStateSHA256: manifest.PricingStateSHA256, PendingIntentSHA256: manifest.PendingIntentSHA256, PendingIntents: manifest.PendingIntents,
		MasterKeyFingerprint: manifest.MasterKeyFingerprint, UsageManifestVersion: manifest.UsageManifestVersion, Build: manifest.Build,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBackup(badPath, archiveKey); err != nil {
		t.Fatal("fixture did not reach domain-validation boundary", err)
	}
	before, err := os.ReadFile(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreBackup(ctx, cfg, badPath, archiveKey, badManifest.BackupID); !errors.Is(err, governance.ErrRevisionConflict) {
		t.Fatal("authenticated invalid revision was published", err)
	}
	after, err := os.ReadFile(cfg.MetadataPath())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed restore changed live Metadata", err)
	}
	actual, err := governance.Verify(cfg.GovernancePath(), key)
	if err != nil || actual != head {
		t.Fatal("failed restore changed live Governance", actual, err)
	}
	clear(key)
}
