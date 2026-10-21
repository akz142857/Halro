package bolt

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"go.etcd.io/bbolt"
)

// clockPair is what the gateway captures at selection: the wall reading and,
// at the same instant, a monotonic one.
func clockPair() (time.Time, time.Time) { return time.Now().UTC(), time.Now() }

func seedClockDeployment(t *testing.T, store *Store, deploymentID string) {
	t.Helper()
	seedPricingDeployment(t, store, deploymentID, 0, 0, 0)
	if _, err := store.CreateDeploymentPriceVersion(context.Background(), newStoredPrice("price_"+deploymentID, deploymentID, time.Now().UTC().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
}

func quarantineReason(t *testing.T, store *Store, deploymentID string) string {
	t.Helper()
	var highWater domain.DeploymentPricingHighWater
	if err := store.db.View(func(tx *bbolt.Tx) error {
		return json.Unmarshal(tx.Bucket(bucketDeploymentPricingHighWater).Get([]byte(deploymentID)), &highWater)
	}); err != nil {
		t.Fatal(err)
	}
	return highWater.QuarantineReason
}

// A metadata write stall longer than the forward tolerance, with the clock
// moving normally, is not a forward jump (261017 V-013).
func TestDeploymentPricePinWriteStallIsNotAForwardJump(t *testing.T) {
	store, err := openForTest(t, filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	seedClockDeployment(t, store, "dep_stall")
	const rollback, forward, stall = 2 * time.Second, 300 * time.Millisecond, 500 * time.Millisecond
	selectedAt, observedAt := clockPair()
	if _, _, _, err := store.PrepareDeploymentPricePin(ctx, "dep_stall", "att_before", selectedAt, observedAt, rollback, forward); err != nil {
		t.Fatal(err)
	}
	release, held := make(chan struct{}), make(chan struct{})
	go func() {
		_ = store.db.Update(func(*bbolt.Tx) error { close(held); <-release; return nil })
	}()
	<-held
	stalled := make(chan error, 1)
	go func() {
		selectedAt, observedAt := clockPair()
		_, _, _, err := store.PrepareDeploymentPricePin(ctx, "dep_stall", "att_stalled", selectedAt, observedAt, rollback, forward)
		stalled <- err
	}()
	time.Sleep(stall)
	close(release)
	if err := <-stalled; err != nil {
		t.Fatalf("stalled selection: %v", err)
	}
	selectedAt, observedAt = clockPair()
	if _, _, _, err := store.PrepareDeploymentPricePin(ctx, "dep_stall", "att_after", selectedAt, observedAt, rollback, forward); err != nil {
		t.Fatalf("first selection after a %v write stall: %v", stall, err)
	}
}

// Two selections captured in order that commit in reverse order, further apart
// than the rollback tolerance, are concurrency, not a clock that went back.
// This is what quarantined the E05b soak (261017 V-013).
func TestDeploymentPricePinReorderedCommitIsNotARollback(t *testing.T) {
	store, err := openForTest(t, filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	seedClockDeployment(t, store, "dep_reorder")
	const rollback, forward, delay = 200 * time.Millisecond, 30 * time.Second, 300 * time.Millisecond
	earlierSelected, earlierObserved := clockPair()
	time.Sleep(delay)
	laterSelected, laterObserved := clockPair()
	if _, _, _, err := store.PrepareDeploymentPricePin(ctx, "dep_reorder", "att_later", laterSelected, laterObserved, rollback, forward); err != nil {
		t.Fatal(err)
	}
	_, snapshot, _, err := store.PrepareDeploymentPricePin(ctx, "dep_reorder", "att_earlier", earlierSelected, earlierObserved, rollback, forward)
	if err != nil {
		t.Fatalf("selection captured %v before one that committed first: %v", delay, err)
	}
	if !snapshot.PricingSelectedAt.Equal(laterSelected) {
		t.Fatalf("late-committing selection selected at %v, want the high-water mark %v", snapshot.PricingSelectedAt, laterSelected)
	}
	selectedAt, observedAt := clockPair()
	if _, _, _, err := store.PrepareDeploymentPricePin(ctx, "dep_reorder", "att_next", selectedAt, observedAt, rollback, forward); err != nil {
		t.Fatalf("next selection: %v", err)
	}
}

// A wall clock stepped back while the monotonic clock ran on is still a
// rollback, within one process.
func TestDeploymentPricePinQuarantinesRollbackDrift(t *testing.T) {
	store, err := openForTest(t, filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	seedClockDeployment(t, store, "dep_drift")
	selectedAt, observedAt := clockPair()
	if _, _, _, err := store.PrepareDeploymentPricePin(ctx, "dep_drift", "att_first", selectedAt, observedAt, 2*time.Second, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	selectedAt, observedAt = clockPair()
	if _, _, _, err := store.PrepareDeploymentPricePin(ctx, "dep_drift", "att_stepped_back", selectedAt.Add(-3*time.Second), observedAt, 2*time.Second, 30*time.Second); !errors.Is(err, ErrPricingQuarantined) {
		t.Fatalf("stepped-back clock error=%v", err)
	}
	if reason := quarantineReason(t, store, "dep_drift"); reason != "wall_clock_rollback" {
		t.Fatalf("quarantine reason=%q", reason)
	}
}

// Across a restart there is no anchor, and the durable high-water mark alone
// still catches a rollback.
func TestDeploymentPricePinQuarantinesRollbackAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	store, err := openForTest(t, path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	seedClockDeployment(t, store, "dep_restart")
	selectedAt, observedAt := clockPair()
	if _, _, _, err := store.PrepareDeploymentPricePin(ctx, "dep_restart", "att_before", selectedAt, observedAt, 2*time.Second, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openForTest(t, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, _, _, err := store.PrepareDeploymentPricePin(ctx, "dep_restart", "att_after", selectedAt.Add(-3*time.Second), time.Now(), 2*time.Second, 30*time.Second); !errors.Is(err, ErrPricingQuarantined) {
		t.Fatalf("rollback across restart error=%v", err)
	}
	if reason := quarantineReason(t, store, "dep_restart"); reason != "wall_clock_rollback" {
		t.Fatalf("quarantine reason=%q", reason)
	}
}
