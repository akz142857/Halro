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

type KMSRevokeOptions struct {
	SlotID                     string
	ConfirmSlotID              string
	ExpectedDescriptorRevision uint64
	ExpectedSlotRevision       uint64
	ReasonCode                 string
}

type KMSRevokeResult struct {
	SlotID             string                   `json:"slot_id"`
	Purpose            masterkey.KeySlotPurpose `json:"purpose"`
	State              masterkey.KeySlotState   `json:"state"`
	DescriptorRevision uint64                   `json:"descriptor_revision"`
	SlotRevision       uint64                   `json:"slot_revision"`
	RevokedAt          time.Time                `json:"revoked_at"`
	ProductionReady    bool                     `json:"production_ready"`
	AlreadyRevoked     bool                     `json:"already_revoked"`
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

func RevokeKMSKeySlot(ctx context.Context, cfg config.Config, options KMSRevokeOptions) (KMSRevokeResult, error) {
	return revokeKMSKeySlotWithOptions(ctx, cfg, options, defaultKMSWrapperFactory, time.Now, nil)
}

func revokeKMSKeySlotWithOptions(
	ctx context.Context,
	cfg config.Config,
	options KMSRevokeOptions,
	factory kmsWrapperFactory,
	now func() time.Time,
	hook func(string) error,
) (KMSRevokeResult, error) {
	if err := ctx.Err(); err != nil {
		return KMSRevokeResult{}, err
	}
	if cfg.Storage.MasterKey.Mode != config.MasterKeyModeKeySlots {
		return KMSRevokeResult{}, errors.New("Slot revoke requires key_slots mode")
	}
	if options.SlotID == "" || options.ConfirmSlotID != options.SlotID {
		return KMSRevokeResult{}, errors.New("revoke confirmation must exactly match --slot-id")
	}
	if options.ExpectedDescriptorRevision == 0 || options.ExpectedSlotRevision == 0 {
		return KMSRevokeResult{}, errors.New("expected descriptor and Slot revisions are required")
	}
	if options.ReasonCode == "" {
		options.ReasonCode = "retirement_window_completed"
	}
	if options.ReasonCode != "retirement_window_completed" && options.ReasonCode != "incident_retirement" {
		return KMSRevokeResult{}, errors.New("revoke reason must be retirement_window_completed or incident_retirement")
	}
	if factory == nil || now == nil {
		return KMSRevokeResult{}, errors.New("Slot revoke dependencies are required")
	}
	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return KMSRevokeResult{}, fmt.Errorf("acquire offline Slot revoke lock: %w", err)
	}
	defer dataLock.Close()
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		return KMSRevokeResult{}, err
	}
	defer store.Close()
	descriptor, err := store.KeySlotDescriptor(ctx)
	if err != nil {
		return KMSRevokeResult{}, err
	}
	target, ok := keySlotByID(descriptor, options.SlotID)
	if !ok {
		return KMSRevokeResult{}, masterkey.ErrSlotNotFound
	}
	if target.State == masterkey.KeySlotRevoked {
		if err := compactRevokedSlotMetadata(ctx, cfg, store, target.ID, hook); err != nil {
			return KMSRevokeResult{}, err
		}
		return KMSRevokeResult{
			SlotID: target.ID, Purpose: target.Purpose, State: target.State,
			DescriptorRevision: descriptor.Revision, SlotRevision: target.Revision,
			RevokedAt: target.UpdatedAt, ProductionReady: descriptor.ProductionReady(), AlreadyRevoked: true,
		}, nil
	}
	if target.State != masterkey.KeySlotRetiring {
		return KMSRevokeResult{}, errors.New("only a retiring Slot can be revoked")
	}
	if descriptor.Revision != options.ExpectedDescriptorRevision || target.Revision != options.ExpectedSlotRevision {
		return KMSRevokeResult{}, errors.New("revoke revisions do not match the current descriptor")
	}
	key, err := unlockKMSMasterKey(ctx, cfg, store, target.Purpose, factory)
	if err != nil {
		return KMSRevokeResult{}, fmt.Errorf("unlock independent replacement Slot: %w", err)
	}
	defer clear(key)
	secretVault, err := vault.New(key)
	if err != nil {
		return KMSRevokeResult{}, err
	}
	defer secretVault.Close()
	auditKey, err := loadAuditHMACKey(store, secretVault, key)
	if err != nil {
		return KMSRevokeResult{}, err
	}
	defer clear(auditKey)
	transitionAt := now().UTC()
	next, transition, err := descriptor.RevokeSlot(target.ID, descriptor.Revision, target.Revision, transitionAt)
	if err != nil {
		return KMSRevokeResult{}, err
	}
	if transition == nil {
		return KMSRevokeResult{}, errors.New("Slot revoke produced no transition")
	}
	if !next.ProductionReady() {
		return KMSRevokeResult{}, errors.New("Slot revoke would leave the descriptor outside production-ready state")
	}
	if err := callKMSLifecycleHook(hook, "before_revoked_slot_published"); err != nil {
		return KMSRevokeResult{}, err
	}
	persist := func() error {
		_, _, err := store.RevokeKeySlot(ctx, target.ID, descriptor.Revision, target.Revision, transitionAt)
		return err
	}
	if err := publishSlotTransitionWithReason(ctx, store, cfg.AuditPath(), auditKey, transition, options.ReasonCode, persist); err != nil {
		return KMSRevokeResult{}, err
	}
	if err := callKMSLifecycleHook(hook, "after_revoked_slot_published"); err != nil {
		return KMSRevokeResult{}, err
	}
	if err := compactRevokedSlotMetadata(ctx, cfg, store, target.ID, hook); err != nil {
		return KMSRevokeResult{}, err
	}
	revoked, _ := keySlotByID(next, target.ID)
	return KMSRevokeResult{
		SlotID: revoked.ID, Purpose: revoked.Purpose, State: revoked.State,
		DescriptorRevision: next.Revision, SlotRevision: revoked.Revision,
		RevokedAt: revoked.UpdatedAt, ProductionReady: next.ProductionReady(),
	}, nil
}

func compactRevokedSlotMetadata(ctx context.Context, cfg config.Config, store *boltstore.Store, slotID string, hook func(string) error) error {
	compactPath, err := newMetadataStagePath(cfg.Storage.DataDir, "slot-revoke-compact")
	if err != nil {
		return err
	}
	defer os.Remove(compactPath)
	if err := store.CompactSnapshot(compactPath); err != nil {
		return err
	}
	compacted, err := boltstore.Open(compactPath)
	if err != nil {
		return err
	}
	descriptor, verifyErr := compacted.KeySlotDescriptor(ctx)
	if verifyErr == nil {
		slot, ok := keySlotByID(descriptor, slotID)
		if !ok || slot.State != masterkey.KeySlotRevoked || len(slot.WrappedKey) != 0 || slot.KeyReference != "" || len(slot.ProviderParameters) != 0 || !descriptor.ProductionReady() {
			verifyErr = errors.New("compacted metadata did not preserve the safe revoked Slot state")
		}
	}
	if closeErr := compacted.Close(); verifyErr == nil {
		verifyErr = closeErr
	}
	if verifyErr != nil {
		return verifyErr
	}
	if err := callKMSLifecycleHook(hook, "after_revoked_slot_compacted"); err != nil {
		return err
	}
	if err := store.Close(); err != nil {
		return err
	}
	if err := publishMetadata(compactPath, cfg.MetadataPath()); err != nil {
		return err
	}
	return callKMSLifecycleHook(hook, "after_revoked_compaction_published")
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
		if err != nil {
			clear(pending.WrappedKey)
			return KMSRewrapResult{}, err
		}
		persist := func() error {
			_, _, err := store.AddKeySlot(ctx, pending, descriptor.Revision, transition.OccurredAt)
			return err
		}
		if err := publishSlotTransition(ctx, store, cfg.AuditPath(), auditKey, transition, persist); err != nil {
			clear(pending.WrappedKey)
			return KMSRewrapResult{}, err
		}
		clear(pending.WrappedKey)
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
		unwrapper := kmsSlotUnwrapper{masterKey: cfg.Storage.MasterKey, factory: factory}
		verifier := envelopeCandidateVerifier{envelope: keyCheck}
		next, transition, err := descriptor.VerifySlot(
			ctx, target.ID, descriptor.Revision, target.Revision,
			unwrapper, verifier, now().UTC(),
		)
		if err != nil {
			return KMSRewrapResult{}, err
		}
		persist := func() error {
			_, _, err := store.VerifyKeySlot(ctx, target.ID, descriptor.Revision, target.Revision, unwrapper, verifier, transition.OccurredAt)
			return err
		}
		if err := publishSlotTransition(ctx, store, cfg.AuditPath(), auditKey, transition, persist); err != nil {
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
		persist := func() error {
			_, _, err := store.RetireKeySlot(ctx, old.ID, descriptor.Revision, old.Revision, transition.OccurredAt)
			return err
		}
		if err := publishSlotTransition(ctx, store, cfg.AuditPath(), auditKey, transition, persist); err != nil {
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
		Context:       ctx,
		VaultKeyCheck: newKeyCheck, AuditHMACEnvelope: newAuditEnvelope,
		Keyring: boltstore.VaultKeyring{
			FormatVersion: 1, ActiveKeyVersion: keyring.ActiveKeyVersion + 1,
			ActiveFingerprint: newFingerprint, PreviousFingerprint: descriptor.MasterKeyFingerprint,
			RecoveryEnvelope:    bridge,
			RotationOperationID: options.operationID,
		},
		KeySlotDescriptor: &rotatedDescriptor,
		Unwrapper:         unwrapper, Verifier: verifier,
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
	transition *masterkey.SlotTransition,
	persist func() error,
) error {
	return publishSlotTransitionWithReason(ctx, store, auditPath, auditKey, transition, "", persist)
}

func publishSlotTransitionWithReason(
	ctx context.Context,
	store *boltstore.Store,
	auditPath string,
	auditKey []byte,
	transition *masterkey.SlotTransition,
	reasonCode string,
	persist func() error,
) error {
	if transition == nil || persist == nil {
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
	// A Slot transition has one stable logical identity. If the process or
	// metadata persistence fails after the durable Audit append, retrying the
	// same optimistic transition reuses this ID and cannot duplicate evidence.
	eventID := fmt.Sprintf("aud-slot-%s-%d-%d", transition.SlotID, transition.DescriptorRevision, transition.SlotRevision)
	if _, err := log.Append(ctx, audit.Event{
		EventID: eventID, OccurredAt: transition.OccurredAt, ActorType: "local_cli",
		Action: transition.AuditAction(), TargetType: "master_key_slot",
		TargetID: transition.SlotID, Outcome: "success", ReasonCode: reasonCode,
	}); err != nil {
		return err
	}
	if err := checkpointAudit(store, log.Summary()); err != nil {
		return err
	}
	return persist()
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
