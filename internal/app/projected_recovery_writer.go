package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// projectedRecoveryWriter is a maintenance-only native/projection/confirmation
// adapter. Default startup uses a fresh authenticated projection while business
// and observers are not yet admitted. Maintenance callers must first drain
// all business/accounting/observer work and establish permitted-prefix trust;
// draining=true is a required withdrawal signal, not proof of that drain.
// It publishes neither a generation nor business readiness.
type projectedRecoveryWriter struct {
	runtime    *Runtime
	projection recoveryProjection
	lease      *ledger.MaintenanceLease
	replica    *replicationRuntime
	authority  replication.RecoveryTarget
	role       budget.RecoveryRole
	ctx        context.Context
	cancel     context.CancelFunc
	gate       chan struct{}
	failure    error // protected by gate
	// Only the private selected HA startup adapter borrows a lease whose
	// generation owner must keep it through Usage catch-up and final Ready gates.
	borrowedLease bool
}

var _ budget.RecoveryWriter = (*projectedRecoveryWriter)(nil)

type recoveryProjection interface {
	ReadPosition(context.Context) (ledger.Watermark, [32]byte, error)
	Apply(context.Context, ledger.Record) error
}

func (r *Runtime) newProjectedRecoveryWriter(ctx context.Context, lease *ledger.MaintenanceLease, projection *ledger.LedgerProjection) (*projectedRecoveryWriter, error) {
	if projection == nil {
		return nil, errors.New("projected recovery requires a projection")
	}
	return r.newRecoveryProjectionWriter(ctx, lease, projection)
}

func (r *Runtime) newRecoveryProjectionWriter(ctx context.Context, lease *ledger.MaintenanceLease, projection recoveryProjection) (*projectedRecoveryWriter, error) {
	if r == nil || r.ledger == nil || projection == nil || !r.draining.Load() {
		return nil, errors.New("projected recovery requires withdrawn business admission and native/projection owners")
	}
	replicated, err := lease.ValidateOwner(r.ledger)
	if err != nil {
		return nil, err
	}
	if replicated != (r.config.Replication != nil) || replicated != (r.replication != nil) {
		return nil, errors.New("native/config/runtime replication mode differs; cannot use Standalone confirmation")
	}
	if binding, ok := projection.(authenticatedRecoveryProjection); ok {
		if err := binding.ValidateNativeWriter(ctx, r.ledger, lease); err != nil {
			return nil, err
		}
	}
	wCtx, cancel := context.WithCancel(context.Background())
	w := &projectedRecoveryWriter{runtime: r, projection: projection, lease: lease, replica: r.replication,
		role: budget.RecoveryStandalone, ctx: wCtx, cancel: cancel, gate: make(chan struct{}, 1)}
	if replicated {
		w.role = budget.RecoveryPrimary
		if w.replica.role != replication.RolePrimary || w.replica.publisher == nil || w.replica.coordinator == nil {
			cancel()
			return nil, errors.New("projected recovery requires the live Primary runtime")
		}
	}
	err = w.withHead(ctx, func(ctx context.Context, head ledger.Watermark, hash [32]byte) error {
		if err := w.verifyApplied(ctx, head, hash); err != nil {
			return err
		}
		if !replicated {
			return ctx.Err()
		}
		target, err := w.captureRecoveryTarget(ctx, head)
		if err != nil {
			return err
		}
		// This token remains the writer's authority for its entire lifetime.
		// Later batches capture new targets only for confirmation, never to
		// reauthorize a writer after withdrawal/stepdown/restore.
		w.authority = target
		return w.authorize(ctx)
	})
	if err != nil {
		cancel()
		return nil, err
	}
	return w, nil
}

func (w *projectedRecoveryWriter) withHead(ctx context.Context, visit func(context.Context, ledger.Watermark, [32]byte) error) error {
	if _, ha := w.projection.(haSelectedRecoveryProjection); ha {
		return w.lease.WithHASelectedRecoveryHead(ctx, visit)
	}
	return w.lease.WithHead(ctx, visit)
}

func (w *projectedRecoveryWriter) enter(ctx context.Context) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	waitCtx, stopWait := resourcebudget.WorkQueueContext(ctx)
	defer stopWait()
	select {
	case w.gate <- struct{}{}:
	case <-waitCtx.Done():
		return nil, nil, errors.Join(waitCtx.Err(), context.Cause(waitCtx))
	case <-w.ctx.Done():
		return nil, nil, errors.New("projected recovery writer is closed")
	}
	if waitCtx.Err() != nil {
		<-w.gate
		return nil, nil, errors.Join(waitCtx.Err(), context.Cause(waitCtx))
	}
	if w.failure != nil || w.ctx.Err() != nil {
		<-w.gate
		return nil, nil, errors.New("projected recovery writer is closed or requires fresh recovery")
	}
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(w.ctx, cancel)
	return opCtx, func() { stop(); cancel(); <-w.gate }, nil
}

func (w *projectedRecoveryWriter) fail(err error) error {
	if err != nil {
		w.failure = err
		w.runtime.ledger.Status().RequireRecovery()
	}
	return err
}

func (w *projectedRecoveryWriter) authorize(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	replicated, err := w.lease.ValidateOwner(w.runtime.ledger)
	if err != nil || !w.runtime.draining.Load() || w.runtime.replication != w.replica || replicated != (w.role == budget.RecoveryPrimary) || replicated != (w.runtime.config.Replication != nil) {
		return errors.Join(err, errors.New("maintenance owner/mode/admission was revoked"))
	}
	if w.role == budget.RecoveryStandalone {
		return nil
	}
	state := w.replica.publisher.Snapshot()
	prefix := w.authority.Evidence()
	if w.replica.role != replication.RolePrimary || state.Role != replication.RolePrimary || state.ClusterID != prefix.ClusterID || state.Incarnation != prefix.Incarnation || state.Term != prefix.Term || state.PromisedTerm != prefix.Term || state.NodeID != w.runtime.config.Replication.NodeID {
		return errors.New("maintenance Primary role/term/identity was revoked")
	}
	return w.waitRecoveryTarget(ctx, w.authority)
}

// Only the selected startup adapter owns a withdrawn producer lane and joined
// maintenance lease. Ordinary recovery retains immediate peer refusal.
func (w *projectedRecoveryWriter) captureRecoveryTarget(ctx context.Context, head ledger.Watermark) (replication.RecoveryTarget, error) {
	position := replication.StoreCursor{Generation: head.Generation, Sequence: head.Sequence}
	if _, selected := w.projection.(haSelectedRecoveryProjection); selected {
		return w.replica.coordinator.CaptureRecoveryTargetWhenAvailable(ctx, position)
	}
	return w.replica.coordinator.CaptureRecoveryTarget(ctx, position)
}

func (w *projectedRecoveryWriter) waitRecoveryTarget(ctx context.Context, target replication.RecoveryTarget) error {
	if _, selected := w.projection.(haSelectedRecoveryProjection); selected {
		return w.replica.coordinator.WaitRecoveryTargetWithPeer(ctx, target)
	}
	return w.replica.coordinator.WaitRecoveryTarget(ctx, target)
}

func (w *projectedRecoveryWriter) verifyApplied(ctx context.Context, head ledger.Watermark, hash [32]byte) error {
	actual, actualHash, err := w.projection.ReadPosition(ctx)
	if err != nil || actual != head || actualHash != hash {
		return errors.Join(err, errors.New("maintenance applied position/hash differs from durable native head"))
	}
	return nil
}

func (w *projectedRecoveryWriter) AppendApplyRecovery(ctx context.Context, event ledger.Event) (ledger.Record, error) {
	opCtx, release, err := w.enter(ctx)
	if err != nil {
		return ledger.Record{}, err
	}
	defer release()
	if err := w.withHead(opCtx, func(ctx context.Context, head ledger.Watermark, hash [32]byte) error {
		if err := w.authorize(ctx); err != nil {
			return err
		}
		return w.verifyApplied(ctx, head, hash)
	}); err != nil {
		return ledger.Record{}, w.fail(err)
	}
	var record ledger.Record
	var receipt ledger.AppendReceipt
	appendRecord := func(appendCtx context.Context) error {
		var err error
		record, receipt, err = w.lease.AppendApply(appendCtx, event, func(ctx context.Context, r ledger.Record) error {
			if err := w.authorize(ctx); err != nil {
				return err
			}
			return w.projection.Apply(ctx, r)
		})
		return err
	}
	if projection, ok := w.projection.(interface {
		withAppendWorkspace(context.Context, func(context.Context) error) error
	}); ok {
		err = projection.withAppendWorkspace(opCtx, appendRecord)
	} else {
		err = appendRecord(opCtx)
	}
	if err == nil && (receipt.Watermark != (ledger.Watermark{Generation: record.Generation, Offset: record.Offset, Sequence: record.Sequence}) || (w.role == budget.RecoveryPrimary) != (receipt.ReplicationIndex != 0)) {
		err = errors.New("maintenance native receipt does not match actual Record or role")
	}
	if err == nil {
		err = w.authorize(opCtx)
	}
	if err != nil {
		return ledger.Record{}, w.fail(err)
	}
	return record, nil
}

func (w *projectedRecoveryWriter) ConfirmRecoveryPrefix(ctx context.Context, expected ledger.Watermark) error {
	opCtx, release, err := w.enter(ctx)
	if err != nil {
		return err
	}
	defer release()
	err = w.withHead(opCtx, func(ctx context.Context, head ledger.Watermark, hash [32]byte) error {
		if head != expected {
			return errors.New("recovery confirmation target is not the applied native cut")
		}
		if err := w.authorize(ctx); err != nil {
			return err
		}
		if err := w.verifyApplied(ctx, head, hash); err != nil {
			return err
		}
		if w.role == budget.RecoveryStandalone {
			return ctx.Err()
		}
		target, err := w.captureRecoveryTarget(ctx, head)
		if err != nil {
			return err
		}
		if err := w.waitRecoveryTarget(ctx, target); err != nil {
			return fmt.Errorf("confirm full maintenance ordering target: %w", err)
		}
		return w.authorize(ctx)
	})
	return w.fail(err)
}

// Close stops and joins this adapter before its native lease may be released.
// The owner must also join the full recovery algorithm before closing its DB.
func (w *projectedRecoveryWriter) Close() error {
	w.cancel()
	// Retain native exclusivity until post-append authorization/failure checks
	// also exit. Releasing the lease first would admit ordinary mutations before
	// an interrupted operation has latched recovery-required.
	w.gate <- struct{}{}
	var err error
	if !w.borrowedLease {
		err = w.lease.Close()
	}
	<-w.gate
	return err
}
