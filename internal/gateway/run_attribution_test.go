package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/requestmeta"
)

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
	got, ok := f.accounting.Run(f.project.ID, run.ID)
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
	got, _ := f.accounting.Run(f.project.ID, run.ID)
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
	if len(f.accounting.WorkUnits(f.project.ID)) != 0 || len(f.accounting.Runs(f.project.ID, "")) != 0 {
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
