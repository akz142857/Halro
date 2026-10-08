package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// The actual Primary retains all three independent completion owners before
// upstream admission: confirmed Ordering, Metadata native/COW and Ledger
// native/COW/Directory/Usage. A failed native retirement cannot forgive the
// companion price pin or release its physical recovery ownership.
func (f *haSelectedRuntimeFoundation) physicalTerminalAdmission(ctx context.Context, count int) (context.Context, func() error, error) {
	if f == nil || f.reopened == nil || f.replication == nil || f.replication.role != replication.RolePrimary || f.cfg.ResourceBudget == nil || !f.cfg.ResourceBudget.PhysicalTerminalReserve {
		return ctx, nil, authenticatedstate.ErrSealIneligible
	}
	if count < 2 || count > resourcebudget.MaxCompletionBatch {
		return ctx, nil, resourcebudget.ErrInvalid
	}
	c := f.reopened.Owners.Components
	f.mu.Lock()
	if err := c.Publisher.ValidatePhysicalSealPreparation(ctx, f.reopened.Seal); err != nil {
		f.mu.Unlock()
		if ctx.Err() == nil {
			c.NativeLedger.Status().RequireRecovery()
		}
		return ctx, nil, errors.Join(authenticatedstate.ErrRecovery, err)
	}
	if err := f.prepareHAPhysicalCacheTargets(ctx, f.reopened.Seal); err != nil {
		f.mu.Unlock()
		return ctx, nil, err
	}
	f.mu.Unlock()
	confirmed, closeConfirmed, err := f.replication.reserveTerminalRequest(ctx, count)
	if err != nil {
		return ctx, nil, fmt.Errorf("HA original confirmed terminal window: %w", err)
	}
	metadata, err := c.Metadata.ReservePhysicalTerminals(confirmed, count-1)
	if err != nil {
		return ctx, nil, errors.Join(fmt.Errorf("HA original Metadata terminal window: %w", err), closeConfirmed())
	}
	native, err := c.NativeLedger.ReservePhysicalTerminals(confirmed, count)
	if err != nil {
		return ctx, nil, errors.Join(fmt.Errorf("HA original Ledger terminal window: %w", err), metadata.Close(), closeConfirmed())
	}
	return native.Bind(metadata.Bind(confirmed)), func() error {
		nativeErr := native.Close()
		if nativeErr != nil {
			return errors.Join(nativeErr, metadata.RetainForRecovery(nativeErr), closeConfirmed())
		}
		return errors.Join(metadata.Close(), closeConfirmed())
	}, nil
}
