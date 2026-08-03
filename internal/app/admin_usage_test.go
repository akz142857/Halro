package app

import (
	"testing"

	gatewaycore "github.com/akz142857/Heimdall/internal/gateway"
)

func TestDashboardGovernanceSummaries(t *testing.T) {
	rejections := summarizeRejections(gatewaycore.RejectionMetrics{
		RPM: 1, TPM: 2, ProjectConcurrency: 3, ProviderConcurrency: 4,
		DeploymentConcurrency: 5, Budget: 6, TokenGuard: 7,
	})
	if rejections.Total != 28 || rejections.TokenGuard != 7 {
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
