package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	removed, err := r.store.ReapProviderResources(ctx, time.Now().UTC())
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			r.logger.Error("provider resource TTL reaper failed", "error", err)
		}
		return
	}
	objectDir := filepath.Join(r.config.Storage.DataDir, "provider-objects")
	for _, resource := range removed {
		if resource.ObjectPath == "" || filepath.Base(resource.ObjectPath) != resource.ObjectPath {
			continue
		}
		if err := os.Remove(filepath.Join(objectDir, resource.ObjectPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
			r.logger.Error("provider resource object cleanup failed", "resource_id", resource.ID, "error", err)
		}
	}
}
