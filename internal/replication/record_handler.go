package replication

import (
	"context"
	"errors"
)

// PrimaryRecordHandler is the role-specific half of an authenticated data
// session. Stream framing has already limited this direction to ACK records;
// this layer binds the record's claimed node to the TLS-authenticated Hello
// before it may advance the coordinator.
type PrimaryRecordHandler struct {
	coordinator *PrimaryCoordinator
}

func NewPrimaryRecordHandler(coordinator *PrimaryCoordinator) (*PrimaryRecordHandler, error) {
	if coordinator == nil {
		return nil, errors.New("primary record handler requires a coordinator")
	}
	return &PrimaryRecordHandler{coordinator: coordinator}, nil
}

func (h *PrimaryRecordHandler) Handle(_ context.Context, peer Hello, record StreamRecord, _ func([]byte) error) error {
	if peer.Role != RoleReplica || record.Kind != StreamRecordAcknowledgement {
		return errors.New("primary data session accepts acknowledgements from a Replica only")
	}
	ack, err := UnmarshalAcknowledgement(record.Encoded)
	if err != nil {
		return err
	}
	if ack.NodeID != peer.NodeID {
		return errors.New("acknowledgement node does not match the authenticated peer")
	}
	_, err = h.coordinator.Acknowledge(ack)
	return err
}

// ReplicaRecordHandler persists a Primary frame, applies only the confirmed
// prefix, and returns an ACK carrying the post-apply watermark. Commit notices
// take the same apply-and-ACK path so the final frame in a quiet stream cannot
// remain indefinitely durable but unapplied.
type ReplicaRecordHandler struct {
	receiver *ReplicaReceiver
	applier  *ReplicaApplier
}

func NewReplicaRecordHandler(receiver *ReplicaReceiver, applier *ReplicaApplier) (*ReplicaRecordHandler, error) {
	if receiver == nil || applier == nil {
		return nil, errors.New("replica record handler requires a receiver and applier")
	}
	return &ReplicaRecordHandler{receiver: receiver, applier: applier}, nil
}

func (h *ReplicaRecordHandler) Handle(ctx context.Context, peer Hello, record StreamRecord, send func([]byte) error) error {
	if peer.Role != RolePrimary || send == nil {
		return errors.New("replica data session requires an authenticated Primary and ACK sender")
	}
	switch record.Kind {
	case StreamRecordFrame:
		if err := h.receiver.BindPrimary(peer.NodeID); err != nil {
			return err
		}
		if _, err := h.receiver.Receive(record.Encoded); err != nil {
			return err
		}
	case StreamRecordCommitNotice:
		notice, err := UnmarshalCommitNotice(record.Encoded)
		if err != nil {
			return err
		}
		if notice.NodeID != peer.NodeID {
			return errors.New("commit notice node does not match the authenticated peer")
		}
		if err := h.receiver.BindPrimary(peer.NodeID); err != nil {
			return err
		}
		if err := h.receiver.Confirm(notice); err != nil {
			return err
		}
	default:
		return errors.New("replica data session accepts frames and commit notices only")
	}
	if _, err := h.applier.ApplyConfirmed(ctx); err != nil {
		return err
	}
	ack, err := h.receiver.Acknowledgement()
	if err != nil {
		return err
	}
	encoded, err := ack.MarshalBinary()
	if err != nil {
		return err
	}
	return send(encoded)
}
