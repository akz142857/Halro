package routegate

import "time"

// DeploymentProbe is the last active-probe verdict for one deployment.
//
// It carries why as well as whether, because the verdict alone leaves an
// operator with a deployment that is enabled, tested and priced and still takes
// no traffic. The reason is the classified error only: a probe failure's
// sentence is the upstream's prose about the request, and it stays inside the
// error rather than being copied into state the console and the logs read.
type DeploymentProbe struct {
	Healthy    bool
	ObservedAt time.Time
	// Empty when healthy. The classified form the console already has wording
	// for, so a probe failure and a manual test failure read the same way.
	ErrorClass string
}

// ObserveProbe records an active-probe verdict and feeds it into admission.
//
// A probe is always evidence about the deployment it probed, never wider. It
// cannot tell an endpoint-wide outage from one model misbehaving — it only asked
// about one model — and the narrow reading is the one that costs a rediscovery
// rather than stranding deployments that work.
//
// A passing probe clears that deployment and nothing else, which is the whole
// difference between this and a completed request. The probe endpoint is a model
// listing: it consumes no quota, so an account at zero balance answers it
// perfectly well. Letting it clear a credential suspension would hand traffic
// back to an upstream that is still refusing to serve, once every probe
// interval, forever.
func (g *Gate) ObserveProbe(deploymentID string, probe DeploymentProbe, now time.Time) {
	if deploymentID == "" {
		return
	}
	scope := Scope{Kind: ScopeDeployment, Key: deploymentID}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.probes == nil {
		g.probes = make(map[string]DeploymentProbe)
	}
	g.probes[deploymentID] = probe
	if probe.Healthy {
		delete(g.scopes, scope)
		return
	}
	// Threshold one, not the availability threshold. That threshold exists
	// because a single 5xx among real traffic is noise; a probe is a deliberate
	// health check whose whole purpose is to answer this question, and making a
	// failed one wait for four more would leave a dead deployment taking traffic
	// for several probe intervals.
	policy := availabilityPolicy(
		ScopeDeployment, 1,
		g.config.AvailabilityWindow, g.config.MaxAvailabilityWindow,
	)
	g.observeLocked(scope, policy, Observation{Code: probe.ErrorClass}, now, 0)
}

// DeploymentProbes is the last verdict for every deployment still known, for the
// console and the metrics endpoint.
func (g *Gate) DeploymentProbes() map[string]DeploymentProbe {
	g.mu.Lock()
	defer g.mu.Unlock()
	result := make(map[string]DeploymentProbe, len(g.probes))
	for deploymentID, probe := range g.probes {
		result[deploymentID] = probe
	}
	return result
}

// RetainDeployments forgets every deployment not named, and anything suspended
// against one.
//
// Nothing else removes them. State outliving its deployment is how the metrics
// exporter came to emit a series for an ID that no longer exists, and a label
// set that only ever grows is the shape this repository bans by name. The caller
// is the probe loop, which reads the deployment list from the store and is
// therefore the only place that knows which IDs are still real.
func (g *Gate) RetainDeployments(deploymentIDs []string) {
	live := make(map[string]struct{}, len(deploymentIDs))
	for _, id := range deploymentIDs {
		live[id] = struct{}{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for id := range g.probes {
		if _, ok := live[id]; !ok {
			delete(g.probes, id)
		}
	}
	for scope := range g.scopes {
		if scope.Kind != ScopeDeployment {
			continue
		}
		if _, ok := live[scope.Key]; !ok {
			delete(g.scopes, scope)
		}
	}
}
