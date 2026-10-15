package bolt

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	bbolt "go.etcd.io/bbolt"
)

// pinnedAttempt is one attempt priced through a pin: the pin is prepared in the
// store and the lease and settlement records are returned for the test to
// apply or replay.
func pinnedAttempt(t *testing.T, store *Store, attemptID string, sequence uint64, outcome string) (lease, settlement ledger.Record) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	_, snapshot, _, err := store.PrepareDeploymentPricePin(ctx, "dep_hot", attemptID, now, time.Second, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	leaseEvent := ledger.Event{
		EventID: "evt_lease_" + attemptID, Kind: ledger.EventReservationCreated, RequestID: "req_" + attemptID,
		AttemptID: attemptID, ProjectID: "prj_pin", PeriodID: "2026-08-04", OccurredAt: now,
		ReservationMicrosUSD: ledger.MicrosUSD(1), LeaseMode: ledger.LeaseModeMetered, PriceSnapshot: &snapshot,
		TokenGuardPricingViewDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	settled := leaseEvent
	settled.EventID, settled.Kind = "evt_settle_"+attemptID, ledger.EventAttemptSettled
	settled.ReservationMicrosUSD, settled.TokenGuardPricingViewDigest = nil, ""
	settled.CommittedMicrosUSD, settled.Outcome = ledger.MicrosUSD(0), outcome
	return ledger.Record{Generation: 1, Sequence: sequence, Offset: int64(sequence * 100), Event: leaseEvent},
		ledger.Record{Generation: 1, Sequence: sequence + 1, Offset: int64(sequence*100 + 50), Event: settled}
}

func openHotStatePricing(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	seedPricingDeployment(t, store, "dep_hot", 0, 0, 0)
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	if _, err := store.CreateDeploymentPriceVersion(context.Background(), newStoredPrice("price_hot", "dep_hot", now.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	return store
}

func pinState(t *testing.T, store *Store, attemptID string) (domain.PricePinIntent, bool) {
	t.Helper()
	var pin domain.PricePinIntent
	found := false
	if err := store.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketDeploymentPricePins).Get([]byte(attemptID))
		if raw == nil {
			return nil
		}
		found = true
		return json.Unmarshal(raw, &pin)
	}); err != nil {
		t.Fatal(err)
	}
	return pin, found
}

// The state keeps no settled leases, except one settled because its pin could
// not commit: that pin is still prepared, and the startup that resolves it
// must find the lease and commit it rather than delete a pin an attempt was
// priced from — deleting it would let the price it names be cancelled.
func TestPinLeftPreparedByAFailedCommitIsCommittedAtStartup(t *testing.T) {
	store := openHotStatePricing(t)
	state := ledger.NewState()
	lease, settlement := pinnedAttempt(t, store, "att_failed_commit", 1, ledger.OutcomePinCommitFailed)
	for _, record := range []ledger.Record{lease, settlement} {
		if err := state.Apply(record); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RecoverDeploymentPricePins(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	pin, found := pinState(t, store, "att_failed_commit")
	if !found || pin.State != domain.PricePinCommitted || pin.LedgerSequence != lease.Sequence {
		t.Fatalf("pin = %#v found=%t, want committed at %d", pin, found, lease.Sequence)
	}
}

// A committed pin whose attempt has settled has no lease left in the state,
// and startup leaves it alone instead of refusing to start.
func TestCommittedPinOfASettledAttemptPassesStartup(t *testing.T) {
	store := openHotStatePricing(t)
	state := ledger.NewState()
	lease, settlement := pinnedAttempt(t, store, "att_settled", 1, "success")
	if _, err := store.CommitDeploymentPricePin(context.Background(), "att_settled", mustDigest(t, lease.Event.PriceSnapshot), lease.Sequence, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, record := range []ledger.Record{lease, settlement} {
		if err := state.Apply(record); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := state.AccountingLease("att_settled"); ok {
		t.Fatal("a settled attempt kept its lease")
	}
	if err := store.RecoverDeploymentPricePins(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if pin, found := pinState(t, store, "att_settled"); !found || pin.State != domain.PricePinCommitted {
		t.Fatalf("pin = %#v found=%t", pin, found)
	}
}

func mustDigest(t *testing.T, snapshot *domain.PriceSnapshot) string {
	t.Helper()
	digest, err := snapshot.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

// The backup check reads the Ledger as it is replayed: every committed pin
// must meet its lease on the way, and none may be left prepared.
func TestPriceReferenceCheckStreamsTheLedger(t *testing.T) {
	ctx := context.Background()
	t.Run("consistent", func(t *testing.T) {
		store := openHotStatePricing(t)
		lease, settlement := pinnedAttempt(t, store, "att_ok", 1, "success")
		if _, err := store.CommitDeploymentPricePin(ctx, "att_ok", mustDigest(t, lease.Event.PriceSnapshot), lease.Sequence, time.Now()); err != nil {
			t.Fatal(err)
		}
		check := store.BeginPriceReferenceCheck()
		for _, record := range []ledger.Record{lease, settlement} {
			if err := check.Observe(record); err != nil {
				t.Fatal(err)
			}
		}
		if err := check.Finish(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("committed pin without its lease", func(t *testing.T) {
		store := openHotStatePricing(t)
		lease, _ := pinnedAttempt(t, store, "att_lost", 1, "success")
		if _, err := store.CommitDeploymentPricePin(ctx, "att_lost", mustDigest(t, lease.Event.PriceSnapshot), lease.Sequence, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := store.BeginPriceReferenceCheck().Finish(); err == nil || !strings.Contains(err.Error(), "no authoritative Ledger lease") {
			t.Fatalf("a committed pin with no lease passed: %v", err)
		}
	})
	t.Run("pin naming another sequence", func(t *testing.T) {
		store := openHotStatePricing(t)
		lease, _ := pinnedAttempt(t, store, "att_moved", 1, "success")
		if _, err := store.CommitDeploymentPricePin(ctx, "att_moved", mustDigest(t, lease.Event.PriceSnapshot), lease.Sequence+7, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := store.BeginPriceReferenceCheck().Observe(lease); err == nil || !strings.Contains(err.Error(), "conflicts with Ledger") {
			t.Fatalf("a pin naming another lease passed: %v", err)
		}
	})
	t.Run("prepared pin", func(t *testing.T) {
		store := openHotStatePricing(t)
		pinnedAttempt(t, store, "att_prepared", 1, "success")
		if err := store.BeginPriceReferenceCheck().Finish(); err == nil || !strings.Contains(err.Error(), "unresolved") {
			t.Fatalf("a prepared pin passed: %v", err)
		}
	})
}
