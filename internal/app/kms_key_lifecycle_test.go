package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/adminauth"
	"github.com/akz142857/Heimdall/internal/audit"
	"github.com/akz142857/Heimdall/internal/config"
	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/kms/awskms"
	"github.com/akz142857/Heimdall/internal/kms/fakekms"
	"github.com/akz142857/Heimdall/internal/masterkey"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
	"github.com/akz142857/Heimdall/internal/vault"
)

const replacementPrimaryKMSKeyARN = "arn:aws:kms:eu-west-1:345678901234:key/33333333-3333-4333-8333-333333333333"
const replacementRecoveryKMSKeyARN = "arn:aws:kms:eu-central-1:456789012345:key/44444444-4444-4444-8444-444444444444"

func TestKMSRewrapPreservesMasterKeyCiphertextAndKeyVersion(t *testing.T) {
	cfg, rewrapCfg, harness, credentialID := kmsRewrapFixture(t)
	beforeStore, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	beforeDescriptor, err := beforeStore.KeySlotDescriptor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	beforeCredential, err := beforeStore.GetCredential(context.Background(), credentialID)
	if err != nil {
		t.Fatal(err)
	}
	beforeKeyring, err := beforeStore.VaultKeyring()
	if closeErr := beforeStore.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}

	result, err := rewrapKMSKeyWithOptions(context.Background(), rewrapCfg, KMSRewrapOptions{
		Purpose: masterkey.KeySlotPrimary, SlotID: rewrapCfg.Storage.MasterKey.PrimarySlot,
		KeyReference: replacementPrimaryKMSKeyARN,
	}, harness.factory, time.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	afterDescriptor, err := store.KeySlotDescriptor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	afterCredential, err := store.GetCredential(context.Background(), credentialID)
	if err != nil {
		t.Fatal(err)
	}
	afterKeyring, err := store.VaultKeyring()
	if err != nil {
		t.Fatal(err)
	}
	if result.MasterKeyFingerprint != beforeDescriptor.MasterKeyFingerprint ||
		afterDescriptor.MasterKeyFingerprint != beforeDescriptor.MasterKeyFingerprint ||
		afterDescriptor.ActiveGeneration != beforeDescriptor.ActiveGeneration {
		t.Fatalf("rewrap changed Master Key generation: result=%#v descriptor=%#v", result, afterDescriptor)
	}
	if !bytes.Equal(afterCredential.Ciphertext, beforeCredential.Ciphertext) || afterCredential.KeyVersion != beforeCredential.KeyVersion {
		t.Fatal("rewrap changed Credential ciphertext or KeyVersion")
	}
	if afterKeyring.ActiveKeyVersion != beforeKeyring.ActiveKeyVersion || afterKeyring.ActiveFingerprint != beforeKeyring.ActiveFingerprint {
		t.Fatal("rewrap changed Vault Keyring generation")
	}
	newPrimary := keySlotForAppTest(t, afterDescriptor, rewrapCfg.Storage.MasterKey.PrimarySlot)
	oldPrimary := keySlotForAppTest(t, afterDescriptor, cfg.Storage.MasterKey.PrimarySlot)
	if newPrimary.State != masterkey.KeySlotActive || newPrimary.VerifiedAt == nil || oldPrimary.State != masterkey.KeySlotRetiring {
		t.Fatalf("new=%#v old=%#v", newPrimary, oldPrimary)
	}
	key, err := unlockKMSMasterKey(context.Background(), rewrapCfg, store, masterkey.KeySlotPrimary, harness.factory)
	if err != nil {
		t.Fatal(err)
	}
	clear(key)
}

func TestKMSRewrapPersistsRedactedOfflineProviderAudit(t *testing.T) {
	_, cfg, harness, _ := kmsRewrapFixture(t)
	recorder := &kmsAuditRecorder{}
	ctx := withKMSAuditRecorder(context.Background(), recorder)
	if _, err := rewrapKMSKeyWithOptions(ctx, cfg, KMSRewrapOptions{
		Purpose: masterkey.KeySlotPrimary, SlotID: cfg.Storage.MasterKey.PrimarySlot,
		KeyReference: replacementPrimaryKMSKeyARN,
	}, harness.factory, time.Now, nil); err != nil {
		t.Fatal(err)
	}
	auditKey, err := vault.DeriveAuditHMACKey(bytes.Repeat([]byte{0x61}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer clear(auditKey)
	log, err := audit.Open(cfg.AuditPath(), auditKey)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	providerCalls := 0
	_, err = log.Replay(func(record audit.Record) error {
		if record.Event.Action == "security.kms.call" {
			providerCalls++
			if record.Event.ActorType != "local_cli" || record.Event.TargetType != "kms_operation" || record.Event.TargetID == "" {
				t.Fatalf("provider event=%#v", record.Event)
			}
			payload, marshalErr := json.Marshal(record.Event)
			if marshalErr != nil {
				return marshalErr
			}
			if strings.Contains(string(payload), "arn:aws:kms") || strings.Contains(string(payload), replacementPrimaryKMSKeyARN) {
				t.Fatalf("provider Audit leaked KMS identity: %s", payload)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if providerCalls < 3 {
		t.Fatalf("providerCalls=%d want at least source unwrap, target wrap, and target verify", providerCalls)
	}
}

func TestKMSRewrapRecoversIdempotentlyAtEveryPublicationPoint(t *testing.T) {
	for _, point := range []string{
		"after_pending_slot_persisted", "after_added_audit_append", "after_added_audit_checkpoint", "after_added_audit_intent_delivered", "after_pending_slot_published",
		"after_active_slot_persisted", "after_verified_audit_append", "after_verified_audit_checkpoint", "after_verified_audit_intent_delivered", "after_active_slot_published",
		"after_old_slot_retiring_persisted", "after_retiring_audit_append", "after_retiring_audit_checkpoint", "after_retiring_audit_intent_delivered", "after_old_slot_retired",
	} {
		t.Run(point, func(t *testing.T) {
			_, cfg, harness, _ := kmsRewrapFixture(t)
			options := KMSRewrapOptions{Purpose: masterkey.KeySlotPrimary, SlotID: cfg.Storage.MasterKey.PrimarySlot, KeyReference: replacementPrimaryKMSKeyARN}
			injected := errors.New("injected rewrap process death")
			_, err := rewrapKMSKeyWithOptions(context.Background(), cfg, options, harness.factory, time.Now, func(current string) error {
				if current == point {
					return injected
				}
				return nil
			})
			if !errors.Is(err, injected) {
				t.Fatalf("point=%s err=%v", point, err)
			}
			if _, err := rewrapKMSKeyWithOptions(context.Background(), cfg, options, harness.factory, time.Now, nil); err != nil {
				t.Fatalf("recover %s: %v", point, err)
			}
			store, err := boltstore.Open(cfg.MetadataPath())
			if err != nil {
				t.Fatal(err)
			}
			descriptor, err := store.KeySlotDescriptor(context.Background())
			store.Close()
			if err != nil || !descriptor.ProductionReady() || keySlotForAppTest(t, descriptor, cfg.Storage.MasterKey.PrimarySlot).State != masterkey.KeySlotActive {
				t.Fatalf("point=%s descriptor=%#v err=%v", point, descriptor, err)
			}
			auditKey, err := vault.DeriveAuditHMACKey(bytes.Repeat([]byte{0x61}, 32))
			if err != nil {
				t.Fatal(err)
			}
			log, err := audit.Open(cfg.AuditPath(), auditKey)
			clear(auditKey)
			if err != nil {
				t.Fatal(err)
			}
			counts := map[string]int{}
			_, err = log.Replay(func(record audit.Record) error {
				if record.Event.Action == "security.master_key_slot.retiring" ||
					((record.Event.Action == "security.master_key_slot.added" || record.Event.Action == "security.master_key_slot.verified") && record.Event.TargetID == cfg.Storage.MasterKey.PrimarySlot) {
					counts[record.Event.Action]++
				}
				return nil
			})
			log.Close()
			for _, action := range []string{"security.master_key_slot.added", "security.master_key_slot.verified", "security.master_key_slot.retiring"} {
				if err != nil || counts[action] != 1 {
					t.Fatalf("point=%s action=%s count=%d replay=%v", point, action, counts[action], err)
				}
			}
		})
	}
}

func TestKMSRewrapRejectsConflictingDurableAuditPayload(t *testing.T) {
	_, cfg, harness, _ := kmsRewrapFixture(t)
	options := KMSRewrapOptions{Purpose: masterkey.KeySlotPrimary, SlotID: cfg.Storage.MasterKey.PrimarySlot, KeyReference: replacementPrimaryKMSKeyARN}
	injected := errors.New("stop after durable transition")
	_, err := rewrapKMSKeyWithOptions(context.Background(), cfg, options, harness.factory, time.Now, func(point string) error {
		if point == "after_pending_slot_persisted" {
			return injected
		}
		return nil
	})
	if !errors.Is(err, injected) {
		t.Fatalf("err=%v", err)
	}
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	intent, err := store.KeySlotAuditIntent()
	store.Close()
	if err != nil || intent.Delivered {
		t.Fatalf("intent=%#v err=%v", intent, err)
	}
	auditKey, err := vault.DeriveAuditHMACKey(bytes.Repeat([]byte{0x61}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer clear(auditKey)
	log, err := audit.Open(cfg.AuditPath(), auditKey)
	if err != nil {
		t.Fatal(err)
	}
	conflict := keySlotAuditEvent(intent)
	conflict.ReasonCode = "conflicting-payload"
	if _, err := log.Append(context.Background(), conflict); err != nil {
		t.Fatal(err)
	}
	log.Close()
	_, err = rewrapKMSKeyWithOptions(context.Background(), cfg, options, harness.factory, time.Now, nil)
	if err == nil || !strings.Contains(err.Error(), "conflicts with a different payload") {
		t.Fatalf("conflicting payload was accepted: %v", err)
	}
}

func TestKMSRevokeRequiresConfirmationAndIsIdempotent(t *testing.T) {
	baseCfg, cfg, harness, _ := kmsRewrapFixture(t)
	if _, err := rewrapKMSKeyWithOptions(context.Background(), cfg, KMSRewrapOptions{
		Purpose: masterkey.KeySlotPrimary, SlotID: cfg.Storage.MasterKey.PrimarySlot,
		KeyReference: replacementPrimaryKMSKeyARN,
	}, harness.factory, time.Now, nil); err != nil {
		t.Fatal(err)
	}
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := store.KeySlotDescriptor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	old := keySlotForAppTest(t, descriptor, baseCfg.Storage.MasterKey.PrimarySlot)
	oldCiphertext := bytes.Clone(old.WrappedKey)
	store.Close()
	options := KMSRevokeOptions{
		SlotID: old.ID, ConfirmSlotID: old.ID,
		ExpectedDescriptorRevision: descriptor.Revision, ExpectedSlotRevision: old.Revision,
		ReasonCode: "retirement_window_completed",
	}
	wrong := options
	wrong.ConfirmSlotID = "wrong"
	if _, err := revokeKMSKeySlotWithOptions(context.Background(), cfg, wrong, harness.factory, time.Now, nil); err == nil {
		t.Fatal("revoke accepted an inexact confirmation")
	}
	result, err := revokeKMSKeySlotWithOptions(context.Background(), cfg, options, harness.factory, time.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != masterkey.KeySlotRevoked || !result.DescriptorReady || result.AlreadyRevoked {
		t.Fatalf("result=%#v", result)
	}
	auditKey, err := vault.DeriveAuditHMACKey(bytes.Repeat([]byte{0x61}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer clear(auditKey)
	beforeRetry, err := audit.Verify(cfg.AuditPath(), auditKey)
	if err != nil {
		t.Fatal(err)
	}
	wrongRevision := options
	wrongRevision.ExpectedDescriptorRevision--
	if _, err := revokeKMSKeySlotWithOptions(context.Background(), cfg, wrongRevision, harness.factory, time.Now, nil); err == nil {
		t.Fatal("already-revoked retry accepted unrelated revisions")
	}
	wrongReason := options
	wrongReason.ReasonCode = "incident_retirement"
	if _, err := revokeKMSKeySlotWithOptions(context.Background(), cfg, wrongReason, harness.factory, time.Now, nil); err == nil {
		t.Fatal("already-revoked retry accepted a conflicting reason")
	}
	again, err := revokeKMSKeySlotWithOptions(context.Background(), cfg, options, harness.factory, time.Now, nil)
	if err != nil || !again.AlreadyRevoked {
		t.Fatalf("idempotent retry result=%#v err=%v", again, err)
	}
	afterRetry, err := audit.Verify(cfg.AuditPath(), auditKey)
	if err != nil || afterRetry.Records != beforeRetry.Records {
		t.Fatalf("retry appended duplicate audit event: before=%d after=%d err=%v", beforeRetry.Records, afterRetry.Records, err)
	}
	store, err = boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	after, err := store.KeySlotDescriptor(context.Background())
	store.Close()
	if err != nil {
		t.Fatal(err)
	}
	revoked := keySlotForAppTest(t, after, old.ID)
	if len(revoked.WrappedKey) != 0 || revoked.KeyReference != "" || len(revoked.ProviderParameters) != 0 {
		t.Fatalf("revoked Slot retained protected provider material: %#v", revoked)
	}
	raw, err := os.ReadFile(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(oldCiphertext) > 0 && bytes.Contains(raw, oldCiphertext) {
		t.Fatal("compacted metadata retained revoked Slot ciphertext")
	}
}

func TestKMSKeySlotStatusProvidesRevisionsWithoutKMSOrProviderMaterial(t *testing.T) {
	cfg, _, harness, _ := kmsRewrapFixture(t)
	before := harness.callCount()
	status, err := InspectKMSKeySlots(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if harness.callCount() != before {
		t.Fatal("Slot status called KMS")
	}
	if status.DescriptorRevision == 0 || !status.DescriptorReady || len(status.Slots) != 2 {
		t.Fatalf("status=%#v", status)
	}
	payload, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	for _, forbidden := range []string{"arn:aws:kms", "wrapped_key", "key_reference", "provider_parameters", "fingerprint"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("Slot status leaked %q: %s", forbidden, text)
		}
	}
}

func TestKMSRevokeRecoversAtEveryPublicationPoint(t *testing.T) {
	for _, point := range []string{"after_revoked_stage_persisted", "after_revoked_slot_compacted", "before_revoked_metadata_publish", "after_revoked_metadata_publish", "after_revoked_audit_append", "after_revoked_audit_checkpoint", "after_revoked_audit_intent_delivered", "after_revoked_audit_delivered"} {
		t.Run(point, func(t *testing.T) {
			baseCfg, cfg, harness, _ := kmsRewrapFixture(t)
			if _, err := rewrapKMSKeyWithOptions(context.Background(), cfg, KMSRewrapOptions{
				Purpose: masterkey.KeySlotPrimary, SlotID: cfg.Storage.MasterKey.PrimarySlot, KeyReference: replacementPrimaryKMSKeyARN,
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
			old := keySlotForAppTest(t, descriptor, baseCfg.Storage.MasterKey.PrimarySlot)
			options := KMSRevokeOptions{SlotID: old.ID, ConfirmSlotID: old.ID, ExpectedDescriptorRevision: descriptor.Revision, ExpectedSlotRevision: old.Revision}
			injected := errors.New("injected revoke process death")
			_, err = revokeKMSKeySlotWithOptions(context.Background(), cfg, options, harness.factory, time.Now, func(current string) error {
				if current == point {
					return injected
				}
				return nil
			})
			if !errors.Is(err, injected) {
				t.Fatalf("point=%s err=%v", point, err)
			}
			result, err := revokeKMSKeySlotWithOptions(context.Background(), cfg, options, harness.factory, time.Now, nil)
			published := point == "after_revoked_metadata_publish" || strings.HasPrefix(point, "after_revoked_audit")
			if err != nil || !result.DescriptorReady || (published && !result.AlreadyRevoked) || (!published && result.AlreadyRevoked) {
				t.Fatalf("recover %s result=%#v err=%v", point, result, err)
			}
		})
	}
}

func TestKMSRevokePublishesCleanMetadataBeforeFinalSuccessAudit(t *testing.T) {
	baseCfg, cfg, harness, _ := kmsRewrapFixture(t)
	if _, err := rewrapKMSKeyWithOptions(context.Background(), cfg, KMSRewrapOptions{
		Purpose: masterkey.KeySlotPrimary, SlotID: cfg.Storage.MasterKey.PrimarySlot, KeyReference: replacementPrimaryKMSKeyARN,
	}, harness.factory, time.Now, nil); err != nil {
		t.Fatal(err)
	}
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := store.KeySlotDescriptor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	old := keySlotForAppTest(t, descriptor, baseCfg.Storage.MasterKey.PrimarySlot)
	oldCiphertext := bytes.Clone(old.WrappedKey)
	store.Close()
	auditKey, err := vault.DeriveAuditHMACKey(bytes.Repeat([]byte{0x61}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer clear(auditKey)
	before, err := audit.Verify(cfg.AuditPath(), auditKey)
	if err != nil {
		t.Fatal(err)
	}
	options := KMSRevokeOptions{SlotID: old.ID, ConfirmSlotID: old.ID, ExpectedDescriptorRevision: descriptor.Revision, ExpectedSlotRevision: old.Revision, ReasonCode: "retirement_window_completed"}
	injected := errors.New("process died after metadata rename")
	_, err = revokeKMSKeySlotWithOptions(context.Background(), cfg, options, harness.factory, time.Now, func(point string) error {
		if point == "after_revoked_metadata_publish" {
			return injected
		}
		return nil
	})
	if !errors.Is(err, injected) {
		t.Fatalf("err=%v", err)
	}
	afterFailure, err := audit.Verify(cfg.AuditPath(), auditKey)
	if err != nil || afterFailure.Records != before.Records {
		t.Fatalf("final success Audit was written before delivery: before=%d after=%d err=%v", before.Records, afterFailure.Records, err)
	}
	raw, err := os.ReadFile(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, oldCiphertext) {
		t.Fatal("published revoked metadata retained old wrapped ciphertext")
	}
	store, err = boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	intent, err := store.KeySlotAuditIntent()
	if err != nil || intent.Delivered {
		store.Close()
		t.Fatalf("pending intent=%#v err=%v", intent, err)
	}
	auditLog, err := audit.Open(cfg.AuditPath(), auditKey)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := drainKeySlotAuditIntent(context.Background(), store, auditLog); err != nil {
		auditLog.Close()
		store.Close()
		t.Fatal(err)
	}
	auditLog.Close()
	delivered, err := store.KeySlotAuditIntent()
	store.Close()
	if err != nil || !delivered.Delivered {
		t.Fatalf("startup did not deliver pending intent=%#v err=%v", delivered, err)
	}
	if result, err := revokeKMSKeySlotWithOptions(context.Background(), cfg, options, harness.factory, time.Now, nil); err != nil || !result.AlreadyRevoked {
		t.Fatalf("idempotent retry result=%#v err=%v", result, err)
	}
	afterRecovery, err := audit.Verify(cfg.AuditPath(), auditKey)
	if err != nil || afterRecovery.Records != before.Records+1 {
		t.Fatalf("recovered Audit summary=%#v err=%v", afterRecovery, err)
	}
}

func TestKMSRecoveryRewrapUsesPrimaryAsIndependentSource(t *testing.T) {
	cfg := kmsAppTestConfig(t)
	harness := newKMSAppHarness(t)
	addKMSHarnessRoot(t, harness, replacementRecoveryKMSKeyARN, 0x44)
	if err := initializeKMS(context.Background(), cfg, kmsInitializationOptions{
		factory: harness.factory, random: bytes.NewReader(bytes.Repeat([]byte{0x62}, 32)),
	}); err != nil {
		t.Fatal(err)
	}
	oldRecoveryCalls := len(harness.wrappers[recoveryKMSKeyARN].Calls())
	primaryCalls := len(harness.wrappers[primaryKMSKeyARN].Calls())
	rewrapCfg := cfg
	rewrapCfg.Storage.MasterKey.RecoverySlot = "slot_aws_recovery_v2"
	rewrapCfg.Storage.MasterKey.AllowedKMSKeys = append(append([]config.AllowedKMSKey(nil), cfg.Storage.MasterKey.AllowedKMSKeys...), config.AllowedKMSKey{
		Purpose: "recovery", Provider: awskms.Provider, Region: "eu-central-1", Account: "456789012345",
		KeyID: replacementRecoveryKMSKeyARN, Algorithm: awskms.SymmetricDefaultAlgorithm,
	})
	if _, err := rewrapKMSKeyWithOptions(context.Background(), rewrapCfg, KMSRewrapOptions{
		Purpose: masterkey.KeySlotRecovery, SlotID: rewrapCfg.Storage.MasterKey.RecoverySlot,
		KeyReference: replacementRecoveryKMSKeyARN,
	}, harness.factory, time.Now, nil); err != nil {
		t.Fatal(err)
	}
	if len(harness.wrappers[primaryKMSKeyARN].Calls()) <= primaryCalls {
		t.Fatal("Recovery rewrap did not unlock through the independent Primary Slot")
	}
	if len(harness.wrappers[recoveryKMSKeyARN].Calls()) != oldRecoveryCalls {
		t.Fatal("Recovery rewrap attempted to use the retiring Recovery KMS Key")
	}
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	descriptor, err := store.KeySlotDescriptor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if keySlotForAppTest(t, descriptor, rewrapCfg.Storage.MasterKey.RecoverySlot).State != masterkey.KeySlotActive ||
		keySlotForAppTest(t, descriptor, cfg.Storage.MasterKey.RecoverySlot).State != masterkey.KeySlotRetiring ||
		keySlotForAppTest(t, descriptor, cfg.Storage.MasterKey.PrimarySlot).State != masterkey.KeySlotActive {
		t.Fatalf("descriptor=%#v", descriptor)
	}
}

func TestRecoveryRepairsPermanentlyUnavailablePrimaryBeforeColdStart(t *testing.T) {
	baseCfg, cfg, harness, _ := kmsRewrapFixture(t)
	brokenPrimary, err := fakekms.New(bytes.Repeat([]byte{0xee}, 32))
	if err != nil {
		t.Fatal(err)
	}
	harness.wrappers[primaryKMSKeyARN] = brokenPrimary
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	if key, err := unlockKMSMasterKey(context.Background(), baseCfg, store, masterkey.KeySlotPrimary, harness.factory); err == nil {
		clear(key)
		store.Close()
		t.Fatal("permanently unavailable Primary unexpectedly unlocked")
	}
	store.Close()
	recovery, err := VerifyRecoverySlot(context.Background(), baseCfg, baseCfg.Storage.MasterKey.RecoverySlot)
	if err != nil || !recovery.VaultVerified || !recovery.RecoveryAudited {
		t.Fatalf("recovery=%#v err=%v", recovery, err)
	}
	if _, err := rewrapKMSKeyWithOptions(context.Background(), cfg, KMSRewrapOptions{
		Purpose: masterkey.KeySlotPrimary, SlotID: cfg.Storage.MasterKey.PrimarySlot, KeyReference: replacementPrimaryKMSKeyARN,
	}, harness.factory, time.Now, nil); err != nil {
		t.Fatal(err)
	}
	brokenRecovery, err := fakekms.New(bytes.Repeat([]byte{0xdd}, 32))
	if err != nil {
		t.Fatal(err)
	}
	harness.wrappers[recoveryKMSKeyARN] = brokenRecovery
	store, err = boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	key, err := unlockKMSMasterKey(context.Background(), cfg, store, masterkey.KeySlotPrimary, harness.factory)
	if err != nil {
		t.Fatalf("repaired Primary cold-start unlock failed: %v", err)
	}
	clear(key)
}

func TestKMSRewrapFailsClosedForSuspectedCompromise(t *testing.T) {
	_, cfg, harness, _ := kmsRewrapFixture(t)
	before := harness.callCount()
	_, err := RewrapKMSKey(context.Background(), cfg, KMSRewrapOptions{
		Purpose: masterkey.KeySlotPrimary, SlotID: cfg.Storage.MasterKey.PrimarySlot,
		KeyReference: replacementPrimaryKMSKeyARN, Compromised: true,
	})
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("requires DEK rotation")) {
		t.Fatalf("compromise intent was not rejected: %v", err)
	}
	if harness.callCount() != before {
		t.Fatal("compromise rejection reached KMS instead of failing closed")
	}
}

func TestKMSDEKRotationReencryptsAllMaterialAndPreservesAudit(t *testing.T) {
	cfg, harness, oldKey, newKey, credentialID, oldAuditKey := kmsRotationFixture(t)
	defer clear(oldKey)
	defer clear(newKey)
	defer clear(oldAuditKey)
	beforeStore, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	beforeDescriptor, err := beforeStore.KeySlotDescriptor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	beforeCredential, err := beforeStore.GetCredential(context.Background(), credentialID)
	if err != nil {
		t.Fatal(err)
	}
	user, err := adminauth.NewUser("admin", []byte("correct horse battery staple"), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	user, err = beforeStore.PutAdminUser(context.Background(), user, 0)
	if err != nil {
		t.Fatal(err)
	}
	sessionHash := sha256.Sum256([]byte("kms-rotation-session"))
	now := time.Now().UTC()
	if err := beforeStore.PutAdminSession(context.Background(), domain.AdminSession{
		IDHash: sessionHash, Username: user.Username, Generation: user.SessionGeneration,
		CreatedAt: now, LastSeenAt: now, AbsoluteExpiresAt: now.Add(time.Hour), IdleExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	oldVault, err := vault.New(oldKey)
	if err != nil {
		t.Fatal(err)
	}
	mfaSecret := []byte("12345678901234567890")
	mfaCiphertext, err := oldVault.EncryptAdminMFA("mfa_kms_rotate", user.Username, mfaSecret)
	oldVault.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := beforeStore.PutAdminMFAAuthenticator(context.Background(), domain.AdminMFAAuthenticator{
		ID: "mfa_kms_rotate", Username: user.Username, Name: "rotation phone", Type: domain.AdminMFATypeTOTP,
		SecretCiphertext: mfaCiphertext, Status: domain.AdminMFAStatusActive, CreatedAt: now, ConfirmedAt: &now,
	}, 0); err != nil {
		t.Fatal(err)
	}
	if err := beforeStore.Close(); err != nil {
		t.Fatal(err)
	}

	recorder := &kmsAuditRecorder{}
	result, err := rotateKMSMasterKeyWithOptions(withKMSAuditRecorder(context.Background(), recorder), cfg, kmsRotationOptions{
		factory: harness.factory, random: bytes.NewReader(newKey), operationID: "rotate-production-001",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.OldKeyVersion != 1 || result.NewKeyVersion != 2 || result.OldFingerprint == result.NewFingerprint {
		t.Fatalf("result=%#v", result)
	}
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	descriptor, err := store.KeySlotDescriptor(context.Background())
	if err != nil || descriptor.ActiveGeneration != beforeDescriptor.ActiveGeneration+1 || descriptor.MasterKeyFingerprint != result.NewFingerprint {
		t.Fatalf("descriptor=%#v err=%v", descriptor, err)
	}
	activeKey, err := unlockKMSMasterKey(context.Background(), cfg, store, masterkey.KeySlotPrimary, harness.factory)
	if err != nil || !bytes.Equal(activeKey, newKey) {
		t.Fatalf("new Primary mismatch=%t err=%v", bytes.Equal(activeKey, newKey), err)
	}
	defer clear(activeKey)
	newVault, err := vault.New(activeKey)
	if err != nil {
		t.Fatal(err)
	}
	defer newVault.Close()
	credential, err := store.GetCredential(context.Background(), credentialID)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(credential.Ciphertext, beforeCredential.Ciphertext) || credential.KeyVersion != beforeCredential.KeyVersion+1 {
		t.Fatal("DEK rotation did not advance Credential ciphertext generation")
	}
	plaintext, err := newVault.DecryptCredential(credential.ID, string(credential.Type), credential.Audience, credential.Ciphertext)
	if err != nil || !bytes.Equal(plaintext, []byte("kms-rotation-provider-secret")) {
		t.Fatalf("rotated Credential plaintext mismatch: %v", err)
	}
	clear(plaintext)
	mfa, err := store.GetAdminMFAAuthenticator(context.Background(), user.Username, "mfa_kms_rotate")
	if err != nil {
		t.Fatal(err)
	}
	mfaPlaintext, err := newVault.DecryptAdminMFA(mfa.ID, mfa.Username, mfa.SecretCiphertext)
	if err != nil || !bytes.Equal(mfaPlaintext, mfaSecret) {
		t.Fatalf("rotated MFA mismatch: %v", err)
	}
	clear(mfaPlaintext)
	if _, err := store.GetAdminSession(context.Background(), sessionHash); !errors.Is(err, boltstore.ErrNotFound) {
		t.Fatalf("Admin Session survived KMS DEK rotation: %v", err)
	}
	protectedAuditKey, err := loadAuditHMACKey(store, newVault, activeKey)
	if err != nil || !bytes.Equal(protectedAuditKey, oldAuditKey) {
		t.Fatalf("Audit HMAC continuity failed: %v", err)
	}
	clear(protectedAuditKey)
	if _, err := store.VaultRotationBridge(); !errors.Is(err, boltstore.ErrNotFound) {
		t.Fatalf("rotation bridge survived compaction: %v", err)
	}
	rawMetadata, err := os.ReadFile(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(rawMetadata, beforeCredential.Ciphertext) {
		t.Fatal("compacted KMS metadata retained retired Credential ciphertext")
	}
	for _, slot := range beforeDescriptor.Slots {
		if bytes.Contains(rawMetadata, slot.WrappedKey) {
			t.Fatalf("compacted KMS metadata retained retired Slot ciphertext for %s", slot.ID)
		}
	}
	if _, err := audit.Verify(cfg.AuditPath(), oldAuditKey); err != nil {
		t.Fatalf("historical Audit chain failed: %v", err)
	}
	auditLog, err := audit.Open(cfg.AuditPath(), oldAuditKey)
	if err != nil {
		t.Fatal(err)
	}
	providerCalls := 0
	_, err = auditLog.Replay(func(record audit.Record) error {
		if record.Event.Action == "security.kms.call" && record.Event.ActorType == "local_cli" {
			providerCalls++
		}
		return nil
	})
	auditLog.Close()
	if err != nil || providerCalls == 0 {
		t.Fatalf("rotation provider Audit calls=%d err=%v", providerCalls, err)
	}
}

func TestKMSDEKRotationRecoversIdempotentlyAtEveryPublicationPoint(t *testing.T) {
	points := []string{
		"after_new_slots_verified", "after_rotation_started_audit_append", "after_rotation_started_audit_checkpoint", "after_rotation_started_audit_intent_delivered", "after_started_audit", "after_metadata_snapshot",
		"after_rewrite_verification", "before_metadata_publish", "after_metadata_publish",
		"after_persisted_primary_verified", "after_rotation_completed_intent_persisted", "before_bridge_cleanup_publish", "after_bridge_cleanup_publish",
		"after_rotation_completed_audit_append", "after_rotation_completed_audit_checkpoint", "after_rotation_completed_audit_intent_delivered", "after_rotation_completed_audit_delivered",
	}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			cfg, harness, oldKey, newKey, _, oldAuditKey := kmsRotationFixture(t)
			defer clear(oldKey)
			defer clear(newKey)
			defer clear(oldAuditKey)
			injected := errors.New("injected rotation process death")
			operationID := "rotate-killpoint-" + point
			_, err := rotateKMSMasterKeyWithOptions(context.Background(), cfg, kmsRotationOptions{
				factory: harness.factory, random: bytes.NewReader(newKey), operationID: operationID,
				hook: func(current string) error {
					if current == point {
						return injected
					}
					return nil
				},
			})
			if !errors.Is(err, injected) {
				t.Fatalf("point=%s err=%v", point, err)
			}
			result, err := rotateKMSMasterKeyWithOptions(context.Background(), cfg, kmsRotationOptions{
				factory: harness.factory, random: bytes.NewReader(newKey), operationID: operationID,
			})
			if err != nil || result.NewKeyVersion != 2 {
				t.Fatalf("recover %s result=%#v err=%v", point, result, err)
			}
			again, err := rotateKMSMasterKeyWithOptions(context.Background(), cfg, kmsRotationOptions{
				factory: harness.factory, random: bytes.NewReader(bytes.Repeat([]byte{0x7f}, 32)), operationID: operationID,
			})
			if err != nil || again.NewKeyVersion != 2 || !again.RecoveredPending {
				t.Fatalf("idempotent retry %s result=%#v err=%v", point, again, err)
			}
			log, err := audit.Open(cfg.AuditPath(), oldAuditKey)
			if err != nil {
				t.Fatal(err)
			}
			counts := map[string]int{}
			_, err = log.Replay(func(record audit.Record) error {
				if record.Event.Action == "security.master_key_rotation.started" || record.Event.Action == "security.master_key_rotation.completed" {
					counts[record.Event.Action]++
					if record.Event.ActorType != "local_cli" || record.Event.CorrelationID != operationID {
						t.Fatalf("point=%s event=%#v", point, record.Event)
					}
				}
				return nil
			})
			log.Close()
			if err != nil || counts["security.master_key_rotation.started"] != 1 || counts["security.master_key_rotation.completed"] != 1 {
				t.Fatalf("point=%s counts=%v replay=%v", point, counts, err)
			}
		})
	}
}

func kmsRewrapFixture(t *testing.T) (config.Config, config.Config, *kmsAppHarness, string) {
	t.Helper()
	cfg := kmsAppTestConfig(t)
	harness := newKMSAppHarness(t)
	addKMSHarnessRoot(t, harness, replacementPrimaryKMSKeyARN, 0x33)
	oldKey := bytes.Repeat([]byte{0x61}, 32)
	if err := initializeKMS(context.Background(), cfg, kmsInitializationOptions{factory: harness.factory, random: bytes.NewReader(oldKey)}); err != nil {
		t.Fatal(err)
	}
	previousFactory := defaultKMSWrapperFactory
	defaultKMSWrapperFactory = harness.factory
	t.Cleanup(func() { defaultKMSWrapperFactory = previousFactory })
	_, err := Bootstrap(context.Background(), cfg, BootstrapOptions{
		ProviderName: "OpenAI", ProviderType: domain.ProviderOpenAI, ProviderBaseURL: "https://api.openai.com",
		ProviderModel: "gpt-test", PublicModel: "chat", ProjectName: "Rewrap",
	}, []byte("rewrap-provider-secret"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := store.ListCredentials(context.Background())
	store.Close()
	if err != nil || len(credentials) != 1 {
		t.Fatalf("credentials=%d err=%v", len(credentials), err)
	}
	rewrapCfg := cfg
	rewrapCfg.Storage.MasterKey.PrimarySlot = "slot_aws_primary_v2"
	rewrapCfg.Storage.MasterKey.AllowedKMSKeys = append(append([]config.AllowedKMSKey(nil), cfg.Storage.MasterKey.AllowedKMSKeys...), config.AllowedKMSKey{
		Purpose: "primary", Provider: awskms.Provider, Region: "eu-west-1", Account: "345678901234",
		KeyID: replacementPrimaryKMSKeyARN, Algorithm: awskms.SymmetricDefaultAlgorithm,
	})
	if err := rewrapCfg.Validate(config.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	return cfg, rewrapCfg, harness, credentials[0].ID
}

func kmsRotationFixture(t *testing.T) (config.Config, *kmsAppHarness, []byte, []byte, string, []byte) {
	t.Helper()
	cfg := kmsAppTestConfig(t)
	harness := newKMSAppHarness(t)
	oldKey := bytes.Repeat([]byte{0x41}, 32)
	newKey := bytes.Repeat([]byte{0x42}, 32)
	if err := initializeKMS(context.Background(), cfg, kmsInitializationOptions{factory: harness.factory, random: bytes.NewReader(oldKey)}); err != nil {
		t.Fatal(err)
	}
	previousFactory := defaultKMSWrapperFactory
	defaultKMSWrapperFactory = harness.factory
	t.Cleanup(func() { defaultKMSWrapperFactory = previousFactory })
	if _, err := Bootstrap(context.Background(), cfg, BootstrapOptions{
		ProviderName: "OpenAI", ProviderType: domain.ProviderOpenAI, ProviderBaseURL: "https://api.openai.com",
		ProviderModel: "gpt-test", PublicModel: "chat", ProjectName: "Rotate",
	}, []byte("kms-rotation-provider-secret")); err != nil {
		t.Fatal(err)
	}
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := store.ListCredentials(context.Background())
	store.Close()
	if err != nil || len(credentials) != 1 {
		t.Fatalf("credentials=%d err=%v", len(credentials), err)
	}
	auditKey, err := vault.DeriveAuditHMACKey(oldKey)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, harness, bytes.Clone(oldKey), bytes.Clone(newKey), credentials[0].ID, auditKey
}

func addKMSHarnessRoot(t *testing.T, harness *kmsAppHarness, keyARN string, rootByte byte) {
	t.Helper()
	wrapper, err := fakekms.New(bytes.Repeat([]byte{rootByte}, 32))
	if err != nil {
		t.Fatal(err)
	}
	harness.wrappers[keyARN] = wrapper
}
