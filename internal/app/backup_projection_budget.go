package app

import (
	"context"
	"errors"
	"math"
	"os"

	"github.com/akz142857/Halro/internal/accountinglimits"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

type backupProjectionKind uint8

const (
	backupLedgerProjection backupProjectionKind = iota
	backupGovernanceProjection
)

type backupProjectionBudget struct {
	limit  int64
	permit resourcebudget.Permit
}

// One unpublished, authenticated reference owns workspace and the complete DB
// mapping/data-file ceilings before its directory or native reader is opened.
// No online admission can call this helper to borrow a Recovery credit.
func acquireBackupProjectionBudget(ctx context.Context, cfg config.Config, kind backupProjectionKind) (backupProjectionBudget, error) {
	if err := ctx.Err(); err != nil {
		return backupProjectionBudget{}, err
	}
	if cfg.ResourceBudget == nil {
		return backupProjectionBudget{}, nil
	}
	pressure, err := commandPressure(ctx, cfg, nil)
	if err != nil {
		return backupProjectionBudget{}, err
	}
	if pressure.logicalBudget() == nil {
		return backupProjectionBudget{}, errors.New("backup reference lost its root shared manager")
	}
	limit, workspace := int64(0), int64(0)
	switch kind {
	case backupLedgerProjection:
		limit, workspace = cfg.ResourceBudget.LedgerProjectionDiskBytes, cfg.ResourceBudget.AccountingRecoveryRAMBytes
		if workspace < accountinglimits.MinRecoveryRAMBytes {
			return backupProjectionBudget{}, resourcebudget.ErrInvalid
		}
	case backupGovernanceProjection:
		limit, workspace = cfg.ResourceBudget.GovernanceProjectionDiskBytes, 32<<20
	default:
		return backupProjectionBudget{}, resourcebudget.ErrInvalid
	}
	if limit <= 0 || limit > math.MaxInt64-workspace {
		return backupProjectionBudget{}, errors.New("budgeted backup reference requires a positive non-overflowing projection file ceiling")
	}
	ram, err := accountinglimits.ProjectionMappingRAMBytes(limit)
	if err != nil || ram > math.MaxInt64-workspace {
		return backupProjectionBudget{}, resourcebudget.ErrInvalid
	}
	permit, err := pressure.logicalBudget().TryAcquireRecovery(resourcebudget.Cost{RAM: ram + workspace, Disk: limit})
	if err != nil {
		return backupProjectionBudget{}, err
	}
	return backupProjectionBudget{limit: limit, permit: permit}, nil
}

func (b *backupProjectionBudget) retire(cleanupErr error) error {
	if cleanupErr != nil || b.permit == (resourcebudget.Permit{}) {
		return cleanupErr
	}
	err := b.permit.Release()
	if err == nil {
		b.permit = resourcebudget.Permit{}
	}
	return err
}

// Empty names or successfully retired independent scratch files cannot repay
// another actual constructor's uncertain FD/engine lifecycle.
func withRetirementDebt(cleanupErr, cause error) error {
	if errors.Is(cause, ownedfile.ErrRetirement) {
		return errors.Join(cleanupErr, cause)
	}
	return cleanupErr
}

// Never recursively unlink a private directory that acquired an unknown entry
// or whose name now refers to another inode. Retain its entire job fee on error.
func removeOwnedReferenceDirectory(path string, expected os.FileInfo) error {
	current, err := os.Lstat(path)
	if err != nil || expected == nil || !current.IsDir() || !os.SameFile(expected, current) {
		return errors.Join(err, errors.New("private reference directory identity changed before retirement"))
	}
	return os.Remove(path)
}
