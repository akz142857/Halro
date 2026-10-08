package app

import (
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/replication"
)

// A reconnecting Primary may have nothing left to send to this Replica. Send
// the actual Receiver's acknowledgement after genuine data authentication, so
// its last frame can be confirmed even when the prior ACK was lost. Hello's
// watermarks never advance the coordinator by themselves.
func (f *haSelectedRuntimeFoundation) sendSelectedReplicaReadyAck(node string, peer replication.Hello, send func([]byte) error) error {
	if f == nil || f.replication == nil || f.reopened == nil || f.peerPromotionRecovery != nil || f.promotionRecovery != nil || f.ctx.Err() != nil || send == nil {
		return authenticatedstate.ErrSealIneligible
	}
	r := f.replication
	m := r.publisher.Snapshot()
	if r.receiver == nil || r.role != replication.RoleReplica || m.Role != replication.RoleReplica || m.Term != m.PromisedTerm || peer.NodeID != node || peer.Role != replication.RolePrimary || peer.NodeID != f.owners.Baseline.CurrentPrimary() || peer.ClusterID != m.ClusterID || peer.Incarnation != m.Incarnation || peer.Term != m.Term || peer.PromisedTerm != m.Term || peer.MetadataBaselineAuthority != f.owners.Baseline.Descriptor().AuthorityDigest {
		return replication.ErrMemberAuthority
	}
	ack, err := r.receiver.Acknowledgement()
	if err != nil {
		return err
	}
	// A concurrent real apply may make a snapshot stale. Decline and reconnect
	// instead of constructing a copied progress tuple or publishing from Hello.
	m = r.publisher.Snapshot()
	if ack.ClusterID != m.ClusterID || ack.Incarnation != m.Incarnation || ack.NodeID != m.NodeID || ack.Term != m.Term || m.Term != m.PromisedTerm || ack.DurableIndex != m.DurableIndex || ack.AppliedIndex != m.AppliedIndex || m.Projection.Index != m.AppliedIndex || r.receiver.ProjectionProgress() != m.Projection {
		return replication.ErrMemberAuthority
	}
	if m.DurableIndex != 0 {
		record, e := r.journal.Record(m.DurableIndex)
		if e != nil || record.MAC != m.OrderingHeadMAC {
			return errors.Join(replication.ErrMemberAuthority, e)
		}
	}
	encoded, err := ack.MarshalBinary()
	if err != nil {
		return err
	}
	return send(encoded)
}
