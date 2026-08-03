package masterkey

import (
	"errors"
	"time"
)

// KeySlotAuditIntent is durable metadata for the one Slot transition whose
// final Audit record may still need delivery. It contains no provider material.
type KeySlotAuditIntent struct {
	EventID                    string         `json:"event_id"`
	OccurredAt                 time.Time      `json:"occurred_at"`
	Action                     string         `json:"action"`
	TargetID                   string         `json:"target_id"`
	Purpose                    KeySlotPurpose `json:"purpose"`
	ReasonCode                 string         `json:"reason_code"`
	ExpectedDescriptorRevision uint64         `json:"expected_descriptor_revision"`
	ExpectedSlotRevision       uint64         `json:"expected_slot_revision"`
	DescriptorRevision         uint64         `json:"descriptor_revision"`
	SlotRevision               uint64         `json:"slot_revision"`
	Delivered                  bool           `json:"delivered"`
}

func (i KeySlotAuditIntent) Validate() error {
	if i.EventID == "" || i.OccurredAt.IsZero() || i.OccurredAt.Location() != time.UTC ||
		i.Action != "security.master_key_slot.revoked" || i.TargetID == "" ||
		(i.Purpose != KeySlotPrimary && i.Purpose != KeySlotRecovery) ||
		(i.ReasonCode != "retirement_window_completed" && i.ReasonCode != "incident_retirement") ||
		i.ExpectedDescriptorRevision == 0 || i.ExpectedSlotRevision == 0 ||
		i.DescriptorRevision != i.ExpectedDescriptorRevision+1 || i.SlotRevision != i.ExpectedSlotRevision+1 {
		return errors.New("invalid Key Slot audit intent")
	}
	return nil
}
