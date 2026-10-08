package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/akz142857/Halro/internal/ledger"
)

const (
	businessDrainUnstarted uint32 = iota
	businessDrainStarted
	businessDrainComplete
)

// Terminal close already withdrew fresh calls after Governance joined. Native
// Close must wake any missing-Apply predecessor before the final Manager join.
// Keep shared readers/Metadata/observers alive until that join really finishes.
func (r *Runtime) closeAccountingOwner() error {
	if r.selectedState != nil {
		// Direct Close can follow a healthy shutdown audit without Run's HTTP
		// drain. Join the actual owners before publishing that last derived
		// Metadata change. A previously failed drain is not upgraded by an
		// empty server list; keep its failure and physical ownership visible.
		var drainErr error
		switch r.backgroundWait.drainState.Load() {
		case businessDrainUnstarted:
			drainErr = r.drainAccountingBusiness(context.Background(), nil)
		case businessDrainStarted:
			drainErr = errors.New("selected terminal close has an incomplete business drain")
		}
		r.liveCheckpoint.job.Lock()
		defer r.liveCheckpoint.job.Unlock()
		// Terminal native Close wakes any incomplete predecessor wait before
		// joining Manager tickets. The common generation then joins every DAO
		// and root borrower, keeping persistent state for the next startup.
		r.usage.owner.retireMaintenance()
		checkpointErr := errors.Join(drainErr, r.checkpointSelectedState(context.Background()))
		closeEntered := false
		closeNative := func(ctx context.Context) error {
			closeEntered = true
			return r.ledger.CloseContext(ctx)
		}
		var nativeErr error
		if generation := r.selectedState.generation; generation != nil && generation.Owners.Directory != nil {
			// The checkpoint owner's original pool remains alive until every
			// native/common-root close check and actual descriptor join exits.
			nativeErr = generation.Owners.Directory.WithPhysicalCatchupWorkspace(context.Background(), closeNative)
		} else {
			nativeErr = closeNative(context.Background())
		}
		if !closeEntered {
			// A refused/poisoned workspace cannot skip the actual native join.
			// Unqualified physical state remains held by Close for recovery.
			nativeErr = errors.Join(nativeErr, closeNative(context.Background()))
		}
		stage := func(name string, err error) error {
			if err == nil {
				return nil
			}
			return fmt.Errorf("close selected accounting %s: %w", name, err)
		}
		err := errors.Join(stage("common checkpoint", checkpointErr), stage("native Ledger", nativeErr))
		err = errors.Join(err, stage("selected generation", r.selectedState.close()), stage("Usage owner", r.usage.closeOwner()), stage("accounting owner", r.accountingState.close()))
		return err
	}
	if r.replication != nil {
		// Startup confirmation/recovery borrows the same native/projection and
		// Metadata owners as business work. Join it before dependencies retire.
		r.replication.stopPrimaryCompletion()
	}
	err := r.accountingState.releaseStartupLease()
	// An applied receipt can still be waiting on ACK with an uncanceled caller
	// context, outside the native mutation lifetime. Terminal close must wake
	// that waiter before joining Manager tickets. Normal business drain keeps
	// confirmation running; only this final resource-close path stops it.
	if r.replication != nil && r.replication.coordinator != nil {
		r.replication.coordinator.Stop()
	}
	// A terminal/offline Close may run without Serve having finished. Session
	// callbacks can still use native stores and the selected projection owner.
	// Cancel and join transport before closing those dependencies.
	if r.replication != nil && r.replication.manager != nil {
		err = errors.Join(err, r.replication.manager.Close())
	}
	if r.usageCollector != nil {
		err = errors.Join(err, r.usageCollector.Retire(context.Background()))
	}
	if r.usage != nil {
		r.usage.owner.retireMaintenance()
	}
	if r.accountingState.replica != nil {
		err = errors.Join(err, r.accountingState.close()) // Transport is joined; retire the whole adapter before native Close.
	}
	err = errors.Join(err, r.ledger.Close())
	if r.accounting != nil {
		err = errors.Join(err, r.accounting.Retire(context.Background()))
	}
	err = errors.Join(err, r.usage.closeOwner(), r.accountingState.close())
	return err
}

// Keep worker registration, the one actual join handle and business-drain
// evidence in the same lifecycle owner. WaitGroup methods remain available
// for the Runtime's existing worker registrations; never copy this object.
type runtimeBackground struct {
	sync.WaitGroup
	summaries  usageSummaryLifecycle
	stopOnce   sync.Once
	stopped    chan struct{}
	drainState atomic.Uint32
}

// beginAccountingMaintenance transfers native mutation admission only after a
// successful whole-business drain. On failure the old business generation stays
// withdrawn; a new context can retry, but cannot reopen the retired Manager.
// This is not D-03 trust, an HA allowed-prefix decision or generation publication.
func (r *Runtime) beginAccountingMaintenance(ctx context.Context, servers []shutdownHTTPServer) (*ledger.MaintenanceLease, error) {
	if r.ledger == nil {
		return nil, errors.New("accounting maintenance requires a native Ledger")
	}
	if err := r.drainAccountingBusiness(ctx, servers); err != nil {
		return nil, err
	}
	lease, err := r.ledger.BeginMaintenance(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire drained accounting native ownership: %w", err)
	}
	return lease, nil
}

// drainAccountingBusiness is irreversible withdrawal, not publication of a
// replacement generation. Replication must remain available throughout it:
// accepted HTTP work and deferred cleanup may still need confirmed writes.
// A failed phase must not authorize native maintenance or a promotion promise.
func (r *Runtime) drainAccountingBusiness(ctx context.Context, servers []shutdownHTTPServer) error {
	r.backgroundWait.drainState.CompareAndSwap(businessDrainUnstarted, businessDrainStarted)
	r.draining.Store(true)
	r.governance.exports.withdraw()
	r.backgroundWait.summaries.withdraw()
	r.withdrawUsageReads()
	if r.replication != nil {
		r.replication.withdrawPrimaryCompletion()
	}
	if err := errors.Join(errors.Join(r.shutdownHTTPServers(ctx, servers)...), ctx.Err()); err != nil {
		return fmt.Errorf("drain business HTTP: %w", err)
	}
	if r.replication != nil {
		if err := r.replication.retirePrimaryCompletion(ctx); err != nil {
			return fmt.Errorf("join Primary startup work: %w", err)
		}
	}
	if err := r.governance.exports.drain(ctx); err != nil {
		return fmt.Errorf("join governance exports: %w", err)
	}
	if err := r.backgroundWait.summaries.drain(ctx); err != nil {
		return fmt.Errorf("join Usage Summary responses: %w", err)
	}
	if err := r.drainUsageReads(ctx); err != nil {
		return fmt.Errorf("join Usage query responses: %w", err)
	}
	if err := r.stopBackgroundWorkContext(ctx); err != nil {
		return fmt.Errorf("join business background producers: %w", err)
	}
	// Governance Report/export may still use the selected accounting and
	// Metadata owners. Join them before retiring those dependencies.
	if r.governance.manager != nil {
		if err := r.governance.manager.Retire(ctx); err != nil {
			return fmt.Errorf("retire drained Governance Manager: %w", err)
		}
	}
	// Collector.Run may exit before the last deferred settlement notification.
	// Reconcile the full durable suffix only after every producer has joined.
	if r.usageCollector != nil {
		if err := r.usageCollector.CatchUp(ctx); err != nil {
			return fmt.Errorf("catch up drained Usage: %w", err)
		}
	}
	if r.accounting != nil {
		if err := r.accounting.Retire(ctx); err != nil {
			return fmt.Errorf("retire drained accounting Manager: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.backgroundWait.drainState.Store(businessDrainComplete)
	return nil
}

// Cancellation requests permanent producer shutdown. The context bounds only
// this caller's wait; one Runtime-owned joiner continues to hold the resources
// until workers and alert callbacks really exit. Retrying waits on the same
// handle, and Close uses an unbounded join before closing files/vault/lock.
func (r *Runtime) stopBackgroundWorkContext(ctx context.Context) error {
	r.backgroundWait.stopOnce.Do(func() {
		r.backgroundWait.stopped = make(chan struct{})
		if r.backgroundCancel != nil {
			r.backgroundCancel()
		}
		go func() {
			r.backgroundWait.Wait()
			if r.alerts != nil {
				// Delivery callbacks append Audit records. Join them before
				// declaring the drained ordering prefix stable.
				r.alerts.Close()
			}
			close(r.backgroundWait.stopped)
		}()
	})
	select {
	case <-r.backgroundWait.stopped:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}
