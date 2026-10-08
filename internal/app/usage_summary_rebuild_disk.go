package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/replication"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/usage"
)

// RebuildUsageSummary authenticates native facts into fresh disk references,
// then publishes all daily rollups in finite transactions. The marker is NOT a
// restorable Aggregate or an authenticated startup snapshot. Aggregate startup
// must discard it and replay; neither old rollups nor old checkpoints contribute.
func RebuildUsageSummary(ctx context.Context, cfg config.Config) (SummaryRebuildReport, error) {
	return rebuildUsageSummary(ctx, cfg, nil, nil)
}

func rebuildUsageSummary(ctx context.Context, cfg config.Config, beforeRevalidate, afterPublish func() error) (report SummaryRebuildReport, err error) {
	defer func() {
		if err != nil {
			report = SummaryRebuildReport{}
		}
	}()
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if cfg.Replication != nil {
		return report, errors.New("rebuild-summary requires an offline Standalone directory")
	}
	if err := replication.GuardUnavailableRuntime(cfg.Storage.DataDir, false, "rebuild Usage summary"); err != nil {
		return report, err
	}
	_, err = withUsageDiskReferencePublication(ctx, cfg,
		func(context.Context, *usage.DiskProjection, *usage.Exporter, usage.ReconciliationReport) error {
			return nil
		},
		func(ctx context.Context, disk *usage.DiskProjection, store *boltstore.Store, scope usage.ReconciliationReport) (err error) {
			if err := replication.GuardUnavailableRuntime(cfg.Storage.DataDir, false, "rebuild Usage summary"); err != nil {
				return err
			}
			var cursor usage.DiskRollupSnapshotCursor
			replacement, err := store.ReplaceUsageRollups(ctx, scope.SourceHead, func(ctx context.Context) ([]boltstore.UsageRollupReplacementRow, bool, error) {
				page, err := disk.RollupSnapshotPage(ctx, cursor, usage.DiskUsagePageLimits{ScanRows: 100, InputBytes: 8 << 20, OutputBytes: 8 << 20})
				if err != nil {
					return nil, false, err
				}
				if page.Head != scope.SourceHead || !page.Done && page.Next == cursor {
					return nil, false, errors.New("Usage rollup source does not match frozen complete reference")
				}
				cursor = page.Next
				rows := make([]boltstore.UsageRollupReplacementRow, len(page.Rows))
				for i, row := range page.Rows {
					rows[i] = boltstore.UsageRollupReplacementRow{Key: row.Key, Row: row.Row}
				}
				return rows, page.Done, nil
			})
			if err != nil {
				return fmt.Errorf("replace Usage summary: %w", err)
			}
			report = SummaryRebuildReport{Watermark: scope.SourceHead, RollupVersion: domain.RollupVersion, RollupRows: replacement.Rows, AccountingDays: replacement.AccountingDays}
			if afterPublish != nil {
				return afterPublish()
			}
			return nil
		}, beforeRevalidate)
	return report, err
}
