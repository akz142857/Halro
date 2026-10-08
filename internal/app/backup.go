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
	"reflect"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/buildinfo"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/id"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/masterkey"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
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
	return CreateBackupWithOptions(ctx, cfg, configPath, outputPath, backupKey, CreateBackupOptions{})
}

type CreateBackupOptions struct {
	Replica bool
}

func CreateBackupWithOptions(
	ctx context.Context,
	cfg config.Config,
	configPath string,
	outputPath string,
	backupKey []byte,
	options CreateBackupOptions,
) (backup.Manifest, error) {
	return createBackupWithWriter(ctx, cfg, configPath, outputPath, backupKey, options, backup.Create)
}

func createBackupWithWriter(ctx context.Context, cfg config.Config, configPath, outputPath string, backupKey []byte, options CreateBackupOptions, writer func(backup.CreateOptions) (backup.Manifest, error)) (result backup.Manifest, resultErr error) {
	ctx, _, commandScopeErr := commandBudgetContext(ctx, cfg)
	if commandScopeErr != nil {
		var commandScopeZero0 backup.Manifest
		return commandScopeZero0, commandScopeErr
	}

	if err := ctx.Err(); err != nil {
		return backup.Manifest{}, err
	}
	if options.Replica {
		if cfg.Replication == nil {
			return backup.Manifest{}, errors.New("backup --replica requires replication configuration")
		}
		if err := replication.RequireMemberRuntime(cfg.Storage.DataDir, true, "backup create --replica"); err != nil {
			return backup.Manifest{}, err
		}
	} else if cfg.Replication != nil {
		return backup.Manifest{}, errors.New("an HA member backup requires explicit --replica on a stopped Replica")
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
	if err := checkOperationDiskHeadroom(ctx, cfg, filepath.Dir(absoluteOutput)); err != nil {
		return backup.Manifest{}, err
	}
	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return backup.Manifest{}, fmt.Errorf("acquire offline backup lock: %w", err)
	}
	ctx = withCommandDataLock(ctx, dataLock)
	var nativeDisk *commandNativeDiskOwner
	defer func() {
		resultErr = nativeDisk.finish(resultErr)
		resultErr = finishCommandDataLock(dataLock, resultErr)
		if resultErr != nil {
			result = backup.Manifest{}
		}
	}()
	if options.Replica {
		if err := refuseLegacySeedMemoryState(ctx, cfg); err != nil {
			return backup.Manifest{}, err
		}
	}
	var commandPressure *resourcePressureRuntime
	if cfg.ResourceBudget != nil {
		_, commandPressure, err = commandBudgetContext(ctx, cfg)
		if err != nil {
			return backup.Manifest{}, err
		}
	}
	nativeDisk, err = prepareCommandNativeDisk(ctx, cfg, dataLock)
	if err != nil {
		return backup.Manifest{}, err
	}
	var metadata *boltstore.Store
	if options.Replica {
		metadata, err = openCommandMetadataReplica(ctx, cfg, cfg.MetadataPath())
	} else {
		metadata, err = openCommandMetadataStore(ctx, cfg, cfg.MetadataPath())
	}
	if err != nil {
		return backup.Manifest{}, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, metadata.Close())
		if resultErr != nil {
			result = backup.Manifest{}
		}
	}()
	var masterKey []byte
	if options.Replica {
		if cfg.Storage.MasterKey.Mode == config.MasterKeyModeKeySlots {
			// Borrow the already opened actual projection solely for candidate
			// key inspection; a second bbolt descriptor would wait on this
			// owner's exclusive lock. Member/source authentication follows.
			masterKey, err = unlockKMSMasterKey(ctx, cfg, metadata, masterkey.KeySlotPrimary, defaultKMSWrapperFactory)
		} else {
			masterKey, err = unlockMemberMasterKey(ctx, cfg)
		}
	} else {
		masterKey, err = unlockMasterKey(ctx, cfg, metadata)
	}
	if err != nil {
		return backup.Manifest{}, err
	}
	fingerprint := sha256.Sum256(masterKey)
	fingerprintText := "sha256:" + hex.EncodeToString(fingerprint[:])
	var memberState *replication.MemberState
	var memberClusterKey []byte
	if options.Replica {
		state, derived, stateErr := readMemberState(cfg, masterKey)
		if stateErr != nil {
			clear(masterKey)
			return backup.Manifest{}, stateErr
		}
		defer clear(derived[:])
		if state.Role != replication.RoleReplica || state.AppliedIndex != state.DurableIndex {
			clear(masterKey)
			return backup.Manifest{}, errors.New("Replica backup requires role=replica and applied_index == durable_index")
		}
		memberState = &state
		memberClusterKey = append([]byte(nil), derived[:]...)
		clear(derived[:])
		defer clear(memberClusterKey)
	}
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
	if err != nil {
		secretVault.Close()
		clear(masterKey)
		clear(auditKey)
		return backup.Manifest{}, err
	}
	if options.Replica {
		governanceKey, deriveErr := vault.DeriveGovernanceHMACKey(ledgerKey)
		if deriveErr != nil {
			secretVault.Close()
			clear(masterKey)
			clear(auditKey)
			clear(ledgerKey)
			return backup.Manifest{}, deriveErr
		}
		metadataKey, loadErr := loadMetadataJournalHMACKey(metadata, secretVault, masterKey)
		if loadErr == nil {
			loadErr = recoverReplicaForBackup(ctx, cfg, *memberState, memberClusterKey, ledgerKey, auditKey, governanceKey, metadataKey, commandPressure)
		}
		clear(governanceKey)
		clear(metadataKey)
		if loadErr != nil {
			secretVault.Close()
			clear(masterKey)
			clear(auditKey)
			clear(ledgerKey)
			return backup.Manifest{}, loadErr
		}
	}
	secretVault.Close()
	clear(masterKey)
	var auditLog *audit.Log
	if options.Replica {
		auditLog, err = openConfiguredAuditLog(cfg, cfg.AuditPath(), auditKey, audit.Options{Replica: true, RequireExisting: true}, commandPressure.logicalBudget())
	} else {
		auditLog, err = openOneShotConfiguredAuditLog(ctx, cfg, cfg.AuditPath(), auditKey, audit.Options{})
	}
	clear(auditKey)
	if err != nil {
		clear(ledgerKey)
		return backup.Manifest{}, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, auditLog.Close())
		if resultErr != nil {
			result = backup.Manifest{}
		}
	}()
	if err := verifyBackupAuditCheckpoint(metadata, auditLog.Summary(), options.Replica); err != nil {
		clear(ledgerKey)
		return backup.Manifest{}, err
	}
	if !options.Replica {
		if err := appendBackupAudit(ctx, metadata, auditLog, "requested", ""); err != nil {
			clear(ledgerKey)
			return backup.Manifest{}, err
		}
	}
	manifest, createErr := createBackupSnapshot(
		ctx, cfg, absoluteConfig, absoluteOutput, backupKey, metadata, fingerprintText, ledgerKey, memberState, memberClusterKey, commandPressure, writer,
	)
	clear(ledgerKey)
	if options.Replica {
		return manifest, createErr
	}
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

func recoverReplicaForBackup(
	ctx context.Context,
	cfg config.Config,
	state replication.MemberState,
	clusterKey, ledgerKey, auditKey, governanceKey, metadataKey []byte,
	pressure *resourcePressureRuntime,
) (resultErr error) {
	indexDiskOwner, indexLimit, err := newOneShotLedgerSegmentIndexDiskOwnerWithPressure(ctx, cfg, pressure)
	if err != nil {
		return err
	}
	var source *replication.NativeSource
	defer func() {
		resultErr = retireCommandLedgerIndex(indexDiskOwner, source, resultErr)
	}()
	journal, err := replication.OpenExistingOrderingJournalWithOptions(
		cfg.OrderingJournalPath(), clusterKey, state.ClusterID, state.Incarnation,
		state.DurableIndex, state.OrderingHeadMAC, orderingJournalOptions(cfg),
	)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, journal.Close()) }()
	head, _, headMAC := journal.Head()
	if head != state.DurableIndex || headMAC != state.OrderingHeadMAC {
		return errors.New("Replica backup requires runtime recovery because ordering is ahead of authenticated member state")
	}
	nativeOptions := commandNativeOptions(ctx, ledger.Options{})
	source, err = replication.NewNativeSource(replication.NativeSourceOptions{
		LedgerPath: cfg.LedgerPath(), AuditPath: cfg.AuditPath(), GovernancePath: cfg.GovernancePath(),
		MetadataPath: filepathMetadataJournal(cfg), ProviderObjectDir: providerObjectSourceDir(cfg),
		LedgerKey: ledgerKey, AuditKey: auditKey, GovernanceKey: governanceKey, MetadataKey: metadataKey,
		LedgerDirectoryIndexMaxBytes: indexLimit,
		ReserveLedgerFileDisk:        nativeOptions.ReserveNativeFileDisk,
	})
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, source.Close()) }()
	if err := replication.RecoverReplicaStores(journal, source); err != nil {
		return fmt.Errorf("reconcile Replica stores before backup: %w", err)
	}
	want, err := journal.StoreCursorsThrough(state.AppliedIndex)
	if err != nil {
		return err
	}
	for store := replication.StoreLedger; store <= replication.StoreMetadata; store++ {
		actual, err := source.Cursor(store)
		if err != nil {
			return err
		}
		if actual != want[store-replication.StoreLedger] {
			return fmt.Errorf("Replica backup store %s ended at %d/%d, want authenticated applied prefix %d/%d",
				replicationStoreName(store), actual.Generation, actual.Sequence,
				want[store-replication.StoreLedger].Generation, want[store-replication.StoreLedger].Sequence)
		}
	}
	return nil
}

func verifyBackupAuditCheckpoint(metadata *boltstore.Store, summary audit.Summary, readOnly bool) error {
	if !readOnly {
		return reconcileAuditCheckpoint(metadata, summary)
	}
	checkpoint, err := metadata.AuditCheckpoint()
	if err != nil {
		return fmt.Errorf("load audit checkpoint: %w", err)
	}
	if checkpoint.Records > summary.Records || checkpoint.Records == summary.Records &&
		(checkpoint.Bytes != summary.Bytes || checkpoint.LastHash != summary.LastHash) {
		return errors.New("audit log does not match its trusted checkpoint")
	}
	return nil
}

func replicationStoreName(store replication.Store) string {
	switch store {
	case replication.StoreLedger:
		return "ledger"
	case replication.StoreAudit:
		return "audit"
	case replication.StoreGovernance:
		return "governance"
	case replication.StoreMetadata:
		return "metadata"
	default:
		return "unknown"
	}
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
	ClusterIncarnation             string   `json:"cluster_incarnation,omitempty"`
	ClusterRole                    string   `json:"cluster_role,omitempty"`
}

type RestoreOptions struct {
	UseRecoverySlot     bool
	ConfirmRecoverySlot string
	NewIncarnation      string
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
	return restoreBackupWithExtract(ctx, cfg, archivePath, backupKey, confirmBackupID, options, factory, nil)
}

func restoreBackupWithExtract(
	ctx context.Context,
	cfg config.Config,
	archivePath string,
	backupKey []byte,
	confirmBackupID string,
	options RestoreOptions,
	factory kmsWrapperFactory,
	extract func(string, []byte, string) (backup.Manifest, error),
) (result RestoreResult, resultErr error) {
	ctx, finish, commandScopeErr := restoreReportContext(ctx, cfg)
	if commandScopeErr != nil {
		return RestoreResult{}, commandScopeErr
	}
	defer func() {
		restoreReportConstructorDebt(ctx, resultErr)
		resultErr = errors.Join(resultErr, finish(), ctx.Err())
		if resultErr != nil {
			result = RestoreResult{}
		}
	}()

	ctx, commandScopeErr = backupWorkspaceContext(ctx, cfg)
	if commandScopeErr != nil {
		return RestoreResult{}, commandScopeErr
	}

	if err := ctx.Err(); err != nil {
		return RestoreResult{}, err
	}
	var err error
	var commandPressure *resourcePressureRuntime
	var manifest backup.Manifest
	var verified *backup.VerifiedArchive
	if extract == nil {
		verified, err = backup.OpenVerified(ctx, archivePath, backupKey)
		if err != nil {
			return RestoreResult{}, err
		}
		defer func() {
			if err := restoreReportClose(ctx, verified); err != nil {
				resultErr = errors.Join(resultErr, err)
				result = RestoreResult{}
			}
		}()
		summary := verified.Summary()
		manifest = summary.Manifest
		// Domain validators and HA selection use the validated compatibility
		// contract, never infer Replica solely from the new container version.
		manifest.FormatVersion = summary.CompatibilityVersion
	} else {
		// Per-operation legacy extraction seam used by existing confirmation tests.
		manifest, err = backup.VerifyContext(ctx, archivePath, backupKey)
		if err != nil {
			return RestoreResult{}, err
		}
	}
	if confirmBackupID == "" || confirmBackupID != manifest.BackupID {
		return RestoreResult{}, errors.New("restore confirmation must exactly match the verified backup id")
	}
	var restorePayloadBytes int64
	if verified != nil {
		restorePayloadBytes, err = verified.PayloadBytes()
	} else {
		for _, file := range manifest.Files {
			if file.Size < 0 || file.Size > int64(^uint64(0)>>1)-restorePayloadBytes {
				return RestoreResult{}, errors.New("restore payload byte count overflow")
			}
			restorePayloadBytes += file.Size
		}
	}
	if err != nil {
		return RestoreResult{}, err
	}
	isHABackup := manifest.FormatVersion == 4
	if isHABackup {
		if cfg.Replication == nil || options.NewIncarnation == "" || options.NewIncarnation == manifest.ClusterIncarnation {
			return RestoreResult{}, errors.New("HA restore requires replication configuration and a new --incarnation")
		}
	} else if cfg.Replication != nil {
		return RestoreResult{}, errors.New("a Standalone backup cannot be restored directly as an HA member; restore Standalone then seed the cluster")
	} else if options.NewIncarnation != "" {
		return RestoreResult{}, errors.New("--incarnation is valid only for an HA backup restore")
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
	if err := checkOperationDiskHeadroom(ctx, cfg, dataParent); err != nil {
		return RestoreResult{}, err
	}
	if cfg.ResourceBudget != nil {
		_, commandPressure, err = commandBudgetContext(ctx, cfg)
		if err != nil {
			return RestoreResult{}, err
		}
	}
	restoreWorkspacePermit, err := acquireRestoreWorkspaceDisk(ctx, cfg, commandPressure, restorePayloadBytes)
	if err != nil {
		return RestoreResult{}, err
	}
	indexDiskOwner, indexLimit, err := newOneShotLedgerSegmentIndexDiskOwnerWithPressure(ctx, cfg, commandPressure)
	if err != nil {
		return RestoreResult{}, errors.Join(err, restoreWorkspacePermit.Release())
	}
	defer func() {
		if debt := restoreReportReady(ctx, resultErr); debt != nil {
			resultErr = errors.Join(resultErr, debt)
		} else {
			resultErr = errors.Join(resultErr, indexDiskOwner.Release())
		}
		if resultErr != nil {
			result = RestoreResult{}
		}
	}()

	stagingRoot, err := os.MkdirTemp(dataParent, ".halro-restore-stage-*")
	if err != nil {
		return RestoreResult{}, errors.Join(err, restoreWorkspacePermit.Release())
	}
	stagingIdentity, identityErr := os.Lstat(stagingRoot)
	parentIdentity, parentErr := os.Lstat(dataParent)
	if err := errors.Join(identityErr, parentErr); err != nil {
		restoreReportDebt(ctx, err)
		return RestoreResult{}, err
	}
	defer func() {
		cleanupErr := retireRestoreStage(ctx, stagingRoot, stagingIdentity, parentIdentity, resultErr)
		restoreReportDebt(ctx, cleanupErr)
		resultErr = errors.Join(resultErr, cleanupErr)
		if cleanupErr == nil {
			resultErr = errors.Join(resultErr, restoreWorkspacePermit.Release())
		}
		if resultErr != nil {
			result = RestoreResult{}
		}
	}()
	if err := os.Chmod(stagingRoot, 0o700); err != nil {
		return RestoreResult{}, err
	}
	extractRoot := filepath.Join(stagingRoot, "extract")
	if verified != nil {
		if err := verified.Extract(ctx, extractRoot); err != nil {
			return RestoreResult{}, err
		}
		// Before native validation or live publication, retire the ciphertext and
		// all derivative owners. The first/second stream fence binds confirmation.
		if err := restoreReportClose(ctx, verified); err != nil {
			return RestoreResult{}, err
		}
	} else {
		extractedManifest, err := extract(archivePath, backupKey, extractRoot)
		if err != nil {
			return RestoreResult{}, err
		}
		if !reflect.DeepEqual(manifest, extractedManifest) {
			return RestoreResult{}, errors.New("backup manifest changed after restore confirmation")
		}
	}
	stageData := filepath.Join(extractRoot, "data")
	if isHABackup {
		// Archived member files are evidence only. Reusing their term,
		// incarnation or ordering prefix would let a retained old PVC rejoin the
		// restored cluster.
		if err := os.RemoveAll(filepath.Join(stageData, replication.ClusterDirectoryName)); err != nil {
			return RestoreResult{}, fmt.Errorf("remove archived cluster evidence before restore: %w", err)
		}
	}
	archiveMetadata := filepath.Join(stageData, "metadata.db")
	stageMetadata := filepath.Join(stageData, cfg.Storage.MetadataFile)
	if archiveMetadata != stageMetadata {
		if err := os.Rename(archiveMetadata, stageMetadata); err != nil {
			return RestoreResult{}, err
		}
	}
	// The archived journal is evidence, not state to resume. A restore is a
	// whole-file publish of the projection, so the chain the archive carries
	// stops describing the database the moment it is published (§6.1.4, §12.2).
	// It is withdrawn before validation, because validation is also where the
	// restored directory's own epoch is opened — and that has to be a new one,
	// not a continuation of somebody else's chain.
	if err := os.Remove(filepath.Join(stageData, boltstore.MetadataJournalFileName)); err != nil &&
		!errors.Is(err, os.ErrNotExist) {
		return RestoreResult{}, fmt.Errorf("withdraw the archived metadata journal: %w", err)
	}
	if err := validateRestoreStage(ctx, cfg, stageData, manifest, purpose, factory, indexLimit, commandPressure); err != nil {
		return RestoreResult{}, err
	}
	stageStore, err := openCommandMetadataPublish(ctx, cfg, stageMetadata)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("open staged metadata for authentication invalidation: %w", err)
	}
	schemaVersionAfter, schemaErr := stageStore.SchemaVersion()
	restoredEnabledGatewayKeyIDs, restoredEnabledGatewayKeyCount, gatewayKeysErr := collectRestoreGatewayIDs(ctx, stageStore)

	quarantined, quarantineErr := stageStore.QuarantineRestoredScheduledPrices(ctx, manifest.CreatedAt.UTC(), time.Now().UTC())
	invalidateErr := stageStore.InvalidateAdminAuthenticationForRestore(ctx)
	closeErr := restoreReportClose(ctx, stageStore)
	if err := errors.Join(schemaErr, gatewayKeysErr, quarantineErr, invalidateErr, closeErr); err != nil {
		return RestoreResult{}, fmt.Errorf("invalidate restored admin authentication: %w", err)
	}
	if isHABackup {
		stageCfg := cfg
		stageCfg.Storage.DataDir = stageData
		if err := EstablishMemberState(ctx, stageCfg, replication.RolePrimary, options.NewIncarnation, 1); err != nil {
			return RestoreResult{}, fmt.Errorf("establish restored HA incarnation: %w", err)
		}
	}

	if err := restoreReportReady(ctx, nil); err != nil {
		return RestoreResult{}, err
	}
	liveLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("acquire offline restore lock: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, restoreReportClose(ctx, liveLock)) }()
	stageLock, err := lock.Acquire(stageData)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("lock staged restore: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, restoreReportClose(ctx, stageLock)) }()
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
	if err := durable.SyncDirectory(dataParent); err != nil {
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
		rollbackSyncErr := durable.SyncDirectory(dataParent)
		return RestoreResult{}, errors.Join(publicationErr, rollbackSyncErr)
	}
	return RestoreResult{
		BackupID: manifest.BackupID, DataDir: cfg.Storage.DataDir,
		PreviousDataDir: previousDataDir, LedgerSequence: manifest.LedgerWatermark.Sequence,
		UnlockPath: unlockPath, VaultVerified: true, RecoveryAudited: options.UseRecoverySlot,
		QuarantinedScheduledPrices: quarantined,
		SchemaVersionBefore:        manifest.Metadata.SchemaVersion, SchemaVersionAfter: schemaVersionAfter,
		RestoredEnabledGatewayKeyCount: restoredEnabledGatewayKeyCount,
		RestoredEnabledGatewayKeyIDs:   restoredEnabledGatewayKeyIDs,
		ClusterIncarnation:             options.NewIncarnation,
		ClusterRole: func() string {
			if isHABackup {
				return string(replication.RolePrimary)
			}
			return ""
		}(),
	}, nil
}

func validateRestoreStage(
	ctx context.Context,
	cfg config.Config,
	stageData string,
	manifest backup.Manifest,
	purpose masterkey.KeySlotPurpose,
	factory kmsWrapperFactory,
	indexLimit int64,
	pressure *resourcePressureRuntime,
) (resultErr error) {
	ctx, _, commandScopeErr := commandBudgetContext(ctx, cfg)
	if commandScopeErr != nil {

		return commandScopeErr
	}
	var err error
	pressure, err = commandPressure(ctx, cfg, pressure)
	if err != nil {
		return err
	}
	stageMetadataPath := filepath.Join(stageData, cfg.Storage.MetadataFile)
	metadataManager, err := reserveCommandMetadataFile(ctx, cfg, stageMetadataPath)
	if err != nil {
		return err
	}
	legacyPricingState, legacyStateErr := boltstore.LegacyPricingBackupStateWithMappingBudget(ctx, stageMetadataPath, metadataLegacyFileCeiling(cfg), metadataManager)
	if err := restoreReportReady(ctx, legacyStateErr); err != nil {
		return err
	}
	metadata, err := openCommandMetadataPublish(ctx, cfg, filepath.Join(stageData, cfg.Storage.MetadataFile))
	if err != nil {
		return fmt.Errorf("open staged metadata: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, restoreReportClose(ctx, metadata)) }()
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
	if err != nil {
		secretVault.Close()
		clear(masterKey)
		clear(auditKey)
		return err
	}
	defer clear(ledgerKey)
	// Open the restored projection's journal epoch here, while the staged tree
	// is still staged: the rename below publishes the database and its journal
	// together, so there is no window in which a data directory follows an
	// epoch whose file does not exist. Deferring it to the first start left
	// exactly that window, and `halro doctor` was right to call it a
	// divergence — which is how it was found.
	journalErr := attachStagedMetadataJournal(metadata, secretVault, masterKey, cfg)
	secretVault.Close()
	clear(masterKey)
	if journalErr != nil {
		clear(auditKey)
		clear(ledgerKey)
		return journalErr
	}
	governanceKey, err := vault.DeriveGovernanceHMACKey(ledgerKey)
	if err != nil {
		clear(auditKey)
		clear(ledgerKey)
		return err
	}
	defer clear(governanceKey)
	auditLog, err := openConfiguredAuditLog(cfg, filepath.Join(stageData, "audit", "audit.log"), auditKey, audit.Options{}, pressure.logicalBudget())
	clear(auditKey)
	if err != nil {
		return fmt.Errorf("verify staged Audit: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, restoreReportClose(ctx, auditLog)) }()
	if err := reconcileAuditCheckpoint(metadata, auditLog.Summary()); err != nil {
		return fmt.Errorf("verify staged Audit checkpoint: %w", err)
	}
	if recorder := kmsAuditRecorderFromContext(ctx); recorder != nil {
		if err := appendKMSProviderAuditAs(ctx, auditLog, metadata, recorder, "local_cli"); err != nil {
			return fmt.Errorf("append restore KMS provider Audit: %w", err)
		}
	}
	status := ledger.NewStatus()
	ledgerLog, err := ledger.OpenWithOptions(filepath.Join(stageData, "ledger", "ledger.wal"), status, ledger.Options{
		ChainKey: ledgerKey, MaxSegmentDirectoryIndexBytes: indexLimit,
	})
	if err != nil {
		return fmt.Errorf("open staged Ledger: %w", err)
	}
	chainHead, chainHash, chainVerified := ledgerLog.ChainHead()
	chainSequence, chainOffset := chainHead.Sequence, chainHead.Offset
	report, partial, replayErr := ledger.InspectReplayAuthenticated(filepath.Join(stageData, "ledger", "ledger.wal"), ledgerKey, func(record ledger.Record) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return nil
	})
	watermark := report.Head
	closeErr := restoreReportClose(ctx, ledgerLog)
	if err := errors.Join(replayErr, closeErr); err != nil {
		return fmt.Errorf("replay staged Ledger: %w", err)
	}
	if partial || watermark != manifest.LedgerWatermark {
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
	if report.ChainSequence != chainSequence || report.ChainOffset != chainOffset || report.ChainHash != chainHash || report.ChainVerified != chainVerified {
		return errors.New("staged Ledger chain changed during authenticated replay")
	}
	if manifest.FormatVersion >= 3 {
		governanceLog, err := governance.OpenWithOptions(filepath.Join(stageData, "governance", "governance.journal"), governanceKey, governanceLogOptions(cfg, pressure.logicalBudget(), governance.Options{}))
		if err != nil {
			return fmt.Errorf("open staged Governance Journal: %w", err)
		}
		expected := governance.Summary{Records: manifest.GovernanceSequence, Bytes: manifest.GovernanceOffset, LastHash: manifest.GovernanceHeadHash}
		verifyErr := validateBackupGovernanceProjection(ctx, cfg, metadata, governanceLog, filepath.Join(stageData, "governance", "governance.journal"), governanceKey, expected, filepath.Dir(stageData))
		closeErr := restoreReportClose(ctx, governanceLog)
		if err := errors.Join(verifyErr, closeErr); err != nil {
			return fmt.Errorf("verify staged Governance Journal: %w", err)
		}
	}
	if _, err := withBackupPricingAndUsageReference(ctx, cfg, metadata, filepath.Join(stageData, "usage"), filepath.Join(stageData, "ledger", "ledger.wal"), ledgerKey, report, filepath.Dir(stageData), pressure, nil); err != nil {
		return fmt.Errorf("verify staged pricing and Usage: %w", err)
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

func createBackupSnapshot(
	ctx context.Context,
	cfg config.Config,
	configPath, outputPath string,
	backupKey []byte,
	metadata *boltstore.Store,
	masterFingerprint string,
	ledgerKey []byte,
	memberState *replication.MemberState,
	memberClusterKey []byte,
	pressure *resourcePressureRuntime,
	writer func(backup.CreateOptions) (backup.Manifest, error),
) (backup.Manifest, error) {
	if scope, ok := ctx.Value(commandResourceScopeKey{}).(*commandResourceScope); ok && scope.selected != nil {
		return createBackupSnapshotWithLedgerWriter(ctx, cfg, configPath, outputPath, backupKey, metadata, masterFingerprint, scope.selected.state.generation.Owners.NativeLedger, ledgerKey, memberState, memberClusterKey, writer, pressure)
	}
	status := ledger.NewStatus()
	indexDiskOwner, indexLimit, err := newOneShotLedgerSegmentIndexDiskOwnerWithPressure(ctx, cfg, pressure)
	if err != nil {
		return backup.Manifest{}, err
	}
	var ledgerLog *ledger.Log
	if memberState != nil {
		ledgerLog, err = ledger.OpenWithOptions(cfg.LedgerPath(), status, commandNativeOptions(ctx, ledger.Options{
			ChainKey: ledgerKey, Replica: true, RequireExisting: true, MaxSegmentDirectoryIndexBytes: indexLimit,
		}))
	} else {
		ledgerLog, err = ledger.OpenWithOptions(cfg.LedgerPath(), status, commandNativeOptions(ctx, ledger.Options{
			MaxSegmentDirectoryIndexBytes: indexLimit,
		}))
	}
	if err != nil {
		return backup.Manifest{}, retireCommandLedgerIndex(indexDiskOwner, ledgerLog, err)
	}
	manifest, createErr := createBackupSnapshotWithLedgerWriter(
		ctx, cfg, configPath, outputPath, backupKey, metadata, masterFingerprint, ledgerLog, ledgerKey, memberState, memberClusterKey, writer, pressure,
	)
	closeErr := ledgerLog.Close()
	return manifest, retireCommandLedgerIndex(indexDiskOwner, ledgerLog, errors.Join(createErr, closeErr))
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
	memberState *replication.MemberState,
	memberClusterKey []byte,
) (result backup.Manifest, resultErr error) {
	return createBackupSnapshotWithLedgerWriter(ctx, cfg, configPath, outputPath, backupKey, metadata, masterFingerprint, ledgerLog, ledgerKey, memberState, memberClusterKey, backup.Create)
}

func createBackupSnapshotWithLedgerWriter(
	ctx context.Context,
	cfg config.Config,
	configPath, outputPath string,
	backupKey []byte,
	metadata *boltstore.Store,
	masterFingerprint string,
	ledgerLog *ledger.Log,
	ledgerKey []byte,
	memberState *replication.MemberState,
	memberClusterKey []byte,
	writer func(backup.CreateOptions) (backup.Manifest, error),
	pressures ...*resourcePressureRuntime,
) (result backup.Manifest, resultErr error) {
	ctx, sharedPressure, scopeErr := commandBudgetContext(ctx, cfg)
	if scopeErr != nil {
		return backup.Manifest{}, scopeErr
	}
	if err := ctx.Err(); err != nil {
		return backup.Manifest{}, err
	}
	if ledgerLog == nil {
		return backup.Manifest{}, errors.New("open Ledger is required for backup snapshot")
	}
	var pressure *resourcePressureRuntime
	if len(pressures) > 1 {
		return backup.Manifest{}, errors.New("backup snapshot accepts one shared command manager")
	}
	if len(pressures) > 0 {
		pressure = pressures[0]
	}
	if pressure != nil && pressure.logicalBudget() != sharedPressure.logicalBudget() {
		return backup.Manifest{}, errors.New("backup snapshot substituted its command manager")
	}
	pressure = sharedPressure
	governanceKey, err := vault.DeriveGovernanceHMACKey(ledgerKey)
	if err != nil {
		return backup.Manifest{}, err
	}
	defer clear(governanceKey)
	var governanceLog *governance.Log
	borrowedGovernance := false
	if scope, ok := ctx.Value(commandResourceScopeKey{}).(*commandResourceScope); ok && scope.selected != nil {
		governanceLog, borrowedGovernance = scope.selected.state.generation.Owners.NativeGovernance, true
	} else if memberState != nil {
		governanceLog, err = governance.OpenWithOptions(cfg.GovernancePath(), governanceKey, governanceLogOptions(cfg, pressure.logicalBudget(), governance.Options{Replica: true, RequireExisting: true}))
	} else {
		governanceLog, err = governance.OpenWithOptions(cfg.GovernancePath(), governanceKey, governanceLogOptions(cfg, pressure.logicalBudget(), governance.Options{}))
	}
	if err != nil {
		return backup.Manifest{}, fmt.Errorf("open Governance Journal for backup: %w", err)
	}
	governanceSummary := governanceLog.Summary()
	verifyErr := validateBackupGovernanceProjection(ctx, cfg, metadata, governanceLog, cfg.GovernancePath(), governanceKey, governanceSummary, filepath.Dir(outputPath))
	if verifyErr == nil && memberState == nil && governanceSummary.Records > 0 {
		// Advance only after complete native/domain validation and scratch
		// retirement, before taking the Metadata snapshot. Replica is read-only.
		verifyErr = ctx.Err()
		if verifyErr == nil {
			head, authentication := governanceLog.AuthenticatedHead()
			if head != governanceSummary {
				verifyErr = errors.New("Governance head changed before backup anchor publication")
			} else {
				verifyErr = metadata.PutGovernanceJournalAnchor(head.Records, head.Bytes, head.LastHash, authentication)
			}
		}
	}
	var governanceCloseErr error
	if !borrowedGovernance {
		governanceCloseErr = governanceLog.Close()
	}
	if err := errors.Join(verifyErr, governanceCloseErr); err != nil {
		return backup.Manifest{}, fmt.Errorf("verify Governance Journal for backup: %w", err)
	}
	stagingOwner, err := prepareBackupStagingDisk(ctx, cfg, pressure)
	if err != nil {
		return backup.Manifest{}, fmt.Errorf("admit complete backup staging tree before creation: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, stagingOwner.finish(resultErr))
		resultErr = stagingDebt(stagingOwner, nil, resultErr)
		if resultErr != nil {
			result = backup.Manifest{}
		}
	}()
	var reserveStaging func(int64) (ledger.NativeFileDiskReservation, error)
	if stagingOwner != nil {
		reserveStaging = stagingOwner.reserve
		// The actual private root directory and namespace metadata have an
		// independent conservative envelope before MkdirTemp.
		if _, err := stagingOwner.reserve(64 << 10); err != nil {
			return backup.Manifest{}, err
		}
	}
	stagingParent, err := os.Lstat(filepath.Dir(outputPath))
	if err != nil || !stagingParent.IsDir() {
		return backup.Manifest{}, errors.Join(err, ownedfile.ErrReplaced)
	}
	staging, err := os.MkdirTemp(filepath.Dir(outputPath), ".halro-backup-stage-*")
	if err != nil {
		return backup.Manifest{}, err
	}
	stagingInfo, err := os.Lstat(staging)
	if err != nil || !stagingInfo.IsDir() {
		return backup.Manifest{}, errors.Join(err, ownedfile.ErrReplaced)
	}
	defer func() {
		// Unknown actual readers and replaced namespaces retain their private
		// artifacts. Missing names and RemoveAll never authorize fee refunds.
		if errors.Is(resultErr, ownedfile.ErrRetirement) || errors.Is(resultErr, ledger.ErrSegmentRetirement) || errors.Is(resultErr, boltstore.ErrMetadataFileRetirement) || errors.Is(resultErr, ownedfile.ErrReplaced) {
			resultErr = errors.Join(resultErr, ownedfile.ErrRetirement)
			return
		}
		parent, parentErr := os.Lstat(filepath.Dir(staging))
		current, currentErr := os.Lstat(staging)
		if parentErr != nil || currentErr != nil || !parent.IsDir() || !current.IsDir() || !os.SameFile(parent, stagingParent) || !os.SameFile(current, stagingInfo) {
			resultErr = errors.Join(resultErr, ownedfile.ErrRetirement, ownedfile.ErrReplaced, parentErr, currentErr)
			return
		}
		var cleanupErr error
		if stagingOwner != nil && stagingOwner.metadataImage != nil {
			cleanupErr = stagingOwner.metadataImage.Discard()
		}
		if cleanupErr == nil {
			cleanupErr = os.RemoveAll(staging)
		}
		if cleanupErr == nil {
			cleanupErr = durable.SyncDirectory(filepath.Dir(staging))
		}
		resultErr = errors.Join(resultErr, cleanupErr)
	}()
	if err := os.Chmod(staging, 0o700); err != nil {
		return backup.Manifest{}, err
	}
	metadataSnapshot := filepath.Join(staging, "metadata.db")
	if err := os.MkdirAll(filepath.Dir(metadataSnapshot), 0o700); err != nil {
		return backup.Manifest{}, err
	}
	metadataInfo, err := snapshotBackupMetadata(ctx, cfg, metadata, metadataSnapshot, stagingOwner)
	if err != nil {
		return backup.Manifest{}, err
	}
	// Copied after the snapshot, so the journal in the archive is at least as
	// long as the position the snapshot recorded. The other direction would be
	// a journal that cannot explain the database beside it.
	metadataJournalSnapshot := filepath.Join(staging, boltstore.MetadataJournalFileName)
	journalStaged := true
	if err := copyFileForBackupWithDisk(ctx, metadata.MetadataJournalPath(), metadataJournalSnapshot, stagingOwner); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return backup.Manifest{}, fmt.Errorf("stage metadata journal: %w", err)
		}
		// A data directory that has not started since the journal was
		// introduced has none yet, and the database is its own starting
		// projection. The manifest's zero epoch says so; inventing an empty
		// file would claim a chain that does not exist.
		journalStaged = false
	}
	pricingBackupState, err := metadata.PricingBackupState()
	if err != nil {
		return backup.Manifest{}, err
	}
	ledgerSnapshot := filepath.Join(staging, "ledger.wal")
	ledgerWatermark, err := ledgerLog.SnapshotWithDisk(ledgerSnapshot, reserveStaging)
	if err != nil {
		return backup.Manifest{}, err
	}
	// The sealed generations go with it. A backup of the active file alone
	// verifies perfectly and restores an installation whose balances begin at
	// whatever the last roll left behind — the worst shape a backup defect can
	// take, because nothing about the restored instance looks wrong.
	catalogPermit, catalogLimit, err := acquireBackupWorkspaceDisk(ctx, cfg, pressure)
	if err != nil {
		return backup.Manifest{}, err
	}
	sources, err := backup.NewSourceCatalogWithLimit(staging, catalogLimit)
	if err != nil {
		return backup.Manifest{}, errors.Join(err, catalogPermit.Release())
	}
	defer func() {
		closeErr := sources.Close()
		resultErr = errors.Join(resultErr, closeErr)
		if closeErr == nil {
			resultErr = errors.Join(resultErr, catalogPermit.Release())
		} else {
			resultErr = &backupStagingDebt{owner: stagingOwner, catalog: sources, cause: resultErr}
		}
		if resultErr != nil {
			result = backup.Manifest{}
		}
	}()
	for _, source := range []backup.SourceFile{
		{ArchivePath: "config/config.yaml", LocalPath: configPath},
		{ArchivePath: "data/metadata.db", LocalPath: metadataSnapshot},

		{ArchivePath: "data/ledger/ledger.wal", LocalPath: ledgerSnapshot},
		{ArchivePath: "data/audit/audit.log", LocalPath: cfg.AuditPath()},
		{ArchivePath: "data/governance/governance.journal", LocalPath: cfg.GovernancePath()},
	} {
		if err := sources.Add(source); err != nil {
			return backup.Manifest{}, err
		}
	}
	err = ledgerLog.StageSegmentsEachWithDisk(ctx, staging, reserveStaging, func(name string) error {
		return sources.Add(backup.SourceFile{
			ArchivePath: "data/ledger/" + name, LocalPath: filepath.Join(staging, name),
		})
	})
	if err != nil {
		return backup.Manifest{}, err
	}
	snapshotLog, err := ledger.OpenWithOptions(ledgerSnapshot, ledger.NewStatus(), ledger.Options{
		ChainKey: ledgerKey, ReserveNativeFileDisk: reserveStaging,
	})
	if err != nil {
		return backup.Manifest{}, err
	}
	chainHead, chainHash, chainVerified := snapshotLog.ChainHead()
	chainSequence, chainOffset := chainHead.Sequence, chainHead.Offset
	report, partial, replayErr := ledger.InspectReplayAuthenticated(ledgerSnapshot, ledgerKey, func(record ledger.Record) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return nil
	})
	replayedWatermark := report.Head
	closeErr := snapshotLog.Close()
	if err := errors.Join(replayErr, closeErr); err != nil {
		return backup.Manifest{}, err
	}
	if partial || replayedWatermark != ledgerWatermark {
		return backup.Manifest{}, errors.New("ledger snapshot watermark changed during replay")
	}
	if report.ChainSequence != chainSequence || report.ChainOffset != chainOffset || report.ChainHash != chainHash || report.ChainVerified != chainVerified {
		return backup.Manifest{}, errors.New("ledger snapshot chain changed during authenticated replay")
	}
	// Default Runtime reconstructs independent fresh disk Usage from native
	// facts; this backup selects no trusted saved Usage checkpoint. Record the
	// zero (no selected checkpoint) position instead of loading historical
	// payload/segments into an Aggregate. Metadata snapshot retains the original
	// derivative bytes for older readers; native/Usage archive checks above and
	// below remain authoritative. Store-level errors are caught by Snapshot and
	// the actual native/Metadata validations, never hidden by this omission.
	checkpoint := ledger.Watermark{}
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
	position, err := withBackupPricingAndUsageReference(ctx, cfg, metadata, cfg.UsagePath(), ledgerSnapshot, ledgerKey, report, staging, pressure, func(e *usage.Exporter, _ usage.ArchivePosition) error {
		return e.WalkArchiveBackupFiles(ctx, report.Head, func(relative string) error {
			source, err := backupUsageFile(cfg.UsagePath(), relative)
			if err != nil {
				return err
			}
			return sources.Add(source)
		})
	})
	if err != nil {
		return backup.Manifest{}, fmt.Errorf("verify pricing and Usage before backup: %w", err)
	}
	usageManifestVersion = position.SchemaVersion

	var clusterOptions backup.CreateOptions
	if memberState != nil {
		journal, err := replication.OpenExistingOrderingJournalWithOptions(
			cfg.OrderingJournalPath(), memberClusterKey,
			memberState.ClusterID, memberState.Incarnation, memberState.DurableIndex, memberState.OrderingHeadMAC, orderingJournalOptions(cfg),
		)
		if err != nil {
			return backup.Manifest{}, err
		}
		cursors, cursorErr := journal.StoreCursorsThrough(memberState.AppliedIndex)
		closeErr := journal.Close()
		if err := errors.Join(cursorErr, closeErr); err != nil {
			return backup.Manifest{}, err
		}
		perStore := make(map[string]backup.StoreHead, 4)
		for store := replication.StoreLedger; store <= replication.StoreMetadata; store++ {
			cursor := cursors[store-replication.StoreLedger]
			perStore[replicationStoreName(store)] = backup.StoreHead{Generation: cursor.Generation, Sequence: cursor.Sequence}
		}
		for _, source := range []backup.SourceFile{
			{ArchivePath: "data/cluster/state.json", LocalPath: cfg.ReplicationStatePath()},
			{ArchivePath: "data/cluster/ordering.journal", LocalPath: cfg.OrderingJournalPath()},
		} {
			if err := sources.Add(source); err != nil {
				return backup.Manifest{}, err
			}
		}
		metadataHead := perStore["metadata"]
		clusterOptions = backup.CreateOptions{
			ClusterID: memberState.ClusterID, ClusterIncarnation: memberState.Incarnation,
			SourceNodeID: memberState.NodeID, SourceRole: string(memberState.Role), Term: memberState.Term,
			AppliedIndex: memberState.AppliedIndex, MetadataJournalEpoch: metadataHead.Generation,
			MetadataJournalSequence: metadataHead.Sequence, PerStoreHead: perStore,
		}
	}
	if journalStaged {
		// The journal's current epoch (HA design §12.1). A restore does not
		// reuse it — restore is a whole-file publish and starts a fresh epoch —
		// but without it the archive cannot show that the metadata snapshot
		// sits at the position its manifest claims.
		if err := sources.Add(backup.SourceFile{
			ArchivePath: "data/" + boltstore.MetadataJournalFileName, LocalPath: metadataJournalSnapshot,
		}); err != nil {
			return backup.Manifest{}, err
		}
	}

	sourceInfo, err := metadata.Info()
	if err != nil {
		return backup.Manifest{}, err
	}
	providerSourceTxID := metadataInfo.TxID
	if metadata.AuthenticatedMetadata() {
		providerSourceTxID = sourceInfo.TxID
	}
	if err := walkBackupProviderObjectFiles(ctx, metadata, providerSourceTxID, filepath.Join(cfg.Storage.DataDir, "provider-objects"), sources.AddShared); err != nil {
		return backup.Manifest{}, err
	}
	createOptions := backup.CreateOptions{
		OutputPath: outputPath, BackupKey: backupKey, SourceCatalog: sources, Context: ctx,
		Metadata: metadataInfo, LedgerWatermark: ledgerWatermark,
		LedgerChainHeadSequence: chainSequence, LedgerChainHeadOffset: chainOffset,
		LedgerChainHeadHash: chainHash, LedgerChainVerified: chainVerified,
		GovernanceSequence: governanceSummary.Records, GovernanceOffset: governanceSummary.Bytes,
		GovernanceHeadHash: governanceSummary.LastHash, GovernanceFormatVersion: 1,
		CheckpointWatermark: checkpoint, UsageManifestVersion: usageManifestVersion,
		LedgerFeatureEpoch: metadataInfo.LedgerFeatureEpoch, MinimumLedgerReaderVersion: metadataInfo.MinimumLedgerReaderVersion,
		PricingStateSHA256: pricingBackupState.StateSHA256, PendingIntentSHA256: pricingBackupState.PendingIntentSHA256, PendingIntents: pricingBackupState.PendingIntents,
		MasterKeyFingerprint: masterFingerprint, Build: buildinfo.Current(),
		KeySlotDescriptorSHA256: descriptorDigest,
	}
	createOptions.ClusterID = clusterOptions.ClusterID
	createOptions.ClusterIncarnation = clusterOptions.ClusterIncarnation
	createOptions.SourceNodeID = clusterOptions.SourceNodeID
	createOptions.SourceRole = clusterOptions.SourceRole
	createOptions.Term = clusterOptions.Term
	createOptions.AppliedIndex = clusterOptions.AppliedIndex
	createOptions.MetadataJournalEpoch = clusterOptions.MetadataJournalEpoch
	createOptions.MetadataJournalSequence = clusterOptions.MetadataJournalSequence
	createOptions.PerStoreHead = clusterOptions.PerStoreHead
	outputOwner, err := prepareBackupOutputDisk(ctx, cfg, pressure)
	if err != nil {
		return backup.Manifest{}, fmt.Errorf("admit encrypted backup output before creation: %w", err)
	}
	defer func() {
		resultErr = outputOwner.finish(resultErr)
		if resultErr != nil {
			result = backup.Manifest{}
		}
	}()
	if outputOwner != nil {
		createOptions.OutputDiskBytes = outputOwner.quota.ceiling
		createOptions.ReserveOutputDisk = outputOwner.quota.reserveNativeFile
	}
	if stagingOwner != nil && stagingOwner.metadataImage != nil {
		if _, err := stagingOwner.metadataImage.SnapshotInfo(ctx); err != nil {
			return backup.Manifest{}, err
		}
	}
	return writer(createOptions)
}

func walkBackupProviderObjectFiles(ctx context.Context, metadata *boltstore.Store, snapshotTxID uint64, root string, consume func(backup.SourceFile) error) error {
	return metadata.WalkProviderObjectReferences(ctx, snapshotTxID, func(resource boltstore.ProviderObjectReference) error {
		// Include both request and response objects. Shared references are deduped
		// by identical source facts in the private disk catalog, never a RAM map.
		for _, name := range []string{resource.ObjectPath, resource.InputObjectPath} {
			if name == "" {
				continue
			}
			if filepath.IsAbs(name) || filepath.Base(name) != name || filepath.Clean(name) != name {
				return fmt.Errorf("provider resource %q has an unsafe object path", resource.ID)
			}
			localPath := filepath.Join(root, name)
			info, err := os.Lstat(localPath)
			if err != nil {
				return fmt.Errorf("inspect provider resource object %q: %w", resource.ID, err)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("provider resource object %q is not a regular file", resource.ID)
			}
			if err := consume(backup.SourceFile{ArchivePath: "data/provider-objects/" + filepath.ToSlash(name), LocalPath: localPath}); err != nil {
				return err
			}
		}
		return nil
	})
}

func backupUsageFile(root, relative string) (backup.SourceFile, error) {
	if relative == "" || filepath.IsAbs(relative) {
		return backup.SourceFile{}, errors.New("usage manifest contains an unsafe path")
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean != filepath.FromSlash(relative) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return backup.SourceFile{}, errors.New("usage manifest path escapes usage directory")
	}
	localPath := filepath.Join(root, clean)
	info, err := os.Lstat(localPath)
	if err != nil {
		return backup.SourceFile{}, err
	}
	if !info.Mode().IsRegular() {
		return backup.SourceFile{}, fmt.Errorf("usage backup source is not regular file %q", localPath)
	}
	return backup.SourceFile{ArchivePath: "data/usage/" + filepath.ToSlash(clean), LocalPath: localPath}, nil
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

// copyFileForBackup stages one file into the archive's staging directory.
//
// It copies rather than referencing the live path because the staging
// directory is what the archive is built from, and a file that changed between
// staging and archiving would produce a manifest describing bytes the archive
// does not contain.
func copyFileForBackup(source, destination string) error {
	return copyFileForBackupWithDisk(context.Background(), source, destination, nil)
}

// attachStagedMetadataJournal publishes the next journal epoch into a staged
// data directory.
//
// The staged store records nothing — it is how the next projection is built,
// not an operation inside one — but the attach itself writes the epoch and the
// applied sequence on the raw transaction, and creates the file. Both travel
// with the rename.
func attachStagedMetadataJournal(store *boltstore.Store, secretVault *vault.Vault, masterKey []byte, cfg config.Config) error {
	if _, err := attachConfiguredMetadataJournal(store, secretVault, masterKey, "backup restore", cfg); err != nil {
		return fmt.Errorf("publish the restored metadata journal epoch: %w", err)
	}
	return nil
}

// The offline caller owns a frozen staged native tree. Always rebuild a new
// derivative from that authenticated cut; never reopen or publish a cache DB.
// The temporary tree is removed before either archive or restore publication.
func validateBackupPricingProjection(ctx context.Context, cfg config.Config, metadata *boltstore.Store, source string, key []byte, report ledger.ChainReport, parent string) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cfg.ResourceBudget != nil && cfg.ResourceBudget.MemoryStateRequired {
		pressure, err := commandPressure(ctx, cfg, nil)
		if err != nil {
			return err
		}
		return withPackedBackupReference(ctx, cfg, source, key, report, pressure, parent, false, func(ctx context.Context, d *packedBackupUsageFailure, _ string, _ [32]byte) error {
			return d.Ledger.WithFreshPricingView(ctx, func(reader ledger.AuthenticatedPricingView) error {
				return metadata.ValidateDeploymentPriceReferences(ctx, reader)
			})
		})
	}
	credit, err := acquireBackupProjectionBudget(ctx, cfg, backupLedgerProjection)
	if err != nil {
		return err
	}
	var cleanupErr error
	defer func() { resultErr = errors.Join(resultErr, credit.retire(withRetirementDebt(cleanupErr, resultErr))) }()
	directory, err := os.MkdirTemp(parent, ".halro-pricing-check-*")
	if err != nil {
		return err
	}
	identity, err := os.Lstat(directory)
	if err != nil {
		cleanupErr = err
		return err
	}
	defer func() {
		cleanupErr = errors.Join(cleanupErr, removeOwnedReferenceDirectory(directory, identity))
		resultErr = errors.Join(resultErr, cleanupErr)
	}()
	var projection *ledger.LedgerProjection
	if credit.limit > 0 {
		projection, err = ledger.BuildLedgerProjectionWithLimit(ctx, source, key, filepath.Join(directory, "ledger.db"), report, credit.limit)
	} else {
		projection, err = ledger.BuildLedgerProjection(ctx, source, key, filepath.Join(directory, "ledger.db"), report)
	}
	if err != nil {
		return err
	}
	defer func() {
		cleanupErr = errors.Join(cleanupErr, projection.Discard())
		resultErr = errors.Join(resultErr, cleanupErr)
	}()
	return metadata.ValidateDeploymentPriceReferences(ctx, projection)
}
