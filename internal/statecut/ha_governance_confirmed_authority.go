package statecut

import (
	"context"
	"crypto/sha256"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/replication"
)

// WithHAGovernanceConfirmedApplyAuthority grants only the actual receiver's
// still-live confirmed application job. The original four-source proof is
// repeated under the actual Member and Ordering owners; ordinary readers keep
// their separate Applied permission.
func WithHAGovernanceConfirmedApplyAuthority(ctx context.Context, o HAOwners, receiver *replication.ReplicaReceiver, prefix *replication.ReplicaConfirmedPrefix, visit func(context.Context, authenticatedstate.SealEligibility, governance.HASelectedNativePosition) error) error {
	if visit == nil {
		return authenticatedstate.ErrSealIneligible
	}
	return WithHAConfirmedSourceAuthority(ctx, o, receiver, prefix, func(ctx context.Context, cut HAConfirmedSources) error {
		position, allowed := cut.Position, cut.Allowed[2]
		if allowed.Generation != 1 {
			return authenticatedstate.ErrSealIneligible
		}
		a := authenticatedstate.SealEligibility{Instance: o.Baseline.Claims().New.Baseline.Identity, Cluster: sha256.Sum256([]byte(position.ClusterID)), Member: sha256.Sum256([]byte(position.NodeID)), Role: authenticatedstate.SealReplica, Term: position.Term, OrderingIndex: position.Confirmed, Coverage: authenticatedstate.SealCoverageAuthenticated}
		incarnation := sha256.Sum256([]byte(position.Incarnation))
		copy(a.Incarnation[:], incarnation[:16])
		return visit(ctx, a, governance.HASelectedNativePosition{Sequence: allowed.Sequence, Hash: cut.Heads[2]})
	})
}
