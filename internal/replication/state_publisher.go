package replication

import (
	"crypto/sha256"
	"errors"
	"slices"
	"sync"
)

// StatePublisher serializes the Primary coordinator or Replica receiver's
// progress into one authenticated state.json snapshot. Identity, membership,
// role and term are immutable here; Phase 2 promotion owns changing them.
type StatePublisher struct {
	mu    sync.Mutex
	path  string
	key   [sha256.Size]byte
	state MemberState
}

func NewStatePublisher(path string, key []byte, state MemberState) (*StatePublisher, error) {
	if path == "" || len(key) != sha256.Size {
		return nil, errors.New("member-state publisher requires a path and 32-byte key")
	}
	if err := state.Validate(); err != nil {
		return nil, err
	}
	publisher := &StatePublisher{path: path, state: cloneMemberState(state)}
	copy(publisher.key[:], key)
	return publisher, nil
}

func (p *StatePublisher) Snapshot() MemberState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return cloneMemberState(p.state)
}

func (p *StatePublisher) PublishPrimary(progress PrimaryProgress) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state.Role != RolePrimary {
		return errors.New("cannot publish Primary progress for a non-Primary member")
	}
	if progress.DurableIndex < p.state.DurableIndex || progress.ConfirmedIndex < p.state.ConfirmedIndex ||
		progress.ConfirmedIndex > progress.DurableIndex {
		return errors.New("primary member-state progress regressed or crossed its durable prefix")
	}
	if progress.DurableIndex == 0 && progress.OrderingHeadMAC != ([sha256.Size]byte{}) ||
		progress.DurableIndex > 0 && progress.OrderingHeadMAC == ([sha256.Size]byte{}) {
		return errors.New("primary member-state progress has an invalid ordering head")
	}
	next := cloneMemberState(p.state)
	next.DurableIndex = progress.DurableIndex
	next.ConfirmedIndex = progress.ConfirmedIndex
	next.OrderingHeadMAC = progress.OrderingHeadMAC
	return p.publish(next)
}

func (p *StatePublisher) PublishReplica(progress ReplicaProgress) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state.Role != RoleReplica {
		return errors.New("cannot publish Replica progress for a non-Replica member")
	}
	if progress.DurableIndex < p.state.DurableIndex || progress.ConfirmedIndex < p.state.ConfirmedIndex ||
		progress.AppliedIndex < p.state.AppliedIndex || progress.ConfirmedIndex > progress.DurableIndex ||
		progress.AppliedIndex > progress.ConfirmedIndex {
		return errors.New("replica member-state progress regressed or crossed its allowed prefix")
	}
	if progress.Projection.Index != progress.AppliedIndex ||
		progress.Projection.MetadataEpoch < p.state.Projection.MetadataEpoch ||
		progress.Projection.MetadataEpoch == p.state.Projection.MetadataEpoch && progress.Projection.MetadataSequence < p.state.Projection.MetadataSequence {
		return errors.New("replica member-state projection regressed or does not match applied progress")
	}
	if progress.DurableIndex == 0 && progress.OrderingHeadMAC != ([sha256.Size]byte{}) ||
		progress.DurableIndex > 0 && progress.OrderingHeadMAC == ([sha256.Size]byte{}) {
		return errors.New("replica member-state progress has an invalid ordering head")
	}
	next := cloneMemberState(p.state)
	next.DurableIndex = progress.DurableIndex
	next.ConfirmedIndex = progress.ConfirmedIndex
	next.AppliedIndex = progress.AppliedIndex
	next.OrderingHeadMAC = progress.OrderingHeadMAC
	next.Projection = progress.Projection
	return p.publish(next)
}

func (p *StatePublisher) publish(next MemberState) error {
	if !sameMemberIdentity(p.state, next) {
		return errors.New("member-state progress publication cannot change identity, role, term or membership")
	}
	if err := WriteState(p.path, next, p.key[:]); err != nil {
		return err
	}
	p.state = next
	return nil
}

func cloneMemberState(state MemberState) MemberState {
	state.Peers = append([]StatePeer(nil), state.Peers...)
	return state
}

func sameMemberIdentity(left, right MemberState) bool {
	return left.Version == right.Version && left.ClusterID == right.ClusterID && left.Incarnation == right.Incarnation &&
		left.NodeID == right.NodeID && left.Role == right.Role && left.Term == right.Term && left.PromisedTerm == right.PromisedTerm &&
		slices.Equal(left.Peers, right.Peers)
}
