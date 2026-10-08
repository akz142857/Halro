package statecut

import (
	"context"
	"github.com/akz142857/Halro/internal/authenticatedstate"
)

// WithHAPeerPromotionSelectedCut is a stopped transition-only source scope.
// It preserves the peer's actual old common role and seven root identities;
// the signed peer receipt plus current real Promise authorize historical
// reconstruction only. All ordinary authority and business entry points
// reject PeerPromotionRecovery.
func WithHAPeerPromotionSelectedCut(ctx context.Context, o HAOwners, visit func(context.Context, HASelectedCut) error) error {
	if o.PeerPromotionRecovery == nil || o.PromotionRecovery != nil || visit == nil {
		return authenticatedstate.ErrSealIneligible
	}
	return withHASelectedCutMode(ctx, o, nil, o.PeerPromotionRecovery, visit)
}
