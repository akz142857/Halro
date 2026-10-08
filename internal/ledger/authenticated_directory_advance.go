package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// AdvanceFromNative refreshes the complete directory at the actual, already
// durable Ledger head. Existing compressed descriptors are authenticated again
// too: changing only the active anchor would preserve stale physical locators.
// Source verification reads history with fixed workspace; the COW update is
// limited to 63 descriptor mutations per page and a final state row. The
// common selection changes only after all pages qualify; an interrupted live
// page sequence withdraws this owner until selected startup authenticates it.
var ErrDirectoryPartialAdvance = errors.New("Ledger directory maintenance has unpublished partial pages")

func (d *AuthenticatedDirectory) AdvanceFromNative(ctx context.Context, native *Log, lease *MaintenanceLease, owner *AuthenticatedLedgerProjection) (uint64, error) {
	return d.advanceFromNative(ctx, native, lease, owner, nil)
}

func (d *AuthenticatedDirectory) advanceFromNative(ctx context.Context, native *Log, lease *MaintenanceLease, owner *AuthenticatedLedgerProjection, phase func(string) error) (changed uint64, resultErr error) {
	if d == nil || d.store == nil || native == nil || lease == nil || owner == nil {
		return 0, ErrProjectionSource
	}
	if _, err := lease.ValidateOwner(native); err != nil {
		return 0, err
	}
	if err := owner.ValidateNativeSource(ctx, native, lease); err != nil {
		return 0, err
	}
	if err := owner.ValidateResourceOwner(ctx, d.budget); err != nil {
		return 0, err
	}
	if !d.mu.TryLock() {
		return 0, authenticatedstate.ErrBusy
	}
	defer d.mu.Unlock()
	if err := errors.Join(ctx.Err(), d.closeCtx.Err(), d.sealErr); err != nil {
		return 0, err
	}
	if p := d.physical; p != nil {
		if p.work == nil {
			return 0, authenticatedstate.ErrRecovery
		}
		resultErr = p.work.WithWorkspace(ctx, func(ctx context.Context) error {
			var err error
			changed, err = d.advanceFrozenNativeLocked(ctx, native, lease, owner, phase)
			return err
		})
		return changed, resultErr
	}
	return d.advanceFrozenNativeLocked(ctx, native, lease, owner, phase)
}

// Caller holds d.mu and, for a physical vector, the original prepaid workspace.
func (d *AuthenticatedDirectory) advanceFrozenNativeLocked(ctx context.Context, native *Log, lease *MaintenanceLease, owner *AuthenticatedLedgerProjection, phase func(string) error) (changed uint64, resultErr error) {
	credit, err := d.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: AuthenticatedLedgerSourceRAMBytes})
	if err != nil {
		return 0, err
	}
	defer func() {
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			if err := credit.Release(); err == nil {
				return
			} else {
				resultErr = errors.Join(resultErr, err)
			}
		}
		d.sealDebt, d.sealErr = credit, resultErr
	}()
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(d.closeCtx, cancel)
	defer func() { stop(); cancel() }()
	resultErr = lease.ReadFrozenHead(opCtx, func(ctx context.Context, head Watermark, hash [32]byte) error {
		ref, err := owner.SealReference(ctx)
		if err != nil || ref.Scope != d.ledgerScope {
			return errors.Join(ErrProjectionSource, err)
		}
		native.mu.Lock()
		defer native.mu.Unlock()
		anchor := ReplayAnchor{Head: head, Hash: hash, Epoch: native.chainEpoch}
		if ref.Anchor != anchor || native.file == nil || native.segmentDirectory == nil || native.segmentDirectory.parsed.count != head.Generation-1 || head.Generation < d.anchor.Head.Generation || head.Sequence < d.anchor.Head.Sequence {
			return ErrProjectionSource
		}
		seen := false
		observe := func(a ReplayAnchor) { seen = seen || a == d.anchor }
		genesis := d.anchor.Head == (Watermark{Generation: 1}) && d.anchor.Hash == ([32]byte{})
		// An empty keyed writer records its intended epoch before its first
		// frame; the full-history verifier starts at epoch zero. The first
		// authenticated frame must independently support that signed genesis
		// epoch. No nonzero sequence/offset may borrow this exception.
		actual, err := native.replayAuthenticatedLocked(ctx, ReplayAnchor{}, 0, func(r Record) error {
			if genesis && r.Generation == 1 && r.Sequence == 1 && r.Epoch >= d.anchor.Epoch {
				seen = true
			}
			observe(ReplayAnchor{Head: Watermark{Generation: r.Generation, Sequence: r.Sequence, Offset: r.Offset}, Hash: r.Hash, Epoch: r.Epoch})
			return ctx.Err()
		}, func(a ReplayAnchor) error { observe(a); return ctx.Err() })
		if genesis && native.sequence == 0 && native.generation == 1 && native.chainHash == ([32]byte{}) && native.chainEpoch == d.anchor.Epoch {
			seen = true
		}
		if err != nil || actual != head || !seen || native.chainHash != hash {
			return errors.Join(ErrProjectionSource, err)
		}
		pin, err := d.store.Snapshot(ctx)
		if err != nil {
			return err
		}
		// Count before writing any candidate page, preserving the old healthy
		// root when the bounded job cannot fit a physical-directory refresh.
		for generation := uint64(1); generation < head.Generation; generation++ {
			segment, found, readErr := native.lookupSegmentLocked(generation)
			if readErr != nil || !found {
				err = errors.Join(ErrSegmentMissing, readErr)
				break
			}
			raw, marshalErr := json.Marshal(segment)
			old, oldFound, readErr := pin.Get(ctx, authenticatedDirectoryKey(generation))
			if marshalErr != nil || readErr != nil || oldFound != (generation <= d.segments) {
				err = errors.Join(ErrProjectionCorrupt, marshalErr, readErr)
				break
			}
			if !bytes.Equal(raw, old) {
				changed++

			}
		}
		if err = errors.Join(err, pin.Close()); err != nil {
			return err
		}
		if anchor == d.anchor && changed == 0 {
			return ctx.Err()
		}
		if d.physical != nil && (changed+62)/63+1 > uint64(d.physical.remaining) {
			return resourcebudget.ErrExhausted
		}
		written := false
		complete := false
		defer func() {
			if written && !complete {
				d.sealErr = errors.Join(d.sealErr, ErrDirectoryPartialAdvance)
			}
		}()
		for generation := uint64(1); generation < head.Generation; {
			var rows [63]authenticatedstate.Row
			n := 0
			pin, err := d.store.Snapshot(ctx)
			if err != nil {
				return err
			}
			for generation < head.Generation && n < len(rows) {
				segment, found, readErr := native.lookupSegmentLocked(generation)
				if readErr != nil || !found {
					err = errors.Join(ErrSegmentMissing, readErr)
					break
				}
				raw, marshalErr := json.Marshal(segment)
				key := authenticatedDirectoryKey(generation)
				old, _, readErr := pin.Get(ctx, key)
				if marshalErr != nil || readErr != nil {
					err = errors.Join(marshalErr, readErr)
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
				}, func() error { return ctx.Err() })
				return err
			})
			if err != nil {
				return err
			}
			written = true
			if phase != nil {
				if err := phase("directory-page-published"); err != nil {
					return errors.Join(ErrDirectoryPartialAdvance, err)
				}
			}
		}
		state, err := json.Marshal(authenticatedDirectoryState{Version: 1, Anchor: anchor, Segments: head.Generation - 1})
		if err != nil {
			return err
		}
		apply := func(tx *authenticatedstate.Transaction) error { return tx.Put([]byte{0}, state) }
		cut := authenticatedDirectoryCut(d.scope, anchor)
		err = d.physicalCatchupUpdateLocked(ctx, func(ctx context.Context) error {
			if anchor == d.anchor {
				_, err = d.store.UpdateDerived(ctx, cut, apply, func() error { return ctx.Err() })
			} else {
				_, err = d.store.Update(ctx, cut, apply)
			}
			return err
		})
		if err != nil {
			if written {
				return errors.Join(ErrDirectoryPartialAdvance, err)
			}
			return err
		}
		complete = true
		d.anchor, d.segments = anchor, head.Generation-1
		if d.physical != nil {
			reference, end, err := d.physicalSourceLocked(native, owner, d.physical.selection)
			if err != nil {
				d.sealErr = errors.Join(d.sealErr, err)
				return err
			}
			if err := d.store.RecordPhysicalNativeRange(reference, end); err != nil {
				d.sealErr = errors.Join(d.sealErr, err)
				return err
			}
		}
		return ctx.Err()
	})
	if resultErr != nil {
		resultErr = errors.Join(resultErr, d.sealErr)
		changed = 0
	}
	return changed, resultErr
}
