package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	backuppkg "github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	corekms "github.com/akz142857/Halro/internal/kms"
	"github.com/akz142857/Halro/internal/kms/awskms"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/masterkey"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/vault"
)

func TestKMSBackupManifestAndHistoricalDescriptorRemainImmutableAfterRewrap(t *testing.T) {
	cfg, archivePath, backupKey, manifest, harness, _ := kmsBackupFixture(t)
	if manifest.KeySlotDescriptorSHA256 == "" || manifest.RestoreDrillVerified {
		t.Fatalf("manifest=%#v", manifest)
	}
	archiveBefore, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	addKMSHarnessRoot(t, harness, replacementPrimaryKMSKeyARN, 0x33)
	rewrapCfg := cfg
	rewrapCfg.Storage.MasterKey.PrimarySlot = "slot_aws_primary_backup_rewrap"
	rewrapCfg.Storage.MasterKey.AllowedKMSKeys = append(append([]config.AllowedKMSKey(nil), cfg.Storage.MasterKey.AllowedKMSKeys...), config.AllowedKMSKey{
		Purpose: "primary", Provider: "aws-kms", Region: "eu-west-1", Account: "345678901234",
		KeyID: replacementPrimaryKMSKeyARN, Algorithm: "SYMMETRIC_DEFAULT",
	})
	if _, err := rewrapKMSKeyWithOptions(context.Background(), rewrapCfg, KMSRewrapOptions{
		Purpose: masterkey.KeySlotPrimary, SlotID: rewrapCfg.Storage.MasterKey.PrimarySlot,
		KeyReference: replacementPrimaryKMSKeyARN,
	}, harness.factory, time.Now, nil); err != nil {
		t.Fatal(err)
	}
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := store.KeySlotDescriptor(context.Background())
	store.Close()
	if err != nil {
		t.Fatal(err)
	}
	if descriptorDigestForBackupTest(t, descriptor) == manifest.KeySlotDescriptorSHA256 {
		t.Fatal("rewrap did not produce a new current descriptor digest")
	}
	verified, err := VerifyBackup(archivePath, backupKey)
	if err != nil || verified.KeySlotDescriptorSHA256 != manifest.KeySlotDescriptorSHA256 || verified.RestoreDrillVerified {
		t.Fatalf("verified=%#v err=%v", verified, err)
	}
	archiveAfter, err := os.ReadFile(archivePath)
	if err != nil || !bytes.Equal(archiveBefore, archiveAfter) {
		t.Fatalf("rewrap changed historical backup: %v", err)
	}
}

func TestKMSRestoreUsesStagedDescriptorForPrimaryAndExplicitRecovery(t *testing.T) {
	for _, useRecovery := range []bool{false, true} {
		name := "primary"
		if useRecovery {
			name = "recovery"
		}
		t.Run(name, func(t *testing.T) {
			cfg, archivePath, backupKey, manifest, harness, masterKey := kmsBackupFixture(t)
			defer clear(masterKey)
			sentinel := filepath.Join(cfg.Storage.DataDir, "post-backup-sentinel")
			if err := os.WriteFile(sentinel, []byte("preserve in rollback"), 0o600); err != nil {
				t.Fatal(err)
			}
			options := RestoreOptions{}
			if useRecovery {
				options.UseRecoverySlot = true
				options.ConfirmRecoverySlot = cfg.Storage.MasterKey.RecoverySlot
			}
			result, err := restoreBackupWithFactory(context.Background(), cfg, archivePath, backupKey, manifest.BackupID, options, harness.factory)
			if err != nil {
				t.Fatal(err)
			}
			if result.UnlockPath != name || result.PreviousDataDir == "" {
				t.Fatalf("result=%#v", result)
			}
			if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("restored data retained post-backup sentinel: %v", err)
			}
			if payload, err := os.ReadFile(filepath.Join(result.PreviousDataDir, "post-backup-sentinel")); err != nil || string(payload) != "preserve in rollback" {
				t.Fatalf("rollback payload=%q err=%v", payload, err)
			}
			store, err := boltstore.Open(cfg.MetadataPath())
			if err != nil {
				t.Fatal(err)
			}
			key, err := unlockKMSMasterKey(context.Background(), cfg, store, masterkey.KeySlotPurpose(name), harness.factory)
			store.Close()
			if err != nil || !bytes.Equal(key, masterKey) {
				t.Fatalf("restored Vault key mismatch=%t err=%v", bytes.Equal(key, masterKey), err)
			}
			clear(key)
			if useRecovery {
				auditKey, err := vault.DeriveAuditHMACKey(masterKey)
				if err != nil {
					t.Fatal(err)
				}
				log, err := audit.Open(cfg.AuditPath(), auditKey)
				clear(auditKey)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				if _, err := log.Replay(func(record audit.Record) error {
					if record.Event.Action == "security.master_key.recovery_used" && record.Event.ReasonCode == "break_glass_restore" {
						found = record.Event.TargetID == cfg.Storage.MasterKey.RecoverySlot
					}
					return nil
				}); err != nil {
					log.Close()
					t.Fatal(err)
				}
				log.Close()
				if !found {
					t.Fatal("Recovery restore did not append high-severity Audit evidence")
				}
			}
		})
	}
}

func TestKMSRestoreRejectsWrongAllowlistAndRecoveryConfirmationBeforeKMS(t *testing.T) {
	cfg, archivePath, backupKey, manifest, harness, _ := kmsBackupFixture(t)
	beforeCalls := harness.callCount()
	if _, err := restoreBackupWithFactory(context.Background(), cfg, archivePath, backupKey, manifest.BackupID,
		RestoreOptions{UseRecoverySlot: true, ConfirmRecoverySlot: "wrong-slot"}, harness.factory); err == nil {
		t.Fatal("wrong Recovery confirmation was accepted")
	}
	if harness.callCount() != beforeCalls {
		t.Fatal("wrong Recovery confirmation reached KMS")
	}
	wrong := cfg
	wrong.Storage.MasterKey.AllowedKMSKeys = append([]config.AllowedKMSKey(nil), cfg.Storage.MasterKey.AllowedKMSKeys...)
	wrong.Storage.MasterKey.AllowedKMSKeys[0].KeyID = "arn:aws:kms:ap-southeast-1:123456789012:key/99999999-9999-4999-8999-999999999999"
	if _, err := restoreBackupWithFactory(context.Background(), wrong, archivePath, backupKey, manifest.BackupID, RestoreOptions{}, harness.factory); err == nil {
		t.Fatal("wrong target allowlist was accepted")
	}
	if harness.callCount() != beforeCalls {
		t.Fatal("wrong target allowlist reached KMS")
	}
}

func TestKMSRestoreRecoversWithExplicitRecoveryWhenPrimaryIsDisabled(t *testing.T) {
	cfg, archivePath, backupKey, manifest, harness, _ := kmsBackupFixture(t)
	sentinel := filepath.Join(cfg.Storage.DataDir, "primary-disabled-sentinel")
	if err := os.WriteFile(sentinel, []byte("live state"), 0o600); err != nil {
		t.Fatal(err)
	}
	factory := func(ctx context.Context, allowed config.AllowedKMSKey) (corekms.Wrapper, error) {
		if allowed.Purpose == string(masterkey.KeySlotPrimary) {
			return nil, corekms.NewError(corekms.ErrorKeyUnavailable, awskms.Provider, corekms.OperationUnwrap, 0, errors.New("simulated disabled Primary KMS Key"))
		}
		return harness.factory(ctx, allowed)
	}
	if _, err := restoreBackupWithFactory(context.Background(), cfg, archivePath, backupKey, manifest.BackupID, RestoreOptions{}, factory); corekms.Classify(err) != corekms.ErrorKeyUnavailable {
		t.Fatalf("Primary disabled class=%q err=%v", corekms.Classify(err), err)
	}
	if payload, err := os.ReadFile(sentinel); err != nil || string(payload) != "live state" {
		t.Fatalf("failed Primary restore changed live state: payload=%q err=%v", payload, err)
	}
	result, err := restoreBackupWithFactory(context.Background(), cfg, archivePath, backupKey, manifest.BackupID, RestoreOptions{
		UseRecoverySlot: true, ConfirmRecoverySlot: cfg.Storage.MasterKey.RecoverySlot,
	}, factory)
	if err != nil || result.UnlockPath != "recovery" {
		t.Fatalf("Recovery result=%#v err=%v", result, err)
	}
}

func kmsBackupFixture(t *testing.T) (config.Config, string, []byte, backuppkg.Manifest, *kmsAppHarness, []byte) {
	t.Helper()
	cfg := kmsAppTestConfig(t)
	harness := newKMSAppHarness(t)
	masterKey := bytes.Repeat([]byte{0x6a}, 32)
	if err := initializeKMS(context.Background(), cfg, kmsInitializationOptions{
		factory: harness.factory, random: bytes.NewReader(masterKey),
	}); err != nil {
		t.Fatal(err)
	}
	previousFactory := defaultKMSWrapperFactory
	defaultKMSWrapperFactory = harness.factory
	t.Cleanup(func() { defaultKMSWrapperFactory = previousFactory })
	if _, err := Bootstrap(context.Background(), cfg, BootstrapOptions{
		ProviderName: "OpenAI", ProviderType: domain.ProviderOpenAI, ProviderBaseURL: "https://api.openai.com",
		ProviderModel: "gpt-test", PublicModel: "chat", ProjectName: "Backup",
		BillingMode: domain.BillingModeFree,
	}, []byte("kms-backup-provider-secret")); err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(cfg.Storage.DataDir)
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(root, "kms-backup.hmbk")
	backupKey := bytes.Repeat([]byte{0x4b}, 32)
	manifest, err := CreateBackup(context.Background(), cfg, configPath, archivePath, backupKey)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, archivePath, backupKey, manifest, harness, bytes.Clone(masterKey)
}

func descriptorDigestForBackupTest(t *testing.T, descriptor masterkey.KeySlotDescriptor) string {
	t.Helper()
	encoded, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func TestOfflineEncryptedBackupCapturesConsistentManifestAndAudit(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := Bootstrap(context.Background(), cfg, BootstrapOptions{
		ProviderName: "OpenAI", ProviderType: domain.ProviderOpenAI,
		ProviderBaseURL: "https://api.openai.com", ProviderModel: "gpt-test",
		PublicModel: "chat", ProjectName: "Backup", BillingMode: domain.BillingModeFree,
	}, []byte("provider-secret"))
	if err != nil {
		t.Fatal(err)
	}
	const objectName = "file_backup.object"
	const objectCanary = "provider-object-backup-canary"
	objectDir := filepath.Join(cfg.Storage.DataDir, "provider-objects")
	if err := os.MkdirAll(objectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(objectDir, objectName), []byte(objectCanary), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	profile, _ := domain.DefaultProviderProfile(domain.ProviderOpenAI)
	now := time.Now().UTC()
	_, putErr := store.PutProviderResource(context.Background(), domain.ProviderResource{
		ID: "file_backup", Kind: domain.ResourceFile, ProjectID: bootstrap.ProjectID,
		ProviderID: bootstrap.ProviderID, DeploymentID: bootstrap.DeploymentID,
		PublicModel: "chat", ProfileID: profile.ProfileID, ObjectPath: objectName,
		CreationStatus: "completed", Status: "uploaded", CreatedAt: now,
		UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
	}, 0)
	instance, detectionErr := store.GetProvider(context.Background(), bootstrap.ProviderID)
	if detectionErr == nil {
		credential, credentialErr := store.GetCredential(context.Background(), instance.CredentialID)
		if credentialErr != nil {
			detectionErr = credentialErr
		} else {
			binding := instance.EffectiveProfileBindings()[0]
			_, _, detectionErr = store.CreateModelCapabilityDetection(context.Background(), domain.ModelCapabilityDetection{
				ID: "mcd_backup", ProviderID: instance.ID, ProviderRevision: instance.Revision,
				CredentialRevision: credential.Revision, CredentialKeyVersion: credential.KeyVersion,
				ProviderModel: "backup-unknown-model", ModelRevision: "sha256:backup-model",
				Candidates: []domain.DetectionBindingCandidate{{BindingID: binding.ID, ProfileID: binding.ProfileID,
					AccessSurface: binding.AccessSurface, ModelRevision: "sha256:backup-model", Status: domain.ProbeNotProbed}},
				BindingID: binding.ID,
				ProfileID: binding.ProfileID, AccessSurface: binding.AccessSurface, TargetKind: domain.TargetModelID,
				CanonicalTarget: "backup-unknown-model", SelectionFingerprint: "sha256:backup-selection",
				TargetFingerprint: "sha256:backup-target",
				DetectorVersion:   "capability-detector-v1", RiskTier: "safe_automatic", Status: domain.DetectionQueued,
				Source: "verified_probe", Results: map[string]domain.CapabilityProbeResult{}, MaxProviderCalls: 8,
				CreatedBy: "admin", IdempotencyKeyHash: "sha256:backup-key", RequestHash: "sha256:backup-request",
				CreatedAt: now, UpdatedAt: now,
			}, now)
		}
	}
	closeErr := store.Close()
	if err := errors.Join(putErr, detectionErr, closeErr); err != nil {
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
	foundObject := false
	for _, file := range verified.Files {
		if file.Path == "data/usage/orphan.tmp" {
			t.Fatal("uncommitted usage orphan was included in backup")
		}
		if file.Path == "data/provider-objects/"+objectName {
			foundObject = true
		}
	}
	if !foundObject {
		t.Fatal("referenced provider object was omitted from backup")
	}
	extracted := filepath.Join(root, "extracted-backup")
	if _, err := backuppkg.Extract(output, key, extracted); err != nil {
		t.Fatal(err)
	}
	if payload, err := os.ReadFile(filepath.Join(extracted, "data", "provider-objects", objectName)); err != nil || string(payload) != objectCanary {
		t.Fatalf("restored provider object=%q err=%v", payload, err)
	}
	restoredStore, err := boltstore.Open(filepath.Join(extracted, "data", "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restoredStore.GetModelCapabilityDetection(context.Background(), "mcd_backup"); err != nil {
		restoredStore.Close()
		t.Fatalf("restored capability detection: %v", err)
	}
	if err := restoredStore.Close(); err != nil {
		t.Fatal(err)
	}
	encrypted, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, []byte(configCanary)) {
		t.Fatal("backup leaked config plaintext")
	}
	if bytes.Contains(encrypted, []byte(objectCanary)) {
		t.Fatal("backup leaked provider object plaintext")
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

func TestBackupRejectsMissingReferencedProviderObject(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := Bootstrap(context.Background(), cfg, BootstrapOptions{
		ProviderName: "OpenAI", ProviderType: domain.ProviderOpenAI,
		ProviderBaseURL: "https://api.openai.com", ProviderModel: "gpt-test",
		PublicModel: "chat", ProjectName: "Backup", BillingMode: domain.BillingModeFree,
	}, []byte("provider-secret"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	profile, _ := domain.DefaultProviderProfile(domain.ProviderOpenAI)
	now := time.Now().UTC()
	_, putErr := store.PutProviderResource(context.Background(), domain.ProviderResource{
		ID: "file_missing", Kind: domain.ResourceFile, ProjectID: bootstrap.ProjectID,
		ProviderID: bootstrap.ProviderID, DeploymentID: bootstrap.DeploymentID,
		PublicModel: "chat", ProfileID: profile.ProfileID, ObjectPath: "file_missing.object",
		CreationStatus: "completed", Status: "uploaded", CreatedAt: now,
		UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
	}, 0)
	closeErr := store.Close()
	if err := errors.Join(putErr, closeErr); err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(cfg.Storage.DataDir)
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "incomplete.hmbk")
	if _, err := CreateBackup(context.Background(), cfg, configPath, output, bytes.Repeat([]byte{0x61}, 32)); err == nil {
		t.Fatal("backup accepted a missing referenced provider object")
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete backup was published: %v", err)
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
	masterKey, err := vault.LoadMasterKey(cfg.Storage.MasterKey.File)
	if err != nil {
		metadata.Close()
		t.Fatal(err)
	}
	masterFingerprint := keyFingerprint(masterKey)
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
	liveLedger, err := ledger.OpenWithOptions(cfg.LedgerPath(), ledger.NewStatus(), ledger.Options{
		QueueCapacity: 256, MaxBatch: 1, ChainKey: ledgerKey,
	})
	if err != nil {
		clear(ledgerKey)
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
				PeriodTimezone: "UTC", PeriodTimezoneVersion: 1,
			})
			errorsByWriter <- err
		}()
	}
	archivePath := filepath.Join(root, "concurrent.hmbk")
	backupKey := bytes.Repeat([]byte{0x6c}, 32)
	close(start)
	manifest, snapshotErr := createBackupSnapshotWithLedger(
		context.Background(), cfg, configPath, archivePath, backupKey,
		metadata, masterFingerprint, liveLedger, ledgerKey,
	)
	clear(ledgerKey)
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
	bootstrap, err := Bootstrap(context.Background(), cfg, BootstrapOptions{
		ProviderName: "OpenAI", ProviderType: domain.ProviderOpenAI,
		ProviderBaseURL: "https://api.openai.com", ProviderModel: "gpt-test",
		PublicModel: "chat", ProjectName: "Restore", BillingMode: domain.BillingModeFree,
	}, []byte("restore-provider-secret"))
	if err != nil {
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
	if result.BackupID != manifest.BackupID || result.DataDir != cfg.Storage.DataDir || result.PreviousDataDir == "" ||
		result.SchemaVersionBefore != manifest.Metadata.SchemaVersion || result.SchemaVersionAfter != boltstore.CurrentSchemaVersion() ||
		result.RestoredEnabledGatewayKeyCount != 1 || !slices.Equal(result.RestoredEnabledGatewayKeyIDs, []string{bootstrap.KeyID}) {
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

func TestRestoreMasterKeyMismatchNamesBothFingerprintsAndRecoveryStep(t *testing.T) {
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
	backupKey := bytes.Repeat([]byte{0x64}, 32)
	archivePath := filepath.Join(root, "pre-rotation.hmbk")
	manifest, err := CreateBackup(context.Background(), cfg, configPath, archivePath, backupKey)
	if err != nil {
		t.Fatal(err)
	}
	replacementKey := bytes.Repeat([]byte{0x65}, 32)
	if err := os.WriteFile(cfg.Storage.MasterKey.File, replacementKey, 0o600); err != nil {
		t.Fatal(err)
	}
	replacementDigest := sha256.Sum256(replacementKey)
	replacementFingerprint := "sha256:" + hex.EncodeToString(replacementDigest[:])
	sentinel := filepath.Join(cfg.Storage.DataDir, "live-sentinel")
	if err := os.WriteFile(sentinel, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = RestoreBackup(context.Background(), cfg, archivePath, backupKey, manifest.BackupID)
	if err == nil || !strings.Contains(err.Error(), fingerprintPrefix(replacementFingerprint)) ||
		!strings.Contains(err.Error(), fingerprintPrefix(manifest.MasterKeyFingerprint)) ||
		!strings.Contains(err.Error(), "storage.master_key.file") || !strings.Contains(err.Error(), "retry") {
		t.Fatalf("Master Key mismatch error=%v", err)
	}
	if payload, readErr := os.ReadFile(sentinel); readErr != nil || string(payload) != "live" {
		t.Fatalf("rejected restore changed live data: payload=%q err=%v", payload, readErr)
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
