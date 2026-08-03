package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/config"
	"github.com/akz142857/Heimdall/internal/masterkey"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
)

func TestMasterKeyCustodyViewRedactsProviderMaterial(t *testing.T) {
	now := time.Now().UTC()
	descriptor := masterkey.KeySlotDescriptor{
		FormatVersion: masterkey.KeySlotDescriptorFormatVersion, MasterKeyFingerprint: "sha256:" + strings.Repeat("a", 64),
		ActiveGeneration: 1, Revision: 4,
		Slots: []masterkey.KeySlot{
			{ID: "slot_primary_secret", Purpose: masterkey.KeySlotPrimary, Provider: "aws-kms", KeyReference: "arn:aws:kms:secret", Algorithm: "SYMMETRIC_DEFAULT", ProviderParameters: map[string]string{"instance_id": "secret"}, WrappedKey: []byte("ciphertext-secret"), MasterKeyFingerprint: "sha256:" + strings.Repeat("a", 64), State: masterkey.KeySlotActive, Revision: 2, VerifiedAt: &now, CreatedAt: now, UpdatedAt: now},
			{ID: "slot_recovery_secret", Purpose: masterkey.KeySlotRecovery, Provider: "aws-kms", KeyReference: "arn:aws:kms:recovery-secret", Algorithm: "SYMMETRIC_DEFAULT", ProviderParameters: map[string]string{"instance_id": "secret"}, WrappedKey: []byte("recovery-ciphertext"), MasterKeyFingerprint: "sha256:" + strings.Repeat("a", 64), State: masterkey.KeySlotActive, Revision: 2, VerifiedAt: &now, CreatedAt: now, UpdatedAt: now},
		},
	}
	view := buildMasterKeyCustodyView(config.MasterKeyModeKeySlots, descriptor, boltstore.VaultKeyring{}, time.Time{}, now)
	payload, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	for _, secret := range []string{"slot_primary_secret", "slot_recovery_secret", "arn:aws:kms", "ciphertext-secret", "sha256:"} {
		if strings.Contains(text, secret) {
			t.Fatalf("custody response leaked %q: %s", secret, text)
		}
	}
	if !view.DescriptorReady || view.CustodyState != "healthy" || view.RotationIncomplete || view.RecoveryVerifiedAt == nil || view.RecoveryVerificationExpired || len(view.Slots) != 2 || view.ProductionAdmission != "external_evidence_required" {
		t.Fatalf("view=%#v", view)
	}
}

func TestMasterKeyCustodyViewReportsExpiryAndRotationType(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-recoveryVerificationMaxAge - time.Hour)
	fingerprint := "sha256:" + strings.Repeat("b", 64)
	descriptor := masterkey.KeySlotDescriptor{
		FormatVersion: masterkey.KeySlotDescriptorFormatVersion, MasterKeyFingerprint: fingerprint,
		ActiveGeneration: 2, Revision: 7,
		Slots: []masterkey.KeySlot{
			{ID: "slot_primary", Purpose: masterkey.KeySlotPrimary, Provider: "aws-kms", KeyReference: "primary", WrappedKey: []byte("p"), MasterKeyFingerprint: fingerprint, State: masterkey.KeySlotActive, Revision: 2, VerifiedAt: &now, CreatedAt: old, UpdatedAt: now},
			{ID: "slot_recovery", Purpose: masterkey.KeySlotRecovery, Provider: "aws-kms", KeyReference: "recovery", WrappedKey: []byte("r"), MasterKeyFingerprint: fingerprint, State: masterkey.KeySlotActive, Revision: 2, VerifiedAt: &old, CreatedAt: old, UpdatedAt: old},
		},
	}
	view := buildMasterKeyCustodyView(config.MasterKeyModeKeySlots, descriptor, boltstore.VaultKeyring{RecoveryEnvelope: []byte("pending")}, time.Time{}, now)
	if view.CustodyState != "degraded" || !view.RotationIncomplete || view.LifecycleOperation != "dek_rotate" || !view.RecoveryVerificationExpired {
		t.Fatalf("view=%#v", view)
	}
	file := buildFileMasterKeyCustodyView()
	if file.Mode != config.MasterKeyModeFile || !file.DescriptorReady || file.ProductionAdmission != "not_applicable" || file.CustodyState != "healthy" {
		t.Fatalf("file=%#v", file)
	}
}
