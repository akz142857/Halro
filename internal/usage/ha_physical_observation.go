package usage

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// HAReplicaPhysicalObservation describes the original fixed Usage banks and
// subtracts the durable source suffix that still owes a Usage reduction.
// It grants neither selection, native acceptance nor Applied permission.
type HAReplicaPhysicalObservation struct {
	Windows         [resourcebudget.MaxCompletionBatch]authenticatedstate.PhysicalTerminalWindowObservation
	WindowCount     uint32
	Native          authenticatedstate.PhysicalNativeReference
	AvailableFrames uint32
}

func (p *DiskProjection) ObserveHAReplicaPhysicalCatchup(ctx context.Context, native *ledger.Log, owner *ledger.AuthenticatedLedgerProjection) (out HAReplicaPhysicalObservation, resultErr error) {
	if p == nil || native == nil || owner == nil || !p.mu.TryRLock() {
		return out, authenticatedstate.ErrBusy
	}
	defer p.mu.RUnlock()
	q, e := p.physical, p.engine
	if p.retired.Load() || q == nil || e == nil || !e.selectedHA || e.native != native || e.owner != owner || q.native != native || q.ledger != owner || q.store != e.cow || !q.ha || !q.installed || q.work == nil {
		return out, authenticatedstate.ErrRecovery
	}
	resultErr = p.withHAPhysicalSourceLocked(ctx, func(ctx context.Context, source ledger.AuthenticatedPhysicalUsageSource) error {
		ha, ok := source.(ledger.AuthenticatedHAPhysicalUsageSource)
		if !ok {
			return authenticatedstate.ErrRecovery
		}
		if err := ha.VerifyHAReplicaPhysicalSource(ctx); err != nil {
			return err
		}
		// The genuine source callback holds native.mu; do not reacquire Usage.mu.
		q.mu.Lock()
		defer q.mu.Unlock()
		if q.failure != nil {
			return q.failure
		}
		var err error
		out.Native, err = source.PhysicalNativeReference(ctx)
		if err != nil {
			return err
		}
		end, err := source.PhysicalNativePreparedEnd(ctx)
		if err != nil {
			return err
		}
		if out.Native.Sequence < q.applied.Head.Sequence || out.Native.Sequence-q.applied.Head.Sequence > uint64(q.remaining) {
			return authenticatedstate.ErrRecovery
		}
		var roots uint32
		var first *authenticatedstate.PhysicalTerminalWindowObservation
		for i, w := range q.windows {
			if w == nil {
				if q.counts[i] != 0 {
					return authenticatedstate.ErrRecovery
				}
				continue
			}
			obs, err := w.PreparedObservation(ctx)
			if err != nil {
				return err
			}
			if obs.Selection != q.selection || obs.NativeEnd != end || obs.UnusedRoots != uint32(q.counts[i]) {
				return authenticatedstate.ErrRecovery
			}
			out.Windows[out.WindowCount] = obs
			out.WindowCount++
			roots += obs.UnusedRoots
			if first == nil {
				first = &out.Windows[out.WindowCount-1]
				if err := source.VerifyPhysicalNativePrefix(ctx, obs.Native, obs.NativeEnd); err != nil {
					return err
				}
			} else if obs.Native != first.Native || obs.Controls != first.Controls || obs.COWEnd != first.COWEnd || obs.Pack != first.Pack {
				return authenticatedstate.ErrRecovery
			}
		}
		if roots != uint32(q.remaining) || first == nil {
			return authenticatedstate.ErrRecovery
		}
		out.AvailableFrames = uint32(uint64(q.remaining) - (out.Native.Sequence - q.applied.Head.Sequence))
		return errors.Join(source.Check(ctx), ctx.Err())
	})
	if resultErr != nil {
		out = HAReplicaPhysicalObservation{}
	}
	return out, resultErr
}
