package ledger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

func TestTerminalPhysicalWindowConcurrentCloseJoinsSameExit(t *testing.T) {
	l := &Log{status: NewStatus(), closeContext: context.Background()}
	l.physical.Store(&nativePhysicalOwner{})
	child := make(chan struct{})
	w := &TerminalPhysicalWindow{native: l, active: 1, done: child}
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- w.Close() }()
	deadline := time.Now().Add(time.Second)
	for {
		l.mu.Lock()
		closing := w.closed
		l.mu.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Close did not withdraw")
		}
		time.Sleep(time.Millisecond)
	}
	go func() { second <- w.Close() }()
	select {
	case err := <-first:
		t.Fatal("first Close guessed child exit", err)
	case err := <-second:
		t.Fatal("repeated Close skipped actual child", err)
	case <-time.After(20 * time.Millisecond):
	}
	l.mu.Lock()
	w.active = 0
	close(child)
	l.mu.Unlock()
	for _, result := range []chan error{first, second} {
		select {
		case err := <-result:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("Close failed to join actual exit")
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPhysicalDerivedCloseRequiresBorrowerMarkerAfterNativeFDExit(t *testing.T) {
	for _, qualified := range []bool{false, true} {
		t.Run(map[bool]string{false: "unselected", true: "selected"}[qualified], func(t *testing.T) {
			l := &Log{status: NewStatus(), closeContext: context.Background(), ioClosed: true}
			o := &nativePhysicalOwner{allocatedEnd: 8192}
			l.physical.Store(o)
			calls := 0
			o.derived[0] = func(ctx context.Context, actual *Log, ref authenticatedstate.PhysicalNativeReference, end int64, stage PhysicalDerivedStage, future int) error {
				calls++
				if actual != l || ref != (authenticatedstate.PhysicalNativeReference{}) || end != 0 || stage != PhysicalDerivedClose || future != 0 {
					t.Fatal("Close carried source IO authority")
				}
				if err := actual.TransferPhysicalDerivedNativeReceipt(ctx, nil); !errors.Is(err, ErrProjectionSource) {
					t.Fatal("Close manufactured a successor grant", err)
				}
				if !qualified {
					return authenticatedstate.ErrRecovery
				}
				return nil
			}
			l.mu.Lock()
			err := l.preparePhysicalDerivedLocked(context.Background(), PhysicalDerivedClose, 0)
			l.mu.Unlock()
			if calls != 1 || qualified && err != nil || !qualified && !errors.Is(err, authenticatedstate.ErrRecovery) {
				t.Fatal("Close skipped or guessed actual borrower qualification", calls, err)
			}
		})
	}
}
