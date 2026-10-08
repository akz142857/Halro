package governance

import (
	"context"
	"errors"
	"sync"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

// Only an ordinary read enters this provider. Startup authority and confirmed
// apply contexts do not create a read loan. The caller already excludes root
// mutations with its actual projection read lock; it never takes native.mu
// while holding Member, preserving the real applier's native -> projection order.
func (h *haSelectedStartupProvenance) withReadScope(ctx context.Context, visit func(context.Context, HASelectedNativePosition) error) error {
	if scope, ok := ctx.Value(haSelectedStartupScopeKey{}).(*haSelectedStartupScope); ok && h != nil && scope.owner == h {
		scope.mu.Lock()
		if !scope.active.Load() || scope.count >= 8 || ctx.Err() != nil || visit == nil {
			scope.mu.Unlock()
			return errors.Join(ErrUnavailable, ctx.Err())
		}
		scope.count++
		scope.borrowers.Add(1)
		scope.mu.Unlock()
		err := visit(ctx, scope.permitted)
		scope.mu.Lock()
		scope.count--
		scope.failure = errors.Join(scope.failure, err)
		scope.mu.Unlock()
		scope.borrowers.Done()
		return err
	}
	if h == nil || h.withReadAuthority == nil || visit == nil || ctx.Err() != nil {
		return errors.Join(ErrUnavailable, ctx.Err())
	}
	var mu sync.Mutex
	active, entered := true, false
	var failure error
	err := h.withReadAuthority(ctx, func(ctx context.Context, a authenticatedstate.SealEligibility, p HASelectedNativePosition) error {
		mu.Lock()
		defer mu.Unlock()
		if !active || entered || ctx == nil || ctx.Err() != nil || a.Role != h.role || a.Coverage != authenticatedstate.SealCoverageAuthenticated || a.Term == 0 || a.OrderingIndex < h.initialAuthority.OrderingIndex || a.Instance != h.initialAuthority.Instance || a.Cluster != h.initialAuthority.Cluster || a.Member != h.initialAuthority.Member || a.Incarnation != h.initialAuthority.Incarnation || a.Term != h.initialAuthority.Term {
			failure = errors.Join(failure, ErrUnavailable)
			return ErrUnavailable
		}
		entered = true
		failure = errors.Join(failure, visit(ctx, p), ctx.Err())
		return failure
	})
	mu.Lock()
	defer mu.Unlock()
	active = false
	if !entered {
		return errors.Join(ErrUnavailable, err)
	}
	return errors.Join(err, failure)
}
