package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

type haDirectoryPhysicalStartupSource struct {
	SelectedHAPhysicalStartupSource
}

func (s haDirectoryPhysicalStartupSource) PhysicalNativeReference(ctx context.Context) (authenticatedstate.PhysicalNativeReference, error) {
	return s.HAPhysicalNativeReference(ctx)
}
func (s haDirectoryPhysicalStartupSource) PhysicalNativePreparedEnd(ctx context.Context) (int64, error) {
	return s.HAPhysicalNativePreparedEnd(ctx)
}
func (s haDirectoryPhysicalStartupSource) VerifyPhysicalNativePrefix(ctx context.Context, prior authenticatedstate.PhysicalNativeReference, end int64) error {
	return s.VerifyHAPhysicalNativePrefix(ctx, prior, end)
}

// Directory.mu precedes the actual HA projection/native frozen source loan.
// Public comparison values never replace that original Member/Ordering scope.
func (d *AuthenticatedDirectory) withHAPhysicalSourceLocked(ctx context.Context, native *Log, owner *AuthenticatedLedgerProjection, lease *MaintenanceLease, visit func(context.Context, AuthenticatedHAPhysicalUsageSource) error) error {
	if d == nil || native == nil || owner == nil || lease == nil || d.selectedNative != native || owner.selectedNative.Load() != native || owner.budget != d.budget || native.startupBudget != d.budget || native.startupProvenance == nil || native.startupProvenance.ha == nil {
		return ErrProjectionSource
	}
	return owner.withFullHAPhysicalMaintenanceSource(ctx, native, lease, visit, true)
}

func (d *AuthenticatedDirectory) verifyHAPhysicalCommonLocked(ctx context.Context, source AuthenticatedHAPhysicalUsageSource) error {
	q, ok := source.(*haPhysicalUsageSource)
	if !ok {
		return ErrProjectionSource
	}
	q.source.mu.Lock()
	seal, _, err := q.commonLocked(ctx)
	q.source.mu.Unlock()
	if err != nil {
		return err
	}
	root, err := d.store.SealRootReference(ctx, authenticatedDirectorySchema)
	if err != nil {
		return err
	}
	for _, saved := range seal.Roots[:seal.RootCount] {
		if saved == root && seal.Positions[0] == authenticatedDirectoryPosition(d.anchor) {
			return nil
		}
	}
	return authenticatedstate.ErrSealIneligible
}

// EnableHAPhysicalCatchup is separate from the Standalone entrance. All initial
// actual targets are paid while the original full-cut maintenance lease lives.
func (d *AuthenticatedDirectory) EnableHAPhysicalCatchup(ctx context.Context, native *Log, owner *AuthenticatedLedgerProjection, lease *MaintenanceLease, selection [32]byte) error {
	if d == nil {
		return ErrProjectionSource
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.withHAPhysicalSourceLocked(ctx, native, owner, lease, func(ctx context.Context, source AuthenticatedHAPhysicalUsageSource) error {
		if err := source.VerifyHAPhysicalNoDebt(ctx); err != nil {
			return fmt.Errorf("HA directory full NoDebt: %w", err)
		}
		digest, err := source.HAPhysicalSelectionDigest(ctx)
		if err != nil || digest != selection {
			return errors.Join(authenticatedstate.ErrSealIneligible, err)
		}
		if d.anchor.Head != (Watermark{native.generation, native.offset, native.sequence}) || d.anchor.Hash != native.chainHash || d.anchor.Epoch != native.chainEpoch || d.segments != native.generation-1 {
			return ErrProjectionSource
		}
		if err := d.verifyHAPhysicalCommonLocked(ctx, source); err != nil {
			return fmt.Errorf("HA directory independent common root: %w", err)
		}
		reference, err := source.HAPhysicalNativeReference(ctx)
		if err != nil {
			return fmt.Errorf("HA directory native reference: %w", err)
		}
		end, err := source.HAPhysicalNativePreparedEnd(ctx)
		if err != nil {
			return fmt.Errorf("HA directory native prepared range: %w", err)
		}
		p := d.physical
		if p != nil {
			if !p.ha || p.native != native || p.ledger != owner || p.selection != selection {
				return ErrProjectionSource
			}
			// Outstanding windows were created by this original owner. Repeated
			// Enable cannot replace them, their fees, or the independent selection.
			if p.window != nil {
				return source.Check(ctx)
			}
			found, err := d.store.ValidatePhysicalTerminalPreparation(ctx, selection, reference, func(prior authenticatedstate.PhysicalNativeReference, end int64) error {
				return source.VerifyHAPhysicalNativePrefix(ctx, prior, end)
			})
			if err != nil || !found {
				return errors.Join(authenticatedstate.ErrRecovery, err)
			}
		} else {
			p = &directoryPhysicalOwner{native: native, ledger: owner, selection: selection, ha: true}
			d.physical = p
			if err := d.acquirePhysicalCatchupWork(ctx, p); err != nil {
				return err
			}
			_, err = d.store.EnablePhysicalTerminalQualified(ctx, selection, reference, func(prior authenticatedstate.PhysicalNativeReference, end int64) error {
				return source.VerifyHAPhysicalNativePrefix(ctx, prior, end)
			})
			if err != nil {
				d.sealErr = errors.Join(d.sealErr, err)
				return err
			}
		}
		if err = d.store.RecordPhysicalNativeRange(reference, end); err == nil {
			p.window, err = d.store.ReservePhysicalTerminals(ctx, directoryPhysicalCatchupSlots)
		}
		if err != nil {
			d.sealErr = errors.Join(d.sealErr, err)
			return err
		}
		p.remaining = directoryPhysicalCatchupSlots
		return source.Check(ctx)
	})
}

func (d *AuthenticatedDirectory) InstallHAPhysicalCatchupCoverage(ctx context.Context, native *Log, owner *AuthenticatedLedgerProjection, lease *MaintenanceLease) error {
	if d == nil || native == nil || owner == nil {
		return ErrProjectionSource
	}
	// Qualification must use this Directory's original paid pool even when
	// invoked from outside its preparation callback.
	return d.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		return d.installHAPhysicalCatchupCoverageOwned(ctx, native, owner, lease)
	})
}

func (d *AuthenticatedDirectory) installHAPhysicalCatchupCoverageOwned(ctx context.Context, native *Log, owner *AuthenticatedLedgerProjection, lease *MaintenanceLease) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	p := d.physical
	if p == nil || !p.ha || p.native != native || p.ledger != owner {
		return ErrProjectionSource
	}
	return native.bindHAPhysicalDerivedCoverage(ctx, owner, lease, PhysicalDerivedDirectory, d.physicalCatchupCoverage(p, owner), p)
}

func (d *AuthenticatedDirectory) RetireHAPhysicalCatchupAfterCommon(ctx context.Context, native *Log, owner *AuthenticatedLedgerProjection, lease *MaintenanceLease, publisher *authenticatedstate.SealCandidatePublisher) error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	p := d.physical
	if p == nil {
		return nil
	}
	if !p.ha || p.native != native || p.ledger != owner {
		return ErrProjectionSource
	}
	return d.withHAPhysicalSourceLocked(ctx, native, owner, lease, func(ctx context.Context, source AuthenticatedHAPhysicalUsageSource) error {
		if err := source.ValidateHAPhysicalPublisher(ctx, publisher); err != nil {
			return err
		}
		if err := source.VerifyHAPhysicalNoDebtAfterCommon(ctx); err != nil {
			return err
		}
		if d.anchor.Head != (Watermark{native.generation, native.offset, native.sequence}) || d.anchor.Hash != native.chainHash || d.anchor.Epoch != native.chainEpoch || d.segments != native.generation-1 {
			return authenticatedstate.ErrRecovery
		}
		if err := d.verifyHAPhysicalCommonLocked(ctx, source); err != nil {
			return err
		}
		if p.window != nil {
			if err := p.window.Close(); err != nil {
				d.sealErr = errors.Join(d.sealErr, err)
				return err
			}
		}
		p.window, p.remaining = nil, 0
		if err := d.store.RetirePhysicalPreparationKeepingControls(ctx); err != nil {
			d.sealErr = errors.Join(d.sealErr, err)
			return err
		}
		p.closedQualified = true
		return source.Check(ctx)
	})
}

func (d *AuthenticatedDirectory) BindHAPhysicalCommonSelection(ctx context.Context, native *Log, owner *AuthenticatedLedgerProjection, lease *MaintenanceLease, selection [32]byte) error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	p := d.physical
	if p == nil {
		return nil
	}
	if !p.ha || p.native != native || p.ledger != owner || p.window != nil || p.remaining != 0 {
		return authenticatedstate.ErrRecovery
	}
	return d.withHAPhysicalSourceLocked(ctx, native, owner, lease, func(ctx context.Context, source AuthenticatedHAPhysicalUsageSource) error {
		if err := source.VerifyHAPhysicalNoDebt(ctx); err != nil {
			return err
		}
		digest, err := source.HAPhysicalSelectionDigest(ctx)
		if err != nil || digest != selection {
			return errors.Join(authenticatedstate.ErrSealIneligible, err)
		}
		if err := d.verifyHAPhysicalCommonLocked(ctx, source); err != nil {
			return err
		}
		reference, err := source.HAPhysicalNativeReference(ctx)
		if err != nil {
			return err
		}
		end, err := source.HAPhysicalNativePreparedEnd(ctx)
		if err != nil {
			return err
		}
		if err = d.store.BindPhysicalSelectionDigest(selection, reference); err == nil {
			err = d.store.RecordPhysicalNativeRange(reference, end)
		}
		if err != nil {
			d.sealErr = errors.Join(d.sealErr, err)
			return err
		}
		p.selection = selection
		return source.Check(ctx)
	})
}
