package statecut

import (
	"context"
	"testing"

	"github.com/akz142857/Halro/internal/ledger"
)

func TestCommonSelectedCompletedRollRestoresAllRootsAndPublishes(t *testing.T) {
	ctx := context.Background()
	f := newCommonFixture(t)
	o := prepareCommonSelectedReopenWithSuffix(t, f, func(native *ledger.Log, lease *ledger.MaintenanceLease) {
		if err := lease.Close(); err != nil {
			t.Fatal(err)
		}
		for i, kind := range []ledger.EventKind{ledger.EventRequestAccepted, ledger.EventReservationCreated, ledger.EventAttemptStarted, ledger.EventAttemptSettled, ledger.EventRequestFinalized} {
			e := ledger.Event{EventID: []string{"rolled_accept", "rolled_reserve", "rolled_start", "rolled_settle", "rolled_final"}[i], Kind: kind, RequestID: "req_rolled_delta", ProjectID: "prj_common", PeriodID: "2026-10-06", PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, OccurredAt: commonGovernanceEvent().IngestedAt}
			if i >= 1 && i <= 3 {
				e.AttemptID = "att_rolled_delta"
			}
			if i == 1 {
				v := int64(20)
				e.ReservationMicrosUSD = &v
			}
			if i == 3 {
				v := int64(12)
				e.CommittedMicrosUSD = &v
				e.Outcome = "success"
			}
			if i == 4 {
				e.Outcome = "success"
			}
			if _, err := native.Append(ctx, e); err != nil {
				t.Fatal(err)
			}
			if i == 0 || i == 3 {
				if _, err := native.Roll(); err != nil {
					t.Fatal(err)
				}
			}
		}
	})
	r, err := ReopenSelectedStandalone(ctx, o)
	if err != nil {
		t.Fatal("common generation after two completed post-seal Rolls", err)
	}
	defer r.Close()
	if err := r.WaitStartupAudit(ctx); err != nil {
		t.Fatal(err)
	}
	if r.LedgerReceipt.AppliedRecords != 5 {
		t.Fatal("saved history replayed or delta lost", r.LedgerReceipt)
	}
	summary, found, err := r.Owners.Usage.ReadRequest(ctx, "req_rolled_delta")
	if err != nil || !found || summary.CostMicrosUSD != 12 || summary.Outcome != "success" {
		t.Fatal("cross-generation Usage", summary, found, err)
	}
	directory, err := r.Owners.Directory.SealReference(ctx)
	if err != nil || directory.Root.Rows != 3 || directory.Position.Generation != 3 {
		t.Fatal("new authenticated directory descriptors", directory, err)
	}
	authority, err := CurrentStandaloneAuthority(ctx, r.Owners, o.Authority)
	if err != nil {
		t.Fatal(err)
	}
	base := f.base
	base.Revision++
	pub, err := PublishStandalone(ctx, r.Owners, base, authority)
	if err != nil || pub.Seal.Positions[0].Generation != 3 || pub.Seal.Roots[6] != directory.Root {
		t.Fatal("common selection lost rolled directory", pub, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	for _, adopt := range []func() error{
		func() error { return f.owners.Ledger.AdoptClosedSuccessor(r.Owners.Ledger) },
		func() error { return f.owners.Usage.AdoptClosedTypedSuccessor(r.Owners.Usage) },
		func() error { return f.owners.Governance.AdoptClosedTypedSuccessor(r.Owners.Governance) },
		func() error { return f.owners.Metadata.AdoptClosedSuccessor(r.Owners.Metadata) },
		func() error { return f.owners.Directory.AdoptClosedSuccessor(r.Owners.Directory) },
	} {
		if err := adopt(); err != nil {
			t.Fatal(err)
		}
	}
}
