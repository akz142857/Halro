package statecut

import (
	"context"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
)

// An uncertain descriptor/credit retirement remains reachable through the
// original qualification error. It never becomes publication or Ready evidence.
type HASelectedQualificationFailure struct {
	Owner *Publication
	Cause error
}

func (e *HASelectedQualificationFailure) Error() string { return e.Cause.Error() }
func (e *HASelectedQualificationFailure) Unwrap() error { return e.Cause }

// ValidateHASelectedMaintenance authenticates all seven current roots under
// the real exclusive native lease, independently frozen Member/Ordering/four
// native sources and complete source audits. It requires d=c=a and verifies the
// same assembled root vector as publication, while performing no publication.
// The caller owns startup peer adjudication, admission and later role revocation.
func ValidateHASelectedMaintenance(ctx context.Context, o HAOwners) (resultErr error) {
	if o.Components.NativeLedger == nil {
		return authenticatedstate.ErrSealIneligible
	}
	var verification Publication
	resultErr = o.Components.NativeLedger.WithHASelectedSealAuthority(ctx, func(ctx context.Context, saved authenticatedstate.SealEligibility, allowed ledger.HASelectedLedgerPosition) error {
		return withBorrowedHASelectedCut(ctx, o, func(ctx context.Context, cut HASelectedCut) error {
			if cut.Member.DurableIndex != cut.Member.ConfirmedIndex || cut.Member.DurableIndex != cut.Member.AppliedIndex || saved != cut.Authority || allowed.Generation != cut.Allowed[0].Generation || allowed.Sequence != cut.Allowed[0].Sequence || allowed.Hash != cut.Heads[0] {
				return authenticatedstate.ErrSealIneligible
			}
			a, err := selectedHAMaintenanceAuthority(ctx, o, cut)
			if err != nil {
				return fmt.Errorf("current component authority: %w", err)
			}
			verification, err = publishHAComponentsFrozen(ctx, o.Components, cut.Seal, a, func(ctx context.Context, _ authenticatedstate.CandidateSeal, _ func(context.Context) error) error {
				return ctx.Err()
			})
			if err != nil {
				return fmt.Errorf("current seven-root assembly: %w", err)
			}
			return nil
		})
	})
	if err := verification.CleanupError(); err != nil {
		return &HASelectedQualificationFailure{Owner: &verification, Cause: errors.Join(resultErr, ownedfile.ErrRetirement, err)}
	}
	return resultErr
}
