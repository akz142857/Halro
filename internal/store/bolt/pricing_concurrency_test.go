package bolt

// Concurrency tests for ADR 0012, "Amendment 2026-08-07": the deployment pricing
// gate is shared for selection and exclusive for timeline mutation, and price
// pins are written with db.Batch. Both changes let same-deployment selections
// overlap, so the properties the exclusive mutex used to provide for free now
// have to be stated and checked.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

func newPricedStore(t *testing.T, deploymentID string, effectiveFrom time.Time) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	seedPricingDeployment(t, store, deploymentID, 0, 0, 0)
	if _, err := store.CreateDeploymentPriceVersion(context.Background(),
		newStoredPrice("price_"+deploymentID, deploymentID, effectiveFrom)); err != nil {
		t.Fatal(err)
	}
	return store
}

// The cancellation invariant the gate exists for: an exclusive acquirer must wait
// for every in-flight shared holder, so no timeline mutation can be interleaved
// between a price selection and the Lease that makes it durable.
func TestExclusivePricingGateWaitsForInFlightSelections(t *testing.T) {
	store := newPricedStore(t, "dep_gate", time.Now().UTC().Add(-time.Hour))

	releaseSelection := store.LockDeploymentPricingShared("dep_gate")
	mutated := make(chan struct{})
	go func() {
		unlock := store.LockDeploymentPricingExclusive("dep_gate")
		close(mutated)
		unlock()
	}()
	select {
	case <-mutated:
		t.Fatal("an Admin timeline mutation took the gate while a selection was in flight")
	case <-time.After(100 * time.Millisecond):
	}
	releaseSelection()
	select {
	case <-mutated:
	case <-time.After(5 * time.Second):
		t.Fatal("the Admin timeline mutation never acquired the gate")
	}

	// And the other direction: a selection cannot start under an Admin mutation.
	releaseMutation := store.LockDeploymentPricingExclusive("dep_gate")
	selected := make(chan struct{})
	go func() {
		unlock := store.LockDeploymentPricingShared("dep_gate")
		close(selected)
		unlock()
	}()
	select {
	case <-selected:
		t.Fatal("a selection took the gate while an Admin mutation held it")
	case <-time.After(100 * time.Millisecond):
	}
	releaseMutation()
	select {
	case <-selected:
	case <-time.After(5 * time.Second):
		t.Fatal("the selection never acquired the gate")
	}
}

// Making the gate shared introduces a starvation question the exclusive mutex did
// not have: a steady stream of selections must not postpone a scheduled
// cancellation indefinitely. sync.RWMutex blocks new readers once a writer is
// waiting, so the property comes from the standard library — this pins it, because
// the amendment's availability argument depends on it.
func TestScheduledCancellationIsNotStarvedBySelections(t *testing.T) {
	store := newPricedStore(t, "dep_starve", time.Now().UTC().Add(-time.Hour))

	firstSelection := store.LockDeploymentPricingShared("dep_starve")
	order := make(chan string, 2)
	var waiting sync.WaitGroup

	waiting.Add(1)
	go func() {
		defer waiting.Done()
		unlock := store.LockDeploymentPricingExclusive("dep_starve")
		order <- "cancellation"
		unlock()
	}()
	// Give the cancellation time to become a waiting writer before the next
	// selection arrives; that ordering is the whole point of the test.
	time.Sleep(100 * time.Millisecond)

	waiting.Add(1)
	go func() {
		defer waiting.Done()
		unlock := store.LockDeploymentPricingShared("dep_starve")
		order <- "selection"
		unlock()
	}()
	time.Sleep(100 * time.Millisecond)

	select {
	case first := <-order:
		t.Fatalf("%q acquired the gate while the first selection still held it", first)
	default:
	}

	firstSelection()
	waiting.Wait()
	if first := <-order; first != "cancellation" {
		t.Fatalf("a selection arriving after a waiting cancellation overtook it: %q went first", first)
	}
}

// Concurrent selections reach their durable pin in an order that need not match
// the order they captured pricing_selected_at, so the later commit reads a
// selection time behind the high-water mark. That backwards step is caused by
// this process, not by the clock, and must never quarantine the deployment — at
// any tolerance the configuration permits, including the smallest.
func TestOutOfOrderSelectionsDoNotQuarantine(t *testing.T) {
	base := time.Now().UTC()
	store := newPricedStore(t, "dep_reorder", base.Add(-time.Hour))
	ctx := context.Background()
	tolerance := config.MinPricingClockRollbackTolerance

	// Explicitly inverted: the second call presents an older selection time than
	// the first, by less than the floor. This is what reordering looks like from
	// inside the store.
	if _, _, _, err := store.PrepareDeploymentPricePin(ctx, "dep_reorder", "att_late", base, tolerance, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.PrepareDeploymentPricePin(ctx, "dep_reorder", "att_early", base.Add(-tolerance+10*time.Millisecond), tolerance, 30*time.Second); err != nil {
		t.Fatalf("a reordered selection within the tolerance was rejected: %v", err)
	}
	if quarantined, reason, err := store.DeploymentPricingQuarantine(ctx, "dep_reorder"); err != nil || quarantined {
		t.Fatalf("reordering quarantined the deployment: quarantined=%v reason=%q err=%v", quarantined, reason, err)
	}

	// A rollback materially past the tolerance is still a rollback.
	if _, _, _, err := store.PrepareDeploymentPricePin(ctx, "dep_reorder", "att_rolled_back", base.Add(-tolerance-time.Second), tolerance, 30*time.Second); !errors.Is(err, ErrPricingQuarantined) {
		t.Fatalf("a material rollback was not quarantined: err=%v", err)
	}
}

// The same property under real concurrency rather than a scripted inversion:
// every worker draws its own selection time from a window narrower than the
// tolerance and they race, which is exactly the shape production traffic has.
func TestConcurrentSelectionsOnOneDeploymentDoNotQuarantine(t *testing.T) {
	base := time.Now().UTC()
	store := newPricedStore(t, "dep_race", base.Add(-time.Hour))
	ctx := context.Background()
	tolerance := config.MinPricingClockRollbackTolerance

	var group sync.WaitGroup
	failures := make(chan error, 64)
	for worker := 0; worker < 32; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			unlock := store.LockDeploymentPricingShared("dep_race")
			defer unlock()
			// Spread within the tolerance, deliberately not in worker order.
			selectedAt := base.Add(-time.Duration(worker%8) * 50 * time.Millisecond)
			_, _, intent, err := store.PrepareDeploymentPricePin(ctx, "dep_race", fmt.Sprintf("att_race_%d", worker), selectedAt, tolerance, 30*time.Second)
			if err != nil {
				failures <- fmt.Errorf("worker %d: %w", worker, err)
				return
			}
			if intent.PriceVersionID != "price_dep_race" {
				failures <- fmt.Errorf("worker %d pinned %q", worker, intent.PriceVersionID)
			}
		}(worker)
	}
	group.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if quarantined, reason, err := store.DeploymentPricingQuarantine(ctx, "dep_race"); err != nil || quarantined {
		t.Fatalf("concurrent selections quarantined the deployment: quarantined=%v reason=%q err=%v", quarantined, reason, err)
	}
}

// The in-memory clock anchor is what detects a forward jump, and it must not walk
// backwards when selections commit out of order — otherwise an ordinary
// reordering lowers the anchor and the next request looks like a jump ahead.
//
// The merge is asserted directly rather than through two Prepare calls, because
// an end-to-end pair cannot reach the case: the durable high-water clamp raises
// the later-arriving selection to the high-water mark before the anchor is
// written, so a sequential test never offers the anchor a smaller value and
// passes whether or not the merge guards anything. The window the merge exists
// for is narrower — two selections commit in capture order, then their
// post-commit anchor writes land in the opposite order — and it is reachable only
// by interleaving goroutines between the commit and the merge.
func TestClockAnchorDoesNotRegress(t *testing.T) {
	base := time.Now().UTC()
	store := newPricedStore(t, "dep_anchor", base.Add(-time.Hour))
	state := store.deploymentPricingState("dep_anchor")

	observed := time.Now()
	state.mergeClockAnchor(pricingClockObservation{SelectedAt: base, ObservedAt: observed})
	// The write a late-merging sibling would perform.
	state.mergeClockAnchor(pricingClockObservation{SelectedAt: base.Add(-time.Second), ObservedAt: observed.Add(time.Millisecond)})

	anchor, ok := state.clockAnchor()
	if !ok {
		t.Fatal("no clock anchor was recorded")
	}
	if !anchor.SelectedAt.Equal(base) {
		t.Fatalf("the anchor regressed to %s, want it held at %s", anchor.SelectedAt, base)
	}
	// The pair travels together: an ObservedAt is only meaningful beside the
	// SelectedAt it was captured with, so a rejected merge must not leave the
	// sibling's observation instant behind.
	if !anchor.ObservedAt.Equal(observed) {
		t.Fatalf("the anchor kept a mismatched observation instant %s, want %s", anchor.ObservedAt, observed)
	}

	// A genuinely newer selection still advances it.
	state.mergeClockAnchor(pricingClockObservation{SelectedAt: base.Add(time.Second), ObservedAt: observed.Add(time.Second)})
	if anchor, _ = state.clockAnchor(); !anchor.SelectedAt.Equal(base.Add(time.Second)) {
		t.Fatalf("the anchor did not advance: %s", anchor.SelectedAt)
	}
}

// Selection times that arrive out of order must not lower the durable high-water
// mark either, since that is what stops a restarted process from selecting off an
// older price timeline.
func TestDurableHighWaterDoesNotRegressOnOutOfOrderSelections(t *testing.T) {
	base := time.Now().UTC()
	store := newPricedStore(t, "dep_high_water", base.Add(-time.Hour))
	ctx := context.Background()
	tolerance := config.MinPricingClockRollbackTolerance

	if _, _, _, err := store.PrepareDeploymentPricePin(ctx, "dep_high_water", "att_hw_late", base, tolerance, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	_, _, intent, err := store.PrepareDeploymentPricePin(ctx, "dep_high_water", "att_hw_early", base.Add(-500*time.Millisecond), tolerance, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// The reordered selection is clamped up to the high-water mark, not served at
	// the time it presented.
	if !intent.PricingSelectedAt.Equal(base) {
		t.Fatalf("reordered selection pinned at %s, want it clamped to %s", intent.PricingSelectedAt, base)
	}
}

// db.Batch runs every queued function in one transaction, and when any of them
// returns an error it rolls the whole transaction back, evicts that caller, and
// re-runs the survivors from scratch. Price pin preparation is now a batched
// write, so an unrelated failing writer can force it to run more than once — its
// result must be the state that actually committed, never a value left over from
// a discarded run.
func TestPricePinPreparationSurvivesBatchSiblingFailures(t *testing.T) {
	base := time.Now().UTC()
	store := newPricedStore(t, "dep_batch", base.Add(-time.Hour))
	ctx := context.Background()
	tolerance := config.MinPricingClockRollbackTolerance

	stop := make(chan struct{})
	var poisoners sync.WaitGroup
	for poisoner := 0; poisoner < 4; poisoner++ {
		poisoners.Add(1)
		go func() {
			defer poisoners.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// A sibling that always fails: every batch it lands in is rolled
				// back and its survivors re-run.
				_ = store.db.Batch(func(*bbolt.Tx) error { return errors.New("poisoned batch sibling") })
			}
		}()
	}

	var prepared atomic.Int64
	var group sync.WaitGroup
	failures := make(chan error, 64)
	for worker := 0; worker < 32; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			unlock := store.LockDeploymentPricingShared("dep_batch")
			defer unlock()
			attemptID := fmt.Sprintf("att_batch_%d", worker)
			_, snapshot, intent, err := store.PrepareDeploymentPricePin(ctx, "dep_batch", attemptID, base, tolerance, 30*time.Second)
			if err != nil {
				failures <- fmt.Errorf("worker %d: %w", worker, err)
				return
			}
			digest, err := snapshot.Digest()
			if err != nil {
				failures <- fmt.Errorf("worker %d digest: %w", worker, err)
				return
			}
			if intent.AttemptID != attemptID || intent.SnapshotSHA256 != digest || intent.State != domain.PricePinPrepared {
				failures <- fmt.Errorf("worker %d got intent %#v", worker, intent)
				return
			}
			// The pin the caller was told about must be the pin that committed.
			stored, err := store.CommitDeploymentPricePin(ctx, attemptID, intent.SnapshotSHA256, uint64(worker+1), time.Now().UTC())
			if err != nil {
				failures <- fmt.Errorf("worker %d commit: %w", worker, err)
				return
			}
			if stored.State != domain.PricePinCommitted || stored.LedgerSequence != uint64(worker+1) {
				failures <- fmt.Errorf("worker %d committed %#v", worker, stored)
				return
			}
			prepared.Add(1)
		}(worker)
	}
	group.Wait()
	close(stop)
	poisoners.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if prepared.Load() != 32 {
		t.Fatalf("%d of 32 attempts completed", prepared.Load())
	}
	if quarantined, reason, err := store.DeploymentPricingQuarantine(ctx, "dep_batch"); err != nil || quarantined {
		t.Fatalf("batch retries quarantined the deployment: quarantined=%v reason=%q err=%v", quarantined, reason, err)
	}
}

// BatchCalls/BatchTransactions is the metadata store's coalescing factor, the
// counterpart of the Ledger's records-per-batch. It is the only way to tell from
// outside whether batching the pin writes does anything on a given host, so it
// has to actually count coalescing rather than just count calls.
func TestMetadataWriteStatsCountCoalescing(t *testing.T) {
	base := time.Now().UTC()
	store := newPricedStore(t, "dep_stats", base.Add(-time.Hour))
	ctx := context.Background()
	tolerance := config.MinPricingClockRollbackTolerance

	const workers = 32
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			unlock := store.LockDeploymentPricingShared("dep_stats")
			defer unlock()
			attemptID := fmt.Sprintf("att_stats_%d", worker)
			_, _, intent, err := store.PrepareDeploymentPricePin(ctx, "dep_stats", attemptID, base, tolerance, 30*time.Second)
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := store.CommitDeploymentPricePin(ctx, attemptID, intent.SnapshotSHA256, uint64(worker+1), time.Now().UTC()); err != nil {
				t.Error(err)
			}
		}(worker)
	}
	group.Wait()

	stats := store.MetadataWriteStats()
	// One prepare and one commit per worker.
	if stats.BatchCalls < 2*workers {
		t.Fatalf("counted %d batched calls for %d workers", stats.BatchCalls, workers)
	}
	if stats.BatchTransactions == 0 {
		t.Fatal("no write transactions were counted")
	}
	if stats.BatchTransactions > stats.BatchCalls {
		t.Fatalf("transactions=%d exceeds calls=%d, so this is not a coalescing factor",
			stats.BatchTransactions, stats.BatchCalls)
	}
	if stats.PageWrites <= 0 || stats.PageWriteDuration <= 0 {
		t.Fatalf("metadata write cost went unmeasured: writes=%d duration=%s",
			stats.PageWrites, stats.PageWriteDuration)
	}
}
