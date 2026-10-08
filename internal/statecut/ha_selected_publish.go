package statecut

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
)

// PublishHASelectedMaintenanceCapturedStartupCaches publishes the actual
// selected business generation only after its application has withdrawn all
// DAO/transport producers and installed a real native maintenance lease. The
// constructor's exclusive loan captures Member/Ordering/source permission;
// no authority is reconstructed from a candidate. This initial checkpoint
// requires all physical sources to end at the actual allowed cut. An excluded
// received tail must first be confirmed/applied or use the prefix checkpoint.
// The old generation must close after publication; reopening the new selected
// native/capsule owners is a separate readiness barrier.
func PublishHASelectedMaintenanceCapturedStartupCaches(ctx context.Context, o HAOwners, base authenticatedstate.CandidateSeal, prepare func(context.Context) (StartupCacheCandidates, error)) (result Publication, resultErr error) {
	if o.Components.NativeLedger == nil || prepare == nil {
		return result, authenticatedstate.ErrSealIneligible
	}
	resultErr = o.Components.NativeLedger.WithHASelectedSealAuthority(ctx, func(ctx context.Context, saved authenticatedstate.SealEligibility, allowed ledger.HASelectedLedgerPosition) error {
		return withBorrowedHASelectedCut(ctx, o, func(ctx context.Context, cut HASelectedCut) error {
			if cut.Member.DurableIndex != cut.Member.ConfirmedIndex || cut.Member.DurableIndex != cut.Member.AppliedIndex {
				return fmt.Errorf("HA terminal full Member confirmation/application: %w", authenticatedstate.ErrSealIneligible)
			}
			if cut.Authority != saved || cut.Allowed[0].Generation != allowed.Generation || cut.Allowed[0].Sequence != allowed.Sequence || cut.Heads[0] != allowed.Hash || base.Revision <= cut.Seal.Revision {
				return fmt.Errorf("HA terminal original authority/revision: %w", authenticatedstate.ErrSealIneligible)
			}
			authority, err := selectedHAMaintenanceAuthority(ctx, o, cut)
			if err != nil {
				return fmt.Errorf("HA terminal current source authority: %w", err)
			}
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

// The actual Member barrier and complete Ordering/native qualification are
// still held by withBorrowedHASelectedCut. Its observations cannot be supplied
// through the public API or reused after that constructor loan returns.
func selectedHAMaintenanceAuthority(ctx context.Context, o HAOwners, cut HASelectedCut) (authenticatedstate.SealEligibility, error) {
	c := o.Components
	a := cut.Authority
	a.OrderingIndex = cut.AllowedIndex
	record, err := o.Ordering.Record(cut.AllowedIndex)
	if err != nil || record.Term != cut.Member.Term {
		return a, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	offset, err := o.Ordering.PrefixOffset(cut.AllowedIndex)
	if err != nil || offset <= 0 {
		return a, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	a.OrderingOffset, a.OrderingMAC = uint64(offset), record.MAC
	lr, err := c.Ledger.SealReference(ctx)
	if err != nil {
		return a, fmt.Errorf("Ledger current root: %w", err)
	}
	a.Positions[0] = authenticatedstate.SealPosition{Component: authenticatedstate.SealComponentLedger, Generation: lr.Anchor.Head.Generation, Sequence: lr.Anchor.Head.Sequence, Offset: uint64(lr.Anchor.Head.Offset), Hash: lr.Anchor.Hash, Epoch: uint64(lr.Anchor.Epoch)}
	mr, err := c.Metadata.SealReference(ctx)
	if err != nil {
		return a, fmt.Errorf("Metadata current root: %w", err)
	}
	a.Positions[1] = mr.Position
	gr, err := c.Governance.TypedSealReference(ctx)
	if err != nil {
		return a, fmt.Errorf("Governance current roots: %w", err)
	}
	a.Positions[2] = gr.Journal.Position
	ar, err := c.Audit.SealReference(ctx, c.Budget, c.Audit.Summary())
	if err != nil {
		return a, fmt.Errorf("Audit current root: %w", err)
	}
	a.Positions[3] = ar.Position
	a.Positions[4] = a.Positions[0]
	a.Positions[4].Component = authenticatedstate.SealComponentUsage
	for store := replication.StoreLedger; store <= replication.StoreMetadata; store++ {
		slot := store - replication.StoreLedger
		p := a.Positions[[4]int{0, 3, 2, 1}[slot]]
		generation := p.Generation
		if store == replication.StoreMetadata {
			generation = p.Epoch
		}
		if store == replication.StoreAudit || store == replication.StoreGovernance {
			if generation != 0 {
				return a, authenticatedstate.ErrSealIneligible
			}
			generation = 1
		}
		if generation != cut.Allowed[slot].Generation || p.Sequence != cut.Allowed[slot].Sequence || p.Hash != cut.Heads[slot] {
			return a, authenticatedstate.ErrSealIneligible
		}
	}
	return a, ctx.Err()
}
