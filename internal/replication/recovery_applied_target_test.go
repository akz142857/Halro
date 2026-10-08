package replication

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRecoveryAppliedTargetRequiresEveryPeerAndTrailingStore(t *testing.T) {
	c, _ := recoveryTargetPrimary(t)
	target, err := c.CaptureRecoveryTarget(context.Background(), StoreCursor{1, 1})
	if err != nil {
		t.Fatal(err)
	}
	ack := testAcknowledgement("halro-1", 5)
	if _, err := c.Acknowledge(ack); err != nil {
		t.Fatal(err)
	}
	if err := c.WaitRecoveryTarget(context.Background(), target); err != nil {
		t.Fatal("real confirmation quorum", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.WaitRecoveryAppliedTarget(ctx, target) }()
	assertBlocked := func() {
		t.Helper()
		select {
		case err := <-done:
			t.Fatal("incomplete whole-peer application was joined", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	assertBlocked()
	ack.AppliedIndex = 5
	if _, err := c.Acknowledge(ack); err != nil {
		t.Fatal(err)
	}
	assertBlocked()
	ack.NodeID, ack.AppliedIndex = "halro-2", 4
	if _, err := c.Acknowledge(ack); err != nil {
		t.Fatal(err)
	}
	assertBlocked()
	// The target includes trailing Metadata at index 5, not only Ledger at 2.
	ack.AppliedIndex = 5
	if _, err := c.Acknowledge(ack); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := c.WaitRecoveryAppliedTarget(ctx, RecoveryTarget{}); err == nil {
		t.Fatal("unbound target joined")
	}
	foreign, _ := recoveryTargetPrimary(t)
	if err := foreign.WaitRecoveryAppliedTarget(ctx, target); err == nil {
		t.Fatal("foreign lifecycle joined")
	}
	if err := c.WaitRecoveryAppliedTarget(func() context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}(), target); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled target joined", err)
	}
	c.Stop()
	if err := c.WaitRecoveryAppliedTarget(ctx, target); err == nil {
		t.Fatal("stopped lifecycle joined already applied target")
	}
}

func TestRecoveryAppliedTargetPendingWaitJoinsJournalInvalidation(t *testing.T) {
	c, journal := recoveryTargetPrimary(t)
	target, err := c.CaptureRecoveryTarget(context.Background(), StoreCursor{1, 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.WaitRecoveryAppliedTarget(ctx, target) }()
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("closed Ordering journal joined")
	}
}
