package circuit

import (
	"errors"
	"testing"
	"time"
)

func TestCircuitOpensAndAllowsSingleHalfOpenProbe(t *testing.T) {
	manager, err := New(Config{FailureThreshold: 2, OpenDuration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	for index := 0; index < 2; index++ {
		lease, err := manager.Acquire("target_1", now)
		if err != nil {
			t.Fatal(err)
		}
		lease.Done(errors.New("unavailable"), now)
	}
	if _, err := manager.Acquire("target_1", now); !errors.Is(err, ErrOpen) {
		t.Fatalf("expected open circuit, got %v", err)
	}
	probe, err := manager.Acquire("target_1", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Acquire("target_1", now.Add(time.Minute)); !errors.Is(err, ErrOpen) {
		t.Fatal("more than one half-open probe was admitted")
	}
	probe.Done(nil, now.Add(time.Minute))
	if _, err := manager.Acquire("target_1", now.Add(time.Minute)); err != nil {
		t.Fatalf("successful probe did not close circuit: %v", err)
	}
}

// The half-open slot is claimed at Acquire, before the gateway has run any of
// its local admission checks. A probe rejected by concurrency, budget, pricing,
// or policy therefore holds the slot without having learned anything about the
// provider, and must hand it back without voting either way.
func TestAbandonedHalfOpenProbeNeitherClosesNorFaultsTheCircuit(t *testing.T) {
	manager, err := New(Config{FailureThreshold: 2, OpenDuration: time.Minute, HalfOpenMaxRequests: 1})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	for index := 0; index < 2; index++ {
		lease, err := manager.Acquire("target_1", now)
		if err != nil {
			t.Fatal(err)
		}
		lease.Done(errors.New("provider unavailable"), now)
	}
	if !manager.IsOpen("target_1", now) {
		t.Fatal("circuit did not open at the failure threshold")
	}

	halfOpen := now.Add(time.Minute)
	probe, err := manager.Acquire("target_1", halfOpen)
	if err != nil {
		t.Fatalf("half-open window admitted no probe: %v", err)
	}
	probe.Abandon()

	// The slot has to come back; holding it would strand the target open even
	// after the open duration expires.
	next, err := manager.Acquire("target_1", halfOpen)
	if err != nil {
		t.Fatalf("abandoned probe kept its half-open slot: %v", err)
	}
	// The accumulated failures have to survive too. With them intact one more
	// real failure re-opens immediately; had Abandon cleared them the way
	// Done(nil) does, this failure would be the first of a fresh streak and the
	// circuit would stay closed over a provider that is still down.
	next.Done(errors.New("provider unavailable"), halfOpen)
	if !manager.IsOpen("target_1", halfOpen.Add(time.Second)) {
		t.Fatal("abandoning a probe reset the failure count")
	}
}

// Abandon and Done share the once guard, so whichever the caller reaches first
// wins and a later duplicate cannot double-release the half-open slot.
func TestLeaseOutcomeIsRecordedOnlyOnce(t *testing.T) {
	manager, err := New(Config{FailureThreshold: 1, OpenDuration: time.Minute, HalfOpenMaxRequests: 1})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	lease, err := manager.Acquire("target_1", now)
	if err != nil {
		t.Fatal(err)
	}
	lease.Done(errors.New("provider unavailable"), now)
	lease.Abandon()
	lease.Done(nil, now)
	if !manager.IsOpen("target_1", now) {
		t.Fatal("a second outcome on the same lease overrode the first")
	}
}
