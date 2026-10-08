package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
	"github.com/akz142857/Halro/internal/store/lock"
)

type selectedRotationColdSourceKey struct{}
type selectedRotationColdSource struct {
	intent  *selectedRotationIntent
	manager *resourcebudget.Manager
	guard   *lock.Lock
}

// Only explicit same-operation resume, after MAC intent and current source
// selection validation, installs this private scope. It does not expose DAO,
// Ready, live native writes or a permission minted from a candidate header.
func selectedRotationColdSourceContext(ctx context.Context, i *selectedRotationIntent) context.Context {
	scope := keyScope(ctx)
	return context.WithValue(ctx, selectedRotationColdSourceKey{}, &selectedRotationColdSource{intent: i, manager: scope.root.pressure.logicalBudget(), guard: scope.lock.(*lock.Lock)})
}

func selectedRotationColdSourceRequested(ctx context.Context) bool {
	_, ok := ctx.Value(selectedRotationColdSourceKey{}).(*selectedRotationColdSource)
	return ok
}

func qualifySelectedRotationColdSource(ctx context.Context, s *runtimeSelectedState) error {
	c, ok := ctx.Value(selectedRotationColdSourceKey{}).(*selectedRotationColdSource)
	scope := keyScope(ctx)
	if !ok || c.intent == nil || scope == nil || scope.root.pressure.logicalBudget() != c.manager || scope.lock != c.guard || s.generation.Owners.Budget != c.manager || s.generation.Owners.Guard != c.guard || s.services != nil || s.generation.Seal != c.intent.SourceSeal {
		return fmt.Errorf("cold rotation source original scope/selection: %w", authenticatedstate.ErrSealIneligible)
	}
	// The persisted bootstrap fixes identity and versions; the signed intent
	// captured the later fully authenticated source publication positions.
	// Normalize only to this already exactly selected actual source Seal.
	actualBootstrapValue := s.bootstrap
	copy(actualBootstrapValue.Authority.Positions[:], s.generation.Seal.Positions[:])
	actualBootstrap, err := json.Marshal(actualBootstrapValue)
	if err != nil {
		return err
	}
	defer clear(actualBootstrap)
	savedBootstrap, err := json.Marshal(c.intent.SourceBootstrap)
	if err != nil {
		return err
	}
	defer clear(savedBootstrap)
	if !bytes.Equal(actualBootstrap, savedBootstrap) {
		return fmt.Errorf("cold rotation source independent bootstrap differs: %w", authenticatedstate.ErrSealIneligible)
	}
	if err = s.generation.WaitStartupAudit(ctx); err != nil {
		return err
	}
	o := s.generation.Owners
	a, err := statecut.CurrentStandaloneAuthority(ctx, o, c.intent.SourceBootstrap.Authority)
	if err != nil {
		return fmt.Errorf("cold rotation source actual seven-component authority: %w", err)
	}
	selected, found, err := s.publisher.SelectContext(ctx, a)
	if err != nil || !found || selected != c.intent.SourceSeal {
		return errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	// Complete original request and lease MAC scans under the still-held actual
	// startup maintenance lease. Zero receipt debt alone cannot authorize this.
	if err = o.Ledger.VerifyPhysicalNoDebt(ctx, o.NativeLedger, o.Lease); err != nil {
		return fmt.Errorf("cold rotation source actual full NoDebt: %w", err)
	}
	// The selected factory already constructed Usage under this actual startup
	// lease. No live-to-maintenance rebind is needed or permitted here.
	return nil
}
