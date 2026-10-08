package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
)

func TestGovernanceSummaryAcrossFiniteOutcomeBatches(t *testing.T) {
	for _, projected := range []bool{false, true} {
		t.Run(map[bool]string{false: "state", true: "disk"}[projected], func(t *testing.T) {
			r, bootstrap, _ := openBootstrappedRuntime(t)
			defer r.Close()
			var p *governance.GovernanceProjection
			if projected {
				p = installTestGovernanceProjection(t, r)
			}
			ctx := context.Background()
			project, err := r.store.GetProject(ctx, bootstrap.ProjectID)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			definition := domain.OutcomeDefinition{ID: "odef_batches", ProjectID: bootstrap.ProjectID, Name: "batches", Version: 1, DataType: domain.OutcomeCategorical, AllowedValues: []string{"accepted", "rejected"}, SuccessValues: []string{"accepted"}, Enabled: true, CreatedAt: now, CreatedBy: "admin"}
			if _, err := r.store.PutOutcomeDefinition(ctx, definition, project.Revision, 0, nil); err != nil {
				t.Fatal(err)
			}
			intent := func(label string) budget.GovernanceIntent {
				return budget.GovernanceIntent{Operation: label, IdempotencyKeyHash: outcomePageTestLabel(label), RequestFingerprint: outcomePageTestLabel("fp-" + label)}
			}
			var period string
			for i := 0; i < 70; i++ {
				w, _, err := r.accounting.CreateWorkUnitWithDefinitions(ctx, bootstrap.ProjectID, bootstrap.KeyID, 10, []domain.OutcomeDefinitionRef{{ID: definition.ID, Version: 1}}, intent(fmt.Sprintf("wu-%d", i)))
				if err != nil {
					t.Fatal(err)
				}
				period = w.PeriodID
				value := "accepted"
				if i%2 == 1 {
					value = "rejected"
				}
				label := fmt.Sprintf("outcome-%d", i)
				if _, _, err := r.governance.manager.Report(ctx, governance.ReportInput{ProjectID: bootstrap.ProjectID, WorkUnitID: w.ID, DefinitionID: definition.ID, Value: value, ReporterKeyID: bootstrap.KeyID, ObservedAt: now, Intent: governance.Intent{IdempotencyKeyHash: outcomePageTestLabel(label), RequestFingerprint: outcomePageTestLabel("fp-" + label)}}); err != nil {
					t.Fatal(err)
				}
				if _, _, err := r.accounting.CloseWorkUnit(ctx, bootstrap.ProjectID, bootstrap.KeyID, w.ID, intent(fmt.Sprintf("close-%d", i))); err != nil {
					t.Fatal(err)
				}
			}
			query := "/admin/api/v1/governance/summary?project_id=" + bootstrap.ProjectID + "&definition_id=" + definition.ID + "&definition_version=1&cohort_start=" + period + "&cohort_end=" + period
			response := httptest.NewRecorder()
			r.adminGovernanceSummary(response, httptest.NewRequest(http.MethodGet, query, nil))
			var result governanceSummary
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil {
				t.Fatal(response.Code, response.Body.String())
			}
			if result.EligibleUnits != 70 || result.MaturedUnits != 70 || result.EvaluatedUnits != 70 || result.SuccessfulUnits != 35 || result.OutcomeCompleteness != "complete" || result.OutcomeCoverage == nil || *result.OutcomeCoverage != 1 || result.SuccessRate == nil || *result.SuccessRate != 0.5 || result.KnownCostMicrosUSD != 0 {
				t.Fatal(result)
			}
			if p != nil {
				if err := p.Close(); err != nil {
					t.Fatal(err)
				}
				response = httptest.NewRecorder()
				r.adminGovernanceSummary(response, httptest.NewRequest(http.MethodGet, query, nil))
				if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "eligible_units") {
					t.Fatal("closed owner credited partial summary", response.Code, response.Body.String())
				}
			}
		})
	}
}

type unusedSummaryDefinitions struct{}

func (unusedSummaryDefinitions) GetOutcomeDefinition(context.Context, string, string, uint64) (domain.OutcomeDefinition, error) {
	return domain.OutcomeDefinition{}, errors.New("not used by readonly fixture")
}

func TestGovernanceSummaryWideSourceSplitPreservesOriginalFenceAndOrder(t *testing.T) {
	ctx := context.Background()
	key := make([]byte, 32)
	key[0] = 41
	source := filepath.Join(t.TempDir(), "governance.journal")
	log, err := governance.Open(source, key)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	now := time.Now().UTC()
	events := make([]governance.Event, 4)
	for i := range events {
		id := fmt.Sprintf("out_unit%drev1", i)
		e := governance.Event{EventID: "gov_" + id, ProjectID: "prj_1", WorkUnitID: fmt.Sprintf("wku_unit%d", i), DefinitionID: "odef_result", DefinitionVersion: 1, OutcomeID: id, Value: "accepted", ReporterKeyID: strings.Repeat("x", 60<<10), ObservedAt: now, IngestedAt: now, Revision: 1, IdempotencyKeyHash: outcomePageTestLabel(id), RequestFingerprint: outcomePageTestLabel("fp-" + id)}
		if _, err := log.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
		events[i] = e
	}
	p, err := governance.BuildGovernanceProjection(ctx, source, key, filepath.Join(t.TempDir(), "view.db"), log.Summary())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { log.Close(); p.Close() }()
	m, err := governance.NewProjectedManager(ctx, log, p, ledger.NewState(), unusedSummaryDefinitions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fence, err := m.CaptureOutcomeFence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for revision := 2; revision <= 20; revision++ {
		for i := range events {
			previous := events[i]
			e := previous
			e.OutcomeID = fmt.Sprintf("out_unit%drev%d", i, revision)
			e.EventID = "gov_" + e.OutcomeID
			e.Revision = uint64(revision)
			e.SupersedesOutcomeID = previous.OutcomeID
			e.Value = "rejected"
			e.IdempotencyKeyHash = outcomePageTestLabel(e.OutcomeID)
			e.RequestFingerprint = outcomePageTestLabel("fp-" + e.OutcomeID)
			r, err := log.Append(ctx, e)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Apply(ctx, r); err != nil {
				t.Fatal(err)
			}
			events[i] = e
		}
	}
	refs := make([]governance.CurrentOutcomeReference, 64)
	for i := range refs {
		e := events[i%4]
		refs[i] = governance.CurrentOutcomeReference{ProjectID: e.ProjectID, WorkUnitID: e.WorkUnitID, DefinitionID: e.DefinitionID, DefinitionVersion: e.DefinitionVersion}
	}
	if rows, err := m.ReadCurrentOutcomeBatch(ctx, fence, refs); !errors.Is(err, governance.ErrOutcomePageLimit) || len(rows) != 0 {
		t.Fatal("fixture did not require shrinking", len(rows), err)
	}
	r := &Runtime{governance: governanceRuntime{manager: m}}
	rows, err := r.readSummaryOutcomeBatch(ctx, fence, refs)
	if err != nil || len(rows) != 64 {
		t.Fatal(len(rows), err)
	}
	for i, row := range rows {
		if row.Reference != refs[i] || !row.Found || row.Value != "accepted" {
			t.Fatal("split changed cut/order/value", i, row)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if rows, err := r.readSummaryOutcomeBatch(canceled, fence, refs); !errors.Is(err, context.Canceled) || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if rows, err := r.readSummaryOutcomeBatch(ctx, fence, refs); !errors.Is(err, governance.ErrUnavailable) || len(rows) != 0 {
		t.Fatal(rows, err)
	}
}
