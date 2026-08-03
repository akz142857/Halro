package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
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

func TestKMSRewrapRecoversIdempotentlyAtEveryPublicationPoint(t *testing.T) {
	for _, point := range []string{"after_pending_slot_published", "after_active_slot_published", "after_old_slot_retired"} {
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
		})
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

	result, err := rotateKMSMasterKeyWithOptions(context.Background(), cfg, kmsRotationOptions{
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
}

func TestKMSDEKRotationRecoversIdempotentlyAtEveryPublicationPoint(t *testing.T) {
	points := []string{
		"after_new_slots_verified", "after_started_audit", "after_metadata_snapshot",
		"after_rewrite_verification", "before_metadata_publish", "after_metadata_publish",
		"after_persisted_primary_verified", "before_bridge_cleanup_publish", "after_bridge_cleanup_publish",
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
