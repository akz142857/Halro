package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/akz142857/Heimdall/internal/audit"
	"github.com/akz142857/Heimdall/internal/config"
	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/id"
	"github.com/akz142857/Heimdall/internal/masterkey"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
	"github.com/akz142857/Heimdall/internal/store/lock"
	"github.com/akz142857/Heimdall/internal/vault"
)

type KMSRewrapOptions struct {
	Purpose      masterkey.KeySlotPurpose
	SlotID       string
	KeyReference string
	Compromised  bool
}

type KMSRewrapResult struct {
	Purpose              masterkey.KeySlotPurpose `json:"purpose"`
	ActiveSlot           string                   `json:"active_slot"`
	RetiringSlot         string                   `json:"retiring_slot,omitempty"`
	MasterKeyFingerprint string                   `json:"master_key_fingerprint"`
	DescriptorRevision   uint64                   `json:"descriptor_revision"`
	RecoveredPending     bool                     `json:"recovered_pending"`
}

type kmsRotationOptions struct {
	factory     kmsWrapperFactory
	random      io.Reader
	now         func() time.Time
	hook        func(string) error
	operationID string
}

func RotateKMSMasterKey(ctx context.Context, cfg config.Config, operationID string) (KeyRotationResult, error) {
	return rotateKMSMasterKeyWithOptions(ctx, cfg, kmsRotationOptions{operationID: operationID})
}

func RewrapKMSKey(ctx context.Context, cfg config.Config, options KMSRewrapOptions) (KMSRewrapResult, error) {
	return rewrapKMSKeyWithOptions(ctx, cfg, options, defaultKMSWrapperFactory, time.Now, nil)
}

func rewrapKMSKeyWithOptions(
	ctx context.Context,
	cfg config.Config,
	options KMSRewrapOptions,
	factory kmsWrapperFactory,
	now func() time.Time,
	hook func(string) error,
) (KMSRewrapResult, error) {
	if err := ctx.Err(); err != nil {
		return KMSRewrapResult{}, err
	}
	if cfg.Storage.MasterKey.Mode != config.MasterKeyModeKeySlots {
		return KMSRewrapResult{}, errors.New("KEK rewrap requires key_slots mode")
	}
	if options.Compromised {
		return KMSRewrapResult{}, errors.New("suspected KEK or Decrypt identity compromise requires DEK rotation and historical backup disposition; rewrap is not an incident remedy")
	}
	if factory == nil || now == nil {
		return KMSRewrapResult{}, errors.New("KMS rewrap dependencies are required")
	}
	configuredSlot := cfg.Storage.MasterKey.PrimarySlot
	sourcePurpose := masterkey.KeySlotRecovery
	if options.Purpose == masterkey.KeySlotRecovery {
		configuredSlot = cfg.Storage.MasterKey.RecoverySlot
		sourcePurpose = masterkey.KeySlotPrimary
	} else if options.Purpose != masterkey.KeySlotPrimary {
		return KMSRewrapResult{}, errors.New("rewrap purpose must be primary or recovery")
	}
	if options.SlotID == "" || options.SlotID != configuredSlot {
		return KMSRewrapResult{}, errors.New("rewrap Slot ID must exactly match the configured target Slot")
	}
	allowed, err := configuredKMSKeyReference(cfg.Storage.MasterKey, options.Purpose, options.KeyReference)
	if err != nil {
		return KMSRewrapResult{}, err
	}

	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return KMSRewrapResult{}, fmt.Errorf("acquire offline KMS rewrap lock: %w", err)
	}
	defer dataLock.Close()
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		return KMSRewrapResult{}, err
	}
	defer store.Close()
	descriptor, err := store.KeySlotDescriptor(ctx)
	if err != nil {
		return KMSRewrapResult{}, err
	}
	key, err := unlockKMSMasterKey(ctx, cfg, store, sourcePurpose, factory)
	if err != nil {
		return KMSRewrapResult{}, fmt.Errorf("unlock independent source Slot: %w", err)
	}
	defer clear(key)
	secretVault, err := vault.New(key)
	if err != nil {
		return KMSRewrapResult{}, err
	}
	defer secretVault.Close()
	auditKey, err := loadAuditHMACKey(store, secretVault, key)
	if err != nil {
		return KMSRewrapResult{}, err
	}
	defer clear(auditKey)
	keyCheck, err := store.VaultKeyCheck()
	if err != nil {
		return KMSRewrapResult{}, err
	}
	result := KMSRewrapResult{
		Purpose: options.Purpose, ActiveSlot: options.SlotID,
		MasterKeyFingerprint: descriptor.MasterKeyFingerprint,
	}

	target, targetExists := keySlotByID(descriptor, options.SlotID)
	if !targetExists {
		instanceID, err := descriptorInstanceID(descriptor)
		if err != nil {
			return KMSRewrapResult{}, err
		}
		pending, err := wrapPendingSlotWithKey(ctx, cfg.Storage.MasterKey, factory, allowed, instanceID, options.SlotID, options.Purpose, key)
		if err != nil {
			return KMSRewrapResult{}, err
		}
		next, transition, err := descriptor.AddSlot(pending, descriptor.Revision, now().UTC())
		clear(pending.WrappedKey)
		if err != nil {
			return KMSRewrapResult{}, err
		}
		if err := publishSlotTransition(ctx, store, cfg.AuditPath(), auditKey, descriptor, next, transition); err != nil {
			return KMSRewrapResult{}, err
		}
		descriptor = next
		if err := callKMSLifecycleHook(hook, "after_pending_slot_published"); err != nil {
			return KMSRewrapResult{}, err
		}
		target, _ = keySlotByID(descriptor, options.SlotID)
	} else {
		result.RecoveredPending = target.State == masterkey.KeySlotPending
		if target.Purpose != options.Purpose || target.KeyReference != options.KeyReference {
			return KMSRewrapResult{}, errors.New("existing target Slot does not match the requested rewrap")
		}
	}

	if target.State == masterkey.KeySlotPending {
		next, transition, err := descriptor.VerifySlot(
			ctx, target.ID, descriptor.Revision, target.Revision,
			kmsSlotUnwrapper{masterKey: cfg.Storage.MasterKey, factory: factory},
			envelopeCandidateVerifier{envelope: keyCheck}, now().UTC(),
		)
		if err != nil {
			return KMSRewrapResult{}, err
		}
		if err := publishSlotTransition(ctx, store, cfg.AuditPath(), auditKey, descriptor, next, transition); err != nil {
			return KMSRewrapResult{}, err
		}
		descriptor = next
		if err := callKMSLifecycleHook(hook, "after_active_slot_published"); err != nil {
			return KMSRewrapResult{}, err
		}
		target, _ = keySlotByID(descriptor, options.SlotID)
	}
	if target.State != masterkey.KeySlotActive || target.VerifiedAt == nil {
		return KMSRewrapResult{}, errors.New("rewrap target Slot is not independently verified and active")
	}

	var oldActive []masterkey.KeySlot
	for _, slot := range descriptor.Slots {
		if slot.ID != target.ID && slot.Purpose == options.Purpose && slot.State == masterkey.KeySlotActive {
			oldActive = append(oldActive, slot)
		}
		if slot.ID != target.ID && slot.Purpose == options.Purpose && slot.State == masterkey.KeySlotRetiring {
			result.RetiringSlot = slot.ID
		}
	}
	if len(oldActive) > 1 {
		return KMSRewrapResult{}, errors.New("rewrap found multiple old active Slots for the same purpose")
	}
	if len(oldActive) == 1 {
		old := oldActive[0]
		next, transition, err := descriptor.RetireSlot(old.ID, descriptor.Revision, old.Revision, now().UTC())
		if err != nil {
			return KMSRewrapResult{}, err
		}
		if err := publishSlotTransition(ctx, store, cfg.AuditPath(), auditKey, descriptor, next, transition); err != nil {
			return KMSRewrapResult{}, err
		}
		descriptor = next
		result.RetiringSlot = old.ID
		if err := callKMSLifecycleHook(hook, "after_old_slot_retired"); err != nil {
			return KMSRewrapResult{}, err
		}
	}
	if !descriptor.ProductionReady() || descriptor.MasterKeyFingerprint != result.MasterKeyFingerprint {
		return KMSRewrapResult{}, errors.New("rewrap did not preserve a production-ready descriptor generation")
	}
	result.DescriptorRevision = descriptor.Revision
	return result, nil
}

func rotateKMSMasterKeyWithOptions(ctx context.Context, cfg config.Config, options kmsRotationOptions) (KeyRotationResult, error) {
	if err := ctx.Err(); err != nil {
		return KeyRotationResult{}, err
	}
	if cfg.Storage.MasterKey.Mode != config.MasterKeyModeKeySlots {
		return KeyRotationResult{}, errors.New("KMS DEK rotation requires key_slots mode")
	}
	if !validKMSOperationID(options.operationID) {
		return KeyRotationResult{}, errors.New("KMS DEK rotation requires a 1-128 character operation ID using letters, digits, dot, underscore, or hyphen")
	}
	if options.factory == nil {
		options.factory = defaultKMSWrapperFactory
	}
	if options.random == nil {
		options.random = rand.Reader
	}
	if options.now == nil {
		options.now = time.Now
	}
	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return KeyRotationResult{}, fmt.Errorf("acquire offline KMS DEK rotation lock: %w", err)
	}
	defer dataLock.Close()

	metadata, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		return KeyRotationResult{}, err
	}
	currentKey, err := unlockKMSMasterKey(ctx, cfg, metadata, masterkey.KeySlotPrimary, options.factory)
	if err != nil {
		metadata.Close()
		return KeyRotationResult{}, err
	}
	defer clear(currentKey)
	currentVault, err := vault.New(currentKey)
	if err != nil {
		metadata.Close()
		return KeyRotationResult{}, err
	}
	defer currentVault.Close()
	keyring, err := metadata.VaultKeyring()
	if err != nil {
		metadata.Close()
		return KeyRotationResult{}, err
	}
	descriptor, err := metadata.KeySlotDescriptor(ctx)
	if err != nil {
		metadata.Close()
		return KeyRotationResult{}, err
	}
	credentials, err := metadata.ListCredentials(ctx)
	if err != nil {
		metadata.Close()
		return KeyRotationResult{}, err
	}
	result := KeyRotationResult{
		OldFingerprint: descriptor.MasterKeyFingerprint,
		NewFingerprint: descriptor.MasterKeyFingerprint,
		OldKeyVersion:  keyring.ActiveKeyVersion,
		NewKeyVersion:  keyring.ActiveKeyVersion,
		Credentials:    len(credentials),
	}
	if keyring.RotationOperationID == options.operationID && len(keyring.RecoveryEnvelope) == 0 {
		result.OldFingerprint = keyring.PreviousFingerprint
		result.RecoveredPending = true
		if keyring.ActiveKeyVersion > 1 {
			result.OldKeyVersion = keyring.ActiveKeyVersion - 1
		}
		if err := metadata.Close(); err != nil {
			return KeyRotationResult{}, err
		}
		return result, nil
	}
	if len(keyring.RecoveryEnvelope) > 0 {
		if keyring.RotationOperationID != options.operationID {
			metadata.Close()
			return KeyRotationResult{}, errors.New("a different KMS DEK rotation operation is pending recovery")
		}
		result.OldFingerprint = keyring.PreviousFingerprint
		result.RecoveredPending = true
		if keyring.ActiveKeyVersion > 1 {
			result.OldKeyVersion = keyring.ActiveKeyVersion - 1
		}
		if err := metadata.Close(); err != nil {
			return KeyRotationResult{}, err
		}
		if err := finalizeMasterKeyRotation(ctx, cfg, currentVault, currentKey, options.hook); err != nil {
			return KeyRotationResult{}, err
		}
		return result, nil
	}
	if keyring.ActiveFingerprint != descriptor.MasterKeyFingerprint || keyring.ActiveKeyVersion == ^uint64(0) {
		metadata.Close()
		return KeyRotationResult{}, errors.New("KMS Vault generation is inconsistent or exhausted")
	}

	newKey := make([]byte, vault.MasterKeySize)
	if _, err := io.ReadFull(options.random, newKey); err != nil {
		metadata.Close()
		clear(newKey)
		return KeyRotationResult{}, fmt.Errorf("generate rotated Master Key: %w", err)
	}
	defer clear(newKey)
	newFingerprint, err := masterkey.MasterKeyFingerprint(newKey)
	if err != nil {
		metadata.Close()
		return KeyRotationResult{}, err
	}
	newVault, err := vault.New(newKey)
	if err != nil {
		metadata.Close()
		return KeyRotationResult{}, err
	}
	defer newVault.Close()
	newKeyCheck, err := newVault.EncryptCredential(vaultKeyCheckID, vaultKeyCheckProvider, vaultKeyCheckAudience, []byte(vaultKeyCheckPlaintext))
	if err != nil {
		metadata.Close()
		return KeyRotationResult{}, err
	}
	auditKey, err := loadAuditHMACKey(metadata, currentVault, currentKey)
	if err != nil {
		metadata.Close()
		return KeyRotationResult{}, err
	}
	defer clear(auditKey)
	newAuditEnvelope, err := encryptAuditHMACKey(newVault, auditKey)
	if err != nil {
		metadata.Close()
		return KeyRotationResult{}, err
	}
	bridge, err := encryptRotationBridge(currentVault, newKey)
	if err != nil {
		metadata.Close()
		return KeyRotationResult{}, err
	}
	instanceID, err := descriptorInstanceID(descriptor)
	if err != nil {
		metadata.Close()
		return KeyRotationResult{}, err
	}
	rotatedDescriptor, err := masterkey.NewRotatedKeySlotDescriptor(descriptor, newFingerprint)
	if err != nil {
		metadata.Close()
		return KeyRotationResult{}, err
	}
	verifier := envelopeCandidateVerifier{envelope: newKeyCheck}
	unwrapper := kmsSlotUnwrapper{masterKey: cfg.Storage.MasterKey, factory: options.factory}
	lastTransition := options.now().UTC()
	for _, target := range []struct {
		id      string
		purpose masterkey.KeySlotPurpose
	}{
		{id: cfg.Storage.MasterKey.PrimarySlot, purpose: masterkey.KeySlotPrimary},
		{id: cfg.Storage.MasterKey.RecoverySlot, purpose: masterkey.KeySlotRecovery},
	} {
		allowed, err := configuredKMSKey(cfg.Storage.MasterKey, target.purpose)
		if err != nil {
			metadata.Close()
			return KeyRotationResult{}, err
		}
		pending, err := wrapPendingSlotWithKey(ctx, cfg.Storage.MasterKey, options.factory, allowed, instanceID, target.id, target.purpose, newKey)
		if err != nil {
			metadata.Close()
			return KeyRotationResult{}, err
		}
		lastTransition = lastTransition.Add(time.Nanosecond)
		next, _, err := rotatedDescriptor.AddSlot(pending, rotatedDescriptor.Revision, lastTransition)
		clear(pending.WrappedKey)
		if err != nil {
			metadata.Close()
			return KeyRotationResult{}, err
		}
		rotatedDescriptor = next
		slot, _ := keySlotByID(rotatedDescriptor, target.id)
		lastTransition = lastTransition.Add(time.Nanosecond)
		next, _, err = rotatedDescriptor.VerifySlot(ctx, target.id, rotatedDescriptor.Revision, slot.Revision, unwrapper, verifier, lastTransition)
		if err != nil {
			metadata.Close()
			return KeyRotationResult{}, err
		}
		rotatedDescriptor = next
	}
	if err := rotatedDescriptor.ValidateRotationSuccessor(descriptor); err != nil {
		metadata.Close()
		return KeyRotationResult{}, err
	}
	if err := callKMSLifecycleHook(options.hook, "after_new_slots_verified"); err != nil {
		metadata.Close()
		return KeyRotationResult{}, err
	}
	if err := appendRotationAudit(metadata, cfg.AuditPath(), auditKey, "security.master_key_rotation.started"); err != nil {
		metadata.Close()
		return KeyRotationResult{}, err
	}
	if err := callRotationHook(options.hook, "after_started_audit"); err != nil {
		metadata.Close()
		return KeyRotationResult{}, err
	}
	stagePath, err := newMetadataStagePath(cfg.Storage.DataDir, "kms-rewrite")
	if err != nil {
		metadata.Close()
		return KeyRotationResult{}, err
	}
	defer os.Remove(stagePath)
	if _, err := metadata.Snapshot(stagePath); err != nil {
		metadata.Close()
		return KeyRotationResult{}, err
	}
	if err := metadata.Close(); err != nil {
		return KeyRotationResult{}, err
	}
	if err := callRotationHook(options.hook, "after_metadata_snapshot"); err != nil {
		return KeyRotationResult{}, err
	}
	stage, err := boltstore.Open(stagePath)
	if err != nil {
		return KeyRotationResult{}, err
	}
	err = stage.RewriteVaultMaterial(boltstore.VaultRewrite{
		VaultKeyCheck: newKeyCheck, AuditHMACEnvelope: newAuditEnvelope,
		Keyring: boltstore.VaultKeyring{
			FormatVersion: 1, ActiveKeyVersion: keyring.ActiveKeyVersion + 1,
			ActiveFingerprint: newFingerprint, PreviousFingerprint: descriptor.MasterKeyFingerprint,
			RecoveryEnvelope:    bridge,
			RotationOperationID: options.operationID,
		},
		KeySlotDescriptor: &rotatedDescriptor,
		Transform: func(credential domain.Credential) (domain.Credential, error) {
			plaintext, err := currentVault.DecryptCredential(credential.ID, string(credential.Type), credential.Audience, credential.Ciphertext)
			if err != nil {
				return domain.Credential{}, err
			}
			defer clear(plaintext)
			ciphertext, err := newVault.EncryptCredential(credential.ID, string(credential.Type), credential.Audience, plaintext)
			if err != nil {
				return domain.Credential{}, err
			}
			if credential.KeyVersion == ^uint16(0) {
				return domain.Credential{}, errors.New("credential key version is exhausted")
			}
			credential.Ciphertext = ciphertext
			credential.KeyVersion++
			return credential, nil
		},
		TransformAdminMFA: func(value domain.AdminMFAAuthenticator) (domain.AdminMFAAuthenticator, error) {
			if value.Status == domain.AdminMFAStatusRevoked {
				return value, nil
			}
			plaintext, err := currentVault.DecryptAdminMFA(value.ID, value.Username, value.SecretCiphertext)
			if err != nil {
				return value, err
			}
			defer clear(plaintext)
			value.SecretCiphertext, err = newVault.EncryptAdminMFA(value.ID, value.Username, plaintext)
			return value, err
		},
	})
	compactPath, compactErr := newMetadataStagePath(cfg.Storage.DataDir, "kms-compact")
	if compactErr != nil && err == nil {
		err = compactErr
	}
	if compactPath != "" {
		defer os.Remove(compactPath)
	}
	if err == nil {
		err = stage.CompactSnapshot(compactPath)
	}
	if closeErr := stage.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return KeyRotationResult{}, err
	}
	compacted, err := boltstore.Open(compactPath)
	if err != nil {
		return KeyRotationResult{}, err
	}
	err = verifyRotatedMetadata(ctx, compacted, newVault, currentVault, newKey, auditKey, true)
	if closeErr := compacted.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return KeyRotationResult{}, err
	}
	if err := callRotationHook(options.hook, "after_rewrite_verification"); err != nil {
		return KeyRotationResult{}, err
	}
	if err := callRotationHook(options.hook, "before_metadata_publish"); err != nil {
		return KeyRotationResult{}, err
	}
	if err := publishMetadata(compactPath, cfg.MetadataPath()); err != nil {
		return KeyRotationResult{}, err
	}
	if err := callRotationHook(options.hook, "after_metadata_publish"); err != nil {
		return KeyRotationResult{}, err
	}
	persisted, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		return KeyRotationResult{}, err
	}
	persistedKey, err := unlockKMSMasterKey(ctx, cfg, persisted, masterkey.KeySlotPrimary, options.factory)
	if err == nil && !bytes.Equal(persistedKey, newKey) {
		err = errors.New("persisted Primary Slot unlocked an unexpected Master Key")
	}
	clear(persistedKey)
	if closeErr := persisted.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return KeyRotationResult{}, err
	}
	if err := callKMSLifecycleHook(options.hook, "after_persisted_primary_verified"); err != nil {
		return KeyRotationResult{}, err
	}
	if err := finalizeMasterKeyRotation(ctx, cfg, newVault, newKey, options.hook); err != nil {
		return KeyRotationResult{}, err
	}
	result.NewFingerprint = newFingerprint
	result.NewKeyVersion = keyring.ActiveKeyVersion + 1
	return result, nil
}

func validKMSOperationID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '.' && character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func publishSlotTransition(
	ctx context.Context,
	store *boltstore.Store,
	auditPath string,
	auditKey []byte,
	previous masterkey.KeySlotDescriptor,
	next masterkey.KeySlotDescriptor,
	transition *masterkey.SlotTransition,
) error {
	if transition == nil {
		return nil
	}
	log, err := audit.Open(auditPath, auditKey)
	if err != nil {
		return err
	}
	defer log.Close()
	if err := reconcileAuditCheckpoint(store, log.Summary()); err != nil {
		return err
	}
	eventID, err := id.New("aud")
	if err != nil {
		return err
	}
	if _, err := log.Append(ctx, audit.Event{
		EventID: eventID, OccurredAt: transition.OccurredAt, ActorType: "local_cli",
		Action: transition.AuditAction(), TargetType: "master_key_slot",
		TargetID: transition.SlotID, Outcome: "success",
	}); err != nil {
		return err
	}
	if err := checkpointAudit(store, log.Summary()); err != nil {
		return err
	}
	return store.ReplaceKeySlotDescriptor(ctx, previous.Revision, next)
}

func descriptorInstanceID(descriptor masterkey.KeySlotDescriptor) (string, error) {
	instanceID := ""
	for _, slot := range descriptor.Slots {
		candidate := slot.ProviderParameters[slotParameterInstanceID]
		if candidate == "" {
			continue
		}
		if instanceID != "" && instanceID != candidate {
			return "", errors.New("Key Slot descriptor contains inconsistent instance bindings")
		}
		instanceID = candidate
	}
	if instanceID == "" {
		return "", errors.New("Key Slot descriptor has no instance binding")
	}
	return instanceID, nil
}

func keySlotByID(descriptor masterkey.KeySlotDescriptor, slotID string) (masterkey.KeySlot, bool) {
	for _, slot := range descriptor.Slots {
		if slot.ID == slotID {
			return slot, true
		}
	}
	return masterkey.KeySlot{}, false
}

func callKMSLifecycleHook(hook func(string) error, point string) error {
	if hook == nil {
		return nil
	}
	if err := hook(point); err != nil {
		return fmt.Errorf("KMS lifecycle operation interrupted at %s: %w", point, err)
	}
	return nil
}
