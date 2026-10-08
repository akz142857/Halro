package statecut

import (
	"context"
	"testing"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/usage"
)

func TestCommonSelectedGenerationContinuesActualLiveCollectorAfterStartupLease(t *testing.T) {
	f := newCommonFixture(t)
	o := prepareCommonSelectedReopen(t, f)
	ctx := context.Background()
	r, err := ReopenSelectedStandalone(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.WaitStartupAudit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Owners.Usage.ActivateTypedLive(ctx, r.Owners.NativeLedger, r.Owners.Budget); err != nil {
		t.Fatal(err)
	}
	if err := r.Owners.Lease.Close(); err != nil {
		t.Fatal(err)
	}
	collector, err := usage.NewTypedLiveCollector(r.Derivative, r.Owners.NativeLedger, r.Owners.Budget, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer collector.Retire(ctx)
	e := ledger.Event{EventID: "evt_selected_live", Kind: ledger.EventRequestAccepted, RequestID: "req_selected_live", ProjectID: "prj_common", PeriodID: "2026-10-06", PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, OccurredAt: commonGovernanceEvent().IngestedAt}
	record, _, err := r.Owners.NativeLedger.AppendApplyRecordWithReceipt(ctx, e, func(ctx context.Context, record ledger.Record) error {
		_, err := r.Owners.Ledger.AdvanceOneFromNative(ctx, r.Owners.NativeLedger, ledger.ProjectionPrefix{Head: ledger.Watermark{Generation: record.Generation, Sequence: record.Sequence, Offset: record.Offset}, Hash: record.Hash})
		return err
	})
	if err != nil {
		t.Fatal("actual selected durable apply", err)
	}
	collector.Observe(record)
	if err := collector.CatchUp(ctx); err != nil {
		t.Fatal("actual selected routine source", err)
	}
	anchor, err := r.Owners.Usage.ReadReplayAnchor(ctx)
	if err != nil || anchor.Head.Sequence != record.Sequence || anchor.Hash != record.Hash {
		t.Fatal(anchor, record, err)
	}
	before, err := r.Owners.Usage.TypedSealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if receipt, err := r.Derivative.CatchUpTypedLive(ctx, r.Owners.NativeLedger, r.Owners.Budget); err != nil || receipt.AppliedRecords != 0 {
		t.Fatal(receipt, err)
	}
	if after, err := r.Owners.Usage.TypedSealReference(ctx); err != nil || after != before {
		t.Fatal("empty retry changed complete root", err)
	}
	if err := collector.Retire(ctx); err != nil {
		t.Fatal(err)
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
