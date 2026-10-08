package statecut

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/usage"
)

func retireSelectedFixture(t *testing.T, f *commonFixture, r *ReopenedStandalone) {
	t.Helper()
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

func TestCommonSelectedServicesRecoverThenUseActualBusinessOwners(t *testing.T) {
	f := newCommonFixture(t)
	ctx := context.Background()
	// Real accepted-but-unfinalized native suffix; saved roots still represent
	// the common seal. The factory reduces it, then startup finalizes it once.
	e := ledger.Event{EventID: "evt_services_orphan", Kind: ledger.EventRequestAccepted, RequestID: "req_services_orphan", ProjectID: "prj_common", PeriodID: "2026-10-06", PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, OccurredAt: commonGovernanceEvent().IngestedAt}
	o := prepareCommonSelectedReopenWithSuffix(t, f, func(_ *ledger.Log, lease *ledger.MaintenanceLease) {
		if _, _, err := lease.AppendApply(ctx, e, func(context.Context, ledger.Record) error { return nil }); err != nil {
			t.Fatal(err)
		}
	})
	r, err := ReopenSelectedStandalone(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { retireSelectedFixture(t, f, r) })
	s, err := r.StartSelectedServices(ctx, SelectedServicesOptions{Authority: o.Authority, Timezone: "UTC", QueueCapacity: 4, Accounting: budget.Options{Now: func() time.Time { return commonGovernanceEvent().IngestedAt }}})
	if err != nil {
		t.Fatal("selected service startup", err)
	}
	head, _, _ := o.Sources.NativeLedger.ChainHead()
	if s.Recovery.RequestsFinalized != 1 || s.Recovery.AttemptsSettled != 0 || s.Recovery.ConfirmedHead != head {
		t.Fatal("two-level recovery", s.Recovery)
	}
	parent, found, err := r.Owners.Ledger.ReadRequest(ctx, e.RequestID)
	if err != nil || !found || parent.Finalized == nil {
		t.Fatal(parent, found, err)
	}
	project := domain.Project{ID: "prj_services", Name: "Actual selected DAO", Enabled: true}
	if _, err := s.Metadata.PutProject(ctx, project, 0, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Metadata.GetProject(ctx, project.ID); err != nil || got.Name != project.Name {
		t.Fatal(got, err)
	}
	request, err := s.Accounting.BeginRequestDetailed(ctx, project.ID, "key_services", "req_services", "model_services")
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := s.Accounting.ReserveAttempt(ctx, request, 30, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Accounting.MarkStarted(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	if err := s.Accounting.Settle(ctx, attempt, budget.Settlement{CommittedMicrosUSD: 9, Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Accounting.Finalize(ctx, request, "success"); err != nil {
		t.Fatal(err)
	}
	if err := s.Collector.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	balance, err := s.Accounting.ReadBalance(ctx, ledger.BalanceKey{ProjectID: request.ProjectID, PeriodID: request.Period.ID, TimezoneVersion: request.Period.TimezoneVersion})
	if err != nil || balance.CommittedMicrosUSD != 9 || balance.ReservedMicrosUSD != 0 {
		t.Fatal(balance, err)
	}
	if summary, found, err := r.Owners.Usage.ReadRequest(ctx, request.RequestID); err != nil || !found || summary.CostMicrosUSD != 9 {
		t.Fatal(summary, found, err)
	}
	if ready, err := s.Governance.Ready(); err != nil || !ready {
		t.Fatal(ready, err)
	}
	storedProject, err := s.Metadata.GetProject(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	definition := domain.OutcomeDefinition{ID: "odef_services", ProjectID: project.ID, Name: "accepted", Version: 1, DataType: domain.OutcomeCategorical, AllowedValues: []string{"accepted", "rejected"}, SuccessValues: []string{"accepted"}, Enabled: true, CreatedAt: commonGovernanceEvent().IngestedAt, CreatedBy: "admin"}
	definition, err = s.Metadata.PutOutcomeDefinition(ctx, definition, storedProject.Revision, 0, nil)
	if err != nil {
		t.Fatal("actual authenticated definition DAO", err)
	}
	unit, _, err := s.Accounting.CreateWorkUnitWithDefinitions(ctx, project.ID, "key_services", 4, []domain.OutcomeDefinitionRef{{ID: definition.ID, Version: definition.Version}}, budget.GovernanceIntent{Operation: "work_unit.create", IdempotencyKeyHash: "sha256:" + strings.Repeat("3", 64), RequestFingerprint: "sha256:" + strings.Repeat("4", 64)})
	if err != nil {
		t.Fatal("actual WorkUnit accounting", err)
	}
	input := governance.ReportInput{ProjectID: project.ID, WorkUnitID: unit.ID, DefinitionID: definition.ID, Value: "accepted", ReporterKeyID: "key_services", ObservedAt: commonGovernanceEvent().IngestedAt, Intent: governance.Intent{IdempotencyKeyHash: "sha256:" + strings.Repeat("5", 64), RequestFingerprint: "sha256:" + strings.Repeat("6", 64)}}
	outcome, replayed, err := s.Governance.Report(ctx, input)
	if err != nil || replayed {
		t.Fatal("actual direct Governance Apply/checkpoint", outcome, replayed, err)
	}
	if again, replayed, err := s.Governance.Report(ctx, input); err != nil || !replayed || again.ID != outcome.ID {
		t.Fatal("actual Governance idempotency", again, replayed, err)
	}
	if stored, found, err := r.Owners.Governance.ReadOutcome(ctx, outcome.ID); err != nil || !found || stored.Value != outcome.Value {
		t.Fatal(stored, found, err)
	}
	if anchor, err := r.Owners.Governance.ReadHead(ctx); err != nil || anchor != r.Owners.NativeGovernance.Summary() {
		t.Fatal("actual durable content-root checkpoint", anchor, err)
	}
	if _, err := s.Metadata.GovernanceJournalAnchor(); !errors.Is(err, boltstore.ErrNotFound) {
		t.Fatal("node-derived anchor entered authoritative Metadata journal", err)
	}
	if duplicate, err := r.StartSelectedServices(ctx, SelectedServicesOptions{Authority: o.Authority, Timezone: "UTC", QueueCapacity: 4}); duplicate != nil || err == nil {
		t.Fatal("duplicate service startup", duplicate, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Accounting.BeginRequest(ctx, project.ID, "after_close"); !errors.Is(err, budget.ErrAccountingRetired) {
		t.Fatal(err)
	}
	if ready, err := s.Governance.Ready(); ready || !errors.Is(err, governance.ErrRetired) {
		t.Fatal(ready, err)
	}
	if err := s.Collector.CatchUp(ctx); !errors.Is(err, usage.ErrCollectorRetired) {
		t.Fatal(err)
	}
	if _, err := s.Metadata.GetProject(ctx, project.ID); err == nil {
		t.Fatal("closed common generation retained DAO")
	}
}

func TestCommonSelectedServicesRejectUnselectedAuthorityBeforeRecovery(t *testing.T) {
	f := newCommonFixture(t)
	o := prepareCommonSelectedReopen(t, f)
	ctx := context.Background()
	r, err := ReopenSelectedStandalone(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { retireSelectedFixture(t, f, r) })
	before := o.Sources.NativeLedger.Stats().Records
	wrong := o.Authority
	wrong.Positions[0].Sequence++
	if s, err := r.StartSelectedServices(ctx, SelectedServicesOptions{Authority: wrong, Timezone: "UTC", QueueCapacity: 4}); s != nil || err == nil {
		t.Fatal(s, err)
	}
	if o.Sources.NativeLedger.Stats().Records != before || r.services != nil || r.closed {
		t.Fatal("authority rejection mutated healthy generation")
	}
}

func TestCommonSelectedServicesCloseJoinsActualAccountingObserver(t *testing.T) {
	f := newCommonFixture(t)
	o := prepareCommonSelectedReopen(t, f)
	ctx := context.Background()
	r, err := ReopenSelectedStandalone(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { retireSelectedFixture(t, f, r) })
	s, err := r.StartSelectedServices(ctx, SelectedServicesOptions{Authority: o.Authority, Timezone: "UTC", QueueCapacity: 4})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	s.Accounting.AddObserver(func(ledger.Record) { close(entered); <-release })
	work := make(chan error, 1)
	go func() { _, err := s.Accounting.BeginRequest(ctx, "prj_join", "req_join"); work <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("actual accepted observer did not enter")
	}
	done := make(chan error, 1)
	go func() { done <- r.Close() }()
	// The accepted callback can still use the actual root while Close waits.
	select {
	case err := <-done:
		t.Fatal("root owner closed ahead of accepted callback", err)
	case <-time.After(30 * time.Millisecond):
	}
	if _, found, err := r.Owners.Ledger.ReadRequest(ctx, "req_join"); err != nil || !found {
		t.Fatal("root retired under accepted observer", found, err)
	}
	close(release)
	if err := <-work; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("whole generation close did not join actual work")
	}
	if _, err := s.Accounting.BeginRequest(ctx, "prj_join", "req_new"); !errors.Is(err, budget.ErrAccountingRetired) {
		t.Fatal(err)
	}
}

func TestCommonSelectedServicesBudgetRefusalClosesGenerationWithoutRecoveryWrites(t *testing.T) {
	f := newCommonFixture(t)
	o := prepareCommonSelectedReopen(t, f)
	ctx := context.Background()
	r, err := ReopenSelectedStandalone(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { retireSelectedFixture(t, f, r) })
	if err := r.WaitStartupAudit(ctx); err != nil {
		t.Fatal(err)
	}
	before := o.Sources.NativeLedger.Stats().Records
	// Leave only 16 MiB of ordinary RAM: the actual authenticated recovery
	// view requires 64 MiB before reading or appending a single input.
	hold, err := o.Sources.Budget.TryAcquire(resourcebudget.IO, resourcebudget.Cost{RAM: (1 << 30) - o.Sources.Budget.Snapshot().Normal.RAM - (24 << 20)})
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Release()
	if s, err := r.StartSelectedServices(ctx, SelectedServicesOptions{Authority: o.Authority, Timezone: "UTC", QueueCapacity: 4}); s != nil || !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatal(s, err)
	}
	if !r.closed || o.Sources.NativeLedger.Stats().Records != before {
		t.Fatal("refused startup retained admission or appended", r.closed)
	}
	if _, _, err := r.Owners.Ledger.ReadRequest(ctx, "req_common"); err == nil {
		t.Fatal("failed startup kept an active reader")
	}
}
