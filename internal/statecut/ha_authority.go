package statecut

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/replication"
)

// HAOwners borrows actual component and independent member/order/source
// owners. Components reuses the owner inventory only; it never assigns the
// Standalone role to a replicated writer. Baseline is the qualified signed
// group carrier, rather than fields copied from a candidate header.
type HAOwners struct {
	// PromotionRecovery permits only stopped old-cut reconstruction. All ordinary
	// business/source and publication entry points explicitly refuse this mode.
	PromotionRecovery       *replication.HAPromotionAuthority
	PeerPromotionRecovery   *replication.HAPromotionPeerAuthority
	PeerPromotionCompletion *replication.HAPromotionPeerCompletion
	Components              StandaloneOwners
	Member                  *replication.StatePublisher
	Ordering                *replication.OrderingJournal
	Source                  *replication.NativeSource
	Baseline                *replication.MetadataBaselineAuthority
}

// WithHASealAuthority derives and freezes actual same-term role permission.
// Primary may own its durable local prefix ahead of quorum confirmation;
// Replica roots belong only to its actual applied prefix. The callback may
// read/publish components but must not publish Member progress or reenter this
// freeze. Promotion requires an independently qualified term transition.
func WithHASealAuthority(ctx context.Context, o HAOwners, visit func(context.Context, authenticatedstate.SealEligibility) error) error {
	c := o.Components
	if o.PromotionRecovery != nil || o.PeerPromotionRecovery != nil || o.PeerPromotionCompletion != nil {
		return authenticatedstate.ErrSealIneligible
	}
	if c.Budget == nil || c.Guard == nil || c.Publisher == nil || c.NativeLedger == nil || c.Lease == nil || c.Ledger == nil || c.Directory == nil || c.Usage == nil || c.NativeGovernance == nil || c.Governance == nil || c.Audit == nil || c.Metadata == nil || o.Member == nil || o.Ordering == nil || o.Source == nil || o.Baseline == nil || visit == nil {
		return authenticatedstate.ErrSealIneligible
	}
	if err := c.Publisher.ValidateOwner(c.Guard, c.Budget); err != nil {
		return err
	}
	claims := o.Baseline.Claims()
	if c.InstanceID == "" || c.KeyGeneration != claims.New.Baseline.KeyGeneration {
		return authenticatedstate.ErrSealIneligible
	}
	return o.Member.WithReadAuthority(ctx, c.Budget, func(ctx context.Context, member replication.MemberState, authorize func(func() error) error) error {
		if err := validateHAMemberAuthority(member, o.Baseline); err != nil {
			return err
		}
		if member.ClusterID != claims.ClusterID || member.Incarnation != claims.Incarnation || member.Projection.Index != member.AppliedIndex || member.ConfirmedIndex < claims.Index+1 {
			return replication.ErrMemberAuthority
		}
		return o.Ordering.WithFrozenHead(ctx, c.Budget, func(ctx context.Context, head replication.FrozenOrderingHead) error {
			if head.Header.ClusterID != member.ClusterID || head.Header.Incarnation != member.Incarnation || head.Index != member.DurableIndex || head.MAC != member.OrderingHeadMAC || head.Term != member.Term {
				return authenticatedstate.ErrSealIneligible
			}
			boundary, err := o.Ordering.Record(claims.Index + 1)
			if err != nil {
				return err
			}
			descriptor, err := replication.DecodeMetadataBaselineMetadata(boundary.Metadata)
			if err != nil || boundary.Kind != replication.KindMetadataBaseline || boundary.Term != claims.Term || boundary.PreviousMAC != claims.OrderingHead || descriptor != o.Baseline.Descriptor() {
				return errors.Join(authenticatedstate.ErrSealIneligible, err)
			}
			if err := o.Source.ValidateMetadataBaseline(boundary); err != nil {
				return err
			}
			index, role := member.AppliedIndex, authenticatedstate.SealReplica
			if member.Role == replication.RolePrimary {
				if member.NodeID != o.Baseline.CurrentPrimary() {
					return replication.ErrMemberAuthority
				}
				index, role = member.DurableIndex, authenticatedstate.SealPrimary
			} else if member.Role != replication.RoleReplica || member.AppliedIndex > member.ConfirmedIndex {
				return replication.ErrMemberAuthority
			}
			if index < claims.Index+1 {
				return authenticatedstate.ErrSealIneligible
			}
			applied, err := o.Ordering.StoreCursorsThroughContext(ctx, member.AppliedIndex)
			if err != nil {
				return err
			}
			if applied[3].Generation != member.Projection.MetadataEpoch || applied[3].Sequence != member.Projection.MetadataSequence {
				return replication.ErrMemberAuthority
			}
			cursors, err := o.Ordering.StoreCursorsThroughContext(ctx, index)
			if err != nil {
				return err
			}
			record, err := o.Ordering.Record(index)
			if err != nil || record.Term != member.Term {
				return errors.Join(authenticatedstate.ErrSealIneligible, err)
			}
			offset, err := o.Ordering.PrefixOffset(index)
			if err != nil {
				return err
			}
			a := authenticatedstate.SealEligibility{SchemaVersion: 1, ApplyVersion: 1, IndexVersion: 1, KeyDerivation: 1, Instance: claims.New.Baseline.Identity, Cluster: sha256.Sum256([]byte(member.ClusterID)), Member: sha256.Sum256([]byte(member.NodeID)), Role: role, Term: member.Term, OrderingIndex: index, OrderingOffset: uint64(offset), OrderingMAC: record.MAC, Coverage: authenticatedstate.SealCoverageAuthenticated}
			incarnation := sha256.Sum256([]byte(member.Incarnation))
			copy(a.Incarnation[:], incarnation[:16])
			lr, err := c.Ledger.SealReference(ctx)
			if err != nil {
				return err
			}
			a.Positions[0] = authenticatedstate.SealPosition{Component: authenticatedstate.SealComponentLedger, Generation: lr.Anchor.Head.Generation, Sequence: lr.Anchor.Head.Sequence, Offset: uint64(lr.Anchor.Head.Offset), Hash: lr.Anchor.Hash, Epoch: uint64(lr.Anchor.Epoch)}
			mr, err := c.Metadata.SealReference(ctx)
			if err != nil {
				return err
			}
			a.Positions[1] = mr.Position
			gr, err := c.Governance.TypedSealReference(ctx)
			if err != nil {
				return err
			}
			a.Positions[2] = gr.Journal.Position
			ar, err := c.Audit.SealReference(ctx, c.Budget, c.Audit.Summary())
			if err != nil {
				return err
			}
			a.Positions[3] = ar.Position
			a.Positions[4] = a.Positions[0]
			a.Positions[4].Component = authenticatedstate.SealComponentUsage
			// Root/native positions must equal the independently authenticated
			// four allowed source heads, never the physical unconfirmed tail.
			for store := replication.StoreLedger; store <= replication.StoreMetadata; store++ {
				positionIndex := []int{0, 3, 2, 1}[store-replication.StoreLedger]
				position, cursor := a.Positions[positionIndex], cursors[store-replication.StoreLedger]
				generation := position.Generation
				if store == replication.StoreMetadata {
					generation = position.Epoch
				}
				if store == replication.StoreAudit || store == replication.StoreGovernance {
					if position.Generation != 0 {
						return authenticatedstate.ErrSealIneligible
					}
					generation = 1
				}
				hash, err := o.Source.HeadAt(store, cursor)
				if err != nil || generation != cursor.Generation || position.Sequence != cursor.Sequence || position.Hash != hash {
					return fmt.Errorf("HA root component %d differs from actual allowed ordering prefix: %w", store, errors.Join(authenticatedstate.ErrSealIneligible, err))
				}
			}
			return authorize(func() error { return visit(ctx, a) })
		})
	})
}
