package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
)

// renderMetricsForTest scrapes /metrics the way Prometheus would, so what is
// asserted is the exposition an operator actually gets — not the struct behind
// it, which could render correctly and still be wired to nothing.
func renderMetricsForTest(t *testing.T, runtime *Runtime) string {
	t.Helper()
	writer := httptest.NewRecorder()
	if err := runtime.writeMetrics(writer); err != nil {
		t.Fatal(err)
	}
	body := writer.Body.String()
	// The exposition contract applies to the new families too: one HELP and one
	// TYPE each, and no label outside the allowlist.
	assertMetricsExpositionContract(t, body)
	return body
}

// Every §13 series must be present from the very first scrape, including the
// ones that are still zero. A series that only appears once it is non-zero
// cannot be alerted on with `== 0`, and "no drifted deployments" is exactly the
// condition worth asserting.
func TestCapabilityMetricsAreAllPresentAndZeroedAtStart(t *testing.T) {
	runtime, _ := bootstrapForCapabilityTest(t)
	body := renderMetricsForTest(t, runtime)

	for _, series := range []string{
		`halro_capability_drift_total{reason="catalog"} 0`,
		`halro_capability_drift_total{reason="profile"} 0`,
		`halro_model_revision_conflicts_total 0`,
		`halro_deployment_test_total{status="success"} 0`,
		`halro_deployment_test_total{status="failure"} 0`,
		`halro_deployment_capability_status{state="known"} 0`,
		`halro_deployment_capability_status{state="conflicting"} 0`,
	} {
		if !strings.Contains(body, series) {
			t.Fatalf("missing zeroed series %q", series)
		}
	}
	// Bootstrap declares its model, so that gauge is the one that is not zero.
	if !strings.Contains(body, "halro_operator_declared_deployments 1") {
		t.Fatalf("operator-declared gauge did not count the bootstrap deployment:\n%s",
			grepSeries(body, "halro_operator_declared_deployments"))
	}
	if !strings.Contains(body, `halro_deployment_capability_status{state="partial"} 1`) {
		t.Fatalf("snapshot status gauge did not count the bootstrap deployment:\n%s",
			grepSeries(body, "halro_deployment_capability_status"))
	}
}

// §13 requires the two drift sources to be distinguishable: a catalog that
// moved is an upstream fact, a narrowed profile is something this binary did,
// and they call for different responses.
func TestDriftMetricSeparatesProfileNarrowingFromCatalogMovement(t *testing.T) {
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	driftDeployment(t, runtime, bootstrap.DeploymentID)

	if err := runtime.reloadProviderRegistry(context.Background()); err != nil {
		t.Fatal(err)
	}

	body := renderMetricsForTest(t, runtime)
	// driftDeployment makes the snapshot exceed the running profile.
	if !strings.Contains(body, `halro_capability_drift_total{reason="profile"} 1`) {
		t.Fatalf("profile narrowing was not counted:\n%s", grepSeries(body, "halro_capability_drift_total"))
	}
	if !strings.Contains(body, `halro_capability_drift_total{reason="catalog"} 0`) {
		t.Fatalf("catalog drift was counted for a profile narrowing:\n%s",
			grepSeries(body, "halro_capability_drift_total"))
	}
}

func TestDeploymentTestOutcomeIsCounted(t *testing.T) {
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	cookie, csrf := loginAdminForTest(t, runtime)

	// The provider endpoint is unreachable in a test, so this is the failure
	// arm — which is the arm worth asserting, since a silent probe failure is
	// what the counter exists to surface.
	performAdminMutation(t, runtime, cookie, csrf, http.MethodPost,
		"/admin/api/v1/deployments/"+bootstrap.DeploymentID+"/test", "", map[string]any{})

	body := renderMetricsForTest(t, runtime)
	if strings.Contains(body, `halro_deployment_test_total{status="failure"} 0`) &&
		strings.Contains(body, `halro_deployment_test_total{status="success"} 0`) {
		t.Fatalf("a deployment test did not move either counter:\n%s",
			grepSeries(body, "halro_deployment_test_total"))
	}
}

// No model, provider or deployment identifier may become a Prometheus label:
// §13 forbids it, and the metrics port is scraped and retained indefinitely.
func TestCapabilityMetricsCarryNoObjectIdentifiers(t *testing.T) {
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	driftDeployment(t, runtime, bootstrap.DeploymentID)
	if err := runtime.reloadProviderRegistry(context.Background()); err != nil {
		t.Fatal(err)
	}

	for _, line := range strings.Split(renderMetricsForTest(t, runtime), "\n") {
		if !strings.HasPrefix(line, "halro_model_catalog") &&
			!strings.HasPrefix(line, "halro_capability_") &&
			!strings.HasPrefix(line, "halro_deployment_capability") &&
			!strings.HasPrefix(line, "halro_operator_declared") {
			continue
		}
		for _, forbidden := range []string{bootstrap.DeploymentID, bootstrap.ProviderID, "gpt-test"} {
			if strings.Contains(line, forbidden) {
				t.Fatalf("capability metric leaked an object identifier: %q", line)
			}
		}
	}
}

func TestDeploymentCapabilityGaugesIgnoreDeletedDeployments(t *testing.T) {
	deleted := domain.Deployment{ModelCapabilitySnapshot: domain.ModelCapabilitySnapshot{
		Status: "known", Source: "builtin_catalog",
	}}
	stamp := deleted.CreatedAt
	deleted.DeletedAt = &stamp
	live := domain.Deployment{ModelCapabilitySnapshot: domain.ModelCapabilitySnapshot{
		Status: "known", Source: "operator_declared",
	}}

	gauges := summariseDeploymentCapabilities([]domain.Deployment{deleted, live})

	if gauges.ByStatus["known"] != 1 {
		t.Fatalf("a deleted deployment was counted: %+v", gauges.ByStatus)
	}
	if gauges.OperatorDeclared != 1 {
		t.Fatalf("operator_declared=%d", gauges.OperatorDeclared)
	}
}

func grepSeries(body, prefix string) string {
	var matched []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, prefix) {
			matched = append(matched, line)
		}
	}
	return strings.Join(matched, "\n")
}
