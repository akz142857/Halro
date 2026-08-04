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
	DescriptorReady    bool                     `json:"descriptor_ready"`
	AlreadyRevoked     bool                     `json:"already_revoked"`
}

type KMSKeySlotStatus struct {
	ID         string                   `json:"id"`
	Purpose    masterkey.KeySlotPurpose `json:"purpose"`
	State      masterkey.KeySlotState   `json:"state"`
	Provider   string                   `json:"provider"`
	Revision   uint64                   `json:"revision"`
	VerifiedAt *time.Time               `json:"verified_at,omitempty"`
	UpdatedAt  time.Time                `json:"updated_at"`
}

type KMSKeySlotStatusResult struct {
	DescriptorRevision uint64             `json:"descriptor_revision"`
	ActiveGeneration   uint64             `json:"active_generation"`
	DescriptorReady    bool               `json:"descriptor_ready"`
	Slots              []KMSKeySlotStatus `json:"slots"`
}

type kmsRotationOptions struct {
	factory     kmsWrapperFactory
	random      io.Reader
	now         func() time.Time
	hook        func(string) error
	operationID string
}

func RotateKMSMasterKey(ctx context.Context, cfg config.Config, operationID string) (KeyRotationResult, error) {
	recorder := &kmsAuditRecorder{}
	return rotateKMSMasterKeyWithOptions(withKMSAuditRecorder(ctx, recorder), cfg, kmsRotationOptions{operationID: operationID})
}

func RewrapKMSKey(ctx context.Context, cfg config.Config, options KMSRewrapOptions) (KMSRewrapResult, error) {
	recorder := &kmsAuditRecorder{}
	return rewrapKMSKeyWithOptions(withKMSAuditRecorder(ctx, recorder), cfg, options, defaultKMSWrapperFactory, time.Now, nil)
}

func RevokeKMSKeySlot(ctx context.Context, cfg config.Config, options KMSRevokeOptions) (KMSRevokeResult, error) {
	recorder := &kmsAuditRecorder{}
	return revokeKMSKeySlotWithOptions(withKMSAuditRecorder(ctx, recorder), cfg, options, defaultKMSWrapperFactory, time.Now, nil)
}

func InspectKMSKeySlots(ctx context.Context, cfg config.Config) (KMSKeySlotStatusResult, error) {
	if cfg.Storage.MasterKey.Mode != config.MasterKeyModeKeySlots {
		return KMSKeySlotStatusResult{}, errors.New("Slot status requires key_slots mode")
	}
	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return KMSKeySlotStatusResult{}, fmt.Errorf("acquire offline Slot status lock: %w", err)
	}
	defer dataLock.Close()
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		return KMSKeySlotStatusResult{}, err
	}
	defer store.Close()
	descriptor, err := store.KeySlotDescriptor(ctx)
	if err != nil {
		return KMSKeySlotStatusResult{}, err
	}
	result := KMSKeySlotStatusResult{
		DescriptorRevision: descriptor.Revision, ActiveGeneration: descriptor.ActiveGeneration,
		DescriptorReady: descriptor.ProductionReady(), Slots: make([]KMSKeySlotStatus, 0, len(descriptor.Slots)),
	}
	for _, slot := range descriptor.Slots {
		result.Slots = append(result.Slots, KMSKeySlotStatus{
			ID: slot.ID, Purpose: slot.Purpose, State: slot.State, Provider: slot.Provider,
			Revision: slot.Revision, VerifiedAt: slot.VerifiedAt, UpdatedAt: slot.UpdatedAt,
		})
	}
	return result, nil
}

func revokeKMSKeySlotWithOptions(
	ctx context.Context,
	cfg config.Config,
	options KMSRevokeOptions,
	factory kmsWrapperFactory,
	now func() time.Time,
	hook func(string) error,
) (result KMSRevokeResult, resultErr error) {
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
		if descriptor.Revision != options.ExpectedDescriptorRevision+1 || target.Revision != options.ExpectedSlotRevision+1 {
			return KMSRevokeResult{}, errors.New("already-revoked Slot does not match the requested optimistic revisions")
		}
		intent, err := store.KeySlotAuditIntent()
		if err != nil {
			return KMSRevokeResult{}, fmt.Errorf("load completed Slot revoke identity: %w", err)
		}
		if err := validateRevokeIntent(intent, options, descriptor, target); err != nil {
			return KMSRevokeResult{}, err
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
		defer func() {
			resultErr = finishOfflineKMSProviderAuditToStore(cfg, auditKey, kmsAuditRecorderFromContext(ctx), store, resultErr)
		}()
		if err := deliverKeySlotAuditIntent(ctx, store, cfg.AuditPath(), auditKey, intent, hook); err != nil {
			return KMSRevokeResult{}, err
		}
		return KMSRevokeResult{
			SlotID: target.ID, Purpose: target.Purpose, State: target.State,
			DescriptorRevision: descriptor.Revision, SlotRevision: target.Revision,
			RevokedAt: target.UpdatedAt, DescriptorReady: descriptor.ProductionReady(), AlreadyRevoked: true,
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
	defer func() {
		resultErr = finishOfflineKMSProviderAuditToStore(cfg, auditKey, kmsAuditRecorderFromContext(ctx), store, resultErr)
	}()
	stagePath, err := newMetadataStagePath(cfg.Storage.DataDir, "slot-revoke-stage")
	if err != nil {
		return KMSRevokeResult{}, err
	}
	defer os.Remove(stagePath)
	if _, err := store.Snapshot(stagePath); err != nil {
		return KMSRevokeResult{}, err
	}
	stage, err := boltstore.Open(stagePath)
	if err != nil {
		return KMSRevokeResult{}, err
	}
	next, intent, err := stage.RevokeKeySlotWithAuditIntent(
		ctx, target.ID, descriptor.Revision, target.Revision, options.ReasonCode, now().UTC(),
	)
	if err != nil {
		stage.Close()
		return KMSRevokeResult{}, err
	}
	if !next.ProductionReady() {
		stage.Close()
		return KMSRevokeResult{}, errors.New("Slot revoke would leave the descriptor outside production-ready state")
	}
	if err := callKMSLifecycleHook(hook, "after_revoked_stage_persisted"); err != nil {
		stage.Close()
		return KMSRevokeResult{}, err
	}
	compactPath, err := newMetadataStagePath(cfg.Storage.DataDir, "slot-revoke-compact")
	if err == nil {
		err = stage.CompactSnapshot(compactPath)
	}
	if closeErr := stage.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		if compactPath != "" {
			os.Remove(compactPath)
		}
		return KMSRevokeResult{}, err
	}
	defer os.Remove(compactPath)
	compacted, err := boltstore.Open(compactPath)
	if err != nil {
		return KMSRevokeResult{}, err
	}
	compactedDescriptor, verifyErr := compacted.KeySlotDescriptor(ctx)
	compactedIntent, intentErr := compacted.KeySlotAuditIntent()
	if verifyErr == nil {
		verifyErr = intentErr
	}
	if verifyErr == nil {
		revoked, ok := keySlotByID(compactedDescriptor, target.ID)
		if !ok || revoked.State != masterkey.KeySlotRevoked || len(revoked.WrappedKey) != 0 || revoked.KeyReference != "" || len(revoked.ProviderParameters) != 0 ||
			!compactedDescriptor.ProductionReady() || compactedIntent != intent {
			verifyErr = errors.New("compacted metadata did not preserve the safe revoked Slot and Audit intent")
		}
	}
	if closeErr := compacted.Close(); verifyErr == nil {
		verifyErr = closeErr
	}
	if verifyErr != nil {
		return KMSRevokeResult{}, verifyErr
	}
	if err := callKMSLifecycleHook(hook, "after_revoked_slot_compacted"); err != nil {
		return KMSRevokeResult{}, err
	}
	if err := callKMSLifecycleHook(hook, "before_revoked_metadata_publish"); err != nil {
		return KMSRevokeResult{}, err
	}
	if err := store.Close(); err != nil {
		return KMSRevokeResult{}, err
	}
	if err := publishMetadata(compactPath, cfg.MetadataPath()); err != nil {
		return KMSRevokeResult{}, err
	}
	if err := callKMSLifecycleHook(hook, "after_revoked_metadata_publish"); err != nil {
		return KMSRevokeResult{}, err
	}
	published, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		return KMSRevokeResult{}, err
	}
	if err := deliverKeySlotAuditIntent(ctx, published, cfg.AuditPath(), auditKey, intent, hook); err != nil {
		published.Close()
		return KMSRevokeResult{}, err
	}
	if err := published.Close(); err != nil {
		return KMSRevokeResult{}, err
	}
	if err := callKMSLifecycleHook(hook, "after_revoked_audit_delivered"); err != nil {
		return KMSRevokeResult{}, err
	}
	revoked, _ := keySlotByID(next, target.ID)
	return KMSRevokeResult{
		SlotID: revoked.ID, Purpose: revoked.Purpose, State: revoked.State,
		DescriptorRevision: next.Revision, SlotRevision: revoked.Revision,
		RevokedAt: revoked.UpdatedAt, DescriptorReady: next.ProductionReady(),
	}, nil
}

func validateRevokeIntent(intent masterkey.KeySlotAuditIntent, options KMSRevokeOptions, descriptor masterkey.KeySlotDescriptor, slot masterkey.KeySlot) error {
	if err := intent.Validate(); err != nil {
		return err
	}
	if intent.TargetID != options.SlotID || intent.Purpose != slot.Purpose || intent.ReasonCode != options.ReasonCode ||
		intent.ExpectedDescriptorRevision != options.ExpectedDescriptorRevision || intent.ExpectedSlotRevision != options.ExpectedSlotRevision ||
		intent.DescriptorRevision != descriptor.Revision || intent.SlotRevision != slot.Revision || intent.OccurredAt != slot.UpdatedAt {
		return errors.New("already-revoked Slot does not match the durable operation identity")
	}
	return nil
}

func keySlotAuditEvent(intent masterkey.KeySlotAuditIntent) audit.Event {
	return audit.Event{
		EventID: intent.EventID, OccurredAt: intent.OccurredAt, ActorType: "local_cli",
		Action: intent.Action, TargetType: "master_key_slot", TargetID: intent.TargetID,
		Outcome: "success", ReasonCode: intent.ReasonCode,
	}
}

func deliverKeySlotAuditIntent(ctx context.Context, store *boltstore.Store, auditPath string, auditKey []byte, intent masterkey.KeySlotAuditIntent, hook func(string) error) error {
	if err := intent.Validate(); err != nil {
		return err
	}
	log, err := audit.Open(auditPath, auditKey)
	if err != nil {
		return err
	}
	defer log.Close()
	return deliverKeySlotAuditIntentToLog(ctx, store, log, intent, hook)
}

func deliverKeySlotAuditIntentToLog(ctx context.Context, store *boltstore.Store, log *audit.Log, intent masterkey.KeySlotAuditIntent, hook func(string) error) error {
	if err := reconcileAuditCheckpoint(store, log.Summary()); err != nil {
		return err
	}
	if _, err := log.Append(ctx, keySlotAuditEvent(intent)); err != nil {
		return err
	}
	if err := callKMSLifecycleHook(hook, keySlotAuditHookPoint(intent, "audit_append")); err != nil {
		return err
	}
	if err := checkpointAudit(store, log.Summary()); err != nil {
		return err
	}
	if err := callKMSLifecycleHook(hook, keySlotAuditHookPoint(intent, "audit_checkpoint")); err != nil {
		return err
	}
	if intent.Delivered {
		return nil
	}
	if err := store.MarkKeySlotAuditDelivered(ctx, intent.EventID); err != nil {
		return err
	}
	return callKMSLifecycleHook(hook, keySlotAuditHookPoint(intent, "audit_intent_delivered"))
}

func keySlotAuditHookPoint(intent masterkey.KeySlotAuditIntent, phase string) string {
	prefix := "slot"
	switch intent.Action {
	case "security.master_key_slot.added":
		prefix = "added"
	case "security.master_key_slot.verified":
		prefix = "verified"
	case "security.master_key_slot.retiring":
		prefix = "retiring"
	case "security.master_key_slot.revoked":
		prefix = "revoked"
	}
	return "after_" + prefix + "_" + phase
}

func drainKeySlotAuditIntent(ctx context.Context, store *boltstore.Store, log *audit.Log) error {
	intent, err := store.KeySlotAuditIntent()
	if errors.Is(err, boltstore.ErrNotFound) {
		return nil
	}
	if err != nil || intent.Delivered {
		return err
	}
	return deliverKeySlotAuditIntentToLog(ctx, store, log, intent, nil)
}

func drainKeySlotAuditIntentAtPath(ctx context.Context, store *boltstore.Store, auditPath string, auditKey []byte) error {
	intent, err := store.KeySlotAuditIntent()
	if errors.Is(err, boltstore.ErrNotFound) || (err == nil && intent.Delivered) {
		return nil
	}
	if err != nil {
		return err
	}
	return deliverKeySlotAuditIntent(ctx, store, auditPath, auditKey, intent, nil)
}

func masterKeyRotationAuditEvent(intent masterkey.MasterKeyRotationAuditIntent, phase string) (audit.Event, error) {
	if err := intent.Validate(); err != nil {
		return audit.Event{}, err
	}
	event := audit.Event{
		ActorType: "local_cli", TargetType: "master_key", TargetID: "local", Outcome: "success",
		CorrelationID: intent.OperationID,
	}
	switch phase {
	case "started":
		event.EventID = intent.StartedEventID
		event.OccurredAt = intent.StartedAt
		event.Action = "security.master_key_rotation.started"
	case "completed":
		if intent.CompletedAt == nil || intent.CompletedEventID == "" {
			return audit.Event{}, errors.New("Master Key rotation completion Audit is not prepared")
		}
		event.EventID = intent.CompletedEventID
		event.OccurredAt = *intent.CompletedAt
		event.Action = "security.master_key_rotation.completed"
	default:
		return audit.Event{}, errors.New("Master Key rotation Audit phase is invalid")
	}
	return event, nil
}

func deliverMasterKeyRotationAuditIntentAtPath(ctx context.Context, store *boltstore.Store, auditPath string, auditKey []byte, intent masterkey.MasterKeyRotationAuditIntent, phase string, hook func(string) error) error {
	log, err := audit.Open(auditPath, auditKey)
	if err != nil {
		return err
	}
	defer log.Close()
	return deliverMasterKeyRotationAuditIntent(ctx, store, log, intent, phase, hook)
}

func deliverMasterKeyRotationAuditIntent(ctx context.Context, store *boltstore.Store, log *audit.Log, intent masterkey.MasterKeyRotationAuditIntent, phase string, hook func(string) error) error {
	event, err := masterKeyRotationAuditEvent(intent, phase)
	if err != nil {
		return err
	}
	if err := reconcileAuditCheckpoint(store, log.Summary()); err != nil {
		return err
	}
	if _, err := log.Append(ctx, event); err != nil {
		return err
	}
	if err := callKMSLifecycleHook(hook, "after_rotation_"+phase+"_audit_append"); err != nil {
		return err
	}
	if err := checkpointAudit(store, log.Summary()); err != nil {
		return err
	}
	if err := callKMSLifecycleHook(hook, "after_rotation_"+phase+"_audit_checkpoint"); err != nil {
		return err
	}
	delivered := intent.StartedDelivered
	if phase == "completed" {
		delivered = intent.CompletedDelivered
	}
	if delivered {
		return nil
	}
	if err := store.MarkMasterKeyRotationAuditDelivered(ctx, event.EventID); err != nil {
		return err
	}
	return callKMSLifecycleHook(hook, "after_rotation_"+phase+"_audit_intent_delivered")
}

// drainMasterKeyRotationAuditIntent is called during Runtime startup after the
// trusted Audit checkpoint has been reconciled. The Runtime caller lives in
// runtime.go so listeners remain closed on any delivery or payload conflict.
func drainMasterKeyRotationAuditIntent(ctx context.Context, store *boltstore.Store, log *audit.Log) error {
	intent, err := store.MasterKeyRotationAuditIntent()
	if errors.Is(err, boltstore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !intent.StartedDelivered {
		if err := deliverMasterKeyRotationAuditIntent(ctx, store, log, intent, "started", nil); err != nil {
			return err
		}
		intent, err = store.MasterKeyRotationAuditIntent()
		if err != nil {
			return err
		}
	}
	if intent.CompletedAt != nil && !intent.CompletedDelivered {
		return deliverMasterKeyRotationAuditIntent(ctx, store, log, intent, "completed", nil)
	}
	return nil
}

func drainMasterKeyRotationAuditIntentAtPath(ctx context.Context, store *boltstore.Store, auditPath string, auditKey []byte) error {
	intent, err := store.MasterKeyRotationAuditIntent()
	if errors.Is(err, boltstore.ErrNotFound) || (err == nil && intent.StartedDelivered && (intent.CompletedAt == nil || intent.CompletedDelivered)) {
		return nil
	}
	if err != nil {
		return err
	}
	log, err := audit.Open(auditPath, auditKey)
	if err != nil {
		return err
	}
	defer log.Close()
	return drainMasterKeyRotationAuditIntent(ctx, store, log)
}

func rewrapKMSKeyWithOptions(
	ctx context.Context,
	cfg config.Config,
	options KMSRewrapOptions,
	factory kmsWrapperFactory,
	now func() time.Time,
	hook func(string) error,
) (result KMSRewrapResult, resultErr error) {
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
	defer func() {
		resultErr = finishOfflineKMSProviderAuditToStore(cfg, auditKey, kmsAuditRecorderFromContext(ctx), store, resultErr)
	}()
	if err := drainKeySlotAuditIntentAtPath(ctx, store, cfg.AuditPath(), auditKey); err != nil {
		return KMSRewrapResult{}, fmt.Errorf("recover pending Key Slot audit: %w", err)
	}
	keyCheck, err := store.VaultKeyCheck()
	if err != nil {
		return KMSRewrapResult{}, err
	}
	result = KMSRewrapResult{
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
		_, transition, err := descriptor.AddSlot(pending, descriptor.Revision, now().UTC())
		if err != nil {
			clear(pending.WrappedKey)
			return KMSRewrapResult{}, err
		}
		persisted, intent, err := store.AddKeySlotWithAuditIntent(ctx, pending, descriptor.Revision, transition.OccurredAt)
		if err != nil {
			clear(pending.WrappedKey)
			return KMSRewrapResult{}, err
		}
		if err := callKMSLifecycleHook(hook, "after_pending_slot_persisted"); err != nil {
			clear(pending.WrappedKey)
			return KMSRewrapResult{}, err
		}
		if err := deliverKeySlotAuditIntent(ctx, store, cfg.AuditPath(), auditKey, intent, hook); err != nil {
			clear(pending.WrappedKey)
			return KMSRewrapResult{}, err
		}
		clear(pending.WrappedKey)
		descriptor = persisted
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
		_, transition, err := descriptor.VerifySlot(
			ctx, target.ID, descriptor.Revision, target.Revision,
			unwrapper, verifier, now().UTC(),
		)
		if err != nil {
			return KMSRewrapResult{}, err
		}
		persisted, intent, err := store.VerifyKeySlotWithAuditIntent(ctx, target.ID, descriptor.Revision, target.Revision, unwrapper, verifier, transition.OccurredAt)
		if err != nil {
			return KMSRewrapResult{}, err
		}
		if err := callKMSLifecycleHook(hook, "after_active_slot_persisted"); err != nil {
			return KMSRewrapResult{}, err
		}
		if err := deliverKeySlotAuditIntent(ctx, store, cfg.AuditPath(), auditKey, intent, hook); err != nil {
			return KMSRewrapResult{}, err
		}
		descriptor = persisted
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
		_, transition, err := descriptor.RetireSlot(old.ID, descriptor.Revision, old.Revision, now().UTC())
		if err != nil {
			return KMSRewrapResult{}, err
		}
		persisted, intent, err := store.RetireKeySlotWithAuditIntent(ctx, old.ID, descriptor.Revision, old.Revision, transition.OccurredAt)
		if err != nil {
			return KMSRewrapResult{}, err
		}
		if err := callKMSLifecycleHook(hook, "after_old_slot_retiring_persisted"); err != nil {
			return KMSRewrapResult{}, err
		}
		if err := deliverKeySlotAuditIntent(ctx, store, cfg.AuditPath(), auditKey, intent, hook); err != nil {
			return KMSRewrapResult{}, err
		}
		descriptor = persisted
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

func rotateKMSMasterKeyWithOptions(ctx context.Context, cfg config.Config, options kmsRotationOptions) (result KeyRotationResult, resultErr error) {
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
	auditKey, err := loadAuditHMACKey(metadata, currentVault, currentKey)
	if err != nil {
		metadata.Close()
		return KeyRotationResult{}, err
	}
	defer clear(auditKey)
	defer func() {
		resultErr = finishOfflineKMSProviderAudit(cfg, auditKey, kmsAuditRecorderFromContext(ctx), resultErr)
	}()
	if err := drainMasterKeyRotationAuditIntentAtPath(ctx, metadata, cfg.AuditPath(), auditKey); err != nil {
		metadata.Close()
		return KeyRotationResult{}, fmt.Errorf("recover pending Master Key rotation audit: %w", err)
	}
	result = KeyRotationResult{
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
		if err := finalizeKMSMasterKeyRotation(ctx, cfg, currentVault, currentKey, options.operationID, options.now, options.hook); err != nil {
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
	rotationIntent, err := metadata.EnsureMasterKeyRotationAuditIntent(ctx, options.operationID, options.now().UTC())
	if err != nil {
		metadata.Close()
		return KeyRotationResult{}, err
	}
	if err := deliverMasterKeyRotationAuditIntentAtPath(ctx, metadata, cfg.AuditPath(), auditKey, rotationIntent, "started", options.hook); err != nil {
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
	if err := finalizeKMSMasterKeyRotation(ctx, cfg, newVault, newKey, options.operationID, options.now, options.hook); err != nil {
		return KeyRotationResult{}, err
	}
	result.NewFingerprint = newFingerprint
	result.NewKeyVersion = keyring.ActiveKeyVersion + 1
	return result, nil
}

func finalizeKMSMasterKeyRotation(ctx context.Context, cfg config.Config, newVault *vault.Vault, newKey []byte, operationID string, now func() time.Time, hook func(string) error) error {
	metadata, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		return err
	}
	if err := verifyVaultKeyCheck(metadata, newVault); err != nil {
		metadata.Close()
		return err
	}
	auditKey, err := loadAuditHMACKey(metadata, newVault, newKey)
	if err != nil {
		metadata.Close()
		return err
	}
	defer clear(auditKey)
	stagePath, err := newMetadataStagePath(cfg.Storage.DataDir, "kms-cleanup")
	if err != nil {
		metadata.Close()
		return err
	}
	defer os.Remove(stagePath)
	if _, err := metadata.Snapshot(stagePath); err != nil {
		metadata.Close()
		return err
	}
	if err := metadata.Close(); err != nil {
		return err
	}
	stage, err := boltstore.Open(stagePath)
	if err != nil {
		return err
	}
	completionIntent, err := stage.ClearVaultRotationBridgeWithAuditIntent(ctx, operationID, now().UTC())
	if err != nil {
		stage.Close()
		return err
	}
	if err := callKMSLifecycleHook(hook, "after_rotation_completed_intent_persisted"); err != nil {
		stage.Close()
		return err
	}
	compactPath, err := newMetadataStagePath(cfg.Storage.DataDir, "kms-cleanup-compact")
	if err == nil {
		err = stage.CompactSnapshot(compactPath)
	}
	if closeErr := stage.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		if compactPath != "" {
			os.Remove(compactPath)
		}
		return err
	}
	defer os.Remove(compactPath)
	compacted, err := boltstore.Open(compactPath)
	if err != nil {
		return err
	}
	err = verifyRotatedMetadata(ctx, compacted, newVault, nil, newKey, auditKey, false)
	if err == nil {
		persistedIntent, intentErr := compacted.MasterKeyRotationAuditIntent()
		if intentErr != nil {
			err = intentErr
		} else if !masterKeyRotationAuditIntentsEqual(persistedIntent, completionIntent) {
			err = errors.New("compacted metadata changed the Master Key rotation Audit intent")
		}
	}
	if closeErr := compacted.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := callRotationHook(hook, "before_bridge_cleanup_publish"); err != nil {
		return err
	}
	if err := publishMetadata(compactPath, cfg.MetadataPath()); err != nil {
		return err
	}
	if err := callRotationHook(hook, "after_bridge_cleanup_publish"); err != nil {
		return err
	}
	published, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		return err
	}
	if err := deliverMasterKeyRotationAuditIntentAtPath(ctx, published, cfg.AuditPath(), auditKey, completionIntent, "completed", hook); err != nil {
		published.Close()
		return err
	}
	if err := published.Close(); err != nil {
		return err
	}
	return callKMSLifecycleHook(hook, "after_rotation_completed_audit_delivered")
}

func masterKeyRotationAuditIntentsEqual(left, right masterkey.MasterKeyRotationAuditIntent) bool {
	if left.OperationID != right.OperationID || left.StartedEventID != right.StartedEventID ||
		!left.StartedAt.Equal(right.StartedAt) || left.StartedDelivered != right.StartedDelivered ||
		left.CompletedEventID != right.CompletedEventID || left.CompletedDelivered != right.CompletedDelivered {
		return false
	}
	if left.CompletedAt == nil || right.CompletedAt == nil {
		return left.CompletedAt == nil && right.CompletedAt == nil
	}
	return left.CompletedAt.Equal(*right.CompletedAt)
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
