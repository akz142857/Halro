package replication

import (
	"context"
	"errors"
)

// WithPhysicalReceiveReadBarrier excludes the actual receiver's native receipt,
// state publication and original applier's projection writes using their own
// mutexes. The copied position grants no source, IO, role or root authority.
// The callback must not reenter this receiver or applier. Busy/cancellation
// produces no observation, and original mutations cannot resume until it exits.
func (a *ReplicaApplier) WithPhysicalReceiveReadBarrier(ctx context.Context, visit func(context.Context, ReplicaConfirmedPosition, Acknowledgement) error) error {
	if a == nil || ctx == nil || visit == nil || a.receiver == nil || a.journal == nil || ctx.Err() != nil {
		return ErrMemberAuthority
	}
	if !a.mu.TryLock() {
		return ErrMemberAuthority
	}
	defer a.mu.Unlock()
	r := a.receiver
	if !r.mu.TryLock() {
		return ErrMemberAuthority
	}
	defer r.mu.Unlock()
	if r.journal != a.journal || r.poisoned != nil || r.term != r.promisedTerm || r.appliedIndex > r.confirmedIndex || r.confirmedIndex > r.durableIndex || r.projection.Index != r.appliedIndex || r.startupSink != nil && r.startupSink.native == nil {
		return errors.Join(ErrMemberAuthority, r.poisoned)
	}
	position := ReplicaConfirmedPosition{ClusterID: r.clusterID, Incarnation: r.incarnation, NodeID: r.nodeID, PrimaryNodeID: r.primaryNodeID, Term: r.term, PromisedTerm: r.promisedTerm, Durable: r.durableIndex, Confirmed: r.confirmedIndex, Applied: r.appliedIndex, Projection: r.projection}
	return errors.Join(visit(ctx, position, r.acknowledgement()), ctx.Err())
}

// InstallPhysicalReceiveAdmission adds a deny-only policy to the original
// receiver, before its first data-session producer is exposed. It cannot
// replace any original source/term/Ordering permission or accept duplicates.
func (a *ReplicaApplier) InstallPhysicalReceiveAdmission(ctx context.Context, before func(Frame) error) error {
	if before == nil {
		return ErrMemberAuthority
	}
	return a.WithPhysicalReceiveReadBarrier(ctx, func(context.Context, ReplicaConfirmedPosition, Acknowledgement) error {
		if a.receiver.physicalAdmission != nil {
			return ErrMemberAuthority
		}
		a.receiver.physicalAdmission = before
		return nil
	})
}
