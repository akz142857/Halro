package ledger

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// HAReplicaPhysicalObservation describes one original paid receive bank. It
// cannot select a root or grant permission to receive, confirm or apply.
type HAReplicaPhysicalObservation struct {
	Window           authenticatedstate.PhysicalTerminalWindowObservation
	Native           authenticatedstate.PhysicalNativeReference
	AvailableRecords uint32
	AvailableBytes   int64
}

// ObserveHAReplicaPhysicalCatchup authenticates the actual bank and its native
// prefix while the original native mutex excludes receive and Apply. Received
// but unapplied records remain obligations and are subtracted from spare roots.
func (p *AuthenticatedLedgerProjection) ObserveHAReplicaPhysicalCatchup(ctx context.Context, native *Log) (out HAReplicaPhysicalObservation, resultErr error) {
	if p == nil || native == nil || !native.options.Replica || p.selectedNative.Load() != native {
		return out, ErrProjectionSource
	}
	if !native.mu.TryLock() {
		return out, authenticatedstate.ErrBusy
	}
	defer native.mu.Unlock()
	if !p.mu.TryRLock() {
		return out, authenticatedstate.ErrBusy
	}
	defer p.mu.RUnlock()
	o := native.physical.Load()
	w := p.physicalReplica
	if !p.ready || p.closeContext.Err() != nil || native.startupProvenance == nil || native.startupProvenance.ha == nil || o == nil || o.projection != p || o.failure != nil || w == nil || w.native != native || w.closed || w.closeErr != nil || w.active != 0 || w.claimed != 0 || w.remaining < w.replicaReceived || w.replicaReceived < 0 {
		return out, authenticatedstate.ErrRecovery
	}
	if err := errors.Join(ctx.Err(), native.checkPhysicalNativeLocked()); err != nil {
		return out, err
	}
	work, err := p.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: AuthenticatedLedgerSourceRAMBytes})
	if err != nil {
		return out, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, work.Release())
		if resultErr != nil {
			out = HAReplicaPhysicalObservation{}
		}
	}()
	out.Window, err = w.cow.PreparedObservation(ctx)
	if err != nil {
		return out, err
	}
	out.Native, err = native.physicalNativeReferenceLocked()
	if err != nil {
		return out, err
	}
	if out.Window.Selection != o.selection || out.Window.UnusedRoots != uint32(w.remaining) || out.Window.NativeEnd != o.allocatedEnd {
		return out, authenticatedstate.ErrRecovery
	}
	if err := directoryVerifyPhysicalPrefixLocked(ctx, native, out.Native, out.Window.Native, out.Window.NativeEnd); err != nil {
		return out, err
	}
	if o.allocatedEnd > native.offset {
		if err := resourcebudget.CheckPhysicalRange(native.file, native.offset, o.allocatedEnd); err != nil {
			return out, err
		}
	}
	out.AvailableRecords = uint32(w.remaining - w.replicaReceived)
	out.AvailableBytes = o.allocatedEnd - native.offset
	return out, errors.Join(ctx.Err(), native.checkPhysicalNativeLocked())
}
