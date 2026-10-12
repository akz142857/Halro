package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// A selected (memory-state) install exports Governance from its authenticated
// Ledger projection through the operator's path (board §36 v7 found it had no
// source at all). The export must name the projection's cut and carry every
// fact in it, the closed WorkUnit as closed.
func TestSelectedRuntimeGovernanceExportPublishesAccountingCut(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	legacy, credentials, _ := openBootstrappedRuntimeForModel(t, "gpt-test")
	cfg, instance := legacy.config, legacy.instanceID
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	cfg = activateClosedRuntimeMemoryStateFixture(t, ctx, cfg, instance, false)
	opts := OpenOptions{AllowGeneratedSetupToken: true, resourceSampleSource: pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() })}
	r, err := OpenWithOptions(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	}()
	if r.selectedState == nil {
		t.Fatal("fixture did not open a selected Runtime")
	}
	if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
		t.Fatal(err)
	}
	project, err := r.store.GetProject(ctx, credentials.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	definition := domain.OutcomeDefinition{ID: "odef_selected", ProjectID: credentials.ProjectID, Name: "selected", Version: 1, DataType: domain.OutcomeCategorical, AllowedValues: []string{"accepted", "rejected"}, SuccessValues: []string{"accepted"}, Enabled: true, CreatedAt: now, CreatedBy: "admin"}
	if definition, err = r.store.PutOutcomeDefinition(ctx, definition, project.Revision, 0, nil); err != nil {
		t.Fatal(err)
	}
	intent := func(label string) budget.GovernanceIntent {
		return budget.GovernanceIntent{Operation: label, IdempotencyKeyHash: outcomePageTestLabel(label), RequestFingerprint: outcomePageTestLabel("fp-" + label)}
	}
	const units = 3
	closed := ""
	for i := 0; i < units; i++ {
		w, _, err := r.accounting.CreateWorkUnitWithDefinitions(ctx, credentials.ProjectID, credentials.KeyID, 20, []domain.OutcomeDefinitionRef{{ID: definition.ID, Version: 1}}, intent(fmt.Sprintf("selected-unit-%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := r.accounting.CreateRun(ctx, credentials.ProjectID, credentials.KeyID, w.ID, 1000, time.Hour, 20, intent(fmt.Sprintf("selected-run-%d", i))); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			closed = w.ID
			if _, _, err := r.accounting.CloseWorkUnit(ctx, credentials.ProjectID, credentials.KeyID, w.ID, intent("selected-close")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, _, err := r.governance.manager.Report(ctx, governance.ReportInput{ProjectID: credentials.ProjectID, WorkUnitID: closed, DefinitionID: definition.ID, ReporterKeyID: credentials.KeyID, Value: "accepted", ObservedAt: now, Intent: governance.Intent{IdempotencyKeyHash: outcomePageTestLabel("selected-outcome"), RequestFingerprint: outcomePageTestLabel("fp-selected-outcome")}}); err != nil {
		t.Fatal(err)
	}
	head, _, ok := r.ledger.ChainHead()
	if !ok {
		t.Fatal("native chain head unavailable")
	}
	directory := filepath.Join(r.config.GovernanceExportPath(), "gex_selected")
	manifest, err := r.writeGovernanceExport(ctx, directory)
	if err != nil {
		t.Fatal("selected Governance export", err)
	}
	verified, err := governance.VerifyExport(directory)
	if err != nil {
		t.Fatal(err)
	}
	if verified.AccountingWatermark != manifest.AccountingWatermark || verified.AccountingWatermark != head {
		t.Fatal("export did not name the projection cut", verified.AccountingWatermark, manifest.AccountingWatermark, head)
	}
	want := map[string]int{"work_units": units, "runs": units, "outcomes": 1, "outcome_definitions": 1}
	for _, file := range verified.Files {
		if n, ok := want[file.Dataset]; ok && file.RecordCount != n {
			t.Fatal("dataset count", file.Dataset, file.RecordCount, n)
		}
		delete(want, file.Dataset)
	}
	if len(want) != 0 {
		t.Fatal("datasets missing from the export", want)
	}
	if r.status.Load() != ledger.AccountingHealthy {
		t.Fatal("export changed accounting health", r.status.Load())
	}
}
