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

	added, _, err := stored.AddSlot(testPendingSlot("slot_primary", masterkey.KeySlotPrimary), stored.Revision, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceKeySlotDescriptor(ctx, stored.Revision, added); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceKeySlotDescriptor(ctx, stored.Revision, added); !errors.Is(err, ErrRevisionConflict) {
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

func TestKeySlotDescriptorStoreRejectsSkippedStateAndRemovedMetadata(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Date(2026, 8, 3, 15, 0, 0, 0, time.UTC)
	descriptor := newTestKeySlotDescriptor(t)
	descriptor, _, err = descriptor.AddSlot(testPendingSlot("slot_primary", masterkey.KeySlotPrimary), descriptor.Revision, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutKeySlotDescriptor(ctx, descriptor); err != nil {
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
	if err := store.ReplaceKeySlotDescriptor(ctx, descriptor.Revision, skipped); !errors.Is(err, masterkey.ErrInvalidTransition) {
		t.Fatalf("skipped transition error=%v", err)
	}

	removed := descriptor.Clone()
	removed.Revision++
	removed.Slots = nil
	if err := store.ReplaceKeySlotDescriptor(ctx, descriptor.Revision, removed); !errors.Is(err, masterkey.ErrInvalidDescriptor) {
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
	next, _, err := descriptor.RetireSlot(old.ID, descriptor.Revision, old.Revision, now.Add(6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceKeySlotDescriptor(ctx, descriptor.Revision, next); err != nil {
		t.Fatal(err)
	}
	descriptor = next
	old = slotByIDForBoltTest(t, descriptor, primary.ID)
	next, _, err = descriptor.RevokeSlot(old.ID, descriptor.Revision, old.Revision, now.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceKeySlotDescriptor(ctx, descriptor.Revision, next); err != nil {
		t.Fatal(err)
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
		VaultKeyCheck: []byte("encrypted-vault-key-check"), AuditHMACEnvelope: []byte("encrypted-audit-key"),
		AuditCheckpoint: AuditCheckpoint{Records: 1, Bytes: 128},
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
	next, _, err := descriptor.AddSlot(pending, descriptor.Revision, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceKeySlotDescriptor(context.Background(), descriptor.Revision, next); err != nil {
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
	next, _, err := descriptor.VerifySlot(
		context.Background(), slotID, descriptor.Revision, slot.Revision,
		boltTestSlotUnwrapper{key: key}, boltTestCandidateVerifier{}, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceKeySlotDescriptor(context.Background(), descriptor.Revision, next); err != nil {
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
