package app

import (
	"context"
	"fmt"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
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

// SummaryRebuildReport is what a rebuild wrote, so an operator can compare it
// against what the console shows instead of taking the command's word for it.
type SummaryRebuildReport struct {
	Watermark      ledger.Watermark `json:"watermark"`
	RollupVersion  int              `json:"rollup_version"`
	RollupRows     int              `json:"rollup_rows"`
	AccountingDays int              `json:"accounting_days"`
}

// RebuildUsageSummary discards both Usage derivatives and rewrites them from
// the Ledger.
//
// It needs a writable metadata handle, which is what separates it from compact,
// verify and prune — those only read a derived view. It also replays into a
// full aggregate rather than streaming straight into rollup rows: the rollup
// and the checkpoint have to be written in one transaction describing one
// position in the WAL, and the checkpoint payload only exists if the aggregate
// does. That makes the command pay the same memory cost as `usage compact`
// already does on a long-lived instance.
//
// The instance must be stopped: the data lock is exclusive, by design, because
// one process owns one data directory.
func RebuildUsageSummary(ctx context.Context, cfg config.Config) (SummaryRebuildReport, error) {
	aggregate, _, closeResources, err := openUsageOffline(ctx, cfg)
	if err != nil {
		return SummaryRebuildReport{}, err
	}
	defer closeResources()
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		return SummaryRebuildReport{}, fmt.Errorf("open metadata: %w", err)
	}
	defer store.Close()
	snapshot, err := aggregate.TakeCheckpoint()
	if err != nil {
		return SummaryRebuildReport{}, err
	}
	// Clear first: a stale row that the rebuild does not happen to overwrite
	// would survive as a phantom day, and the increments are additive, so a
	// surviving row would be added to rather than replaced.
	if err := store.ResetUsageDerivatives(); err != nil {
		return SummaryRebuildReport{}, fmt.Errorf("reset usage derivatives: %w", err)
	}
	report := SummaryRebuildReport{
		Watermark: snapshot.Watermark, RollupVersion: domain.RollupVersion,
		RollupRows: len(snapshot.Rollup),
	}
	days := make(map[string]struct{})
	for encoded := range snapshot.Rollup {
		key, err := domain.DecodeRollupKey(encoded)
		if err != nil {
			return SummaryRebuildReport{}, err
		}
		days[domain.RollupDayPrefix(key.PeriodID, key.TimezoneVersion)] = struct{}{}
	}
	report.AccountingDays = len(days)
	if snapshot.Watermark.Sequence == 0 {
		// An empty ledger has nothing to write, and PutUsageCheckpoint refuses a
		// zero watermark. Both views are already cleared, which is the correct
		// state for an instance that has billed nothing.
		return report, nil
	}
	if err := store.PutUsageCheckpoint(
		snapshot.Watermark, snapshot.Payload, domain.RollupVersion, snapshot.Rollup,
	); err != nil {
		return SummaryRebuildReport{}, fmt.Errorf("write usage derivatives: %w", err)
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
