package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/accountinglimits"
	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// Bind every library-owned index/report to the same immutable command manager.
// Each file takes its own whole lifetime claim; no mirror budget or reuse of
// another still-live file's capacity is allowed.
func backupWorkspaceContext(ctx context.Context, cfg config.Config) (context.Context, error) {
	ctx, pressure, err := commandBudgetContext(ctx, cfg)
	if err != nil || cfg.ResourceBudget == nil {
		return ctx, err
	}
	limit := backupCatalogDiskCeiling(cfg)
	if limit <= 0 {
		return nil, errors.New("budgeted backup verification/report requires positive backup_workspace_disk_bytes")
	}
	ram, err := accountinglimits.ProjectionMappingRAMBytes(limit)
	if err != nil {
		return nil, err
	}
	scope, ok := ctx.Value(commandResourceScopeKey{}).(*commandResourceScope)
	if !ok || pressure.logicalBudget() == nil {
		return nil, errors.New("backup workspace lost its root command manager")
	}
	scope.backupMu.Lock()
	defer scope.backupMu.Unlock()
	if scope.backupBudget == nil {
		manager := pressure.logicalBudget()
		root := ctx
		scope.backupBudget, err = backup.NewWorkspaceBudget(ctx, limit, func(ctx context.Context, bytes int64) (func() error, error) {
			if err := errors.Join(root.Err(), ctx.Err()); err != nil {
				return nil, err
			}
			if bytes != limit {
				return nil, resourcebudget.ErrInvalid
			}
			permit, err := manager.TryAcquireRecovery(resourcebudget.Cost{RAM: ram, Disk: limit})
			if err != nil {
				return nil, fmt.Errorf("reserve backup workspace RAM=%d Disk=%d: %w", ram, limit, err)
			}
			return permit.Release, nil
		})
		if err != nil {
			return nil, err
		}
	}
	return scope.backupBudget.Bind(ctx)
}

// Source/catalog observation indexes are control data, not the staged archive
// payload. The bounded engine caps their actual bbolt mapping independently;
// exhaustion refuses the complete operation instead of omitting source rows.
const boundedBackupCatalogDiskBytes int64 = 64 << 20

func backupCatalogDiskCeiling(cfg config.Config) int64 {
	if cfg.ResourceBudget == nil {
		return 0
	}
	limit := cfg.ResourceBudget.BackupWorkspaceDiskBytes
	if cfg.ResourceBudget.MemoryStateRequired {
		return min(limit, boundedBackupCatalogDiskBytes)
	}
	return limit
}
