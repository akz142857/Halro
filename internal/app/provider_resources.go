package app

import (
	"context"
	"errors"
	"time"
)

const providerResourceReapInterval = time.Hour

func (r *Runtime) runProviderResourceMaintenance(ctx context.Context) {
	r.reapProviderResources(ctx)
	ticker := time.NewTicker(providerResourceReapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.reapProviderResources(ctx)
		}
	}
}

func (r *Runtime) reapProviderResources(ctx context.Context) {
	expired, err := r.store.ExpiredProviderResources(ctx, time.Now().UTC())
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			r.logger.Error("provider resource TTL reaper failed", "error", err)
		}
		return
	}
	for _, resource := range expired {
		if err := r.gatewayService.CleanupExpiredProviderResource(ctx, resource); err != nil {
			r.logger.Error("provider resource cleanup failed", "resource_id", resource.ID, "error", err)
		}
	}
}
