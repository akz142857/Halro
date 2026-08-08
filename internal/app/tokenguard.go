package app

import (
	"context"
	"fmt"
	"log/slog"

	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/tokenguard"
)

func loadTokenGuard(ctx context.Context, store *boltstore.Store, logger *slog.Logger) (*tokenguard.Manager, error) {
	policies, err := store.ListTokenGuardPolicies(ctx)
	if err != nil {
		return nil, fmt.Errorf("list token guard policies: %w", err)
	}
	manager, err := tokenguard.New(policies)
	if err != nil {
		return nil, fmt.Errorf("load token guard policies: %w", err)
	}
	if payload, checkpointErr := store.TokenGuardCheckpoint(); checkpointErr == nil {
		if restoreErr := manager.RestoreCheckpoint(payload); restoreErr != nil {
			// Baselines are advisory detect-only state. Corruption must fall back
			// to fixed limits instead of preventing Gateway startup.
			_ = store.DeleteTokenGuardCheckpoint()
			logger.Warn("Token Guard checkpoint ignored; fixed limits remain active", "error", restoreErr)
		}
	} else if checkpointErr != boltstore.ErrNotFound {
		_ = store.DeleteTokenGuardCheckpoint()
		logger.Warn("Token Guard checkpoint ignored; fixed limits remain active", "error", checkpointErr)
	}
	projects, err := store.ListProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("list projects for token guard: %w", err)
	}
	for _, project := range projects {
		if project.DeletedAt == nil && project.Enabled && project.TokenGuardPolicyID != "" &&
			!manager.HasPolicy(project.TokenGuardPolicyID) {
			return nil, fmt.Errorf(
				"project %q references unavailable token guard policy %q",
				project.ID,
				project.TokenGuardPolicyID,
			)
		}
	}
	return manager, nil
}
