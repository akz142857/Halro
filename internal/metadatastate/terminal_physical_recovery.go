package metadatastate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func (b *Baseline) restorePhysicalTerminals(ctx context.Context, seal authenticatedstate.CandidateSeal) (*authenticatedstate.PhysicalTerminalWindow, error) {
	raw, err := json.Marshal(seal)
	if err != nil {
		return nil, err
	}
	selection := sha256.Sum256(raw)
	var restored *authenticatedstate.PhysicalTerminalWindow
	err = b.withTerminalPhysicalRecoveryNativeLocked(ctx, func(q *metadatajournal.PhysicalNativeLoan) error {
		native, err := q.Reference()
		if err != nil {
			return err
		}
		var projected metadatajournal.Head
		cut, found, err := b.store.WithPhysicalRecoveryRoot(ctx, selection, native, func(pin *authenticatedstate.Snapshot, candidateCut [32]byte, recorded authenticatedstate.PhysicalNativeReference) ([32]byte, error) {
			if err := q.AuthenticatePrefix(ctx, recorded); err != nil {
				return [32]byte{}, err
			}
			var epoch, sequence uint64
			for _, e := range []struct {
				name string
				out  *uint64
			}{{"metadata_journal_epoch", &epoch}, {"applied_journal_sequence", &sequence}} {
				key, err := RowKey([][]byte{[]byte("meta")}, []byte(e.name), false)
				if err != nil {
					return [32]byte{}, err
				}
				v, ok, err := pin.Get(ctx, key)
				if err != nil || !ok || len(v) != 8 {
					return [32]byte{}, errors.Join(ErrSource, err)
				}
				*e.out = decodeMetadataSealUint(v)
			}
			if epoch != b.position.Epoch || sequence < b.position.Sequence || sequence > native.Sequence {
				return [32]byte{}, ErrSource
			}
			if s := b.haPhysicalSource; s != nil && (epoch != s.recoveryPermitted.Epoch || sequence > s.recoveryPermitted.Sequence) {
				return [32]byte{}, ErrSource
			}
			var record metadatajournal.Record
			projected, record, err = q.HeadAt(ctx, sequence)
			if err != nil {
				return [32]byte{}, err
			}
			if sequence == 0 {
				return b.position.Baseline.Cut, nil
			}
			return deltaCut(projected, record), nil
		})
		if err != nil {
			return err
		}
		if !found {
			// Strict absence still checks the actual tail/pending namespace and
			// clears only this selected owner's deferred physical-open gate.
			root, err := b.store.RootIdentity(ctx)
			if err != nil {
				return err
			}
			_, _, _, err = b.store.RestorePhysicalTerminalForRecovery(ctx, selection, native, root.Cut, func(prior authenticatedstate.PhysicalNativeReference, end int64) error {
				return q.VerifyPrefix(ctx, prior, end)
			})
			return err
		}
		var actualFound bool
		restored, _, actualFound, err = b.store.RestorePhysicalTerminalForRecovery(ctx, selection, native, cut, func(prior authenticatedstate.PhysicalNativeReference, end int64) error {
			return q.VerifyPrefix(ctx, prior, end)
		})
		if err != nil || !actualFound {
			return errors.Join(ErrSource, err)
		}
		b.position = projected
		b.physicalEnabled = true
		b.physicalDebt = restored.Count()
		pending := native.Sequence - projected.Sequence
		if pending > uint64(b.physicalDebt) {
			return authenticatedstate.ErrRecovery
		}
		// Already durable suffix facts need only their original COW targets;
		// they must not be counted again as future native append bytes.
		if err := q.VerifyFutureCapacity(b.physicalDebt - int(pending)); err != nil {
			return err
		}
		return q.Enable()
	})
	return restored, err
}

// RetirePhysicalPreparation requires the same actual post-publication gate.
func (b *Baseline) RetirePhysicalPreparation(ctx context.Context) error {
	return b.RetirePhysicalPreparationAfterPinRecovery(ctx)
}

func (b *Baseline) restorePhysicalReplay(ctx context.Context, window *authenticatedstate.PhysicalTerminalWindow, record metadatajournal.Record, visit func(context.Context) error) (err error) {
	if !b.physicalEnabled {
		return visit(ctx)
	}
	if window == nil || b.physicalDebt <= 0 {
		return resourcebudget.ErrPhysicalAllocation
	}
	u, err := b.store.PreparePhysicalUpdate(ctx, window)
	if err != nil {
		return err
	}
	// Every suffix frame is already a fully authenticated durable native fact.
	defer func() {
		err = errors.Join(err, u.Finish(true, err))
		if err == nil {
			b.physicalDebt--
		}
	}()
	return visit(u.Bind(ctx))
}

// RetirePhysicalPreparationAfterPinRecovery is a joined maintenance step after
// the actual Ledger NoDebt check and seven-root publication. It additionally
// proves this Metadata owner is exactly in the publisher's current selection.
func (b *Baseline) RetirePhysicalPreparationAfterPinRecovery(ctx context.Context) error {
	if b == nil || !b.mu.TryLock() {
		return authenticatedstate.ErrBusy
	}
	defer b.mu.Unlock()
	if !b.physicalEnabled {
		return nil
	}
	if !b.ready.Load() {
		return authenticatedstate.ErrRecovery
	}
	selected, found, err := b.selectedPublisher.CurrentContext(ctx)
	if err != nil || !found || selected.Role != authenticatedstate.SealStandalone {
		return errors.Join(ErrSource, err)
	}
	actual, err := b.sealReferenceLocked(ctx)
	if err != nil {
		return err
	}
	rootFound, positionFound := false, false
	for _, r := range selected.Roots[:selected.RootCount] {
		if r == actual.Root {
			rootFound = true
		}
	}
	for _, p := range selected.Positions[:selected.PositionCount] {
		if p == actual.Position {
			positionFound = true
		}
	}
	if !rootFound || !positionFound {
		return ErrSource
	}
	if b.physicalRecovery != nil {
		if !b.physicalPinRecoveryVerified {
			return authenticatedstate.ErrRecovery
		}
		if err := b.physicalRecovery.closeLocked(); err != nil {
			return err
		}
		b.physicalRecovery = nil
	}
	if b.physicalDebt != 0 {
		return authenticatedstate.ErrRecovery
	}
	if err := b.store.RetirePhysicalPreparationKeepingControls(ctx); err != nil {
		return err
	}
	return b.journal.WithPhysicalNative(ctx, b.budget, func(q *metadatajournal.PhysicalNativeLoan) error { return q.RetireUnused() })
}
