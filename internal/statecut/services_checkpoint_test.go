package statecut

import (
	"context"
	"testing"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/domain"
)

func TestCommonSelectedServicesBusinessCheckpointPublishesAdvancedSevenRoots(t *testing.T) {
	f := newCommonFixture(t)
	o := prepareCommonSelectedReopen(t, f)
	ctx := context.Background()
	r, err := ReopenSelectedStandalone(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	firstRetired := false
	t.Cleanup(func() {
		if !firstRetired {
			retireSelectedFixture(t, f, r)
		}
	})
	s, err := r.StartSelectedServices(ctx, SelectedServicesOptions{Authority: o.Authority, Timezone: "UTC", QueueCapacity: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Metadata.PutProject(ctx, domain.Project{ID: "prj_checkpoint", Name: "Business checkpoint", Enabled: true}, 0, nil); err != nil {
		t.Fatal(err)
	}
	request, err := s.Accounting.BeginRequestDetailed(ctx, "prj_checkpoint", "key_checkpoint", "req_checkpoint", "model")
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := s.Accounting.ReserveAttempt(ctx, request, 20, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Accounting.Settle(ctx, attempt, budget.Settlement{CommittedMicrosUSD: 7, Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Accounting.Finalize(ctx, request, "success"); err != nil {
		t.Fatal(err)
	}
	before := r.Seal
	caches := persistStartupCachesAt(t, r.Owners, o.Authority.Instance, f.dir, "business")
	publication, err := r.CheckpointSelectedServices(ctx, caches)
	if err != nil {
		t.Fatal("actual service-to-common-seal handoff", err)
	}
	if publication.Seal.Revision != before.Revision+1 || publication.Seal.RootCount != 7 || publication.Seal.Positions[0].Sequence <= before.Positions[0].Sequence || publication.Seal.Positions[1].Sequence <= before.Positions[1].Sequence || publication.Seal.Positions[4].Sequence != publication.Seal.Positions[0].Sequence {
		t.Fatal("business facts did not reach the complete common cut", publication.Seal)
	}
	if summary, found, err := r.Owners.Usage.ReadRequest(ctx, request.RequestID); err != nil || !found || summary.CostMicrosUSD != 7 {
		t.Fatal("last business fact missing from quiescent Usage", summary, found, err)
	}
	if _, err := s.Accounting.BeginRequestDetailed(ctx, "prj_checkpoint", "key_checkpoint", "req_late", "model"); err == nil {
		t.Fatal("retired business writer remained admitted after checkpoint")
	}
	if _, err := s.Metadata.GetProject(ctx, "prj_checkpoint"); err == nil {
		t.Fatal("retired DAO remained admitted after checkpoint")
	}
	if _, err := r.Owners.Metadata.SealReference(ctx); err != nil {
		t.Fatal("DAO retirement prematurely closed the actual Metadata root", err)
	}
	head, err := r.Owners.Metadata.AppliedHead()
	if err != nil {
		t.Fatal(err)
	}
	retireSelectedFixture(t, f, r)
	firstRetired = true
	for _, close := range []func() error{r.Owners.Lease.Close, r.Owners.NativeLedger.Close, r.Owners.Audit.Close, r.Owners.NativeGovernance.Close} {
		if err := close(); err != nil {
			t.Fatal(err)
		}
	}
	next := o.ReopenOptions
	next.Authority.Positions = [5]authenticatedstate.SealPosition{publication.Seal.Positions[0], publication.Seal.Positions[1], publication.Seal.Positions[2], publication.Seal.Positions[3], publication.Seal.Positions[4]}
	next.MetadataOptions.SavedHead, next.MetadataOptions.NativeHead, next.MetadataOptions.PermittedHead = head, head, head
	selectedNext := openCommonSelectedSources(t, f, next, caches.Audit.Path(), caches.Governance.Path())
	reopened, err := ReopenSelectedStandalone(ctx, selectedNext)
	if err != nil {
		t.Fatal("reopen the actual business checkpoint", err)
	}
	t.Cleanup(func() { retireSelectedFixture(t, f, reopened) })
	services, err := reopened.StartSelectedServices(ctx, SelectedServicesOptions{Authority: next.Authority, Timezone: "UTC", QueueCapacity: 4})
	if err != nil {
		t.Fatal("second generation business readiness", err)
	}
	if services.Recovery.RequestsFinalized != 0 || services.Recovery.AttemptsSettled != 0 || reopened.LedgerReceipt.AppliedRecords != 0 || reopened.MetadataReceipt.AppliedFrames != 0 {
		t.Fatal("reopening replayed finalized business facts", services.Recovery, reopened.LedgerReceipt, reopened.MetadataReceipt)
	}
	if project, err := services.Metadata.GetProject(ctx, "prj_checkpoint"); err != nil || project.Name != "Business checkpoint" {
		t.Fatal("second generation lost business metadata", project, err)
	}
	if summary, found, err := reopened.Owners.Usage.ReadRequest(ctx, request.RequestID); err != nil || !found || summary.CostMicrosUSD != 7 {
		t.Fatal("second generation lost final Usage", summary, found, err)
	}
	if _, err := services.Accounting.BeginRequestDetailed(ctx, "prj_checkpoint", "key_checkpoint", "req_second_generation", "model"); err != nil {
		t.Fatal("second generation cannot process new business", err)
	}
}
