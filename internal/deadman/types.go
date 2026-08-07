package deadman

import "time"

type Phase string

const (
	PhaseUnknown     Phase = "unknown"
	PhaseUp          Phase = "up"
	PhasePendingDown Phase = "pending_down"
	PhaseDown        Phase = "down"
	PhasePendingUp   Phase = "pending_up"
)

type TargetState struct {
	Phase                Phase     `json:"phase"`
	StablePhase          Phase     `json:"stable_phase"`
	EverDown             bool      `json:"ever_down"`
	ConsecutiveFailures  int       `json:"consecutive_failures"`
	ConsecutiveSuccesses int       `json:"consecutive_successes"`
	LastCheckedAt        time.Time `json:"last_checked_at"`
	LastReason           string    `json:"last_reason,omitempty"`
	// LastAnchorSequence is the highest audit-anchor sequence (ADR 0015)
	// already pulled and recorded from this target, persisted so a restart
	// cannot be used to replay a stale anchor as if it were current — the
	// next pull always asks for "since" this value, never since zero.
	LastAnchorSequence uint64 `json:"last_anchor_sequence,omitempty"`
	// AnchorReason is the last anchor-pull problem observed for this target,
	// empty when the last pull was clean. It is persisted so a restart does
	// not read as a recovery: the witness going quiet is precisely the state
	// an attacker would want to look like a fresh start.
	AnchorReason string `json:"anchor_reason,omitempty"`
}

type Event struct {
	SchemaVersion string    `json:"schema_version"`
	EventID       string    `json:"event_id"`
	Sequence      uint64    `json:"sequence"`
	ProbeID       string    `json:"probe_id"`
	Environment   string    `json:"environment"`
	Region        string    `json:"region"`
	Cluster       string    `json:"cluster"`
	Kind          string    `json:"kind"`
	TargetID      string    `json:"target_id,omitempty"`
	TargetKind    string    `json:"target_kind,omitempty"`
	State         Phase     `json:"state,omitempty"`
	ObservedAt    time.Time `json:"observed_at"`
	HeartbeatTTL  string    `json:"heartbeat_ttl"`
	ReasonCode    string    `json:"reason_code,omitempty"`
	LatencyMS     int64     `json:"latency_ms,omitempty"`
	Attempt       int       `json:"attempt"`
}

type queuedEvent struct {
	Event       Event     `json:"event"`
	NextAttempt time.Time `json:"next_attempt"`
}

type persistedState struct {
	Version  int                    `json:"version"`
	Sequence uint64                 `json:"sequence"`
	Targets  map[string]TargetState `json:"targets"`
	Outbox   []queuedEvent          `json:"outbox"`
}
