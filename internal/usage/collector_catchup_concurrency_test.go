package usage

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/ledger"
)

// CatchUp runs on the maintenance tick, and from Admin reads, while live
// records keep arriving. Run used to take a record off the queue and only then
// wait for applyMu; a CatchUp holding the lock drained the records queued
// behind it, the held record then arrived behind the watermark, was refused as
// out of order and was lost for good, because replay resumes from the
// watermark. The index ended a few requests short of the Ledger with nothing
// dropped and nothing reported.
func TestCatchUpDuringLiveTrafficLosesNothing(t *testing.T) {
	log, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "ledger.wal"), nil, ledger.Options{ChainKey: testChainKey})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	state := ledger.NewState()
	accounting, err := budget.New(log, state, mustResolver(t, "UTC"))
	if err != nil {
		t.Fatal(err)
	}
	aggregate := newTestAggregate(t)
	collector, err := NewCollector(aggregate, log, 8192)
	if err != nil {
		t.Fatal(err)
	}
	accounting.AddObserver(collector.Observe)
	if err := collector.CatchUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { collector.Run(ctx); close(done) }()

	var stop atomic.Bool
	var issued atomic.Int64
	var writers sync.WaitGroup
	for writer := 0; writer < 8; writer++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for !stop.Load() {
				index := issued.Add(1)
				request, err := accounting.BeginRequestDetailed(context.Background(), "project_1", "key_1", fmt.Sprintf("request_%07d", index), "chat")
				if err != nil {
					t.Error(err)
					return
				}
				if err := accounting.Finalize(context.Background(), request, "success"); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	for deadline := time.Now().Add(1500 * time.Millisecond); time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
		_ = collector.CatchUp(context.Background())
	}
	stop.Store(true)
	writers.Wait()
	if err := collector.CatchUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	cancel()
	<-done

	if stats := collector.Stats(); stats.Dropped != 0 {
		t.Fatalf("the queue overflowed, so this run did not exercise the ordering: %#v", stats)
	}
	expected := newTestAggregate(t)
	if _, err := log.Replay(ledger.Watermark{}, expected.Apply); err != nil {
		t.Fatal(err)
	}
	actual, want := snapshotOf(t, aggregate), snapshotOf(t, expected)
	if actual.Totals.Requests != want.Totals.Requests || !reflect.DeepEqual(actual, want) || aggregate.Metrics() != expected.Metrics() {
		t.Fatalf("the index lost records under concurrent catch-up: %d requests indexed, %d in the Ledger (%d issued)",
			actual.Totals.Requests, want.Totals.Requests, issued.Load())
	}
}
