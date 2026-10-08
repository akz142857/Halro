package app

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

const ledgerMaintenanceRAMBytes int64 = 32 << 20
const legacySealReportDiskBytes int64 = 8 << 20

var errLedgerMaintenanceRetirement = errors.New("ledger maintenance physical retirement is uncertain")

type ledgerMaintenanceContextKey struct{}
type ledgerMaintenanceScope struct {
	root  *commandResourceScope
	spool bool
	limit int64
}

type ledgerMaintenanceOwner struct {
	manager  *resourcebudget.Manager
	budgeted bool
}

func newLedgerMaintenanceOwner(cfg config.Config, pressure *resourcePressureRuntime) *ledgerMaintenanceOwner {
	return &ledgerMaintenanceOwner{manager: pressure.logicalBudget(), budgeted: cfg.ResourceBudget != nil}
}

func (r *Runtime) withLedgerMaintenance(ctx context.Context, work func(context.Context) error) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	var owner *ledgerMaintenanceOwner
	if r.maintenanceOwners != nil {
		owner = r.maintenanceOwners.ledger
	}
	if owner == nil {
		owner = newLedgerMaintenanceOwner(r.config, r.resourcePressure)
	}
	var permit resourcebudget.Permit
	if owner.budgeted {
		if owner.manager == nil {
			return resourcebudget.ErrInvalid
		}
		var err error
		permit, err = owner.manager.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: ledgerMaintenanceRAMBytes})
		if err != nil {
			return err
		}
	}
	defer func() {
		if !errors.Is(resultErr, ledger.ErrSegmentRetirement) && !errors.Is(resultErr, ownedfile.ErrRetirement) && permit != (resourcebudget.Permit{}) {
			resultErr = errors.Join(resultErr, permit.Release())
		}
		resultErr = errors.Join(resultErr, ctx.Err())
	}()
	return work(ctx)
}

// The complete command (including actual response writes) owns one workspace.
// Nested sealing borrows this original scope; it cannot create another pool.
// Report bytes use the configured backup workspace ceiling, or a finite legacy
// 8MiB ceiling. This derivative fee does not cover native historical inventory.
func ledgerMaintenanceContext(ctx context.Context, cfg config.Config, spool bool) (context.Context, int64, func(bool) error, error) {
	ctx, pressure, err := commandBudgetContext(ctx, cfg)
	if err != nil {
		return ctx, 0, nil, err
	}
	root, _ := ctx.Value(commandResourceScopeKey{}).(*commandResourceScope)
	if previous, ok := ctx.Value(ledgerMaintenanceContextKey{}).(*ledgerMaintenanceScope); ok {
		if previous.root != root || spool {
			return ctx, 0, nil, resourcebudget.ErrInvalid
		}
		return ctx, previous.limit, func(bool) error { return nil }, nil
	}
	limit := int64(0)
	if spool {
		limit = legacySealReportDiskBytes
		if cfg.ResourceBudget != nil && cfg.ResourceBudget.BackupWorkspaceDiskBytes > 0 {
			limit = cfg.ResourceBudget.BackupWorkspaceDiskBytes
		}
	}
	var permit resourcebudget.Permit
	if cfg.ResourceBudget != nil {
		if pressure.logicalBudget() == nil || limit < 0 {
			return ctx, 0, nil, resourcebudget.ErrInvalid
		}
		permit, err = pressure.logicalBudget().TryAcquireRecovery(resourcebudget.Cost{RAM: ledgerMaintenanceRAMBytes, Disk: limit})
		if err != nil {
			return ctx, 0, nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		if permit != (resourcebudget.Permit{}) {
			err = errors.Join(err, permit.Release())
		}
		return ctx, 0, nil, err
	}
	ctx = context.WithValue(ctx, ledgerMaintenanceContextKey{}, &ledgerMaintenanceScope{root: root, spool: spool, limit: limit})
	return ctx, limit, func(retired bool) error {
		if retired && permit != (resourcebudget.Permit{}) {
			return permit.Release()
		}
		return nil
	}, nil
}

type sealReportBoundedWriter struct {
	writer    interface{ Write([]byte) (int, error) }
	remaining int64
}

func (w *sealReportBoundedWriter) Write(raw []byte) (int, error) {
	if int64(len(raw)) > w.remaining {
		return 0, errors.New("seal report exceeds admitted file ceiling")
	}
	n, err := w.writer.Write(raw)
	w.remaining -= int64(n)
	return n, err
}
