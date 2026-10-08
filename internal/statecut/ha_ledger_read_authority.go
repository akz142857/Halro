package statecut

import (
	"context"
	"crypto/sha256"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
)

// WithHALedgerReadAuthority proves the single Ledger stream's current permitted
// head from actual Member and Ordering owners. It is called with the actual
// Ledger writer excluded. It grants no selection of a seven-root common seal:
// other component positions and root evidence are deliberately absent.
func WithHALedgerReadAuthority(ctx context.Context, o HAOwners, visit func(context.Context, authenticatedstate.SealEligibility, ledger.HASelectedLedgerPosition) error) error {
	return withHALedgerReadAuthority(ctx, o, nil, visit)
}

// WithHAConfirmedLedgerApplyAuthority accepts only an actual still-live
// receiver/applier capability. It authorizes the confirmed Ledger prefix for
// this apply job; ordinary readers continue to use actual Member applied.
func WithHAConfirmedLedgerApplyAuthority(ctx context.Context, o HAOwners, receiver *replication.ReplicaReceiver, prefix *replication.ReplicaConfirmedPrefix, visit func(context.Context, authenticatedstate.SealEligibility, ledger.HASelectedLedgerPosition) error) error {
	if prefix == nil {
		return authenticatedstate.ErrSealIneligible
	}
	return prefix.WithPosition(ctx, receiver, o.Ordering, func(ctx context.Context, position replication.ReplicaConfirmedPosition) error {
		return withHALedgerReadAuthority(ctx, o, &position, visit)
	})
}

func withHALedgerReadAuthority(ctx context.Context, o HAOwners, applying *replication.ReplicaConfirmedPosition, visit func(context.Context, authenticatedstate.SealEligibility, ledger.HASelectedLedgerPosition) error) error {
	c := o.Components
	if c.Budget == nil || o.Member == nil || o.Ordering == nil || o.Source == nil || o.Baseline == nil || visit == nil {
		return authenticatedstate.ErrSealIneligible
	}
	if o.PromotionRecovery != nil || o.PeerPromotionRecovery != nil || o.PeerPromotionCompletion != nil {
		return authenticatedstate.ErrSealIneligible
	}

	claims := o.Baseline.Claims()
	read := o.Member.WithReadAuthority
	if applying == nil {
		read = o.Member.WithLedgerReadAuthority
	}
	return read(ctx, c.Budget, func(ctx context.Context, member replication.MemberState, authorize func(func() error) error) error {
		if err := validateHAMemberAuthority(member, o.Baseline); err != nil {
			return err
		}
		if member.AppliedIndex > member.ConfirmedIndex || member.ConfirmedIndex > member.DurableIndex || member.Projection.Index != member.AppliedIndex || member.ConfirmedIndex < claims.Index+1 {
			return replication.ErrMemberAuthority
		}
		index, role := member.AppliedIndex, authenticatedstate.SealReplica
		if applying != nil {
			if member.Role != replication.RoleReplica || applying.ClusterID != member.ClusterID || applying.Incarnation != member.Incarnation || applying.NodeID != member.NodeID || applying.PrimaryNodeID != o.Baseline.CurrentPrimary() || applying.Term != member.Term || applying.PromisedTerm != member.PromisedTerm || applying.Applied != member.AppliedIndex || applying.Confirmed != member.ConfirmedIndex || applying.Durable != member.DurableIndex || applying.Projection != member.Projection {
				return replication.ErrMemberAuthority
			}
			index = applying.Confirmed
		}
		if member.Role == replication.RolePrimary {
			index, role = member.DurableIndex, authenticatedstate.SealPrimary
		} else if member.Role != replication.RoleReplica {
			return replication.ErrMemberAuthority
		}
		if index < claims.Index+1 {
			return authenticatedstate.ErrSealIneligible
		}
		return o.Ordering.WithLedgerReadPrefix(ctx, c.Budget, index, claims.Index+1, func(ctx context.Context, head replication.FrozenOrderingHead, cursor replication.StoreCursor, boundary replication.OrderingRecord) error {
			if head.Header.ClusterID != member.ClusterID || head.Header.Incarnation != member.Incarnation || head.Index != member.DurableIndex || head.MAC != member.OrderingHeadMAC || head.Term != member.Term {
				return replication.ErrMemberAuthority
			}
			descriptor, err := replication.DecodeMetadataBaselineMetadata(boundary.Metadata)
			if err != nil || boundary.Kind != replication.KindMetadataBaseline || boundary.Term != claims.Term || boundary.PreviousMAC != claims.OrderingHead || descriptor != o.Baseline.Descriptor() {
				return errors.Join(authenticatedstate.ErrSealIneligible, err)
			}
			hash, err := o.Source.HeadAt(replication.StoreLedger, cursor)
			if err != nil {
				return err
			}
			a := authenticatedstate.SealEligibility{Instance: claims.New.Baseline.Identity, Cluster: sha256.Sum256([]byte(member.ClusterID)), Member: sha256.Sum256([]byte(member.NodeID)), Role: role, Term: member.Term, OrderingIndex: index, Coverage: authenticatedstate.SealCoverageAuthenticated}
			incarnation := sha256.Sum256([]byte(member.Incarnation))
			copy(a.Incarnation[:], incarnation[:16])
			return authorize(func() error {
				return visit(ctx, a, ledger.HASelectedLedgerPosition{Generation: cursor.Generation, Sequence: cursor.Sequence, Hash: hash})
			})
		})
	})
}
