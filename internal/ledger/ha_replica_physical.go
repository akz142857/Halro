package ledger

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// PrepareHAReplicaPhysicalCatchup reserves the original Replica's native and
// COW receive window while its independently qualified startup lease excludes
// transport. It grants neither confirmation nor permission to apply a frame.
func (p *AuthenticatedLedgerProjection) PrepareHAReplicaPhysicalCatchup(ctx context.Context, native *Log, lease *MaintenanceLease) error {
	if p == nil || native == nil || !native.options.Replica || lease == nil || !lease.readOnly || p.selectedNative.Load() != native {
		return ErrProjectionSource
	}
	if err := p.withFullHAPhysicalMaintenanceSource(ctx, native, lease, func(ctx context.Context, source AuthenticatedHAPhysicalUsageSource) error {
		return source.Check(ctx)
	}, true); err != nil {
		return err
	}
	w, err := native.ReservePhysicalTerminals(ctx, resourcebudget.MaxCompletionBatch)
	if err != nil {
		return err
	}
	native.mu.Lock()
	if p.physicalReplica != nil {
		native.mu.Unlock()
		return errors.Join(ErrProjectionSource, w.Close())
	}
	p.physicalReplica = w
	native.mu.Unlock()
	return nil
}

// Called after actual MAC verification and before the original Replica writes
// native bytes. Every record must have its own still-unused preallocated root.
func (l *Log) admitPhysicalReplicaLocked(records uint64, bytes int64) error {
	o := l.physical.Load()
	if o == nil {
		return nil
	}
	w := o.projection.physicalReplica
	if !l.options.Replica || w == nil || w.native != l || w.closed || records == 0 || records > uint64(w.remaining-w.replicaReceived) || bytes <= 0 || bytes > int64(records)*terminalNativePhysicalBytes || bytes > o.allocatedEnd-l.offset {
		return resourcebudget.ErrPhysicalAllocation
	}
	return l.checkPhysicalNativeLocked()
}

func (l *Log) holdPhysicalReplicaFailureLocked(cause error) error {
	o := l.physical.Load()
	if o == nil {
		return cause
	}
	err := errors.Join(authenticatedstate.ErrRecovery, cause, o.projection.store.HoldPhysicalPreparationForRecovery(context.Background()))
	o.failure = errors.Join(o.failure, err)
	l.status.RequireRecovery()
	return err
}

// Close joins only the original window, after all received records have really
// applied. A maintenance lease excludes new receive; no counters supplied by a
// caller can forgive an accepted native suffix.
func (p *AuthenticatedLedgerProjection) CloseHAReplicaPhysicalCatchup(ctx context.Context, native *Log, lease *MaintenanceLease) error {
	if p == nil || native == nil || !native.options.Replica || lease == nil || !lease.readOnly || p.selectedNative.Load() != native {
		return ErrProjectionSource
	}
	if _, err := lease.ValidateOwner(native); err != nil {
		return err
	}
	native.mu.Lock()
	w := p.physicalReplica
	if w == nil {
		native.mu.Unlock()
		return nil
	}
	o := native.physical.Load()
	if w.replicaReceived != 0 || w.active != 0 || w.claimed != 0 || o == nil || o.failure != nil || native.status.Load() != AccountingHealthy {
		native.mu.Unlock()
		return authenticatedstate.ErrRecovery
	}
	native.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	native.mu.Lock()
	p.physicalReplica = nil
	native.mu.Unlock()
	return nil
}
