package app

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/go-chi/chi/v5"
)

func TestGovernancePointReadCancellationIsUnavailableRatherThanNotFound(t *testing.T) {
	runtime, bootstrap, _ := openBootstrappedRuntime(t)
	defer runtime.Close()
	enableRunGovernanceForTest(t, runtime, bootstrap.ProjectID, bootstrap.KeyID)
	unit, _, err := runtime.accounting.CreateWorkUnit(context.Background(), bootstrap.ProjectID, bootstrap.KeyID, 10, budget.GovernanceIntent{
		Operation: "test.unit", IdempotencyKeyHash: "sha256:" + strings.Repeat("a", 64), RequestFingerprint: "sha256:" + strings.Repeat("b", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := runtime.accounting.CreateRun(context.Background(), bootstrap.ProjectID, bootstrap.KeyID, unit.ID, 100, time.Hour, 10, budget.GovernanceIntent{
		Operation: "test.run", IdempotencyKeyHash: "sha256:" + strings.Repeat("a", 64), RequestFingerprint: "sha256:" + strings.Repeat("b", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, method, param, id string
		handler                 http.HandlerFunc
	}{
		{"public_read", http.MethodGet, "runID", run.ID, runtime.getRun},
		{"public_close", http.MethodPost, "runID", run.ID, runtime.closeRun},
		{"admin_run", http.MethodGet, "runID", run.ID, runtime.getAdminRun},
		{"admin_unit", http.MethodGet, "workUnitID", unit.ID, runtime.getAdminWorkUnit},
	} {
		t.Run(test.name, func(t *testing.T) {
			route := chi.NewRouteContext()
			route.URLParams.Add(test.param, test.id)
			request := governanceRequest(test.method, "/", bootstrap.GatewayKey, "read-failure", `{}`)
			ctx, cancel := context.WithCancel(context.WithValue(request.Context(), chi.RouteCtxKey, route))
			cancel()
			before := accountingReaderHead(t, runtime.accountingState.reader)
			response := httptest.NewRecorder()
			test.handler(response, request.WithContext(ctx))
			if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "run_governance_unavailable") {
				t.Fatal("failed read became absence/success", response.Code, response.Body.String())
			}
			if accountingReaderHead(t, runtime.accountingState.reader) != before {
				t.Fatal("failed read performed a mutation")
			}
		})
	}
}

type pressureBalanceReader struct {
	balance ledger.Balance
	err     error
	keys    []ledger.BalanceKey
}

func TestGovernanceRetiredManagerIsUnavailableWithoutNativeMutation(t *testing.T) {
	runtime, bootstrap, _ := openBootstrappedRuntime(t)
	defer runtime.Close()
	enableRunGovernanceForTest(t, runtime, bootstrap.ProjectID, bootstrap.KeyID)
	if err := runtime.accounting.Retire(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := accountingReaderHead(t, runtime.accountingState.reader)
	for _, test := range []struct {
		name, method, param string
		handler             http.HandlerFunc
	}{
		{"public_read", http.MethodGet, "runID", runtime.getRun},
		{"public_close", http.MethodPost, "runID", runtime.closeRun},
		{"admin_run", http.MethodGet, "runID", runtime.getAdminRun},
		{"admin_unit", http.MethodGet, "workUnitID", runtime.getAdminWorkUnit},
		{"admin_units", http.MethodGet, "", runtime.listAdminWorkUnits},
		{"admin_runs", http.MethodGet, "", runtime.listAdminRuns},
	} {
		t.Run(test.name, func(t *testing.T) {
			route := chi.NewRouteContext()
			if test.param != "" {
				resourceID := "run_retired"
				if test.param == "workUnitID" {
					resourceID = "wku_retired"
				}
				route.URLParams.Add(test.param, resourceID)
			}
			request := governanceRequest(test.method, "/", bootstrap.GatewayKey, "retired", `{}`)
			response := httptest.NewRecorder()
			test.handler(response, request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, route)))
			if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "run_governance_unavailable") {
				t.Fatal("retired Manager credited absence/success", response.Code, response.Body.String())
			}
			if accountingReaderHead(t, runtime.accountingState.reader) != before {
				t.Fatal("retired Manager mutated native accounting")
			}
		})
	}
}

func (r *pressureBalanceReader) ReadBalance(ctx context.Context, key ledger.BalanceKey) (ledger.Balance, error) {
	r.keys = append(r.keys, key)
	return r.balance, errors.Join(r.err, ctx.Err())
}

func TestBudgetPressureReadErrorsAndBoundsDoNotProducePartialDashboard(t *testing.T) {
	period := budget.Period{ID: "2026-10-03", TimezoneVersion: 9}
	projects := []domain.Project{{ID: "prj_disabled", DailyBudgetMicrosUSD: 100}, {ID: "prj_unlimited", Enabled: true}, {ID: "prj_1", Enabled: true, DailyBudgetMicrosUSD: 100}}
	reader := &pressureBalanceReader{balance: ledger.Balance{CommittedMicrosUSD: 40, ReservedMicrosUSD: 50}}
	got, err := budgetPressure(context.Background(), projects, reader, period)
	if err != nil || got.AtRisk != 1 || len(got.Items) != 1 || got.Items[0].Current != 90 || len(reader.keys) != 1 || reader.keys[0] != (ledger.BalanceKey{ProjectID: "prj_1", PeriodID: period.ID, TimezoneVersion: 9}) {
		t.Fatal(got, reader.keys, err)
	}
	failure := errors.New("accounting storage unavailable")
	for _, bad := range []*pressureBalanceReader{
		{balance: ledger.Balance{CommittedMicrosUSD: 90}, err: failure},
		{balance: ledger.Balance{CommittedMicrosUSD: -1}},
		{balance: ledger.Balance{CommittedMicrosUSD: math.MaxInt64, ReservedMicrosUSD: 1}},
	} {
		got, err := budgetPressure(context.Background(), projects, bad, period)
		if err == nil || len(got.Items) != 0 || got.AtRisk != 0 {
			t.Fatal("failed dashboard retained financial result", got, err)
		}
		if bad.err != nil && !errors.Is(err, failure) {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := budgetPressure(ctx, projects, reader, period); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
