package statecut

import (
	"context"
	"errors"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/replication"
)

// ReplenishHAOrderingWindow is valid only inside the original full selected
// source callback after actual seven-root publication and debt retirement. It
// borrows Member's held whole-cut mutex; neither a literal cut nor ordinary
// ledger/live authority can obtain this IO range.
func ReplenishHAOrderingWindow(ctx context.Context, o HAOwners, owner *replication.PhysicalPublication, records int) error {
	if owner == nil || o.PromotionRecovery != nil || o.PeerPromotionRecovery != nil || o.PeerPromotionCompletion != nil {
		return authenticatedstate.ErrSealIneligible
	}
	return withBorrowedHASelectedCut(ctx, o, func(ctx context.Context, cut HASelectedCut) error {
		m := cut.Member
		if m.Term != m.PromisedTerm || m.DurableIndex != m.ConfirmedIndex || m.AppliedIndex != m.DurableIndex || cut.AllowedIndex != m.DurableIndex {
			return authenticatedstate.ErrSealIneligible
		}
		// The scope may have entered with an older common root. Reconstruct the
		// newly published eligibility from actual current roots and independent
		// native allowed heads rather than updating the saved candidate fields.
		a, err := selectedHAMaintenanceAuthority(ctx, o, cut)
		if err != nil {
			return err
		}
		selected, found, err := o.Components.Publisher.SelectContext(ctx, a)
		if err != nil || !found || selected.RootCount != 7 || selected.OrderingIndex != m.DurableIndex || selected.OrderingMAC != m.OrderingHeadMAC {
			return errors.Join(authenticatedstate.ErrSealIneligible, err)
		}
		return owner.ReplenishFrozen(ctx, records)
	})
}
