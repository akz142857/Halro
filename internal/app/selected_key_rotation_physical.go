package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
)

// The ordinary source Metadata owner cannot remain physically bound after a
// successor replaces its root and journal. Join it while its exact original
// common is still independently selected. This is a stopped offline rotation:
// all Facade/business borrowers have retired and the real maintenance lease
// already proved Ledger NoDebt. Native facts and the source root remain saved.
func joinSelectedPhysicalRotationMetadata(ctx context.Context, cfg config.Config, s *runtimeSelectedState) error {
	if cfg.ResourceBudget == nil || !cfg.ResourceBudget.PhysicalTerminalReserve {
		return nil
	}
	o := s.generation.Owners
	return o.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		if err := o.Ledger.VerifyPhysicalNoDebt(ctx, o.NativeLedger, o.Lease); err != nil {
			return err
		}
		if err := o.Metadata.CloseContext(ctx); err != nil {
			return fmt.Errorf("join independently selected rotation source Metadata: %w", err)
		}
		return nil
	})
}

// Only the publisher's actual successful successor publication permits the
// retained unchanged Ledger/Usage/Directory owners to move their physical
// receipts. Each existing API authenticates the full current native prefix,
// exact root and original owner/Manager; a supplied Seal alone grants nothing.
func bindSelectedPhysicalRotationSuccessor(ctx context.Context, cfg config.Config, s *runtimeSelectedState, expected authenticatedstate.CandidateSeal, authority authenticatedstate.SealEligibility) error {
	if cfg.ResourceBudget == nil || !cfg.ResourceBudget.PhysicalTerminalReserve {
		return nil
	}
	o := s.generation.Owners
	return o.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		actual, found, err := s.publisher.SelectContext(ctx, authority)
		if err != nil || !found || actual != expected {
			return errors.Join(authenticatedstate.ErrSealIneligible, err)
		}
		if err = o.Directory.RetirePhysicalCatchupAfterCommon(ctx, o.NativeLedger, o.Ledger, o.Lease, s.publisher); err != nil {
			return fmt.Errorf("retire rotation Directory after actual successor common: %w", err)
		}
		if err = o.Usage.RetirePhysicalCatchupAfterCommon(ctx, o.NativeLedger, o.Ledger, o.Lease, s.publisher); err != nil {
			return fmt.Errorf("retire rotation Usage after actual successor common: %w", err)
		}
		if err = o.NativeLedger.RetirePhysicalPreparationForMaintenance(ctx, o.Lease); err != nil {
			return fmt.Errorf("retire rotation native after actual successor common: %w", err)
		}
		raw, err := json.Marshal(actual)
		if err != nil {
			return err
		}
		selection := sha256.Sum256(raw)
		if err = o.NativeLedger.BindPhysicalSelectionDigestContext(ctx, selection); err != nil {
			return fmt.Errorf("bind rotation native to actual successor common: %w", err)
		}
		if err = o.Usage.BindPhysicalCommonSelection(ctx, o.NativeLedger, o.Ledger, selection); err != nil {
			return fmt.Errorf("bind rotation Usage to actual successor common: %w", err)
		}
		if err = o.Directory.BindPhysicalCommonSelection(ctx, o.NativeLedger, o.Ledger, selection); err != nil {
			return fmt.Errorf("bind rotation Directory to actual successor common: %w", err)
		}
		s.generation.Seal = actual
		return nil
	})
}
