package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/nativephysical"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
)

// This startup-only handoff follows the actual joined historical audits and
// precedes every derived factory, business service and transport publication.
// It prepares native births; it cannot grant a received tail or HA readiness.
func (f *haSelectedRuntimeFoundation) prepareHAPhysicalNativeSources(ctx context.Context) error {
	if f == nil || f.cfg.ResourceBudget == nil {
		return authenticatedstate.ErrSealIneligible
	}
	if !f.cfg.ResourceBudget.PhysicalTerminalReserve {
		return nil
	}
	c, r := f.owners.Components, f.replication
	if r == nil || r.publisher == nil || r.journal == nil || c.Lease == nil || c.Audit == nil || c.NativeGovernance == nil {
		return authenticatedstate.ErrSealIneligible
	}
	for _, auditor := range f.audits {
		if auditor == nil || !auditor.Snapshot().Ready {
			return audit.ErrStartupAuditPending
		}
	}
	// Independently authenticate the current common cut before any new inode
	// changes. The subsequent native APIs re-enter their original source scopes.
	if err := f.withSelectedCut(ctx, func(ctx context.Context, _ statecut.HASelectedCut) error { return ctx.Err() }); err != nil {
		return err
	}
	acquire := func(bytes int64) (resourcebudget.Permit, error) {
		fee, err := c.Budget.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: bytes})
		if err == nil {
			f.fees = append(f.fees, fee)
		}
		return fee, err
	}
	// Member/Ordering physical births are restored earlier, before the actual
	// Primary constructor's first progress publication can exchange either bank.
	if err := f.prepareHAPhysicalMemberOrdering(ctx); err != nil {
		return err
	}
	ac, err := acquire(nativephysical.ControlDiskBytes)
	if err != nil {
		return err
	}
	if err = c.Audit.PrepareHAPhysicalNative(ctx, c.Budget, audit.HAPhysicalOptions{NativeDisk: f.auditNativeDisk, ControlDisk: ac, WindowBytes: 16 << 20, Restart: true}); err != nil {
		return fmt.Errorf("HA physical Audit preparation: %w", err)
	}
	gc, err := acquire(nativephysical.ControlDiskBytes)
	if err != nil {
		return err
	}
	if err = c.NativeGovernance.PrepareHAPhysicalNative(ctx, c.Budget, governance.HAPhysicalOptions{NativeDisk: f.governanceNativeDisk, ControlDisk: gc, WindowBytes: 16 << 20, Restart: true}); err != nil {
		return fmt.Errorf("HA physical Governance preparation: %w", err)
	}
	return errors.Join(ctx.Err(), f.ctx.Err())
}
