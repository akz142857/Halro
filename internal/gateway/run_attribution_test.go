package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/auth"
	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/requestmeta"
)

func TestInferenceRunCanceledReadFailsBeforeNativeAcceptanceAndProviderIO(t *testing.T) {
	f := newFixtureShaped(t, 1_000, ledger.Options{}, nil, func(project *domain.Project) {
		project.RunGovernance = testRunGovernanceConfig()
	}, nil)
	defer f.close()
	f.key.Scopes = []domain.GatewayScope{domain.GatewayScopeInference, domain.GatewayScopeRunAttach}
	unit, _, err := f.accounting.CreateWorkUnit(context.Background(), f.project.ID, f.key.ID, 10, gatewayGovernanceIntent("canceled-wu"))
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := f.accounting.CreateRun(context.Background(), f.project.ID, f.key.ID, unit.ID, 1_000, time.Hour, 10, gatewayGovernanceIntent("canceled-run"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(requestmeta.WithRunID(context.Background(), run.ID))
	cancel()
	before := f.state.Watermark()
	_, _, err = f.service.resolveRunAttribution(ctx, auth.AuthResult{Key: f.key, Project: f.project})
	assertGatewayCode(t, err, "run_governance_unavailable")
	var failure *Error
	if !errors.As(err, &failure) || failure.HTTPStatus != 503 || !errors.Is(err, context.Canceled) {
		t.Fatal("read failure became run absence", err)
	}
	if f.state.Watermark() != before || f.adapter.calls != 0 {
		t.Fatal("failed Run read reached acceptance/provider")
	}
}

func TestInferenceRetiredAccountingRefusesBeforeNativeAcceptanceAndProviderIO(t *testing.T) {
	f := newFixtureShaped(t, 1_000, ledger.Options{}, nil, func(project *domain.Project) {
		project.RunGovernance = testRunGovernanceConfig()
	}, nil)
	defer f.close()
	f.key.Scopes = []domain.GatewayScope{domain.GatewayScopeInference, domain.GatewayScopeRunAttach}
	if err := f.service.auth.Refresh(context.Background(), source{keys: []domain.GatewayKey{f.key}, projects: []domain.Project{f.project}}); err != nil {
		t.Fatal(err)
	}
	if err := f.accounting.Retire(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := f.state.Watermark()
	for _, runID := range []string{"", "run_retired"} {
		t.Run("run="+runID, func(t *testing.T) {
			ctx := context.Background()
			if runID != "" {
				ctx = requestmeta.WithRunID(ctx, runID)
			}
			_, err := f.service.Chat(ctx, f.plaintext, chatRequest())
			var failure *Error
			if !errors.As(err, &failure) || failure.HTTPStatus != 503 || !errors.Is(err, budget.ErrAccountingRetired) {
				t.Fatal("retired accounting credited absence/success", err)
			}
			if f.state.Watermark() != before || f.adapter.calls != 0 {
				t.Fatal("retired accounting reached native acceptance/provider")
			}
		})
	}
}

func TestInferenceRunHeaderAttributesEveryAccountingEvent(t *testing.T) {
	f := newFixtureShaped(t, 1_000, ledger.Options{}, nil, func(project *domain.Project) {
		project.RunGovernance = testRunGovernanceConfig()
	}, nil)
	defer f.close()
	f.key.Scopes = []domain.GatewayScope{domain.GatewayScopeInference, domain.GatewayScopeRunAttach}
	if err := f.service.auth.Refresh(context.Background(), source{keys: []domain.GatewayKey{f.key}, projects: []domain.Project{f.project}}); err != nil {
		t.Fatal(err)
	}
	workUnit, _, err := f.accounting.CreateWorkUnit(context.Background(), f.project.ID, f.key.ID, 10, gatewayGovernanceIntent("wu"))
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := f.accounting.CreateRun(context.Background(), f.project.ID, f.key.ID, workUnit.ID, 1_000, time.Hour, 10, gatewayGovernanceIntent("run"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := requestmeta.WithRunID(context.Background(), run.ID)
	if _, err := f.service.Chat(ctx, f.plaintext, chatRequest()); err != nil {
		t.Fatal(err)
	}
	got, ok, readErr := f.accounting.ReadRun(context.Background(), f.project.ID, run.ID)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !ok || got.ReservedMicrosUSD != 0 || got.CommittedMicrosUSD != 20 {
		t.Fatalf("attributed Run=%#v found=%t", got, ok)
	}
}

func TestInferenceRunHeaderRequiresAttachScopeBeforeProviderIO(t *testing.T) {
	f := newFixtureShaped(t, 1_000, ledger.Options{}, nil, func(project *domain.Project) {
		project.RunGovernance = testRunGovernanceConfig()
	}, nil)
	defer f.close()
	workUnit, _, err := f.accounting.CreateWorkUnit(context.Background(), f.project.ID, f.key.ID, 10, gatewayGovernanceIntent("wu-denied"))
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := f.accounting.CreateRun(context.Background(), f.project.ID, f.key.ID, workUnit.ID, 1_000, time.Hour, 10, gatewayGovernanceIntent("run-denied"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.service.Chat(requestmeta.WithRunID(context.Background(), run.ID), f.plaintext, chatRequest())
	assertGatewayCode(t, err, "gateway_key_scope_denied")
	if f.adapter.calls != 0 {
		t.Fatal("scope-denied Run attachment reached the provider")
	}
}

func TestInferenceRunBudgetRejectsBeforeProviderIO(t *testing.T) {
	f := newFixtureShaped(t, 1_000, ledger.Options{}, nil, func(project *domain.Project) {
		project.RunGovernance = testRunGovernanceConfig()
	}, nil)
	defer f.close()
	f.key.Scopes = []domain.GatewayScope{domain.GatewayScopeInference, domain.GatewayScopeRunAttach}
	if err := f.service.auth.Refresh(context.Background(), source{keys: []domain.GatewayKey{f.key}, projects: []domain.Project{f.project}}); err != nil {
		t.Fatal(err)
	}
	workUnit, _, err := f.accounting.CreateWorkUnit(context.Background(), f.project.ID, f.key.ID, 10, gatewayGovernanceIntent("wu-budget"))
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := f.accounting.CreateRun(context.Background(), f.project.ID, f.key.ID, workUnit.ID, 10, time.Hour, 10, gatewayGovernanceIntent("run-budget"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.service.Chat(requestmeta.WithRunID(context.Background(), run.ID), f.plaintext, chatRequest())
	assertGatewayCode(t, err, "run_budget_exceeded")
	if f.adapter.calls != 0 {
		t.Fatal("Run budget rejection reached the provider")
	}
	if got := f.service.RejectionMetrics().RunBudget; got != 1 {
		t.Fatalf("Run budget rejections=%d", got)
	}
	got, _, readErr := f.accounting.ReadRun(context.Background(), f.project.ID, run.ID)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got.CommittedMicrosUSD != 0 || got.ReservedMicrosUSD != 0 || got.BudgetState != domain.RunBudgetAvailable {
		t.Fatalf("rejected request changed Run=%#v", got)
	}
}

func TestUntaggedLegacyInferenceDoesNotEnterRunGovernancePath(t *testing.T) {
	f := newFixtureShaped(t, 1_000, ledger.Options{}, nil, func(project *domain.Project) {
		project.RunGovernance = testRunGovernanceConfig()
	}, nil)
	defer f.close()
	// A legacy key has no persisted scopes and resolves to inference only. If an
	// untagged request accidentally entered the Run path, it would fail the
	// missing run:attach scope before reaching this provider.
	if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err != nil {
		t.Fatal(err)
	}
	options := ledger.GovernanceListOptions{Limits: ledger.ProjectionPageLimits{Rows: 1, Bytes: ledger.GovernanceListMaxBytes}}
	units, unitErr := f.accounting.ReadWorkUnitPage(context.Background(), ledger.WorkUnitListQuery{ProjectID: f.project.ID, GovernanceListOptions: options})
	runs, runErr := f.accounting.ReadRunPage(context.Background(), ledger.RunListQuery{ProjectID: f.project.ID, At: time.Now(), GovernanceListOptions: options})
	if unitErr != nil || runErr != nil || len(units.Items) != 0 || units.NextID != "" || len(runs.Items) != 0 || runs.NextID != "" {
		t.Fatal("untagged inference created Run Governance lifecycle state")
	}
	settled := f.state.SettledAttempts()
	if len(settled) != 1 || settled[0].Settlement.WorkUnitID != "" || settled[0].Settlement.RunID != "" {
		t.Fatalf("legacy settlement gained attribution: %#v", settled)
	}
}

func testRunGovernanceConfig() domain.RunGovernanceConfig {
	return domain.RunGovernanceConfig{
		Enabled: true, DefaultRunBudgetMicrosUSD: 1_000, MaxRunBudgetMicrosUSD: 10_000,
		DefaultRunTTLSeconds: 3600, MaxRunTTLSeconds: 86400,
		MaxActiveRuns: 10, MaxOpenWorkUnits: 10,
	}
}

func gatewayGovernanceIntent(operation string) budget.GovernanceIntent {
	return budget.GovernanceIntent{
		Operation:          operation,
		IdempotencyKeyHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		RequestFingerprint: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
}
