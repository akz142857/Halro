package usage

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/ledger"
	bbolt "go.etcd.io/bbolt"
)

// A write that keeps failing must not leave the collector applying live
// records on top of a batch that can never be written. Each failed write hands
// its batch back, so at any arrival rate slower than the failing write the
// batch grew by every request for as long as the failure lasted — about 1.2 KB
// of heap each — and only arrivals fast enough to overflow the queue ever
// stopped it. A failed write now stops application the way a dropped record
// does, and the gap is replayed from the Ledger once the write succeeds.
func TestAFailingHistoryWriteStopsApplyingAndCatchesUpOnceItSucceeds(t *testing.T) {
	log, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "ledger.wal"), nil, ledger.Options{MaxBatch: 1, ChainKey: testChainKey})
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
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { collector.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	// A request that is accepted and finalized leaves a summary row, which is
	// what a batch holds.
	begin := func(index int) {
		t.Helper()
		request, err := accounting.BeginRequestDetailed(context.Background(), "project_1", "key_1", fmt.Sprintf("request_%04d", index), "chat")
		if err != nil {
			t.Fatal(err)
		}
		if err := accounting.Finalize(context.Background(), request, "success"); err != nil {
			t.Fatal(err)
		}
	}
	for index := 0; index < 10; index++ {
		begin(index)
	}
	if err := collector.CatchUp(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Every commit reads the stored state row back; damage it.
	var original []byte
	if err := aggregate.History().db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(historyMeta)
		original = append([]byte(nil), bucket.Get(historyKeyState)...)
		damaged := append([]byte(nil), original...)
		damaged[0] ^= 0x01
		return bucket.Put(historyKeyState, damaged)
	}); err != nil {
		t.Fatal(err)
	}

	// Paced well below what a failing write can absorb, so the queue never
	// overflows: the case that used to grow without bound.
	peak := 0
	for index := 10; index < 410; index++ {
		begin(index)
		time.Sleep(2 * time.Millisecond)
		peak = max(peak, aggregate.PendingRecords())
	}
	time.Sleep(600 * time.Millisecond)
	stats := collector.Stats()
	if peak > 100 {
		t.Fatalf("the pending batch kept growing while the write failed: peak %d records for 400 requests", peak)
	}
	if stats.FlushFailures == 0 || !stats.Lagging || stats.Dropped != 0 {
		t.Fatalf("a failing write did not put the collector into lagging: %#v", stats)
	}
	if err := collector.CatchUp(context.Background()); err == nil {
		t.Fatal("catch-up succeeded while the write still fails")
	}
	if pending := aggregate.PendingRecords(); pending > 100 {
		t.Fatalf("catch-up applied more records on top of a batch it cannot write: %d pending", pending)
	}

	// The fault clears; the next catch-up writes the batch and replays the gap.
	if err := aggregate.History().db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(historyMeta).Put(historyKeyState, original)
	}); err != nil {
		t.Fatal(err)
	}
	if err := collector.CatchUp(context.Background()); err != nil {
		t.Fatalf("catch-up after the fault cleared: %v", err)
	}
	if after := collector.Stats(); after.Lagging {
		t.Fatalf("still lagging after a successful catch-up: %#v", after)
	}
	expected := newTestAggregate(t)
	if _, err := log.Replay(ledger.Watermark{}, expected.Apply); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshotOf(t, aggregate), snapshotOf(t, expected)) || aggregate.Metrics() != expected.Metrics() {
		t.Fatalf("catch-up differs from authoritative replay\nactual=%#v\nexpected=%#v", snapshotOf(t, aggregate), snapshotOf(t, expected))
	}
}
