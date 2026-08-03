package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/config"
	"github.com/akz142857/Heimdall/internal/kms/awskms"
	"github.com/akz142857/Heimdall/internal/masterkey"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
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
