package ledger

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// Called under native.mu, before any derived pool renewal or rename. TryRLock
// cannot wait behind a projection writer which itself needs native.mu. The
// pinned position and both complete original recovery indexes, rather than a
// zero physical counter, prove the actual selected source has no future debt.
func (l *Log) verifyPhysicalRollNoDebtLocked(ctx context.Context) (resultErr error) {
	o := l.physical.Load()
	if o == nil || l.startupProvenance == nil {
		return nil
	}
	p := o.projection
	if p == nil || p.selectedNative.Load() != l || l.startupProvenance.ha != nil || !l.selectedStartupStructuralAllowedLocked() || o.debt != 0 || o.active != 0 {
		return resourcebudget.ErrPhysicalAllocation
	}
	return l.verifyPhysicalRollRecoveryIndexesLocked(ctx)
}

// Source qualification belongs to verifyPhysicalRollNoDebtLocked. This helper
// proves only the actual current authenticated indexes and native position.
func (l *Log) verifyPhysicalRollRecoveryIndexesLocked(ctx context.Context) (resultErr error) {
	o := l.physical.Load()
	if o == nil || o.projection == nil {
		return ErrProjectionSource
	}
	return o.projection.verifyPhysicalRollRecoveryIndexesLocked(ctx, l)
}

func (p *AuthenticatedLedgerProjection) verifyPhysicalRollRecoveryIndexesLocked(ctx context.Context, l *Log) (resultErr error) {
	if p == nil || l == nil {
		return ErrProjectionSource
	}
	if !p.mu.TryRLock() {
		return authenticatedstate.ErrBusy
	}
	defer p.mu.RUnlock()
	if !p.ready || p.closeContext.Err() != nil || p.readFault() != nil {
		return errors.Join(ErrProjectionUnavailable, p.readFault())
	}
	pin, err := p.store.Snapshot(ctx)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, pin.Close()) }()
	v := &authenticatedRecoveryView{owner: p, snapshot: pin, ctx: ctx, active: true}
	position, found, _, err := recoveryGet[projectionPosition](v, ctx, "meta", []byte("position"))
	if err != nil || !found || position.Watermark != (Watermark{Generation: l.generation, Sequence: l.chainSequence, Offset: l.offset}) || position.Hash != l.chainHash || position.Epoch != l.chainEpoch {
		return errors.Join(ErrProjectionSource, err)
	}
	for _, bucket := range []string{"pending", "active_requests"} {
		lo, hi, err := recoveryBounds(bucket, nil)
		if err != nil {
			return err
		}
		page, err := pin.Range(ctx, authenticatedstate.RangeOptions{Lo: lo, Hi: hi, MaxRows: 1, MaxBytes: 8 << 20, MaxPages: 4096})
		if err != nil {
			return err
		}
		if len(page.Rows) != 0 || !page.Done {
			return resourcebudget.ErrPhysicalAllocation
		}
	}
	return ctx.Err()
}
