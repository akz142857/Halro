package replication

import "time"

const replicaStageEventLimit = 128

// ReplicaStageTransition records a failure/recovery boundary, not every frame
// or apply batch. TargetIndex is an attempted index and is not a durable fact.
type ReplicaStageTransition struct {
	Sequence    uint64    `json:"sequence"`
	At          time.Time `json:"at"`
	Stage       string    `json:"stage"`
	From        string    `json:"from"`
	To          string    `json:"to"`
	Reason      string    `json:"reason"`
	TargetIndex uint64    `json:"target_index"`
}

type ReplicaStageHistory struct {
	ReceiverStartedAt time.Time                `json:"receiver_started_at"`
	ReceiveState      string                   `json:"receive_state"`
	ApplyState        string                   `json:"apply_state"`
	Dropped           uint64                   `json:"dropped"`
	Events            []ReplicaStageTransition `json:"events"`
}

// setStage is called with the receiver mutex held. The ring is deliberately
// process-local so recording cannot add I/O to HA durability barriers.
func (r *ReplicaReceiver) setStage(stage, to, reason string, target uint64) {
	current := &r.receiveStage
	if stage == "apply" {
		current = &r.applyStage
	}
	if *current == to {
		return
	}
	r.stageNext++
	event := ReplicaStageTransition{Sequence: r.stageNext, At: time.Now().UTC(), Stage: stage,
		From: *current, To: to, Reason: reason, TargetIndex: target}
	*current = to
	if len(r.stageEvents) == replicaStageEventLimit {
		copy(r.stageEvents, r.stageEvents[1:])
		r.stageEvents[len(r.stageEvents)-1] = event
		r.stageDropped++
	} else {
		r.stageEvents = append(r.stageEvents, event)
	}
}

func (r *ReplicaReceiver) recordApplyBlocked(reason string, target uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setStage("apply", "blocked", reason, target)
}

func (r *ReplicaReceiver) StageTransitions() ReplicaStageHistory {
	r.mu.Lock()
	defer r.mu.Unlock()
	return ReplicaStageHistory{ReceiverStartedAt: r.stageStartedAt, ReceiveState: r.receiveStage,
		ApplyState: r.applyStage, Dropped: r.stageDropped,
		Events: append([]ReplicaStageTransition(nil), r.stageEvents...)}
}
