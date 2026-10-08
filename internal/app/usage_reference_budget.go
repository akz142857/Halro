package app

import (
	"context"
	"errors"
	"math"

	"github.com/akz142857/Halro/internal/accountinglimits"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// One private reference job owns both DB ceilings and its reducer workspace.
// Conservatively charge the full mmap ceilings as RAM for their whole lifetime;
// this is logical admission, not a physical RSS or filesystem reservation.
type usageReferenceBudget struct {
	ledgerBytes, usageBytes int64
	permit                  resourcebudget.Permit
}

func acquireUsageReferenceBudget(ctx context.Context, cfg config.Config, pressure *resourcePressureRuntime, class resourcebudget.Class) (usageReferenceBudget, error) {
	var out usageReferenceBudget
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if cfg.ResourceBudget == nil {
		if pressure.logicalBudget() != nil {
			return out, errors.New("Usage reference manager has no resource configuration")
		}
		return out, nil
	}
	b := cfg.ResourceBudget
	if pressure.logicalBudget() == nil || b.LedgerProjectionDiskBytes <= 0 || b.UsageProjectionDiskBytes <= 0 || b.AccountingRecoveryRAMBytes < accountinglimits.MinRecoveryRAMBytes {
		return out, errors.New("Usage reference requires shared workspace and positive Ledger/Usage disk ceilings")
	}
	ram, err := accountinglimits.ProjectionMappingRAMBytes(b.LedgerProjectionDiskBytes, b.UsageProjectionDiskBytes)
	if err != nil || b.LedgerProjectionDiskBytes > math.MaxInt64-b.UsageProjectionDiskBytes || b.AccountingRecoveryRAMBytes > math.MaxInt64-ram {
		return out, resourcebudget.ErrInvalid
	}
	cost := resourcebudget.Cost{RAM: b.AccountingRecoveryRAMBytes + ram, Disk: b.LedgerProjectionDiskBytes + b.UsageProjectionDiskBytes}
	if class == resourcebudget.Recovery {
		out.permit, err = pressure.logicalBudget().TryAcquireRecovery(cost)
	} else if class == resourcebudget.IO {
		out.permit, err = pressure.logicalBudget().TryAcquire(class, cost)
	} else {
		return out, resourcebudget.ErrInvalid
	}
	if err != nil {
		return usageReferenceBudget{}, err
	}
	out.ledgerBytes, out.usageBytes = b.LedgerProjectionDiskBytes, b.UsageProjectionDiskBytes
	return out, nil
}

// Failed physical Close or owned removal retains the entire job charge.
func (b *usageReferenceBudget) retire(cleanupErr error) error {
	if cleanupErr != nil || b.permit == (resourcebudget.Permit{}) {
		return cleanupErr
	}
	err := b.permit.Release()
	if err == nil {
		b.permit = resourcebudget.Permit{}
	}
	return err
}
