package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/usage"
	"github.com/akz142857/Halro/internal/vault"
)

// Legacy aggregate/export helpers exist only to construct compatibility fixtures.
func openUsageOffline(
	ctx context.Context,
	cfg config.Config,
) (*usage.Aggregate, *usage.Exporter, func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	dataLock, err := lock.AcquireExistingReadOnly(cfg.Storage.DataDir)
	if err != nil {
		return nil, nil, nil, err
	}
	closeResources := func() error { return dataLock.Close() }
	aggregate := usage.NewAggregate()
	if err := replayUsageLedger(ctx, cfg, aggregate); err != nil {
		closeResources()
		return nil, nil, nil, fmt.Errorf("replay usage ledger: %w", err)
	}
	archiveBudget, releaseArchive, err := newOneShotUsageArchiveDiskBudget(ctx, cfg)
	if err != nil {
		closeResources()
		return nil, nil, nil, err
	}
	var reserveArchiveDisk func(context.Context, int64) (func() error, error)
	if archiveBudget != nil {
		reserveArchiveDisk = archiveBudget.reserve
	}
	exporter, err := usage.NewExporterWithOptions(cfg.UsagePath(), usage.Options{Format: cfg.Usage.ExportFormat, ArchiveDiskBytes: usageArchiveDiskCeiling(cfg), ReserveArchiveDisk: reserveArchiveDisk, ReserveArchiveRAM: archiveBudget.readerReservation(cfg), ReserveArchiveWorkRAM: archiveBudget.workReservation()})
	if err != nil {
		_ = releaseArchive()
		closeResources()
		return nil, nil, nil, err
	}
	closeResources = func() error {
		closeErr := exporter.Close()
		if closeErr != nil {
			return errors.Join(closeErr, dataLock.Close())
		}
		return errors.Join(releaseArchive(), dataLock.Close())
	}
	return aggregate, exporter, closeResources, nil
}

// Authenticate before opening an exporter or a writable metadata handle. A
// failed replay must leave both the authority and its existing derivatives
// alone; even a partial tail is refused rather than repaired or published.
func replayUsageLedger(ctx context.Context, cfg config.Config, aggregate *usage.Aggregate) error {
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
	report, partial, err := ledger.InspectReplayAuthenticated(cfg.LedgerPath(), ledgerKey, func(record ledger.Record) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return aggregate.Apply(record)
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
