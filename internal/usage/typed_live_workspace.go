package usage

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replicationlimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// Every simultaneous source, temporary actual FD, verified page read and COW
// update has a separate finite slot. The three small IO loans cover frozen-root
// ownership, its nested structural reference, and that reference's actual root
// receipt. Actual source/Usage pins overlap and therefore have two separate
// Background slots. No slot can be borrowed concurrently or after its job exits.
func typedLiveWorkspaceRequirements() []resourcebudget.WorkspaceRequirement {
	return []resourcebudget.WorkspaceRequirement{
		{Class: resourcebudget.IO, RAM: ledger.AuthenticatedLedgerSourceRAMBytes},
		{Class: resourcebudget.IO, RAM: authenticatedstate.ReadRAMBytes},
		{Class: resourcebudget.IO, RAM: 4096},
		{Class: resourcebudget.IO, RAM: 4096},
		{Class: resourcebudget.IO, RAM: 4096},
		{Class: resourcebudget.Background, RAM: authenticatedstate.UpdateRAMBytes},
		{Class: resourcebudget.Background, RAM: 4096},
		{Class: resourcebudget.Background, RAM: 4096},
	}
}
func (d *LedgerDerivative) catchUpTypedSource(ctx context.Context, native *ledger.Log, manager *resourcebudget.Manager, lease *ledger.MaintenanceLease) (AuthenticatedUsageAdvance, error) {
	return d.catchUpTypedSourceWithPhase(ctx, native, manager, lease, nil)
}
func (d *LedgerDerivative) catchUpTypedSourceWithPhase(ctx context.Context, native *ledger.Log, manager *resourcebudget.Manager, lease *ledger.MaintenanceLease, phase func(string)) (receipt AuthenticatedUsageAdvance, resultErr error) {
	return d.catchUpTypedSourceMode(ctx, native, manager, lease, phase, false)
}

// CatchUpHASelectedStartup authenticates and advances a selected HA derivative
// while its original actual startup lease remains installed. Activating its
// query lane does not release that lease or authorize ordinary live collection.
// The real maintenance source rechecks the constructor, role and allowed cut.
func (d *LedgerDerivative) CatchUpHASelectedStartup(ctx context.Context, native *ledger.Log, lease *ledger.MaintenanceLease, manager *resourcebudget.Manager) (AuthenticatedUsageAdvance, error) {
	if lease == nil {
		return AuthenticatedUsageAdvance{}, authenticatedstate.ErrRecovery
	}
	if replicated, err := lease.ValidateOwner(native); err != nil || !replicated {
		return AuthenticatedUsageAdvance{}, errors.Join(authenticatedstate.ErrRecovery, err)
	}
	return d.catchUpTypedSourceMode(ctx, native, manager, lease, nil, true)
}

func (d *LedgerDerivative) catchUpTypedSourceMode(ctx context.Context, native *ledger.Log, manager *resourcebudget.Manager, lease *ledger.MaintenanceLease, phase func(string), haStartup bool) (receipt AuthenticatedUsageAdvance, resultErr error) {
	if d == nil || native == nil || manager == nil {
		return receipt, authenticatedstate.ErrRecovery
	}
	d.mu.Lock()
	var physical *typedPhysicalUsageOwner
	var projection *DiskProjection
	if d.disk != nil {
		projection = d.disk
		d.disk.mu.RLock()
		physical = d.disk.physical
		d.disk.mu.RUnlock()
	}
	d.mu.Unlock()
	if physical != nil {
		opCtx, finish, err := projection.beginPhysicalUsageJob(ctx)
		if err != nil {
			return receipt, err
		}
		defer finish()
		ctx = opCtx
		if physical.native != native || physical.manager != manager || physical.work == nil || haStartup && !physical.ha {
			return receipt, authenticatedstate.ErrRecovery
		}
		resultErr = resourcebudget.WithCompletionWorkspace(physical.work.Bind(ctx), func(ctx context.Context) error {
			work, err := physical.work.Active.RetainCovering(manager, resourcebudget.Cost{RAM: usageSealWorkspaceRAMBytes})
			if err != nil {
				return err
			}
			receipt, err = d.catchUpTypedPreparedSource(ctx, native, manager, lease, work, phase, haStartup)
			return err
		})
		if resultErr != nil {
			receipt = AuthenticatedUsageAdvance{}
		}
		return receipt, resultErr
	}
	// This distinct work allocation preserves the existing prepared-evidence RAM
	// fee; it cannot consume the page-reader slot that must remain reusable.
	work, err := manager.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: usageSealWorkspaceRAMBytes})
	if err != nil {
		return receipt, err
	}
	entered := false
	requirements := typedLiveWorkspaceRequirements()
	d.mu.Lock()
	ha := d.disk != nil && d.disk.engine != nil && d.disk.engine.selectedHA
	d.mu.Unlock()
	if ha {
		// The three small ordinary-source loans finish before the authority
		// readers begin. Their fixed slots can then be reused for the larger
		// Member and Ordering readers without increasing the eight-owner cap.
		requirements[3].RAM = replicationlimits.MemberAuthorityRAMBytes
		requirements[4].RAM = replicationlimits.OrderingFrozenReadRAMBytes
	}
	resultErr = manager.WithRAMWorkspace(ctx, requirements, func(ctx context.Context) error {
		entered = true
		var err error
		receipt, err = d.catchUpTypedPreparedSource(ctx, native, manager, lease, work, phase, haStartup)
		return err
	})
	if !entered {
		resultErr = releaseTypedLiveWork(work, resultErr)
	}
	if resultErr != nil {
		receipt = AuthenticatedUsageAdvance{}
	}
	return receipt, resultErr
}

// A failed logical release retains the actual opaque permit; it cannot be
// reconstructed from a cost estimate or erased by a repeated unrelated Close.
type typedLiveWorkRetirement struct {
	Cause error
	Work  resourcebudget.Permit
}

func (e *typedLiveWorkRetirement) Error() string { return e.Cause.Error() }
func (e *typedLiveWorkRetirement) Unwrap() error { return e.Cause }
func releaseTypedLiveWork(work resourcebudget.Permit, cause error) error {
	if err := work.Release(); err != nil {
		return &typedLiveWorkRetirement{errors.Join(cause, ownedfile.ErrRetirement, err), work}
	}
	return cause
}
