package ledger

import (
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/replicationlimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// ResourceManager returns this component's immutable shared accounting owner.
// It grants no read, write, role, recovery or publication permission.
func (p *AuthenticatedLedgerProjection) ResourceManager(ctx context.Context) (*resourcebudget.Manager, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p == nil || p.budget == nil {
		return nil, ErrProjectionUnavailable
	}
	return p.budget, nil
}

// WithNativeApplyWorkspace reserves the structural source/COW/read profile
// before a caller accepts one native fact. Native mutation, maintenance/role
// permission, disk growth and confirmation remain independent requirements.
func (p *AuthenticatedLedgerProjection) WithNativeApplyWorkspace(ctx context.Context, visit func(context.Context) error) error {
	manager, err := p.ResourceManager(ctx)
	if err != nil {
		return err
	}
	requirements := NativeApplyWorkspaceRequirements()
	if native := p.selectedNative.Load(); native != nil && native.startupProvenance != nil && native.startupProvenance.ha != nil {
		// The actual HA durable position read overlaps its ordinary-read lane,
		// authenticated Member file, Ordering prefix and physical root pin.
		// Reserve those real owners before native acceptance; a five-slot
		// standalone profile cannot safely fund this eight-slot operation.
		requirements = append(requirements,
			resourcebudget.WorkspaceRequirement{Class: resourcebudget.IO, RAM: replicationlimits.MemberAuthorityRAMBytes},
			resourcebudget.WorkspaceRequirement{Class: resourcebudget.IO, RAM: replicationlimits.OrderingFrozenReadRAMBytes},
			resourcebudget.WorkspaceRequirement{Class: resourcebudget.IO, RAM: 4096})
	}
	return manager.WithRAMWorkspace(ctx, requirements, visit)
}

func NativeApplyWorkspaceRequirements() []resourcebudget.WorkspaceRequirement {
	return []resourcebudget.WorkspaceRequirement{
		{Class: resourcebudget.IO, RAM: AuthenticatedLedgerSourceRAMBytes},
		{Class: resourcebudget.IO, RAM: AuthenticatedLedgerReadRAMBytes},
		{Class: resourcebudget.IO, RAM: authenticatedstate.ReadRAMBytes},
		{Class: resourcebudget.IO, RAM: 4096},
		{Class: resourcebudget.Background, RAM: authenticatedstate.UpdateRAMBytes},
	}
}

// CompletionWorkspaceRequirements includes the actual serialized append queue
// ticket, which overlaps the five source/COW slots during terminal Apply.
func CompletionWorkspaceRequirements() []resourcebudget.WorkspaceRequirement {
	return append(NativeApplyWorkspaceRequirements(), resourcebudget.WorkspaceRequirement{Class: resourcebudget.IO, RAM: 4096})
}

// The actual selected HA terminal job also overlaps its Member/Ordering/root
// permission readers. The queue ticket remains a separate simultaneous owner.
func HASelectedCompletionWorkspaceRequirements() []resourcebudget.WorkspaceRequirement {
	return append(CompletionWorkspaceRequirements(),
		resourcebudget.WorkspaceRequirement{Class: resourcebudget.IO, RAM: replicationlimits.MemberAuthorityRAMBytes},
		resourcebudget.WorkspaceRequirement{Class: resourcebudget.IO, RAM: replicationlimits.OrderingFrozenReadRAMBytes},
		resourcebudget.WorkspaceRequirement{Class: resourcebudget.IO, RAM: 4096})
}

// ValidateResourceOwner refuses an isolated component pool where a caller
// requires a specific shared Manager. This does not establish physical/disk
// reconciliation, Recovery class eligibility or permission to reopen a root.
func (p *AuthenticatedLedgerProjection) ValidateResourceOwner(ctx context.Context, manager *resourcebudget.Manager) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil || manager == nil || p.budget != manager {
		return errors.New("authenticated Ledger requires the same shared resource owner")
	}
	if !p.mu.TryRLock() {
		return authenticatedstate.ErrBusy
	}
	defer p.mu.RUnlock()
	if !p.ready || p.closeContext.Err() != nil || p.readFailure.Load() != nil {
		return errors.Join(ErrProjectionUnavailable, p.failure, p.readFault())
	}
	return nil
}

// ValidateNativeWriter checks the original source path/key and the complete
// applied position/hash while this exact Log is exclusively in maintenance.
// Equal positions on a different native source do not bind an owner. The caller
// must drain/Retire old business owners and establish role/generation permission;
// this check neither publishes a Runtime nor authorizes saved-root recovery.
func (p *AuthenticatedLedgerProjection) ValidateNativeWriter(ctx context.Context, native *Log, lease *MaintenanceLease) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil || native == nil {
		return ErrProjectionUnavailable
	}
	if _, err := lease.ValidateOwner(native); err != nil {
		return err
	}
	verify := func(ctx context.Context, head Watermark, hash [32]byte) error {
		if err := p.checkNativeWriter(native); err != nil {
			return err
		}
		actual, actualHash, err := p.ReadPosition(ctx)
		if err != nil {
			return err
		}
		if actual != head || actualHash != hash {
			return errors.New("authenticated accounting projection differs from exclusive native position/hash")
		}
		return nil
	}
	// The checks only try their locks. A writer or reader holding one for a
	// moment is not a verdict, so wait it out within a bound.
	deadline := time.Now().Add(validateNativeWriterBusyWait)
	for delay := time.Millisecond; ; delay = min(2*delay, 50*time.Millisecond) {
		err := lease.WithHead(ctx, verify)
		if !onlyBusy(err) || !time.Now().Before(deadline) {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(err, ctx.Err())
		case <-timer.C:
		}
	}
}

// validateNativeWriterBusyWait bounds how long writer validation waits out
// momentarily held locks, as every other admission wait stage is bounded.
const validateNativeWriterBusyWait = 30 * time.Second

// ValidateFreshNativeWriter keeps full-build accounting construction separate
// from selected startup, whose finite native grant must cover every business
// operation and its final ACK rather than only projection point reads.
func (p *AuthenticatedLedgerProjection) ValidateFreshNativeWriter(ctx context.Context, native *Log, lease *MaintenanceLease) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil || native == nil || native.startupProvenance != nil || p.selectedNative.Load() != nil {
		return ErrProjectionSource
	}
	return p.ValidateNativeWriter(ctx, native, lease)
}

// ValidateSelectedNativeWriter binds a business accounting owner to the actual
// selected native opener, its shared budget and a still-live maintenance lease.
// A fresh full rebuild or a matching caller-supplied cut cannot obtain this
// qualification. Ordinary finite history readiness is sufficient; this method
// grants no structural publication permission.
func (p *AuthenticatedLedgerProjection) ValidateSelectedNativeWriter(ctx context.Context, native *Log, lease *MaintenanceLease, manager *resourcebudget.Manager) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil || native == nil || manager == nil || p.selectedNative.Load() != native || native.startupProvenance == nil || native.startupProvenance.ha != nil || native.startupBudget != manager {
		return ErrProjectionSource
	}
	if err := p.ValidateResourceOwner(ctx, manager); err != nil {
		return err
	}
	if err := native.ValidateSelectedStartupRead(ctx); err != nil {
		return err
	}
	if err := p.ValidateNativeWriter(ctx, native, lease); err != nil {
		return err
	}
	return native.ValidateSelectedStartupRead(ctx)
}

// ValidateNativeSource binds a maintenance reader to this exact native path,
// chain key and shared lifecycle without requiring its permitted projection to
// equal the durable tail. It establishes no role or permission for that tail.
func (p *AuthenticatedLedgerProjection) ValidateNativeSource(ctx context.Context, native *Log, lease *MaintenanceLease) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil || native == nil {
		return ErrProjectionUnavailable
	}
	if _, err := lease.ValidateOwner(native); err != nil {
		return err
	}
	return p.checkNativeWriter(native)
}

func (p *AuthenticatedLedgerProjection) checkNativeWriter(native *Log) error {
	if !p.mu.TryRLock() {
		return authenticatedstate.ErrBusy
	}
	defer p.mu.RUnlock()
	if !p.ready || p.readFailure.Load() != nil || p.closeContext.Err() != nil {
		return errors.Join(ErrProjectionUnavailable, p.failure, p.readFault())
	}
	if !native.mu.TryLock() {
		return authenticatedstate.ErrBusy
	}
	defer native.mu.Unlock()
	return p.checkNativeWriterLocked(native)
}

// Caller holds both the projection owner and actual native writer locks.
func (p *AuthenticatedLedgerProjection) checkNativeWriterLocked(native *Log) error {
	path, err := filepath.Abs(native.path)
	if err != nil || path != p.nativePath || sha256.Sum256(native.chainKey) != p.nativeKeyTag {
		return errors.Join(ErrProjectionSource, err)
	}
	if native.file == nil || len(native.chainKey) != 32 || native.status.Load() != AccountingHealthy {
		return ErrProjectionUnavailable
	}
	return nil
}
