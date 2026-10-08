package app

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
)

// buildReplicaLedgerOwner prepares one fresh owner from the authenticated
// applied ordering cut. It never selects a later durable/confirmed cursor just
// because those bytes exist. This is construction only: callers retain startup
// or maintenance ownership, budget admission, and must revalidate identity while
// atomically handing off every dependent reader/reducer before publishing ready.
func (r *replicationRuntime) buildReplicaLedgerOwner(ctx context.Context, native *ledger.Log, output string) (*ledger.LedgerProjection, ledger.ProjectionPrefix, error) {
	return r.buildReplicaLedgerOwnerWithLimit(ctx, native, output, 0, nil)
}

func (r *replicationRuntime) buildReplicaLedgerOwnerWithLimit(ctx context.Context, native *ledger.Log, output string, maxDiskBytes int64, ownCandidate func(*ledger.LedgerProjection)) (*ledger.LedgerProjection, ledger.ProjectionPrefix, error) {
	if err := ctx.Err(); err != nil {
		return nil, ledger.ProjectionPrefix{}, err
	}
	if r == nil || r.role != replication.RoleReplica || r.publisher == nil || r.journal == nil || native == nil {
		return nil, ledger.ProjectionPrefix{}, errors.New("Replica owner build requires authenticated member/ordering/native owners")
	}
	before := r.publisher.Snapshot()
	if before.Role != replication.RoleReplica || before.AppliedIndex > before.ConfirmedIndex || before.Projection.Index != before.AppliedIndex {
		return nil, ledger.ProjectionPrefix{}, errors.New("Replica applied prefix is unavailable")
	}
	cursors, err := r.journal.StoreCursorsThroughContext(ctx, before.AppliedIndex)
	if err != nil {
		return nil, ledger.ProjectionPrefix{}, err
	}
	allowed := cursors[replication.StoreLedger-replication.StoreLedger]
	var p *ledger.LedgerProjection
	var prefix ledger.ProjectionPrefix
	if maxDiskBytes > 0 {
		p, prefix, err = native.BuildProjectionFromCursorWithLimit(ctx, allowed.Generation, allowed.Sequence, output, maxDiskBytes)
	} else {
		p, prefix, err = native.BuildProjectionFromCursor(ctx, allowed.Generation, allowed.Sequence, output)
	}
	if err != nil {
		return nil, ledger.ProjectionPrefix{}, err
	}
	if ownCandidate != nil {
		ownCandidate(p)
	}
	after := r.publisher.Snapshot()
	if err := ctx.Err(); err != nil || !sameReplicaOwnerBuildCut(before, after) {
		return nil, ledger.ProjectionPrefix{}, errors.Join(err, errors.New("Replica owner build identity or applied cut changed"), p.Discard())
	}
	return p, prefix, nil
}

func sameReplicaOwnerBuildCut(a, b replication.MemberState) bool {
	return a.Version == b.Version && a.Role == b.Role && a.ClusterID == b.ClusterID && a.Incarnation == b.Incarnation && a.NodeID == b.NodeID && a.Term == b.Term && a.PromisedTerm == b.PromisedTerm && a.DurableIndex == b.DurableIndex && a.ConfirmedIndex == b.ConfirmedIndex && a.AppliedIndex == b.AppliedIndex && a.OrderingHeadMAC == b.OrderingHeadMAC && a.Projection == b.Projection
}
