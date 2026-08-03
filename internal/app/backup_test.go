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
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/audit"
	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/ledger"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
	"github.com/akz142857/Heimdall/internal/vault"
)

func TestOfflineEncryptedBackupCapturesConsistentManifestAndAudit(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(cfg.Storage.DataDir)
	configPath := filepath.Join(root, "config.yaml")
	const configCanary = "backup-config-canary"
	if err := os.WriteFile(configPath, []byte("version: 1\n# "+configCanary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.UsagePath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(cfg.UsagePath(), "orphan.tmp"), []byte("uncommitted"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "snapshot.hmbk")
	key := bytes.Repeat([]byte{0x5a}, 32)
	created, err := CreateBackup(context.Background(), cfg, configPath, output, key)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := VerifyBackup(output, key)
	if err != nil {
		t.Fatal(err)
	}
	if created.BackupID != verified.BackupID ||
		verified.Metadata.SchemaVersion != boltstore.CurrentSchemaVersion() ||
		verified.LedgerWatermark.Generation != 1 ||
		verified.MasterKeyFingerprint == "" {
		t.Fatalf("manifest=%#v", verified)
	}
	for _, file := range verified.Files {
		if file.Path == "data/usage/orphan.tmp" {
			t.Fatal("uncommitted usage orphan was included in backup")
		}
	}
	encrypted, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, []byte(configCanary)) {
		t.Fatal("backup leaked config plaintext")
	}
	masterKey, err := os.ReadFile(cfg.Storage.MasterKey.File)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, masterKey) {
		t.Fatal("backup packaged the master key")
	}
	clear(masterKey)
	actions := make(map[string]int)
	summary, err := VerifyAudit(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Records < 2 {
		t.Fatalf("audit summary=%#v", summary)
	}
	masterKey, err = vault.LoadMasterKey(cfg.Storage.MasterKey.File)
	if err != nil {
		t.Fatal(err)
	}
	auditKey, err := vault.DeriveAuditHMACKey(masterKey)
	clear(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	auditLog, err := audit.Open(cfg.AuditPath(), auditKey)
	clear(auditKey)
	if err != nil {
		t.Fatal(err)
	}
	defer auditLog.Close()
	if _, err := auditLog.Replay(func(record audit.Record) error {
		if record.Event.Action == "backup.create" {
			actions[record.Event.Outcome]++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if actions["requested"] != 1 || actions["success"] != 1 {
		t.Fatalf("backup audit outcomes=%#v", actions)
	}
}

func TestBackupRequiresExclusiveDataLockAndExternalOutput(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(cfg.Storage.DataDir)
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x33}, 32)
	if _, err := CreateBackup(
		context.Background(), cfg, configPath,
		filepath.Join(cfg.Storage.DataDir, "forbidden.hmbk"), key,
	); err == nil {
		t.Fatal("backup inside live data directory was accepted")
	}
	runtime, err := Open(
		context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	output := filepath.Join(root, "locked.hmbk")
	if _, err := CreateBackup(context.Background(), cfg, configPath, output, key); err == nil {
		t.Fatal("backup acquired the data directory while runtime was active")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("failed backup published output: %v", err)
	}
}

func TestBackupRestoreMatchesManifestDuringOneHundredConcurrentLedgerWrites(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(cfg.Storage.DataDir)
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	liveLedger, err := ledger.OpenWithOptions(cfg.LedgerPath(), ledger.NewStatus(), ledger.Options{
		QueueCapacity: 256, MaxBatch: 1,
	})
	if err != nil {
		metadata.Close()
		t.Fatal(err)
	}
	start := make(chan struct{})
	errorsByWriter := make(chan error, 100)
	var writers sync.WaitGroup
	writers.Add(100)
	for index := 0; index < 100; index++ {
		index := index
		go func() {
			defer writers.Done()
			<-start
			_, err := liveLedger.Append(context.Background(), ledger.Event{
				EventID: fmt.Sprintf("backup_evt_%03d", index), Kind: ledger.EventRequestAccepted,
				RequestID: fmt.Sprintf("backup_req_%03d", index), ProjectID: "project_backup",
				PeriodID: "project_backup:2026-07-31:UTC", OccurredAt: time.Now().UTC(),
			})
			errorsByWriter <- err
		}()
	}
	masterKey, err := vault.LoadMasterKey(cfg.Storage.MasterKey.File)
	if err != nil {
		liveLedger.Close()
		metadata.Close()
		t.Fatal(err)
	}
	masterFingerprint := keyFingerprint(masterKey)
	clear(masterKey)
	archivePath := filepath.Join(root, "concurrent.hmbk")
	backupKey := bytes.Repeat([]byte{0x6c}, 32)
	close(start)
	manifest, snapshotErr := createBackupSnapshotWithLedger(
		context.Background(), cfg, configPath, archivePath, backupKey,
		metadata, masterFingerprint, liveLedger,
	)
	writers.Wait()
	close(errorsByWriter)
	for writerErr := range errorsByWriter {
		if writerErr != nil {
			t.Fatal(writerErr)
		}
	}
	if closeErr := liveLedger.Close(); snapshotErr == nil {
		snapshotErr = closeErr
	}
	if closeErr := metadata.Close(); snapshotErr == nil {
		snapshotErr = closeErr
	}
	if snapshotErr != nil {
		t.Fatal(snapshotErr)
	}
	if manifest.LedgerWatermark.Sequence > 100 {
		t.Fatalf("manifest watermark=%#v", manifest.LedgerWatermark)
	}
	verified, err := VerifyBackup(archivePath, backupKey)
	if err != nil || verified.LedgerWatermark != manifest.LedgerWatermark {
		t.Fatalf("verified=%#v err=%v", verified.LedgerWatermark, err)
	}
	result, err := RestoreBackup(
		context.Background(), cfg, archivePath, backupKey, manifest.BackupID,
	)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := ledger.Open(cfg.LedgerPath(), ledger.NewStatus())
	if err != nil {
		t.Fatal(err)
	}
	restoredWatermark, replayErr := restored.Replay(ledger.Watermark{}, nil)
	closeErr := restored.Close()
	if err := errors.Join(replayErr, closeErr); err != nil {
		t.Fatal(err)
	}
	if restoredWatermark != manifest.LedgerWatermark || result.LedgerSequence != manifest.LedgerWatermark.Sequence {
		t.Fatalf("restored=%#v manifest=%#v result=%#v", restoredWatermark, manifest.LedgerWatermark, result)
	}
	previous, err := ledger.Open(filepath.Join(result.PreviousDataDir, "ledger", "ledger.wal"), ledger.NewStatus())
	if err != nil {
		t.Fatal(err)
	}
	previousWatermark, replayErr := previous.Replay(ledger.Watermark{}, nil)
	closeErr = previous.Close()
	if err := errors.Join(replayErr, closeErr); err != nil {
		t.Fatal(err)
	}
	if previousWatermark.Sequence != 100 || previousWatermark.Sequence < restoredWatermark.Sequence {
		t.Fatalf("previous=%#v restored=%#v", previousWatermark, restoredWatermark)
	}
}

func TestRestoreValidatesStagesAtomicallyAndPreservesRollbackDirectory(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte("correct horse battery staple")); err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(cfg.Storage.DataDir)
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x71}, 32)
	archivePath := filepath.Join(root, "restore-source.hmbk")
	manifest, err := CreateBackup(context.Background(), cfg, configPath, archivePath, key)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(cfg.Storage.DataDir, "created-after-backup")
	if err := os.WriteFile(sentinel, []byte("rollback evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreBackup(context.Background(), cfg, archivePath, key, "wrong-id"); err == nil {
		t.Fatal("restore accepted an incorrect confirmation id")
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("failed confirmation modified live data: %v", err)
	}
	result, err := RestoreBackup(context.Background(), cfg, archivePath, key, manifest.BackupID)
	if err != nil {
		t.Fatal(err)
	}
	if result.BackupID != manifest.BackupID || result.DataDir != cfg.Storage.DataDir || result.PreviousDataDir == "" {
		t.Fatalf("restore result=%#v", result)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("post-backup file survived restored snapshot: %v", err)
	}
	if payload, err := os.ReadFile(filepath.Join(result.PreviousDataDir, "created-after-backup")); err != nil || string(payload) != "rollback evidence" {
		t.Fatalf("rollback directory did not preserve old data: payload=%q err=%v", payload, err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("restored runtime did not open: %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	summary, err := VerifyAudit(context.Background(), cfg)
	if err != nil || summary.Records < 3 {
		t.Fatalf("restored audit is invalid: summary=%#v err=%v", summary, err)
	}
}

func TestRestoreInvalidatesCapturedAdminSessionsAndMFAChallenges(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte("correct horse battery staple")); err != nil {
		t.Fatal(err)
	}
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	user, err := store.GetAdminUser(context.Background(), "admin")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	now := time.Now().UTC()
	sessionHash := sha256.Sum256([]byte("captured-restore-session"))
	challengeHash := sha256.Sum256([]byte("captured-restore-challenge"))
	if err := store.PutAdminSession(context.Background(), domain.AdminSession{
		IDHash: sessionHash, Username: user.Username, Generation: user.SessionGeneration,
		CreatedAt: now, LastSeenAt: now, AbsoluteExpiresAt: now.Add(time.Hour), IdleExpiresAt: now.Add(time.Hour),
	}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.PutAdminMFAChallenge(context.Background(), domain.AdminMFAChallenge{
		IDHash: challengeHash, Username: user.Username, Purpose: domain.AdminMFAChallengeLogin,
		CreatedAt: now, ExpiresAt: now.Add(5 * time.Minute), AttemptsRemaining: 5,
		SessionGeneration: user.SessionGeneration,
	}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	root := filepath.Dir(cfg.Storage.DataDir)
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(root, "authentication-restore.hmbk")
	backupKey := bytes.Repeat([]byte{0x58}, 32)
	manifest, err := CreateBackup(context.Background(), cfg, configPath, archivePath, backupKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreBackup(context.Background(), cfg, archivePath, backupKey, manifest.BackupID); err != nil {
		t.Fatal(err)
	}

	restored, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	restoredUser, err := restored.GetAdminUser(context.Background(), user.Username)
	if err != nil {
		t.Fatal(err)
	}
	if restoredUser.SessionGeneration != user.SessionGeneration+1 {
		t.Fatalf("restored session generation=%d want=%d", restoredUser.SessionGeneration, user.SessionGeneration+1)
	}
	if _, err := restored.GetAdminSession(context.Background(), sessionHash); !errors.Is(err, boltstore.ErrNotFound) {
		t.Fatalf("captured session survived restore: %v", err)
	}
	if _, err := restored.GetAdminMFAChallenge(context.Background(), challengeHash); !errors.Is(err, boltstore.ErrNotFound) {
		t.Fatalf("captured MFA challenge survived restore: %v", err)
	}
}
