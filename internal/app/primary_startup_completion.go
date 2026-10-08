package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/replication"
)

// One completion worker belongs to the HA runtime. Confirmation transport stays
// alive while it runs; shutdown cancels and joins it before closing dependencies.
// The optional recovery continuation is installed before any peer session starts.
func (r *replicationRuntime) startPrimaryCompletion(target uint64) {
	r.startupMu.Lock()
	if r.startupClosed || r.startupDone != nil {
		r.startupMu.Unlock()
		return
	}
	if r.startupCtx == nil || r.coordinator == nil || r.publisher == nil {
		r.startupMu.Unlock()
		r.fail(errors.New("Primary startup completion requires owned context/coordinator/member"))
		return
	}
	before := r.publisher.Snapshot()
	if before.Role != replication.RolePrimary || before.Term != before.PromisedTerm {
		r.startupMu.Unlock()
		r.fail(errors.New("Primary startup completion requires current role and promised term"))
		return
	}
	done := make(chan struct{})
	r.startupDone = done
	ctx, finish := r.startupCtx, r.startupFinish
	r.startupMu.Unlock()
	go func() {
		defer close(done)
		if err := r.coordinator.WaitRecoveryAvailability(ctx); err != nil {
			if !errors.Is(err, context.Canceled) {
				r.fail(fmt.Errorf("await Primary startup peer window: %w", err))
			}
			return
		}
		if err := r.coordinator.WaitConfirmed(ctx, target); err != nil {
			if !errors.Is(err, context.Canceled) {
				r.fail(fmt.Errorf("confirm Primary startup prefix through %d: %w", target, err))
			}
			return
		}
		if err := r.coordinator.WaitRecoveryAvailability(ctx); err != nil {
			if !errors.Is(err, context.Canceled) {
				r.fail(fmt.Errorf("await Primary recovery peer window: %w", err))
			}
			return
		}
		if !samePrimaryStartupAuthority(before, r.publisher.Snapshot()) {
			r.fail(errors.New("Primary startup role/term/identity changed before recovery"))
			return
		}
		if finish != nil {
			if err := finish(ctx); err != nil {
				if !errors.Is(err, context.Canceled) {
					r.fail(fmt.Errorf("finish Primary startup recovery: %w", err))
				}
				return
			}
		}
		// Share the publication lock with permanent retirement: Close cannot
		// withdraw readiness only to have this worker publish it again later.
		r.startupMu.Lock()
		_, available, cause := r.coordinator.Status()
		valid := !r.startupClosed && ctx.Err() == nil && available && samePrimaryStartupAuthority(before, r.publisher.Snapshot())
		if valid {
			r.startupReady.Store(true)
			if r.startupProducers != nil {
				close(r.startupProducers)
			}
		}
		r.startupMu.Unlock()
		if !valid && ctx.Err() == nil {
			r.fail(errors.Join(errors.New("Primary startup authority changed before readiness publication"), cause))
		}
	}()
}

func samePrimaryStartupAuthority(a, b replication.MemberState) bool {
	return a.Role == replication.RolePrimary && b.Role == a.Role && b.ClusterID == a.ClusterID && b.Incarnation == a.Incarnation && b.NodeID == a.NodeID && b.Term == a.Term && b.PromisedTerm == a.PromisedTerm
}

func (r *replicationRuntime) withdrawPrimaryCompletion() <-chan struct{} {
	if r == nil {
		return nil
	}
	r.startupMu.Lock()
	r.startupClosed = true
	r.startupReady.Store(false)
	if r.startupStop != nil {
		r.startupStop()
	}
	done := r.startupDone
	r.startupMu.Unlock()
	return done
}

func (r *replicationRuntime) retirePrimaryCompletion(ctx context.Context) error {
	done := r.withdrawPrimaryCompletion()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (r *replicationRuntime) stopPrimaryCompletion() {
	_ = r.retirePrimaryCompletion(context.Background())
}
