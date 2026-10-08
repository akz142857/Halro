package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/statecut"
)

// This runs only while the actual startup maintenance lease excludes every
// business and transport producer. Each domain re-enters its genuine HA source
// scope; the digest below is a comparison, never a reusable permission.
func (f *haSelectedRuntimeFoundation) prepareHAPhysicalDerivedVector(ctx context.Context) error {
	if f == nil || f.cfg.ResourceBudget == nil || !f.cfg.ResourceBudget.PhysicalTerminalReserve || f.reopened == nil || f.growth == nil {
		return authenticatedstate.ErrSealIneligible
	}
	// Once the original Directory pool exists, its paid qualification lanes
	// cover subsequent NoDebt preparation too. The initial constructor has no
	// pool yet and acquires its real work as each domain is enabled.
	return f.reopened.Owners.Components.Directory.WithPhysicalCatchupWorkspace(ctx, f.prepareHAPhysicalDerivedVectorOwned)
}

func (f *haSelectedRuntimeFoundation) prepareHAPhysicalDerivedVectorOwned(ctx context.Context) error {
	c, limits := f.reopened.Owners.Components, f.cfg.ResourceBudget
	var selected authenticatedstate.CandidateSeal
	if err := f.withSelectedCut(ctx, func(ctx context.Context, cut statecut.HASelectedCut) error { selected = cut.Seal; return ctx.Err() }); err != nil {
		return err
	}
	raw, err := json.Marshal(selected)
	if err != nil {
		return err
	}
	selection := sha256.Sum256(raw)
	if err = f.prepareHAPhysicalPublicationTargets(ctx); err != nil {
		return fmt.Errorf("HA physical common targets: %w", err)
	}
	if err = c.Ledger.EnableHAQualifiedPhysicalTerminals(ctx, c.NativeLedger, c.Lease, limits.LedgerAppendDiskBytes, f.growth.credit, f.growth, selection); err != nil {
		return fmt.Errorf("HA physical Ledger: %w", err)
	}
	if err = c.NativeLedger.BindHAPhysicalSelectionDigestContext(ctx, c.Lease, selection); err != nil {
		return fmt.Errorf("HA physical Ledger selection: %w", err)
	}
	if err = c.Directory.EnableHAPhysicalCatchup(ctx, c.NativeLedger, c.Ledger, c.Lease, selection); err != nil {
		return fmt.Errorf("HA physical Directory: %w", err)
	}
	if err = c.Directory.InstallHAPhysicalCatchupCoverage(ctx, c.NativeLedger, c.Ledger, c.Lease); err != nil {
		return fmt.Errorf("HA physical Directory coverage: %w", err)
	}
	// The Directory pool did not exist when the outer startup callback entered.
	// Re-enter its original paid lanes after installation so Usage qualification
	// does not acquire a duplicate Member/Ordering/source workspace.
	if err = c.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		return c.Usage.EnableHAPhysicalCatchup(ctx, c.NativeLedger, c.Ledger, c.Lease, selection)
	}); err != nil {
		return fmt.Errorf("HA physical Usage: %w", err)
	}
	if err = c.NativeLedger.RequirePhysicalDerivedCoverage(ctx, c.Ledger); err != nil {
		return fmt.Errorf("HA physical complete Ledger coverage: %w", err)
	}
	if err = c.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		return c.Governance.PrepareHAPhysicalProjection(ctx, 2)
	}); err != nil {
		return fmt.Errorf("HA physical Governance COW: %w", err)
	}
	if err = c.Directory.WithPhysicalCatchupWorkspace(ctx, c.Metadata.EnableHAPhysicalTerminals); err != nil {
		return fmt.Errorf("HA physical Metadata: %w", err)
	}
	return ctx.Err()
}

// An actual newly published common cut and both exhausted recovery streams are
// mandatory. This does not close a source inside its own permission callback.
func (f *haSelectedRuntimeFoundation) retireHAPhysicalDerivedVectorAfterCommon(ctx context.Context) error {
	if f == nil || f.reopened == nil {
		return authenticatedstate.ErrSealIneligible
	}
	c := f.reopened.Owners.Components
	return c.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		var selected authenticatedstate.CandidateSeal
		if err := f.withSelectedCut(ctx, func(ctx context.Context, cut statecut.HASelectedCut) error {
			if err := statecut.RequireNoHAOrderingPeerPromises(ctx, f.owners, f.physicalPublication); err != nil {
				return err
			}
			selected = cut.Seal
			return ctx.Err()
		}); err != nil {
			return err
		}
		raw, err := json.Marshal(selected)
		if err != nil {
			return err
		}
		selection := sha256.Sum256(raw)
		if err := c.Usage.RetireHAPhysicalCatchupAfterCommon(ctx, c.NativeLedger, c.Ledger, c.Lease, c.Publisher); err != nil {
			return fmt.Errorf("HA physical Usage retirement: %w", err)
		}
		if err := c.Directory.RetireHAPhysicalCatchupAfterCommon(ctx, c.NativeLedger, c.Ledger, c.Lease, c.Publisher); err != nil {
			return fmt.Errorf("HA physical Directory retirement: %w", err)
		}
		if err := c.NativeLedger.RetireHAPhysicalPreparationAfterCommon(ctx, c.Lease); err != nil {
			return fmt.Errorf("HA physical Ledger retirement: %w", err)
		}
		if err := c.NativeLedger.BindHAPhysicalSelectionDigestContext(ctx, c.Lease, selection); err != nil {
			return err
		}
		if err := c.Directory.BindHAPhysicalCommonSelection(ctx, c.NativeLedger, c.Ledger, c.Lease, selection); err != nil {
			return err
		}
		if err := c.Usage.BindHAPhysicalCommonSelection(ctx, c.NativeLedger, c.Ledger, selection); err != nil {
			return err
		}
		if err := c.Governance.RetireHAPhysicalProjectionAfterCommon(ctx); err != nil {
			return fmt.Errorf("HA physical Governance retirement: %w", err)
		}
		if err := c.Audit.CheckpointHAPhysicalNative(ctx, false); err != nil {
			return fmt.Errorf("HA physical Audit checkpoint: %w", err)
		}
		if err := c.Metadata.RetireHAPhysicalPreparationAfterCommon(ctx); err != nil {
			return fmt.Errorf("HA physical Metadata retirement: %w", err)
		}
		return c.Metadata.BindHAPhysicalCommonSelection(ctx)
	})
}
