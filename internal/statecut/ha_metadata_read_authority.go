package statecut

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/replication"
)

// The actual selected Metadata constructor fixed the immutable baseline/file
// revision. Ordinary reads obtain only their independently permitted stream
// prefix. Holding the Ordering writer mutex makes later native publication wait
// for this finite job; an offline common-cut freeze cannot be used in business.
func withHAMetadataReadAuthority(ctx context.Context, o HAOwners, saved authenticatedstate.SealPosition, role authenticatedstate.SealRole, visit func(context.Context, authenticatedstate.SealPosition) error) error {
	if o.Member == nil || o.Ordering == nil || o.Source == nil || o.Baseline == nil || o.Components.Budget == nil || visit == nil || saved.Component != authenticatedstate.SealComponentMetadata || saved.Revision == 0 {
		return authenticatedstate.ErrSealIneligible
	}
	if o.PromotionRecovery != nil {
		return authenticatedstate.ErrSealIneligible
	}

	claims := o.Baseline.Claims()
	return o.Member.WithMetadataReadAuthority(ctx, o.Components.Budget, func(ctx context.Context, member replication.MemberState, authorize func(func() error) error) error {
		if err := validateHAMemberAuthority(member, o.Baseline); err != nil {
			return err
		}
		if member.AppliedIndex > member.ConfirmedIndex || member.ConfirmedIndex > member.DurableIndex || member.Projection.Index != member.AppliedIndex || member.ConfirmedIndex < claims.Index+1 {
			return replication.ErrMemberAuthority
		}
		index, actualRole := member.AppliedIndex, authenticatedstate.SealReplica
		if member.Role == replication.RolePrimary {
			index, actualRole = member.DurableIndex, authenticatedstate.SealPrimary
		} else if member.Role != replication.RoleReplica {
			return replication.ErrMemberAuthority
		}
		if actualRole != role {
			return replication.ErrMemberAuthority
		}
		return o.Ordering.WithMetadataReadPrefix(ctx, o.Components.Budget, index, claims.Index+1, func(ctx context.Context, head replication.FrozenOrderingHead, cursor replication.StoreCursor, boundary replication.OrderingRecord) error {
			if head.Header.ClusterID != member.ClusterID || head.Header.Incarnation != member.Incarnation || head.Index != member.DurableIndex || head.MAC != member.OrderingHeadMAC || head.Term != member.Term {
				return replication.ErrMemberAuthority
			}
			descriptor, err := replication.DecodeMetadataBaselineMetadata(boundary.Metadata)
			if err != nil || boundary.Kind != replication.KindMetadataBaseline || boundary.Term != claims.Term || boundary.PreviousMAC != claims.OrderingHead || descriptor != o.Baseline.Descriptor() {
				return errors.Join(authenticatedstate.ErrSealIneligible, err)
			}
			if cursor.Generation != claims.New.Baseline.Epoch || saved.Epoch != claims.New.Baseline.Epoch || saved.Revision != claims.New.FileRevision || saved.MetadataBaseline != claims.New.Baseline.Cut || cursor.Sequence < saved.Sequence {
				return authenticatedstate.ErrSealIneligible
			}
			hash, err := o.Source.HeadAt(replication.StoreMetadata, cursor)
			if err != nil {
				return err
			}
			position := saved
			position.Epoch, position.Sequence, position.Hash = cursor.Generation, cursor.Sequence, hash
			return authorize(func() error { return visit(ctx, position) })
		})
	})
}
