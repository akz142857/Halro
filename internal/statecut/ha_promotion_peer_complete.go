package statecut

import (
	"context"
	"errors"
	"math"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
)

// ApplyHAPeerLeadership authenticates all seven OWN old roots inside the
// original Receiver/Applier's opaque confirmed job. The one structural record
// changes no native cursor; only the original Applier may persist Applied.
func ApplyHAPeerLeadership(ctx context.Context, o HAOwners, receiver *replication.ReplicaReceiver, prefix *replication.ReplicaConfirmedPrefix) (result replication.ProjectionState, resultErr error) {
	completion := o.PeerPromotionCompletion
	if completion == nil || completion.Peer() != o.PeerPromotionRecovery || prefix == nil || receiver == nil || o.Components.NativeLedger == nil {
		return result, authenticatedstate.ErrSealIneligible
	}
	p := completion.Peer().Claims()
	var retirement Publication
	resultErr = prefix.WithPosition(ctx, receiver, o.Ordering, func(ctx context.Context, position replication.ReplicaConfirmedPosition) error {
		if position.ClusterID != p.Before.ClusterID || position.Incarnation != p.Before.Incarnation || position.NodeID != p.Before.NodeID || position.PrimaryNodeID != p.Proposal.CandidateNodeID || position.Term != p.Proposal.Term || position.PromisedTerm != p.Proposal.Term || position.Durable != p.Before.AppliedIndex+1 || position.Confirmed != position.Durable || position.Applied != p.Before.AppliedIndex || position.Projection != p.Before.Projection {
			return replication.ErrMemberAuthority
		}
		return o.Components.NativeLedger.WithHASelectedSealAuthority(ctx, func(ctx context.Context, a authenticatedstate.SealEligibility, _ ledger.HASelectedLedgerPosition) error {
			return withBorrowedHASelectedCut(ctx, o, func(ctx context.Context, cut HASelectedCut) error {
				m := cut.ActualMember
				if cut.Authority != a || cut.Seal != p.Seal || cut.Allowed != p.Cursors || cut.Heads != p.Heads || m.AppliedIndex != position.Applied || m.DurableIndex != position.Durable || m.ConfirmedIndex != position.Confirmed || m.Projection != position.Projection {
					return replication.ErrMemberAuthority
				}
				if err := completion.ValidateLeadershipMember(m, o.Ordering); err != nil {
					return err
				}
				var err error
				retirement, err = publishHAComponentsFrozen(ctx, o.Components, cut.Seal, cut.Authority, func(ctx context.Context, candidate authenticatedstate.CandidateSeal, _ func(context.Context) error) error {
					if candidate != p.Seal {
						return authenticatedstate.ErrSealIneligible
					}
					result = position.Projection
					result.Index = position.Confirmed
					return ctx.Err()
				})
				return err
			})
		})
	})
	if resultErr != nil {
		result = replication.ProjectionState{}
		if retirement.CleanupError() != nil || errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = &HAPromotionPeerCaptureFailure{Owner: retirement, Cause: resultErr}
		}
	}
	return
}

// PublishHAPeerLeadership derives Replica role/term from the actual durable
// Member after the original job completed, and verifies the real K+1 Ordering.
// The old Primary seal is only a reconstruction source; it is never relabeled
// to grant ordinary reads before this complete durable publication.
func PublishHAPeerLeadership(ctx context.Context, o HAOwners, prepare func(context.Context) (StartupCacheCandidates, error)) (result Publication, resultErr error) {
	completion := o.PeerPromotionCompletion
	if completion == nil || completion.Peer() != o.PeerPromotionRecovery || o.Components.NativeLedger == nil || prepare == nil {
		return result, authenticatedstate.ErrSealIneligible
	}
	p := completion.Peer().Claims()
	resultErr = o.Components.NativeLedger.WithHASelectedSealAuthority(ctx, func(ctx context.Context, saved authenticatedstate.SealEligibility, _ ledger.HASelectedLedgerPosition) error {
		return withBorrowedHASelectedCut(ctx, o, func(ctx context.Context, cut HASelectedCut) error {
			m := cut.ActualMember
			if cut.Authority != saved || cut.Seal != p.Seal || m.Role != replication.RoleReplica || m.Term != p.Proposal.Term || m.Term != m.PromisedTerm || m.AppliedIndex != p.Before.AppliedIndex+1 || m.AppliedIndex != m.ConfirmedIndex || m.ConfirmedIndex != m.DurableIndex || cut.Allowed != p.Cursors || cut.Heads != p.Heads {
				return replication.ErrMemberAuthority
			}
			if err := completion.ValidateLeadershipMember(m, o.Ordering); err != nil {
				return err
			}
			record, err := o.Ordering.Record(m.AppliedIndex)
			if err != nil {
				return err
			}
			offset, err := o.Ordering.PrefixOffset(m.AppliedIndex)
			if err != nil || offset <= 0 {
				return errors.Join(replication.ErrMemberAuthority, err)
			}
			a := cut.Authority
			a.Role, a.Term = authenticatedstate.SealReplica, m.Term
			a.OrderingIndex, a.OrderingOffset, a.OrderingMAC = m.AppliedIndex, uint64(offset), record.MAC
			base := cut.Seal
			if base.Generation == ^uint64(0) {
				return authenticatedstate.ErrLimit
			}
			base.Generation++
			base.Revision = 1
			result, err = publishHAComponentsFrozen(ctx, o.Components, base, a, func(ctx context.Context, candidate authenticatedstate.CandidateSeal, persist func(context.Context) error) error {
				caches, err := prepare(ctx)
				if err != nil {
					return err
				}
				if caches.Audit == nil || caches.Governance == nil {
					return authenticatedstate.ErrSealIneligible
				}
				x, y := candidate.Positions[3], candidate.Positions[2]
				if x.Offset > math.MaxInt64 || y.Offset > math.MaxInt64 {
					return authenticatedstate.ErrSealIneligible
				}
				return caches.Audit.WithFrozenOwner(ctx, o.Components.Audit, o.Components.Budget, a.Instance, audit.Summary{Records: x.Sequence, Bytes: int64(x.Offset), LastHash: x.Hash}, func(ctx context.Context) error {
					return caches.Governance.WithFrozenOwner(ctx, o.Components.NativeGovernance, o.Components.Budget, a.Instance, governance.Summary{Records: y.Sequence, Bytes: int64(y.Offset), LastHash: y.Hash}, persist)
				})
			})
			return err
		})
	})
	if resultErr != nil {
		result.Seal = authenticatedstate.CandidateSeal{}
	}
	return
}
