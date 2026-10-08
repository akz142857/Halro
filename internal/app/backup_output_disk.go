package app

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// A distinct physical archive output never borrows the private staging tree's
// capacity. Both full ceilings and all index/report files share one Manager.
type backupOutputDiskOwner struct {
	quota       *runtimeLedgerAppendDiskOwner
	workRetired bool
}

type backupOutputDiskFailure struct {
	owner *backupOutputDiskOwner
	cause error
}

func (e *backupOutputDiskFailure) Error() string { return e.cause.Error() }
func (e *backupOutputDiskFailure) Unwrap() error { return e.cause }

func prepareBackupOutputDisk(ctx context.Context, cfg config.Config, pressure *resourcePressureRuntime) (*backupOutputDiskOwner, error) {
	if cfg.ResourceBudget == nil {
		return nil, nil
	}
	shared, err := commandPressure(ctx, cfg, pressure)
	if err != nil {
		return nil, err
	}
	scope, ok := ctx.Value(commandResourceScopeKey{}).(*commandResourceScope)
	limit := cfg.ResourceBudget.BackupWorkspaceDiskBytes
	if !ok || shared == nil || shared.logicalBudget() == nil || limit <= 0 || limit > 1<<50 {
		return nil, resourcebudget.ErrInvalid
	}
	scope.outputMu.Lock()
	defer scope.outputMu.Unlock()
	if scope.backupOutput != nil {
		return scope.backupOutput, nil
	}
	credit, err := shared.logicalBudget().TryAcquireRecovery(resourcebudget.Cost{RAM: backup.OutputWorkspaceRAMBytes, Disk: limit})
	if err != nil {
		return nil, err
	}
	owner := &backupOutputDiskOwner{quota: &runtimeLedgerAppendDiskOwner{credit: credit, ceiling: limit}}
	scope.backupOutput = owner
	return owner, nil
}

func (o *backupOutputDiskOwner) finish(cause error) error {
	if o == nil {
		return cause
	}
	o.quota.mu.Lock()
	defer o.quota.mu.Unlock()
	o.quota.closed = true
	if !o.workRetired && !errors.Is(cause, ownedfile.ErrRetirement) {
		retireErr := o.quota.credit.ReleasePart(resourcebudget.Cost{RAM: backup.OutputWorkspaceRAMBytes})
		cause = errors.Join(cause, retireErr)
		if retireErr == nil {
			o.workRetired = true
		}
	}
	// File publication or Close does not physically retire the archive inode.
	// This entire Disk/claim survives, including conservative unused capacity.
	if cause != nil {
		return &backupOutputDiskFailure{owner: o, cause: cause}
	}
	return nil
}
