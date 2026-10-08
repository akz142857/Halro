package governance

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/nativephysical"
)

type HAReplicaPhysicalObservation struct {
	Native          nativephysical.PreparedObservation
	Window          authenticatedstate.PhysicalTerminalWindowObservation
	AvailableFrames uint32
}

func (p *GovernanceProjection) ObserveHAReplicaPhysicalProjection(ctx context.Context) (out HAReplicaPhysicalObservation, resultErr error) {
	if p == nil || p.cowNative == nil {
		return out, ErrStartupCache
	}
	n := p.cowNative
	if !n.mu.TryLock() {
		return out, nativephysical.ErrBusy
	}
	defer n.mu.Unlock()
	if !p.mu.TryRLock() {
		return out, nativephysical.ErrBusy
	}
	defer p.mu.RUnlock()
	q := p.physical
	if !n.replica || n.startupProvenance == nil || n.startupProvenance.ha == nil || n.physical == nil || n.closing || n.closed || n.terminalErr != nil || !p.ready || p.retired.Load() || p.cowClosed || q == nil || n.physicalProjection != q || q.projection != p || q.native != n || q.store != p.cow {
		return out, ErrUnavailable
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.failure != nil || q.window == nil || q.pending < 0 || q.remaining < q.pending || n.sequence != q.applied.Records+uint64(q.pending) {
		return out, ErrUnavailable
	}
	head := n.physicalHeadLocked()
	if err := n.authenticatePhysicalPrefixLocked(ctx, head); err != nil {
		return out, err
	}
	var err error
	out.Native, err = n.physical.ObservePrepared(ctx, head)
	if err != nil {
		return out, err
	}
	out.Window, err = q.window.PreparedObservation(ctx)
	if err != nil {
		return HAReplicaPhysicalObservation{}, err
	}
	if out.Window.Selection != q.selection || out.Window.UnusedRoots != uint32(q.remaining) || out.Window.NativeEnd != out.Native.Native.End {
		return HAReplicaPhysicalObservation{}, authenticatedstate.ErrRecovery
	}
	if err := n.verifyPhysicalGovernancePrefixLocked(ctx, out.Window.Native, out.Window.NativeEnd); err != nil {
		return HAReplicaPhysicalObservation{}, err
	}
	out.AvailableFrames = uint32(q.remaining - q.pending)
	resultErr = errors.Join(ctx.Err(), q.failure)
	if resultErr != nil {
		out = HAReplicaPhysicalObservation{}
	}
	return out, resultErr
}
