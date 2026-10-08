package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/vault"
)

type outcomePageResponse struct {
	Items               []domain.Outcome `json:"items"`
	NextCursor          string           `json:"next_cursor"`
	GovernanceWatermark struct {
		Sequence uint64 `json:"sequence"`
		Offset   int64  `json:"offset"`
	} `json:"governance_watermark"`
	AccountingWatermark ledger.Watermark `json:"accounting_watermark"`
}

func TestAdminOutcomePageFreezesFactsAndReadsCurrentDiskMaturityWithoutState(t *testing.T) {
	for _, projected := range []bool{false, true} {
		t.Run(map[bool]string{false: "state_governance", true: "disk_governance"}[projected], func(t *testing.T) {
			testAdminOutcomePageDiskMaturity(t, projected)
		})
	}
}

func testAdminOutcomePageDiskMaturity(t *testing.T, projected bool) {
	r, bootstrap, _ := openBootstrappedRuntime(t)
	defer r.Close()
	ctx := context.Background()
	var governanceProjection *governance.GovernanceProjection
	if projected {
		governanceProjection = installTestGovernanceProjection(t, r)
	} else {
		installTestGovernanceState(t, r)
	}
	project, err := r.store.GetProject(ctx, bootstrap.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	definition := domain.OutcomeDefinition{ID: "odef_page", ProjectID: bootstrap.ProjectID, Name: "page", Version: 1,
		DataType: domain.OutcomeCategorical, AllowedValues: []string{"accepted", "rejected"}, SuccessValues: []string{"accepted"}, Enabled: true, CreatedAt: now, CreatedBy: "admin"}
	if _, err := r.store.PutOutcomeDefinition(ctx, definition, project.Revision, 0, nil); err != nil {
		t.Fatal(err)
	}
	intent := func(label string) budget.GovernanceIntent {
		return budget.GovernanceIntent{Operation: label, IdempotencyKeyHash: outcomePageTestLabel(label), RequestFingerprint: outcomePageTestLabel("fp-" + label)}
	}
	// Native Ledger permits wide creator metadata; twenty short Outcome
	// revisions for this same WorkUnit must not reread it twenty times.
	w, _, err := r.accounting.CreateWorkUnitWithDefinitions(ctx, bootstrap.ProjectID, strings.Repeat("k", 70<<10), 10,
		[]domain.OutcomeDefinitionRef{{ID: definition.ID, Version: 1}}, intent("page-wu"))
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := r.accounting.CreateRun(ctx, bootstrap.ProjectID, bootstrap.KeyID, w.ID, 100, time.Hour, 10, intent("page-run"))
	if err != nil {
		t.Fatal(err)
	}
	request, err := r.accounting.BeginRequestAttributed(ctx, bootstrap.ProjectID, bootstrap.KeyID, "req_page", "model", w.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.accounting.CloseWorkUnit(ctx, bootstrap.ProjectID, bootstrap.KeyID, w.ID, intent("page-close")); err != nil {
		t.Fatal(err)
	}
	previous := ""
	var initialIDs []string
	for i := 0; i < 3; i++ {
		label := fmt.Sprintf("page-report-%d", i)
		outcome, _, err := r.governance.manager.Report(ctx, governance.ReportInput{ProjectID: bootstrap.ProjectID, WorkUnitID: w.ID, DefinitionID: definition.ID,
			ReporterKeyID: bootstrap.KeyID, Value: "accepted", ObservedAt: now, SupersedesOutcomeID: previous,
			Intent: governance.Intent{IdempotencyKeyHash: outcomePageTestLabel(label), RequestFingerprint: outcomePageTestLabel("fp-" + label)}})
		if err != nil {
			t.Fatal(err)
		}
		previous = outcome.ID
		initialIDs = append(initialIDs, outcome.ID)
	}
	base := "/admin/api/v1/governance/outcomes?project_id=" + bootstrap.ProjectID + "&limit=1"
	read := func(path string) outcomePageResponse {
		t.Helper()
		response := httptest.NewRecorder()
		r.listAdminOutcomes(response, httptest.NewRequest(http.MethodGet, path, nil))
		var page outcomePageResponse
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &page) != nil {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		return page
	}
	first := read(base)
	if len(first.Items) != 1 || !first.Items[0].Provisional || first.NextCursor == "" || first.GovernanceWatermark.Sequence != 3 {
		t.Fatal(first)
	}
	if err := r.accounting.Finalize(ctx, request, "success"); err != nil {
		t.Fatal(err)
	}
	last, _, err := r.governance.manager.Report(ctx, governance.ReportInput{ProjectID: bootstrap.ProjectID, WorkUnitID: w.ID, DefinitionID: definition.ID,
		ReporterKeyID: bootstrap.KeyID, Value: "rejected", ObservedAt: now, SupersedesOutcomeID: previous,
		Intent: governance.Intent{IdempotencyKeyHash: outcomePageTestLabel("late-page"), RequestFingerprint: outcomePageTestLabel("fp-late-page")}})
	if err != nil {
		t.Fatal(err)
	}
	previous = last.ID
	for i := 4; i < domain.MaxOutcomeRevisions; i++ {
		label := fmt.Sprintf("page-extra-%d", i)
		outcome, _, err := r.governance.manager.Report(ctx, governance.ReportInput{ProjectID: bootstrap.ProjectID, WorkUnitID: w.ID, DefinitionID: definition.ID,
			ReporterKeyID: bootstrap.KeyID, Value: "accepted", ObservedAt: now, SupersedesOutcomeID: previous,
			Intent: governance.Intent{IdempotencyKeyHash: outcomePageTestLabel(label), RequestFingerprint: outcomePageTestLabel("fp-" + label)}})
		if err != nil {
			t.Fatal(err)
		}
		previous = outcome.ID
	}
	seen := []string{first.Items[0].ID}
	cursor := first.NextCursor
	for cursor != "" {
		page := read(base + "&cursor=" + url.QueryEscape(cursor))
		if page.GovernanceWatermark != first.GovernanceWatermark || len(page.Items) > 1 || page.AccountingWatermark.Sequence <= first.AccountingWatermark.Sequence {
			t.Fatal("fact cut/current maturity were conflated", page, first)
		}
		for _, item := range page.Items {
			if item.Provisional {
				t.Fatal("closed disk WorkUnit remained provisional", item)
			}
			seen = append(seen, item.ID)
		}
		cursor = page.NextCursor
	}
	sort.Strings(initialIDs)
	if !reflect.DeepEqual(seen, initialIDs) {
		t.Fatal("lost/repeated/mixed revision facts", seen, initialIDs)
	}
	oldGovernanceState := r.governance.manager.State()
	// Test-only disk handoff after real producer drain, remaining not-ready.
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
	lease, projection := projectedWriterCandidate(t, r.ledger, r.config.LedgerPath(), key)
	next, err := budget.NewWithProjection(ctx, r.ledger, projection, r.periods, lease, budget.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	r.accounting = next
	// The old Governance generation was permanently retired by the drain.
	if projected {
		r.governance.manager, err = governance.NewProjectedManager(ctx, r.governance.log, governanceProjection, next, r.store, r.store)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		r.governance.manager = governance.NewManager(r.governance.log, oldGovernanceState, next, r.store, r.store)
	}
	stale := httptest.NewRecorder()
	r.listAdminOutcomes(stale, httptest.NewRequest(http.MethodGet, base+"&cursor="+url.QueryEscape(first.NextCursor), nil))
	if stale.Code != http.StatusBadRequest || strings.Contains(stale.Body.String(), "items") {
		t.Fatal("old generation cursor survived retirement", stale.Code, stale.Body.String())
	}
	defer func() { r.Close(); projection.Close() }()
	oldState := r.accountingState.reader
	r.accountingState.reader = nil
	defer func() { r.accountingState.reader = oldState }()
	fresh := read(strings.Replace(base, "limit=1", "limit=200", 1))
	if len(fresh.Items) != domain.MaxOutcomeRevisions || fresh.GovernanceWatermark.Sequence != domain.MaxOutcomeRevisions {
		t.Fatal(fresh, last)
	}
	for _, item := range fresh.Items {
		if item.Provisional {
			t.Fatal("closed disk WorkUnit remained provisional", item)
		}
	}
	for _, path := range []string{base + "&cursor=bad!", strings.Replace(base, "limit=1", "limit=201", 1), base + "&work_unit_id=" + w.ID + "&cursor=" + url.QueryEscape(first.NextCursor)} {
		response := httptest.NewRecorder()
		r.listAdminOutcomes(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), "items") {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	if err := next.Retire(ctx); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	r.listAdminOutcomes(response, httptest.NewRequest(http.MethodGet, base, nil))
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "run_governance_unavailable") || strings.Contains(response.Body.String(), "items") {
		t.Fatal(response.Code, response.Body.String())
	}
	if governanceProjection != nil {
		if err := governanceProjection.Close(); err != nil {
			t.Fatal(err)
		}
		response = httptest.NewRecorder()
		r.listAdminOutcomes(response, httptest.NewRequest(http.MethodGet, base, nil))
		if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "governance_unavailable") || strings.Contains(response.Body.String(), "items") {
			t.Fatal(response.Code, response.Body.String())
		}
	}
}

func outcomePageTestLabel(value string) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(value)))
}

// installTestGovernanceProjection selects a fresh owner only in a quiescent,
// unserved test Runtime; it does not exercise production publication.
func installTestGovernanceProjection(t *testing.T, r *Runtime) *governance.GovernanceProjection {
	t.Helper()
	ctx := context.Background()
	if err := r.governance.manager.Retire(ctx); err != nil {
		t.Fatal(err)
	}
	master, err := unlockMasterKey(ctx, r.config)
	if err != nil {
		t.Fatal(err)
	}
	ledgerKey, err := loadLedgerHMACKey(r.store, r.vault, master)
	clear(master)
	if err != nil {
		t.Fatal(err)
	}
	governanceKey, err := vault.DeriveGovernanceHMACKey(ledgerKey)
	clear(ledgerKey)
	if err != nil {
		t.Fatal(err)
	}
	governanceProjection, err := governance.BuildGovernanceProjection(ctx, r.config.GovernancePath(), governanceKey,
		filepath.Join(t.TempDir(), "governance.db"), r.governance.log.Summary())
	clear(governanceKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(); governanceProjection.Close() })
	r.governance.manager, err = governance.NewProjectedManager(ctx, r.governance.log, governanceProjection, r.accounting, r.store, r.store)
	if err != nil {
		t.Fatal(err)
	}
	if r.governance.manager.State() != nil {
		t.Fatal("Governance disk owner retained State")
	}
	return governanceProjection
}
