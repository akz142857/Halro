package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/durable"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/usage"
	"github.com/akz142857/Halro/internal/vault"
)

func CompactUsage(ctx context.Context, cfg config.Config) (usage.Manifest, error) {
	aggregate, exporter, closeResources, err := openUsageOffline(ctx, cfg, false)
	if err != nil {
		return usage.Manifest{}, err
	}
	defer closeResources()
	manifest, err := exporter.Export(aggregate.History())
	if err != nil {
		return usage.Manifest{}, fmt.Errorf("compact usage: %w", err)
	}
	if err := exporter.Verify(aggregate.History()); err != nil {
		return usage.Manifest{}, fmt.Errorf("verify compacted usage: %w", err)
	}
	return manifest, nil
}

func VerifyUsage(ctx context.Context, cfg config.Config) (usage.ReconciliationReport, error) {
	aggregate, exporter, closeResources, err := openUsageOffline(ctx, cfg, false)
	if err != nil {
		return usage.ReconciliationReport{}, err
	}
	defer closeResources()
	report, err := exporter.Reconcile(aggregate.History())
	if err != nil {
		// A directory that has not exported yet has no manifest, and the bare
		// "open .../manifest.json: no such file or directory" this used to
		// print reads as data loss on an install that has none. It is still a
		// failure — an absent manifest under a ledger that was erased looks the
		// same — but the answer now says which state it cannot tell apart.
		//
		// The distinction is taken from the filesystem, not from the error:
		// Verify hashes every partition the manifest lists and returns the same
		// *os.PathError when one of those files is gone, which is exactly the
		// divergence this command exists to catch.
		if manifestAbsent(cfg) && aggregate.Watermark().Sequence == 0 {
			return usage.ReconciliationReport{}, errors.New(
				"usage holds no manifest: expected on a directory that has not exported usage yet, " +
					"and indistinguishable from one whose usage tree was erased")
		}
		return usage.ReconciliationReport{}, fmt.Errorf("verify usage: %w", err)
	}
	return report, nil
}

func manifestAbsent(cfg config.Config) bool {
	_, err := os.Stat(filepath.Join(cfg.UsagePath(), "manifest.json"))
	return errors.Is(err, os.ErrNotExist)
}

func PruneUsage(
	ctx context.Context,
	cfg config.Config,
	cutoff time.Time,
) (usage.RetentionReport, error) {
	if err := ctx.Err(); err != nil {
		return usage.RetentionReport{}, err
	}
	dataLock, err := lock.AcquireExistingReadOnly(cfg.Storage.DataDir)
	if err != nil {
		return usage.RetentionReport{}, err
	}
	defer dataLock.Close()
	// The archive is only touched under an authenticated Ledger: a directory
	// whose authority cannot be verified is not one to delete files from.
	if err := replayUsageLedger(ctx, cfg, func([]byte) error { return nil }, func(ledger.Record) error { return nil }); err != nil {
		return usage.RetentionReport{}, fmt.Errorf("replay usage ledger: %w", err)
	}
	exporter, err := usage.NewExporterWithOptions(cfg.UsagePath(), usage.Options{Format: cfg.Usage.ExportFormat})
	if err != nil {
		return usage.RetentionReport{}, err
	}
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

// RebuildUsageSummary empties the instance's usage history index and rewrites
// it from the Ledger. The index is a derivative, so this is always safe; it is
// what an operator runs when the console's figures are in doubt.
//
// The instance must be stopped: the data lock is exclusive, by design, because
// one process owns one data directory.
func RebuildUsageSummary(ctx context.Context, cfg config.Config) (SummaryRebuildReport, error) {
	aggregate, _, closeResources, err := openUsageOffline(ctx, cfg, true)
	if err != nil {
		return SummaryRebuildReport{}, err
	}
	defer closeResources()
	report := SummaryRebuildReport{Watermark: aggregate.Watermark(), RollupVersion: domain.RollupVersion}
	days := make(map[string]struct{})
	if err := aggregate.History().RollupRange("", "", func(key domain.RollupKey, _ domain.DailyRollup) error {
		report.RollupRows++
		days[domain.RollupDayPrefix(key.PeriodID, key.TimezoneVersion)] = struct{}{}
		return nil
	}); err != nil {
		return SummaryRebuildReport{}, err
	}
	report.AccountingDays = len(days)
	return report, nil
}

// openFreshHistory creates an empty usage history index at path, keyed for the
// Ledger whose key is given, and an aggregate to fill it.
func openFreshHistory(path string, ledgerKey []byte) (*usage.HistoryStore, *usage.Aggregate, error) {
	key, err := vault.DeriveHistoryHMACKey(ledgerKey)
	if err != nil {
		return nil, nil, err
	}
	defer clear(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, nil, err
	}
	history, _, err := usage.OpenHistory(path, key)
	if err != nil {
		return nil, nil, err
	}
	if err := history.Reset(); err != nil {
		history.Close()
		return nil, nil, err
	}
	return history, usage.NewAggregate(history), nil
}

// openUsageOffline replays the authenticated Ledger into a usage history index
// and returns the aggregate over it. Without rebuild the index is a scratch
// copy in a temporary directory, removed by the returned close; with rebuild it
// is the instance's own history.db, emptied first.
func openUsageOffline(
	ctx context.Context,
	cfg config.Config,
	rebuild bool,
) (*usage.Aggregate, *usage.Exporter, func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	dataLock, err := lock.AcquireExistingReadOnly(cfg.Storage.DataDir)
	if err != nil {
		return nil, nil, nil, err
	}
	var history *usage.HistoryStore
	scratch := ""
	var historyKey []byte
	closeResources := func() error {
		var errs []error
		if history != nil {
			errs = append(errs, history.Close())
		}
		if scratch != "" {
			errs = append(errs, os.RemoveAll(scratch))
		}
		errs = append(errs, dataLock.Close())
		clear(historyKey)
		return errors.Join(errs...)
	}
	var aggregate *usage.Aggregate
	// The index is always built in a scratch directory. A rebuild installs it
	// only once the whole Ledger has authenticated and replayed, so a refused
	// or interrupted command leaves the data directory as it found it.
	prepare := func(ledgerKey []byte) error {
		var err error
		if historyKey, err = vault.DeriveHistoryHMACKey(ledgerKey); err != nil {
			return err
		}
		dir, err := os.MkdirTemp("", "halro-usage-*")
		if err != nil {
			return err
		}
		scratch = dir
		history, aggregate, err = openFreshHistory(filepath.Join(dir, "history.db"), ledgerKey)
		return err
	}
	if err := replayUsageLedger(ctx, cfg, prepare, func(record ledger.Record) error {
		if err := aggregate.Apply(record); err != nil {
			return err
		}
		return aggregate.FlushIfFull()
	}); err != nil {
		closeResources()
		return nil, nil, nil, fmt.Errorf("replay usage ledger: %w", err)
	}
	if err := aggregate.Flush(); err != nil {
		closeResources()
		return nil, nil, nil, err
	}
	if rebuild {
		if err := history.Close(); err != nil {
			closeResources()
			return nil, nil, nil, err
		}
		history = nil
		if err := installHistory(filepath.Join(scratch, "history.db"), cfg.HistoryPath()); err != nil {
			closeResources()
			return nil, nil, nil, fmt.Errorf("install rebuilt usage history: %w", err)
		}
		if history, _, err = usage.OpenHistory(cfg.HistoryPath(), historyKey); err != nil {
			closeResources()
			return nil, nil, nil, err
		}
		if aggregate, err = usage.RestoreAggregate(history); err != nil {
			closeResources()
			return nil, nil, nil, err
		}
	}
	exporter, err := usage.NewExporterWithOptions(cfg.UsagePath(), usage.Options{Format: cfg.Usage.ExportFormat})
	if err != nil {
		closeResources()
		return nil, nil, nil, err
	}
	return aggregate, exporter, closeResources, nil
}

// installHistory copies a rebuilt index into place: written beside the target,
// synced, then renamed over it, so the target is either the old index or the
// complete new one.
func installHistory(source, target string) (err error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	staged := target + ".rebuild"
	out, err := os.OpenFile(staged, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(staged)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := errors.Join(out.Sync(), out.Close()); err != nil {
		return err
	}
	if err := os.Rename(staged, target); err != nil {
		return err
	}
	return durable.SyncDirectory(filepath.Dir(target))
}

// Authenticate before opening an exporter or a writable metadata handle. A
// failed replay must leave both the authority and its existing derivatives
// alone; even a partial tail is refused rather than repaired or published.
func replayUsageLedger(
	ctx context.Context,
	cfg config.Config,
	prepare func(ledgerKey []byte) error,
	apply func(ledger.Record) error,
) error {
	store, err := boltstore.OpenReadOnly(cfg.MetadataPath())
	if err != nil {
		if errors.Is(err, boltstore.ErrSchemaVersionMismatch) {
			return fmt.Errorf("%w; usage commands do not migrate metadata. Take a backup with the old binary before running `halro start` to upgrade, or use the binary that wrote this directory", err)
		}
		return fmt.Errorf("open metadata for ledger authentication: %w", err)
	}
	defer store.Close()
	masterKey, err := unlockMasterKey(ctx, cfg, store)
	if err != nil {
		return err
	}
	defer clear(masterKey)
	secretVault, err := vault.New(masterKey)
	if err != nil {
		return err
	}
	defer secretVault.Close()
	if err := verifyVaultKeyCheck(store, secretVault); err != nil {
		return err
	}
	ledgerKey, err := loadLedgerHMACKey(store, secretVault, masterKey)
	if err != nil {
		return err
	}
	defer clear(ledgerKey)
	if err := prepare(ledgerKey); err != nil {
		return err
	}
	report, partial, err := ledger.InspectReplayAuthenticated(cfg.LedgerPath(), ledgerKey, func(record ledger.Record) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return apply(record)
	})
	if err != nil {
		return err
	}
	if partial {
		return errors.New("ledger has a partial tail; start Halro to repair it before running usage commands")
	}
	checkpoint, err := store.LedgerChainCheckpoint()
	if err != nil {
		return fmt.Errorf("load ledger chain checkpoint: %w", err)
	}
	return verifyLedgerCheckpoint(report, checkpoint)
}

// pruneUsageArchive removes exported partitions past the archive's retention.
//
// The same sweep `halro usage prune` performs, moved onto the maintenance tick.
// It was a manual, offline command — it takes the data directory lock, so it
// required stopping the gateway — which meant the setting shipped since the
// beginning did nothing on an instance nobody remembered to stop. A retention
// value that only takes effect when someone runs a command is a retention value
// an auditor cannot be shown.
//
// The cutoff keeps one extra day on purpose; usage.RetentionCutoff says why,
// and is the same rule the offline `usage prune` command applies.
func (r *Runtime) pruneUsageArchive() {
	days := r.config.Usage.RetentionDays
	if days < 1 {
		return
	}
	cutoff := usage.RetentionCutoff(time.Now(), days)
	report, err := r.usageExporter.PruneBefore(cutoff)
	if err != nil {
		r.logger.Warn("usage archive not pruned", "error", err)
		return
	}
	if report.FilesRemoved == 0 {
		return
	}
	r.logger.Info("usage archive pruned",
		"files", report.FilesRemoved, "rows", report.RowsRemoved,
		"cutoff", report.Cutoff, "retention_days", days)
}
