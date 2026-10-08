package replication

import (
	"context"
	"errors"
	"sync"
)

// ReplicaConfirmedPrefixProjection applies all derived components in one
// confirmed job. Its result becomes Member applied progress only after the
// actual job and every admitted permission borrower have joined successfully.
// Existing compatibility projections retain the original per-store path.
type ReplicaConfirmedPrefixProjection interface {
	ApplyConfirmedPrefix(context.Context, *ReplicaConfirmedPrefix) (ProjectionState, error)
}

// ReplicaConfirmedPosition is a copy of the actual receiver's frozen identity
// and progress, not a publication token. It must be authenticated against the
// independent Member, ordering chain and native sources before component use.
type ReplicaConfirmedPosition struct {
	ClusterID, Incarnation, NodeID, PrimaryNodeID string
	Term, PromisedTerm                            uint64
	Durable, Confirmed, Applied                   uint64
	Projection                                    ProjectionState
}

// ReplicaConfirmedPrefix is minted only by the actual applier while it owns
// receiver.mu. Native receipt, term changes and other apply publication wait
// for this job. The pointer grants no ordinary business read or common seal.
type ReplicaConfirmedPrefix struct {
	self     *ReplicaConfirmedPrefix
	receiver *ReplicaReceiver
	journal  *OrderingJournal
	position ReplicaConfirmedPosition
	ctx      context.Context
	mu       sync.Mutex
	active   bool
	count    int
	borrowed sync.WaitGroup
	failure  error
}

// WithPosition loans only this actual receiver/journal's confirmed job. A
// copied struct, foreign owner, escaped callback or ninth concurrent borrower
// is rejected. Releasing the outer job waits for accepted borrowers and latches
// their errors even when a caller ignores them.
func (p *ReplicaConfirmedPrefix) WithPosition(ctx context.Context, receiver *ReplicaReceiver, journal *OrderingJournal, visit func(context.Context, ReplicaConfirmedPosition) error) (resultErr error) {
	if p == nil || receiver == nil || journal == nil || visit == nil {
		return ErrMemberAuthority
	}
	p.mu.Lock()
	if p.self != p || !p.active || p.receiver != receiver || p.journal != journal || p.count >= 8 || p.ctx.Err() != nil || ctx.Err() != nil {
		p.mu.Unlock()
		return ErrMemberAuthority
	}
	p.count++
	p.borrowed.Add(1)
	p.mu.Unlock()
	completed := false
	defer func() {
		p.mu.Lock()
		p.count--
		p.failure = errors.Join(p.failure, resultErr)
		if !completed {
			p.failure = errors.Join(p.failure, ErrMemberAuthority)
		}
		p.mu.Unlock()
		p.borrowed.Done()
	}()
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(p.ctx, cancel)
	defer func() { stop(); cancel() }()
	resultErr = errors.Join(visit(opCtx, p.position), opCtx.Err(), p.ctx.Err())
	completed = true
	return
}

func (p *ReplicaConfirmedPrefix) join() {
	p.mu.Lock()
	p.active = false
	p.mu.Unlock()
	p.borrowed.Wait()
}

func (a *ReplicaApplier) applyConfirmedPrefix(ctx context.Context, owner ReplicaConfirmedPrefixProjection) (uint64, error) {
	r := a.receiver
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.poisoned != nil || r.journal != a.journal || r.appliedIndex > r.confirmedIndex || r.confirmedIndex > r.durableIndex || r.projection.Index != r.appliedIndex || r.term != r.promisedTerm {
		return r.appliedIndex, errors.Join(ErrMemberAuthority, r.poisoned)
	}
	if r.confirmedIndex == r.appliedIndex {
		return r.appliedIndex, ctx.Err()
	}
	opCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	p := &ReplicaConfirmedPrefix{receiver: r, journal: a.journal, ctx: opCtx, active: true,
		position: ReplicaConfirmedPosition{ClusterID: r.clusterID, Incarnation: r.incarnation, NodeID: r.nodeID, PrimaryNodeID: r.primaryNodeID, Term: r.term, PromisedTerm: r.promisedTerm, Durable: r.durableIndex, Confirmed: r.confirmedIndex, Applied: r.appliedIndex, Projection: r.projection}}
	p.self = p
	defer p.join()
	projection, err := owner.ApplyConfirmedPrefix(opCtx, p)
	p.join()
	p.mu.Lock()
	err = errors.Join(err, p.failure, opCtx.Err())
	p.mu.Unlock()
	if err != nil {
		return r.appliedIndex, err
	}
	if projection.Index != p.position.Confirmed {
		return r.appliedIndex, ErrMemberAuthority
	}
	if err := r.advanceAppliedWithProjectionLocked(p.position.Confirmed, projection); err != nil {
		return r.appliedIndex, err
	}
	return r.appliedIndex, nil
}
