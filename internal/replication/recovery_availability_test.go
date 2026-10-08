package replication

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRecoveryAvailabilityConfirmedPrefixStillWaitsForActualPeer(t *testing.T) {
	j, source := windowHistory(t, 1, nil)
	c := windowPrimary(t, j, source, 1, DefaultPrimaryWindowLimits(), nil)
	if err := c.WaitConfirmed(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.WaitRecoveryAvailability(ctx) }()
	select {
	case err := <-done:
		t.Fatal("confirmed baseline invented peer availability", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := c.PreparePeer("halro-1", 1, 1, (&windowSender{}).send); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, available, err := c.Status(); !available || err != nil {
		t.Fatal(available, err)
	}
}

func TestRecoveryAvailabilityCancelAndPermanentFailure(t *testing.T) {
	for _, name := range []string{"cancel", "stop", "manual-peer-unavailable", "source-failure"} {
		t.Run(name, func(t *testing.T) {
			j, source := windowHistory(t, 1, nil)
			c := windowPrimary(t, j, source, 1, DefaultPrimaryWindowLimits(), nil)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			switch name {
			case "cancel":
				cancel()
			case "stop":
				c.Stop()
			case "manual-peer-unavailable":
				c.MarkUnavailable(ErrPeerNotConnected)
			case "source-failure":
				c.mu.Lock()
				c.poisoned = errors.New("uncertain source")
				c.refreshSourceAvailability()
				c.mu.Unlock()
			}
			err := c.WaitRecoveryAvailability(ctx)
			if name == "cancel" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrReplicationUnavailable) || errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("permanent failure was treated as reconnection", err)
			}
		})
	}
}

func TestRecoveryPeerWaitRetainsOriginalTargetAndImmediateRefusal(t *testing.T) {
	j, source := windowHistory(t, 1, nil)
	c := windowPrimary(t, j, source, 1, DefaultPrimaryWindowLimits(), nil)
	send := (&windowSender{}).send
	if err := c.PreparePeer("halro-1", 1, 1, send); err != nil {
		t.Fatal(err)
	}
	target, err := c.CaptureRecoveryTarget(context.Background(), StoreCursor{})
	if err != nil {
		t.Fatal(err)
	}
	c.ForgetPeer("halro-1")
	if err := c.WaitRecoveryTarget(context.Background(), target); !errors.Is(err, ErrReplicationUnavailable) {
		t.Fatal("ordinary wait lost immediate refusal", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.WaitRecoveryTargetWithPeer(ctx, target) }()
	select {
	case err := <-done:
		t.Fatal("confirmed target invented connected peer", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := c.PreparePeer("halro-1", 1, 1, send); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	c.ForgetPeer("halro-1")
	c.MarkUnavailable(ErrPeerNotConnected)
	c.MarkAvailable()
	if err := c.WaitRecoveryTargetWithPeer(ctx, target); err == nil {
		t.Fatal("revoked target was renewed", err)
	}
}

func TestRecoveryCaptureAvailabilityAtomicAndCancellation(t *testing.T) {
	j, source := windowHistory(t, 1, nil)
	c := windowPrimary(t, j, source, 1, DefaultPrimaryWindowLimits(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.CaptureRecoveryTargetWhenAvailable(ctx, StoreCursor{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("capture ignored disconnected peer", err)
	}
	done := make(chan error, 1)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	go func() {
		target, err := c.CaptureRecoveryTargetWhenAvailable(ctx2, StoreCursor{})
		if err == nil && target.Evidence().Index != 1 {
			err = errors.New("wrong captured prefix")
		}
		done <- err
	}()
	if err := c.PreparePeer("halro-1", 1, 1, (&windowSender{}).send); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	c.MarkUnavailable(ErrPeerNotConnected)
	if _, err := c.CaptureRecoveryTargetWhenAvailable(ctx2, StoreCursor{}); !errors.Is(err, ErrReplicationUnavailable) {
		t.Fatal("manual withdrawal treated as temporary", err)
	}
}
