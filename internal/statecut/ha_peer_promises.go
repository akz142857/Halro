package statecut

import (
	"context"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/replication"
)

// RequireNoHAOrderingPeerPromises must run inside the originating selected
// whole-source loan before publication, target retirement, renewal or closing.
// Unlike renewal it accepts an older saved common root: the callback must be
// able to check obligations before publishing its genuine successor. It grants
// no source, root, role, native IO or readiness authority.
func RequireNoHAOrderingPeerPromises(ctx context.Context, o HAOwners, owner *replication.PhysicalPublication) error {
	if owner == nil || o.PromotionRecovery != nil || o.PeerPromotionRecovery != nil || o.PeerPromotionCompletion != nil {
		return authenticatedstate.ErrSealIneligible
	}
	return withBorrowedHASelectedCut(ctx, o, func(ctx context.Context, cut HASelectedCut) error {
		m := cut.Member
		if m.Term != m.PromisedTerm {
			return authenticatedstate.ErrSealIneligible
		}
		return owner.RequireNoPeerPhysicalPromises(ctx)
	})
}
