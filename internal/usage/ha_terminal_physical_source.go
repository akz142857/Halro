package usage

import (
	"context"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
)

// This private consumer adapter never grants a Ledger physical capability. Its
// underlying loan comes only from the independent HA native constructor; the
// original Standalone extension retains its distinct hard role guard.
type haPhysicalUsageSourceAdapter struct {
	ledger.AuthenticatedHAPhysicalUsageSource
}

type haPhysicalStartupUsageSourceAdapter struct {
	ledger.SelectedHAPhysicalStartupSource
}

func (s haPhysicalStartupUsageSourceAdapter) PhysicalNativeReference(ctx context.Context) (authenticatedstate.PhysicalNativeReference, error) {
	return s.HAPhysicalNativeReference(ctx)
}
func (s haPhysicalStartupUsageSourceAdapter) PhysicalNativePreparedEnd(ctx context.Context) (int64, error) {
	return s.HAPhysicalNativePreparedEnd(ctx)
}
func (s haPhysicalStartupUsageSourceAdapter) VerifyPhysicalNativePrefix(ctx context.Context, prior authenticatedstate.PhysicalNativeReference, end int64) error {
	return s.VerifyHAPhysicalNativePrefix(ctx, prior, end)
}

func (s haPhysicalUsageSourceAdapter) PhysicalNativeReference(ctx context.Context) (authenticatedstate.PhysicalNativeReference, error) {
	return s.HAPhysicalNativeReference(ctx)
}
func (s haPhysicalUsageSourceAdapter) PhysicalNativePreparedEnd(ctx context.Context) (int64, error) {
	return s.HAPhysicalNativePreparedEnd(ctx)
}
func (s haPhysicalUsageSourceAdapter) VerifyPhysicalNativePrefix(ctx context.Context, prior authenticatedstate.PhysicalNativeReference, end int64) error {
	return s.VerifyHAPhysicalNativePrefix(ctx, prior, end)
}
func (s haPhysicalUsageSourceAdapter) PhysicalSelectionDigest(ctx context.Context) ([32]byte, error) {
	return s.HAPhysicalSelectionDigest(ctx)
}
func (s haPhysicalUsageSourceAdapter) ValidatePhysicalPublisher(ctx context.Context, publisher *authenticatedstate.SealCandidatePublisher) error {
	return s.ValidateHAPhysicalPublisher(ctx, publisher)
}
func (s haPhysicalUsageSourceAdapter) VerifyPhysicalNoDebt(ctx context.Context) error {
	return s.VerifyHAPhysicalNoDebt(ctx)
}

func (p *DiskProjection) withHAPhysicalSourceLocked(ctx context.Context, visit func(context.Context, ledger.AuthenticatedPhysicalUsageSource) error) error {
	e := p.engine
	if e == nil || !e.selectedHA || e.cow == nil || e.native == nil || e.owner == nil || e.budget == nil || visit == nil {
		return authenticatedstate.ErrRecovery
	}
	ref, err := e.owner.ReadUsageSourceReference(ctx)
	if err != nil {
		return err
	}
	callback := func(ctx context.Context, source ledger.AuthenticatedHAPhysicalUsageSource) error {
		return visit(ctx, haPhysicalUsageSourceAdapter{source})
	}
	frozen := e.live && e.lease != nil && e.native.ValidateHASelectedFrozenMaintenanceRead(ctx, e.lease) == nil
	if !e.live || frozen {
		return e.native.WithHASelectedSealAuthority(ctx, func(ctx context.Context, _ authenticatedstate.SealEligibility, _ ledger.HASelectedLedgerPosition) error {
			return e.owner.WithHASelectedPhysicalMaintenanceUsageSource(ctx, e.native, e.lease, e.budget, ref.Scope, callback)
		})
	}
	return e.owner.WithHASelectedPhysicalFrozenUsageSource(ctx, e.native, e.budget, ref.Scope, callback)
}

func (p *DiskProjection) withHAPhysicalMaintenanceSourceLocked(ctx context.Context, visit func(context.Context, ledger.AuthenticatedPhysicalUsageSource) error) error {
	e := p.engine
	if e == nil || !e.selectedHA || e.lease == nil || e.native == nil || e.owner == nil || e.budget == nil || visit == nil {
		return authenticatedstate.ErrRecovery
	}
	return e.native.WithHASelectedSealAuthority(ctx, func(ctx context.Context, _ authenticatedstate.SealEligibility, _ ledger.HASelectedLedgerPosition) error {
		ref, err := e.owner.ReadUsageSourceReference(ctx)
		if err != nil {
			return err
		}
		return e.owner.WithHASelectedPhysicalMaintenanceUsageSource(ctx, e.native, e.lease, e.budget, ref.Scope, func(ctx context.Context, s ledger.AuthenticatedHAPhysicalUsageSource) error {
			return visit(ctx, haPhysicalUsageSourceAdapter{s})
		})
	})
}
