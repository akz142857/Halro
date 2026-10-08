package usage

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
	bbolt "go.etcd.io/bbolt"
)

type AuthenticatedUsageAdvance struct {
	From, To                        ledger.ReplayAnchor
	NativeHead                      ledger.Watermark
	VerifiedRecords, AppliedRecords uint64
}

// CatchUpAuthenticatedFromNative applies only the selected Ledger owner's
// already independently permitted delta to a restored Usage image. It requires
// this native maintenance lease (Standalone/Primary); Replica selection remains
// external. Both complete suffix passes share lease exclusion. Readers and
// Close join the whole job; a failure after reduction begins retires the image.
// Fact counts exclude old bytes needed to reach a compressed replay anchor.
func (d *LedgerDerivative) CatchUpAuthenticatedFromNative(ctx context.Context, native *ledger.Log, lease *ledger.MaintenanceLease, budget *resourcebudget.Manager) (AuthenticatedUsageAdvance, error) {
	// Selection is checked again under the derivative lock in the typed job.
	if d != nil {
		d.mu.Lock()
		typed := d.disk != nil && d.disk.engine != nil && d.disk.engine.cow != nil
		selected := typed && d.disk.engine.selectedStartup
		d.mu.Unlock()
		if selected {
			return d.catchUpTypedSource(ctx, native, budget, lease)
		}
		if typed {
			return d.catchUpTypedAuthenticated(ctx, native, lease, budget, nil)
		}
	}
	return d.catchUpAuthenticatedFromNative(ctx, native, lease, budget, nil)
}

func (d *LedgerDerivative) catchUpAuthenticatedFromNative(ctx context.Context, native *ledger.Log, lease *ledger.MaintenanceLease, budget *resourcebudget.Manager, phase func(string)) (receipt AuthenticatedUsageAdvance, resultErr error) {
	if d == nil || native == nil || lease == nil || budget == nil {
		return receipt, authenticatedstate.ErrRecovery
	}
	if _, err := lease.ValidateOwner(native); err != nil {
		return receipt, err
	}
	defer func() {
		if resultErr != nil {
			receipt = AuthenticatedUsageAdvance{}
		}
	}()
	resultErr = lease.WithHead(ctx, func(ctx context.Context, frozen ledger.Watermark, hash [32]byte) (jobErr error) {
		if !d.mu.TryLock() {
			return authenticatedstate.ErrBusy
		}
		defer d.mu.Unlock()
		if d.retired.Load() || d.disk == nil {
			return ErrLedgerDerivativeRetired
		}
		p := d.disk
		ctx, cancel := context.WithCancel(ctx)
		p.catchUpMu.Lock()
		if p.retired.Load() || p.catchUpDone != nil {
			p.catchUpMu.Unlock()
			cancel()
			return authenticatedstate.ErrBusy
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
		if d.retired.Load() {
			d.replayMu.Unlock()
			return ErrLedgerDerivativeRetired
		}
		d.replayCancel = cancel
		d.replayMu.Unlock()
		defer func() { d.replayMu.Lock(); d.replayCancel = nil; d.replayMu.Unlock() }()
		d.ownerMu.RLock()
		owner, ok := d.owner.(*ledger.AuthenticatedLedgerProjection)
		d.ownerMu.RUnlock()
		if !ok {
			return authenticatedstate.ErrRecovery
		}
		if err := owner.ValidateNativeSource(ctx, native, lease); err != nil {
			return err
		}
		if err := owner.ValidateResourceOwner(ctx, budget); err != nil {
			return err
		}
		credit, err := p.liveRAM.RetainCovering(budget, resourcebudget.Cost{RAM: usageSealWorkspaceRAMBytes})
		if err != nil {
			return err
		}
		defer func() { jobErr = errors.Join(jobErr, credit.Release()) }()
		work, err := budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: usageSealWorkspaceRAMBytes + ledger.AuthenticatedLedgerSourceRAMBytes})
		if err != nil {
			return err
		}
		defer func() { jobErr = errors.Join(jobErr, work.Release()) }()
		p.mu.Lock()
		defer p.mu.Unlock()
		mutated := false
		defer func() {
			if jobErr != nil && (mutated || errors.Is(jobErr, ledger.ErrCorrupt) || errors.Is(jobErr, ErrDiskUsageCorrupt)) {
				p.retired.Store(true)
			}
		}()
		if p.retired.Load() || p.db == nil {
			return ErrDiskUsageClosed
		}
		var from ledger.ReplayAnchor
		if err := p.db.View(func(tx *bbolt.Tx) error {
			state, found, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
			if err != nil || !found || state.Version != diskUsageVersion {
				return errors.Join(ErrDiskUsageCorrupt, err)
			}
			from = ledger.ReplayAnchor{Head: state.Watermark, Hash: state.ReplayHash, Epoch: state.ReplayEpoch}
			return ctx.Err()
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
		seen := from == to || from.Head.Sequence == to.Head.Sequence && from.Hash == to.Hash && to.Head.Offset == 0
		full, err := native.ReplayAuthenticated(ctx, from, func(record ledger.Record) error {
			receipt.VerifiedRecords++
			if (ledger.ReplayAnchor{Head: ledger.Watermark{Generation: record.Generation, Sequence: record.Sequence, Offset: record.Offset}, Hash: record.Hash, Epoch: record.Epoch}) == to {
				seen = true
			}
			return ctx.Err()
		})
		if err != nil {
			return err
		}
		if full != frozen || !seen {
			return authenticatedstate.ErrRecovery
		}
		if current, currentHash, valid := native.ChainHead(); !valid || current != frozen || currentHash != hash {
			return authenticatedstate.ErrRecovery
		}
		if phase != nil {
			phase("source-verified")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		receipt.From, receipt.To, receipt.NativeHead = from, to, full
		if from == to {
			final, err := owner.SealReference(ctx)
			if err != nil || final != ref {
				return errors.Join(authenticatedstate.ErrRecovery, err)
			}
			return ctx.Err()
		}
		mutated = true
		batch := make([]ledger.Record, 0, diskReplayBatchRows)
		bytes := 0
		defer clear(batch[:cap(batch)])
		flush := func() error {
			if len(batch) == 0 {
				return nil
			}
			if d.retired.Load() {
				return ErrLedgerDerivativeRetired
			}
			if err := p.applyReplayBatchLocked(ctx, batch); err != nil {
				return err
			}
			receipt.AppliedRecords += uint64(len(batch))
			clear(batch)
			batch = batch[:0]
			bytes = 0
			if phase != nil {
				phase("batch-applied")
			}
			return ctx.Err()
		}
		full, err = native.ReplayAuthenticated(ctx, from, func(record ledger.Record) error {
			receipt.VerifiedRecords++
			if record.Sequence > to.Head.Sequence {
				return ctx.Err()
			}
			raw, err := json.Marshal(record)
			if err != nil {
				return err
			}
			if len(raw) > diskUsageMaxValue {
				return ErrDiskUsageLimit
			}
			if len(batch) == diskReplayBatchRows || len(raw) > diskReplayBatchBytes-bytes {
				if err := flush(); err != nil {
					return err
				}
			}
			batch = append(batch, record)
			bytes += len(raw)
			return ctx.Err()
		})
		if err != nil {
			return err
		}
		if full != frozen {
			return authenticatedstate.ErrRecovery
		}
		if current, currentHash, valid := native.ChainHead(); !valid || current != frozen || currentHash != hash {
			return authenticatedstate.ErrRecovery
		}
		if err := flush(); err != nil {
			return err
		}
		final, err := owner.SealReference(ctx)
		if err != nil || final != ref {
			return errors.Join(authenticatedstate.ErrRecovery, err)
		}
		return p.db.View(func(tx *bbolt.Tx) error {
			state, found, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
			if err != nil || !found || (ledger.ReplayAnchor{Head: state.Watermark, Hash: state.ReplayHash, Epoch: state.ReplayEpoch}) != to {
				return errors.Join(ErrDiskUsageCorrupt, err)
			}
			return ctx.Err()
		})
	})
	return receipt, resultErr
}
