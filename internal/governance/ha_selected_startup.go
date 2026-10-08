package governance

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
)

// The callback freezes actual Member, Ordering and authenticated native source
// evidence. Permitted is that actual role's independent sequence/hash, whereas
// Authority selects the older saved common seal and retry capsule.
type HASelectedNativePosition struct {
	Sequence uint64
	Hash     [32]byte
}
type HASelectedSealOpenOptions struct {
	SelectedSealOpenOptions
	WithAuthority      func(context.Context, func(context.Context, authenticatedstate.SealEligibility, HASelectedNativePosition) error) error
	WithReadAuthority  func(context.Context, func(context.Context, authenticatedstate.SealEligibility, HASelectedNativePosition) error) error
	WithApplyAuthority func(context.Context, func(context.Context, authenticatedstate.SealEligibility, HASelectedNativePosition) error) error
}
type haSelectedStartupProvenance struct {
	withAuthority      func(context.Context, func(context.Context, authenticatedstate.SealEligibility, HASelectedNativePosition) error) error
	withReadAuthority  func(context.Context, func(context.Context, authenticatedstate.SealEligibility, HASelectedNativePosition) error) error
	withApplyAuthority func(context.Context, func(context.Context, authenticatedstate.SealEligibility, HASelectedNativePosition) error) error
	role               authenticatedstate.SealRole
	initialAuthority   authenticatedstate.SealEligibility
}
type haSelectedStartupScopeKey struct{}
type haSelectedStartupScope struct {
	owner     *haSelectedStartupProvenance
	authority authenticatedstate.SealEligibility
	active    atomic.Bool
	permitted HASelectedNativePosition
	mu        sync.Mutex
	borrowers sync.WaitGroup
	count     int
	failure   error
}

func (h *haSelectedStartupProvenance) withScope(ctx context.Context, visit func(context.Context, authenticatedstate.SealEligibility, HASelectedNativePosition) error) error {
	if h == nil || h.withAuthority == nil || visit == nil {
		return ErrStartupCache
	}
	var mu sync.Mutex
	active, entered := true, false
	var callbackErr error
	err := h.withAuthority(ctx, func(ctx context.Context, a authenticatedstate.SealEligibility, p HASelectedNativePosition) error {
		mu.Lock()
		defer mu.Unlock()
		if !active || entered || ctx == nil || a.Role != h.role || a.Coverage != authenticatedstate.SealCoverageAuthenticated {
			callbackErr = errors.Join(callbackErr, ErrStartupCache)
			return ErrStartupCache
		}
		entered = true
		scope := &haSelectedStartupScope{owner: h, authority: a, permitted: p}
		scope.active.Store(true)
		callbackErr = visit(context.WithValue(ctx, haSelectedStartupScopeKey{}, scope), a, p)
		scope.mu.Lock()
		scope.active.Store(false)
		scope.mu.Unlock()
		scope.borrowers.Wait()
		scope.mu.Lock()
		callbackErr = errors.Join(callbackErr, scope.failure)
		scope.mu.Unlock()
		return callbackErr
	})
	mu.Lock()
	defer mu.Unlock()
	active = false
	if !entered {
		return errors.Join(ErrStartupCache, err)
	}
	return errors.Join(err, callbackErr)
}
func selectedStartupModeValid(ctx context.Context, o Options, a authenticatedstate.SealEligibility, h *haSelectedStartupProvenance) bool {
	if h == nil {
		return !o.Replica && o.BeforeDurable == nil && o.AfterDurable == nil && a.Role == authenticatedstate.SealStandalone
	}
	scope, ok := ctx.Value(haSelectedStartupScopeKey{}).(*haSelectedStartupScope)
	if !ok || scope.owner != h || !scope.active.Load() || scope.authority != a || a.Role != h.role {
		return false
	}
	if h.role == authenticatedstate.SealReplica {
		return o.Replica && o.BeforeDurable == nil && o.AfterDurable == nil
	}
	return h.role == authenticatedstate.SealPrimary && !o.Replica && o.BeforeDurable != nil && o.AfterDurable != nil
}

// OpenWithHASelectedSeal preserves actual Primary native replication hooks or
// Replica physical-only append mode. It selects under independent permission,
// restores the MAC retry capsule and authenticates only its suffix. Full native
// head is fully authenticated. Replica received tails remain excluded from the
// independently permitted prefix; Primary must retain the full durable head. A joined finite historical audit still gates all writes.
func OpenWithHASelectedSeal(ctx context.Context, path string, key []byte, options Options, o HASelectedSealOpenOptions) (result *Log, receipt NativeStartupReceipt, resultErr error) {
	role := authenticatedstate.SealPrimary
	if options.Replica {
		role = authenticatedstate.SealReplica
	}
	h := &haSelectedStartupProvenance{withAuthority: o.WithAuthority, withReadAuthority: o.WithReadAuthority, withApplyAuthority: o.WithApplyAuthority, role: role}
	resultErr = h.withScope(ctx, func(ctx context.Context, a authenticatedstate.SealEligibility, p HASelectedNativePosition) error {
		if !selectedStartupModeValid(ctx, options, a, h) {
			return ErrStartupCache
		}
		h.initialAuthority = a
		selected := o.SelectedSealOpenOptions
		selected.Authority = a
		var err error
		result, receipt, err = openWithSelectedSeal(ctx, path, key, options, selected, h)
		if err != nil {
			return err
		}
		receipt.Allowed, err = result.resolveHASelectedPrefix(ctx, a.Role, receipt, p)
		if err != nil {
			return err
		}
		return ctx.Err()
	})
	if resultErr != nil && result != nil {
		resultErr = errors.Join(resultErr, result.Close())
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			result = nil
		}
		receipt = NativeStartupReceipt{}
	}
	return
}
