package authenticatedstate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

func TestSealCandidateConcurrentReadQualificationsShareBoundedOwner(t *testing.T) {
	dir := t.TempDir()
	guard, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	budget := testBudget(t)
	reservation := sealPublisherReservation(t, budget)
	want := sealCandidateFixture()
	p, err := OpenSealCandidatePublisher(dir, guard, make([]byte, 32), sealHardFor(want), budget, reservation)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
		if err := p.Retire(); err != nil {
			t.Error(err)
		}
		if err := reservation.Release(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := p.Publish(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	before := budget.Snapshot().Used
	start := make(chan struct{})
	done := make(chan error, 4)
	for range 4 {
		go func() {
			<-start
			var failure error
			for range 64 {
				got, found, err := p.Select(sealEligibilityFor(want))
				if err != nil || !found || got != want {
					failure = errors.Join(failure, err, ErrSealIneligible)
					break
				}
			}
			done <- failure
		}()
	}
	close(start)
	for range 4 {
		if err := <-done; err != nil {
			t.Error("same immutable common seal qualification collided with another actual reader", err)
		}
	}
	if budget.Snapshot().Used != before {
		t.Fatal("queued read ownership was not released after actual IO joined")
	}
	// Stop the actual publisher lane. More readers than the entire claim pool
	// must be refused before waiting, rather than forming an uncharged queue.
	p.mu.Lock()
	locked := true
	defer func() {
		if locked {
			p.mu.Unlock()
		}
	}()
	queued := make(chan error, 32)
	for range 32 {
		go func() { _, _, err := p.Current(); queued <- err }()
	}
	deadline := time.Now().Add(time.Second)
	for budget.Snapshot().NormalClaims < 16 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	stats := budget.Snapshot()
	if stats.NormalClaims != 16 || stats.Classes[resourcebudget.IO].RAM > 15*candidateSealMaxBytes {
		t.Fatal("actual pending reader queue escaped shared admission", stats)
	}
	p.mu.Unlock()
	locked = false
	rejected := 0
	for range 32 {
		err := <-queued
		if errors.Is(err, resourcebudget.ErrExhausted) {
			rejected++
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if rejected == 0 || budget.Snapshot().Used != before {
		t.Fatal("bounded reader refusal or joined refunds missing", rejected, budget.Snapshot())
	}
}
