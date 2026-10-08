package governance

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
)

func (l *Log) selectedProjectionMode(ctx context.Context, a authenticatedstate.SealEligibility, ha bool) bool {
	if l == nil || l.startupProvenance == nil {
		return false
	}
	h := l.startupProvenance.ha
	if !ha {
		return h == nil && a.Role == authenticatedstate.SealStandalone && !l.replica && l.beforeDurable == nil && l.afterDurable == nil
	}
	return h != nil && selectedStartupModeValid(ctx, Options{Replica: l.replica, BeforeDurable: l.beforeDurable, AfterDurable: l.afterDurable}, a, h)
}

// OpenHASelectedTypedGovernanceProjection borrows the actual HA constructor's
// independent Member/source permission. The complete physical native tail is
// authenticated; only the actual independently allowed prefix is reduced.
func OpenHASelectedTypedGovernanceProjection(ctx context.Context, o AuthenticatedGovernanceOptions, s SelectedTypedGovernanceOpenOptions) (result *GovernanceProjection, receipt TypedGovernanceReopen, resultErr error) {
	if o.Native == nil || o.Native.startupProvenance == nil || o.Native.startupProvenance.ha == nil {
		return nil, receipt, ErrStartupCache
	}
	native := o.Native
	resultErr = native.startupProvenance.ha.withScope(ctx, func(ctx context.Context, a authenticatedstate.SealEligibility, allowed HASelectedNativePosition) error {
		native.mu.Lock()
		head := Summary{Records: native.sequence, Bytes: native.offset, LastHash: native.lastHash}
		native.mu.Unlock()
		permitted, err := native.resolveHASelectedPrefix(ctx, a.Role, NativeStartupReceipt{Saved: native.startupSaved, Head: head}, allowed)
		if err != nil {
			return err
		}
		s.Authority, s.Permitted = a, permitted
		result, receipt, err = openSelectedTypedGovernanceProjection(ctx, o, s, true)
		return err
	})
	if resultErr != nil && result != nil {
		resultErr = errors.Join(resultErr, result.Close())
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			result = nil
		}
		receipt = TypedGovernanceReopen{}
	}
	return
}
