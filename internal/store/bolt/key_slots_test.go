package bolt

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/masterkey"
)

type boltTestSlotUnwrapper struct {
	key []byte
}

func (u boltTestSlotUnwrapper) Unwrap(context.Context, masterkey.KeySlot) ([]byte, error) {
	return bytes.Clone(u.key), nil
}

type boltTestCandidateVerifier struct{}

func (boltTestCandidateVerifier) VerifyCandidate(context.Context, []byte) error { return nil }

type rejectingBoltTestCandidateVerifier struct{ err error }

func (v rejectingBoltTestCandidateVerifier) VerifyCandidate(context.Context, []byte) error {
	return v.err
}

func TestKeySlotDescriptorPersistenceUsesRevisionedCOW(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Date(2026, 8, 3, 13, 0, 0, 0, time.UTC)
	descriptor := newTestKeySlotDescriptor(t)
	if err := store.PutKeySlotDescriptor(ctx, descriptor); err != nil {
		t.Fatal(err)
	}
	if err := store.PutKeySlotDescriptor(ctx, descriptor); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate descriptor error=%v", err)
	}
	stored, err := store.KeySlotDescriptor(ctx)
	if err != nil || stored.Revision != 1 || len(stored.Slots) != 0 {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}

	added, _, err := store.AddKeySlot(ctx, testPendingSlot("slot_primary", masterkey.KeySlotPrimary), stored.Revision, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.replaceKeySlotDescriptor(ctx, stored.Revision, added); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale replacement error=%v", err)
	}
	stored, err = store.KeySlotDescriptor(ctx)
	if err != nil || stored.Revision != 2 || len(stored.Slots) != 1 || stored.Slots[0].State != masterkey.KeySlotPending {
		t.Fatalf("stored after add=%#v err=%v", stored, err)
	}

	// Returned values and caller inputs must not alias persistent bytes.
	stored.Slots[0].WrappedKey[0] ^= 0xff
	reloaded, err := store.KeySlotDescriptor(ctx)
	if err != nil || bytes.Equal(stored.Slots[0].WrappedKey, reloaded.Slots[0].WrappedKey) {
		t.Fatalf("persistent descriptor aliased caller memory: reloaded=%#v err=%v", reloaded, err)
	}
}

func TestKeySlotDescriptorPublicationRollsBackAtEveryKillPoint(t *testing.T) {
	for _, point := range []string{"before_put_key_slot_descriptor", "after_put_key_slot_descriptor"} {
		t.Run(point, func(t *testing.T) {
			store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			descriptor := newTestKeySlotDescriptor(t)
			if err := store.PutKeySlotDescriptor(ctx, descriptor); err != nil {
				t.Fatal(err)
			}
			next, _, err := descriptor.AddSlot(
				testPendingSlot("slot_primary", masterkey.KeySlotPrimary), descriptor.Revision,
				time.Date(2026, 8, 3, 14, 0, 0, 0, time.UTC),
			)
			if err != nil {
				t.Fatal(err)
			}
			injected := errors.New("injected publication failure")
			err = store.replaceKeySlotDescriptorWithHook(ctx, descriptor.Revision, next, func(current string) error {
				if current == point {
					return injected
				}
				return nil
			})
			if !errors.Is(err, injected) {
				t.Fatalf("kill point error=%v", err)
			}
			unchanged, err := store.KeySlotDescriptor(ctx)
			if err != nil || unchanged.Revision != descriptor.Revision || len(unchanged.Slots) != 0 {
				t.Fatalf("transaction was partially published: %#v err=%v", unchanged, err)
			}
		})
	}
}

func TestKeySlotInitializationPublishesCompleteStateAtomically(t *testing.T) {
	state := newTestKeySlotInitialization(t)
	points := []string{
		"before_put_descriptor", "after_put_descriptor",
		"before_put_keyring", "after_put_keyring",
		"before_put_vault_key_check", "after_put_vault_key_check",
		"before_put_audit_hmac_envelope", "after_put_audit_hmac_envelope",
		"before_put_audit_checkpoint", "after_put_audit_checkpoint",
		"before_put_ledger_hmac_envelope", "after_put_ledger_hmac_envelope",
	}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			injected := errors.New("injected initialization failure")
			err = store.initializeKeySlotStateWithHook(context.Background(), state, func(current string) error {
				if current == point {
					return injected
				}
				return nil
			})
			if !errors.Is(err, injected) {
				t.Fatalf("kill point error=%v", err)
			}
			for name, load := range map[string]func() error{
				"descriptor":       func() error { _, err := store.KeySlotDescriptor(context.Background()); return err },
				"keyring":          func() error { _, err := store.VaultKeyring(); return err },
				"vault key check":  func() error { _, err := store.VaultKeyCheck(); return err },
				"audit envelope":   func() error { _, err := store.AuditHMACEnvelope(); return err },
				"audit checkpoint": func() error { _, err := store.AuditCheckpoint(); return err },
				"ledger envelope":  func() error { _, err := store.LedgerHMACEnvelope(); return err },
			} {
				if err := load(); !errors.Is(err, ErrNotFound) {
					t.Fatalf("%s was partially published: %v", name, err)
				}
			}
		})
	}

	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InitializeKeySlotState(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	descriptor, err := store.KeySlotDescriptor(context.Background())
	if err != nil || !descriptor.ProductionReady() {
		t.Fatalf("descriptor=%#v err=%v", descriptor, err)
	}
	if err := store.InitializeKeySlotState(context.Background(), state); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate initialization error=%v", err)
	}
}

func TestKeySlotInitializationRejectsUnverifiedDescriptor(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	state := newTestKeySlotInitialization(t)
	rejected := errors.New("Vault Key Check rejected candidate")
	state.Verifier = rejectingBoltTestCandidateVerifier{err: rejected}
	if err := store.InitializeKeySlotState(context.Background(), state); !errors.Is(err, rejected) {
		t.Fatalf("forged initialization error=%v", err)
	}
	if _, err := store.KeySlotDescriptor(context.Background()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected descriptor was persisted: %v", err)
	}
}

func TestKeySlotDescriptorStoreRejectsSkippedStateAndRemovedMetadata(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Date(2026, 8, 3, 15, 0, 0, 0, time.UTC)
	descriptor := newTestKeySlotDescriptor(t)
	if err := store.PutKeySlotDescriptor(ctx, descriptor); err != nil {
		t.Fatal(err)
	}
	descriptor, _, err = store.AddKeySlot(ctx, testPendingSlot("slot_primary", masterkey.KeySlotPrimary), descriptor.Revision, now)
	if err != nil {
		t.Fatal(err)
	}

	skipped := descriptor.Clone()
	skipped.Revision++
	skipped.Slots[0].State = masterkey.KeySlotRetiring
	skipped.Slots[0].Revision++
	skipped.Slots[0].UpdatedAt = now.Add(time.Minute)
	skipped.Slots[0].VerifiedAt = timePointerForBoltTest(now.Add(time.Minute))
	if err := skipped.Validate(); err != nil {
		t.Fatalf("test successor must be structurally valid: %v", err)
	}
	if err := store.replaceKeySlotDescriptor(ctx, descriptor.Revision, skipped); !errors.Is(err, masterkey.ErrInvalidTransition) {
		t.Fatalf("skipped transition error=%v", err)
	}

	removed := descriptor.Clone()
	removed.Revision++
	removed.Slots = nil
	if err := store.replaceKeySlotDescriptor(ctx, descriptor.Revision, removed); !errors.Is(err, masterkey.ErrInvalidDescriptor) {
		t.Fatalf("removed slot metadata error=%v", err)
	}
	unchanged, err := store.KeySlotDescriptor(ctx)
	if err != nil || unchanged.Revision != descriptor.Revision || len(unchanged.Slots) != 1 {
		t.Fatalf("rejected successor changed storage: %#v err=%v", unchanged, err)
	}
}

func TestKeySlotDescriptorOperationsHonorCancellation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.PutKeySlotDescriptor(ctx, newTestKeySlotDescriptor(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled put error=%v", err)
	}
	if _, err := store.KeySlotDescriptor(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled get error=%v", err)
	}
}

func TestKeySlotCompactionExcludesRevokedProviderMaterial(t *testing.T) {
	livePath := filepath.Join(t.TempDir(), "metadata.db")
	store, err := Open(livePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Date(2026, 8, 3, 17, 0, 0, 0, time.UTC)
	key := bytes.Repeat([]byte{9}, 32)
	descriptor := newTestKeySlotDescriptor(t)
	if err := store.PutKeySlotDescriptor(ctx, descriptor); err != nil {
		t.Fatal(err)
	}

	secretReference := strings.Repeat("retired-secret-reference-", 80)
	primary := testPendingSlot("slot_primary_old", masterkey.KeySlotPrimary)
	primary.KeyReference = secretReference
	primary.WrappedKey = bytes.Repeat([]byte("retired-wrapped-material-"), 700)
	descriptor = persistAddedBoltTestSlot(t, store, descriptor, primary, now)
	descriptor = persistVerifiedBoltTestSlot(t, store, descriptor, primary.ID, key, now.Add(time.Minute))
	descriptor = persistAddedBoltTestSlot(t, store, descriptor, testPendingSlot("slot_recovery", masterkey.KeySlotRecovery), now.Add(2*time.Minute))
	descriptor = persistVerifiedBoltTestSlot(t, store, descriptor, "slot_recovery", key, now.Add(3*time.Minute))
	descriptor = persistAddedBoltTestSlot(t, store, descriptor, testPendingSlot("slot_primary_new", masterkey.KeySlotPrimary), now.Add(4*time.Minute))
	descriptor = persistVerifiedBoltTestSlot(t, store, descriptor, "slot_primary_new", key, now.Add(5*time.Minute))

	old := slotByIDForBoltTest(t, descriptor, primary.ID)
	next, _, err := store.RetireKeySlot(ctx, old.ID, descriptor.Revision, old.Revision, now.Add(6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	descriptor = next
	old = slotByIDForBoltTest(t, descriptor, primary.ID)
	next, intent, err := store.RevokeKeySlotWithAuditIntent(ctx, old.ID, descriptor.Revision, old.Revision, "retirement_window_completed", now.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if intent.Delivered || intent.TargetID != old.ID || intent.ExpectedDescriptorRevision != descriptor.Revision || intent.ExpectedSlotRevision != old.Revision {
		t.Fatalf("intent=%#v", intent)
	}
	storedIntent, err := store.KeySlotAuditIntent()
	if err != nil || storedIntent != intent {
		t.Fatalf("stored intent=%#v err=%v", storedIntent, err)
	}
	liveBytes, err := os.ReadFile(livePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(liveBytes, []byte(secretReference)) {
		t.Fatal("test fixture did not leave retired material in a free database page")
	}
	compactPath := filepath.Join(t.TempDir(), "metadata.compact.db")
	if err := store.CompactSnapshot(compactPath); err != nil {
		t.Fatal(err)
	}
	compactBytes, err := os.ReadFile(compactPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(compactBytes, []byte(secretReference)) {
		t.Fatal("compact snapshot retained revoked provider material")
	}
	compact, err := Open(compactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer compact.Close()
	stored, err := compact.KeySlotDescriptor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	revoked := slotByIDForBoltTest(t, stored, primary.ID)
	if revoked.State != masterkey.KeySlotRevoked || revoked.KeyReference != "" || len(revoked.WrappedKey) != 0 {
		t.Fatalf("unexpected compacted revoked slot: %#v", revoked)
	}
	if err := store.MarkKeySlotAuditDelivered(ctx, intent.EventID); err != nil {
		t.Fatal(err)
	}
	storedIntent, err = store.KeySlotAuditIntent()
	if err != nil || !storedIntent.Delivered {
		t.Fatalf("delivered intent=%#v err=%v", storedIntent, err)
	}
}

func TestMasterKeyRotationAuditIntentIsAtomicWithBridgeCleanup(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	fingerprint, err := masterkey.MasterKeyFingerprint(bytes.Repeat([]byte{0x41}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutVaultKeyring(VaultKeyring{
		FormatVersion: 1, ActiveKeyVersion: 2, ActiveFingerprint: fingerprint,
		PreviousFingerprint: fingerprint, RecoveryEnvelope: []byte("rotation-bridge"), RotationOperationID: "rotation-001",
	}); err != nil {
		t.Fatal(err)
	}
	intent, err := store.EnsureMasterKeyRotationAuditIntent(ctx, "rotation-001", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClearVaultRotationBridgeWithAuditIntent(ctx, "rotation-001", now.Add(time.Minute)); err == nil {
		t.Fatal("bridge cleanup succeeded before started Audit delivery")
	}
	if _, err := store.VaultRotationBridge(); err != nil {
		t.Fatalf("rejected completion removed bridge: %v", err)
	}
	if err := store.MarkMasterKeyRotationAuditDelivered(ctx, intent.StartedEventID); err != nil {
		t.Fatal(err)
	}
	completed, err := store.ClearVaultRotationBridgeWithAuditIntent(ctx, "rotation-001", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if completed.CompletedAt == nil || completed.CompletedDelivered {
		t.Fatalf("completed=%#v", completed)
	}
	if _, err := store.VaultRotationBridge(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bridge survived completion intent transaction: %v", err)
	}
	persisted, err := store.MasterKeyRotationAuditIntent()
	if err != nil || persisted.CompletedEventID != completed.CompletedEventID || persisted.CompletedAt == nil || !persisted.CompletedAt.Equal(*completed.CompletedAt) {
		t.Fatalf("persisted=%#v err=%v", persisted, err)
	}
	if err := store.MarkMasterKeyRotationAuditDelivered(ctx, completed.CompletedEventID); err != nil {
		t.Fatal(err)
	}
	delivered, err := store.MasterKeyRotationAuditIntent()
	if err != nil || !delivered.StartedDelivered || !delivered.CompletedDelivered {
		t.Fatalf("delivered=%#v err=%v", delivered, err)
	}
}

func TestVaultRewritePublishesRotatedDescriptorWithVaultGeneration(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	state := newTestKeySlotInitialization(t)
	if err := store.InitializeKeySlotState(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	newKey := bytes.Repeat([]byte{0x44}, 32)
	newFingerprint, err := masterkey.MasterKeyFingerprint(newKey)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := masterkey.NewRotatedKeySlotDescriptor(state.Descriptor, newFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	rotated = addAndVerifyBoltRotationSlot(t, rotated, newKey, "slot_primary", masterkey.KeySlotPrimary)
	rotated = addAndVerifyBoltRotationSlot(t, rotated, newKey, "slot_recovery", masterkey.KeySlotRecovery)
	if err := store.RewriteVaultMaterial(VaultRewrite{
		VaultKeyCheck: []byte("new-check"), AuditHMACEnvelope: []byte("new-audit"), LedgerHMACEnvelope: []byte("new-ledger"),
		Keyring: VaultKeyring{FormatVersion: 1, ActiveKeyVersion: 2, ActiveFingerprint: newFingerprint,
			PreviousFingerprint: state.Descriptor.MasterKeyFingerprint, RecoveryEnvelope: []byte("bridge")},
		KeySlotDescriptor: &rotated,
		Transform:         func(value domain.Credential) (domain.Credential, error) { return value, nil },
		TransformAdminMFA: func(value domain.AdminMFAAuthenticator) (domain.AdminMFAAuthenticator, error) { return value, nil },
	}); err == nil {
		t.Fatal("rotated descriptor published without unwrap and candidate verification")
	}
	unchanged, err := store.KeySlotDescriptor(context.Background())
	if err != nil || unchanged.ActiveGeneration != state.Descriptor.ActiveGeneration {
		t.Fatalf("rejected rotation changed descriptor: %#v err=%v", unchanged, err)
	}
	if err := store.RewriteVaultMaterial(VaultRewrite{
		Context:       context.Background(),
		VaultKeyCheck: []byte("new-check"), AuditHMACEnvelope: []byte("new-audit"), LedgerHMACEnvelope: []byte("new-ledger"),
		Keyring: VaultKeyring{FormatVersion: 1, ActiveKeyVersion: 2, ActiveFingerprint: newFingerprint,
			PreviousFingerprint: state.Descriptor.MasterKeyFingerprint, RecoveryEnvelope: []byte("bridge")},
		KeySlotDescriptor: &rotated,
		Unwrapper:         boltTestSlotUnwrapper{key: newKey}, Verifier: boltTestCandidateVerifier{},
		Transform:         func(value domain.Credential) (domain.Credential, error) { return value, nil },
		TransformAdminMFA: func(value domain.AdminMFAAuthenticator) (domain.AdminMFAAuthenticator, error) { return value, nil },
	}); err != nil {
		t.Fatal(err)
	}
	persisted, err := store.KeySlotDescriptor(context.Background())
	if err != nil || persisted.ActiveGeneration != state.Descriptor.ActiveGeneration+1 ||
		persisted.MasterKeyFingerprint != newFingerprint || !persisted.ProductionReady() {
		t.Fatalf("persisted=%#v err=%v", persisted, err)
	}
	keyring, err := store.VaultKeyring()
	if err != nil || keyring.ActiveFingerprint != persisted.MasterKeyFingerprint {
		t.Fatalf("keyring=%#v err=%v", keyring, err)
	}
}

func addAndVerifyBoltRotationSlot(t *testing.T, descriptor masterkey.KeySlotDescriptor, key []byte, id string, purpose masterkey.KeySlotPurpose) masterkey.KeySlotDescriptor {
	t.Helper()
	now := time.Now().UTC().Add(time.Duration(descriptor.Revision) * time.Second)
	pending := testPendingSlot(id, purpose)
	next, _, err := descriptor.AddSlot(pending, descriptor.Revision, now)
	if err != nil {
		t.Fatal(err)
	}
	slot := slotByIDForBoltTest(t, next, id)
	next, _, err = next.VerifySlot(context.Background(), id, next.Revision, slot.Revision,
		boltTestSlotUnwrapper{key: bytes.Clone(key)}, boltTestCandidateVerifier{}, now.Add(time.Nanosecond))
	if err != nil {
		t.Fatal(err)
	}
	return next
}

func newTestKeySlotDescriptor(t *testing.T) masterkey.KeySlotDescriptor {
	t.Helper()
	fingerprint, err := masterkey.MasterKeyFingerprint(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := masterkey.NewKeySlotDescriptor(fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func newTestKeySlotInitialization(t *testing.T) KeySlotInitialization {
	t.Helper()
	key := bytes.Repeat([]byte{9}, 32)
	descriptor := newTestKeySlotDescriptor(t)
	now := time.Date(2026, 8, 3, 18, 0, 0, 0, time.UTC)
	for index, pending := range []masterkey.PendingKeySlot{
		testPendingSlot("slot_primary", masterkey.KeySlotPrimary),
		testPendingSlot("slot_recovery", masterkey.KeySlotRecovery),
	} {
		var err error
		descriptor, _, err = descriptor.AddSlot(pending, descriptor.Revision, now.Add(time.Duration(index*2)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		slot := slotByIDForBoltTest(t, descriptor, pending.ID)
		descriptor, _, err = descriptor.VerifySlot(
			context.Background(), pending.ID, descriptor.Revision, slot.Revision,
			boltTestSlotUnwrapper{key: key}, boltTestCandidateVerifier{}, now.Add(time.Duration(index*2+1)*time.Minute),
		)
		if err != nil {
			t.Fatal(err)
		}
	}
	return KeySlotInitialization{
		Descriptor: descriptor,
		Keyring: VaultKeyring{
			FormatVersion: 1, ActiveKeyVersion: 1, ActiveFingerprint: descriptor.MasterKeyFingerprint,
		},
		VaultKeyCheck: []byte("encrypted-vault-key-check"), AuditHMACEnvelope: []byte("encrypted-audit-key"), LedgerHMACEnvelope: []byte("encrypted-ledger-key"),
		AuditCheckpoint: AuditCheckpoint{Records: 1, Bytes: 128},
		Unwrapper:       boltTestSlotUnwrapper{key: key}, Verifier: boltTestCandidateVerifier{},
	}
}

func testPendingSlot(id string, purpose masterkey.KeySlotPurpose) masterkey.PendingKeySlot {
	return masterkey.PendingKeySlot{
		ID: id, Provider: "fake-kms", Purpose: purpose, KeyReference: "opaque-key-reference",
		Algorithm: "fake-v1", ProviderParameters: map[string]string{"scope": "test"}, WrappedKey: []byte("wrapped-key"),
	}
}

func timePointerForBoltTest(value time.Time) *time.Time {
	return &value
}

func persistAddedBoltTestSlot(
	t *testing.T,
	store *Store,
	descriptor masterkey.KeySlotDescriptor,
	pending masterkey.PendingKeySlot,
	now time.Time,
) masterkey.KeySlotDescriptor {
	t.Helper()
	next, _, err := store.AddKeySlot(context.Background(), pending, descriptor.Revision, now)
	if err != nil {
		t.Fatal(err)
	}
	return next
}

func persistVerifiedBoltTestSlot(
	t *testing.T,
	store *Store,
	descriptor masterkey.KeySlotDescriptor,
	slotID string,
	key []byte,
	now time.Time,
) masterkey.KeySlotDescriptor {
	t.Helper()
	slot := slotByIDForBoltTest(t, descriptor, slotID)
	next, _, err := store.VerifyKeySlot(
		context.Background(), slotID, descriptor.Revision, slot.Revision,
		boltTestSlotUnwrapper{key: key}, boltTestCandidateVerifier{}, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	return next
}

func slotByIDForBoltTest(t *testing.T, descriptor masterkey.KeySlotDescriptor, slotID string) masterkey.KeySlot {
	t.Helper()
	for _, slot := range descriptor.Slots {
		if slot.ID == slotID {
			return slot
		}
	}
	t.Fatalf("slot %q not found", slotID)
	return masterkey.KeySlot{}
}
