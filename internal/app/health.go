package app

import (
	"context"
	"time"

	"github.com/akz142857/Halro/internal/provider"
)

func (r *Runtime) runActiveDeploymentProbes(ctx context.Context) {
	timer := time.NewTimer(r.healthProbeInterval())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			r.probeDeployments(ctx)
			timer.Reset(r.healthProbeInterval())
		}
	}
}

func (r *Runtime) healthProbeInterval() time.Duration {
	settings := r.runtimeSettings.Load()
	if settings == nil || settings.HealthProbeIntervalSeconds <= 0 {
		return r.config.Gateway.HealthProbeInterval.Value()
	}
	return time.Duration(settings.HealthProbeIntervalSeconds) * time.Second
}

func (r *Runtime) probeDeployments(ctx context.Context) {
	deployments, err := r.store.ListDeployments(ctx)
	if err != nil {
		r.logger.Warn("active deployment probe skipped", "error", err)
		return
	}
	timeout := r.config.Gateway.AttemptResponseHeaderTimeout.Value()
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	// This loop holds the only authoritative list of deployments the registry
	// ever sees, so it is where a probe result outliving its deployment is
	// caught. Collected before the probes run and applied after, so a record
	// written during this pass is not pruned by a list read before it.
	live := make([]string, 0, len(deployments))
	defer func() { r.providers.RetainDeploymentProbes(live) }()
	for _, deployment := range deployments {
		if !deployment.Enabled || deployment.DeletedAt != nil {
			continue
		}
		live = append(live, deployment.ID)
		instance, instanceErr := r.store.GetProvider(ctx, deployment.ProviderID)
		if instanceErr != nil || !instance.Enabled || instance.DeletedAt != nil {
			r.recordDeploymentProbe(deployment.ID, errProviderUnavailable)
			continue
		}
		adapter, ok := adapterForDeployment(r.providers, instance, deployment)
		if !ok {
			r.recordDeploymentProbe(deployment.ID, errProviderUnavailable)
			continue
		}
		prober, ok := adapter.(provider.Prober)
		if !ok {
			// Lack of an active probe endpoint must not disable an otherwise
			// routable custom adapter; passive circuit health still applies.
			continue
		}
		// Asked before the wire, like the manual connection test: a model this
		// profile's route refuses is a deployment that cannot serve, and the
		// probe endpoint cannot see that — on Bedrock Mantle the model list
		// enumerates the account rather than the route, so it answers for a
		// model the route will refuse. Without this the active probe keeps
		// reporting healthy while every request the deployment serves is
		// refused, which is exactly the disagreement a reader resolves in the
		// wrong direction.
		err := deploymentRouteRefusal(r.effectiveModelCatalog(), instance, deployment)
		if err == nil {
			probeCtx, cancel := context.WithTimeout(ctx, timeout)
			err = prober.Probe(probeCtx, deployment.ProviderModel)
			cancel()
		}
		failure := r.recordDeploymentProbe(deployment.ID, err)
		if err != nil {
			// The same field set a manual connection test logs, for the same
			// reason. This loop used to log the classified error itself, whose
			// message carries the upstream's sentence — so an upstream that
			// echoed a credential back inside its refusal had it written to the
			// log once per probe interval, forever.
			r.logger.Warn("active deployment probe failed", append(
				[]any{"deployment_id", deployment.ID, "provider_id", deployment.ProviderID},
				probeFailureAttributes(failure)...)...)
		}
	}
}

// errProviderUnavailable is the reason for the two cases that never reach a
// provider: the deployment's instance is gone or disabled, and no adapter is
// bound to it. Both remove the deployment from routing, so both owe the console
// a reason rather than an unexplained "unhealthy".
//
// ErrorBadRequest with no upstream status is what persistedProbeClass reads as
// a local refusal, which is precisely what this is: Halro declined to probe
// before any request left the process, and the console already has that
// sentence.
var errProviderUnavailable = &provider.Error{Class: provider.ErrorBadRequest, Message: "provider is not available for probing"}

// recordDeploymentProbe stores the verdict and, when it failed, the classified
// reason. The class is the same one a manual connection test persists, so the
// console has one wording table for both.
//
// It returns that classification rather than keeping it, so the caller can log
// the failure without describing the error a second time — and so the stored
// class and the logged one can never disagree about the same probe.
func (r *Runtime) recordDeploymentProbe(deploymentID string, err error) probeFailure {
	probe := provider.DeploymentProbe{Healthy: err == nil, ObservedAt: r.clockNow().UTC()}
	if err == nil {
		r.providers.SetDeploymentProbe(deploymentID, probe)
		return probeFailure{}
	}
	failure := describeProbeFailure(err)
	probe.ErrorClass = persistedProbeClass(failure)
	r.providers.SetDeploymentProbe(deploymentID, probe)
	return failure
}
