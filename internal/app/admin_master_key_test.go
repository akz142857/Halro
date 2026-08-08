package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/masterkey"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
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
	if !view.LocalCustodyReady || view.CustodyState != "healthy" || view.RotationIncomplete || view.RecoveryVerifiedAt == nil || view.RecoveryVerificationStatus != recoveryVerificationCurrent || len(view.Slots) != 2 || view.ProductionAdmission != "external_evidence_required" {
		t.Fatalf("view=%#v", view)
	}
	if strings.Contains(text, "descriptor_ready") || strings.Contains(text, "recovery_verification_expired") {
		t.Fatalf("custody response retained mode-specific legacy fields: %s", text)
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
	if view.CustodyState != "degraded" || !view.RotationIncomplete || view.LifecycleOperation != "dek_rotate" || view.RecoveryVerificationStatus != recoveryVerificationExpired {
		t.Fatalf("view=%#v", view)
	}
	file := buildFileMasterKeyCustodyView()
	if file.Mode != config.MasterKeyModeFile || !file.LocalCustodyReady || file.ProductionAdmission != "not_applicable" || file.CustodyState != "healthy" || file.RecoveryVerificationStatus != recoveryVerificationNotApplicable || file.RecoveryVerifiedAt != nil || file.LifecycleRunbookURL != "" || file.RecoveryRunbookURL != "" {
		t.Fatalf("file=%#v", file)
	}
}

func TestMasterKeyCustodyViewRecoveryVerificationStates(t *testing.T) {
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	fingerprint := "sha256:" + strings.Repeat("c", 64)
	primaryVerified := now.Add(-time.Hour)
	base := masterkey.KeySlotDescriptor{
		FormatVersion: masterkey.KeySlotDescriptorFormatVersion, MasterKeyFingerprint: fingerprint,
		ActiveGeneration: 1, Revision: 4,
		Slots: []masterkey.KeySlot{
			{ID: "slot_primary", Purpose: masterkey.KeySlotPrimary, Provider: "aws-kms", KeyReference: "primary", WrappedKey: []byte("p"), MasterKeyFingerprint: fingerprint, State: masterkey.KeySlotActive, Revision: 2, VerifiedAt: &primaryVerified, CreatedAt: primaryVerified, UpdatedAt: primaryVerified},
		},
	}
	tests := []struct {
		name       string
		verifiedAt *time.Time
		lastUsed   time.Time
		want       string
		degraded   bool
	}{
		{name: "missing", want: recoveryVerificationMissing, degraded: true},
		{name: "current", verifiedAt: timePointer(now.Add(-time.Hour)), want: recoveryVerificationCurrent},
		{name: "audit use is the effective verification", verifiedAt: timePointer(now.Add(-2 * time.Hour)), lastUsed: now.Add(-time.Hour), want: recoveryVerificationCurrent},
		{name: "exactly max age is expired", verifiedAt: timePointer(now.Add(-recoveryVerificationMaxAge)), want: recoveryVerificationExpired, degraded: true},
		{name: "small positive clock skew is current", verifiedAt: timePointer(now.Add(recoveryVerificationClockSkew)), want: recoveryVerificationCurrent},
		{name: "future beyond clock skew is invalid", verifiedAt: timePointer(now.Add(recoveryVerificationClockSkew + time.Second)), want: recoveryVerificationInvalidFuture, degraded: true},
		{name: "future audit evidence also fails closed", verifiedAt: timePointer(now.Add(-time.Hour)), lastUsed: now.Add(recoveryVerificationClockSkew + time.Second), want: recoveryVerificationInvalidFuture, degraded: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			descriptor := base.Clone()
			if test.verifiedAt != nil {
				verified := test.verifiedAt.UTC()
				descriptor.Slots = append(descriptor.Slots, masterkey.KeySlot{
					ID: "slot_recovery", Purpose: masterkey.KeySlotRecovery, Provider: "aws-kms", KeyReference: "recovery", WrappedKey: []byte("r"), MasterKeyFingerprint: fingerprint,
					State: masterkey.KeySlotActive, Revision: 2, VerifiedAt: &verified, CreatedAt: verified, UpdatedAt: verified,
				})
			}
			view := buildMasterKeyCustodyView(config.MasterKeyModeKeySlots, descriptor, boltstore.VaultKeyring{}, test.lastUsed, now)
			if view.RecoveryVerificationStatus != test.want || (view.CustodyState == "degraded") != test.degraded {
				t.Fatalf("status=%q custody=%q reasons=%v", view.RecoveryVerificationStatus, view.CustodyState, view.DegradedReasons)
			}
			if test.lastUsed.After(time.Time{}) && (view.RecoveryVerifiedAt == nil || !view.RecoveryVerifiedAt.Equal(test.lastUsed)) {
				t.Fatalf("effective recovery_verified_at=%v want %v", view.RecoveryVerifiedAt, test.lastUsed)
			}
		})
	}
}

func TestAdminMasterKeyCustodyFileModeContract(t *testing.T) {
	runtime := &Runtime{config: config.Config{Storage: config.Storage{MasterKey: config.MasterKey{Mode: config.MasterKeyModeFile}}}}
	response := httptest.NewRecorder()
	runtime.adminMasterKeyCustody(response, httptest.NewRequest(http.MethodGet, "/admin/api/v1/master-key/custody", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var view masterKeyCustodyView
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Mode != config.MasterKeyModeFile || !view.LocalCustodyReady || view.RecoveryVerificationStatus != recoveryVerificationNotApplicable || view.RecoveryVerifiedAt != nil || view.LifecycleRunbookURL != "" || view.RecoveryRunbookURL != "" {
		t.Fatalf("view=%#v", view)
	}
	if strings.Contains(response.Body.String(), "descriptor_ready") || strings.Contains(response.Body.String(), "recovery_verification_expired") {
		t.Fatalf("legacy contract leaked: %s", response.Body.String())
	}
}

func TestAdminMasterKeyCustodyFailsClosedWhenKeySlotMetadataIsUnavailable(t *testing.T) {
	store, err := boltstore.Open(t.TempDir() + "/metadata.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runtime := &Runtime{
		config: config.Config{Storage: config.Storage{MasterKey: config.MasterKey{Mode: config.MasterKeyModeKeySlots}}},
		store:  store,
	}
	response := httptest.NewRecorder()
	runtime.adminMasterKeyCustody(response, httptest.NewRequest(http.MethodGet, "/admin/api/v1/master-key/custody", nil))
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "metadata unavailable") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func timePointer(value time.Time) *time.Time {
	return &value
}
