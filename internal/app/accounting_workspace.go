package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/accountinglimits"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/usage"
)

// Ledger/Usage/Governance construction, Usage window validation and two-level
// recovery are sequential startup jobs. Each uses
// the configured peak envelope, charged before derived-file mutation or input
// decoding, until their synchronous construction/cleanup stack has returned.
// Returned live DBs, mmap, native Open and persistent disk need separate owners.
func acquireProjectionWorkspace(ctx context.Context, cfg config.Config, pressure *resourcePressureRuntime) (func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	manager := pressure.logicalBudget()
	if cfg.ResourceBudget == nil {
		if manager != nil {
			return nil, errors.New("projection workspace manager has no matching configuration")
		}
		return func() error { return nil }, nil
	}
	amount := cfg.ResourceBudget.AccountingRecoveryRAMBytes
	if manager == nil || amount < accountinglimits.MinRecoveryRAMBytes {
		return nil, errors.New("projection requires its configured shared Recovery workspace")
	}
	permit, err := manager.TryAcquireRecovery(resourcebudget.Cost{RAM: amount})
	if err != nil {
		return nil, err
	}
	return permit.Release, nil
}

// A fresh Ledger projection is startup recovery work until its verified head
// is ready for publication. Reserve its configured full-file ceiling before
// deleting/rebuilding the candidate, including its mapping/freelist RAM.
// prepareRuntimeAccountingState moves this
// same claim to Background only after the file is complete and role cut checked.
func acquireLedgerProjectionDisk(ctx context.Context, cfg config.Config, pressure *resourcePressureRuntime) (*runtimeProjectionDiskPermit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.ResourceBudget == nil || cfg.ResourceBudget.LedgerProjectionDiskBytes == 0 {
		return nil, nil
	}
	manager := pressure.logicalBudget()
	if manager == nil {
		return nil, errors.New("Ledger projection disk ceiling requires the configured shared budget")
	}
	ram, err := accountinglimits.ProjectionMappingRAMBytes(cfg.ResourceBudget.LedgerProjectionDiskBytes)
	if err != nil {
		return nil, err
	}
	permit, err := manager.TryAcquireRecovery(resourcebudget.Cost{RAM: ram, Disk: cfg.ResourceBudget.LedgerProjectionDiskBytes})
	if err != nil {
		return nil, err
	}
	return &runtimeProjectionDiskPermit{permit: permit}, nil
}

// One aggregate Ledger index owner may cover a persistent Log locator, its
// pinned predecessor during publication, and up to three bounded NativeSource
// reader locators while they move to the new manifest. Keep one additional
// bounded share for the publisher's temporary file.
func acquireLedgerSegmentIndexDisk(ctx context.Context, cfg config.Config, pressure *resourcePressureRuntime) (*runtimeProjectionDiskPermit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.ResourceBudget == nil || cfg.ResourceBudget.LedgerSegmentIndexDiskBytes == 0 {
		return nil, nil
	}
	manager := pressure.logicalBudget()
	if manager == nil {
		return nil, errors.New("Ledger segment index disk ceiling requires the configured shared budget")
	}
	permit, _, err := acquireLedgerSegmentIndexDiskFromManager(cfg, manager)
	if err != nil {
		return nil, err
	}
	return permit, nil
}

func acquireLedgerSegmentIndexDiskFromManager(cfg config.Config, manager *resourcebudget.Manager) (*runtimeProjectionDiskPermit, int64, error) {
	if cfg.ResourceBudget == nil || cfg.ResourceBudget.LedgerSegmentIndexDiskBytes == 0 {
		return nil, 0, nil
	}
	if manager == nil {
		return nil, 0, errors.New("Ledger segment index disk ceiling requires the configured shared budget")
	}
	permit, err := manager.TryAcquireRecovery(resourcebudget.Cost{Disk: cfg.ResourceBudget.LedgerSegmentIndexDiskBytes})
	if err != nil {
		return nil, 0, err
	}
	return &runtimeProjectionDiskPermit{permit: permit}, ledgerSegmentIndexShareBytes(cfg), nil
}

func ledgerSegmentIndexShareBytes(cfg config.Config) int64 {
	if cfg.ResourceBudget == nil {
		return 0
	}
	limit := cfg.ResourceBudget.LedgerSegmentIndexDiskBytes
	if limit <= 0 {
		return 0
	}
	if cfg.Replication != nil {
		return limit / 6 // new and pinned index copies, three reader locators, and publish workspace
	}
	return limit
}

func acquireUsageProjectionDisk(ctx context.Context, cfg config.Config, pressure *resourcePressureRuntime) (*runtimeProjectionDiskPermit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.ResourceBudget == nil || cfg.ResourceBudget.UsageProjectionDiskBytes == 0 {
		return nil, nil
	}
	manager := pressure.logicalBudget()
	if manager == nil {
		return nil, errors.New("Usage projection disk ceiling requires the configured shared budget")
	}
	ram, err := accountinglimits.ProjectionMappingRAMBytes(cfg.ResourceBudget.UsageProjectionDiskBytes)
	if err != nil {
		return nil, err
	}
	permit, err := manager.TryAcquireRecovery(resourcebudget.Cost{RAM: ram, Disk: cfg.ResourceBudget.UsageProjectionDiskBytes})
	if err != nil {
		return nil, err
	}
	return &runtimeProjectionDiskPermit{permit: permit}, nil
}

// Usage archives survive Runtime.Close, so their full configured logical
// ceiling is held from inventory through the selected role's startup gate and
// retained in the shared Manager after the Runtime owner retires.
func acquireUsageArchiveDisk(ctx context.Context, cfg config.Config, pressure *resourcePressureRuntime) (*runtimeProjectionDiskPermit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.ResourceBudget == nil || cfg.ResourceBudget.UsageArchiveDiskBytes == 0 {
		return nil, nil
	}
	manager := pressure.logicalBudget()
	if manager == nil {
		return nil, errors.New("Usage archive disk ceiling requires the configured shared budget")
	}
	permit, err := manager.TryAcquireRecovery(resourcebudget.Cost{Disk: cfg.ResourceBudget.UsageArchiveDiskBytes})
	if err != nil {
		return nil, fmt.Errorf("reserve Usage archive Recovery Disk ceiling: %w", err)
	}
	return &runtimeProjectionDiskPermit{permit: permit}, nil
}

func usageArchiveDiskCeiling(cfg config.Config) int64 {
	if cfg.ResourceBudget == nil {
		return 0
	}
	return cfg.ResourceBudget.UsageArchiveDiskBytes
}

func acquireGovernanceProjectionDisk(ctx context.Context, cfg config.Config, pressure *resourcePressureRuntime) (*runtimeProjectionDiskPermit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.ResourceBudget == nil || cfg.ResourceBudget.GovernanceProjectionDiskBytes == 0 {
		return nil, nil
	}
	manager := pressure.logicalBudget()
	if manager == nil {
		return nil, errors.New("Governance projection disk ceiling requires the configured shared budget")
	}
	ram, err := accountinglimits.ProjectionMappingRAMBytes(cfg.ResourceBudget.GovernanceProjectionDiskBytes)
	if err != nil {
		return nil, err
	}
	permit, err := manager.TryAcquireRecovery(resourcebudget.Cost{RAM: ram, Disk: cfg.ResourceBudget.GovernanceProjectionDiskBytes})
	if err != nil {
		return nil, err
	}
	return &runtimeProjectionDiskPermit{permit: permit}, nil
}

// Governance's native journal is an authoritative persistent file. Charge its
// configured ceiling for the runtime lifetime; unlike fresh projections, Close
// does not delete it and therefore must not release the disk claim.
func acquireGovernanceLogDisk(ctx context.Context, cfg config.Config, pressure *resourcePressureRuntime) (*runtimeProjectionDiskPermit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.ResourceBudget == nil || cfg.ResourceBudget.GovernanceLogDiskBytes == 0 {
		return nil, nil
	}
	manager := pressure.logicalBudget()
	if manager == nil {
		return nil, errors.New("Governance journal disk ceiling requires the configured shared budget")
	}
	permit, err := manager.TryAcquireRecovery(resourcebudget.Cost{Disk: cfg.ResourceBudget.GovernanceLogDiskBytes})
	if err != nil {
		return nil, err
	}
	return &runtimeProjectionDiskPermit{permit: permit}, nil
}

func acquireAuditLogDisk(ctx context.Context, cfg config.Config, pressure *resourcePressureRuntime) (*runtimeProjectionDiskPermit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.ResourceBudget == nil || cfg.ResourceBudget.AuditLogDiskBytes == 0 {
		return nil, nil
	}
	manager := pressure.logicalBudget()
	if manager == nil {
		return nil, errors.New("Audit journal disk ceiling requires the configured shared budget")
	}
	permit, err := manager.TryAcquireRecovery(resourcebudget.Cost{Disk: cfg.ResourceBudget.AuditLogDiskBytes})
	if err != nil {
		return nil, err
	}
	return &runtimeProjectionDiskPermit{permit: permit}, nil
}

func acquireMetadataJournalDisk(ctx context.Context, cfg config.Config, pressure *resourcePressureRuntime) (*runtimeProjectionDiskPermit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.ResourceBudget == nil || cfg.ResourceBudget.MetadataJournalDiskBytes == 0 {
		return nil, nil
	}
	manager := pressure.logicalBudget()
	if manager == nil {
		return nil, errors.New("Metadata journal disk ceiling requires the configured shared budget")
	}
	permit, err := manager.TryAcquireRecovery(resourcebudget.Cost{Disk: cfg.ResourceBudget.MetadataJournalDiskBytes})
	if err != nil {
		return nil, err
	}
	return &runtimeProjectionDiskPermit{permit: permit}, nil
}

func acquireOrderingJournalDisk(ctx context.Context, cfg config.Config, pressure *resourcePressureRuntime) (*runtimeProjectionDiskPermit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.Replication == nil || cfg.ResourceBudget == nil || cfg.ResourceBudget.OrderingJournalDiskBytes == 0 {
		return nil, nil
	}
	manager := pressure.logicalBudget()
	if manager == nil {
		return nil, errors.New("Ordering journal disk ceiling requires the configured shared budget")
	}
	permit, err := manager.TryAcquireRecovery(resourcebudget.Cost{Disk: cfg.ResourceBudget.OrderingJournalDiskBytes})
	if err != nil {
		return nil, err
	}
	return &runtimeProjectionDiskPermit{permit: permit}, nil
}

// Backup's operation-local SourceCatalog grows with the number of source files.
// Keep its bbolt file under a Recovery claim only for the catalog lifetime.
func acquireBackupWorkspaceDisk(ctx context.Context, cfg config.Config, pressure *resourcePressureRuntime) (*runtimeProjectionDiskPermit, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if cfg.ResourceBudget == nil || cfg.ResourceBudget.BackupWorkspaceDiskBytes == 0 {
		return nil, 0, nil
	}
	if pressure == nil || pressure.logicalBudget() == nil {
		return nil, 0, errors.New("backup workspace disk ceiling requires the configured shared budget")
	}
	limit := backupCatalogDiskCeiling(cfg)
	ram, err := accountinglimits.ProjectionMappingRAMBytes(limit)
	if err != nil {
		return nil, 0, err
	}
	permit, err := pressure.logicalBudget().TryAcquireRecovery(resourcebudget.Cost{RAM: ram, Disk: limit})
	if err != nil {
		return nil, 0, fmt.Errorf("reserve backup catalog Recovery RAM/Disk ceiling: %w", err)
	}
	return &runtimeProjectionDiskPermit{permit: permit}, limit, nil
}

// Restored payload bytes are known only after the complete encrypted archive
// has been authenticated. Charge the exact verified output size before staging.
func acquireRestoreWorkspaceDisk(ctx context.Context, cfg config.Config, pressure *resourcePressureRuntime, payloadBytes int64) (*runtimeProjectionDiskPermit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if payloadBytes < 0 {
		return nil, errors.New("verified restore payload size is invalid")
	}
	if cfg.ResourceBudget == nil || cfg.ResourceBudget.RestoreWorkspaceDiskBytes == 0 {
		return nil, nil
	}
	if payloadBytes > cfg.ResourceBudget.RestoreWorkspaceDiskBytes {
		return nil, fmt.Errorf("verified restore payload %d exceeds restore_workspace_disk_bytes %d", payloadBytes, cfg.ResourceBudget.RestoreWorkspaceDiskBytes)
	}
	if payloadBytes == 0 {
		return nil, nil
	}
	if pressure == nil || pressure.logicalBudget() == nil {
		return nil, errors.New("restore workspace disk ceiling requires the configured shared budget")
	}
	permit, err := pressure.logicalBudget().TryAcquireRecovery(resourcebudget.Cost{Disk: payloadBytes})
	if err != nil {
		return nil, fmt.Errorf("reserve restore workspace Recovery Disk bytes: %w", err)
	}
	return &runtimeProjectionDiskPermit{permit: permit}, nil
}

func usageDiskCapacityLeaseError(err error) error {
	if errors.Is(err, usage.ErrDiskUsageDiskLimit) {
		return errors.Join(ledger.ErrProjectionDiskLimit, err)
	}
	return err
}
