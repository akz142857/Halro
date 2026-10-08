package statecut

import (
	"context"
	"crypto/sha256"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/replication"
)

// WithHAGovernanceReadAuthority grants only the independently authenticated
// ordinary read prefix. A Replica's confirmed application job cannot turn this
// into Applied permission, and no selected root/header supplies the prefix.
func WithHAGovernanceReadAuthority(ctx context.Context, o HAOwners, visit func(context.Context, authenticatedstate.SealEligibility, governance.HASelectedNativePosition) error) error {
	c := o.Components
	if c.Budget == nil || o.Member == nil || o.Ordering == nil || o.Source == nil || o.Baseline == nil || visit == nil {
		return authenticatedstate.ErrSealIneligible
	}
	if o.PromotionRecovery != nil || o.PeerPromotionRecovery != nil || o.PeerPromotionCompletion != nil {
		return authenticatedstate.ErrSealIneligible
	}

	claims := o.Baseline.Claims()
	return o.Member.WithGovernanceReadAuthority(ctx, c.Budget, func(ctx context.Context, member replication.MemberState, authorize func(func() error) error) error {
		if err := validateHAMemberAuthority(member, o.Baseline); err != nil {
			return err
		}
		if member.Projection.Index != member.AppliedIndex || member.AppliedIndex > member.ConfirmedIndex || member.ConfirmedIndex > member.DurableIndex || member.ConfirmedIndex < claims.Index+1 {
			return replication.ErrMemberAuthority
		}
		index, role := member.AppliedIndex, authenticatedstate.SealReplica
		if member.Role == replication.RolePrimary {
			index, role = member.DurableIndex, authenticatedstate.SealPrimary
		} else if member.Role != replication.RoleReplica {
			return replication.ErrMemberAuthority
		}
		return o.Ordering.WithGovernanceReadPrefix(ctx, c.Budget, index, claims.Index+1, func(ctx context.Context, head replication.FrozenOrderingHead, cursor replication.StoreCursor, boundary replication.OrderingRecord) error {
			if head.Header.ClusterID != member.ClusterID || head.Header.Incarnation != member.Incarnation || head.Index != member.DurableIndex || head.MAC != member.OrderingHeadMAC || head.Term != member.Term {
				return replication.ErrMemberAuthority
			}
			descriptor, err := replication.DecodeMetadataBaselineMetadata(boundary.Metadata)
			if err != nil || boundary.Kind != replication.KindMetadataBaseline || boundary.Term != claims.Term || boundary.PreviousMAC != claims.OrderingHead || descriptor != o.Baseline.Descriptor() {
				return errors.Join(authenticatedstate.ErrSealIneligible, err)
			}
			if cursor.Generation != 1 {
				return authenticatedstate.ErrSealIneligible
			}
			hash, err := o.Source.HeadAt(replication.StoreGovernance, cursor)
			if err != nil {
				return err
			}
			a := authenticatedstate.SealEligibility{Instance: claims.New.Baseline.Identity, Cluster: sha256.Sum256([]byte(member.ClusterID)), Member: sha256.Sum256([]byte(member.NodeID)), Role: role, Term: member.Term, OrderingIndex: index, Coverage: authenticatedstate.SealCoverageAuthenticated}
			incarnation := sha256.Sum256([]byte(member.Incarnation))
			copy(a.Incarnation[:], incarnation[:16])
			return authorize(func() error {
				return visit(ctx, a, governance.HASelectedNativePosition{Sequence: cursor.Sequence, Hash: hash})
			})
		})
	})
}
