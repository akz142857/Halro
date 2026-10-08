package app

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/usage"
)

// PruneUsage authenticates the complete native source using private disk
// references. Every physical batch has finite limits and independent authority;
// canceled or failed jobs preserve durable progress and return no success report.
func PruneUsage(ctx context.Context, cfg config.Config, cutoff time.Time) (report usage.RetentionReport, err error) {
	if cutoff.IsZero() || cutoff.UTC().Year() < 1 || cutoff.UTC().Year() > 9999 {
		return report, usage.ErrDiskUsageLimit
	}
	if cfg.Replication != nil {
		return report, errors.New("offline Usage prune is unavailable on HA members; use role-owned maintenance")
	}
	if err := replication.GuardUnavailableRuntime(cfg.Storage.DataDir, false, "prune Usage archive"); err != nil {
		return report, err
	}
	ctx, _, err = commandBudgetContext(ctx, cfg)
	if err != nil {
		return report, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	defer func() {
		err = errors.Join(err, ctx.Err())
		if err != nil {
			report = usage.RetentionReport{}
		}
	}()
	date := cutoff.UTC().Format("2006-01-02")
	report.Cutoff = date
	// A successor policy cannot replace an unfinished original-chain obligation.
	// The date read here is only a selector: each reclaim reauthenticates its
	// signed policy against fresh native references before any unlink.
	var current usage.ArchivePosition
	_, err = withUsageDiskReference(ctx, cfg, func(ctx context.Context, _ *usage.DiskProjection, e *usage.Exporter, _ usage.ReconciliationReport) error {
		var err error
		current, err = e.ReadArchivePosition(ctx)
		return err
	}, nil)
	if err != nil {
		return report, err
	}
	complete := func(at time.Time) error {
		_, err := withUsageDiskReference(ctx, cfg, func(ctx context.Context, p *usage.DiskProjection, e *usage.Exporter, _ usage.ReconciliationReport) error {
			if err := e.ResetPublishedDateCatalogVerification(ctx); err != nil {
				return err
			}
			if err := e.ResetDateCatalogReferences(ctx); err != nil {
				return err
			}
			if _, _, _, err := prepareUsageDateReclaim(ctx, p, e, at); err != nil {
				return err
			}
			present, err := e.HasCurrentDatePageReclamation(ctx, p, at)
			if err != nil {
				return err
			}
			if !present {
				return e.ResetRetainedDateReclamation(ctx)
			}
			return nil
		}, nil)
		if err != nil {
			return err
		}
		for {
			out, err := ReclaimUsageDatePagesStep(ctx, cfg, at)
			if err != nil {
				return err
			}
			if out.Data.FilesRemoved < 0 || out.Data.RowsVerified < 0 || out.Data.FilesRemoved > math.MaxInt-report.FilesRemoved || int64(out.Data.RowsVerified) > math.MaxInt64-report.RowsRemoved {
				return usage.ErrDiskUsageLimit
			}
			report.FilesRemoved += out.Data.FilesRemoved
			report.RowsRemoved += int64(out.Data.RowsVerified)
			if out.Reclamation.Phase == "ready" {
				break
			}
		}
		_, err = CompleteUsageDateObligation(ctx, cfg, at)
		return err
	}
	if current.Retention != (usage.ArchiveDateRetention{}) {
		if date < current.Retention.RetainFromDate {
			return report, errors.New("Usage prune cannot restore an earlier retention date")
		}
		previous, err := time.Parse("2006-01-02", current.Retention.RetainFromDate)
		if err != nil {
			return report, err
		}
		if err := complete(previous); err != nil {
			return report, fmt.Errorf("resume Usage prune: %w", err)
		}
		if date == current.Retention.RetainFromDate {
			return report, nil
		}
	}
	// Discard only stale derivative build/comparison hints. A signed unpublished
	// policy must be resumed under its own date, never silently overwritten.
	_, err = withUsageDiskReference(ctx, cfg, func(ctx context.Context, p *usage.DiskProjection, e *usage.Exporter, _ usage.ReconciliationReport) error {
		pending, err := e.ReadPendingDatePolicy(ctx, p)
		if err != nil {
			return err
		}
		if !pending.IsZero() {
			if pending.UTC().Format("2006-01-02") != date {
				return errors.New("resume the unfinished Usage prune date before publishing a successor")
			}
			return nil
		}
		return errors.Join(e.ResetRetainedDateCatalogBuild(ctx), e.ResetRetainedDateCatalogVerification(ctx))
	}, nil)
	if err != nil {
		return report, err
	}
	if _, err := ActivateUsageDateRetention(ctx, cfg, cutoff); err != nil {
		return report, fmt.Errorf("activate Usage prune: %w", err)
	}
	if err := complete(cutoff); err != nil {
		return report, fmt.Errorf("complete Usage prune: %w", err)
	}
	return report, nil
}
