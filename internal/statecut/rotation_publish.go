package statecut

import (
	"context"
	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/governance"
	"math"
)

// PublishStandaloneAuthorizedRotation persists independent offline authorization
// for the exact fully frozen candidate before the possibly durable common Seal.
// The callback cannot substitute roots or native positions. Cache ownership and
// full native authentication retain the ordinary publication contract.
func PublishStandaloneAuthorizedRotation(ctx context.Context, o StandaloneOwners, base authenticatedstate.CandidateSeal, authority authenticatedstate.SealEligibility, caches StartupCacheCandidates, authorize func(context.Context, authenticatedstate.CandidateSeal) error) (Publication, error) {
	if authorize == nil || caches.Audit == nil || caches.Governance == nil {
		return Publication{}, authenticatedstate.ErrSealIneligible
	}
	return publishStandaloneWithLoan(ctx, o, base, authority, authorize, func(ctx context.Context, candidate authenticatedstate.CandidateSeal, persist func(context.Context) error) error {
		g, a := candidate.Positions[2], candidate.Positions[3]
		if g.Offset > math.MaxInt64 || a.Offset > math.MaxInt64 {
			return authenticatedstate.ErrSealIneligible
		}
		return caches.Audit.WithFrozenOwner(ctx, o.Audit, o.Budget, authority.Instance, audit.Summary{Records: a.Sequence, Bytes: int64(a.Offset), LastHash: a.Hash}, func(ctx context.Context) error {
			return caches.Governance.WithFrozenOwner(ctx, o.NativeGovernance, o.Budget, authority.Instance, governance.Summary{Records: g.Sequence, Bytes: int64(g.Offset), LastHash: g.Hash}, persist)
		})
	})
}

// DrainSelectedForOfflineRotation joins the existing selected service owners
// and obtains their real native maintenance lease without publishing a second
// source Seal. Prepared rotation retries must preserve the authorized source
// revision while re-establishing the same actual maintenance relationships.
func (r *ReopenedStandalone) DrainSelectedForOfflineRotation(ctx context.Context) error {
	if r == nil {
		return authenticatedstate.ErrSealIneligible
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || !r.selectedStartup || r.services == nil {
		return authenticatedstate.ErrSealIneligible
	}
	s := r.services
	s.Accounting.WithdrawAdmission()
	if err := s.Governance.Retire(ctx); err != nil {
		return err
	}
	if err := s.Accounting.Retire(ctx); err != nil {
		return err
	}
	if err := s.Collector.CatchUp(ctx); err != nil {
		return err
	}
	if err := s.Collector.Retire(ctx); err != nil {
		return err
	}
	if err := s.Metadata.RetireFacade(ctx); err != nil {
		return err
	}
	lease, err := r.Owners.NativeLedger.BeginMaintenance(ctx)
	if err != nil {
		return err
	}
	r.Owners.Lease = lease
	r.checkpointLease = lease
	if err := alignStandaloneMaintenanceRoll(ctx, r.Owners, lease); err != nil {
		return err
	}
	if err := r.Owners.Usage.BindTypedMaintenance(ctx, r.Owners.NativeLedger, lease, r.Owners.Ledger, r.Owners.Budget); err != nil {
		return err
	}
	_, err = r.Owners.Directory.AdvanceFromNative(ctx, r.Owners.NativeLedger, lease, r.Owners.Ledger)
	return err
}
