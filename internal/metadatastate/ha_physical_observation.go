package metadatastate

import (
	"context"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// HAReplicaPhysicalObservation is evidence about an original paid bank, never
// an independent selection, native write permission or confirmation authority.
type HAReplicaPhysicalObservation struct {
	Window          authenticatedstate.PhysicalTerminalWindowObservation
	Native          authenticatedstate.PhysicalNativeReference
	AvailableFrames uint32
	AvailableBytes  int64
}

func (b *Baseline) ObserveHAReplicaPhysicalEscrow(ctx context.Context) (out HAReplicaPhysicalObservation, resultErr error) {
	if b == nil || !b.mu.TryLock() {
		return out, authenticatedstate.ErrBusy
	}
	s := b.haPhysicalSource
	if s == nil || s.role != authenticatedstate.SealReplica || !b.physicalEnabled || !b.ready.Load() || s.closing || s.receiveWindow == nil || s.receiveWindow.closed.Load() || s.receiveWindow.completion == nil {
		b.mu.Unlock()
		return out, ErrSource
	}
	w := s.receiveWindow
	s.receiveJobs.Add(1)
	b.mu.Unlock()
	defer s.receiveJobs.Done()
	job, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(b.ctx, cancel)
	defer func() { stop(); cancel() }()
	// Acquire the original workspace before the Baseline mutex, matching receive
	// and replay order. No newly allocated RAM can masquerade as this owner.
	resultErr = resourcebudget.WithCompletionWorkspace(w.completion.Bind(job), func(ctx context.Context) error {
		if !b.mu.TryLock() {
			return authenticatedstate.ErrBusy
		}
		defer b.mu.Unlock()
		if b.haPhysicalSource != s || s.receiveWindow != w || s.closing || !b.ready.Load() || w.closed.Load() || w.closeErr != nil || w.owner != b || w.remaining < 0 {
			return ErrSource
		}
		var err error
		out.Window, err = w.cow.PreparedObservation(ctx)
		if err != nil {
			return err
		}
		if out.Window.UnusedRoots != uint32(w.remaining) {
			return authenticatedstate.ErrRecovery
		}
		return b.withTerminalPhysicalNativeLocked(ctx, func(q *metadatajournal.PhysicalNativeLoan) error {
			out.Native, err = q.Reference()
			if err != nil {
				return err
			}
			end, err := q.PreparedEnd()
			if err != nil {
				return err
			}
			if end != out.Window.NativeEnd || out.Native.Sequence < b.position.Sequence || out.Native.Sequence-b.position.Sequence > uint64(w.remaining) {
				return authenticatedstate.ErrRecovery
			}
			if err := q.AuthenticatePrefix(ctx, out.Window.Native); err != nil {
				return err
			}
			out.AvailableFrames = uint32(uint64(w.remaining) - (out.Native.Sequence - b.position.Sequence))
			if err := q.VerifyFutureCapacity(int(out.AvailableFrames)); err != nil {
				return err
			}
			out.AvailableBytes = end - out.Native.EOF
			return ctx.Err()
		})
	})
	if resultErr != nil {
		out = HAReplicaPhysicalObservation{}
	}
	return out, resultErr
}
