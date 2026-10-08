package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/usage"
	bbolt "go.etcd.io/bbolt"
)

func TestRuntimeCloseJoinsUsageCollectorBeforeBorrowedStores(t *testing.T) {
	r, bootstrap, _ := openBootstrappedRuntime(t)
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() { unblock(); _ = r.Close() })
	// A replacement is installed only after the existing background generation
	// has joined; its maintenance worker reads this field and performs final
	// CatchUp. This fixture tests terminal dependency closure, not publication
	// of a replacement into a live Runtime.
	r.stopBackgroundWork()
	if err := r.usageCollector.Retire(context.Background()); err != nil {
		t.Fatal(err)
	}
	dependency := make(chan error, 1)
	collector, err := usage.NewProjectedCollector(r.usage.owner.derivative, func(ctx context.Context, _ ledger.Watermark, _ func(ledger.Record) error) (ledger.Watermark, error) {
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-release // model non-interruptible source IO after cancellation
		_, nativeErr := r.ledger.ReplayAuthenticated(context.Background(), ledger.ReplayAnchor{}, nil)
		_, metadataErr := r.store.GetProject(context.Background(), bootstrap.ProjectID)
		dependency <- errors.Join(nativeErr, metadataErr)
		return ledger.Watermark{}, ctx.Err()
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	r.usageCollector = collector
	caught := make(chan error, 1)
	go func() { caught <- collector.CatchUp(context.Background()) }()
	awaitDrainSignal(t, entered)
	closed := make(chan error, 1)
	go func() { closed <- r.Close() }()
	awaitDrainSignal(t, canceled)
	select {
	case err := <-closed:
		t.Fatal("Runtime closed before actual collector IO", err)
	default:
	}
	if err := collector.CatchUp(context.Background()); !errors.Is(err, usage.ErrCollectorRetired) {
		t.Fatal("late reader admitted", err)
	}
	unblock()
	if err := <-caught; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-dependency; err != nil {
		t.Fatal("borrowed files closed before collector exited", err)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Runtime did not finish actual collector join")
	}
	if _, err := r.ledger.ReplayAuthenticated(context.Background(), ledger.ReplayAnchor{}, nil); err == nil {
		t.Fatal("native still open after Runtime Close")
	}
	if _, err := r.store.ReadMetadataAppliedPosition(context.Background()); !errors.Is(err, bbolt.ErrDatabaseNotOpen) {
		t.Fatal("Metadata still open after Runtime Close", err)
	}
}
