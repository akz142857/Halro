package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/governance"
)

func TestWorkUnitOutcomeDetailPreservesHistoryWithDiskOwnersAndCurrentMaturity(t *testing.T) {
	for _, projected := range []bool{false, true} {
		t.Run(map[bool]string{false: "state_governance", true: "disk_governance"}[projected], func(t *testing.T) {
			r, bootstrap, _ := openBootstrappedRuntime(t)
			defer r.Close()
			enableRunGovernanceForTest(t, r, bootstrap.ProjectID, bootstrap.KeyID)
			var govProjection *governance.GovernanceProjection
			if projected {
				govProjection = installTestGovernanceProjection(t, r)
			} else {
				installTestGovernanceState(t, r)
			}
			ctx := context.Background()
			now := time.Now().UTC()
			refs := make([]domain.OutcomeDefinitionRef, 2)
			for i := range refs {
				project, err := r.store.GetProject(ctx, bootstrap.ProjectID)
				if err != nil {
					t.Fatal(err)
				}
				definition := domain.OutcomeDefinition{ID: fmt.Sprintf("odef_detail%d", i), ProjectID: bootstrap.ProjectID, Name: fmt.Sprintf("detail%d", i), Version: 1,
					DataType: domain.OutcomeCategorical, AllowedValues: []string{"accepted", "rejected"}, SuccessValues: []string{"accepted"}, Enabled: true, CreatedAt: now, CreatedBy: "admin"}
				if _, err := r.store.PutOutcomeDefinition(ctx, definition, project.Revision, 0, nil); err != nil {
					t.Fatal(err)
				}
				refs[i] = domain.OutcomeDefinitionRef{ID: definition.ID, Version: 1}
			}
			intent := func(label string) budget.GovernanceIntent {
				return budget.GovernanceIntent{Operation: label, IdempotencyKeyHash: outcomePageTestLabel(label), RequestFingerprint: outcomePageTestLabel("fp-" + label)}
			}
			w, _, err := r.accounting.CreateWorkUnitWithDefinitions(ctx, bootstrap.ProjectID, bootstrap.KeyID, 10, refs, intent("detail-wu"))
			if err != nil {
				t.Fatal(err)
			}
			run, _, err := r.accounting.CreateRun(ctx, bootstrap.ProjectID, bootstrap.KeyID, w.ID, 1000, time.Hour, 10, intent("detail-run"))
			if err != nil {
				t.Fatal(err)
			}
			request, err := r.accounting.BeginRequestAttributed(ctx, bootstrap.ProjectID, bootstrap.KeyID, "req_detail", "model", w.ID, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := r.accounting.CloseWorkUnit(ctx, bootstrap.ProjectID, bootstrap.KeyID, w.ID, intent("detail-close")); err != nil {
				t.Fatal(err)
			}
			var want []domain.Outcome
			previous := make([]string, len(refs))
			for revision := 1; revision <= domain.MaxOutcomeRevisions; revision++ {
				for i, ref := range refs {
					label := fmt.Sprintf("detail-%d-%d", i, revision)
					outcome, _, err := r.governance.manager.Report(ctx, governance.ReportInput{ProjectID: bootstrap.ProjectID, WorkUnitID: w.ID, DefinitionID: ref.ID,
						ReporterKeyID: bootstrap.KeyID, Value: "accepted", ObservedAt: now, SupersedesOutcomeID: previous[i],
						Intent: governance.Intent{IdempotencyKeyHash: outcomePageTestLabel(label), RequestFingerprint: outcomePageTestLabel("fp-" + label)}})
					if err != nil {
						t.Fatal(err)
					}
					previous[i] = outcome.ID
					want = append(want, outcome)
				}
			}
			path := "/halro/v1/work-units/" + w.ID
			router := r.gatewayRouter()
			read := func() *httptest.ResponseRecorder {
				t.Helper()
				response := httptest.NewRecorder()
				router.ServeHTTP(response, governanceRequest(http.MethodGet, path, bootstrap.GatewayKey, "", ""))
				return response
			}
			check := func(response *httptest.ResponseRecorder, provisional bool) []byte {
				t.Helper()
				var detail struct {
					WorkUnit domain.WorkUnit  `json:"work_unit"`
					Runs     []domain.Run     `json:"runs"`
					Outcomes []domain.Outcome `json:"outcomes"`
				}
				if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &detail) != nil {
					t.Fatal(response.Code, response.Body.String())
				}
				if detail.WorkUnit.ID != w.ID || detail.WorkUnit.Status != domain.WorkUnitClosed || !reflect.DeepEqual(detail.WorkUnit.OutcomeDefinitions, refs) || len(detail.Runs) != 1 || detail.Runs[0].ID != run.ID {
					t.Fatal("wrong accounting owner details", detail)
				}
				for i := range want {
					want[i].Provisional = provisional
				}
				if !reflect.DeepEqual(detail.Outcomes, want) {
					t.Fatal("revision history/native order/maturity changed", len(detail.Outcomes), len(want))
				}
				return append([]byte(nil), response.Body.Bytes()...)
			}
			check(read(), true) // Accepted, before reserve: closed is still provisional.
			if err := r.accounting.Finalize(ctx, request, "success"); err != nil {
				t.Fatal(err)
			}
			before := check(read(), false)
			// Real producer drain and full authenticated Ledger rebuild. This is a
			// test-only selection; the Runtime remains draining, not published ready.
			oldGovernanceState := r.governance.manager.State()
			if err := r.drainAccountingBusiness(ctx, nil); err != nil {
				t.Fatal(err)
			}
			master, err := unlockMasterKey(ctx, r.config)
			if err != nil {
				t.Fatal(err)
			}
			key, err := loadLedgerHMACKey(r.store, r.vault, master)
			clear(master)
			if err != nil {
				t.Fatal(err)
			}
			defer clear(key)
			lease, p := projectedWriterCandidate(t, r.ledger, r.config.LedgerPath(), key)
			next, err := budget.NewWithProjection(ctx, r.ledger, p, r.periods, lease, budget.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if err := lease.Close(); err != nil {
				t.Fatal(err)
			}
			r.accounting = next
			if projected {
				r.governance.manager, err = governance.NewProjectedManager(ctx, r.governance.log, govProjection, next, r.store, r.store)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				r.governance.manager = governance.NewManager(r.governance.log, oldGovernanceState, next, r.store, r.store)
			}
			defer func() { r.Close(); p.Close() }()
			oldState := r.accountingState.reader
			r.accountingState.reader = nil
			defer func() { r.accountingState.reader = oldState }()
			if after := check(read(), false); string(after) != string(before) {
				t.Fatal("public response changed after disk accounting selection")
			}
			if govProjection != nil {
				if err := govProjection.Close(); err != nil {
					t.Fatal(err)
				}
				response := read()
				if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "governance_unavailable") || strings.Contains(response.Body.String(), "outcomes") || strings.Contains(response.Body.String(), "work_unit\":") {
					t.Fatal("closed Governance owner leaked partial detail", response.Code, response.Body.String())
				}
			}
			if err := next.Retire(ctx); err != nil {
				t.Fatal(err)
			}
			response := read()
			if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "run_governance_unavailable") || strings.Contains(response.Body.String(), "outcomes") || strings.Contains(response.Body.String(), "work_unit\":") {
				t.Fatal("retired accounting owner leaked partial detail", response.Code, response.Body.String())
			}
		})
	}
}
