package statecut

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
)

// AdvanceSelectedMetadataForStartup catches up the actual resumed Metadata
// owner before any DAO facade has been issued. The actual exclusive native
// maintenance loan supplies the independently captured Member/Ordering cut.
// Replica consumes only Applied; a received native suffix remains excluded.
// This publishes neither a new common seal nor business readiness.
func (r *ReopenedHA) AdvanceSelectedMetadataForStartup(ctx context.Context) (resultErr error) {
	if r == nil || !r.mu.TryLock() {
		return authenticatedstate.ErrBusy
	}
	defer r.mu.Unlock()
	if r.closed || r.advanceErr != nil || !r.metadataBorrowed || r.metadataStore != nil || r.metadataSource == nil || r.Owners.Components.NativeLedger == nil {
		return errors.Join(authenticatedstate.ErrSealIneligible, r.advanceErr)
	}
	changed := false
	defer func() {
		if changed && resultErr != nil {
			r.advanceErr = resultErr
		}
	}()
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.ctx, cancel)
	defer func() { stop(); cancel() }()
	c := r.Owners.Components
	return c.NativeLedger.WithHASelectedSealAuthority(opCtx, func(ctx context.Context, a authenticatedstate.SealEligibility, permitted ledger.HASelectedLedgerPosition) error {
		return withBorrowedHASelectedCut(ctx, r.Owners, func(ctx context.Context, cut HASelectedCut) error {
			if cut.Seal != r.Seal || cut.Authority != a || cut.Allowed[0].Generation != permitted.Generation || cut.Allowed[0].Sequence != permitted.Sequence || cut.Heads[0] != permitted.Hash {
				return authenticatedstate.ErrSealIneligible
			}
			allowed := cut.Allowed[3]
			head, err := r.metadataSource.EpochHead()
			if err != nil || head.Epoch != allowed.Generation || head.Sequence < allowed.Sequence {
				return errors.Join(authenticatedstate.ErrSealIneligible, err)
			}
			ref, err := r.metadataSource.SealReference(ctx)
			if err != nil || ref.Position.Epoch != allowed.Generation || ref.Position.Sequence > allowed.Sequence {
				return errors.Join(authenticatedstate.ErrSealIneligible, err)
			}
			if ref.Position.Sequence == allowed.Sequence {
				if ref.Position.Hash != cut.Heads[3] {
					return authenticatedstate.ErrSealIneligible
				}
				return ctx.Err()
			}
			changed = true
			if _, err := r.metadataSource.ReplayTo(ctx, head, allowed.Sequence); err != nil {
				return err
			}
			ref, err = r.metadataSource.SealReference(ctx)
			if err != nil || ref.Position.Epoch != allowed.Generation || ref.Position.Sequence != allowed.Sequence || ref.Position.Hash != cut.Heads[3] {
				return errors.Join(authenticatedstate.ErrSealIneligible, err)
			}
			return ctx.Err()
		})
	})
}
