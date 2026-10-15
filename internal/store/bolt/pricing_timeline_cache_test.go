package bolt

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	bbolt "go.etcd.io/bbolt"
)

// The audited timeline is cached because auditing it means decoding every
// version the deployment has ever had, and the answer only changes when the
// timeline does. Every one of these asserts the other half of that bargain: a
// write that changes what the timeline says must be visible to the very next
// selection. A cache that misses one of them prices requests from a timeline
// that no longer exists, which is an accounting fault, not a stale display.

func cachedTimelineStore(t *testing.T, deploymentID string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	seedPricingDeployment(t, store, deploymentID, 0, 0, 0)
	return store
}

func selectedPriceID(t *testing.T, store *Store, deploymentID string, at time.Time) string {
	t.Helper()
	price, err := store.SelectDeploymentPriceVersion(context.Background(), deploymentID, at)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	return price.ID
}

func TestCachedTimelineSeesANewlyCreatedVersion(t *testing.T) {
	store := cachedTimelineStore(t, "dep_create")
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	later := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	at := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	if _, err := store.CreateDeploymentPriceVersion(ctx, newStoredPrice("price_first", "dep_create", base)); err != nil {
		t.Fatal(err)
	}
	if got := selectedPriceID(t, store, "dep_create", at); got != "price_first" {
		t.Fatalf("selected %q before the second version existed", got)
	}

	// The read above populated the cache; this write has to drop it.
	if _, err := store.CreateDeploymentPriceVersion(ctx, newStoredPrice("price_second", "dep_create", later)); err != nil {
		t.Fatal(err)
	}
	if got := selectedPriceID(t, store, "dep_create", at); got != "price_second" {
		t.Fatalf("selected %q; the newly created version was not visible", got)
	}
}

func TestCachedTimelineSeesACancellation(t *testing.T) {
	store := cachedTimelineStore(t, "dep_cancel")
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	scheduled := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	at := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	if _, err := store.CreateDeploymentPriceVersion(ctx, newStoredPrice("price_kept", "dep_cancel", base)); err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateDeploymentPriceVersion(ctx, newStoredPrice("price_cancelled", "dep_cancel", scheduled))
	if err != nil {
		t.Fatal(err)
	}
	if got := selectedPriceID(t, store, "dep_cancel", at); got != "price_cancelled" {
		t.Fatalf("selected %q before the cancellation", got)
	}

	// A cancellation rewrites the version in place and leaves the timeline's
	// key set untouched, so nothing about the bucket layout reveals it.
	if _, err := store.CancelDeploymentPriceVersion(
		ctx, "dep_cancel", "price_cancelled", "admin_one",
		time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), created.Revision,
	); err != nil {
		t.Fatal(err)
	}
	if got := selectedPriceID(t, store, "dep_cancel", at); got != "price_kept" {
		t.Fatalf("selected %q; the cancellation was not visible", got)
	}
}

// One deployment's write must not drop another's audited copy, and must not
// leak its versions into the neighbour's answer.
func TestCachedTimelineKeepsDeploymentsApart(t *testing.T) {
	store := cachedTimelineStore(t, "dep_left")
	seedPricingDeployment(t, store, "dep_right", 0, 0, 0)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	if _, err := store.CreateDeploymentPriceVersion(ctx, newStoredPrice("price_left", "dep_left", base)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateDeploymentPriceVersion(ctx, newStoredPrice("price_right", "dep_right", base)); err != nil {
		t.Fatal(err)
	}
	if got := selectedPriceID(t, store, "dep_left", at); got != "price_left" {
		t.Fatalf("dep_left selected %q", got)
	}
	if got := selectedPriceID(t, store, "dep_right", at); got != "price_right" {
		t.Fatalf("dep_right selected %q", got)
	}
}

// A timeline that contradicts itself must keep failing closed. Caching the
// verdict of a failed audit would answer the second request from a remembered
// error, and — worse the other way round — caching the versions before the
// audit ran would let a later call select from a timeline already rejected.
func TestBrokenTimelineIsNeverCachedAsUsable(t *testing.T) {
	store := cachedTimelineStore(t, "dep_broken")
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	if _, err := store.CreateDeploymentPriceVersion(ctx, newStoredPrice("price_ok", "dep_broken", base)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SelectDeploymentPriceVersion(ctx, "dep_broken", at); err != nil {
		t.Fatalf("healthy timeline: %v", err)
	}

	// Written around the store, which is the only way to produce a timeline the
	// write paths refuse to create: two versions sharing a version number.
	forged := newStoredPrice("price_forged", "dep_broken", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	forged.Version, forged.Revision = 1, 1
	forged.CreatedAt = base
	if err := store.db.Update(func(tx *bbolt.Tx) error {
		return putDeploymentPriceVersionTx(tx, forged)
	}); err != nil {
		t.Fatal(err)
	}
	store.invalidateDeploymentPricingTimeline("dep_broken")

	if _, err := store.SelectDeploymentPriceVersion(ctx, "dep_broken", at); err == nil {
		t.Fatal("a timeline with a duplicate version number was selected from")
	}
	// And again: the refusal is re-derived, not a remembered verdict that a
	// repair would leave in place.
	if _, err := store.SelectDeploymentPriceVersion(ctx, "dep_broken", at); err == nil {
		t.Fatal("the second selection stopped failing closed")
	}
}

func TestSelectRejectsANonUTCInstant(t *testing.T) {
	store := cachedTimelineStore(t, "dep_zone")
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := store.CreateDeploymentPriceVersion(ctx, newStoredPrice("price_zone", "dep_zone", base)); err != nil {
		t.Fatal(err)
	}
	zone := time.FixedZone("UTC+8", 8*60*60)
	if _, err := store.SelectDeploymentPriceVersion(ctx, "dep_zone", time.Date(2026, 8, 1, 0, 0, 0, 0, zone)); err == nil {
		t.Fatal("a selection instant outside UTC was accepted")
	}
	if _, err := store.SelectDeploymentPriceVersion(ctx, "", base); err == nil {
		t.Fatal("an empty deployment id was accepted")
	}
}
