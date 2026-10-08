package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/ledger"
)

func TestAccountingObservationDashboardAndMetricsUseDiskOwnerWithoutState(t *testing.T) {
	r, bootstrap, _ := openBootstrappedRuntime(t)
	defer r.Close()
	ctx := context.Background()
	project, err := r.store.GetProject(ctx, bootstrap.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	project.DailyBudgetMicrosUSD = 100
	if _, err := r.store.PutProject(ctx, project, project.Revision, nil); err != nil {
		t.Fatal(err)
	}
	// Withdraw/join all production producers before this test-only owner
	// handoff. No business generation is published or made ready here.
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
	// Ensure the original native log is closed before the test projection's
	// registered cleanup; no forced IO close or production publication.
	defer func() { r.Close(); p.Close() }()
	request, err := next.BeginRequest(ctx, bootstrap.ProjectID, "req_disk_observation")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := next.ReserveAttempt(ctx, request, 100, 87); err != nil {
		t.Fatal(err)
	}
	oldState := r.accountingState.reader
	r.accountingState.reader = nil
	defer func() { r.accountingState.reader = oldState }()
	now := time.Now()
	period, err := r.periods.PeriodAt(now)
	if err != nil {
		t.Fatal(err)
	}
	governance, _, err := r.dashboardGovernance(httptest.NewRequest(http.MethodGet, "/", nil), now, period)
	if err != nil || governance.Budget.AtRisk != 1 || len(governance.Budget.Items) != 1 || governance.Budget.Items[0].Current != 87 || governance.Budget.Items[0].ReservedMicrosUSD != 87 {
		t.Fatal("dashboard did not read disk owner", governance.Budget, err)
	}
	metrics := httptest.NewRecorder()
	if err := r.writeMetrics(ctx, metrics); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(metrics.Body.String(), "halro_accounting_pending_leases 1\n") || !strings.Contains(metrics.Body.String(), "halro_accounting_oldest_pending_lease_age_seconds ") {
		t.Fatal("metrics did not read pending disk owner", metrics.Body.String())
	}
	assertMetricsExpositionContract(t, metrics.Body.String())
	// Retired owner is not replaced with old State or a fabricated zero.
	if err := next.Retire(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.dashboardGovernance(httptest.NewRequest(http.MethodGet, "/", nil), now, period); !errors.Is(err, budget.ErrAccountingRetired) {
		t.Fatal("retired dashboard credited zero pressure", err)
	}
	metrics = httptest.NewRecorder()
	if err := r.writeMetrics(ctx, metrics); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(metrics.Body.String(), "halro_accounting_pending_leases") || strings.Contains(metrics.Body.String(), "halro_accounting_oldest_pending_lease_age_seconds") || !strings.Contains(metrics.Body.String(), "halro_accounting_pending_lease_normal_max_age_seconds ") {
		t.Fatal("failed pending read silenced whole scrape or fabricated zero", metrics.Body.String())
	}
	assertMetricsExpositionContract(t, metrics.Body.String())
	before, _, err := p.ReadPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before.Sequence != 2 || accountingReaderHead(t, oldState).Sequence != 0 || r.ledger.Stats().Records != 2 {
		t.Fatal("test-only handoff wrote old State", before, accountingReaderHead(t, oldState))
	}
}

func TestAccountingObservationCanceledMetricsOmitPendingFamilies(t *testing.T) {
	r, _, _ := openBootstrappedRuntime(t)
	defer r.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	metrics := httptest.NewRecorder()
	if err := r.writeMetrics(ctx, metrics); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(metrics.Body.String(), "halro_accounting_pending_leases") || strings.Contains(metrics.Body.String(), "halro_accounting_oldest_pending_lease_age_seconds") || strings.Contains(metrics.Body.String(), "halro_active_requests ") || !strings.Contains(metrics.Body.String(), "halro_usage_read_unavailable 1\n") || !strings.Contains(metrics.Body.String(), "halro_activation_stale ") {
		t.Fatal("cancelled observation returned zeros or aborted scrape")
	}
	assertMetricsExpositionContract(t, metrics.Body.String())
	if _, err := r.accounting.ReadPendingLeaseStats(ctx, time.Now()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if v, err := r.accounting.ReadBalance(ctx, ledger.BalanceKey{}); !errors.Is(err, context.Canceled) || v != (ledger.Balance{}) {
		t.Fatal(v, err)
	}
}
