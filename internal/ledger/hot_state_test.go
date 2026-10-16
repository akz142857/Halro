package ledger

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// A settled attempt leaves the state: its lease goes with the reservation, and
// what governance reads of it stays behind as the Work Unit's totals.
func TestSettlementLeavesOnlyTheWorkUnitTotals(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	state := NewState()
	sequence := uint64(0)
	apply := func(event Event) {
		t.Helper()
		sequence++
		if err := state.Apply(Record{Generation: 1, Sequence: sequence, Offset: int64(sequence * 100), Epoch: frameVersionRunAttribution, Event: event}); err != nil {
			t.Fatalf("apply %d: %v", sequence, err)
		}
	}
	workUnit := governanceEvent("evt_wu", EventWorkUnitCreated, now)
	workUnit.WorkUnitID = "wku_1"
	apply(workUnit)
	runCreated := governanceEvent("evt_run", EventRunCreated, now)
	runCreated.WorkUnitID, runCreated.RunID = "wku_1", "run_1"
	runCreated.RunBudgetMicrosUSD, runCreated.RunExpiresAt = 10_000, now.Add(time.Hour)
	apply(runCreated)
	accepted := governanceEvent("evt_req", EventRequestAccepted, now)
	accepted.RequestID, accepted.WorkUnitID, accepted.RunID = "req_1", "wku_1", "run_1"
	apply(accepted)
	settle := func(attemptID string, committed *int64, estimated bool) {
		t.Helper()
		reserved := governanceEvent("evt_res_"+attemptID, EventReservationCreated, now)
		reserved.RequestID, reserved.AttemptID = "req_1", attemptID
		reserved.WorkUnitID, reserved.RunID = "wku_1", "run_1"
		reserved.ReservationMicrosUSD = MicrosUSD(100)
		apply(reserved)
		if _, ok := state.AccountingLease(attemptID); !ok {
			t.Fatalf("in-flight attempt %s has no lease", attemptID)
		}
		settled := governanceEvent("evt_settle_"+attemptID, EventAttemptSettled, now)
		settled.RequestID, settled.AttemptID = "req_1", attemptID
		settled.WorkUnitID, settled.RunID = "wku_1", "run_1"
		settled.CommittedMicrosUSD, settled.CostEstimated = committed, estimated
		apply(settled)
		if _, ok := state.AccountingLease(attemptID); ok {
			t.Fatalf("settled attempt %s kept its lease", attemptID)
		}
	}
	settle("att_1", MicrosUSD(70), false)
	settle("att_2", MicrosUSD(30), true)

	snapshot, err := state.GovernanceSnapshot(context.Background(), GovernanceSnapshotOptions{ProjectID: "prj_1", IncludeCosts: true})
	if err != nil {
		t.Fatal(err)
	}
	want := WorkUnitCost{KnownMicrosUSD: 100, EstimatedMicrosUSD: 30}
	if got := snapshot.Costs["wku_1"]; got != want {
		t.Fatalf("work unit cost = %#v, want %#v", got, want)
	}
	if len(state.leaseRecords) != 0 || len(state.reservations) != 0 {
		t.Fatalf("leases=%d reservations=%d after every attempt settled", len(state.leaseRecords), len(state.reservations))
	}
}

// An unknown cost is counted, not added.
func TestWorkUnitCostCountsUnknownAttempts(t *testing.T) {
	var cost WorkUnitCost
	cost.add(Event{LeaseMode: LeaseModeUnknownAllowed})
	cost.add(Event{CommittedMicrosUSD: MicrosUSD(5)})
	if want := (WorkUnitCost{KnownMicrosUSD: 5, UnknownAttempts: 1}); cost != want {
		t.Fatalf("cost = %#v, want %#v", cost, want)
	}
}

// A total that passes the int64 range is reported, and the settlement that
// caused it still lands: the totals are derived, the balance is not.
func TestWorkUnitCostOverflowIsReportedNotRefused(t *testing.T) {
	cost := WorkUnitCost{KnownMicrosUSD: 1<<63 - 10}
	cost.add(Event{CommittedMicrosUSD: MicrosUSD(20)})
	if !cost.Overflowed {
		t.Fatalf("overflow not reported: %#v", cost)
	}
}

// The digest window is bounded, and a duplicate inside it is still recognised.
func TestEventDigestWindowIsBounded(t *testing.T) {
	state := NewState()
	for index := 0; index < recentEventDigests+10; index++ {
		state.rememberDigest(fmt.Sprintf("evt_%d", index), [32]byte{byte(index)})
	}
	if len(state.eventDigests) != recentEventDigests || len(state.eventOrder) != recentEventDigests {
		t.Fatalf("digests=%d order=%d, want %d", len(state.eventDigests), len(state.eventOrder), recentEventDigests)
	}
	if _, ok := state.eventDigests["evt_0"]; ok {
		t.Fatal("the oldest digest was not forgotten")
	}
	if _, ok := state.eventDigests[fmt.Sprintf("evt_%d", recentEventDigests+9)]; !ok {
		t.Fatal("the newest digest is missing")
	}
}
