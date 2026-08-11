package domain

import (
	"errors"
	"strings"
	"time"
)

// AdminAuditIntent is an admin mutation's audit record, durable in the same
// transaction as the mutation it describes.
//
// It exists because appending to the audit log after the store commit makes the
// two outcomes independent: a mutation could be durable while its audit append
// failed, and the API could only report the second failure, leaving no record
// that the first happened. Writing the intent with the mutation makes the store
// commit the single commit point — the audit record is then guaranteed to exist
// and merely not yet delivered, which a drain can finish.
//
// Metadata is map[string]string rather than map[string]any because this value
// survives a JSON round trip through bbolt: `any` would reload a version number
// as float64 and silently change what the audit log records.
//
// There is no delivered flag: a delivered intent is removed. Marking one
// instead would leave a row that says nothing the audit log does not already
// say, and it protects nothing — a crash between the durable append and the
// bookkeeping write redelivers either way, so the only difference is whether
// the bucket grows without bound.
type AdminAuditIntent struct {
	EventID       string            `json:"event_id"`
	OccurredAt    time.Time         `json:"occurred_at"`
	ActorType     string            `json:"actor_type"`
	ActorID       string            `json:"actor_id"`
	Action        string            `json:"action"`
	TargetType    string            `json:"target_type"`
	TargetID      string            `json:"target_id"`
	CorrelationID string            `json:"correlation_id,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

// Validate rejects an intent that cannot become a well-formed audit event.
//
// It runs before the intent is made durable and again before delivery. An
// intent that fails here would otherwise become an audit record the integrity
// contract cannot anchor, or a pending row no drain can ever clear.
func (i AdminAuditIntent) Validate() error {
	if strings.TrimSpace(i.EventID) == "" {
		return errors.New("admin audit intent requires an event id")
	}
	if i.OccurredAt.IsZero() {
		return errors.New("admin audit intent requires an occurrence time")
	}
	if strings.TrimSpace(i.ActorType) == "" {
		return errors.New("admin audit intent requires an actor type")
	}
	if strings.TrimSpace(i.Action) == "" {
		return errors.New("admin audit intent requires an action")
	}
	if strings.TrimSpace(i.TargetType) == "" {
		return errors.New("admin audit intent requires a target type")
	}
	if strings.TrimSpace(i.TargetID) == "" {
		return errors.New("admin audit intent requires a target id")
	}
	return nil
}
