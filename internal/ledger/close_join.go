package ledger

import (
	"context"
	"errors"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
)

// nativeCloseJoinRetirement retains the original closed admission owner. A
// timed-out join proves no descriptor exit and cannot refund its obligations.
type nativeCloseJoinRetirement struct {
	log   *Log
	cause error
}

func (e *nativeCloseJoinRetirement) Error() string { return e.cause.Error() }
func (e *nativeCloseJoinRetirement) Unwrap() error { return e.cause }

func waitNativeCloseJoin(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		// Prefer an actually completed original join over simultaneous expiry.
		select {
		case <-done:
			return nil
		default:
			return ctx.Err()
		}
	}
}

func (l *Log) retainUnknownCloseJoin(cause error) error {
	l.closeJoinMu.Lock()
	defer l.closeJoinMu.Unlock()
	if l.closeJoinErr == nil {
		l.closeJoinErr = &nativeCloseJoinRetirement{log: l, cause: errors.Join(ErrSegmentRetirement, ownedfile.ErrRetirement, cause)}
	}
	l.status.RequireRecovery()
	return l.closeJoinErr
}

func (l *Log) joinCloseMutations(ctx context.Context) error {
	l.admission.Lock()
	count, drain := l.mutating, l.drain
	l.admission.Unlock()
	// Closed admission cannot introduce a new mutation after this observation.
	if count == 0 {
		return nil
	}
	return waitNativeCloseJoin(ctx, drain)
}

func (l *Log) lockNativeClose(ctx context.Context) error {
	return lockNativeCloseMutex(ctx, &l.mu)
}

func lockNativeCloseMutex(ctx context.Context, lock interface{ TryLock() bool }) error {
	if lock.TryLock() {
		return nil
	}
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			if lock.TryLock() {
				return nil
			}
		}
	}
}
