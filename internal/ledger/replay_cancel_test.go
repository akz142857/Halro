package ledger

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// TestCanceledVisitDoesNotCondemnTheLedger separates two things Replay used to
// treat identically: the log failing to read, and the caller failing to keep
// up. Only the first says anything about the WAL. The second is routine —
// every usage catch-up replays under a request context, so an admin closing a
// browser tab mid-replay surfaces here as context.Canceled — and latching the
// accounting status on it would let a derived read model take the ledger down
// with it. Recovery status is one-way and has no production reset, so the
// gateway would then refuse every request until the process restarted, with a
// perfectly intact WAL on disk.
func TestCanceledVisitDoesNotCondemnTheLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	status := NewStatus()
	log := openChained(t, path, status)
	defer log.Close()
	for _, id := range []string{"evt_1", "evt_2", "evt_3"} {
		if _, err := log.Append(context.Background(), validReservation(id, "attempt_"+id)); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	visited := 0
	_, err := log.Replay(Watermark{}, func(Record) error {
		visited++
		if visited == 2 {
			cancel()
		}
		return ctx.Err()
	})
	cancel()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("replay error = %v, want context.Canceled", err)
	}
	if status.Load() != AccountingHealthy {
		t.Fatalf("a canceled visit must not require recovery: state=%v", status.Load())
	}
	// The real proof is that the ledger still works: recovery status gates
	// Append, so if the cancellation had latched, this would fail.
	if _, err := log.Append(context.Background(), validReservation("evt_4", "attempt_4")); err != nil {
		t.Fatalf("append after a canceled replay: %v", err)
	}
}

// TestCorruptScanStillCondemnsTheLedger is the other half of the same change.
// Narrowing what counts as the log's own failure is only safe if genuine
// integrity failures still latch, so this pins the behaviour the fix must not
// weaken.
func TestCorruptScanStillCondemnsTheLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	status := NewStatus()
	log := openChained(t, path, status)
	defer log.Close()
	if _, err := log.Append(context.Background(), validReservation("evt_1", "attempt_1")); err != nil {
		t.Fatal(err)
	}
	// Replaying from an offset that is not a frame boundary makes the scanner
	// read a header out of the middle of a payload.
	if _, err := log.Replay(Watermark{Generation: 1, Offset: 40, Sequence: 0}, func(Record) error {
		return nil
	}); err == nil {
		t.Fatal("expected a scan failure when replaying from a non-boundary offset")
	}
	if status.Load() == AccountingHealthy {
		t.Fatal("a corrupt scan must still require recovery")
	}
}
