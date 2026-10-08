package app

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"
)

func TestSelectedUsageWindowReferenceBuildsCompleteFactsAndRetiresActualFiles(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cfg, _, _, sample := selectedLegacyExportFixture(t, ctx, true)
	r, err := OpenWithOptions(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{offlineVerification: true, AllowGeneratedSetupToken: true, resourceSampleSource: sample})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
		t.Fatal(err)
	}
	lease, err := r.ledger.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	before := r.resourcePressure.logicalBudget().Snapshot().Used
	p, err := r.buildSelectedUsageWindowReference(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Discard()
	actual, err := p.ReadReplayAnchor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := r.selectedState.generation.Owners.Ledger.SealReference(ctx)
	if err != nil || actual != ledger.Anchor {
		t.Fatal("reference differs from actual selected Ledger cut", actual, ledger.Anchor, err)
	}
	request, found, err := p.ReadRequest(ctx, "req_before_legacy_export_migration")
	if err != nil || !found {
		t.Fatal("full native reference omitted pre-migration finalized request", found, err)
	}
	expected, found, err := r.usage.disk.ReadRequest(ctx, request.RequestID)
	if err != nil || !found || !reflect.DeepEqual(request, expected) {
		t.Fatal("fresh complete reference facts differ", request, expected, found, err)
	}
	if err := retireSelectedUsageWindowReference(p); err != nil {
		t.Fatal(err)
	}
	if after := r.resourcePressure.logicalBudget().Snapshot().Used; after != before {
		t.Fatal("actual reference retirement retained fees", before, after)
	}
	// Reuse the same fixed slot only after actual retirement. No saved candidate
	// is reopened, and no unbounded historical namespace is introduced.
	p, err = r.buildSelectedUsageWindowReference(ctx, lease)
	if err != nil {
		t.Fatal("retired fixed slot was not reusable", err)
	}
	if err := retireSelectedUsageWindowReference(p); err != nil {
		t.Fatal(err)
	}
}
