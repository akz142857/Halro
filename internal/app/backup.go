package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/buildinfo"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/id"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/masterkey"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/usage"
	"github.com/akz142857/Halro/internal/vault"
)

func CreateBackup(
	ctx context.Context,
	cfg config.Config,
	configPath string,
	outputPath string,
	backupKey []byte,
) (backup.Manifest, error) {
	if err := ctx.Err(); err != nil {
		return backup.Manifest{}, err
	}
	absoluteConfig, err := filepath.Abs(configPath)
	if err != nil {
		return backup.Manifest{}, err
	}
	absoluteOutput, err := filepath.Abs(outputPath)
	if err != nil {
		return backup.Manifest{}, err
	}
	if pathWithin(absoluteOutput, cfg.Storage.DataDir) {
		return backup.Manifest{}, errors.New("backup output must be outside the live data directory")
	}
	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return backup.Manifest{}, fmt.Errorf("acquire offline backup lock: %w", err)
	}
	defer dataLock.Close()
	metadata, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		return backup.Manifest{}, err
	}
	defer metadata.Close()
	masterKey, err := unlockMasterKey(ctx, cfg, metadata)
	if err != nil {
		return backup.Manifest{}, err
	}
	fingerprint := sha256.Sum256(masterKey)
	fingerprintText := "sha256:" + hex.EncodeToString(fingerprint[:])
	secretVault, err := vault.New(masterKey)
	if err != nil {
		clear(masterKey)
		return backup.Manifest{}, err
	}
	if err := verifyVaultKeyCheck(metadata, secretVault); err != nil {
		secretVault.Close()
		clear(masterKey)
		return backup.Manifest{}, err
	}
	auditKey, err := loadAuditHMACKey(metadata, secretVault, masterKey)
	if err != nil {
		secretVault.Close()
		clear(masterKey)
		return backup.Manifest{}, err
	}
	ledgerKey, err := loadLedgerHMACKey(metadata, secretVault, masterKey)
	secretVault.Close()
	clear(masterKey)
	if err != nil {
		clear(auditKey)
		return backup.Manifest{}, err
	}
	auditLog, err := audit.Open(cfg.AuditPath(), auditKey)
	clear(auditKey)
	if err != nil {
		clear(ledgerKey)
		return backup.Manifest{}, err
	}
	defer auditLog.Close()
	if err := reconcileAuditCheckpoint(metadata, auditLog.Summary()); err != nil {
		clear(ledgerKey)
		return backup.Manifest{}, err
	}
	if err := appendBackupAudit(ctx, metadata, auditLog, "requested", ""); err != nil {
		clear(ledgerKey)
		return backup.Manifest{}, err
	}

	manifest, createErr := createBackupSnapshot(
		ctx, cfg, absoluteConfig, absoluteOutput, backupKey, metadata, fingerprintText, ledgerKey,
	)
	clear(ledgerKey)
	outcome := "success"
	reason := ""
	if createErr != nil {
		outcome = "failure"
		reason = "backup_create_failed"
	}
	auditErr := appendBackupAudit(ctx, metadata, auditLog, outcome, reason)
	if createErr != nil || auditErr != nil {
		return backup.Manifest{}, errors.Join(createErr, auditErr)
	}
	return manifest, nil
}

func VerifyBackup(path string, backupKey []byte) (backup.Manifest, error) {
	return backup.Verify(path, backupKey)
}

type RestoreResult struct {
	BackupID                       string   `json:"backup_id"`
	DataDir                        string   `json:"data_dir"`
	PreviousDataDir                string   `json:"previous_data_dir"`
	LedgerSequence                 uint64   `json:"ledger_sequence"`
	UnlockPath                     string   `json:"unlock_path"`
	VaultVerified                  bool     `json:"vault_verified"`
	RecoveryAudited                bool     `json:"recovery_audited"`
	QuarantinedScheduledPrices     int      `json:"quarantined_scheduled_prices"`
	SchemaVersionBefore            uint64   `json:"schema_version_before"`
	SchemaVersionAfter             uint64   `json:"schema_version_after"`
	RestoredEnabledGatewayKeyCount int      `json:"restored_enabled_gateway_key_count"`
	RestoredEnabledGatewayKeyIDs   []string `json:"restored_enabled_gateway_key_ids"`
}

type RestoreOptions struct {
	UseRecoverySlot     bool
	ConfirmRecoverySlot string
}

func RestoreBackup(
	ctx context.Context,
	cfg config.Config,
	archivePath string,
	backupKey []byte,
	confirmBackupID string,
) (RestoreResult, error) {
	return RestoreBackupWithOptions(ctx, cfg, archivePath, backupKey, confirmBackupID, RestoreOptions{})
}

func RestoreBackupWithOptions(
	ctx context.Context,
	cfg config.Config,
	archivePath string,
	backupKey []byte,
	confirmBackupID string,
	options RestoreOptions,
) (RestoreResult, error) {
	recorder := &kmsAuditRecorder{}
	return restoreBackupWithFactory(withKMSAuditRecorder(ctx, recorder), cfg, archivePath, backupKey, confirmBackupID, options, defaultKMSWrapperFactory)
}

func restoreBackupWithFactory(
	ctx context.Context,
	cfg config.Config,
	archivePath string,
	backupKey []byte,
	confirmBackupID string,
	options RestoreOptions,
	factory kmsWrapperFactory,
) (RestoreResult, error) {
	if err := ctx.Err(); err != nil {
		return RestoreResult{}, err
	}
	manifest, err := backup.Verify(archivePath, backupKey)
	if err != nil {
		return RestoreResult{}, err
	}
	if confirmBackupID == "" || confirmBackupID != manifest.BackupID {
		return RestoreResult{}, errors.New("restore confirmation must exactly match the verified backup id")
	}
	if cfg.Storage.MasterKey.Mode == config.MasterKeyModeFile && pathWithin(cfg.Storage.MasterKey.File, cfg.Storage.DataDir) {
		return RestoreResult{}, errors.New("restore requires storage.master_key.file outside storage.data_dir")
	}
	purpose := masterkey.KeySlotPrimary
	unlockPath := "primary"
	if options.UseRecoverySlot {
		if cfg.Storage.MasterKey.Mode != config.MasterKeyModeKeySlots ||
			options.ConfirmRecoverySlot == "" || options.ConfirmRecoverySlot != cfg.Storage.MasterKey.RecoverySlot {
			return RestoreResult{}, errors.New("Recovery restore requires exact confirmation of storage.master_key.recovery_slot")
		}
		purpose = masterkey.KeySlotRecovery
		unlockPath = "recovery"
	} else if options.ConfirmRecoverySlot != "" {
		return RestoreResult{}, errors.New("Recovery Slot confirmation requires explicit Recovery restore mode")
	}

	dataParent := filepath.Dir(cfg.Storage.DataDir)
	stagingRoot, err := os.MkdirTemp(dataParent, ".halro-restore-stage-*")
	if err != nil {
		return RestoreResult{}, err
	}
	defer os.RemoveAll(stagingRoot)
	if err := os.Chmod(stagingRoot, 0o700); err != nil {
		return RestoreResult{}, err
	}
	extractRoot := filepath.Join(stagingRoot, "extract")
	if _, err := backup.Extract(archivePath, backupKey, extractRoot); err != nil {
		return RestoreResult{}, err
	}
	stageData := filepath.Join(extractRoot, "data")
	archiveMetadata := filepath.Join(stageData, "metadata.db")
	stageMetadata := filepath.Join(stageData, cfg.Storage.MetadataFile)
	if archiveMetadata != stageMetadata {
		if err := os.Rename(archiveMetadata, stageMetadata); err != nil {
			return RestoreResult{}, err
		}
	}
	if err := validateRestoreStage(ctx, cfg, stageData, manifest, purpose, factory); err != nil {
		return RestoreResult{}, err
	}
	stageStore, err := boltstore.Open(stageMetadata)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("open staged metadata for authentication invalidation: %w", err)
	}
	schemaVersionAfter, schemaErr := stageStore.SchemaVersion()
	gatewayKeys, gatewayKeysErr := stageStore.ListGatewayKeys(ctx)
	restoredEnabledGatewayKeyIDs := make([]string, 0, len(gatewayKeys))
	if gatewayKeysErr == nil {
		for _, key := range gatewayKeys {
			if key.Enabled && key.DeletedAt == nil {
				restoredEnabledGatewayKeyIDs = append(restoredEnabledGatewayKeyIDs, key.ID)
			}
		}
	}
	quarantined, quarantineErr := stageStore.QuarantineRestoredScheduledPrices(ctx, manifest.CreatedAt.UTC(), time.Now().UTC())
	invalidateErr := stageStore.InvalidateAdminAuthenticationForRestore(ctx)
	closeErr := stageStore.Close()
	if err := errors.Join(schemaErr, gatewayKeysErr, quarantineErr, invalidateErr, closeErr); err != nil {
		return RestoreResult{}, fmt.Errorf("invalidate restored admin authentication: %w", err)
	}

	liveLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("acquire offline restore lock: %w", err)
	}
	defer liveLock.Close()
	stageLock, err := lock.Acquire(stageData)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("lock staged restore: %w", err)
	}
	defer stageLock.Close()
	previousDataDir, err := reservePreviousDataPath(dataParent)
	if err != nil {
		return RestoreResult{}, err
	}
	if err := os.Rename(cfg.Storage.DataDir, previousDataDir); err != nil {
		return RestoreResult{}, fmt.Errorf("preserve current data directory: %w", err)
	}
	if err := os.Rename(stageData, cfg.Storage.DataDir); err != nil {
		rollbackErr := os.Rename(previousDataDir, cfg.Storage.DataDir)
		return RestoreResult{}, errors.Join(fmt.Errorf("publish restored data directory: %w", err), rollbackErr)
	}
	if err := syncDirectoryPath(dataParent); err != nil {
		publicationErr := fmt.Errorf("sync restored data directory publication: %w", err)
		removeCandidateErr := os.Rename(cfg.Storage.DataDir, stageData)
		if removeCandidateErr != nil {
			return RestoreResult{}, errors.Join(publicationErr, fmt.Errorf("remove unsynced restored data directory: %w", removeCandidateErr))
		}
		restorePreviousErr := os.Rename(previousDataDir, cfg.Storage.DataDir)
		if restorePreviousErr != nil {
			republishErr := os.Rename(stageData, cfg.Storage.DataDir)
			var republishContext error
			if republishErr != nil {
				republishContext = fmt.Errorf("republish restored candidate after rollback failure: %w", republishErr)
			}
			return RestoreResult{}, errors.Join(
				publicationErr,
				fmt.Errorf("restore previous data directory: %w", restorePreviousErr),
				republishContext,
			)
		}
		rollbackSyncErr := syncDirectoryPath(dataParent)
		return RestoreResult{}, errors.Join(publicationErr, rollbackSyncErr)
	}
	return RestoreResult{
		BackupID: manifest.BackupID, DataDir: cfg.Storage.DataDir,
		PreviousDataDir: previousDataDir, LedgerSequence: manifest.LedgerWatermark.Sequence,
		UnlockPath: unlockPath, VaultVerified: true, RecoveryAudited: options.UseRecoverySlot,
		QuarantinedScheduledPrices: quarantined,
		SchemaVersionBefore:        manifest.Metadata.SchemaVersion, SchemaVersionAfter: schemaVersionAfter,
		RestoredEnabledGatewayKeyCount: len(restoredEnabledGatewayKeyIDs),
		RestoredEnabledGatewayKeyIDs:   restoredEnabledGatewayKeyIDs,
	}, nil
}

func validateRestoreStage(
	ctx context.Context,
	cfg config.Config,
	stageData string,
	manifest backup.Manifest,
	purpose masterkey.KeySlotPurpose,
	factory kmsWrapperFactory,
) error {
	legacyPricingState, legacyStateErr := boltstore.LegacyPricingBackupState(filepath.Join(stageData, cfg.Storage.MetadataFile))
	metadata, err := boltstore.Open(filepath.Join(stageData, cfg.Storage.MetadataFile))
	if err != nil {
		return fmt.Errorf("open staged metadata: %w", err)
	}
	defer metadata.Close()
	if manifest.FormatVersion >= 2 {
		gate, gateErr := metadata.LedgerCompatibilityGate()
		state, stateErr := metadata.PricingBackupState()
		stateMatches := stateErr == nil && state.StateSHA256 == manifest.PricingStateSHA256 && state.PendingIntentSHA256 == manifest.PendingIntentSHA256 && state.PendingIntents == manifest.PendingIntents
		legacyMatches := legacyStateErr == nil && legacyPricingState.StateSHA256 == manifest.PricingStateSHA256 && legacyPricingState.PendingIntentSHA256 == manifest.PendingIntentSHA256 && legacyPricingState.PendingIntents == manifest.PendingIntents
		if gateErr != nil || gate.FeatureEpoch != manifest.LedgerFeatureEpoch || gate.MinimumReaderVersion != manifest.MinimumLedgerReaderVersion || (!stateMatches && !legacyMatches) {
			return errors.New("staged pricing/accounting compatibility state does not match backup manifest")
		}
	}
	var masterKey []byte
	if cfg.Storage.MasterKey.Mode == config.MasterKeyModeKeySlots {
		descriptor, descriptorErr := metadata.KeySlotDescriptor(ctx)
		if descriptorErr != nil {
			return descriptorErr
		}
		if !descriptor.ProductionReady() || manifest.KeySlotDescriptorSHA256 == "" {
			return errors.New("staged Key Slot descriptor or backup descriptor digest is incomplete")
		}
		encodedDescriptor, encodeErr := json.Marshal(descriptor)
		if encodeErr != nil {
			return encodeErr
		}
		digest := sha256.Sum256(encodedDescriptor)
		if hex.EncodeToString(digest[:]) != manifest.KeySlotDescriptorSHA256 {
			return errors.New("staged Key Slot descriptor does not match the backup manifest")
		}
		if staticErr := validateDoctorKeySlots(ctx, cfg, metadata); staticErr != nil {
			return fmt.Errorf("validate staged Key Slots against target allowlist: %w", staticErr)
		}
		masterKey, err = unlockKMSMasterKey(ctx, cfg, metadata, purpose, factory)
	} else {
		masterKey, err = unlockMasterKey(ctx, cfg, metadata)
	}
	if err != nil {
		return err
	}
	fingerprint := sha256.Sum256(masterKey)
	configuredFingerprint := "sha256:" + hex.EncodeToString(fingerprint[:])
	if configuredFingerprint != manifest.MasterKeyFingerprint {
		clear(masterKey)
		return fmt.Errorf(
			"configured Master Key fingerprint %s does not match backup manifest fingerprint %s; configure the Master Key generation recorded for this backup (storage.master_key.file in file mode) and retry",
			fingerprintPrefix(configuredFingerprint), fingerprintPrefix(manifest.MasterKeyFingerprint),
		)
	}
	secretVault, err := vault.New(masterKey)
	if err != nil {
		clear(masterKey)
		return err
	}
	if err := verifyVaultKeyCheck(metadata, secretVault); err != nil {
		secretVault.Close()
		clear(masterKey)
		return fmt.Errorf("verify staged Vault: %w", err)
	}
	auditKey, err := loadAuditHMACKey(metadata, secretVault, masterKey)
	if err != nil {
		secretVault.Close()
		clear(masterKey)
		return err
	}
	ledgerKey, err := loadLedgerHMACKey(metadata, secretVault, masterKey)
	secretVault.Close()
	clear(masterKey)
	if err != nil {
		clear(auditKey)
		return err
	}
	auditLog, err := audit.Open(filepath.Join(stageData, "audit", "audit.log"), auditKey)
	clear(auditKey)
	if err != nil {
		return fmt.Errorf("verify staged Audit: %w", err)
	}
	defer auditLog.Close()
	if err := reconcileAuditCheckpoint(metadata, auditLog.Summary()); err != nil {
		return fmt.Errorf("verify staged Audit checkpoint: %w", err)
	}
	if recorder := kmsAuditRecorderFromContext(ctx); recorder != nil {
		if err := appendKMSProviderAuditAs(ctx, auditLog, metadata, recorder, "local_cli"); err != nil {
			return fmt.Errorf("append restore KMS provider Audit: %w", err)
		}
	}
	status := ledger.NewStatus()
	ledgerLog, err := ledger.OpenWithOptions(filepath.Join(stageData, "ledger", "ledger.wal"), status, ledger.Options{ChainKey: ledgerKey})
	clear(ledgerKey)
	if err != nil {
		return fmt.Errorf("open staged Ledger: %w", err)
	}
	chainHead, chainHash, chainVerified := ledgerLog.ChainHead()
	chainSequence, chainOffset := chainHead.Sequence, chainHead.Offset
	aggregate := usage.NewAggregate()
	stagedLedgerState := ledger.NewState()
	watermark, replayErr := ledgerLog.Replay(ledger.Watermark{}, func(record ledger.Record) error {
		if err := stagedLedgerState.Apply(record); err != nil {
			return err
		}
		return aggregate.Apply(record)
	})
	closeErr := ledgerLog.Close()
	if err := errors.Join(replayErr, closeErr); err != nil {
		return fmt.Errorf("replay staged Ledger: %w", err)
	}
	if watermark != manifest.LedgerWatermark {
		return errors.New("staged Ledger watermark does not match backup manifest")
	}
	// The chain-head comparison runs against what OpenWithOptions's own tail
	// scan verified moments ago (MAC + hash-chain across the whole staged
	// file), not merely against the manifest's say-so: a restored archive
	// whose recorded head disagrees with the file it shipped with fails
	// restore rather than starting on unverified history.
	if manifest.LedgerChainVerified != chainVerified ||
		(chainVerified && (chainSequence != manifest.LedgerChainHeadSequence ||
			chainOffset != manifest.LedgerChainHeadOffset || chainHash != manifest.LedgerChainHeadHash)) {
		return errors.New("staged Ledger chain head does not match backup manifest")
	}
	if err := metadata.ValidateDeploymentPriceReferences(stagedLedgerState); err != nil {
		return fmt.Errorf("verify staged pricing references: %w", err)
	}
	exporter, err := usage.NewExporter(filepath.Join(stageData, "usage"))
	if err != nil {
		return err
	}
	if _, err := exporter.LoadManifest(); err == nil {
		snapshot := aggregate.Snapshot()
		if err := exporter.Verify(&snapshot); err != nil {
			return fmt.Errorf("verify staged Usage: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if purpose == masterkey.KeySlotRecovery {
		if err := appendRecoveryRestoreAudit(ctx, metadata, auditLog, cfg.Storage.MasterKey.RecoverySlot); err != nil {
			return err
		}
	}
	if err := appendRestoreAudit(ctx, metadata, auditLog, manifest.BackupID); err != nil {
		return err
	}
	return nil
}

func fingerprintPrefix(value string) string {
	const visibleHexDigits = 12
	if !strings.HasPrefix(value, "sha256:") || len(value) <= len("sha256:")+visibleHexDigits {
		return value
	}
	return value[:len("sha256:")+visibleHexDigits] + "..."
}

func appendRecoveryRestoreAudit(ctx context.Context, store *boltstore.Store, log *audit.Log, slotID string) error {
	eventID, err := id.New("aud")
	if err != nil {
		return err
	}
	if _, err := log.Append(ctx, audit.Event{
		EventID: eventID, OccurredAt: time.Now().UTC(), ActorType: "local_cli",
		Action: "security.master_key.recovery_used", TargetType: "master_key_slot",
		TargetID: slotID, Outcome: "success", ReasonCode: "break_glass_restore",
	}); err != nil {
		return err
	}
	return checkpointAudit(store, log.Summary())
}

func appendRestoreAudit(ctx context.Context, store *boltstore.Store, log *audit.Log, backupID string) error {
	eventID, err := id.New("aud")
	if err != nil {
		return err
	}
	if _, err := log.Append(ctx, audit.Event{
		EventID: eventID, OccurredAt: time.Now().UTC(), ActorType: "local_cli",
		Action: "backup.restore", TargetType: "backup", TargetID: backupID, Outcome: "success",
	}); err != nil {
		return err
	}
	return checkpointAudit(store, log.Summary())
}

func reservePreviousDataPath(parent string) (string, error) {
	path, err := os.MkdirTemp(parent, ".halro-pre-restore-*")
	if err != nil {
		return "", err
	}
	if err := os.Remove(path); err != nil {
		return "", err
	}
	return path, nil
}

func syncDirectoryPath(path string) error {
	handle, err := os.Open(path)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}

func createBackupSnapshot(
	ctx context.Context,
	cfg config.Config,
	configPath, outputPath string,
	backupKey []byte,
	metadata *boltstore.Store,
	masterFingerprint string,
	ledgerKey []byte,
) (backup.Manifest, error) {
	status := ledger.NewStatus()
	ledgerLog, err := ledger.Open(cfg.LedgerPath(), status)
	if err != nil {
		return backup.Manifest{}, err
	}
	manifest, createErr := createBackupSnapshotWithLedger(
		ctx, cfg, configPath, outputPath, backupKey, metadata, masterFingerprint, ledgerLog, ledgerKey,
	)
	closeErr := ledgerLog.Close()
	return manifest, errors.Join(createErr, closeErr)
}

func createBackupSnapshotWithLedger(
	ctx context.Context,
	cfg config.Config,
	configPath, outputPath string,
	backupKey []byte,
	metadata *boltstore.Store,
	masterFingerprint string,
	ledgerLog *ledger.Log,
	ledgerKey []byte,
) (backup.Manifest, error) {
	if err := ctx.Err(); err != nil {
		return backup.Manifest{}, err
	}
	if ledgerLog == nil {
		return backup.Manifest{}, errors.New("open Ledger is required for backup snapshot")
	}
	staging, err := os.MkdirTemp(filepath.Dir(outputPath), ".halro-backup-stage-*")
	if err != nil {
		return backup.Manifest{}, err
	}
	defer os.RemoveAll(staging)
	if err := os.Chmod(staging, 0o700); err != nil {
		return backup.Manifest{}, err
	}
	metadataSnapshot := filepath.Join(staging, "metadata.db")
	metadataInfo, err := metadata.Snapshot(metadataSnapshot)
	if err != nil {
		return backup.Manifest{}, err
	}
	pricingBackupState, err := metadata.PricingBackupState()
	if err != nil {
		return backup.Manifest{}, err
	}
	ledgerSnapshot := filepath.Join(staging, "ledger.wal")
	ledgerWatermark, err := ledgerLog.Snapshot(ledgerSnapshot)
	if err != nil {
		return backup.Manifest{}, err
	}
	// The sealed generations go with it. A backup of the active file alone
	// verifies perfectly and restores an installation whose balances begin at
	// whatever the last roll left behind — the worst shape a backup defect can
	// take, because nothing about the restored instance looks wrong.
	stagedSegments, err := ledgerLog.StageSegments(staging)
	if err != nil {
		return backup.Manifest{}, err
	}
	snapshotLog, err := ledger.OpenWithOptions(ledgerSnapshot, ledger.NewStatus(), ledger.Options{ChainKey: ledgerKey})
	if err != nil {
		return backup.Manifest{}, err
	}
	chainHead, chainHash, chainVerified := snapshotLog.ChainHead()
	chainSequence, chainOffset := chainHead.Sequence, chainHead.Offset
	ledgerAggregate := usage.NewAggregate()
	ledgerState := ledger.NewState()
	replayedWatermark, replayErr := snapshotLog.Replay(ledger.Watermark{}, func(record ledger.Record) error {
		if err := ledgerState.Apply(record); err != nil {
			return err
		}
		return ledgerAggregate.Apply(record)
	})
	closeErr := snapshotLog.Close()
	if err := errors.Join(replayErr, closeErr); err != nil {
		return backup.Manifest{}, err
	}
	if replayedWatermark != ledgerWatermark {
		return backup.Manifest{}, errors.New("ledger snapshot watermark changed during replay")
	}
	if err := metadata.ValidateDeploymentPriceReferences(ledgerState); err != nil {
		return backup.Manifest{}, fmt.Errorf("validate pricing references before backup: %w", err)
	}
	// A stored checkpoint this build cannot read is a position the archive
	// cannot name, not a reason to refuse the archive. The Usage checkpoint is
	// a rebuildable derivative of the Ledger, and startup already answers this
	// exact question that way: restoreUsageAggregate clears both derivatives
	// and replays the WAL, whatever the reason. Answering it differently here
	// gave one data directory two verdicts.
	//
	// It also made the previous release's own data directory unbackupable at
	// the moment a backup matters most. The checkpoint payload version moved
	// 7 -> 8 in v0.5.0, so a v0.4.0 directory reaches this branch on the first
	// `backup create` after the upgrade — and by then Open has already migrated
	// the metadata schema past what the v0.4.0 binary will open, so neither
	// binary could produce a backup. Recording no checkpoint position costs
	// nothing: metadata.db is archived byte for byte, so a restored copy rebuilds
	// exactly as the source directory would on its next start.
	//
	// A store-level failure stays fatal. That is a broken database rather than
	// an unreadable derivative, and it is not a state a previous release
	// legitimately produces.
	checkpoint, checkpointHead, err := metadata.UsageCheckpoint()
	if errors.Is(err, boltstore.ErrNotFound) {
		checkpoint = ledger.Watermark{}
	} else if err != nil {
		return backup.Manifest{}, err
	} else {
		checkpointAggregate, restoreErr := usage.RestoreCheckpoint(
			checkpointHead, metadata.UsageCheckpointSegmentPayload)
		if restoreErr != nil || checkpointAggregate.Snapshot().Watermark != checkpoint {
			checkpoint = ledger.Watermark{}
		}
	}
	if checkpoint.Sequence > ledgerWatermark.Sequence ||
		checkpoint.Offset > ledgerWatermark.Offset {
		return backup.Manifest{}, errors.New("usage checkpoint is ahead of the Ledger")
	}
	usageManifestVersion := 0
	descriptorDigest := ""
	if cfg.Storage.MasterKey.Mode == config.MasterKeyModeKeySlots {
		descriptor, err := metadata.KeySlotDescriptor(ctx)
		if err != nil {
			return backup.Manifest{}, err
		}
		if descriptor.MasterKeyFingerprint != masterFingerprint || !descriptor.ProductionReady() {
			return backup.Manifest{}, errors.New("backup Key Slot descriptor does not match the verified Master Key")
		}
		encodedDescriptor, err := json.Marshal(descriptor)
		if err != nil {
			return backup.Manifest{}, err
		}
		digest := sha256.Sum256(encodedDescriptor)
		descriptorDigest = hex.EncodeToString(digest[:])
	}
	var usageManifest *usage.Manifest
	exporter, err := usage.NewExporter(cfg.UsagePath())
	if err != nil {
		return backup.Manifest{}, err
	}
	if loaded, err := exporter.LoadManifest(); err == nil {
		if err := exporter.Verify(pointerTo(ledgerAggregate.Snapshot())); err != nil {
			return backup.Manifest{}, fmt.Errorf("verify usage before backup: %w", err)
		}
		usageManifest = &loaded
		usageManifestVersion = loaded.SchemaVersion
	} else if !errors.Is(err, os.ErrNotExist) {
		return backup.Manifest{}, err
	}
	files := []backup.SourceFile{
		{ArchivePath: "config/config.yaml", LocalPath: configPath},
		{ArchivePath: "data/metadata.db", LocalPath: metadataSnapshot},
		{ArchivePath: "data/ledger/ledger.wal", LocalPath: ledgerSnapshot},
		{ArchivePath: "data/audit/audit.log", LocalPath: cfg.AuditPath()},
	}
	for _, name := range stagedSegments {
		files = append(files, backup.SourceFile{
			ArchivePath: "data/ledger/" + name, LocalPath: filepath.Join(staging, name),
		})
	}
	usageFiles, err := backupUsageFiles(cfg.UsagePath(), usageManifest)
	if err != nil {
		return backup.Manifest{}, err
	}
	files = append(files, usageFiles...)
	objectFiles, err := backupProviderObjectFiles(ctx, metadata, filepath.Join(cfg.Storage.DataDir, "provider-objects"))
	if err != nil {
		return backup.Manifest{}, err
	}
	files = append(files, objectFiles...)
	return backup.Create(backup.CreateOptions{
		OutputPath: outputPath, BackupKey: backupKey, Files: files,
		Metadata: metadataInfo, LedgerWatermark: ledgerWatermark,
		LedgerChainHeadSequence: chainSequence, LedgerChainHeadOffset: chainOffset,
		LedgerChainHeadHash: chainHash, LedgerChainVerified: chainVerified,
		CheckpointWatermark: checkpoint, UsageManifestVersion: usageManifestVersion,
		LedgerFeatureEpoch: metadataInfo.LedgerFeatureEpoch, MinimumLedgerReaderVersion: metadataInfo.MinimumLedgerReaderVersion,
		PricingStateSHA256: pricingBackupState.StateSHA256, PendingIntentSHA256: pricingBackupState.PendingIntentSHA256, PendingIntents: pricingBackupState.PendingIntents,
		MasterKeyFingerprint: masterFingerprint, Build: buildinfo.Current(),
		KeySlotDescriptorSHA256: descriptorDigest,
	})
}

func backupProviderObjectFiles(ctx context.Context, metadata *boltstore.Store, root string) ([]backup.SourceFile, error) {
	resources, err := metadata.ListProviderResources(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(resources))
	files := make([]backup.SourceFile, 0, len(resources))
	for _, resource := range resources {
		if resource.ObjectPath == "" {
			continue
		}
		if filepath.IsAbs(resource.ObjectPath) || filepath.Base(resource.ObjectPath) != resource.ObjectPath ||
			filepath.Clean(resource.ObjectPath) != resource.ObjectPath {
			return nil, fmt.Errorf("provider resource %q has an unsafe object path", resource.ID)
		}
		if _, exists := seen[resource.ObjectPath]; exists {
			continue
		}
		localPath := filepath.Join(root, resource.ObjectPath)
		info, err := os.Lstat(localPath)
		if err != nil {
			return nil, fmt.Errorf("inspect provider resource object %q: %w", resource.ID, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("provider resource object %q is not a regular file", resource.ID)
		}
		seen[resource.ObjectPath] = struct{}{}
		files = append(files, backup.SourceFile{
			ArchivePath: "data/provider-objects/" + filepath.ToSlash(resource.ObjectPath),
			LocalPath:   localPath,
		})
	}
	return files, nil
}

func backupUsageFiles(root string, manifest *usage.Manifest) ([]backup.SourceFile, error) {
	if manifest == nil {
		return nil, nil
	}
	relativePaths := []string{"manifest.json"}
	for _, entry := range manifest.Files {
		relativePaths = append(relativePaths, entry.Path)
	}
	files := make([]backup.SourceFile, 0, len(relativePaths))
	for _, relative := range relativePaths {
		if relative == "" || filepath.IsAbs(relative) {
			return nil, errors.New("usage manifest contains an unsafe path")
		}
		clean := filepath.Clean(filepath.FromSlash(relative))
		if clean != filepath.FromSlash(relative) || clean == ".." ||
			strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
			return nil, errors.New("usage manifest path escapes usage directory")
		}
		localPath := filepath.Join(root, clean)
		info, err := os.Lstat(localPath)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("usage backup source is not regular file %q", localPath)
		}
		files = append(files, backup.SourceFile{
			ArchivePath: "data/usage/" + filepath.ToSlash(clean),
			LocalPath:   localPath,
		})
	}
	return files, nil
}

func pointerTo[T any](value T) *T { return &value }

func appendBackupAudit(
	ctx context.Context,
	store *boltstore.Store,
	log *audit.Log,
	outcome, reason string,
) error {
	eventID, err := id.New("aud")
	if err != nil {
		return err
	}
	_, err = log.Append(ctx, audit.Event{
		EventID: eventID, OccurredAt: time.Now().UTC(), ActorType: "local_cli",
		Action: "backup.create", TargetType: "gateway", TargetID: "local",
		Outcome: outcome, ReasonCode: reason,
	})
	if err != nil {
		return err
	}
	return checkpointAudit(store, log.Summary())
}

func pathWithin(candidate, directory string) bool {
	relative, err := filepath.Rel(filepath.Clean(directory), filepath.Clean(candidate))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}
