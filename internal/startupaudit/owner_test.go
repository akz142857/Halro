package startupaudit

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestExpiredSnapshotCannotBeRevivedByLaterProgressOrCompletion(t *testing.T) {
	entered, leave := make(chan struct{}), make(chan struct{})
	a := Start(context.Background(), Options{Deadline: time.Minute, ProgressTimeout: 30 * time.Second}, nil, func(_ context.Context, progress func(uint64, int64)) error {
		progress(1, 1)
		close(entered)
		<-leave
		// A real reader may ignore cancellation until an actual OS read exits.
		progress(2, 2)
		return nil
	})
	<-entered
	// Place the already authenticated progress before the finite deadline,
	// without waiting for the monitor's one-second tick. This reproduces an
	// admission Snapshot observing expiry before the worker updates progress.
	a.mu.Lock()
	a.last = time.Now().Add(-31 * time.Second)
	a.mu.Unlock()
	if p := a.Snapshot(); p.Ready || !errors.Is(p.Err, ErrProgress) {
		t.Fatal(p)
	}
	close(leave)
	if err := a.Wait(context.Background()); !errors.Is(err, ErrProgress) {
		t.Fatal("expired owner revived", err)
	}
	if p := a.Snapshot(); p.Ready || p.Complete || p.Records != 1 {
		t.Fatal(p)
	}
}
