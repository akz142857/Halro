package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func prepareStandalonePhysicalTerminal(ctx context.Context, cfg config.Config, selected *runtimeSelectedState, member *replicationRuntime, nativeDisk *runtimeLedgerAppendDiskOwner) (func(context.Context, int) (context.Context, func() error, error), error) {
	if cfg.ResourceBudget == nil || !cfg.ResourceBudget.PhysicalTerminalReserve {
		return nil, nil
	}
	if selected == nil || selected.generation == nil || member != nil || nativeDisk == nil {
		return nil, errors.New("physical_terminal_reserve requires the actual selected Standalone generation; HA physical permission is not implemented")
	}
	o := selected.generation.Owners
	if cfg.Gateway.MaxTotalAttempts < 1 || cfg.Gateway.MaxTotalAttempts >= resourcebudget.MaxCompletionBatch {
		return nil, resourcebudget.ErrInvalid
	}
	// Allocation/retirement of an unused tail can change mtime. Join the
	// independently selected historical readers before mutating their source
	// metadata; the opt-in mode must never race qualification of that inode.
	if err := selected.generation.WaitStartupAudit(ctx); err != nil {
		return nil, err
	}
	if err := selected.publisher.EnablePhysicalSealPreparation(ctx, selected.generation.Seal, selected.physicalSealDisk); err != nil {
		return nil, fmt.Errorf("physical common seal preparation: %w", err)
	}
	if err := selected.preparePhysicalCacheTargets(ctx, cfg); err != nil {
		return nil, fmt.Errorf("physical cache preparation: %w", err)
	}
	raw, err := json.Marshal(selected.generation.Seal)
	if err != nil {
		return nil, err
	}
	selection := sha256.Sum256(raw)
	if err := o.Ledger.EnableQualifiedPhysicalTerminals(ctx, o.NativeLedger, cfg.ResourceBudget.LedgerAppendDiskBytes, nativeDisk.credit, nativeDisk, selection); err != nil {
		return nil, fmt.Errorf("physical ledger preparation: %w", err)
	}
	if err := o.NativeLedger.BindPhysicalSelectionDigestContext(ctx, selection); err != nil {
		return nil, fmt.Errorf("physical ledger common selection: %w", err)
	}
	if err := o.Directory.EnablePhysicalCatchup(ctx, o.NativeLedger, o.Ledger, selection); err != nil {
		return nil, fmt.Errorf("physical directory preparation: %w", err)
	}
	if err := o.Directory.InstallPhysicalCatchupCoverage(ctx, o.NativeLedger, o.Ledger); err != nil {
		return nil, fmt.Errorf("physical directory source coverage: %w", err)
	}
	if err := o.Usage.EnablePhysicalCatchup(ctx, o.NativeLedger, o.Ledger, selection); err != nil {
		return nil, fmt.Errorf("physical usage preparation: %w", err)
	}
	if err := o.NativeLedger.RequirePhysicalDerivedCoverage(ctx, o.Ledger); err != nil {
		return nil, fmt.Errorf("physical complete derived coverage: %w", err)
	}
	authority := selected.bootstrap.Authority
	copy(authority.Positions[:], selected.generation.Seal.Positions[:])
	if err := o.Metadata.EnableQualifiedPhysicalTerminals(ctx, selected.publisher, o.Guard, authority); err != nil {
		return nil, fmt.Errorf("physical metadata preparation: %w", err)
	}
	return standalonePhysicalTerminalAdmission(cfg, selected), nil
}

// The returned admission is shared by the actual Gateway and controlled local
// lifecycle fixtures. Preparation and source permission remain with the actual
// selected owners; obtaining this closure does not enable a new owner.
func standalonePhysicalTerminalAdmission(cfg config.Config, selected *runtimeSelectedState) func(context.Context, int) (context.Context, func() error, error) {
	o := selected.generation.Owners
	return func(ctx context.Context, count int) (context.Context, func() error, error) {
		if err := selected.publisher.ValidatePhysicalSealPreparation(ctx, selected.generation.Seal); err != nil {
			if ctx.Err() == nil {
				o.NativeLedger.Status().RequireRecovery()
			}
			return ctx, nil, errors.Join(authenticatedstate.ErrRecovery, err)
		}
		if err := selected.preparePhysicalCacheTargets(ctx, cfg); err != nil {
			if ctx.Err() == nil && !errors.Is(err, resourcebudget.ErrExhausted) && !errors.Is(err, resourcebudget.ErrPressure) {
				o.NativeLedger.Status().RequireRecovery()
			}
			return ctx, nil, err
		}
		if count < 2 {
			return ctx, nil, resourcebudget.ErrInvalid
		}
		metadata, err := o.Metadata.ReservePhysicalTerminals(ctx, count-1)
		if err != nil {
			return ctx, nil, err
		}
		w, err := o.NativeLedger.ReservePhysicalTerminals(ctx, count)
		if err != nil {
			return ctx, nil, errors.Join(err, metadata.Close())
		}
		return w.Bind(metadata.Bind(ctx)), func() error {
			if ledgerErr := w.Close(); ledgerErr != nil {
				return errors.Join(ledgerErr, metadata.RetainForRecovery(ledgerErr))
			}
			return metadata.Close()
		}, nil
	}
}
