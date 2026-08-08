package budget

import (
	"context"
	"path/filepath"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

// A request that outlives the day it began in must still be charged to that
// day. Recomputing the period at settlement would reserve against one budget
// and commit against another: the first day would look under-spent forever and
// the second would absorb a charge it never authorised.
func TestLongRequestSettlesInThePeriodItBeganIn(t *testing.T) {
	log, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "usage.wal"), ledger.NewStatus(), ledger.Options{ChainKey: testChainKey})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	state := ledger.NewState()
	manager, err := New(log, state, mustResolver(t, "UTC"))
	if err != nil {
		t.Fatal(err)
	}

	// Accept the request minutes before midnight, settle it minutes after.
	before := time.Date(2026, time.August, 6, 23, 55, 0, 0, time.UTC)
	after := time.Date(2026, time.August, 7, 0, 5, 0, 0, time.UTC)
	clock := before
	manager.now = func() time.Time { return clock }

	request, err := manager.BeginRequest(context.Background(), "project_long", "request_long")
	if err != nil {
		t.Fatal(err)
	}
	if request.Period.ID != "2026-08-06" {
		t.Fatalf("request opened in period %s, want 2026-08-06", request.Period.ID)
	}

	snapshot := testPriceSnapshot(t, domain.BillingModeMetered)
	attempt, err := manager.ReserveLeaseDetailed(context.Background(), request, 1_000, LeaseSpec{
		Mode: ledger.LeaseModeMetered, ReservationMicrosUSD: 100, PriceSnapshot: snapshot,
		PreparedInputTokens: 10, PreparedOutputTokens: 20,
		TokenGuardPricingViewDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}, AttemptMetadata{DeploymentID: "dep_test", ProviderID: "provider_test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.MarkStarted(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}

	clock = after
	if got, err := manager.periods.PeriodAt(clock); err != nil || got.ID != "2026-08-07" {
		t.Fatalf("clock did not cross the boundary: period=%s err=%v", got.ID, err)
	}
	if err := manager.Settle(context.Background(), attempt, Settlement{
		CommittedMicrosUSD: 50, ProviderInputTokens: 10, ProviderOutputTokens: 20, Outcome: "success",
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Finalize(context.Background(), request, "success"); err != nil {
		t.Fatal(err)
	}

	opening := state.Balance("project_long", "2026-08-06", request.Period.TimezoneVersion)
	if opening.CommittedMicrosUSD != 50 {
		t.Fatalf("opening period committed %d, want 50 — the charge left the period it began in",
			opening.CommittedMicrosUSD)
	}
	closing := state.Balance("project_long", "2026-08-07", request.Period.TimezoneVersion)
	if closing.CommittedMicrosUSD != 0 || closing.ReservedMicrosUSD != 0 {
		t.Fatalf("the following period absorbed %#v from a request it never accepted", closing)
	}
	if opening.ReservedMicrosUSD != 0 {
		t.Fatalf("reservation left outstanding in the opening period: %#v", opening)
	}
}

// Every event of one request carries one period. A mismatch anywhere in the
// chain means the reservation and its settlement are keyed to different
// balances, which silently disables budget enforcement for that request.
func TestEveryEventOfARequestCarriesOnePeriod(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.wal")
	log, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: testChainKey})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := New(log, ledger.NewState(), mustResolver(t, "Asia/Shanghai"))
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, time.August, 6, 15, 59, 0, 0, time.UTC)
	manager.now = func() time.Time { return clock }

	request, err := manager.BeginRequest(context.Background(), "project_chain", "request_chain")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := testPriceSnapshot(t, domain.BillingModeMetered)
	attempt, err := manager.ReserveLeaseDetailed(context.Background(), request, 1_000, LeaseSpec{
		Mode: ledger.LeaseModeMetered, ReservationMicrosUSD: 100, PriceSnapshot: snapshot,
		PreparedInputTokens: 10, PreparedOutputTokens: 20,
		TokenGuardPricingViewDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}, AttemptMetadata{DeploymentID: "dep_test", ProviderID: "provider_test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.MarkStarted(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	// Cross the local midnight before settling.
	clock = clock.Add(2 * time.Minute)
	if err := manager.Settle(context.Background(), attempt, Settlement{
		CommittedMicrosUSD: 50, ProviderInputTokens: 10, ProviderOutputTokens: 20, Outcome: "success",
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Finalize(context.Background(), request, "success"); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	var seen int
	if _, partial, err := ledger.InspectReplay(path, func(record ledger.Record) error {
		event := record.Event
		if event.RequestID != "request_chain" {
			return nil
		}
		seen++
		if event.PeriodID != request.Period.ID ||
			event.PeriodTimezone != request.Period.Timezone ||
			event.PeriodTimezoneVersion != request.Period.TimezoneVersion ||
			event.PeriodStartMicros != request.Period.Start.UnixMicro() ||
			event.PeriodEndMicros != request.Period.End.UnixMicro() {
			t.Fatalf("event %s (kind %d) carries a different period: %s/%s/%d",
				event.EventID, event.Kind, event.PeriodID, event.PeriodTimezone, event.PeriodTimezoneVersion)
		}
		return nil
	}); err != nil || partial {
		t.Fatalf("replay partial=%t err=%v", partial, err)
	}
	// accepted, reservation, started, settled, finalized
	if seen != 5 {
		t.Fatalf("inspected %d events for the request, want 5", seen)
	}
}
