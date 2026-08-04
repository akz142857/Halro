package masterkey

import (
	"crypto/sha256"
	"encoding/hex"
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
	validTransition := false
	switch i.Action {
	case "security.master_key_slot.added":
		validTransition = i.ExpectedSlotRevision == 0 && i.SlotRevision == 1 && i.ReasonCode == ""
	case "security.master_key_slot.verified", "security.master_key_slot.retiring":
		validTransition = i.ExpectedSlotRevision > 0 && i.SlotRevision == i.ExpectedSlotRevision+1 && i.ReasonCode == ""
	case "security.master_key_slot.revoked":
		validTransition = i.ExpectedSlotRevision > 0 && i.SlotRevision == i.ExpectedSlotRevision+1 &&
			(i.ReasonCode == "retirement_window_completed" || i.ReasonCode == "incident_retirement")
	}
	if i.EventID == "" || i.OccurredAt.IsZero() || i.OccurredAt.Location() != time.UTC ||
		!validTransition || i.TargetID == "" ||
		(i.Purpose != KeySlotPrimary && i.Purpose != KeySlotRecovery) ||
		i.ExpectedDescriptorRevision == 0 || i.DescriptorRevision != i.ExpectedDescriptorRevision+1 {
		return errors.New("invalid Key Slot audit intent")
	}
	return nil
}

// MasterKeyRotationAuditIntent durably binds the two logical Audit events for
// one KMS DEK rotation. The operation ID is explicitly non-secret and is also
// used to reject retries for a different logical operation.
type MasterKeyRotationAuditIntent struct {
	OperationID        string     `json:"operation_id"`
	StartedEventID     string     `json:"started_event_id"`
	StartedAt          time.Time  `json:"started_at"`
	StartedDelivered   bool       `json:"started_delivered"`
	CompletedEventID   string     `json:"completed_event_id,omitempty"`
	CompletedAt        *time.Time `json:"completed_at,omitempty"`
	CompletedDelivered bool       `json:"completed_delivered"`
}

func NewMasterKeyRotationAuditIntent(operationID string, now time.Time) (MasterKeyRotationAuditIntent, error) {
	intent := MasterKeyRotationAuditIntent{
		OperationID:    operationID,
		StartedEventID: rotationAuditEventID(operationID, "started"),
		StartedAt:      now.UTC(),
	}
	return intent, intent.Validate()
}

func (i MasterKeyRotationAuditIntent) Validate() error {
	if !validRotationOperationID(i.OperationID) || i.StartedEventID != rotationAuditEventID(i.OperationID, "started") ||
		i.StartedAt.IsZero() || i.StartedAt.Location() != time.UTC {
		return errors.New("invalid Master Key rotation audit intent")
	}
	if i.CompletedAt == nil {
		if i.CompletedEventID != "" || i.CompletedDelivered {
			return errors.New("invalid Master Key rotation completion intent")
		}
		return nil
	}
	if i.CompletedAt.IsZero() || i.CompletedAt.Location() != time.UTC || i.CompletedAt.Before(i.StartedAt) ||
		i.CompletedEventID != rotationAuditEventID(i.OperationID, "completed") || !i.StartedDelivered {
		return errors.New("invalid Master Key rotation completion intent")
	}
	return nil
}

func (i MasterKeyRotationAuditIntent) WithCompletion(now time.Time) (MasterKeyRotationAuditIntent, error) {
	if err := i.Validate(); err != nil {
		return MasterKeyRotationAuditIntent{}, err
	}
	if !i.StartedDelivered {
		return MasterKeyRotationAuditIntent{}, errors.New("Master Key rotation start Audit is not delivered")
	}
	completed := now.UTC()
	i.CompletedAt = &completed
	i.CompletedEventID = rotationAuditEventID(i.OperationID, "completed")
	i.CompletedDelivered = false
	return i, i.Validate()
}

func rotationAuditEventID(operationID, phase string) string {
	digest := sha256.Sum256([]byte(operationID + "\x00" + phase))
	return "aud-kms-rotation-" + hex.EncodeToString(digest[:])
}

func validRotationOperationID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '.' && character != '_' && character != '-' {
			return false
		}
	}
	return true
}
