package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/config"
	"github.com/akz142857/Heimdall/internal/masterkey"
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
	view := buildMasterKeyCustodyView(config.MasterKeyModeKeySlots, descriptor)
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
	if !view.ProductionReady || view.RotationIncomplete || view.RecoveryVerifiedAt == nil || len(view.Slots) != 2 {
		t.Fatalf("view=%#v", view)
	}
}
