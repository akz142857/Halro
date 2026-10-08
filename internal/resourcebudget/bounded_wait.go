package resourcebudget

import (
	"context"
	"errors"
	"time"
)

// MaxWorkQueueWait limits queue residence even when a caller has no deadline.
// It does not shorten an entered callback, cancel a physical owner, or refund
// an accepted job's original completion credit.
const MaxWorkQueueWait = 30 * time.Second

var ErrWorkQueueWait = errors.New("resource work queue wait limit exceeded")

// WorkQueueContext is used only for waiting, never for entered IO/callbacks.
// Its cause distinguishes the fixed wait cap from an earlier caller deadline.
func WorkQueueContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeoutCause(ctx, MaxWorkQueueWait, ErrWorkQueueWait)
}

func workQueueWaitError(caller, waiting context.Context) error {
	if err := caller.Err(); err != nil {
		return err
	}
	return errors.Join(ErrWorkQueueWait, waiting.Err())
}

// AcquireWorkLane enters an already bounded and paid single-owner lane.
// Callers own the waiter count/fee, and remove the lane token after actual work
// and children join. The finite timer ends on admission; work uses caller ctx.
func AcquireWorkLane(ctx context.Context, lane chan struct{}) error {
	if ctx == nil || cap(lane) != 1 {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	waiting, stop := WorkQueueContext(ctx)
	defer stop()
	select {
	case lane <- struct{}{}:
		if waiting.Err() != nil {
			<-lane
			return workQueueWaitError(ctx, waiting)
		}
		return nil
	case <-waiting.Done():
		return workQueueWaitError(ctx, waiting)
	}
}
