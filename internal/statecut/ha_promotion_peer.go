package statecut

import (
	"context"
	"errors"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
)

// HAPromotionPeerCaptureFailure keeps the actual whole-root workspace owner
// reachable if its retirement is uncertain. No partial claims are admitted.
type HAPromotionPeerCaptureFailure struct {
	Owner Publication
	Cause error
}

func (e *HAPromotionPeerCaptureFailure) Error() string { return e.Cause.Error() }
func (e *HAPromotionPeerCaptureFailure) Unwrap() error { return e.Cause }

// CaptureHAPromotionPeer requires the actual installed exclusive maintenance
// lease and full independently selected old common cut. It authenticates every
// current root under its original owners, without publishing a successor.
func CaptureHAPromotionPeer(ctx context.Context, o HAOwners, p replication.PromotionProposal) (claims replication.HAPromotionPeerClaims, resultErr error) {
	if p.Validate() != nil || o.PromotionRecovery != nil || o.PeerPromotionRecovery != nil || o.PeerPromotionCompletion != nil || o.Components.NativeLedger == nil {
		return claims, replication.ErrMemberAuthority
	}
	var retirement Publication
	resultErr = o.Components.NativeLedger.WithHASelectedSealAuthority(ctx, func(ctx context.Context, a authenticatedstate.SealEligibility, _ ledger.HASelectedLedgerPosition) error {
		return withBorrowedHASelectedCut(ctx, o, func(ctx context.Context, cut HASelectedCut) error {
			m := cut.Member
			if cut.Authority != a || m.Term != m.PromisedTerm || m.DurableIndex != m.ConfirmedIndex || m.ConfirmedIndex != m.AppliedIndex || m.AppliedIndex != cut.Seal.OrderingIndex || cut.AllowedIndex != m.AppliedIndex || p.ClusterID != m.ClusterID || p.Incarnation != m.Incarnation || p.CandidateNodeID == m.NodeID || p.OldPrimaryNodeID != o.Baseline.CurrentPrimary() || p.ExpectedTerm != m.Term || p.ExpectedAppliedIndex != m.AppliedIndex || p.Term != m.PromisedTerm+1 {
				return authenticatedstate.ErrSealIneligible
			}
			var err error
			retirement, err = publishHAComponentsFrozen(ctx, o.Components, cut.Seal, cut.Authority, func(ctx context.Context, candidate authenticatedstate.CandidateSeal, _ func(context.Context) error) error {
				// The whole-root assembly is used as a read barrier only. The durable
				// publisher closure is deliberately never invoked by this producer.
				if candidate != cut.Seal {
					return authenticatedstate.ErrSealIneligible
				}
				claims = replication.HAPromotionPeerClaims{Version: 1, OccurredAtMicros: time.Now().UTC().UnixMicro(), BaselineDigest: o.Baseline.Descriptor().AuthorityDigest, Before: m, Seal: cut.Seal, Cursors: cut.Allowed, Heads: cut.Heads, Proposal: p}
				return ctx.Err()
			})
			return err
		})
	})
	if resultErr != nil {
		claims = replication.HAPromotionPeerClaims{}
		if retirement.CleanupError() != nil || errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = &HAPromotionPeerCaptureFailure{Owner: retirement, Cause: resultErr}
		}
	}
	return
}
