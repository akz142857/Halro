package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

type typedLiveSourceKey struct{}
type typedLiveSourceScope struct {
	cow    *authenticatedstate.Store
	native *ledger.Log
	owner  *ledger.AuthenticatedLedgerProjection
	source ledger.AuthenticatedUsageSource
}

// BindTypedMaintenance returns this exact live owner to a new actual standalone
// lease after its Collector and business writers have joined. The full applied
// Ledger/Usage/native positions must coincide. An old lease or merely matching
// caller-supplied watermark cannot reauthorize a source or change its identity.
func (p *DiskProjection) BindTypedMaintenance(ctx context.Context, native *ledger.Log, lease *ledger.MaintenanceLease, owner *ledger.AuthenticatedLedgerProjection, manager *resourcebudget.Manager) error {
	if p == nil || native == nil || lease == nil || owner == nil || manager == nil {
		return authenticatedstate.ErrRecovery
	}
	if replicated, err := lease.ValidateOwner(native); err != nil || replicated {
		return errors.Join(authenticatedstate.ErrRecovery, err)
	}
	if !p.mu.TryLock() {
		return ErrDiskUsageBusy
	}
	defer p.mu.Unlock()
	e := p.engine
	if p.retired.Load() || e == nil || e.cow == nil || !e.live || e.native != native || e.owner != owner || e.budget != manager {
		return authenticatedstate.ErrRecovery
	}
	return lease.ReadFrozenHead(ctx, func(ctx context.Context, head ledger.Watermark, hash [32]byte) error {
		if err := owner.ValidateNativeSource(ctx, native, lease); err != nil {
			return err
		}
		// A completed native Roll may change the physical generation without
		// producing a semantic fact for the Collector. The actual frozen owner
		// must already have authenticated that transition. Align only its exact
		// unchanged sequence/hash/epoch through the real native source loan.
		if e.selectedStartup {
			ledgerRef, err := owner.ReadUsageSourceReference(ctx)
			if err != nil {
				return err
			}
			if err := owner.WithSelectedMaintenanceUsageSource(ctx, native, lease, manager, ledgerRef.Scope, func(ctx context.Context, source ledger.AuthenticatedUsageSource) error {
				ctx = context.WithValue(ctx, typedLiveSourceKey{}, &typedLiveSourceScope{cow: e.cow, native: native, owner: owner, source: source})
				var from ledger.ReplayAnchor
				if err := e.scoped(ctx).View(func(tx *usageTx) error {
					state, found, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
					if err != nil || !found {
						return errors.Join(ErrDiskUsageCorrupt, err)
					}
					from = usageStateAnchor(state)
					return nil
				}); err != nil {
					return err
				}
				to := source.Reference().Anchor
				if to.Head != head || to.Hash != hash {
					return authenticatedstate.ErrRecovery
				}
				if from == to {
					return source.Check(ctx)
				}
				if to.Head.Offset != 0 || to.Head.Sequence != from.Head.Sequence || to.Hash != from.Hash || to.Epoch != from.Epoch || !to.Head.After(from.Head) {
					return authenticatedstate.ErrRecovery
				}
				if err := source.VerifyAnchors(ctx, [2]ledger.ReplayAnchor{from, to}); err != nil {
					return err
				}
				if err := p.alignTypedVerifiedBoundary(ctx, to); err != nil {
					return err
				}
				return errors.Join(source.VerifyAnchors(ctx, [2]ledger.ReplayAnchor{from, to}), source.Check(ctx))
			}); err != nil {
				return err
			}
		}
		ref, err := p.typedSealReferenceLocked(ctx)
		if err != nil || ref.Ledger.Anchor.Head != head || ref.Ledger.Anchor.Hash != hash {
			return errors.Join(authenticatedstate.ErrRecovery, err)
		}
		e.lease, e.live = lease, false
		return nil
	})
}

// ActivateTypedLive binds reads to the actual standalone source before releasing
// the real startup lease. The caller still owns common-cut/Runtime acceptance.
// It admits no mutation: all live reduction must use CatchUpTypedLive.
func (p *DiskProjection) ActivateTypedLive(ctx context.Context, native *ledger.Log, manager *resourcebudget.Manager) error {
	return p.activateTypedLive(ctx, native, manager, false)
}

// ActivateHASelectedTypedLive binds the actual selected HA source before its
// real startup lease is released. Replica stays read-only; Primary durability
// and confirmation hooks remain owned by the actual native writer.
func (p *DiskProjection) ActivateHASelectedTypedLive(ctx context.Context, native *ledger.Log, manager *resourcebudget.Manager) error {
	return p.activateTypedLive(ctx, native, manager, true)
}

func (p *DiskProjection) activateTypedLive(ctx context.Context, native *ledger.Log, manager *resourcebudget.Manager, ha bool) error {
	if p == nil || native == nil || manager == nil {
		return authenticatedstate.ErrRecovery
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.engine
	if p.retired.Load() || e == nil || e.cow == nil || e.native != native || e.budget != manager || e.owner == nil || e.lease == nil {
		return p.unavailableError("live binding")
	}
	if e.selectedHA != ha {
		return authenticatedstate.ErrRecovery
	}
	if replicated, err := e.lease.ValidateOwner(native); err != nil || replicated != ha {
		return errors.Join(authenticatedstate.ErrRecovery, err)
	}
	if !ha {
		if err := e.owner.ValidateNativeWriter(ctx, native, e.lease); err != nil {
			return err
		}
	} else {
		ref, err := e.owner.ReadUsageSourceReference(ctx)
		if err != nil {
			return err
		}
		if err := e.owner.WithHASelectedMaintenanceUsageSource(ctx, native, e.lease, manager, ref.Scope, func(ctx context.Context, source ledger.AuthenticatedUsageSource) error { return source.Check(ctx) }); err != nil {
			return err
		}
	}
	if e.selectedStartup {
		// Live read admission uses the actual finite historical grant. Exporting
		// a structural Seal reference would incorrectly require full audit here.
		var anchor ledger.ReplayAnchor
		if err := e.scoped(ctx).View(func(tx *usageTx) error {
			state, found, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
			if err != nil || !found {
				return errors.Join(ErrDiskUsageCorrupt, err)
			}
			anchor = usageStateAnchor(state)
			return nil
		}); err != nil {
			return err
		}
		actual, err := e.owner.ReadReplayAnchor(ctx)
		if err != nil || actual != anchor {
			return errors.Join(authenticatedstate.ErrRecovery, err)
		}
		if err := native.ValidateSelectedStartupRead(ctx); err != nil {
			return err
		}
	} else {
		if _, err := p.typedSealReferenceLocked(ctx); err != nil {
			return err
		}
	}
	e.live = true
	return nil
}

// CatchUpTypedLive authenticates native facts and all reducer dependencies under
// one actual live Ledger/source job. It remains usable after startup maintenance
// is released, and never uses the closed lease to impersonate a frozen source.
// It reduces only through the selected Ledger owner, even if WAL has later
// durable-but-unapplied facts. Full old-byte authentication remains O(history).
// This local standalone path is not a default Runtime switch or HA authority.
func (d *LedgerDerivative) CatchUpTypedLive(ctx context.Context, native *ledger.Log, manager *resourcebudget.Manager) (receipt AuthenticatedUsageAdvance, resultErr error) {
	return d.catchUpTypedSource(ctx, native, manager, nil)
}

func (d *LedgerDerivative) catchUpTypedPreparedSource(ctx context.Context, native *ledger.Log, manager *resourcebudget.Manager, lease *ledger.MaintenanceLease, work resourcebudget.Permit, phase func(string), haStartup bool) (receipt AuthenticatedUsageAdvance, resultErr error) {
	stage := "ownership"
	defer func() {
		if resultErr != nil {
			resultErr = fmt.Errorf("typed Usage catch-up %s: %w", stage, resultErr)
		}
	}()
	keepWork := false
	var p *DiskProjection
	defer func() {
		if !keepWork {
			resultErr = releaseTypedLiveWork(work, resultErr)
			if p != nil && errors.Is(resultErr, ownedfile.ErrRetirement) {
				p.retireBecause(resultErr)
			}
		}
	}()

	if d == nil || native == nil || manager == nil {
		return receipt, authenticatedstate.ErrRecovery
	}
	if !d.mu.TryLock() {
		return receipt, authenticatedstate.ErrBusy
	}
	defer d.mu.Unlock()
	if d.retired.Load() || d.disk == nil {
		return receipt, ErrLedgerDerivativeRetired
	}
	p = d.disk
	if p.engine == nil || p.engine.cow == nil || p.engine.native != native || p.engine.budget != manager || p.engine.identity == ([32]byte{}) || haStartup && (!p.engine.selectedHA || lease == nil) || lease == nil && !p.engine.live || lease != nil && (p.engine.live && !haStartup || !p.engine.selectedStartup || p.engine.lease != lease) {
		return receipt, authenticatedstate.ErrRecovery
	}
	d.ownerMu.RLock()
	owner, ok := d.owner.(*ledger.AuthenticatedLedgerProjection)
	d.ownerMu.RUnlock()
	if !ok || owner != p.engine.owner {
		return receipt, authenticatedstate.ErrRecovery
	}
	ctx, cancel := context.WithCancel(ctx)
	p.catchUpMu.Lock()
	if p.retired.Load() || p.catchUpDone != nil {
		p.catchUpMu.Unlock()
		cancel()
		return receipt, authenticatedstate.ErrBusy
	}
	done := make(chan struct{})
	p.catchUpCancel, p.catchUpDone = cancel, done
	p.catchUpActive.Store(true)
	p.catchUpMu.Unlock()
	defer func() {
		cancel()
		p.catchUpMu.Lock()
		p.catchUpCancel, p.catchUpDone = nil, nil
		p.catchUpActive.Store(false)
		close(done)
		p.catchUpMu.Unlock()
	}()
	d.replayMu.Lock()
	d.replayCancel = cancel
	d.replayMu.Unlock()
	defer func() { d.replayMu.Lock(); d.replayCancel = nil; d.replayMu.Unlock() }()
	if err := owner.ValidateResourceOwner(ctx, manager); err != nil {
		return receipt, err
	}
	credit, err := p.liveRAM.RetainCovering(manager, resourcebudget.Cost{RAM: usageSealWorkspaceRAMBytes})
	if err != nil {
		return receipt, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	mutated := false
	defer func() {
		if resultErr != nil {
			receipt = AuthenticatedUsageAdvance{}
			if mutated && !p.capacityStop(resultErr) || native.Status().Load() == ledger.AccountingRecoveryRequired || errors.Is(resultErr, ledger.ErrCorrupt) || errors.Is(resultErr, authenticatedstate.ErrCorrupt) || errors.Is(resultErr, ledger.ErrProjectionCorrupt) || errors.Is(resultErr, ErrDiskUsageCorrupt) || p.engine.cow.NativeFrameRecoveryDebt() != nil {
				p.retireBecause(resultErr)
			}
		}
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			p.retireBecause(resultErr)
			p.closeErr = errors.Join(p.closeErr, resultErr)
			keepWork = true
			p.typedWork, p.typedDebtCredit = work, credit
		} else {
			resultErr = errors.Join(resultErr, credit.Release())
		}
	}()
	if p.retired.Load() {
		return receipt, p.unavailableError("native catch-up")
	}
	stage = "Ledger source reference"
	ref, err := owner.ReadUsageSourceReference(ctx)
	if err != nil {
		return receipt, err
	}
	withSource := func(ctx context.Context, visit func(context.Context, ledger.AuthenticatedUsageSource) error) error {
		if p.engine.selectedHA {
			if lease != nil {
				return owner.WithHASelectedMaintenanceUsageSource(ctx, native, lease, manager, ref.Scope, visit)
			}
			return owner.WithHASelectedFrozenUsageSource(ctx, native, manager, ref.Scope, visit)
		}
		if lease != nil {
			return owner.WithSelectedMaintenanceUsageSource(ctx, native, lease, manager, ref.Scope, visit)
		}
		return owner.WithFrozenUsageSource(ctx, native, manager, ref.Scope, visit)
	}
	stage = "frozen Ledger source"
	resultErr = withSource(ctx, func(ctx context.Context, source ledger.AuthenticatedUsageSource) error {
		ctx = context.WithValue(ctx, typedLiveSourceKey{}, &typedLiveSourceScope{cow: p.engine.cow, native: native, owner: owner, source: source})
		stage = "Usage root identity"
		root, err := p.engine.cow.RootIdentity(ctx)
		if err != nil || root.Scope != typedUsageScope(p.engine.identity, source.Reference().Scope) {
			return errors.Join(authenticatedstate.ErrRecovery, err)
		}
		to := source.Reference().Anchor
		var from ledger.ReplayAnchor
		stage = "Usage source position"
		if err := p.engine.scoped(ctx).View(func(tx *usageTx) error {
			state, found, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
			if err != nil || !found {
				return errors.Join(ErrDiskUsageCorrupt, err)
			}
			from = usageStateAnchor(state)
			return nil
		}); err != nil {
			return err
		}
		if from.Head.After(to.Head) || from.Head.Sequence > to.Head.Sequence {
			return authenticatedstate.ErrRecovery
		}
		pair := [2]ledger.ReplayAnchor{from, to}
		stage = "native anchor verification"
		if err := source.VerifyAnchors(ctx, pair); err != nil {
			return err
		}
		receipt.From, receipt.To, receipt.NativeHead = from, to, source.NativeHead().Head
		if from != to {
			previousReader := p.reader
			p.reader = source
			defer func() { p.reader = previousReader }()
			// Each transaction rewrites every touched page path once, so up to
			// typedReduceBatchRecords records share one rewrite. Every batch
			// commits whole with its watermark. The physical reserve prepaid
			// one COW root per native frame and each update consumes one, so
			// there every frame stays its own update: a shared one would leave
			// the rest prepaid and the owner never reaches no remaining debt.
			batchLimit := typedReduceBatchRecords
			if p.physical != nil {
				batchLimit = 1
			}
			var batch []ledger.Record
			batchBytes := 0
			flush := func() error {
				if len(batch) == 0 {
					return nil
				}
				if phase != nil {
					phase("before-record-reduce")
				}
				if err := p.reduceTypedVerifiedBatchLocked(ctx, batch); err != nil {
					return err
				}
				mutated = true
				if phase != nil {
					phase("record-applied")
				}
				receipt.AppliedRecords += uint64(len(batch))
				batch, batchBytes = batch[:0], 0
				return nil
			}
			head, err := source.ReplayAuthenticated(ctx, from, func(r ledger.Record) error {
				receipt.VerifiedRecords++
				if r.Sequence <= to.Head.Sequence {
					raw, err := json.Marshal(r)
					if err != nil {
						return err
					}
					if len(batch) == batchLimit || len(raw) > diskReplayBatchBytes-batchBytes {
						if err := flush(); err != nil {
							return err
						}
					}
					batch = append(batch, r)
					batchBytes += len(raw)
				}
				return ctx.Err()
			})
			if err == nil {
				err = flush()
			}
			if p.capacityStop(err) {
				return err
			}
			if err != nil || head != source.NativeHead().Head {
				return errors.Join(authenticatedstate.ErrRecovery, err)
			}
			if err := source.VerifyAnchors(ctx, pair); err != nil {
				return err
			}
			if err := p.alignTypedVerifiedBoundary(ctx, to); err != nil {
				return err
			}
			mutated = true
		}
		return p.engine.scoped(ctx).View(func(tx *usageTx) error {
			state, found, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
			if err != nil || !found || usageStateAnchor(state) != to {
				return errors.Join(ErrDiskUsageCorrupt, err)
			}
			return ctx.Err()
		})
	})
	return receipt, resultErr
}
