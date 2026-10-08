package metadatastate

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

type standaloneFacade struct{ owner *Baseline }

// Business callbacks share one admission lane. Each queued operation owns a
// bounded reservation, and close/caller cancellation wakes every waiter.
// Maintenance still refuses overlapping owner operations rather than waiting
// inside a publication callback.
func (f *standaloneFacade) admit(ctx context.Context) (func() error, error) {
	if ctx == nil {
		return nil, ErrSource
	}
	if f.owner.businessWaiters.Add(1) > 16 {
		f.owner.businessWaiters.Add(-1)
		return nil, authenticatedstate.ErrBusy
	}
	fee, err := f.owner.physicalWork(ctx, resourcebudget.Cost{RAM: 4096})
	if err != nil {
		f.owner.businessWaiters.Add(-1)
		return nil, err
	}
	release := func() error { f.owner.businessWaiters.Add(-1); return fee.Release() }
	waitCtx, stop := resourcebudget.WorkQueueContext(ctx)
	defer stop()
	select {
	case f.owner.businessLane <- struct{}{}:
		if err := errors.Join(waitCtx.Err(), f.owner.ctx.Err()); err != nil {
			if waitCtx.Err() != nil {
				err = errors.Join(err, context.Cause(waitCtx))
			}
			<-f.owner.businessLane
			return nil, errors.Join(err, release())
		}
		return func() error { <-f.owner.businessLane; return release() }, nil
	case <-waitCtx.Done():
		return nil, errors.Join(waitCtx.Err(), context.Cause(waitCtx), release())
	case <-f.owner.ctx.Done():
		return nil, errors.Join(authenticatedstate.ErrClosed, release())
	}
}

var _ boltstore.FacadeBackend = (*standaloneFacade)(nil)
var _ boltstore.FacadeTransaction = (*Transaction)(nil)

// StandaloneFacade qualifies the exact selected Metadata reference and actual
// native mode before lending a backend to the existing business Store. The
// caller still selects the common seal and Runtime role independently. The
// resulting Store owns Close of this same Baseline; the creator retains physical
// Discard authority and all persistent reservations until actual retirement.
func (b *Baseline) StandaloneFacade(ctx context.Context, manager *resourcebudget.Manager, identity [32]byte, instanceID string, keyGeneration uint64, selected AuthenticatedMetadataSealReference) (boltstore.FacadeBackend, error) {
	if b == nil || b.haPeerTransition != nil {
		return nil, ErrSource
	}
	err := b.WithFrozenStandaloneSealReference(ctx, manager, identity, instanceID, keyGeneration, func(ctx context.Context, actual AuthenticatedMetadataSealReference, head metadatajournal.Head) error {
		if actual != selected || metadataSealPosition(head) != selected.Position {
			return ErrSource
		}
		return ctx.Err()
	})
	if err != nil {
		return nil, err
	}
	b.businessOnce.Do(func() { b.businessLane = make(chan struct{}, 1) })
	return &standaloneFacade{owner: b}, nil
}
func (f *standaloneFacade) View(ctx context.Context, visit func(boltstore.FacadeTransaction) error) error {
	return f.withPhysicalCompletion(ctx, func(ctx context.Context) error { return f.viewPhysicalAdmitted(ctx, visit) })
}
func (f *standaloneFacade) viewPhysicalAdmitted(ctx context.Context, visit func(boltstore.FacadeTransaction) error) (err error) {
	release, err := f.admit(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, release()) }()
	return f.owner.View(ctx, func(tx *Transaction) error { return visit(tx) })
}
func (f *standaloneFacade) Update(ctx context.Context, visit func(boltstore.FacadeTransaction) error) (record metadatajournal.Record, err error) {
	err = f.withPhysicalCompletion(ctx, func(ctx context.Context) error {
		var e error
		record, e = f.updatePhysicalAdmitted(ctx, visit)
		return e
	})
	if err != nil {
		record = metadatajournal.Record{}
	}
	return
}
func (f *standaloneFacade) updatePhysicalAdmitted(ctx context.Context, visit func(boltstore.FacadeTransaction) error) (record metadatajournal.Record, err error) {
	release, err := f.admit(ctx)
	if err != nil {
		return record, err
	}
	defer func() {
		err = errors.Join(err, release())
		if err != nil {
			record = metadatajournal.Record{}
		}
	}()
	return f.owner.Update(ctx, func(tx *Transaction) error { return visit(tx) })
}
func (f *standaloneFacade) Path() string                           { return f.owner.output }
func (f *standaloneFacade) JournalPath() string                    { return f.owner.journalPath }
func (f *standaloneFacade) Close() error                           { return f.owner.Close() }
func (f *standaloneFacade) CloseContext(ctx context.Context) error { return f.owner.CloseContext(ctx) }
func (f *standaloneFacade) HealthError() error {
	if f.owner.ctx.Err() != nil {
		return authenticatedstate.ErrClosed
	}
	if !f.owner.ready.Load() && len(f.owner.businessLane) == 0 {
		if !f.owner.mu.TryLock() {
			return authenticatedstate.ErrBusy
		}
		defer f.owner.mu.Unlock()
		if !f.owner.ready.Load() {
			return authenticatedstate.ErrClosed
		}
	}
	if f.owner.selectedStartup {
		return f.owner.journal.HealthError()
	}
	return nil
}
func (f *standaloneFacade) JournalState(ctx context.Context) (state boltstore.JournalState, err error) {
	release, err := f.admit(ctx)
	if err != nil {
		return state, err
	}
	defer func() { err = errors.Join(err, release()) }()
	return f.journalStateAdmitted(ctx)
}

func (f *standaloneFacade) journalStateAdmitted(ctx context.Context) (state boltstore.JournalState, err error) {
	b := f.owner
	if !b.mu.TryLock() {
		return boltstore.JournalState{}, authenticatedstate.ErrBusy
	}
	defer b.mu.Unlock()
	if !b.ready.Load() {
		return boltstore.JournalState{}, authenticatedstate.ErrClosed
	}
	if b.selectedStartup {
		if err := b.journal.HealthError(); err != nil {
			return boltstore.JournalState{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return boltstore.JournalState{}, err
	}
	head := b.journal.Head()
	if head.Epoch != b.position.Epoch {
		return boltstore.JournalState{}, ErrSource
	}
	return boltstore.JournalState{Path: b.journalPath, Epoch: head.Epoch, Sequence: head.Sequence, Applied: b.position.Sequence, TrimmedThrough: head.TrimmedThrough}, nil
}

// PricePinTerminalContext marks the narrow DAO transition; the reducer independently validates its operations.
func (f *standaloneFacade) PricePinTerminalContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, pricePinTerminalKey{}, true)
}
