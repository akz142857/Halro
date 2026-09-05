package domain

import (
	"testing"
	"time"
)

func TestLegacyGatewayKeyScopesResolveToInferenceOnly(t *testing.T) {
	if !HasGatewayScope(nil, GatewayScopeInference) {
		t.Fatal("legacy key lost inference scope")
	}
	if HasGatewayScope(nil, GatewayScopeRunAttach) {
		t.Fatal("legacy key gained run:attach")
	}
}

func TestRunGovernanceConfigIsFailClosed(t *testing.T) {
	valid := RunGovernanceConfig{
		Enabled: true, DefaultRunBudgetMicrosUSD: 1_000_000, MaxRunBudgetMicrosUSD: 2_000_000,
		DefaultRunTTLSeconds: DefaultRunTTLSeconds, MaxRunTTLSeconds: MaxRunTTLSeconds,
		MaxActiveRuns: MaxActiveRuns, MaxOpenWorkUnits: MaxOpenWorkUnits,
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	invalid := []RunGovernanceConfig{
		{DefaultRunTTLSeconds: 1},
		{Enabled: true},
		{Enabled: true, DefaultRunBudgetMicrosUSD: 2, MaxRunBudgetMicrosUSD: 1, DefaultRunTTLSeconds: 1, MaxRunTTLSeconds: 1, MaxActiveRuns: 1, MaxOpenWorkUnits: 1},
		{Enabled: true, DefaultRunBudgetMicrosUSD: 1, MaxRunBudgetMicrosUSD: 1, DefaultRunTTLSeconds: 1, MaxRunTTLSeconds: MaxRunTTLSeconds + 1, MaxActiveRuns: 1, MaxOpenWorkUnits: 1},
		{Enabled: true, DefaultRunBudgetMicrosUSD: 1, MaxRunBudgetMicrosUSD: 1, DefaultRunTTLSeconds: 1, MaxRunTTLSeconds: 1, MaxActiveRuns: MaxActiveRuns + 1, MaxOpenWorkUnits: 1},
	}
	for index, candidate := range invalid {
		if err := candidate.Validate(); err == nil {
			t.Fatalf("invalid config %d passed", index)
		}
	}
}

func TestWorkUnitAndRunLifecycleShapes(t *testing.T) {
	now := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	workUnit := WorkUnit{ID: "wku_1", ProjectID: "prj_1", Status: WorkUnitOpen, CreatedByKeyID: "key_1", CreatedAt: now, PeriodID: "2026-09-04", PeriodTimezoneVersion: 1}
	if err := workUnit.Validate(); err != nil {
		t.Fatal(err)
	}
	run := Run{ID: "run_1", ProjectID: "prj_1", WorkUnitID: workUnit.ID, BudgetMicrosUSD: 1, Status: RunActive, CreatedByKeyID: "key_1", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := run.Validate(); err != nil {
		t.Fatal(err)
	}
	closed := now.Add(time.Minute)
	workUnit.Status, workUnit.ClosedAt = WorkUnitClosed, &closed
	run.Status, run.ClosedAt, run.CloseReason = RunClosed, &closed, "completed"
	if err := workUnit.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := run.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestEffectiveRunStatusDerivesExpiryWithoutOverridingClose(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	run := Run{Status: RunActive, ExpiresAt: now}
	if got := EffectiveRunStatus(run, now); got != RunExpired {
		t.Fatalf("status=%q want expired", got)
	}
	run.Status = RunClosed
	if got := EffectiveRunStatus(run, now); got != RunClosed {
		t.Fatalf("closed status=%q", got)
	}
}
