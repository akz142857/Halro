package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"syscall"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/vault"
	"gopkg.in/yaml.v3"
)

func rotateSelectedMasterKey(ctx context.Context, cfg config.Config, newKeyFile string, hook func(string) error, source resourcebudget.SampleSource) (KeyRotationResult, error) {
	return rotateSelectedKeyWithAuthority(ctx, cfg, newKeyFile, hook, source, nil)
}

func rotateSelectedKeyWithAuthority(ctx context.Context, cfg config.Config, newKeyFile string, hook func(string) error, source resourcebudget.SampleSource, kmsOptions *kmsRotationOptions) (result KeyRotationResult, resultErr error) {
	if cfg.Replication != nil || cfg.ResourceBudget == nil || cfg.ResourcePressure == nil || (kmsOptions == nil && cfg.Storage.MasterKey.Mode != config.MasterKeyModeFile) || (kmsOptions != nil && cfg.Storage.MasterKey.Mode != config.MasterKeyModeKeySlots) {
		return result, errors.New("selected DEK rotation requires a budgeted offline File Standalone")
	}
	sourceCtx, err := selectedExportCommandContext(ctx, cfg, source)
	if err != nil {
		return result, err
	}
	ctx, finish, err := keyMaintenanceContext(sourceCtx, cfg)
	if err != nil {
		return result, err
	}
	defer func() {
		resultErr = finishKeyMaintenance(ctx, finish, resultErr)
		if resultErr != nil {
			result = KeyRotationResult{}
		}
	}()
	ctx, stopPressure, err := startSelectedRotationPressure(ctx, cfg)
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, stopPressure()) }()
	guard, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return result, err
	}
	keyScope(ctx).lock = guard
	if err := keyScope(ctx).captureSource(cfg); err != nil {
		return result, err
	}
	activeKey, newKey, err := unlockSelectedRotationKeys(ctx, cfg, newKeyFile, kmsOptions)
	if err != nil {
		return result, err
	}
	defer clear(activeKey)
	defer func() { clear(newKey) }()
	if kmsOptions != nil {
		ctx = context.WithValue(ctx, selectedRotationKMSFactoryKey{}, kmsOptions.factory)
	}
	var intent *selectedRotationIntent
	var preparation *selectedRotationIntent
	if _, err := os.Lstat(selectedRotationIntentPath(cfg)); err == nil {
		intent, err = loadSelectedRotationIntent(cfg, activeKey, newKey)
		if err != nil {
			return result, err
		}
		defer clearRotationIntentKeys(intent)
		if kmsOptions != nil {
			if intent.OperationID != kmsOptions.operationID || len(intent.NewKey) != vault.MasterKeySize {
				return result, authenticatedstate.ErrSealIneligible
			}
			clear(newKey)
			newKey = append([]byte(nil), intent.NewKey...)
		}

		if intent.TargetSeal.RootCount == 0 {
			current, err := selectedRotationCurrent(ctx, cfg, intent)
			if err != nil || current != intent.SourceSeal {
				return result, errors.Join(authenticatedstate.ErrSealIneligible, err)
			}
			if intent.PreparationAttempt >= selectedRotationPreparationAttempts {
				return result, errors.New("eight selected preparation attempts are retained; explicit offline archival is required")
			}
			preparation = intent
		} else {
			result = KeyRotationResult{OldFingerprint: intent.OldFingerprint, NewFingerprint: intent.NewFingerprint, OldKeyVersion: intent.OldVersion, NewKeyVersion: intent.NewVersion, Credentials: intent.Credentials, RecoveredPending: true}
			if err := resumeSelectedRotationPublication(ctx, cfg, intent, hook); err != nil {
				return result, err
			}
			return result, finishSelectedFileRotation(ctx, cfg, intent, newKey, hook, source)
		}

	} else if !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	if bytes.Equal(activeKey, newKey) {
		return result, errors.New("selected rotation requires a different DEK")
	}
	oldVault, err := vault.New(activeKey)
	if err != nil {
		return result, err
	}
	defer oldVault.Close()
	legacy, err := openSelectedRotationDB(ctx, cfg, cfg.MetadataPath(), false)
	if err != nil {
		return result, err
	}
	if err := verifyVaultKeyCheck(legacy, oldVault); err != nil {
		return result, errors.Join(err, legacy.Close())
	}
	if err := legacy.Close(); err != nil {
		return result, err
	}
	if err := refuseRotationWithRetainedCiphertextContext(ctx, cfg); err != nil {
		return result, err
	}
	independentBootstrap, err := readMemoryStateBootstrap(ctx, cfg, oldVault, keyScope(ctx).root.pressure.logicalBudget())
	if err != nil || independentBootstrap == nil {
		return result, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}

	completedCandidate := kmsOptions != nil && len(newKey) == 0 && preparation == nil
	var historySlot, targetSlot uint8
	var opaque uint16
	if !completedCandidate {
		historySlot, opaque, _, err = selectedRotationHistoryPreflight(ctx, cfg, oldVault, *independentBootstrap)
		if err != nil {
			return result, err
		}
		if preparation != nil {
			opaque |= preparation.OpaqueMetadataSlots | uint16(1)<<preparation.TargetBootstrap.MetadataSlot
		}
		targetSlot, err = selectRotationPreparationSlot(ctx, cfg, *independentBootstrap, opaque)
		if err != nil {
			return result, err
		}
	}
	native, err := prepareCommandNativeDisk(ctx, cfg, guard)
	if err != nil {
		return result, err
	}
	var s *runtimeSelectedState
	defer func() {
		if s != nil {
			resultErr = errors.Join(resultErr, s.close())
		}
		resultErr = errors.Join(resultErr, native.finish(resultErr))
	}()
	scope := keyScope(ctx).root
	s, err = tryOpenSelectedRuntimeState(ctx, cfg, oldVault, guard, scope.pressure, native.growth, nil)
	if err != nil || s == nil {
		return result, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	if err := s.generation.WaitStartupAudit(ctx); err != nil {
		return result, err
	}

	if completedCandidate {
		current, err := s.services.Metadata.VaultKeyring()
		if err != nil || current.RotationOperationID != kmsOptions.operationID || current.ActiveFingerprint != keyFingerprint(activeKey) || len(current.RecoveryEnvelope) != 0 || current.ActiveKeyVersion < 2 {
			return result, errors.Join(authenticatedstate.ErrSealIneligible, err)
		}
		return KeyRotationResult{OldFingerprint: current.PreviousFingerprint, NewFingerprint: current.ActiveFingerprint, OldKeyVersion: current.ActiveKeyVersion - 1, NewKeyVersion: current.ActiveKeyVersion, RecoveredPending: true}, nil
	}
	operationID := "selected-dek-" + keyFingerprint(newKey)[7:]
	if kmsOptions != nil {
		operationID = kmsOptions.operationID
	}
	auditIntent, err := s.services.Metadata.EnsureMasterKeyRotationAuditIntent(ctx, operationID, time.Now().UTC())
	if err != nil {
		return result, err
	}
	if err := deliverMasterKeyRotationAuditIntent(ctx, s.services.Metadata, s.generation.Owners.Audit, auditIntent, "started", nil); err != nil {
		return result, err
	}
	s.services.Accounting.WithdrawAdmission()
	if err := errors.Join(s.services.Governance.Retire(ctx), s.services.Accounting.Retire(ctx), s.services.Metadata.RetireFacade(ctx)); err != nil {
		return result, err
	}
	if err := checkpointDrainedSelectedState(ctx, cfg, s); err != nil {
		return result, err
	}
	b := s.bootstrap
	targetRootPath, targetJournal := memoryStateMetadataPaths(cfg, targetSlot)
	for _, path := range []string{targetRootPath, targetJournal} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return result, errors.Join(errors.New("inactive Metadata rotation slot requires authorized retirement"), err)
		}
	}
	reference, err := s.generation.Owners.Metadata.SealReference(ctx)
	if err != nil {
		return result, err
	}
	imagePath, err := newMetadataStagePath(cfg.Storage.DataDir, "selected-materialize")
	if err != nil {
		return result, err
	}
	reservation, err := scope.pressure.logicalBudget().TryAcquireRecovery(resourcebudget.Cost{Disk: cfg.ResourceBudget.MetadataStoreDiskBytes})
	if err != nil {
		return result, err
	}
	image, err := s.generation.Owners.Metadata.MaterializeMetadata(ctx, imagePath, reference, metadataLegacyFileCeiling(cfg), reservation)
	if err != nil {
		return result, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, image.Discard())
		if resultErr == nil {
			resultErr = reservation.Release()
		}
	}()
	latest, err := openSelectedRotationDB(ctx, cfg, imagePath, false)
	if err != nil {
		return result, err
	}
	keyring, err := latest.VaultKeyring()
	if errors.Is(err, boltstore.ErrNotFound) {
		keyring = boltstore.VaultKeyring{FormatVersion: 1, ActiveKeyVersion: 1, ActiveFingerprint: keyFingerprint(activeKey)}
		err = nil
	}
	if err != nil || keyring.ActiveFingerprint != keyFingerprint(activeKey) || keyring.ActiveKeyVersion == ^uint64(0) {
		return result, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	if kmsOptions != nil && keyring.RotationOperationID == kmsOptions.operationID && len(keyring.RecoveryEnvelope) == 0 {
		return KeyRotationResult{OldFingerprint: keyring.PreviousFingerprint, NewFingerprint: keyring.ActiveFingerprint, OldKeyVersion: keyring.ActiveKeyVersion - 1, NewKeyVersion: keyring.ActiveKeyVersion, RecoveredPending: true}, nil
	}
	newVault, err := vault.New(newKey)
	if err != nil {
		return result, err
	}
	defer newVault.Close()
	credentials, err := countMaintenanceCredentials(ctx, latest)
	if err != nil {
		return result, err
	}
	journalKey, err := loadMetadataJournalHMACKey(latest, oldVault, activeKey)
	if err != nil {
		return result, err
	}
	defer clear(journalKey)
	targetB := cloneRotationBootstrap(b)
	targetB.Version = 2
	targetB.MetadataSlot = targetSlot
	targetB.VaultVersion = keyring.ActiveKeyVersion + 1
	targetB.VaultFingerprint = keyFingerprint(newKey)
	intent = &selectedRotationIntent{Version: 1, OldKey: append([]byte(nil), activeKey...), OldFingerprint: keyFingerprint(activeKey), NewFingerprint: keyFingerprint(newKey), SourceBootstrap: cloneRotationBootstrap(b), TargetBootstrap: targetB, SourceSeal: s.generation.Seal, Credentials: credentials, OldVersion: keyring.ActiveKeyVersion, NewVersion: keyring.ActiveKeyVersion + 1}
	defer clearRotationIntentKeys(intent)
	intent.SourceHead, err = s.generation.Owners.Metadata.EpochHead()
	if err != nil {
		return result, err
	}
	intent.SourceRoot = reference
	// This is the actual fully audited and published source, captured before
	// candidate creation. Saved candidate headers cannot supply this authority.
	sourceAuthority := b.Authority
	copy(sourceAuthority.Positions[:], intent.SourceSeal.Positions[:])
	selectedSource, sourceFound, err := s.publisher.Select(sourceAuthority)
	if err != nil || !sourceFound || selectedSource != intent.SourceSeal {
		return result, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	intent.SourceBootstrap.Authority = sourceAuthority
	intent.SourceSealBytes, err = authenticatedstate.EncodeSealCandidate(selectedSource, b.LedgerKey, statecut.HardLimits())
	if err != nil {
		return result, err
	}
	intent.HistorySlot = historySlot

	intent.PreparationAttempt = 1
	intent.OpaqueMetadataSlots = opaque
	if preparation != nil {
		intent.PreparationAttempt = preparation.PreparationAttempt + 1
	}
	intent.OperationID = operationID
	if kmsOptions != nil {
		intent.OperationID = kmsOptions.operationID
		intent.NewKey = append([]byte(nil), newKey...)
		intent.TargetDescriptor, err = prepareSelectedRotationKMSDescriptor(ctx, cfg, latest, oldVault, newVault, newKey, *kmsOptions)
		if err != nil {
			return result, err
		}
	}
	if err := persistSelectedRotationIntent(ctx, cfg, intent, oldVault, newVault); err != nil {
		return result, err
	}
	if err := callRotationHook(hook, "selected_after_intent"); err != nil {
		return result, err
	}
	rewritePath, err := newMetadataStagePath(cfg.Storage.DataDir, "selected-rewrite")
	if err != nil {
		return result, err
	}
	if _, err := snapshotCommandMetadata(ctx, cfg, latest, rewritePath); err != nil {
		return result, err
	}
	if err := latest.Close(); err != nil {
		return result, err
	}
	stage, err := openSelectedRotationDB(ctx, cfg, rewritePath, true)
	if err != nil {
		return result, err
	}
	rewrite, err := selectedVaultRewrite(ctx, oldVault, newVault, intent.NewVersion, intent.NewFingerprint, b.AuditKey, b.LedgerKey, journalKey)
	if err != nil {
		return result, err
	}
	rewrite.Keyring.RecoveryEnvelope, err = encryptRotationBridge(oldVault, newKey)
	if err != nil {
		return result, err
	}
	rewrite.Keyring.PreviousFingerprint = intent.OldFingerprint
	rewrite.Keyring.RotationOperationID = intent.OperationID
	if kmsOptions != nil {
		rewrite.Keyring.RotationOperationID = intent.OperationID
		rewrite.KeySlotDescriptor = intent.TargetDescriptor
		rewrite.Unwrapper = kmsSlotUnwrapper{masterKey: cfg.Storage.MasterKey, factory: kmsOptions.factory}
		rewrite.Verifier = envelopeCandidateVerifier{envelope: rewrite.VaultKeyCheck}
	}
	if err := stage.RewriteVaultMaterialStaged(rewrite); err != nil {
		return result, err
	}
	if _, err := stage.ClearVaultRotationBridgeWithAuditIntent(ctx, intent.OperationID, time.Now().UTC()); err != nil {
		return result, err
	}
	artifact, err := newMetadataStagePath(cfg.Storage.DataDir, "selected-unlock")
	if err != nil {
		return result, err
	}
	if err := compactCommandMetadata(ctx, cfg, stage, artifact); err != nil {
		return result, err
	}
	if err := stage.Close(); err != nil {
		return result, err
	}
	candidateDB, err := openSelectedRotationDB(ctx, cfg, artifact, true)
	if err != nil {
		return result, err
	}
	if err := verifyRotatedMetadata(ctx, candidateDB, newVault, oldVault, newKey, b.AuditKey, b.LedgerKey, false); err != nil {
		return result, err
	}
	info, err := candidateDB.Info()
	if err != nil {
		return result, err
	}
	pricing, err := candidateDB.PricingBackupState()
	if err != nil {
		return result, err
	}
	if err := candidateDB.Close(); err != nil {
		return result, err
	}
	intent.UnlockArtifact = artifact
	intent.UnlockDigest, err = selectedRotationDigestFile(ctx, artifact, cfg.ResourceBudget.MetadataStoreDiskBytes)
	if err != nil {
		return result, err
	}
	staging, err := prepareBackupStagingDisk(ctx, cfg, scope.pressure)
	if err != nil {
		return result, err
	}
	archivePath, err := newMetadataStagePath(cfg.Storage.DataDir, "selected-baseline")
	if err != nil {
		return result, err
	}
	configPath, err := newMetadataStagePath(cfg.Storage.DataDir, "selected-config")
	if err != nil {
		return result, err
	}
	configRaw, err := yaml.Marshal(cfg)
	if err != nil {
		return result, err
	}
	if err := writeSelectedExportFile(ctx, configPath, configRaw); err != nil {
		return result, err
	}
	if _, err := backup.Create(backup.CreateOptions{Context: ctx, OutputPath: archivePath, BackupKey: newKey, Files: []backup.SourceFile{{ArchivePath: "config/config.yaml", LocalPath: configPath}, {ArchivePath: "data/metadata.db", LocalPath: artifact}, {ArchivePath: "data/ledger/ledger.wal", LocalPath: cfg.LedgerPath()}, {ArchivePath: "data/audit/audit.log", LocalPath: cfg.AuditPath()}}, Metadata: info, LedgerFeatureEpoch: info.LedgerFeatureEpoch, MinimumLedgerReaderVersion: info.MinimumLedgerReaderVersion, PricingStateSHA256: pricing.StateSHA256, PendingIntentSHA256: pricing.PendingIntentSHA256, PendingIntents: pricing.PendingIntents, MasterKeyFingerprint: intent.NewFingerprint, OutputDiskBytes: cfg.ResourceBudget.MetadataStoreDiskBytes + 1<<20, ReserveOutputDisk: staging.reserve}); err != nil {
		return result, err
	}
	archive, err := backup.OpenVerified(ctx, archivePath, newKey)
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, archive.Close()) }()
	extractPath, err := newMetadataStagePath(cfg.Storage.DataDir, "selected-extract")
	if err != nil {
		return result, err
	}
	target, err := metadatastate.BuildFromBackup(ctx, archive, b.LedgerKey, extractPath, targetRootPath, selectedRotationBaselineOptions(cfg, scope.pressure.logicalBudget(), b))
	if err != nil {
		return result, err
	}
	published := false
	prepared := false
	defer func() {
		if published || prepared {
			resultErr = errors.Join(resultErr, target.Close())
		} else {
			resultErr = errors.Join(resultErr, target.Discard())
		}
	}()
	if err := callRotationHook(hook, "selected_after_build_root"); err != nil {
		return result, err
	}
	head, err := target.StartNewEpoch(ctx, b.LedgerKey, targetJournal, reference.Position.Epoch+1, cfg.ResourceBudget.MetadataJournalDiskBytes)
	if err != nil {
		return result, err
	}
	targetRef, err := target.SealReference(ctx)
	if err != nil {
		return result, err
	}
	intent.TargetHead = head
	intent.TargetRoot = targetRef
	intent.TargetBootstrap.Evidence = target.Evidence()
	intent.TargetBootstrap.Binding = head.Baseline
	if err := callRotationHook(hook, "selected_after_build_epoch"); err != nil {
		return result, err
	}
	o := s.generation.Owners
	o.Metadata = target
	authority, err := statecut.CurrentStandaloneAuthority(ctx, o, b.Authority)
	if err != nil {
		return result, err
	}
	caches, err := prepareSelectedCheckpointCaches(ctx, cfg, s)
	if err != nil {
		return result, err
	}
	hard := statecut.HardLimits()
	base := authenticatedstate.CandidateSeal{Generation: s.generation.Seal.Generation, Revision: s.generation.Seal.Revision + 1, MaxPageBytes: hard.MaxPageBytes, MaxValueBytes: hard.MaxValueBytes, MaxTreeHeight: hard.MaxTreeHeight}
	if err := joinSelectedPhysicalRotationMetadata(ctx, cfg, s); err != nil {
		return result, err
	}
	publication, err := statecut.PublishStandaloneAuthorizedRotation(ctx, o, base, authority, caches, func(ctx context.Context, candidate authenticatedstate.CandidateSeal) error {
		intent.TargetSeal = candidate
		if err := persistSelectedRotationIntent(ctx, cfg, intent, oldVault, newVault); err != nil {
			return err
		}
		prepared = true
		for n := range keyScope(ctx).stages {
			if keyScope(ctx).stages[n].path == artifact {
				keyScope(ctx).stages[n].published = true
			}
		}
		return callRotationHook(hook, "selected_before_seal")
	})
	current, found, selectionErr := s.publisher.Current()
	published = found && current == intent.TargetSeal
	if err != nil || publication.CleanupError() != nil || selectionErr != nil {
		return result, errors.Join(err, publication.CleanupError(), selectionErr)
	}
	if !published {
		return result, authenticatedstate.ErrSealIneligible
	}
	if err := bindSelectedPhysicalRotationSuccessor(ctx, cfg, s, intent.TargetSeal, authority); err != nil {
		return result, err
	}
	if err := callRotationHook(hook, "selected_after_seal"); err != nil {
		return result, err
	}
	if err := target.Close(); err != nil {
		return result, fmt.Errorf("close published rotation target Metadata: %w", err)
	}
	if err := s.close(); err != nil {
		return result, fmt.Errorf("close rotation source after successor common publication: %w", err)
	}
	s = nil
	if err := native.finish(nil); err != nil {
		return result, err
	}
	result = KeyRotationResult{OldFingerprint: intent.OldFingerprint, NewFingerprint: intent.NewFingerprint, OldKeyVersion: intent.OldVersion, NewKeyVersion: intent.NewVersion, Credentials: credentials, RecoveredPending: preparation != nil}
	return result, finishSelectedFileRotation(ctx, cfg, intent, newKey, hook, source)
}

func selectedRotationBaselineOptions(cfg config.Config, m *resourcebudget.Manager, b memoryStateBootstrap) metadatastate.BaselineOptions {
	return metadatastate.BaselineOptions{Identity: b.Authority.Instance, InstanceID: b.InstanceID, KeyGeneration: b.KeyGeneration, Budget: m, MaxSourceBytes: cfg.ResourceBudget.MetadataStoreDiskBytes, MaxDiskBytes: cfg.ResourceBudget.MetadataStoreDiskBytes, MaxAppendBytes: min(cfg.ResourceBudget.MetadataStoreDiskBytes/2, 16<<20)}
}

// A dedicated ordinary staged image owns this mapping. No selected facade is
// allowed to perform a whole-file Vault rewrite.
func openSelectedRotationDB(ctx context.Context, cfg config.Config, path string, publish bool) (*boltstore.Store, error) {
	m, err := reserveCommandMetadataFile(ctx, cfg, path)
	if err != nil {
		return nil, err
	}
	var s *boltstore.Store
	if publish {
		s, err = boltstore.OpenForWholeFilePublishWithMappingBudget(ctx, path, metadataLegacyFileCeiling(cfg), m)
	} else {
		s, err = boltstore.OpenReadOnlyWithMappingBudget(ctx, path, metadataLegacyFileCeiling(cfg), m)
	}
	if err == nil {
		err = keyScope(ctx).trackCloser(s)
	}
	return s, err
}

func selectedVaultRewrite(ctx context.Context, old, new *vault.Vault, version uint64, fingerprint string, auditKey, ledgerKey, journalKey []byte) (boltstore.VaultRewrite, error) {
	check, err := new.EncryptCredential(vaultKeyCheckID, vaultKeyCheckProvider, vaultKeyCheckAudience, []byte(vaultKeyCheckPlaintext))
	if err != nil {
		return boltstore.VaultRewrite{}, err
	}
	a, err := encryptAuditHMACKey(new, auditKey)
	if err != nil {
		return boltstore.VaultRewrite{}, err
	}
	l, err := encryptLedgerHMACKey(new, ledgerKey)
	if err != nil {
		return boltstore.VaultRewrite{}, err
	}
	j, err := encryptMetadataJournalHMACKey(new, journalKey)
	if err != nil {
		return boltstore.VaultRewrite{}, err
	}
	return boltstore.VaultRewrite{Context: ctx, VaultKeyCheck: check, AuditHMACEnvelope: a, LedgerHMACEnvelope: l, MetadataJournalHMACEnvelope: j, Keyring: boltstore.VaultKeyring{FormatVersion: 1, ActiveKeyVersion: version, ActiveFingerprint: fingerprint}, Transform: func(c domain.Credential) (domain.Credential, error) {
		plain, err := old.DecryptCredential(c.ID, string(c.Type), c.Audience, c.Ciphertext)
		if err != nil {
			return c, err
		}
		defer clear(plain)
		if c.KeyVersion == ^uint16(0) {
			return c, errors.New("credential key version exhausted")
		}
		c.Ciphertext, err = new.EncryptCredential(c.ID, string(c.Type), c.Audience, plain)
		c.KeyVersion++
		return c, err
	}, TransformAdminMFA: func(a domain.AdminMFAAuthenticator) (domain.AdminMFAAuthenticator, error) {
		if a.Status == domain.AdminMFAStatusRevoked {
			return a, nil
		}
		plain, err := old.DecryptAdminMFA(a.ID, a.Username, a.SecretCiphertext)
		if err != nil {
			return a, err
		}
		defer clear(plain)
		a.SecretCiphertext, err = new.EncryptAdminMFA(a.ID, a.Username, plain)
		return a, err
	}}, nil
}

func selectedRotationDigestFile(ctx context.Context, path string, limit int64) (digest [32]byte, resultErr error) {
	if err := ctx.Err(); err != nil {
		return digest, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return digest, err
	}
	defer func() {
		if closeErr := ownedfile.CloseFile(f); closeErr != nil {
			resultErr = &selectedExportFileRetirement{cause: errors.Join(resultErr, closeErr), file: f}
		}
	}()
	info, err := f.Stat()
	named, nameErr := os.Lstat(path)
	if err != nil || nameErr != nil || !info.Mode().IsRegular() || info.Size() > limit || !os.SameFile(info, named) {
		return digest, errors.Join(authenticatedstate.ErrLimit, err, nameErr)
	}
	h := sha256.New()
	n, err := io.CopyBuffer(h, io.LimitReader(f, limit+1), make([]byte, 32<<10))
	if err != nil || n != info.Size() {
		return digest, errors.Join(authenticatedstate.ErrCorrupt, err)
	}
	after, err := f.Stat()
	named, nameErr = os.Lstat(path)
	if err != nil || nameErr != nil || !os.SameFile(info, after) || !os.SameFile(info, named) || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) || !info.ModTime().Equal(named.ModTime()) {
		return digest, errors.Join(ownedfile.ErrReplaced, err, nameErr)
	}
	copy(digest[:], h.Sum(nil))
	return digest, ctx.Err()
}

func finishSelectedFileRotation(ctx context.Context, cfg config.Config, i *selectedRotationIntent, newKey []byte, hook func(string) error, source resourcebudget.SampleSource) error {
	if _, err := selectedRotationCurrent(ctx, cfg, i); err != nil {
		return err
	}
	newVault, err := vault.New(newKey)
	if err != nil {
		return err
	}
	defer newVault.Close()
	digest, err := selectedRotationDigestFile(ctx, i.UnlockArtifact, cfg.ResourceBudget.MetadataStoreDiskBytes)
	if err != nil || digest != i.UnlockDigest {
		return errors.Join(authenticatedstate.ErrCorrupt, err)
	}
	// Authenticate the authorized candidate before touching its fixed bootstrap or
	// the legacy unlock DB. The independent intent supplies all authority.
	raw, err := json.Marshal(i.TargetBootstrap)
	if err != nil {
		return err
	}
	defer clear(raw)
	ciphertext, err := newVault.EncryptCredential("system:memory-state-bootstrap", "system", "halro:memory-state:bootstrap:v1", raw)
	if err != nil {
		return err
	}
	defer clear(ciphertext)
	if err := writeSelectedRotationFile(ctx, memoryStateBootstrapPath(cfg), ciphertext); err != nil {
		return err
	}
	if err := callRotationHook(hook, "selected_after_bootstrap"); err != nil {
		return err
	}
	targetDB, err := openSelectedRotationDB(ctx, cfg, i.UnlockArtifact, false)
	if err != nil {
		return err
	}
	if err := verifyVaultKeyCheck(targetDB, newVault); err != nil {
		return errors.Join(err, targetDB.Close())
	}
	stagePath, err := newMetadataStagePath(cfg.Storage.DataDir, "selected-publish")
	if err != nil {
		return err
	}
	if _, err := snapshotCommandMetadata(ctx, cfg, targetDB, stagePath); err != nil {
		return errors.Join(err, targetDB.Close())
	}
	if err := targetDB.Close(); err != nil {
		return err
	}
	if err := publishKeyMetadata(ctx, stagePath, cfg.MetadataPath()); err != nil {
		return err
	}
	if err := callRotationHook(hook, "selected_after_unlock"); err != nil {
		return err
	}
	if err := rewrapSelectedRotationHistory(ctx, cfg, i, newKey); err != nil {
		return err
	}
	if cfg.Storage.MasterKey.Mode == config.MasterKeyModeFile {
		currentStore, err := fileMasterKeyStore(cfg)
		if err != nil {
			return err
		}
		currentKey, err := currentStore.Unlock(ctx)
		if err != nil {
			return err
		}
		if keyFingerprint(currentKey) != i.OldFingerprint && keyFingerprint(currentKey) != i.NewFingerprint {
			clear(currentKey)
			return authenticatedstate.ErrSealIneligible
		}
		clear(currentKey)
		if err := currentStore.Replace(ctx, newKey); err != nil {
			return err
		}
	} else {
		// Publishing the exact verified descriptor and key check in the legacy
		// unlock image is KMS activation. No plaintext key file is introduced.
		if i.TargetDescriptor == nil || i.TargetDescriptor.MasterKeyFingerprint != i.NewFingerprint {
			return authenticatedstate.ErrSealIneligible
		}
	}
	if err := callRotationHook(hook, "selected_after_key"); err != nil {
		return err
	}
	// Close and join the original command lock before acquiring the real Runtime
	// owner. The private capability only bypasses this exact authenticated intent.
	if err := keyScope(ctx).lock.Close(); err != nil {
		return err
	}
	keyScope(ctx).lock = nil
	intentRaw, _, err := readSelectedRotationBytes(selectedRotationIntentPath(cfg))
	if err != nil {
		return err
	}
	capability := &selectedRotationVerification{digest: sha256.Sum256(intentRaw)}
	clear(intentRaw)
	r, err := OpenWithOptions(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{offlineVerification: true, AllowGeneratedSetupToken: true, resourceSampleSource: source, selectedRotationVerification: capability})
	if err != nil {
		return fmt.Errorf("verify rotated actual Runtime: %w", err)
	}
	if r.selectedState == nil || r.selectedState.bootstrap.MetadataSlot != i.TargetBootstrap.MetadataSlot || r.selectedState.bootstrap.VaultVersion != i.NewVersion {
		return errors.Join(authenticatedstate.ErrSealIneligible, r.Close())
	}
	if err := verifyVaultKeyCheck(r.store, newVault); err != nil {
		return errors.Join(err, r.Close())
	}
	if err := r.CheckpointMemoryState(ctx); err != nil {
		return errors.Join(err, r.Close())
	}
	r.selectedState.services.Accounting.WithdrawAdmission()
	if err := retireSelectedRotationSource(ctx, cfg, i, newKey, r, hook); err != nil {
		return fmt.Errorf("retire authorized rotation source: %w", errors.Join(err, r.Close()))
	}
	// Retirement durably advances the independently authorized intent. Its new
	// bytes, rather than the pre-retirement capability, identify completion.
	completionRaw, completionInfo, err := readSelectedRotationBytes(selectedRotationIntentPath(cfg))
	if err != nil {
		return errors.Join(err, r.Close())
	}
	capability.digest = sha256.Sum256(completionRaw)
	clear(completionRaw)
	if err := r.Close(); err != nil {
		return err
	}
	guard, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return err
	}
	keyScope(ctx).lock = guard
	if err := checkSelectedRotationPending(cfg, capability); err != nil {
		return err
	}
	if err := callRotationHook(hook, "selected_after_runtime_verification"); err != nil {
		return err
	}
	return retireSelectedRotationIntent(ctx, cfg, capability, completionInfo)
}

func cloneRotationBootstrap(b memoryStateBootstrap) memoryStateBootstrap {
	b.LedgerKey = append([]byte(nil), b.LedgerKey...)
	b.AuditKey = append([]byte(nil), b.AuditKey...)
	b.GovernanceKey = append([]byte(nil), b.GovernanceKey...)
	return b
}
