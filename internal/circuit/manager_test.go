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
