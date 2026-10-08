package governance

import (
	"context"
	"errors"
	"sync"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

// The actual physical reducer accepts a freshly confirmed receiver scope.
// Caller-provided Summary is a comparison, never an independent source grant.
func (p *GovernanceProjection) withPhysicalApplyAuthority(ctx context.Context, target Summary, visit func(context.Context) error) error {
	if p.physical == nil {
		return visit(ctx)
	}
	n := p.cowNative
	if n == nil || n.startupProvenance == nil || n.startupProvenance.ha == nil {
		return ErrUnavailable
	}
	h := n.startupProvenance.ha
	if h.role == authenticatedstate.SealPrimary {
		return h.withScope(ctx, func(ctx context.Context, _ authenticatedstate.SealEligibility, allowed HASelectedNativePosition) error {
			if target.Records > allowed.Sequence || target.Records == allowed.Sequence && target.LastHash != allowed.Hash {
				return ErrUnavailable
			}
			return visit(ctx)
		})
	}
	if h.withApplyAuthority == nil {
		return ErrUnavailable
	}
	var mu sync.Mutex
	active, entered := true, false
	var callbackErr error
	err := h.withApplyAuthority(ctx, func(ctx context.Context, a authenticatedstate.SealEligibility, allowed HASelectedNativePosition) error {
		mu.Lock()
		defer mu.Unlock()
		if !active || entered || ctx == nil || ctx.Err() != nil || a.Role != h.role || a.Coverage != authenticatedstate.SealCoverageAuthenticated || a.Instance != h.initialAuthority.Instance || a.Cluster != h.initialAuthority.Cluster || a.Member != h.initialAuthority.Member || a.Incarnation != h.initialAuthority.Incarnation || a.Term != h.initialAuthority.Term || a.OrderingIndex < h.initialAuthority.OrderingIndex || target.Records > allowed.Sequence || target.Records == allowed.Sequence && target.LastHash != allowed.Hash {
			callbackErr = errors.Join(ErrUnavailable, ctx.Err())
			return callbackErr
		}
		entered = true
		callbackErr = errors.Join(visit(ctx), ctx.Err())
		return callbackErr
	})
	mu.Lock()
	defer mu.Unlock()
	active = false
	if !entered {
		return errors.Join(ErrUnavailable, err)
	}
	return errors.Join(err, callbackErr)
}
