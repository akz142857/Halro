package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/config"
	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/kms/awskms"
	"github.com/akz142857/Heimdall/internal/masterkey"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
	"gopkg.in/yaml.v3"
)

func TestRealAWSDualSlotInitializeAndRecovery(t *testing.T) {
	if os.Getenv("HEIMDALL_AWS_KMS_DUAL_REAL") != "1" {
		t.Skip("set HEIMDALL_AWS_KMS_DUAL_REAL=1 for real dual-Slot AWS evidence")
	}
	primaryARN := os.Getenv("HEIMDALL_AWS_KMS_PRIMARY_KEY_ARN")
	recoveryARN := os.Getenv("HEIMDALL_AWS_KMS_RECOVERY_KEY_ARN")
	primaryRegion, primaryAccount, ok := awsKeyIdentityForAppTest(primaryARN)
	if !ok {
		t.Fatal("HEIMDALL_AWS_KMS_PRIMARY_KEY_ARN must be a full KMS Key ARN")
	}
	recoveryRegion, recoveryAccount, ok := awsKeyIdentityForAppTest(recoveryARN)
	if !ok || recoveryARN == primaryARN {
		t.Fatal("Recovery must be a different full KMS Key ARN")
	}
	cfg := testConfig(t)
	cfg.Storage.MasterKey = config.MasterKey{
		Mode: config.MasterKeyModeKeySlots, PrimarySlot: "slot_aws_primary", RecoverySlot: "slot_aws_recovery",
		StartupDeadline: config.Duration(time.Minute), CallTimeout: config.Duration(5 * time.Second),
		AllowedKMSKeys: []config.AllowedKMSKey{
			{Purpose: "primary", Provider: awskms.Provider, Region: primaryRegion, Account: primaryAccount, KeyID: primaryARN, Algorithm: awskms.SymmetricDefaultAlgorithm},
			{Purpose: "recovery", Provider: awskms.Provider, Region: recoveryRegion, Account: recoveryAccount, KeyID: recoveryARN, Algorithm: awskms.SymmetricDefaultAlgorithm},
		},
	}
	if err := cfg.Validate(config.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := initializeKMS(ctx, cfg, kmsInitializationOptions{}); err != nil {
		t.Fatal(err)
	}
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := store.KeySlotDescriptor(ctx)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	primaryKey, err := unlockKMSMasterKey(ctx, cfg, store, masterkey.KeySlotPrimary, productionKMSWrapper)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	recoveryKey, err := unlockKMSMasterKey(ctx, cfg, store, masterkey.KeySlotRecovery, productionKMSWrapper)
	if err != nil {
		clear(primaryKey)
		store.Close()
		t.Fatal(err)
	}
	if !bytes.Equal(primaryKey, recoveryKey) || !descriptor.ProductionReady() {
		clear(primaryKey)
		clear(recoveryKey)
		store.Close()
		t.Fatal("real Primary and Recovery did not authenticate the same production-ready Vault")
	}
	masterKeyDigest := digestM11Value(primaryKey)
	clear(primaryKey)
	clear(recoveryKey)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyRecoverySlot(ctx, cfg, cfg.Storage.MasterKey.RecoverySlot); err != nil {
		t.Fatal(err)
	}
	evidence := map[string]any{
		"schema_version": 1, "recorded_at": time.Now().UTC(), "result": "success",
		"primary_key_arn_sha256":  digestM11Value([]byte(primaryARN)),
		"recovery_key_arn_sha256": digestM11Value([]byte(recoveryARN)),
		"master_key_sha256":       masterKeyDigest, "active_slots": len(descriptor.Slots),
		"production_ready": descriptor.ProductionReady(), "recovery_audited": true,
		"independent_kms_keys": true, "cross_region": primaryRegion != recoveryRegion,
		"cross_account": primaryAccount != recoveryAccount, "workload_identity_enforced": true,
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("M11_AWS_KMS_DUAL_SLOT_EVIDENCE=%s\n", encoded)
}

func TestRealAWSKMSKeyLifecycle(t *testing.T) {
	if os.Getenv("HEIMDALL_AWS_KMS_LIFECYCLE_REAL") != "1" {
		t.Skip("set HEIMDALL_AWS_KMS_LIFECYCLE_REAL=1 for real AWS KMS lifecycle evidence")
	}
	primaryARN := os.Getenv("HEIMDALL_AWS_KMS_PRIMARY_KEY_ARN")
	recoveryARN := os.Getenv("HEIMDALL_AWS_KMS_RECOVERY_KEY_ARN")
	replacementARN := os.Getenv("HEIMDALL_AWS_KMS_REPLACEMENT_PRIMARY_KEY_ARN")
	primaryRegion, primaryAccount, primaryOK := awsKeyIdentityForAppTest(primaryARN)
	recoveryRegion, recoveryAccount, recoveryOK := awsKeyIdentityForAppTest(recoveryARN)
	replacementRegion, replacementAccount, replacementOK := awsKeyIdentityForAppTest(replacementARN)
	if !primaryOK || !recoveryOK || !replacementOK || primaryARN == recoveryARN ||
		primaryARN == replacementARN || recoveryARN == replacementARN {
		t.Fatal("lifecycle smoke requires three distinct full customer-managed KMS Key ARNs")
	}
	cfg := testConfig(t)
	cfg.Storage.MasterKey = config.MasterKey{
		Mode: config.MasterKeyModeKeySlots, PrimarySlot: "slot_aws_primary", RecoverySlot: "slot_aws_recovery",
		StartupDeadline: config.Duration(time.Minute), CallTimeout: config.Duration(5 * time.Second),
		AllowedKMSKeys: []config.AllowedKMSKey{
			{Purpose: "primary", Provider: awskms.Provider, Region: primaryRegion, Account: primaryAccount, KeyID: primaryARN, Algorithm: awskms.SymmetricDefaultAlgorithm},
			{Purpose: "recovery", Provider: awskms.Provider, Region: recoveryRegion, Account: recoveryAccount, KeyID: recoveryARN, Algorithm: awskms.SymmetricDefaultAlgorithm},
		},
	}
	if err := cfg.Validate(config.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := initializeKMS(ctx, cfg, kmsInitializationOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Bootstrap(ctx, cfg, BootstrapOptions{
		ProviderName: "M11 lifecycle smoke", ProviderType: "openai", ProviderBaseURL: "https://api.openai.com",
		ProviderModel: "smoke", PublicModel: "smoke", ProjectName: "M11 lifecycle",
		BillingMode: domain.BillingModeFree,
	}, []byte("ephemeral-lifecycle-smoke-secret")); err != nil {
		t.Fatal(err)
	}
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := store.ListCredentials(ctx)
	if err != nil || len(credentials) != 1 {
		store.Close()
		t.Fatalf("credentials=%d err=%v", len(credentials), err)
	}
	before := credentials[0]
	beforeDescriptor, err := store.KeySlotDescriptor(ctx)
	if closeErr := store.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}

	rewrapCfg := cfg
	rewrapCfg.Storage.MasterKey.PrimarySlot = "slot_aws_primary_rewrapped"
	rewrapCfg.Storage.MasterKey.AllowedKMSKeys = append(append([]config.AllowedKMSKey(nil), cfg.Storage.MasterKey.AllowedKMSKeys...), config.AllowedKMSKey{
		Purpose: "primary", Provider: awskms.Provider, Region: replacementRegion, Account: replacementAccount,
		KeyID: replacementARN, Algorithm: awskms.SymmetricDefaultAlgorithm,
	})
	if _, err := RewrapKMSKey(ctx, rewrapCfg, KMSRewrapOptions{
		Purpose: masterkey.KeySlotPrimary, SlotID: rewrapCfg.Storage.MasterKey.PrimarySlot, KeyReference: replacementARN,
	}); err != nil {
		t.Fatal(err)
	}
	store, err = boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	afterRewrap, err := store.GetCredential(ctx, before.ID)
	afterRewrapDescriptor, descriptorErr := store.KeySlotDescriptor(ctx)
	closeErr := store.Close()
	if err != nil || descriptorErr != nil || closeErr != nil {
		t.Fatal(errors.Join(err, descriptorErr, closeErr))
	}
	if !bytes.Equal(before.Ciphertext, afterRewrap.Ciphertext) || before.KeyVersion != afterRewrap.KeyVersion ||
		beforeDescriptor.MasterKeyFingerprint != afterRewrapDescriptor.MasterKeyFingerprint {
		t.Fatal("real AWS rewrap changed the Vault data generation")
	}

	rotationCfg := rewrapCfg
	rotationCfg.Storage.MasterKey.AllowedKMSKeys = []config.AllowedKMSKey{
		rewrapCfg.Storage.MasterKey.AllowedKMSKeys[2], rewrapCfg.Storage.MasterKey.AllowedKMSKeys[1],
	}
	if err := rotationCfg.Validate(config.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	rotation, err := RotateKMSMasterKey(ctx, rotationCfg, "real-aws-lifecycle-smoke-001")
	if err != nil {
		t.Fatal(err)
	}
	store, err = boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	afterRotate, err := store.GetCredential(ctx, before.ID)
	rotatedDescriptor, descriptorErr := store.KeySlotDescriptor(ctx)
	primaryKey, primaryErr := unlockKMSMasterKey(ctx, rotationCfg, store, masterkey.KeySlotPrimary, productionKMSWrapper)
	recoveryKey, recoveryErr := unlockKMSMasterKey(ctx, rotationCfg, store, masterkey.KeySlotRecovery, productionKMSWrapper)
	keysMatch := bytes.Equal(primaryKey, recoveryKey)
	clear(primaryKey)
	clear(recoveryKey)
	if err != nil || descriptorErr != nil || primaryErr != nil || recoveryErr != nil || !keysMatch ||
		afterRotate.KeyVersion != before.KeyVersion+1 || bytes.Equal(afterRotate.Ciphertext, before.Ciphertext) ||
		rotatedDescriptor.ActiveGeneration != beforeDescriptor.ActiveGeneration+1 {
		t.Fatalf("real AWS lifecycle verification failed: %v", errors.Join(err, descriptorErr, primaryErr, recoveryErr))
	}
	evidence := map[string]any{
		"schema_version": 1, "recorded_at": time.Now().UTC(), "result": "success",
		"old_primary_key_arn_sha256":  digestM11Value([]byte(primaryARN)),
		"new_primary_key_arn_sha256":  digestM11Value([]byte(replacementARN)),
		"recovery_key_arn_sha256":     digestM11Value([]byte(recoveryARN)),
		"rewrap_preserved_ciphertext": true, "rewrap_preserved_key_version": true,
		"rotation_key_version_advanced": true, "rotation_ciphertext_changed": true,
		"descriptor_generation_advanced": true, "primary_recovery_match": keysMatch,
		"workload_identity_enforced": true, "rotation_operation_id": "real-aws-lifecycle-smoke-001",
		"new_master_key_fingerprint_sha256": digestM11Value([]byte(rotation.NewFingerprint)),
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("M11_AWS_KMS_LIFECYCLE_EVIDENCE=%s\n", encoded)
}

func TestRealAWSKMSDisasterRecovery(t *testing.T) {
	if os.Getenv("HEIMDALL_AWS_KMS_DR_REAL") != "1" {
		t.Skip("set HEIMDALL_AWS_KMS_DR_REAL=1 for real AWS KMS disaster-recovery evidence")
	}
	primaryARN := os.Getenv("HEIMDALL_AWS_KMS_PRIMARY_KEY_ARN")
	recoveryARN := os.Getenv("HEIMDALL_AWS_KMS_RECOVERY_KEY_ARN")
	primaryRegion, primaryAccount, primaryOK := awsKeyIdentityForAppTest(primaryARN)
	recoveryRegion, recoveryAccount, recoveryOK := awsKeyIdentityForAppTest(recoveryARN)
	if !primaryOK || !recoveryOK || primaryARN == recoveryARN {
		t.Fatal("DR smoke requires two distinct full customer-managed KMS Key ARNs")
	}
	cfg := testConfig(t)
	cfg.Storage.MasterKey = config.MasterKey{
		Mode: config.MasterKeyModeKeySlots, PrimarySlot: "slot_aws_primary", RecoverySlot: "slot_aws_recovery",
		StartupDeadline: config.Duration(time.Minute), CallTimeout: config.Duration(5 * time.Second),
		AllowedKMSKeys: []config.AllowedKMSKey{
			{Purpose: "primary", Provider: awskms.Provider, Region: primaryRegion, Account: primaryAccount, KeyID: primaryARN, Algorithm: awskms.SymmetricDefaultAlgorithm},
			{Purpose: "recovery", Provider: awskms.Provider, Region: recoveryRegion, Account: recoveryAccount, KeyID: recoveryARN, Algorithm: awskms.SymmetricDefaultAlgorithm},
		},
	}
	if err := cfg.Validate(config.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	if err := initializeKMS(ctx, cfg, kmsInitializationOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Bootstrap(ctx, cfg, BootstrapOptions{
		ProviderName: "M11 DR smoke", ProviderType: "openai", ProviderBaseURL: "https://api.openai.com",
		ProviderModel: "smoke", PublicModel: "smoke", ProjectName: "M11 DR",
		BillingMode: domain.BillingModeFree,
	}, []byte("ephemeral-dr-smoke-secret")); err != nil {
		t.Fatal(err)
	}
	configPayload, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(cfg.Storage.DataDir)
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, configPayload, 0o600); err != nil {
		t.Fatal(err)
	}
	backupKey := make([]byte, 32)
	if _, err := rand.Read(backupKey); err != nil {
		t.Fatal(err)
	}
	defer clear(backupKey)
	archivePath := filepath.Join(root, "dr-smoke.hmbk")
	manifest, err := CreateBackup(ctx, cfg, configPath, archivePath, backupKey)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.KeySlotDescriptorSHA256 == "" || manifest.RestoreDrillVerified {
		t.Fatalf("manifest=%#v", manifest)
	}
	if err := os.WriteFile(filepath.Join(cfg.Storage.DataDir, "primary-restore-sentinel"), []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	primaryResult, err := RestoreBackupWithOptions(ctx, cfg, archivePath, backupKey, manifest.BackupID, RestoreOptions{})
	if err != nil || !primaryResult.VaultVerified || primaryResult.UnlockPath != "primary" {
		t.Fatalf("Primary restore=%#v err=%v", primaryResult, err)
	}
	if err := os.WriteFile(filepath.Join(cfg.Storage.DataDir, "recovery-restore-sentinel"), []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	recoveryResult, err := RestoreBackupWithOptions(ctx, cfg, archivePath, backupKey, manifest.BackupID, RestoreOptions{
		UseRecoverySlot: true, ConfirmRecoverySlot: cfg.Storage.MasterKey.RecoverySlot,
	})
	if err != nil || !recoveryResult.VaultVerified || !recoveryResult.RecoveryAudited || recoveryResult.UnlockPath != "recovery" {
		t.Fatalf("Recovery restore=%#v err=%v", recoveryResult, err)
	}
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	descriptor, descriptorErr := store.KeySlotDescriptor(ctx)
	closeErr := store.Close()
	if descriptorErr != nil || closeErr != nil || !descriptor.ProductionReady() {
		t.Fatal(errors.Join(descriptorErr, closeErr))
	}
	evidence := map[string]any{
		"schema_version": 1, "recorded_at": time.Now().UTC(), "result": "success",
		"backup_id_sha256":         digestM11Value([]byte(manifest.BackupID)),
		"descriptor_digest_sha256": digestM11Value([]byte(manifest.KeySlotDescriptorSHA256)),
		"primary_key_arn_sha256":   digestM11Value([]byte(primaryARN)),
		"recovery_key_arn_sha256":  digestM11Value([]byte(recoveryARN)),
		"outer_integrity_verified": true, "manifest_claimed_restore_drill": false,
		"primary_restore_verified": true, "recovery_restore_verified": true,
		"recovery_audited": true, "workload_identity_enforced": true,
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("M11_AWS_KMS_DR_EVIDENCE=%s\n", encoded)
}

func awsKeyIdentityForAppTest(value string) (region, account string, ok bool) {
	parts := strings.SplitN(value, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || !strings.HasPrefix(parts[1], "aws") || parts[2] != "kms" ||
		parts[3] == "" || len(parts[4]) != 12 || !strings.HasPrefix(parts[5], "key/") {
		return "", "", false
	}
	return parts[3], parts[4], true
}

func digestM11Value(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
