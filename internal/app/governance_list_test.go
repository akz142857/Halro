package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

func TestAdminGovernanceListPagesPreserveScopesCountsAndCursor(t *testing.T) {
	r, bootstrap, _ := openBootstrappedRuntime(t)
	defer r.Close()
	ctx := context.Background()
	var unitIDs, runIDs, closedUnits, closedRuns, oneUnitRuns []string
	for i := 0; i < 5; i++ {
		intent := budget.GovernanceIntent{Operation: fmt.Sprintf("test.unit.%d", i), IdempotencyKeyHash: "sha256:" + strings.Repeat("a", 64), RequestFingerprint: "sha256:" + strings.Repeat("b", 64)}
		unit, _, err := r.accounting.CreateWorkUnit(ctx, bootstrap.ProjectID, bootstrap.KeyID, 100, intent)
		if err != nil {
			t.Fatal(err)
		}
		unitIDs = append(unitIDs, unit.ID)
		for j := 0; j < 8; j++ {
			intent.Operation = fmt.Sprintf("test.run.%d.%d", i, j)
			run, _, err := r.accounting.CreateRun(ctx, bootstrap.ProjectID, bootstrap.KeyID, unit.ID, 100, time.Hour, 100, intent)
			if err != nil {
				t.Fatal(err)
			}
			runIDs = append(runIDs, run.ID)
			if i == 0 {
				oneUnitRuns = append(oneUnitRuns, run.ID)
			}
			if j == 0 {
				intent.Operation = fmt.Sprintf("test.close.run.%d", i)
				if _, _, err := r.accounting.CloseRun(ctx, bootstrap.ProjectID, bootstrap.KeyID, unit.ID, run.ID, "operator", intent); err != nil {
					t.Fatal(err)
				}
				closedRuns = append(closedRuns, run.ID)
			}
		}
		if i%2 == 0 {
			intent.Operation = fmt.Sprintf("test.close.unit.%d", i)
			if _, _, err := r.accounting.CloseWorkUnit(ctx, bootstrap.ProjectID, bootstrap.KeyID, unit.ID, intent); err != nil {
				t.Fatal(err)
			}
			closedUnits = append(closedUnits, unit.ID)
		}
	}
	for _, test := range []struct {
		name    string
		handler http.HandlerFunc
		query   url.Values
		want    []string
	}{
		{"units", r.listAdminWorkUnits, url.Values{"project_id": {bootstrap.ProjectID}}, unitIDs},
		{"closed_units", r.listAdminWorkUnits, url.Values{"project_id": {bootstrap.ProjectID}, "status": {"closed"}}, closedUnits},
		{"runs", r.listAdminRuns, url.Values{"project_id": {bootstrap.ProjectID}}, runIDs},
		{"closed_runs", r.listAdminRuns, url.Values{"project_id": {bootstrap.ProjectID}, "status": {"closed"}}, closedRuns},
		{"unit_runs", r.listAdminRuns, url.Values{"work_unit_id": {unitIDs[0]}}, oneUnitRuns},
		{"foreign_project", r.listAdminRuns, url.Values{"project_id": {"prj_absent"}, "work_unit_id": {unitIDs[0]}}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.query.Set("limit", "2")
			var ids []string
			for pages := 0; ; pages++ {
				if pages > 100 {
					t.Fatal("cursor did not finish")
				}
				response := httptest.NewRecorder()
				test.handler(response, httptest.NewRequest(http.MethodGet, "/?"+test.query.Encode(), nil))
				if response.Code != http.StatusOK {
					t.Fatal(response.Code, response.Body.String())
				}
				var page struct {
					Items []struct {
						ID    string `json:"id"`
						Count int    `json:"run_count"`
					}
					Next string `json:"next_cursor"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
					t.Fatal(err)
				}
				if page.Items == nil || len(page.Items) > 2 {
					t.Fatal("invalid page shape", response.Body.String())
				}
				for _, item := range page.Items {
					if strings.Contains(test.name, "units") && item.Count != 8 {
						t.Fatal("summary lost historical closed runs", item)
					}
					ids = append(ids, item.ID)
				}
				if page.Next == "" {
					break
				}
				decoded, err := decodeResourceCursor(page.Next)
				previous, _ := decodeResourceCursor(test.query.Get("cursor"))
				if err != nil || decoded <= previous {
					t.Fatal("cursor failed to advance", decoded, previous, err)
				}
				test.query.Set("cursor", page.Next)
			}
			slices.Sort(test.want)
			if !reflect.DeepEqual(ids, test.want) {
				t.Fatal("list omitted, duplicated or crossed scope", ids, test.want)
			}
		})
	}
	if count, err := r.accounting.ReadActiveRunCount(ctx, bootstrap.ProjectID, r.clockNow()); err != nil || count != 35 {
		t.Fatal(count, err)
	}
}

func TestAdminGovernanceListValidatesBeforeReadAndCancellationFailsClosed(t *testing.T) {
	// Invalid selectors must be rejected before any accounting dependency.
	r := &Runtime{}
	for _, handler := range []http.HandlerFunc{r.listAdminWorkUnits, r.listAdminRuns} {
		for _, query := range []string{"limit=0", "limit=201", "limit=bad", "cursor=%25", "cursor=" + encodeResourceCursor("bad\x00id"), "status=invalid", "unknown=x"} {
			response := httptest.NewRecorder()
			handler(response, httptest.NewRequest(http.MethodGet, "/?"+query, nil))
			if response.Code != http.StatusBadRequest {
				t.Fatal(query, response.Code, response.Body.String())
			}
		}
	}
	r, _, _ = openBootstrappedRuntime(t)
	defer r.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := accountingReaderHead(t, r.accountingState.reader)
	for _, handler := range []http.HandlerFunc{r.listAdminWorkUnits, r.listAdminRuns} {
		response := httptest.NewRecorder()
		handler(response, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx))
		if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "run_governance_unavailable") {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	if view, err := r.adminWorkUnitView(ctx, domain.WorkUnit{ID: "wku_1", ProjectID: "prj_1"}); err == nil || !reflect.DeepEqual(view, adminWorkUnitView{}) {
		t.Fatal("summary returned partial financial data", view, err)
	}
	if accountingReaderHead(t, r.accountingState.reader) != before {
		t.Fatal("failed reads mutated ledger")
	}
	page, err := r.accounting.ReadWorkUnitPage(context.Background(), ledger.WorkUnitListQuery{GovernanceListOptions: ledger.GovernanceListOptions{Limits: ledger.ProjectionPageLimits{Rows: 1, Bytes: ledger.GovernanceListMaxBytes}}})
	if err != nil || len(page.Items) != 0 || page.NextID != "" {
		t.Fatal(page, err)
	}
}
