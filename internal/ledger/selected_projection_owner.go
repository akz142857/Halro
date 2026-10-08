package ledger

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

type SelectedLedgerProjectionOpenOptions struct {
	Publisher        *authenticatedstate.SealCandidatePublisher
	Guard            *lock.Lock
	Authority        authenticatedstate.SealEligibility
	PhysicalRecovery *PhysicalStartupRecoveryOptions
}
type selectedStartupProvenance struct {
	publisher *authenticatedstate.SealCandidatePublisher
	guard     *lock.Lock
	seal      authenticatedstate.CandidateSeal
	identity  [32]byte
	ha        *haSelectedStartupProvenance
}

// OpenSelectedAuthenticatedLedgerProjection requires actual native provenance
// established by OpenWithSelectedSeal. It independently selects the same seal,
// opens its exact authenticated Ledger root, and verifies/reduces only the
// saved native suffix. A caller-supplied root or anchor cannot enable this path.
func OpenSelectedAuthenticatedLedgerProjection(ctx context.Context, native *Log, directory string, o AuthenticatedLedgerOpenOptions, selected SelectedLedgerProjectionOpenOptions) (*AuthenticatedLedgerProjection, AuthenticatedLedgerReopen, error) {
	if native == nil || native.startupProvenance == nil || native.startupProvenance.ha != nil {
		return nil, AuthenticatedLedgerReopen{}, ErrProjectionSource
	}
	return openAuthenticatedLedgerProjectionQualified(ctx, native, directory, o, nil, &selected)
}

// validateSelectedOwnerLocked is called only inside actual native mutation
// ownership and mu; the private constructor provenance cannot be manufactured
// by a regular full-native Open or an imported selected root.
func (l *Log) validateSelectedOwnerLocked(ctx context.Context, budget *resourcebudget.Manager, identity [32]byte, o SelectedLedgerProjectionOpenOptions) (authenticatedstate.CandidateSeal, error) {
	p := l.startupProvenance
	if p == nil || budget == nil || l.startupBudget != budget || p.identity != identity || p.publisher != o.Publisher || p.guard != o.Guard || o.Publisher == nil || o.Guard == nil || !o.Guard.CoversNativePath(l.path) || o.Authority.Coverage != authenticatedstate.SealCoverageAuthenticated || l.file == nil || l.closeContext.Err() != nil || l.status.Load() != AccountingHealthy {
		return authenticatedstate.CandidateSeal{}, ErrProjectionSource
	}
	if p.ha == nil {
		if o.Authority.Role != authenticatedstate.SealStandalone || l.options.Replica || l.options.AfterDurable != nil || l.options.BeforeDurable != nil || l.options.WaitConfirmed != nil {
			return authenticatedstate.CandidateSeal{}, ErrProjectionSource
		}
	} else if err := validateHASelectedStartupScope(ctx, p.ha, o.Authority, l.options); err != nil {
		return authenticatedstate.CandidateSeal{}, err
	}
	if err := o.Publisher.ValidateOwner(o.Guard, budget); err != nil {
		return authenticatedstate.CandidateSeal{}, err
	}
	seal, found, err := o.Publisher.Select(o.Authority)
	if err != nil || !found || seal != p.seal || ctx.Err() != nil {
		return authenticatedstate.CandidateSeal{}, errors.Join(ErrProjectionSource, err, ctx.Err())
	}
	return seal, nil
}
