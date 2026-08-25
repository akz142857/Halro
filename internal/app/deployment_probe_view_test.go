package app

import (
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/provider"
)

// Not probed and unhealthy are different states, and reporting the first as the
// second would have the console claim an outage on every restart — the registry
// deliberately keeps an unprobed deployment eligible for routing.
func TestProbeViewSeparatesNotProbedFromUnhealthy(t *testing.T) {
	view := probeView(map[string]provider.DeploymentProbe{}, "dep_1")

	if view.State != deploymentProbeNotProbed {
		t.Fatalf("state=%q, want %q", view.State, deploymentProbeNotProbed)
	}
	if view.ErrorClass != "" || view.ObservedAt != "" {
		t.Fatalf("a deployment nothing probed reported a result: %+v", view)
	}
}

// The reason travels as a class. A probe failure's sentence is the upstream's
// prose about the request, and this view is read by the console and cached in a
// browser; the class is what the console already has wording for.
func TestProbeViewCarriesTheClassifiedReason(t *testing.T) {
	observed := time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC)
	probes := map[string]provider.DeploymentProbe{
		"dep_1": {Healthy: false, ObservedAt: observed, ErrorClass: string(provider.ErrorConnect)},
	}

	view := probeView(probes, "dep_1")

	if view.State != deploymentProbeUnhealthy {
		t.Fatalf("state=%q, want %q", view.State, deploymentProbeUnhealthy)
	}
	if view.ErrorClass != "connect" {
		t.Fatalf("error class=%q", view.ErrorClass)
	}
	if view.ObservedAt != "2026-08-25T02:00:00Z" {
		t.Fatalf("observed at=%q", view.ObservedAt)
	}
}

func TestProbeViewReportsAHealthyProbeWithoutAReason(t *testing.T) {
	probes := map[string]provider.DeploymentProbe{"dep_1": {Healthy: true, ObservedAt: time.Now().UTC()}}

	view := probeView(probes, "dep_1")

	if view.State != deploymentProbeHealthy {
		t.Fatalf("state=%q, want %q", view.State, deploymentProbeHealthy)
	}
	if view.ErrorClass != "" {
		t.Fatalf("a passing probe carried a reason: %q", view.ErrorClass)
	}
}

// The two cases that never reach a provider — its instance is gone or disabled,
// and no adapter is bound — still remove the deployment from routing, so they
// owe the console a reason. It is Halro's own refusal, and it has to read as one
// rather than as the upstream rejecting a request it never saw.
func TestUnprobableProviderIsReportedAsALocalRefusal(t *testing.T) {
	if class := persistedProbeClass(describeProbeFailure(errProviderUnavailable)); class != localProbeRefusalClass {
		t.Fatalf("class=%q, want %q", class, localProbeRefusalClass)
	}
}
