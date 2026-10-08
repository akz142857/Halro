package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// Acquire the actual constructor's independent whole-cut permission before
// projection/native locks, matching the selected HA reopen factory. A bare
// public method call cannot substitute a single-source live read for this.
func (p *AuthenticatedLedgerProjection) withFullHAPhysicalMaintenanceSource(ctx context.Context, native *Log, lease *MaintenanceLease, visit func(context.Context, AuthenticatedHAPhysicalUsageSource) error, requirePhysical bool) error {
	if p == nil || native == nil || lease == nil || visit == nil {
		return ErrProjectionSource
	}
	return native.WithHASelectedSealAuthority(ctx, func(ctx context.Context, _ authenticatedstate.SealEligibility, _ HASelectedLedgerPosition) error {
		return p.WithHASelectedMaintenanceUsageSource(ctx, native, lease, p.budget, p.scope, haPhysicalUsageVisitorMode(visit, requirePhysical))
	})
}

// EnableHAQualifiedPhysicalTerminals is a separate preparation entrance. The
// actual startup maintenance lease and independently frozen Member/Ordering
// scope must still be live, before admission. It grants no HA confirmation and
// does not complete Directory/Usage/Ordering/Member/other-source preparation.
func (p *AuthenticatedLedgerProjection) EnableHAQualifiedPhysicalTerminals(ctx context.Context, native *Log, lease *MaintenanceLease, growthBytes int64, growthFee resourcebudget.Permit, capacity PhysicalNativeCapacity, selection [32]byte) error {
	if p == nil || native == nil || lease == nil || growthBytes <= 0 || growthBytes > 1<<50 || capacity == nil || selection == ([32]byte{}) {
		return ErrProjectionSource
	}
	if err := p.finishHAPhysicalRecoveryForPreparation(ctx, native, lease, selection); err != nil {
		return err
	}
	return p.withFullHAPhysicalMaintenanceSource(ctx, native, lease, func(ctx context.Context, source AuthenticatedHAPhysicalUsageSource) error {
		q := source.(*haPhysicalUsageSource)
		if _, ok := q.key.(haSelectedStartupScopeKey); !ok {
			return ErrProjectionSource
		}
		s := q.source
		s.mu.Lock()
		defer s.mu.Unlock()
		seal, digest, err := q.commonQualifiedLocked(ctx, false)
		if err != nil || digest != selection {
			return errors.Join(authenticatedstate.ErrSealIneligible, err)
		}
		if err = q.sourceNoDebtLocked(ctx, seal); err != nil {
			return err
		}
		if native.options.ReserveAppendDisk == nil || native.status.Load() != AccountingHealthy || len(native.chainKey) != 32 || !p.ready || p.closeContext.Err() != nil {
			return ErrProjectionSource
		}
		if native.physical.Load() != nil {
			reference, refErr := native.physicalNativeReferenceLocked()
			if refErr != nil {
				return refErr
			}
			found, verifyErr := p.store.ValidatePhysicalTerminalPreparation(ctx, selection, reference, func(prior authenticatedstate.PhysicalNativeReference, end int64) error {
				return directoryVerifyPhysicalPrefixLocked(ctx, native, reference, prior, end)
			})
			if verifyErr != nil || !found {
				return errors.Join(authenticatedstate.ErrRecovery, verifyErr)
			}
			original := native.physical.Load()
			if original.projection != p || original.selection != selection || original.debt != 0 || original.active != 0 || original.failure != nil {
				return authenticatedstate.ErrRecovery
			}
			// Original fully verified receipt/fee, after actual empty-window
			// join. No new installer or fee can replace the recovered owner.
			original.restoring, original.commonQualified = false, true
			return q.checkLocked(ctx, true)
		}
		if err = p.installPhysicalTerminalsLocked(ctx, native, growthBytes, growthFee, capacity, selection); err != nil {
			return err
		}
		// The original source proof above selected this exact complete root and
		// both complete recovery indexes, before any physical inode changed.
		native.physical.Load().commonQualified = true
		native.physical.Load().restoring = false
		return q.checkLocked(ctx, true)
	}, false)
}

// A recovered unused window can be joined only after the actual independent
// common root and complete two-stream NoDebt proof. Join occurs after returning
// the source pin/native lock; Finish cannot reenter that still-borrowed lane.
func (p *AuthenticatedLedgerProjection) finishHAPhysicalRecoveryForPreparation(ctx context.Context, native *Log, lease *MaintenanceLease, selection [32]byte) error {
	if !p.PhysicalStartupRecoveryPending() {
		return nil
	}
	err := p.withFullHAPhysicalMaintenanceSource(ctx, native, lease, func(ctx context.Context, source AuthenticatedHAPhysicalUsageSource) error {
		q := source.(*haPhysicalUsageSource)
		q.source.mu.Lock()
		defer q.source.mu.Unlock()
		seal, digest, err := q.commonLocked(ctx)
		if err != nil || digest != selection {
			return errors.Join(authenticatedstate.ErrSealIneligible, err)
		}
		o, w := native.physical.Load(), p.physicalRecovery
		if o == nil || w == nil || w.native != native || w.closed || w.claimed != 0 || w.active != 0 || o.active != 0 || o.debt != w.remaining {
			return authenticatedstate.ErrRecovery
		}
		return q.sourceNoDebtLocked(ctx, seal)
	}, true)
	if err != nil {
		return err
	}
	return p.FinishPhysicalStartupRecovery(ctx, native)
}

// The actual new seven-root cut, full allowed native boundary, and complete
// empty recovery indexes precede any retirement. The old receipt remains the
// ownership proof; a caller fingerprint grants no right to punch or delete.
func (l *Log) RetireHAPhysicalPreparationAfterCommon(ctx context.Context, lease *MaintenanceLease) error {
	if l == nil || lease == nil || l.startupProvenance == nil || l.startupProvenance.ha == nil {
		return ErrProjectionSource
	}
	o := l.physical.Load()
	if o == nil {
		return nil
	}
	p := o.projection
	return p.withFullHAPhysicalMaintenanceSource(ctx, l, lease, func(ctx context.Context, source AuthenticatedHAPhysicalUsageSource) error {
		if err := source.VerifyHAPhysicalNoDebtAfterCommon(ctx); err != nil {
			return err
		}
		if err := l.checkPhysicalNativeLocked(); err != nil {
			return err
		}
		if err := l.retirePhysicalTailLocked(true); err != nil {
			o.failure = err
			l.status.RequireRecovery()
			return err
		}
		return source.Check(ctx)
	}, true)
}

// Only an original live full-cut source can qualify HA physical FD shutdown.
// CloseContext still cancels and joins all actual writers/FDs. Unknown exit
// retains the owner, key and cost; subsequent ordinary Close cannot remint it.
func (l *Log) CloseHAQualifiedPhysicalContext(ctx context.Context, lease *MaintenanceLease) error {
	if l == nil || lease == nil || l.startupProvenance == nil || l.startupProvenance.ha == nil {
		return ErrProjectionSource
	}
	if ctx.Value(haSelectedStartupScopeKey{}) != nil {
		return authenticatedstate.ErrBusy
	}
	o := l.physical.Load()
	if o == nil {
		return l.CloseContext(ctx)
	}
	p := o.projection
	err := p.withFullHAPhysicalMaintenanceSource(ctx, l, lease, func(ctx context.Context, source AuthenticatedHAPhysicalUsageSource) error {
		if err := source.VerifyHAPhysicalNoDebt(ctx); err != nil {
			return err
		}
		if err := l.preparePhysicalDerivedLocked(ctx, PhysicalDerivedClose, 0); err != nil {
			return err
		}
		if err := source.Check(ctx); err != nil {
			return err
		}
		o.haCloseQualified = true
		return nil
	}, true)
	if err != nil {
		l.mu.Lock()
		o.haCloseQualified = false
		l.mu.Unlock()
		// Failure to qualify does not skip the real shutdown join. The normal
		// HA check cannot retire targets without the original certificate.
		return errors.Join(err, l.CloseContext(ctx))
	}
	return l.CloseContext(ctx)
}

func (l *Log) checkHAPhysicalCloseSelectionLocked(ctx context.Context) error {
	p, o := l.startupProvenance, l.physical.Load()
	if p == nil || p.ha == nil || o == nil || !o.commonQualified || !o.haCloseQualified || p.publisher == nil || p.guard == nil || o.projection == nil || o.projection.selectedNative.Load() != l {
		return authenticatedstate.ErrRecovery
	}
	if err := p.publisher.ValidateOwner(p.guard, o.projection.budget); err != nil {
		return err
	}
	seal, found, err := p.publisher.CurrentContext(ctx)
	if err != nil || !found || seal.Role != p.ha.role || seal.Coverage != authenticatedstate.SealCoverageAuthenticated {
		return errors.Join(authenticatedstate.ErrRecovery, err)
	}
	raw, err := json.Marshal(seal)
	if err != nil || sha256.Sum256(raw) != o.selection {
		return errors.Join(authenticatedstate.ErrRecovery, err)
	}
	return ctx.Err()
}

// BindHAPhysicalSelectionDigestContext finishes only an actual new common cut
// under its original maintenance scope. The Standalone Bind remains unchanged.
func (l *Log) BindHAPhysicalSelectionDigestContext(ctx context.Context, lease *MaintenanceLease, selection [32]byte) error {
	if l == nil || lease == nil || selection == ([32]byte{}) {
		return ErrProjectionSource
	}
	o := l.physical.Load()
	if o == nil {
		return authenticatedstate.ErrRecovery
	}
	p := o.projection
	return p.withFullHAPhysicalMaintenanceSource(ctx, l, lease, func(ctx context.Context, source AuthenticatedHAPhysicalUsageSource) error {
		q := source.(*haPhysicalUsageSource)
		if _, ok := q.key.(haSelectedStartupScopeKey); !ok {
			return ErrProjectionSource
		}
		s := q.source
		s.mu.Lock()
		defer s.mu.Unlock()
		seal, digest, err := q.commonLocked(ctx)
		if err != nil || digest != selection {
			return errors.Join(authenticatedstate.ErrSealIneligible, err)
		}
		if o.debt != 0 || o.active != 0 {
			return authenticatedstate.ErrRecovery
		}
		if err = q.sourceNoDebtLocked(ctx, seal); err != nil {
			return err
		}
		reference, err := l.physicalNativeReferenceLocked()
		if err != nil {
			return err
		}
		if err = p.store.BindPhysicalSelectionDigest(selection, reference); err != nil {
			o.failure = err
			l.status.RequireRecovery()
			return err
		}
		o.selection = selection
		o.restoring = false
		o.commonQualified = true
		return q.checkLocked(ctx, true)
	}, true)
}

// BindHAPhysicalDerivedCoverage captures the same original native physical
// owner, while an actual complete HA source scope excludes producers. Future
// callbacks follow native.mu -> Store.mu and cannot acquire a domain mutex.
func (l *Log) BindHAPhysicalDerivedCoverage(ctx context.Context, projection *AuthenticatedLedgerProjection, lease *MaintenanceLease, role PhysicalDerivedRole, visit PhysicalDerivedCoverage) error {
	return l.bindHAPhysicalDerivedCoverage(ctx, projection, lease, role, visit, nil)
}

func (l *Log) bindHAPhysicalDerivedCoverage(ctx context.Context, projection *AuthenticatedLedgerProjection, lease *MaintenanceLease, role PhysicalDerivedRole, visit PhysicalDerivedCoverage, directory *directoryPhysicalOwner) error {
	if l == nil || projection == nil || lease == nil || visit == nil || role < PhysicalDerivedDirectory || role > PhysicalDerivedUsage {
		return ErrProjectionSource
	}
	return projection.withFullHAPhysicalMaintenanceSource(ctx, l, lease, func(ctx context.Context, source AuthenticatedHAPhysicalUsageSource) error {
		q := source.(*haPhysicalUsageSource)
		if _, ok := q.key.(haSelectedStartupScopeKey); !ok {
			return ErrProjectionSource
		}
		if err := source.VerifyHAPhysicalNoDebt(ctx); err != nil {
			return err
		}
		o := l.physical.Load()
		index := int(role - 1)
		if o == nil || o.projection != projection {
			return ErrProjectionSource
		}
		if directory != nil && (role != PhysicalDerivedDirectory || !directory.ha || directory.native != l || directory.ledger != projection || directory.window == nil || directory.remaining == 0 || directory.selection != o.selection) {
			return ErrProjectionSource
		}
		if o.derived[index] != nil {
			// Re-entered full Member/Ordering maintenance and original native
			// NoDebt above. Keep the original callback and its captured owner;
			// a new closure, public role or equal selection cannot replace it.
			if directory != nil && o.derivedDirectory == directory {
				return source.Check(ctx)
			}
			return authenticatedstate.ErrBusy
		}
		if err := source.Check(ctx); err != nil {
			return err
		}
		o.derived[index] = visit
		if directory != nil {
			o.derivedDirectory = directory
		}
		return nil
	}, true)
}
