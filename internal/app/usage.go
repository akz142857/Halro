package app

import (
	"context"
	"fmt"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/usage"
)

func CompactUsage(ctx context.Context, cfg config.Config) (usage.Manifest, error) {
	aggregate, exporter, closeResources, err := openUsageOffline(ctx, cfg)
	if err != nil {
		return usage.Manifest{}, err
	}
	defer closeResources()
	manifest, err := exporter.Export(aggregate.Snapshot())
	if err != nil {
		return usage.Manifest{}, fmt.Errorf("compact usage: %w", err)
	}
	snapshot := aggregate.Snapshot()
	if err := exporter.Verify(&snapshot); err != nil {
		return usage.Manifest{}, fmt.Errorf("verify compacted usage: %w", err)
	}
	return manifest, nil
}

func VerifyUsage(ctx context.Context, cfg config.Config) (usage.ReconciliationReport, error) {
	aggregate, exporter, closeResources, err := openUsageOffline(ctx, cfg)
	if err != nil {
		return usage.ReconciliationReport{}, err
	}
	defer closeResources()
	snapshot := aggregate.Snapshot()
	report, err := exporter.Reconcile(snapshot)
	if err != nil {
		return usage.ReconciliationReport{}, fmt.Errorf("verify usage: %w", err)
	}
	return report, nil
}

func PruneUsage(
	ctx context.Context,
	cfg config.Config,
	cutoff time.Time,
) (usage.RetentionReport, error) {
	_, exporter, closeResources, err := openUsageOffline(ctx, cfg)
	if err != nil {
		return usage.RetentionReport{}, err
	}
	defer closeResources()
	report, err := exporter.PruneBefore(cutoff)
	if err != nil {
		return usage.RetentionReport{}, fmt.Errorf("prune usage: %w", err)
	}
	if err := exporter.Verify(nil); err != nil {
		return usage.RetentionReport{}, fmt.Errorf("verify retained usage: %w", err)
	}
	return report, nil
}

func openUsageOffline(
	ctx context.Context,
	cfg config.Config,
) (*usage.Aggregate, *usage.Exporter, func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return nil, nil, nil, err
	}
	// InspectReplay rather than Open: these commands read a derived view and
	// have no business holding a write handle on the accounting authority.
	// Open repairs a partial tail by truncating it, so `usage export` on a WAL
	// with a torn last frame would rewrite the Ledger as a side effect of
	// producing a report — with no key, no chain verification, and no
	// checkpoint reconciliation to catch it having done so.
	closeResources := func() error { return dataLock.Close() }
	aggregate := usage.NewAggregate()
	if _, _, err := ledger.InspectReplay(cfg.LedgerPath(), func(record ledger.Record) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return aggregate.Apply(record)
	}); err != nil {
		closeResources()
		return nil, nil, nil, fmt.Errorf("replay usage ledger: %w", err)
	}
	exporter, err := usage.NewExporterWithOptions(cfg.UsagePath(), usage.Options{Format: cfg.Usage.ExportFormat})
	if err != nil {
		closeResources()
		return nil, nil, nil, err
	}
	return aggregate, exporter, closeResources, nil
}
