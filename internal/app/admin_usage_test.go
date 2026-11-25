package app

import (
	"math"
	"testing"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/domain"
	gatewaycore "github.com/akz142857/Halro/internal/gateway"
	"github.com/akz142857/Halro/internal/ledger"
)

func TestDashboardGovernanceSummaries(t *testing.T) {
	rejections := summarizeRejections(gatewaycore.RejectionMetrics{
		RPM: 1, TPM: 2, ProjectConcurrency: 3, ProviderConcurrency: 4,
		DeploymentConcurrency: 5, Budget: 6, TokenGuard: 7, RunBudget: 8,
	})
	if rejections.Total != 36 || rejections.TokenGuard != 7 || rejections.RunBudget != 8 {
		t.Fatalf("rejections=%#v", rejections)
	}

	items := topPressure([]pressureItem{
		{ID: "low", Utilization: .2},
		{ID: "highest", Utilization: .9},
		{ID: "middle", Utilization: .6},
	}, 2)
	if len(items) != 2 || items[0].ID != "highest" || items[1].ID != "middle" {
		t.Fatalf("pressure=%#v", items)
	}
}

type pressureBalance ledger.Balance

func (b pressureBalance) Balance(string, string, uint64) ledger.Balance {
	return ledger.Balance(b)
}

func TestBudgetPressureDoesNotWrapAnOverBudgetBalance(t *testing.T) {
	projects := []domain.Project{{ID: "project", Enabled: true, DailyBudgetMicrosUSD: math.MaxInt64}}
	balance := pressureBalance{CommittedMicrosUSD: math.MaxInt64, ReservedMicrosUSD: 1}
	result := budgetPressure(projects, balance, budget.Period{})
	if len(result.Items) != 1 || result.Items[0].Current != math.MaxInt64 || result.AtRisk != 1 {
		t.Fatalf("budget pressure = %+v", result)
	}
}
