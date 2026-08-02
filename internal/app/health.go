package app

import (
	"context"
	"time"

	"github.com/akz142857/Heimdall/internal/provider"
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
	for _, deployment := range deployments {
		if !deployment.Enabled || deployment.DeletedAt != nil {
			continue
		}
		instance, instanceErr := r.store.GetProvider(ctx, deployment.ProviderID)
		if instanceErr != nil || !instance.Enabled || instance.DeletedAt != nil {
			r.providers.SetDeploymentHealthy(deployment.ID, false)
			continue
		}
		adapter, ok := adapterForDeployment(r.providers, instance, deployment)
		if !ok {
			r.providers.SetDeploymentHealthy(deployment.ID, false)
			continue
		}
		prober, ok := adapter.(provider.Prober)
		if !ok {
			// Lack of an active probe endpoint must not disable an otherwise
			// routable custom adapter; passive circuit health still applies.
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, timeout)
		err := prober.Probe(probeCtx, deployment.ProviderModel)
		cancel()
		r.providers.SetDeploymentHealthy(deployment.ID, err == nil)
		if err != nil {
			r.logger.Warn("active deployment probe failed",
				"deployment_id", deployment.ID, "provider_id", deployment.ProviderID,
				"error", err)
		}
	}
}
