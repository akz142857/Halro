package statecut

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

// BusinessHealthError observes this already qualified owner graph and its four
// original native startup grants. It neither captures a new common cut nor
// grants publication, applied visibility, semantic writes or role authority.
// Every ordinary read/write must still check its actual dynamic source permit.
func (r *ReopenedHA) BusinessHealthError(ctx context.Context) error {
	if r == nil || !r.mu.TryLock() {
		return authenticatedstate.ErrBusy
	}
	c, store, closed, advanceErr := r.Owners.Components, r.metadataStore, r.closed, r.advanceErr
	ownerCtx := r.ctx
	r.mu.Unlock()
	if closed || ownerCtx == nil || ownerCtx.Err() != nil || store == nil {
		return authenticatedstate.ErrClosed
	}
	if advanceErr != nil {
		return errors.Join(authenticatedstate.ErrRecovery, advanceErr)
	}
	if c.NativeLedger == nil || c.Audit == nil || c.NativeGovernance == nil || c.Metadata == nil || c.Ledger == nil || c.Usage == nil || c.Governance == nil || c.Directory == nil || !c.NativeLedger.SelectedStartupReady() || !c.Audit.SelectedStartupReady() || !c.NativeGovernance.SelectedStartupReady() {
		return authenticatedstate.ErrRecovery
	}
	if err := store.RecoveryError(); err != nil {
		return errors.Join(authenticatedstate.ErrRecovery, err)
	}
	return errors.Join(ctx.Err(), ownerCtx.Err())
}
