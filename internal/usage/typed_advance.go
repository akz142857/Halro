package usage

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func (d *LedgerDerivative) catchUpTypedAuthenticated(ctx context.Context, native *ledger.Log, lease *ledger.MaintenanceLease, budget *resourcebudget.Manager, phase func(string)) (receipt AuthenticatedUsageAdvance, resultErr error) {
	if d == nil || native == nil || lease == nil || budget == nil {
		return receipt, authenticatedstate.ErrRecovery
	}
	if !d.mu.TryLock() {
		return receipt, authenticatedstate.ErrBusy
	}
	defer d.mu.Unlock()
	if d.retired.Load() || d.disk == nil {
		return receipt, ErrLedgerDerivativeRetired
	}
	p := d.disk
	if p.engine == nil || p.engine.cow == nil || p.engine.native != native || p.engine.lease != lease {
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
	d.ownerMu.RLock()
	owner, ok := d.owner.(*ledger.AuthenticatedLedgerProjection)
	d.ownerMu.RUnlock()
	if !ok || owner != p.engine.owner {
		return receipt, authenticatedstate.ErrRecovery
	}
	if err := owner.ValidateNativeSource(ctx, native, lease); err != nil {
		return receipt, err
	}
	if err := owner.ValidateResourceOwner(ctx, budget); err != nil {
		return receipt, err
	}
	credit, err := p.liveRAM.RetainCovering(budget, resourcebudget.Cost{RAM: usageSealWorkspaceRAMBytes})
	if err != nil {
		return receipt, err
	}
	work, err := budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: usageSealWorkspaceRAMBytes + ledger.AuthenticatedLedgerSourceRAMBytes})
	if err != nil {
		return receipt, errors.Join(err, credit.Release())
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	mutated := false
	defer func() {
		if resultErr != nil {
			receipt = AuthenticatedUsageAdvance{}
			if mutated || native.Status().Load() == ledger.AccountingRecoveryRequired || errors.Is(resultErr, ledger.ErrCorrupt) || errors.Is(resultErr, authenticatedstate.ErrCorrupt) {
				p.retireBecause(resultErr)
			}
		}
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			p.retireBecause(resultErr)
			p.closeErr = errors.Join(p.closeErr, resultErr)
			p.typedWork = work
			p.typedDebtCredit = credit
		} else {
			resultErr = errors.Join(resultErr, work.Release(), credit.Release())
		}
	}()
	if p.retired.Load() {
		return receipt, p.unavailableError("native catch-up")
	}
	resultErr = lease.ReadFrozenHead(ctx, func(ctx context.Context, frozen ledger.Watermark, hash [32]byte) error {
		var from ledger.ReplayAnchor
		if err := p.engine.scoped(ctx).View(func(tx *usageTx) error {
			s, found, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
			if err != nil || !found {
				return errors.Join(ErrDiskUsageCorrupt, err)
			}
			from = usageStateAnchor(s)
			return nil
		}); err != nil {
			return err
		}
		ref, err := owner.SealReference(ctx)
		if err != nil {
			return err
		}
		to := ref.Anchor
		if from.Head.After(to.Head) || to.Head.After(frozen) || from.Head.Sequence > to.Head.Sequence {
			return authenticatedstate.ErrRecovery
		}
		seen := from == to
		verify := func(reduce bool) error {
			full, err := native.ReplayAuthenticated(ctx, from, func(r ledger.Record) error {
				receipt.VerifiedRecords++
				if typedUsageRecordAnchor(r) == to {
					seen = true
				}
				if reduce && r.Sequence <= to.Head.Sequence {
					mutated = true
					if err := p.reduceTypedVerifiedRecordLocked(ctx, r); err != nil {
						return err
					}
					receipt.AppliedRecords++
					if phase != nil {
						phase("batch-applied")
					}
				}
				return ctx.Err()
			})
			if err != nil {
				return err
			}
			actual, actualHash, valid := native.ChainHead()
			if !seen && to.Head.Offset == 0 {
				proved, err := native.AuthenticateAppliedAnchor(ctx, to.Head)
				if err != nil {
					return err
				}
				seen = proved == to
			}
			if !seen || full != frozen || !valid && (frozen.Sequence != 0 || hash != ([32]byte{})) || actual != frozen || actualHash != hash {
				return authenticatedstate.ErrRecovery
			}
			return ctx.Err()
		}
		if err := verify(false); err != nil {
			return err
		}
		if phase != nil {
			phase("source-verified")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		receipt.From, receipt.To, receipt.NativeHead = from, to, frozen
		if from != to {
			if err := verify(true); err != nil {
				return err
			}
			if err := verify(false); err != nil {
				return err
			}
		}
		if from != to {
			mutated = true
			if err := p.alignTypedVerifiedBoundary(ctx, to); err != nil {
				return err
			}
		}
		final, err := owner.SealReference(ctx)
		if err != nil || final != ref {
			return errors.Join(authenticatedstate.ErrRecovery, err)
		}
		return p.engine.scoped(ctx).View(func(tx *usageTx) error {
			s, found, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
			if err != nil || !found || usageStateAnchor(s) != to {
				return errors.Join(ErrDiskUsageCorrupt, err)
			}
			return ctx.Err()
		})
	})
	return receipt, resultErr
}
