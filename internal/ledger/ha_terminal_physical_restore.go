package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// This is called only inside the actual HA selected projection constructor,
// after full native authentication and independently resolving Allowed. An
// ordinary receipt or candidate root cannot mint this private source scope.
func qualifyHAPhysicalStartupRestoreLocked(ctx context.Context, native *Log, selection [32]byte, target ReplayAnchor) error {
	if native == nil || native.startupProvenance == nil || native.startupProvenance.ha == nil || native.startupProvenance.publisher == nil || native.startupProvenance.guard == nil || !native.selectedStartupStructuralAllowedLocked() {
		return ErrProjectionSource
	}
	scope, _ := ctx.Value(haSelectedStartupScopeKey{}).(*haSelectedStartupScope)
	if scope == nil || scope.owner != native.startupProvenance.ha || !scope.active.Load() || !validHAUsageSourceMode(native, nil, scope.owner, scope.authority) || target.Head.Generation != scope.allowed.Generation || target.Head.Sequence != scope.allowed.Sequence || target.Hash != scope.allowed.Hash {
		return ErrProjectionSource
	}
	seal, found, err := native.startupProvenance.publisher.CurrentContext(ctx)
	if err != nil || !found {
		return errors.Join(ErrProjectionSource, err)
	}
	if err = authenticatedstate.ValidateSealEligibility(seal, scope.authority); err != nil {
		return err
	}
	raw, err := json.Marshal(seal)
	if err != nil || sha256.Sum256(raw) != selection {
		return errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	return ctx.Err()
}

func (p *AuthenticatedLedgerProjection) restoreHAPhysicalStartupLocked(ctx context.Context, native *Log, selection [32]byte, target ReplayAnchor, o *PhysicalStartupRecoveryOptions) (bool, error) {
	if err := qualifyHAPhysicalStartupRestoreLocked(ctx, native, selection, target); err != nil {
		return false, err
	}
	return p.restorePhysicalStartupModeLocked(ctx, native, selection, target, o, true)
}

// Original selected saved root -> candidate receipt root -> independent
// Allowed are each matched against fully authenticated actual native frames.
func verifyHAPhysicalRecoveryAnchorLocked(ctx context.Context, native *Log, candidate, target ReplayAnchor) error {
	if projectionPrefixBefore(candidate.Head, native.startupSaved.Head) || projectionPrefixBefore(target.Head, candidate.Head) {
		return ErrProjectionSource
	}
	seen := candidate == native.startupSaved
	observe := func(anchor ReplayAnchor) error { seen = seen || anchor == candidate; return ctx.Err() }
	head, err := native.replayAuthenticatedLocked(ctx, native.startupSaved, 0, func(r Record) error {
		return observe(ReplayAnchor{Head: Watermark{r.Generation, r.Offset, r.Sequence}, Hash: r.Hash, Epoch: r.Epoch})
	}, observe)
	if err != nil || !seen || head != (Watermark{native.generation, native.offset, native.sequence}) {
		return errors.Join(ErrProjectionSource, err)
	}
	return ctx.Err()
}

// Caller holds original projection/native source locks. These bytes already
// exist durably; this consumes only the receipt's original candidate inode.
// It does not append native, allocate a replacement, or grant confirmation.
func (p *AuthenticatedLedgerProjection) updateHAPhysicalRestoredDeltaLocked(ctx context.Context, native *Log, cut [32]byte, apply func(*authenticatedstate.Transaction) error) error {
	if native.options.Replica && p.physicalReplica != nil && p.physicalRecovery == nil {
		w, o := p.physicalReplica, native.physical.Load()
		if w.native != native || w.closed || w.remaining < 1 || w.replicaReceived < 1 || o == nil || o.projection != p || o.debt < 1 || o.failure != nil {
			return authenticatedstate.ErrRecovery
		}
		u, err := p.store.PreparePhysicalUpdate(ctx, w.cow)
		if err != nil {
			return err
		}
		_, applyErr := p.store.Update(u.Bind(ctx), cut, apply)
		if err = errors.Join(applyErr, u.Finish(true, applyErr)); err != nil {
			o.failure = err
			native.status.RequireRecovery()
			return err
		}
		w.remaining--
		w.replicaReceived--
		o.debt--
		o.commonQualified = false
		return nil
	}
	if native.startupProvenance == nil || native.startupProvenance.ha == nil || p.physicalRecovery == nil {
		receipt, err := p.store.Update(ctx, cut, apply)
		if err == nil {
			p.pack.updated(p.store, receipt.AppendedBytes)
		}
		return err
	}
	w, work := p.physicalRecovery, p.physicalRecoveryWork
	o := native.physical.Load()
	if work == nil || w.native != native || w.remaining < 1 || o == nil || o.projection != p || o.debt < 1 || o.failure != nil {
		return authenticatedstate.ErrRecovery
	}
	return resourcebudget.WithCompletionWorkspace(work.Bind(ctx), func(ctx context.Context) error {
		u, err := p.store.PreparePhysicalUpdate(ctx, w.cow)
		if err != nil {
			return err
		}
		_, applyErr := p.store.Update(u.Bind(ctx), cut, apply)
		err = errors.Join(applyErr, u.Finish(true, applyErr))
		if err != nil {
			o.failure = err
			native.status.RequireRecovery()
			return err
		}
		w.remaining--
		o.debt--
		o.commonQualified = false
		return nil
	})
}
