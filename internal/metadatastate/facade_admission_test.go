package metadatastate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

func TestMetadataFacadeBusinessAdmissionSerializesAndCancelsBoundedWaiters(t *testing.T) {
	b, _, o := metadataFacadeFixture(t)
	ref, err := b.SealReference(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	backend, err := b.StandaloneFacade(context.Background(), o.Budget, o.Identity, o.InstanceID, o.KeyGeneration, ref)
	if err != nil {
		t.Fatal(err)
	}
	// Another qualified facade of the same physical owner shares its lane.
	first, err := b.StandaloneFacade(context.Background(), o.Budget, o.Identity, o.InstanceID, o.KeyGeneration, ref)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- first.View(context.Background(), func(tx boltstore.FacadeTransaction) error { close(entered); <-release; return nil })
	}()
	<-entered
	canceledCtx, cancel := context.WithCancel(context.Background())
	queued := make(chan error, 1)
	go func() {
		queued <- backend.View(canceledCtx, func(boltstore.FacadeTransaction) error { t.Error("canceled waiter entered callback"); return nil })
	}()
	cancel()
	if err := <-queued; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	second := make(chan error, 1)
	go func() {
		second <- backend.View(context.Background(), func(boltstore.FacadeTransaction) error { return nil })
	}()
	select {
	case err := <-second:
		t.Fatal("queued business call failed instead of waiting", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if backend.HealthError() != nil || b.businessWaiters.Load() != 0 {
		t.Fatal("admission debt")
	}
	if err := backend.View(context.Background(), func(boltstore.FacadeTransaction) error { return authenticatedstate.ErrBusy }); !errors.Is(err, authenticatedstate.ErrBusy) {
		t.Fatal(err)
	}
}
