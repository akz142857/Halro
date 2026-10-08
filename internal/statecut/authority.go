package statecut

import (
	"context"
	"github.com/akz142857/Halro/internal/authenticatedstate"
)

// CurrentStandaloneAuthority advances only native positions under actual
// standalone maintenance ownership. Independent identity/versions remain the
// caller's; common publication authenticates every source again under freeze.
func CurrentStandaloneAuthority(ctx context.Context, o StandaloneOwners, a authenticatedstate.SealEligibility) (authenticatedstate.SealEligibility, error) {
	if o.Publisher == nil || o.NativeLedger == nil || o.Lease == nil || o.Ledger == nil || o.Metadata == nil || o.Governance == nil || o.Audit == nil || a.Role != authenticatedstate.SealStandalone || a.Coverage != authenticatedstate.SealCoverageAuthenticated {
		return a, authenticatedstate.ErrSealIneligible
	}
	if err := o.Publisher.ValidateOwner(o.Guard, o.Budget); err != nil {
		return a, err
	}
	if replicated, err := o.Lease.ValidateOwner(o.NativeLedger); err != nil || replicated {
		return a, authenticatedstate.ErrSealIneligible
	}
	lr, err := o.Ledger.SealReference(ctx)
	if err != nil {
		return a, err
	}
	anchor := lr.Anchor
	a.Positions[0] = authenticatedstate.SealPosition{Component: authenticatedstate.SealComponentLedger, Generation: anchor.Head.Generation, Sequence: anchor.Head.Sequence, Offset: uint64(anchor.Head.Offset), Hash: anchor.Hash, Epoch: uint64(anchor.Epoch)}
	mr, err := o.Metadata.SealReference(ctx)
	if err != nil {
		return a, err
	}
	a.Positions[1] = mr.Position
	gr, err := o.Governance.TypedSealReference(ctx)
	if err != nil {
		return a, err
	}
	a.Positions[2] = gr.Journal.Position
	ar, err := o.Audit.SealReference(ctx, o.Budget, o.Audit.Summary())
	if err != nil {
		return a, err
	}
	a.Positions[3] = ar.Position
	a.Positions[4] = a.Positions[0]
	a.Positions[4].Component = authenticatedstate.SealComponentUsage
	return a, nil
}
