package masterkey

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type fakeSlotUnwrapper struct {
	key   []byte
	err   error
	calls int
}

func (f *fakeSlotUnwrapper) Unwrap(ctx context.Context, _ KeySlot) ([]byte, error) {
	f.calls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.key, nil
}

type fakeCandidateVerifier struct {
	err   error
	calls int
}

func (f *fakeCandidateVerifier) VerifyCandidate(ctx context.Context, candidate []byte) error {
	f.calls++
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(candidate) != 32 {
		return errors.New("unexpected candidate size")
	}
	return f.err
}

func TestKeySlotLifecycleAndAuditEvidence(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	key := bytes.Repeat([]byte{7}, 32)
	fingerprint, err := MasterKeyFingerprint(key)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := NewKeySlotDescriptor(fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	primary := pendingSlot("slot_primary", KeySlotPrimary, []byte("wrapped-primary"))
	descriptor, added, err := descriptor.AddSlot(primary, 1, now)
	if err != nil {
		t.Fatal(err)
	}
	assertTransition(t, added, "", KeySlotPending, "security.master_key_slot.added")
	if descriptor.Revision != 2 || descriptor.Slots[0].Revision != 1 {
		t.Fatalf("add revisions descriptor=%d slot=%d", descriptor.Revision, descriptor.Slots[0].Revision)
	}
	idempotent, transition, err := descriptor.AddSlot(primary, 1, now.Add(time.Second))
	if err != nil || transition != nil || idempotent.Revision != descriptor.Revision {
		t.Fatalf("idempotent add descriptor=%#v transition=%#v err=%v", idempotent, transition, err)
	}
	unwrapper := &fakeSlotUnwrapper{key: bytes.Clone(key)}
	verifier := &fakeCandidateVerifier{}
	descriptor, verified, err := descriptor.VerifySlot(context.Background(), primary.ID, 2, 1, unwrapper, verifier, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	assertTransition(t, verified, KeySlotPending, KeySlotActive, "security.master_key_slot.verified")
	if unwrapper.calls != 1 || verifier.calls != 1 || !allZero(unwrapper.key) {
		t.Fatalf("unwrap calls=%d verify calls=%d candidate cleared=%v", unwrapper.calls, verifier.calls, allZero(unwrapper.key))
	}
	if descriptor.ProductionReady() {
		t.Fatal("primary-only descriptor was production ready")
	}

	recovery := pendingSlot("slot_recovery", KeySlotRecovery, []byte("wrapped-recovery"))
	descriptor, _, err = descriptor.AddSlot(recovery, descriptor.Revision, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	descriptor, _, err = descriptor.VerifySlot(
		context.Background(), recovery.ID, descriptor.Revision, 1,
		&fakeSlotUnwrapper{key: bytes.Clone(key)}, &fakeCandidateVerifier{}, now.Add(3*time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !descriptor.ProductionReady() {
		t.Fatal("verified primary/recovery descriptor was not production ready")
	}

	replacement := pendingSlot("slot_primary_next", KeySlotPrimary, []byte("wrapped-primary-next"))
	descriptor, _, err = descriptor.AddSlot(replacement, descriptor.Revision, now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	descriptor, _, err = descriptor.VerifySlot(
		context.Background(), replacement.ID, descriptor.Revision, 1,
		&fakeSlotUnwrapper{key: bytes.Clone(key)}, &fakeCandidateVerifier{}, now.Add(5*time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	oldPrimary, _ := descriptor.slot(primary.ID)
	descriptor, retiring, err := descriptor.RetireSlot(primary.ID, descriptor.Revision, oldPrimary.Revision, now.Add(6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	assertTransition(t, retiring, KeySlotActive, KeySlotRetiring, "security.master_key_slot.retiring")
	oldPrimary, _ = descriptor.slot(primary.ID)
	descriptor, revoked, err := descriptor.RevokeSlot(primary.ID, descriptor.Revision, oldPrimary.Revision, now.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	assertTransition(t, revoked, KeySlotRetiring, KeySlotRevoked, "security.master_key_slot.revoked")
	revokedSlot, _ := descriptor.slot(primary.ID)
	if len(revokedSlot.WrappedKey) != 0 || revokedSlot.KeyReference != "" || len(revokedSlot.ProviderParameters) != 0 {
		t.Fatalf("revoked slot retained protected material: %#v", revokedSlot)
	}
	if !descriptor.ProductionReady() {
		t.Fatal("replacement primary and recovery should remain production ready")
	}
	idempotent, transition, err = descriptor.RevokeSlot(primary.ID, 1, 1, now.Add(8*time.Minute))
	if err != nil || transition != nil || idempotent.Revision != descriptor.Revision {
		t.Fatalf("idempotent revoke descriptor=%#v transition=%#v err=%v", idempotent, transition, err)
	}
}

func TestKeySlotVerificationFailsClosed(t *testing.T) {
	now := time.Date(2026, 8, 3, 11, 0, 0, 0, time.UTC)
	trustedKey := bytes.Repeat([]byte{1}, 32)
	descriptor := descriptorWithPendingSlot(t, trustedKey, now)
	tests := []struct {
		name         string
		unwrapKey    []byte
		unwrapErr    error
		verifyErr    error
		wantVerifier int
	}{
		{name: "unwrap failure", unwrapErr: errors.New("fake KMS unavailable")},
		{name: "fingerprint mismatch", unwrapKey: bytes.Repeat([]byte{2}, 32)},
		{name: "vault key check mismatch", unwrapKey: bytes.Clone(trustedKey), verifyErr: errors.New("wrong vault"), wantVerifier: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			unwrapper := &fakeSlotUnwrapper{key: test.unwrapKey, err: test.unwrapErr}
			verifier := &fakeCandidateVerifier{err: test.verifyErr}
			next, transition, err := descriptor.VerifySlot(
				context.Background(), "slot_primary", descriptor.Revision, 1,
				unwrapper, verifier, now.Add(time.Minute),
			)
			if err == nil || transition != nil || next.Revision != 0 {
				t.Fatalf("verification did not fail closed: next=%#v transition=%#v err=%v", next, transition, err)
			}
			if verifier.calls != test.wantVerifier {
				t.Fatalf("verifier calls=%d want=%d", verifier.calls, test.wantVerifier)
			}
			if test.unwrapKey != nil && !allZero(test.unwrapKey) {
				t.Fatal("candidate key was not cleared")
			}
			original, _ := descriptor.slot("slot_primary")
			if original.State != KeySlotPending || original.Revision != 1 {
				t.Fatalf("original descriptor mutated: %#v", original)
			}
		})
	}
}

func TestKeySlotTransitionsEnforceRevisionsAndRecoveryInvariants(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	key := bytes.Repeat([]byte{3}, 32)
	descriptor := descriptorWithPendingSlot(t, key, now)
	if _, _, err := descriptor.VerifySlot(context.Background(), "slot_primary", 99, 1, &fakeSlotUnwrapper{key: bytes.Clone(key)}, &fakeCandidateVerifier{}, now.Add(time.Minute)); !errors.Is(err, ErrDescriptorRevision) {
		t.Fatalf("expected descriptor conflict, got %v", err)
	}
	if _, _, err := descriptor.VerifySlot(context.Background(), "slot_primary", descriptor.Revision, 99, &fakeSlotUnwrapper{key: bytes.Clone(key)}, &fakeCandidateVerifier{}, now.Add(time.Minute)); !errors.Is(err, ErrSlotRevision) {
		t.Fatalf("expected slot conflict, got %v", err)
	}
	active, _, err := descriptor.VerifySlot(context.Background(), "slot_primary", descriptor.Revision, 1, &fakeSlotUnwrapper{key: bytes.Clone(key)}, &fakeCandidateVerifier{}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	primary, _ := active.slot("slot_primary")
	if _, _, err := active.RetireSlot(primary.ID, active.Revision, primary.Revision, now.Add(2*time.Minute)); !errors.Is(err, ErrLastUsableSlot) {
		t.Fatalf("last active slot retirement error=%v", err)
	}
	if _, _, err := active.RevokeSlot(primary.ID, active.Revision, primary.Revision, now.Add(2*time.Minute)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("active-to-revoked transition error=%v", err)
	}

	// ValidateSuccessor is also a trust boundary for persistence callers. A
	// caller must not bypass the state-machine guard by constructing COW data.
	fabricated := active.Clone()
	fabricated.Revision++
	fabricated.Slots[0].State = KeySlotRetiring
	fabricated.Slots[0].Revision++
	fabricated.Slots[0].UpdatedAt = now.Add(2 * time.Minute)
	if err := fabricated.Validate(); err != nil {
		t.Fatalf("fabricated successor must be structurally valid: %v", err)
	}
	if err := fabricated.ValidateSuccessor(active); !errors.Is(err, ErrLastUsableSlot) {
		t.Fatalf("fabricated last-slot retirement error=%v", err)
	}
}

func TestSlotTransitionAuditEvidenceExcludesProtectedMaterial(t *testing.T) {
	tests := []struct {
		from   KeySlotState
		to     KeySlotState
		action string
	}{
		{to: KeySlotPending, action: "security.master_key_slot.added"},
		{from: KeySlotPending, to: KeySlotActive, action: "security.master_key_slot.verified"},
		{from: KeySlotActive, to: KeySlotRetiring, action: "security.master_key_slot.retiring"},
		{from: KeySlotRetiring, to: KeySlotRevoked, action: "security.master_key_slot.revoked"},
	}
	for _, test := range tests {
		transition := SlotTransition{
			SlotID: "slot_primary", Purpose: KeySlotPrimary, From: test.from, To: test.to,
			SlotRevision: 1, DescriptorRevision: 2, OccurredAt: time.Now().UTC(),
		}
		if transition.AuditAction() != test.action {
			t.Fatalf("state %q audit action=%q want=%q", test.to, transition.AuditAction(), test.action)
		}
		encoded, err := json.Marshal(transition)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range [][]byte{[]byte("wrapped_key"), []byte("key_reference"), []byte("fingerprint"), []byte("provider_parameters")} {
			if bytes.Contains(encoded, forbidden) {
				t.Fatalf("audit transition contains forbidden field %q: %s", forbidden, encoded)
			}
		}
	}
}

func descriptorWithPendingSlot(t *testing.T, key []byte, now time.Time) KeySlotDescriptor {
	t.Helper()
	fingerprint, err := MasterKeyFingerprint(key)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := NewKeySlotDescriptor(fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, _, err = descriptor.AddSlot(pendingSlot("slot_primary", KeySlotPrimary, []byte("wrapped")), descriptor.Revision, now)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func pendingSlot(id string, purpose KeySlotPurpose, wrapped []byte) PendingKeySlot {
	return PendingKeySlot{
		ID: id, Provider: "fake-kms", Purpose: purpose, KeyReference: "opaque-key-reference",
		Algorithm: "fake-v1", ProviderParameters: map[string]string{"scope": "test"}, WrappedKey: wrapped,
	}
}

func assertTransition(t *testing.T, transition *SlotTransition, from, to KeySlotState, action string) {
	t.Helper()
	if transition == nil {
		t.Fatal("transition is nil")
	}
	if transition.From != from || transition.To != to || transition.AuditAction() != action {
		t.Fatalf("unexpected transition: %#v action=%q", transition, transition.AuditAction())
	}
}

func allZero(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}
