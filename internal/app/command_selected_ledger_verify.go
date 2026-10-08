package app

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// A legacy node checkpoint may predate a selected common root. Authenticate
// the complete native source and resolve the independently selected physical
// Ledger boundary before a diagnostic can credit the newer file.
func verifySelectedCommandLedgerPrefix(ctx context.Context, cfg config.Config, key []byte) (report ledger.ChainReport, selected bool, resultErr error) {
	scope, _ := ctx.Value(commandResourceScopeKey{}).(*commandResourceScope)
	if scope == nil || scope.selectedReader == nil {
		return report, false, nil
	}
	r := scope.selectedReader
	if !r.guard.CoversNativePath(cfg.LedgerPath()) {
		return report, true, authenticatedstate.ErrSealIneligible
	}
	seal, found, err := r.state.publisher.Current()
	if err != nil || !found {
		return report, true, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	var saved authenticatedstate.SealPosition
	for _, p := range seal.Positions[:seal.PositionCount] {
		if p.Component == authenticatedstate.SealComponentLedger {
			saved = p
		}
	}
	if saved.Component != authenticatedstate.SealComponentLedger {
		return report, true, authenticatedstate.ErrSealIneligible
	}
	fee, err := scope.pressure.logicalBudget().TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: ledger.AuthenticatedLedgerSourceRAMBytes})
	if err != nil {
		return report, true, err
	}
	defer func() {
		if !errors.Is(resultErr, ownedfile.ErrRetirement) && !errors.Is(resultErr, ledger.ErrSegmentRetirement) {
			resultErr = errors.Join(resultErr, fee.Release())
		}
		if resultErr != nil {
			report = ledger.ChainReport{}
		}
	}()
	prefix, report, err := ledger.InspectProjectionCursor(ctx, cfg.LedgerPath(), key, saved.Generation, saved.Sequence)
	if err != nil || prefix.Head.Generation != saved.Generation || prefix.Head.Sequence != saved.Sequence || uint64(prefix.Head.Offset) != saved.Offset || prefix.Hash != saved.Hash {
		return report, true, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	return report, true, nil
}
