package replication

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

type terminalWindowKey struct{}

// TerminalWindow holds a fixed number of pre-provider native frame credits.
// Unused credits end with the real request; adopted ones belong to the exact
// Primary term and remain charged through transport failure until durable ACK.
type TerminalWindow struct {
	coordinator   *PrimaryCoordinator
	term          uint64
	owner         resourcebudget.Permit
	admissions    [resourcebudget.MaxCompletionBatch]primaryUnconfirmedAdmission
	count, next   int
	pending       int
	ownerReleased bool
	closed        bool
	closeErr      error
}

type terminalWindowFailure struct {
	window *TerminalWindow
	cause  error
}

func (e *terminalWindowFailure) Error() string { return e.cause.Error() }
func (e *terminalWindowFailure) Unwrap() error { return e.cause }

func (w *TerminalWindow) retainFailureLocked() error {
	if w.closeErr == nil {
		return nil
	}
	failure := &terminalWindowFailure{window: w, cause: w.closeErr}
	c := w.coordinator
	c.poisoned = errors.Join(c.poisoned, failure)
	c.available, c.unavailable = false, c.poisoned
	c.signalChanged()
	return failure
}

func (c *PrimaryCoordinator) ReserveTerminalWindow(ctx context.Context, count int, eachBytes int64) (*TerminalWindow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped || c.poisoned != nil || c.manualUnavailable != nil || !c.available {
		return nil, ErrReplicationUnavailable
	}
	if c.windowLimits.UnconfirmedBudget == nil {
		if err := c.journal.requirePhysicalAdmissionBudget(false); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if count < 1 || count > resourcebudget.MaxCompletionBatch || eachBytes <= 0 {
		return nil, resourcebudget.ErrInvalid
	}
	if c.recoveryOverLimit || count > c.windowLimits.MaxUnconfirmedEntries-len(c.unconfirmedEntries)-c.reservedAdmissionSlots {
		c.noteUnconfirmedAdmissionRefusalLocked()
		return nil, resourcebudget.ErrExhausted
	}
	owner, credits, err := c.windowLimits.UnconfirmedBudget.TryAcquireCompletionBatch(resourcebudget.IO, resourcebudget.Cost{RAM: 8192}, count, resourcebudget.Cost{Unconfirmed: eachBytes})
	if err != nil {
		c.noteUnconfirmedAdmissionRefusalLocked()
		return nil, err
	}
	w := &TerminalWindow{coordinator: c, term: c.term, owner: owner, count: count, pending: count}
	for i, credit := range credits {
		physical, reserveErr := c.journal.ReservePhysicalAppend(c.term)
		if reserveErr != nil {
			var cleanup error
			for n := 0; n < i; n++ {
				cleanup = errors.Join(cleanup, w.admissions[n].physical.Release())
			}
			for _, unused := range credits {
				cleanup = errors.Join(cleanup, unused.Release())
			}
			cleanup = errors.Join(cleanup, owner.Release())
			if cleanup != nil {
				c.poisoned = errors.Join(reserveErr, cleanup)
			}
			return nil, errors.Join(reserveErr, cleanup)
		}
		w.admissions[i] = primaryUnconfirmedAdmission{coordinator: c, physical: physical, permit: credit, bytes: eachBytes, slot: true, completion: true, window: w}
	}
	c.reservedAdmissionSlots += count
	return w, nil
}

func (w *TerminalWindow) Bind(ctx context.Context) context.Context {
	if w == nil {
		return ctx
	}
	return context.WithValue(ctx, terminalWindowKey{}, w)
}

func (w *TerminalWindow) Close() error {
	if w == nil {
		return nil
	}
	c := w.coordinator
	c.mu.Lock()
	defer c.mu.Unlock()
	if w.closed {
		return w.closeErr
	}
	w.closed = true
	for i := w.next; i < w.count; i++ {
		w.closeErr = errors.Join(w.closeErr, w.admissions[i].releaseLocked())
	}
	w.retireControlLocked()
	return w.retainFailureLocked()
}

func (w *TerminalWindow) retireControlLocked() {
	if w.closed && w.pending == 0 && !w.ownerReleased && w.closeErr == nil {
		w.closeErr = w.owner.Release()
		w.ownerReleased = w.closeErr == nil
	}
}

func (a *primaryUnconfirmedAdmission) detachWindowLocked() {
	if a.window != nil {
		w := a.window
		a.window = nil
		w.pending--
		w.retireControlLocked()
		if w.closed {
			w.retainFailureLocked()
		}
	}
}

// ReserveNativeSourceUnconfirmedContext consumes only this request's preowned
// terminal slot. Term/stop/poison gates still run before a source append, while
// ordinary admission pressure and a full competing IO class cannot take it.
func (c *PrimaryCoordinator) ReserveNativeSourceUnconfirmedContext(ctx context.Context, sourceBytes int) (UnconfirmedAdmission, error) {
	if !resourcebudget.InCompletionContinuation(ctx) {
		return c.ReserveNativeSourceUnconfirmed(sourceBytes)
	}
	w, scoped := ctx.Value(terminalWindowKey{}).(*TerminalWindow)
	if !scoped {
		return c.ReserveNativeSourceUnconfirmed(sourceBytes)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if w == nil || w.coordinator != c || w.term != c.term || w.closed || c.stopped || c.poisoned != nil || c.manualUnavailable != nil {
		return nil, ErrReplicationUnavailable
	}
	if sourceBytes <= 0 || sourceBytes > MaxPayloadBytes || w.next >= w.count {
		return nil, resourcebudget.ErrExhausted
	}
	wireBytes := int64(4 + frameBodyFixedBytes + len(c.clusterID) + len(c.incarnation) + sourceBytes)
	a := &w.admissions[w.next]
	if !a.slot || a.moved || a.released || wireBytes > a.bytes {
		return nil, resourcebudget.ErrExhausted
	}
	permit, err := a.permit.ActivateCompletion()
	if err != nil {
		return nil, err
	}
	a.permit = permit
	if unused := a.bytes - wireBytes; unused > 0 {
		if err := a.permit.ReleasePart(resourcebudget.Cost{Unconfirmed: unused}); err != nil {
			return nil, err
		}
	}
	a.bytes = wireBytes
	w.next++
	return a, nil
}
