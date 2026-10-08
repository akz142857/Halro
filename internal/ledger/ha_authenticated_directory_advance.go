package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

// AdvanceHASelectedFromNative refreshes only the actual selected Ledger
// owner's permitted directory prefix. The native job authenticates the whole
// physical tail; a received/unapplied roll never becomes a directory row here.
// It lends no arbitrary prefix or common-cut publication authority.
func (d *AuthenticatedDirectory) AdvanceHASelectedFromNative(ctx context.Context, native *Log, owner *AuthenticatedLedgerProjection) (changed uint64, resultErr error) {
	if d == nil || native == nil || owner == nil || d.store == nil {
		return 0, ErrProjectionSource
	}
	return d.advanceHASelectedFromNative(ctx, native, owner, nil)
}

// AdvanceHASelectedReplicaDirectoryFromNative authenticates the actual received
// directory prefix without consuming a checkpoint COW target for every same-
// generation business fact. Closed descriptors still advance after a real roll;
// the complete scalar is published by the original common-maintenance caller.
func (d *AuthenticatedDirectory) AdvanceHASelectedReplicaDirectoryFromNative(ctx context.Context, native *Log, owner *AuthenticatedLedgerProjection) (changed uint64, resultErr error) {
	if d == nil || native == nil || !native.options.Replica || owner == nil || d.physical == nil || !d.physical.ha || d.physical.native != native || d.physical.ledger != owner {
		return 0, ErrProjectionSource
	}
	resultErr = d.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		var err error
		changed, err = d.advanceHASelectedFromNativeOwnedMode(ctx, native, owner, nil, true)
		return err
	})
	return changed, resultErr
}

// AdvanceHASelectedMaintenanceFromNative borrows the real exclusive selected
// lease; it keeps the same permitted source and complete directory MAC checks.
func (d *AuthenticatedDirectory) AdvanceHASelectedMaintenanceFromNative(ctx context.Context, native *Log, lease *MaintenanceLease, owner *AuthenticatedLedgerProjection) (uint64, error) {
	if lease == nil {
		return 0, ErrProjectionSource
	}
	return d.advanceHASelectedFromNative(ctx, native, owner, lease)
}

func (d *AuthenticatedDirectory) advanceHASelectedFromNative(ctx context.Context, native *Log, owner *AuthenticatedLedgerProjection, lease *MaintenanceLease) (changed uint64, resultErr error) {
	if d == nil {
		return 0, ErrProjectionSource
	}
	resultErr = d.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		var err error
		changed, err = d.advanceHASelectedFromNativeOwned(ctx, native, owner, lease)
		return err
	})
	return changed, resultErr
}

func (d *AuthenticatedDirectory) advanceHASelectedFromNativeOwned(ctx context.Context, native *Log, owner *AuthenticatedLedgerProjection, lease *MaintenanceLease) (uint64, error) {
	return d.advanceHASelectedFromNativeOwnedMode(ctx, native, owner, lease, false)
}

func (d *AuthenticatedDirectory) advanceHASelectedFromNativeOwnedMode(ctx context.Context, native *Log, owner *AuthenticatedLedgerProjection, lease *MaintenanceLease, deferSameGenerationScalar bool) (changed uint64, resultErr error) {
	if d == nil || native == nil || owner == nil || d.store == nil {
		return 0, ErrProjectionSource
	}
	withSource := func(ctx context.Context, visit func(context.Context, AuthenticatedUsageSource) error) error {
		if d.physical != nil {
			physicalVisit := func(ctx context.Context, source AuthenticatedHAPhysicalUsageSource) error { return visit(ctx, source) }
			if lease != nil {
				return owner.WithHASelectedPhysicalMaintenanceUsageSource(ctx, native, lease, d.budget, d.ledgerScope, physicalVisit)
			}
			return owner.WithHASelectedPhysicalFrozenUsageSource(ctx, native, d.budget, d.ledgerScope, physicalVisit)
		}
		if lease != nil {
			return owner.WithHASelectedMaintenanceUsageSource(ctx, native, lease, d.budget, d.ledgerScope, visit)
		}
		return owner.WithHASelectedFrozenUsageSource(ctx, native, d.budget, d.ledgerScope, visit)
	}
	resultErr = withSource(ctx, func(ctx context.Context, source AuthenticatedUsageSource) error {
		if !d.mu.TryLock() {
			return authenticatedstate.ErrBusy
		}
		defer d.mu.Unlock()
		if err := errors.Join(ctx.Err(), d.closeCtx.Err(), d.sealErr); err != nil {
			return err
		}
		ref := source.Reference()
		anchor := ref.Anchor
		if ref.Scope != d.ledgerScope || anchor.Head.Generation == 0 || anchor.Head.Generation < d.anchor.Head.Generation || anchor.Head.Sequence < d.anchor.Head.Sequence {
			return ErrProjectionSource
		}
		check := func() error {
			return errors.Join(source.VerifyAnchors(ctx, [2]ReplayAnchor{d.anchor, anchor}), source.Check(ctx), d.closeCtx.Err())
		}
		if err := check(); err != nil {
			return err
		}
		written, complete := false, false
		defer func() {
			if written && !complete {
				d.sealErr = errors.Join(d.sealErr, ErrDirectoryPartialAdvance)
			}
		}()
		for generation := uint64(1); generation < anchor.Head.Generation; {
			var rows [63]authenticatedstate.Row
			n := 0
			pin, err := d.store.Snapshot(ctx)
			if err != nil {
				return err
			}
			for generation < anchor.Head.Generation && n < len(rows) {
				segment, found, readErr := native.lookupSegmentLocked(generation)
				if readErr != nil || !found {
					err = errors.Join(ErrSegmentMissing, readErr)
					break
				}
				raw, marshalErr := json.Marshal(segment)
				key := authenticatedDirectoryKey(generation)
				old, oldFound, readErr := pin.Get(ctx, key)
				if marshalErr != nil || readErr != nil || oldFound != (generation <= d.segments) {
					err = errors.Join(ErrProjectionCorrupt, marshalErr, readErr)
					break
				}
				if !bytes.Equal(old, raw) {
					rows[n] = authenticatedstate.Row{Key: key, Value: raw}
					n++
				}
				generation++
			}
			if err = errors.Join(err, pin.Close()); err != nil {
				return err
			}
			if n == 0 {
				continue
			}
			err = d.physicalCatchupUpdateLocked(ctx, func(ctx context.Context) error {
				_, err := d.store.UpdateDerived(ctx, authenticatedDirectoryCut(d.scope, d.anchor), func(tx *authenticatedstate.Transaction) error {
					for _, row := range rows[:n] {
						if err := tx.Put(row.Key, row.Value); err != nil {
							return err
						}
					}
					return nil
				}, check)
				return err
			})
			if err != nil {
				return err
			}
			written = true
			changed += uint64(n)
		}
		if !written && (anchor == d.anchor || deferSameGenerationScalar && anchor.Head.Generation == d.anchor.Head.Generation) {
			complete = true
			return check()
		}
		state, err := json.Marshal(authenticatedDirectoryState{Version: 1, Anchor: anchor, Segments: anchor.Head.Generation - 1})
		if err != nil {
			return err
		}
		apply := func(tx *authenticatedstate.Transaction) error { return tx.Put([]byte{0}, state) }
		if err := check(); err != nil {
			return err
		}
		cut := authenticatedDirectoryCut(d.scope, anchor)
		err = d.physicalCatchupUpdateLocked(ctx, func(ctx context.Context) error {
			if anchor == d.anchor {
				_, err = d.store.UpdateDerived(ctx, cut, apply, check)
			} else {
				_, err = d.store.Update(ctx, cut, apply)
			}
			return err
		})
		if err != nil {
			return err
		}
		if err := check(); err != nil {
			written = true
			return err
		}
		complete = true
		d.anchor, d.segments = anchor, anchor.Head.Generation-1
		if d.physical != nil {
			physical, ok := source.(AuthenticatedHAPhysicalUsageSource)
			if !ok {
				return ErrProjectionSource
			}
			reference, err := physical.HAPhysicalNativeReference(ctx)
			var end int64
			if err == nil {
				end, err = physical.HAPhysicalNativePreparedEnd(ctx)
			}
			if err == nil {
				err = d.store.RecordPhysicalNativeRange(reference, end)
			}
			if err != nil {
				d.sealErr = errors.Join(d.sealErr, err)
				return err
			}
		}
		return nil
	})
	if resultErr != nil {
		changed = 0
		resultErr = errors.Join(resultErr, d.sealErr)
	}
	return
}
