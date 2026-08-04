package masterkey

import (
	"testing"
	"time"
)

func TestMasterKeyRotationAuditIntentBindsOperationAndPhases(t *testing.T) {
	now := time.Now().UTC()
	intent, err := NewMasterKeyRotationAuditIntent("incident-rotation-001", now)
	if err != nil {
		t.Fatal(err)
	}
	again, err := NewMasterKeyRotationAuditIntent("incident-rotation-001", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if intent.StartedEventID != again.StartedEventID {
		t.Fatal("rotation start event ID is not deterministic")
	}
	if _, err := intent.WithCompletion(now.Add(time.Minute)); err == nil {
		t.Fatal("completion accepted before start Audit delivery")
	}
	intent.StartedDelivered = true
	completed, err := intent.WithCompletion(now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if completed.CompletedEventID == "" || completed.CompletedEventID == completed.StartedEventID || completed.CompletedAt == nil {
		t.Fatalf("completed=%#v", completed)
	}
	conflict := completed
	conflict.OperationID = "different-operation"
	if err := conflict.Validate(); err == nil {
		t.Fatal("intent accepted an event ID from a different operation")
	}
}

func TestKeySlotAuditIntentValidatesEveryLifecycleTransition(t *testing.T) {
	now := time.Now().UTC()
	for _, test := range []struct {
		action       string
		expectedSlot uint64
		slot         uint64
		reason       string
	}{
		{"security.master_key_slot.added", 0, 1, ""},
		{"security.master_key_slot.verified", 1, 2, ""},
		{"security.master_key_slot.retiring", 2, 3, ""},
		{"security.master_key_slot.revoked", 3, 4, "retirement_window_completed"},
	} {
		intent := KeySlotAuditIntent{
			EventID: "aud-slot", OccurredAt: now, Action: test.action, TargetID: "slot_primary", Purpose: KeySlotPrimary,
			ReasonCode: test.reason, ExpectedDescriptorRevision: 4, ExpectedSlotRevision: test.expectedSlot,
			DescriptorRevision: 5, SlotRevision: test.slot,
		}
		if err := intent.Validate(); err != nil {
			t.Fatalf("action=%s err=%v", test.action, err)
		}
		intent.ReasonCode = "unexpected"
		if err := intent.Validate(); err == nil {
			t.Fatalf("action=%s accepted conflicting reason", test.action)
		}
	}
}
