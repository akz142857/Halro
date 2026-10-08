package statecut

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/replication"
)

// HAConfirmedSources exists only inside the real confirmed applier callback.
// It names actual authenticated native cut facts; it is not a common seal or
// Member applied publication, and cannot authorize ordinary readers.
type HAConfirmedSources struct {
	Position replication.ReplicaConfirmedPosition
	Allowed  [4]replication.StoreCursor
	Heads    [4][32]byte
}

func WithHAConfirmedSourceAuthority(ctx context.Context, o HAOwners, receiver *replication.ReplicaReceiver, prefix *replication.ReplicaConfirmedPrefix, visit func(context.Context, HAConfirmedSources) error) error {
	if prefix == nil || receiver == nil || o.Member == nil || o.Ordering == nil || o.Source == nil || o.Baseline == nil || o.Components.Budget == nil || visit == nil {
		return authenticatedstate.ErrSealIneligible
	}
	if o.PromotionRecovery != nil || o.PeerPromotionRecovery != nil || o.PeerPromotionCompletion != nil {
		return authenticatedstate.ErrSealIneligible
	}

	claims := o.Baseline.Claims()
	return prefix.WithPosition(ctx, receiver, o.Ordering, func(ctx context.Context, position replication.ReplicaConfirmedPosition) error {
		return o.Member.WithReadAuthority(ctx, o.Components.Budget, func(ctx context.Context, member replication.MemberState, authorize func(func() error) error) error {
			if err := validateHAMemberAuthority(member, o.Baseline); err != nil {
				return err
			}
			if member.Role != replication.RoleReplica || position.ClusterID != member.ClusterID || position.Incarnation != member.Incarnation || position.NodeID != member.NodeID || position.PrimaryNodeID != o.Baseline.CurrentPrimary() || position.Term != member.Term || position.PromisedTerm != member.PromisedTerm || position.Durable != member.DurableIndex || position.Confirmed != member.ConfirmedIndex || position.Applied != member.AppliedIndex || position.Projection != member.Projection || member.Projection.Index != member.AppliedIndex || member.AppliedIndex < claims.Index+1 || member.AppliedIndex >= member.ConfirmedIndex || member.ConfirmedIndex > member.DurableIndex {
				return replication.ErrMemberAuthority
			}
			return o.Ordering.WithFrozenHead(ctx, o.Components.Budget, func(ctx context.Context, head replication.FrozenOrderingHead) error {
				if head.Index != member.DurableIndex || head.MAC != member.OrderingHeadMAC || head.Term != member.Term || head.Header.ClusterID != member.ClusterID || head.Header.Incarnation != member.Incarnation {
					return replication.ErrMemberAuthority
				}
				boundary, err := o.Ordering.Record(claims.Index + 1)
				if err != nil || o.Source.ValidateMetadataBaseline(boundary) != nil || boundary.PreviousMAC != claims.OrderingHead || boundary.Term != claims.Term {
					return errors.Join(authenticatedstate.ErrSealIneligible, err)
				}
				allowed, err := o.Ordering.StoreCursorsThroughContext(ctx, member.ConfirmedIndex)
				if err != nil {
					return err
				}
				cut := HAConfirmedSources{Position: position, Allowed: allowed}
				for i, store := range []replication.Store{replication.StoreLedger, replication.StoreAudit, replication.StoreGovernance, replication.StoreMetadata} {
					cut.Heads[i], err = o.Source.HeadAt(store, allowed[i])
					if err != nil {
						return err
					}
				}
				// Authenticate every actual ordered native frame, including received
				// frames excluded from this confirmed reducer job. No history slice.
				if err := replication.ReconstructRangeEach(member.ClusterID, member.Incarnation, 1, member.DurableIndex, o.Ordering, o.Source, func(replication.LocalCommit) error { return ctx.Err() }); err != nil {
					return err
				}
				return authorize(func() error { return visitHAConfirmedMetadataScope(ctx, o, cut, visit) })
			})
		})
	})
}
