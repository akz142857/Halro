package ledger

import (
	"context"
	"errors"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// EnableSelectedBusinessReads requires the actual selected native writer and
// its startup lease. Component-only maintenance keeps its refusal semantics.
// Bounded business reads wait through ordinary updates rather than turning a
// durable accounting fact into a permanent Apply failure on transient busy.
func (p *AuthenticatedLedgerProjection) EnableSelectedBusinessReads(ctx context.Context, native *Log, lease *MaintenanceLease, manager *resourcebudget.Manager) error {
	if err := p.ValidateSelectedNativeWriter(ctx, native, lease, manager); err != nil {
		return err
	}
	p.businessReadOnce.Do(func() { p.businessReadLane = make(chan struct{}, 1) })
	p.businessReads.Store(true)
	return nil
}

// EnableHASelectedBusinessReads installs only the bounded read lane. Primary
// retains its actual selected writer/confirmation hooks. Replica requires its
// actual applied cut and exclusive read-only lease; it receives no semantic
// append, confirmation, common publication or Runtime Ready permission.
func (p *AuthenticatedLedgerProjection) EnableHASelectedBusinessReads(ctx context.Context, native *Log, lease *MaintenanceLease, manager *resourcebudget.Manager) error {
	if p == nil || native == nil || lease == nil || native.startupProvenance == nil || native.startupProvenance.ha == nil || p.selectedNative.Load() != native || native.startupBudget != manager || manager == nil {
		return ErrProjectionSource
	}
	h := native.startupProvenance.ha
	if h.role == authenticatedstate.SealPrimary {
		if err := p.ValidateHASelectedNativeWriter(ctx, native, lease, manager); err != nil {
			return err
		}
	} else {
		if h.role != authenticatedstate.SealReplica || !native.options.Replica || !lease.readOnly {
			return ErrProjectionSource
		}
		if replicated, err := lease.ValidateOwner(native); err != nil || !replicated {
			return errors.Join(ErrProjectionSource, err)
		}
		if err := native.WithHASelectedSealAuthority(ctx, func(ctx context.Context, a authenticatedstate.SealEligibility, allowed HASelectedLedgerPosition) error {
			if err := p.ValidateResourceOwner(ctx, manager); err != nil {
				return err
			}
			if err := native.validateSelectedStartupGrant(ctx, true); err != nil {
				return err
			}
			if err := p.checkNativeWriter(native); err != nil {
				return err
			}
			provenance := native.startupProvenance
			selected := SelectedLedgerProjectionOpenOptions{Publisher: provenance.publisher, Guard: provenance.guard, Authority: a}
			return native.WithHASelectedStartupOwner(ctx, manager, selected, provenance.identity, func(ctx context.Context, source SelectedStartupSource) error {
				head := source.NativeHead()
				if a.Role != authenticatedstate.SealReplica || head.Head.Generation != allowed.Generation || head.Head.Sequence != allowed.Sequence || head.Hash != allowed.Hash {
					return ErrProjectionSource
				}
				actual, actualHash, err := p.ReadPosition(ctx)
				if err != nil || actual != head.Head || actualHash != head.Hash {
					return errors.Join(ErrProjectionSource, err)
				}
				return errors.Join(source.Check(ctx), native.validateSelectedStartupGrant(ctx, true))
			})
		}); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.businessReadOnce.Do(func() { p.businessReadLane = make(chan struct{}, 1) })
	p.businessReads.Store(true)
	return nil
}

func (p *AuthenticatedLedgerProjection) admitBusinessRead(ctx context.Context) (func() error, error) {
	if p.businessReadWaiters.Add(1) > 16 {
		p.businessReadWaiters.Add(-1)
		return nil, authenticatedstate.ErrBusy
	}
	fee, err := p.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 4096})
	if err != nil {
		p.businessReadWaiters.Add(-1)
		return nil, err
	}
	releaseFee := func() error {
		p.businessReadWaiters.Add(-1)
		err := finishAuthenticatedSourceCredit(fee, nil)
		if err != nil {
			p.readFailure.CompareAndSwap(nil, &authenticatedLedgerReadFault{err: err})
		}
		return err
	}
	waitCtx, stop := resourcebudget.WorkQueueContext(ctx)
	defer stop()
	select {
	case p.businessReadLane <- struct{}{}:
	case <-waitCtx.Done():
		return nil, errors.Join(waitCtx.Err(), context.Cause(waitCtx), releaseFee())
	case <-p.closeContext.Done():
		return nil, errors.Join(ErrProjectionUnavailable, releaseFee())
	}
	releaseLane := func() error { <-p.businessReadLane; return releaseFee() }
	// One admitted reader waits for the finite owner update; remaining waiters
	// hold only fixed tickets. Cancellation never starts a background lock job.
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if err := errors.Join(waitCtx.Err(), p.closeContext.Err()); err != nil {
			if waitCtx.Err() != nil {
				err = errors.Join(err, context.Cause(waitCtx))
			}
			return nil, errors.Join(err, releaseLane())
		}
		if p.mu.TryLock() {
			return func() error { p.mu.Unlock(); return releaseLane() }, nil
		}
		select {
		case <-waitCtx.Done():
			return nil, errors.Join(waitCtx.Err(), context.Cause(waitCtx), releaseLane())
		case <-p.closeContext.Done():
			return nil, errors.Join(ErrProjectionUnavailable, releaseLane())
		case <-ticker.C:
		}
	}
}
