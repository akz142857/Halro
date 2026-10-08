package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/governance"
)

func TestGovernanceExportPreservesNativeWideDefinitionAfterReopen(t *testing.T) {
	r, bootstrap, reopen := openBootstrappedRuntime(t)
	defer func() { r.Close() }()
	ctx := context.Background()
	project, err := r.store.GetProject(ctx, bootstrap.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	definition := domain.OutcomeDefinition{ID: "odef_export", ProjectID: bootstrap.ProjectID, Name: "export", Version: 1, DataType: domain.OutcomeCategorical, AllowedValues: []string{"accepted", "rejected"}, SuccessValues: []string{"accepted"}, Enabled: true, CreatedAt: now, CreatedBy: strings.Repeat("a", (1<<20)+1)}
	if _, err := r.store.PutOutcomeDefinition(ctx, definition, project.Revision, 0, nil); err != nil {
		t.Fatal(err)
	}
	intent := func(label string) budget.GovernanceIntent {
		return budget.GovernanceIntent{Operation: label, IdempotencyKeyHash: outcomePageTestLabel(label), RequestFingerprint: outcomePageTestLabel("fp-" + label)}
	}
	w, _, err := r.accounting.CreateWorkUnitWithDefinitions(ctx, bootstrap.ProjectID, bootstrap.KeyID, 10, []domain.OutcomeDefinitionRef{{ID: definition.ID, Version: 1}}, intent("export-wu"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.accounting.CreateRun(ctx, bootstrap.ProjectID, bootstrap.KeyID, w.ID, 100, time.Hour, 10, intent("export-run")); err != nil {
		t.Fatal(err)
	}
	previous := ""
	for i := 0; i < domain.MaxOutcomeRevisions; i++ {
		label := fmt.Sprintf("export-%d", i)
		outcome, _, err := r.governance.manager.Report(ctx, governance.ReportInput{ProjectID: bootstrap.ProjectID, WorkUnitID: w.ID, DefinitionID: definition.ID, ReporterKeyID: bootstrap.KeyID, Value: "accepted", ObservedAt: now, SupersedesOutcomeID: previous, Intent: governance.Intent{IdempotencyKeyHash: outcomePageTestLabel(label), RequestFingerprint: outcomePageTestLabel("fp-" + label)}})
		if err != nil {
			t.Fatal(err)
		}
		previous = outcome.ID
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = reopen()
	stored, err := r.store.GetOutcomeDefinition(ctx, bootstrap.ProjectID, definition.ID, 1)
	if err != nil || stored.CreatedBy != definition.CreatedBy {
		t.Fatal("legal metadata definition lost on reopen", err)
	}
	cookie, csrf := loginAdminForTest(t, r)
	request := adminRequest(t, http.MethodPost, "/admin/api/v1/governance/export", nil)
	request.AddCookie(cookie)
	request.Header.Set("X-CSRF-Token", csrf)
	response := httptest.NewRecorder()
	r.adminRouter().ServeHTTP(response, request)
	var result struct {
		Directory string                    `json:"directory"`
		Manifest  governance.ExportManifest `json:"manifest"`
	}
	if response.Code != http.StatusCreated || json.Unmarshal(response.Body.Bytes(), &result) != nil {
		t.Fatal(response.Code, response.Body.String())
	}
	verified, err := governance.VerifyExport(result.Directory)
	if err != nil || verified.AccountingWatermark != accountingReaderHead(t, r.accountingState.reader) || verified.GovernanceWatermark["sequence"].(json.Number) != "20" {
		t.Fatal(verified, err)
	}
	for _, file := range verified.Files {
		want := 1
		if file.Dataset == "outcomes" {
			want = domain.MaxOutcomeRevisions
		}
		if file.RecordCount != want {
			t.Fatal(file)
		}
		data, err := os.ReadFile(filepath.Join(result.Directory, file.Path))
		if err != nil {
			t.Fatal(err)
		}
		if file.Dataset == "outcome_definitions" {
			var got domain.OutcomeDefinition
			if json.Unmarshal(data, &got) != nil || got.CreatedBy != definition.CreatedBy {
				t.Fatal("wide definition truncated")
			}
		}
		if file.Dataset == "outcomes" && strings.Count(string(data), `"provisional":true`) != domain.MaxOutcomeRevisions {
			t.Fatal("accounting maturity changed")
		}
	}
}
