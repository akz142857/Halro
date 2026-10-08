package statecut

import (
	"context"
	"errors"
	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/governance"
	"math"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
)

// CaptureHAPromotion authenticates the actual complete selected seven roots
// and stopped member/order/source cut. The returned facts alone grant neither
// a promise nor the successor term; the app must persist and fulfill them.
func CaptureHAPromotion(ctx context.Context, o HAOwners, p replication.PromotionProposal, noPeerPromise bool) (claims replication.HAPromotionClaims, resultErr error) {
	if p.Validate() != nil {
		return claims, replication.ErrMemberAuthority
	}
	resultErr = WithHASelectedCut(ctx, o, func(ctx context.Context, cut HASelectedCut) error {
		m := cut.Member
		if m.Term != m.PromisedTerm || m.DurableIndex != m.ConfirmedIndex || m.ConfirmedIndex != m.AppliedIndex || cut.Seal.OrderingIndex != m.AppliedIndex || cut.AllowedIndex != m.AppliedIndex || cut.Seal.RootCount != 7 || p.ExpectedTerm != m.Term || p.ExpectedAppliedIndex != m.AppliedIndex || p.CandidateNodeID != m.NodeID || p.Term != m.PromisedTerm+1 {
			return authenticatedstate.ErrSealIneligible
		}
		// The fresh owner has already reopened and fully authenticated all seven
		// roots. The producer independently checks the whole root identities again
		// before a durable promise can withdraw old ordinary source permission.
		if o.Components.Ledger == nil || o.Components.Usage == nil || o.Components.Governance == nil || o.Components.Directory == nil || o.Components.Metadata == nil || o.Components.Audit == nil {
			return authenticatedstate.ErrSealIneligible
		}
		selected, found, err := o.Components.Publisher.SelectContext(ctx, cut.Authority)
		if err != nil || !found || selected != cut.Seal {
			return errors.Join(authenticatedstate.ErrSealIneligible, err)
		}
		claims = replication.HAPromotionClaims{Version: 1, OccurredAtMicros: time.Now().UTC().UnixMicro(), BaselineDigest: o.Baseline.Descriptor().AuthorityDigest, Before: m, Seal: cut.Seal, Cursors: cut.Allowed, Heads: cut.Heads, Proposal: p, NoPeerPromise: noPeerPromise}
		return ctx.Err()
	})
	return
}

// PublishHAPromotion publishes the existing fully authenticated seven roots at
// the actual new-term structural boundary. Only the explicit stopped recovery
// owner can borrow old roots, and every business entry point rejects that owner.
func PublishHAPromotion(ctx context.Context, o HAOwners) (result Publication, resultErr error) {
	return PublishHAPromotionWithStartupCaches(ctx, o, nil)
}

func PublishHAPromotionWithStartupCaches(ctx context.Context, o HAOwners, prepare func(context.Context) (StartupCacheCandidates, error)) (result Publication, resultErr error) {
	if o.PromotionRecovery == nil || o.Components.NativeLedger == nil {
		return result, authenticatedstate.ErrSealIneligible
	}
	transition := o.PromotionRecovery
	claims := transition.Claims()
	if !claims.Fulfilled || prepare == nil {
		return result, replication.ErrMemberAuthority
	}
	resultErr = o.Components.NativeLedger.WithHASelectedSealAuthority(ctx, func(ctx context.Context, saved authenticatedstate.SealEligibility, _ ledger.HASelectedLedgerPosition) error {
		return withBorrowedHASelectedCut(ctx, o, func(ctx context.Context, cut HASelectedCut) error {
			actual := cut.ActualMember
			if cut.Authority != saved || cut.Seal != claims.Seal || actual.Role != replication.RolePrimary || actual.NodeID != claims.Proposal.CandidateNodeID || actual.Term != claims.Proposal.Term || actual.PromisedTerm != actual.Term || actual.DurableIndex != claims.Before.AppliedIndex+2 || actual.ConfirmedIndex != claims.Before.AppliedIndex || actual.AppliedIndex != claims.Before.AppliedIndex {
				return replication.ErrMemberAuthority
			}
			if err := transition.ValidateOrdering(o.Ordering); err != nil {
				return err
			}
			record, err := o.Ordering.Record(actual.DurableIndex)
			if err != nil {
				return err
			}
			cursors, err := o.Ordering.StoreCursorsThroughContext(ctx, actual.DurableIndex)
			expected := claims.Cursors
			expected[1].Sequence++
			if claims.Proposal.PlannedStepdown {
				expected[1].Sequence++
			}
			if err != nil || cursors != expected || record.Kind != replication.KindData || record.Store != replication.StoreAudit || record.StoreGeneration != 1 || record.StoreSequenceFirst != claims.Cursors[1].Sequence+1 || record.StoreSequenceLast != expected[1].Sequence {
				return errors.Join(replication.ErrMemberAuthority, err)
			}
			offset, err := o.Ordering.PrefixOffset(actual.DurableIndex)
			if err != nil || offset <= 0 {
				return errors.Join(replication.ErrMemberAuthority, err)
			}
			authority := cut.Authority
			authority.Role, authority.Term = authenticatedstate.SealPrimary, actual.Term
			authority.OrderingIndex, authority.OrderingOffset, authority.OrderingMAC = actual.DurableIndex, uint64(offset), record.MAC
			ar, err := o.Components.Audit.SealReference(ctx, o.Components.Budget, o.Components.Audit.Summary())
			if err != nil {
				return err
			}
			hash, err := o.Source.HeadAt(replication.StoreAudit, expected[1])
			if err != nil || ar.Position.Sequence != expected[1].Sequence || ar.Position.Hash != hash {
				return errors.Join(replication.ErrMemberAuthority, err)
			}
			authority.Positions[3] = ar.Position
			base := cut.Seal
			if base.Generation == ^uint64(0) {
				return authenticatedstate.ErrLimit
			}
			base.Generation++
			base.Revision = 1
			result, err = publishHAComponentsFrozen(ctx, o.Components, base, authority, func(ctx context.Context, candidate authenticatedstate.CandidateSeal, persist func(context.Context) error) error {
				caches, err := prepare(ctx)
				if err != nil {
					return err
				}
				if caches.Audit == nil || caches.Governance == nil {
					return authenticatedstate.ErrSealIneligible
				}
				a, g := candidate.Positions[3], candidate.Positions[2]
				if a.Offset > math.MaxInt64 || g.Offset > math.MaxInt64 {
					return authenticatedstate.ErrSealIneligible
				}
				return caches.Audit.WithFrozenOwner(ctx, o.Components.Audit, o.Components.Budget, authority.Instance, audit.Summary{Records: a.Sequence, Bytes: int64(a.Offset), LastHash: a.Hash}, func(ctx context.Context) error {
					return caches.Governance.WithFrozenOwner(ctx, o.Components.NativeGovernance, o.Components.Budget, authority.Instance, governance.Summary{Records: g.Sequence, Bytes: int64(g.Offset), LastHash: g.Hash}, persist)
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
