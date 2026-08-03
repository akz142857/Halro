package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/akz142857/Heimdall/internal/audit"
	"github.com/akz142857/Heimdall/internal/config"
	"github.com/akz142857/Heimdall/internal/id"
	corekms "github.com/akz142857/Heimdall/internal/kms"
	"github.com/akz142857/Heimdall/internal/kms/awskms"
	"github.com/akz142857/Heimdall/internal/ledger"
	"github.com/akz142857/Heimdall/internal/masterkey"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
	"github.com/akz142857/Heimdall/internal/store/lock"
	"github.com/akz142857/Heimdall/internal/vault"
)

const (
	slotParameterInstanceID     = "instance_id"
	slotParameterContextVersion = "context_version"
	slotParameterPayloadVersion = "payload_version"
)

type kmsWrapperFactory func(context.Context, config.AllowedKMSKey) (corekms.Wrapper, error)

var defaultKMSWrapperFactory kmsWrapperFactory = productionKMSWrapper

type kmsInitializationOptions struct {
	factory kmsWrapperFactory
	random  io.Reader
	now     func() time.Time
	hook    func(string) error
}

type RecoveryVerificationResult struct {
	SlotID     string    `json:"slot_id"`
	VerifiedAt time.Time `json:"verified_at"`
}

type kmsSlotUnwrapper struct {
	masterKey config.MasterKey
	factory   kmsWrapperFactory
}

func productionKMSWrapper(ctx context.Context, allowed config.AllowedKMSKey) (corekms.Wrapper, error) {
	if allowed.Provider != awskms.Provider {
		return nil, corekms.NewError(corekms.ErrorAdapterUnavailable, allowed.Provider, corekms.OperationUnwrap, 0, errors.New("KMS provider is not compiled"))
	}
	return awskms.New(ctx, awskms.Options{
		Region: allowed.Region, Account: allowed.Account, KeyARN: allowed.KeyID,
		Endpoint: allowed.Endpoint, Algorithm: allowed.Algorithm,
	})
}

func kmsRetryPolicy(masterKey config.MasterKey) (corekms.RetryPolicy, error) {
	deadline := masterKey.StartupDeadline.Value()
	callTimeout := masterKey.CallTimeout.Value()
	maxBackoff := 5 * time.Second
	if maxBackoff >= deadline {
		maxBackoff = deadline / 2
	}
	initialBackoff := 250 * time.Millisecond
	if initialBackoff > maxBackoff {
		initialBackoff = maxBackoff
	}
	policy := corekms.RetryPolicy{
		CallTimeout: callTimeout, StartupDeadline: deadline,
		InitialBackoff: initialBackoff, MaxBackoff: maxBackoff, MaxAttempts: 8,
	}
	if err := policy.Validate(); err != nil {
		return corekms.RetryPolicy{}, fmt.Errorf("KMS retry policy: %w", err)
	}
	return policy, nil
}

func (u kmsSlotUnwrapper) Unwrap(ctx context.Context, slot masterkey.KeySlot) ([]byte, error) {
	allowed, err := trustedAllowedKMSKey(u.masterKey, slot)
	if err != nil {
		return nil, corekms.NewError(corekms.ErrorConfigInvalid, slot.Provider, corekms.OperationUnwrap, 0, err)
	}
	binding, err := payloadBindingForSlot(slot)
	if err != nil {
		return nil, corekms.NewError(corekms.ErrorPayloadInvalid, slot.Provider, corekms.OperationUnwrap, 0, err)
	}
	providerBinding, err := awskms.NewBindingContext(binding, string(slot.Purpose))
	if err != nil {
		return nil, corekms.NewError(corekms.ErrorPayloadInvalid, slot.Provider, corekms.OperationUnwrap, 0, err)
	}
	wrapper, err := u.factory(ctx, allowed)
	if err != nil {
		return nil, err
	}
	if wrapper.Provider() != slot.Provider {
		return nil, corekms.NewError(corekms.ErrorConfigInvalid, slot.Provider, corekms.OperationUnwrap, 0, errors.New("KMS wrapper provider mismatch"))
	}
	policy, err := kmsRetryPolicy(u.masterKey)
	if err != nil {
		return nil, corekms.NewError(corekms.ErrorConfigInvalid, slot.Provider, corekms.OperationUnwrap, 0, err)
	}
	executor, err := corekms.NewExecutor(wrapper, policy)
	if err != nil {
		return nil, err
	}
	result, err := executor.Unwrap(ctx, corekms.UnwrapRequest{
		KeyReference: slot.KeyReference, Algorithm: slot.Algorithm,
		Ciphertext: slot.WrappedKey, BindingContext: providerBinding,
	})
	if err != nil {
		return nil, err
	}
	defer clear(result.Plaintext)
	key, err := corekms.DecodeProtectedPayload(binding, result.Plaintext)
	if err != nil {
		return nil, corekms.NewError(corekms.ErrorPayloadInvalid, slot.Provider, corekms.OperationUnwrap, 0, err)
	}
	return key, nil
}

func payloadBindingForSlot(slot masterkey.KeySlot) (corekms.PayloadBinding, error) {
	parameters := slot.ProviderParameters
	if len(parameters) != 3 || parameters[slotParameterContextVersion] != awskms.EncryptionContextVersion ||
		parameters[slotParameterPayloadVersion] != "1" {
		return corekms.PayloadBinding{}, errors.New("Key Slot provider parameters are incomplete or invalid")
	}
	binding := corekms.PayloadBinding{InstanceID: parameters[slotParameterInstanceID], SlotID: slot.ID}
	return binding, binding.Validate()
}

func trustedAllowedKMSKey(masterKey config.MasterKey, slot masterkey.KeySlot) (config.AllowedKMSKey, error) {
	expectedSlot := masterKey.PrimarySlot
	if slot.Purpose == masterkey.KeySlotRecovery {
		expectedSlot = masterKey.RecoverySlot
	}
	if slot.ID != expectedSlot || (slot.Purpose != masterkey.KeySlotPrimary && slot.Purpose != masterkey.KeySlotRecovery) {
		return config.AllowedKMSKey{}, errors.New("Key Slot ID or purpose is outside trusted configuration")
	}
	var matches []config.AllowedKMSKey
	for _, allowed := range masterKey.AllowedKMSKeys {
		if allowed.Purpose == string(slot.Purpose) && allowed.Provider == slot.Provider &&
			allowed.KeyID == slot.KeyReference && allowed.Algorithm == slot.Algorithm {
			matches = append(matches, allowed)
		}
	}
	if len(matches) != 1 {
		return config.AllowedKMSKey{}, errors.New("Key Slot does not match exactly one trusted KMS allowlist entry")
	}
	return matches[0], nil
}

func configuredKMSKey(masterKey config.MasterKey, purpose masterkey.KeySlotPurpose) (config.AllowedKMSKey, error) {
	var matches []config.AllowedKMSKey
	for _, allowed := range masterKey.AllowedKMSKeys {
		if allowed.Purpose == string(purpose) {
			matches = append(matches, allowed)
		}
	}
	if len(matches) != 1 {
		return config.AllowedKMSKey{}, fmt.Errorf("new KMS initialization requires exactly one %s allowlist entry", purpose)
	}
	return matches[0], nil
}

func wrapPendingSlot(
	ctx context.Context,
	masterKeyConfig config.MasterKey,
	factory kmsWrapperFactory,
	instanceID string,
	slotID string,
	purpose masterkey.KeySlotPurpose,
	masterKey []byte,
) (masterkey.PendingKeySlot, error) {
	allowed, err := configuredKMSKey(masterKeyConfig, purpose)
	if err != nil {
		return masterkey.PendingKeySlot{}, err
	}
	binding := corekms.PayloadBinding{InstanceID: instanceID, SlotID: slotID}
	payload, err := corekms.EncodeProtectedPayload(binding, masterKey)
	if err != nil {
		return masterkey.PendingKeySlot{}, err
	}
	defer clear(payload)
	providerBinding, err := awskms.NewBindingContext(binding, string(purpose))
	if err != nil {
		return masterkey.PendingKeySlot{}, err
	}
	wrapper, err := factory(ctx, allowed)
	if err != nil {
		return masterkey.PendingKeySlot{}, err
	}
	if wrapper.Provider() != allowed.Provider {
		return masterkey.PendingKeySlot{}, errors.New("KMS wrapper provider mismatch")
	}
	policy, err := kmsRetryPolicy(masterKeyConfig)
	if err != nil {
		return masterkey.PendingKeySlot{}, err
	}
	executor, err := corekms.NewExecutor(wrapper, policy)
	if err != nil {
		return masterkey.PendingKeySlot{}, err
	}
	wrapped, err := executor.Wrap(ctx, corekms.WrapRequest{
		KeyReference: allowed.KeyID, Algorithm: allowed.Algorithm,
		Plaintext: payload, BindingContext: providerBinding,
	})
	if err != nil {
		return masterkey.PendingKeySlot{}, err
	}
	defer clear(wrapped.Ciphertext)
	return masterkey.PendingKeySlot{
		ID: slotID, Provider: allowed.Provider, Purpose: purpose,
		KeyReference: allowed.KeyID, Algorithm: allowed.Algorithm,
		ProviderParameters: map[string]string{
			slotParameterInstanceID: instanceID, slotParameterContextVersion: awskms.EncryptionContextVersion,
			slotParameterPayloadVersion: "1",
		},
		WrappedKey: bytes.Clone(wrapped.Ciphertext),
	}, nil
}

type envelopeCandidateVerifier struct{ envelope []byte }

func (v envelopeCandidateVerifier) VerifyCandidate(ctx context.Context, candidate []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	secretVault, err := vault.New(candidate)
	if err != nil {
		return err
	}
	defer secretVault.Close()
	plaintext, err := secretVault.DecryptCredential(vaultKeyCheckID, vaultKeyCheckProvider, vaultKeyCheckAudience, v.envelope)
	if err != nil {
		return err
	}
	defer clear(plaintext)
	if !bytes.Equal(plaintext, []byte(vaultKeyCheckPlaintext)) {
		return errors.New("Vault Key Check plaintext mismatch")
	}
	return nil
}

func unlockKMSMasterKey(
	ctx context.Context,
	cfg config.Config,
	store *boltstore.Store,
	purpose masterkey.KeySlotPurpose,
	factory kmsWrapperFactory,
) ([]byte, error) {
	if store == nil {
		return nil, errors.New("metadata store is required for Key Slot unlock")
	}
	descriptor, err := store.KeySlotDescriptor(ctx)
	if err != nil {
		return nil, err
	}
	if !descriptor.ProductionReady() {
		return nil, errors.New("Key Slot descriptor is not production-ready")
	}
	slotID := cfg.Storage.MasterKey.PrimarySlot
	if purpose == masterkey.KeySlotRecovery {
		slotID = cfg.Storage.MasterKey.RecoverySlot
	}
	var selected *masterkey.KeySlot
	for index := range descriptor.Slots {
		if descriptor.Slots[index].ID == slotID {
			candidate := descriptor.Slots[index]
			selected = &candidate
			break
		}
	}
	if selected == nil || selected.Purpose != purpose || selected.State != masterkey.KeySlotActive || selected.VerifiedAt == nil {
		return nil, errors.New("configured Key Slot is not an active verified Slot")
	}
	unwrapper := kmsSlotUnwrapper{masterKey: cfg.Storage.MasterKey, factory: factory}
	key, err := unwrapper.Unwrap(ctx, *selected)
	if err != nil {
		return nil, err
	}
	fingerprint, err := masterkey.MasterKeyFingerprint(key)
	if err != nil || fingerprint != descriptor.MasterKeyFingerprint {
		clear(key)
		return nil, corekms.NewError(corekms.ErrorVaultMismatch, selected.Provider, corekms.OperationUnwrap, 0, masterkey.ErrVaultKeyMismatch)
	}
	if err := (vaultCandidateVerifier{store: store}).VerifyCandidate(ctx, key); err != nil {
		clear(key)
		return nil, corekms.NewError(corekms.ErrorVaultMismatch, selected.Provider, corekms.OperationUnwrap, 0, err)
	}
	return key, nil
}

// VerifyRecoverySlot is an explicit offline break-glass operation. Normal
// Runtime, Bootstrap and Admin paths never select the Recovery Slot. A
// successful use is durably recorded before the command reports success.
func VerifyRecoverySlot(ctx context.Context, cfg config.Config, confirmedSlot string) (RecoveryVerificationResult, error) {
	if cfg.Storage.MasterKey.Mode != config.MasterKeyModeKeySlots {
		return RecoveryVerificationResult{}, errors.New("Recovery Slot verification requires key_slots mode")
	}
	if confirmedSlot == "" || confirmedSlot != cfg.Storage.MasterKey.RecoverySlot {
		return RecoveryVerificationResult{}, errors.New("recovery confirmation must exactly match storage.master_key.recovery_slot")
	}
	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return RecoveryVerificationResult{}, err
	}
	defer dataLock.Close()
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		return RecoveryVerificationResult{}, err
	}
	defer store.Close()
	key, err := unlockKMSMasterKey(ctx, cfg, store, masterkey.KeySlotRecovery, defaultKMSWrapperFactory)
	if err != nil {
		return RecoveryVerificationResult{}, err
	}
	defer clear(key)
	secretVault, err := vault.New(key)
	if err != nil {
		return RecoveryVerificationResult{}, err
	}
	defer secretVault.Close()
	auditKey, err := loadAuditHMACKey(store, secretVault, key)
	if err != nil {
		return RecoveryVerificationResult{}, err
	}
	defer clear(auditKey)
	auditLog, err := audit.Open(cfg.AuditPath(), auditKey)
	if err != nil {
		return RecoveryVerificationResult{}, err
	}
	defer auditLog.Close()
	if err := reconcileAuditCheckpoint(store, auditLog.Summary()); err != nil {
		return RecoveryVerificationResult{}, err
	}
	eventID, err := id.New("aud")
	if err != nil {
		return RecoveryVerificationResult{}, err
	}
	now := time.Now().UTC()
	if _, err := auditLog.Append(ctx, audit.Event{
		EventID: eventID, OccurredAt: now, ActorType: "local_cli",
		Action: "security.master_key.recovery_used", TargetType: "master_key_slot",
		TargetID: confirmedSlot, Outcome: "success", ReasonCode: "break_glass_recovery",
	}); err != nil {
		return RecoveryVerificationResult{}, err
	}
	if err := checkpointAudit(store, auditLog.Summary()); err != nil {
		return RecoveryVerificationResult{}, err
	}
	return RecoveryVerificationResult{SlotID: confirmedSlot, VerifiedAt: now}, nil
}

func initializeKMS(ctx context.Context, cfg config.Config, options kmsInitializationOptions) error {
	if options.factory == nil {
		options.factory = defaultKMSWrapperFactory
	}
	if options.random == nil {
		options.random = rand.Reader
	}
	if options.now == nil {
		options.now = func() time.Time { return time.Now().UTC() }
	}
	publicationLock, err := lock.AcquireInitialization(cfg.Storage.DataDir)
	if err != nil {
		return err
	}
	defer publicationLock.Close()
	state, err := InspectInitialization(cfg)
	if err != nil {
		return err
	}
	if state != InitializationEmpty {
		return fmt.Errorf("KMS initialization requires an empty instance; state is %s", state)
	}
	if err := runInitializationHook(options.hook, "after_empty_check"); err != nil {
		return err
	}

	instanceID, err := id.New("ins")
	if err != nil {
		return err
	}
	key := make([]byte, vault.MasterKeySize)
	if _, err := io.ReadFull(options.random, key); err != nil {
		clear(key)
		return fmt.Errorf("generate Master Key: %w", err)
	}
	defer clear(key)
	fingerprint, err := masterkey.MasterKeyFingerprint(key)
	if err != nil {
		return err
	}
	secretVault, err := vault.New(key)
	if err != nil {
		return err
	}
	defer secretVault.Close()
	keyCheck, err := secretVault.EncryptCredential(vaultKeyCheckID, vaultKeyCheckProvider, vaultKeyCheckAudience, []byte(vaultKeyCheckPlaintext))
	if err != nil {
		return fmt.Errorf("create Vault Key Check: %w", err)
	}
	auditKey, err := vault.DeriveAuditHMACKey(key)
	if err != nil {
		return err
	}
	defer clear(auditKey)
	auditEnvelope, err := encryptAuditHMACKey(secretVault, auditKey)
	if err != nil {
		return err
	}
	descriptor, err := masterkey.NewKeySlotDescriptor(fingerprint)
	if err != nil {
		return err
	}
	verifier := envelopeCandidateVerifier{envelope: keyCheck}
	unwrapper := kmsSlotUnwrapper{masterKey: cfg.Storage.MasterKey, factory: options.factory}
	var transitions []masterkey.SlotTransition
	var lastTransition time.Time
	nextTransitionTime := func() time.Time {
		candidate := options.now().UTC()
		if !lastTransition.IsZero() && !candidate.After(lastTransition) {
			candidate = lastTransition.Add(time.Nanosecond)
		}
		lastTransition = candidate
		return candidate
	}
	for _, target := range []struct {
		id      string
		purpose masterkey.KeySlotPurpose
	}{
		{id: cfg.Storage.MasterKey.PrimarySlot, purpose: masterkey.KeySlotPrimary},
		{id: cfg.Storage.MasterKey.RecoverySlot, purpose: masterkey.KeySlotRecovery},
	} {
		pending, err := wrapPendingSlot(ctx, cfg.Storage.MasterKey, options.factory, instanceID, target.id, target.purpose, key)
		if err != nil {
			return err
		}
		now := nextTransitionTime()
		next, transition, err := descriptor.AddSlot(pending, descriptor.Revision, now)
		clear(pending.WrappedKey)
		if err != nil {
			return err
		}
		descriptor = next
		if transition != nil {
			transitions = append(transitions, *transition)
		}
		slot := descriptor.Slots[len(descriptor.Slots)-1]
		for _, candidate := range descriptor.Slots {
			if candidate.ID == target.id {
				slot = candidate
				break
			}
		}
		next, transition, err = descriptor.VerifySlot(ctx, target.id, descriptor.Revision, slot.Revision, unwrapper, verifier, nextTransitionTime())
		if err != nil {
			return err
		}
		descriptor = next
		if transition != nil {
			transitions = append(transitions, *transition)
		}
		if err := runInitializationHook(options.hook, "after_"+string(target.purpose)+"_verified"); err != nil {
			return err
		}
	}
	if !descriptor.ProductionReady() {
		return errors.New("KMS initialization did not produce independent active Primary and Recovery Slots")
	}

	parent := filepath.Dir(cfg.Storage.DataDir)
	stageRoot, err := os.MkdirTemp(parent, ".heimdall-init-stage-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stageRoot)
	if err := os.Chmod(stageRoot, 0o700); err != nil {
		return err
	}
	stageConfig := cfg
	stageConfig.Storage.DataDir = stageRoot
	metadata, err := boltstore.Open(stageConfig.MetadataPath())
	if err != nil {
		return err
	}
	metadataOpen := true
	defer func() {
		if metadataOpen {
			metadata.Close()
		}
	}()
	ledgerLog, err := ledger.Open(stageConfig.LedgerPath(), ledger.NewStatus())
	if err != nil {
		return err
	}
	if err := ledgerLog.Close(); err != nil {
		return err
	}
	auditLog, err := audit.Open(stageConfig.AuditPath(), auditKey)
	if err != nil {
		return err
	}
	for _, transition := range transitions {
		eventID, err := id.New("aud")
		if err != nil {
			auditLog.Close()
			return err
		}
		if _, err := auditLog.Append(ctx, audit.Event{
			EventID: eventID, OccurredAt: transition.OccurredAt, ActorType: "local_cli",
			Action: transition.AuditAction(), TargetType: "master_key_slot", TargetID: transition.SlotID, Outcome: "success",
		}); err != nil {
			auditLog.Close()
			return err
		}
	}
	auditSummary := auditLog.Summary()
	if err := auditLog.Close(); err != nil {
		return err
	}
	if err := metadata.InitializeKeySlotState(ctx, boltstore.KeySlotInitialization{
		Descriptor: descriptor,
		Keyring: boltstore.VaultKeyring{
			FormatVersion: 1, ActiveKeyVersion: 1, ActiveFingerprint: fingerprint,
		},
		VaultKeyCheck: keyCheck, AuditHMACEnvelope: auditEnvelope,
		AuditCheckpoint: boltstore.AuditCheckpoint{Records: auditSummary.Records, Bytes: auditSummary.Bytes, LastHash: auditSummary.LastHash},
	}); err != nil {
		return err
	}
	if err := metadata.Close(); err != nil {
		return err
	}
	metadataOpen = false
	if err := runInitializationHook(options.hook, "after_stage_persisted"); err != nil {
		return err
	}
	for _, directory := range []string{filepath.Dir(stageConfig.LedgerPath()), filepath.Dir(stageConfig.AuditPath()), stageRoot} {
		if err := syncDirectoryPath(directory); err != nil {
			return err
		}
	}
	verificationStore, err := boltstore.Open(stageConfig.MetadataPath())
	if err != nil {
		return err
	}
	persistedKey, err := unlockKMSMasterKey(ctx, stageConfig, verificationStore, masterkey.KeySlotPrimary, options.factory)
	if err == nil {
		clear(persistedKey)
	}
	closeErr := verificationStore.Close()
	if err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	if err := runInitializationHook(options.hook, "after_persisted_primary_verified"); err != nil {
		return err
	}
	if _, err := os.Lstat(cfg.Storage.DataDir); err == nil {
		return errors.New("live data directory appeared during KMS initialization")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(stageRoot, cfg.Storage.DataDir); err != nil {
		return fmt.Errorf("publish initialized KMS data directory: %w", err)
	}
	if err := syncDirectoryPath(parent); err != nil {
		rollbackErr := os.Rename(cfg.Storage.DataDir, stageRoot)
		return errors.Join(fmt.Errorf("sync KMS initialization publication: %w", err), rollbackErr)
	}
	return nil
}

func runInitializationHook(hook func(string) error, point string) error {
	if hook == nil {
		return nil
	}
	return hook(point)
}
