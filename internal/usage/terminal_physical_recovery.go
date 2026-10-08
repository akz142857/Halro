package usage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
)

func decodePhysicalUsageState(raw []byte) (diskUsageState, error) {
	var state diskUsageState
	if len(raw) > diskUsageMaxValue || json.Unmarshal(raw, &state) != nil || state.Version != diskUsageVersion || state.Watermark.Generation == 0 || state.Watermark.Offset < 0 {
		return state, ErrDiskUsageCorrupt
	}
	return state, nil
}

// Only the original selected source loan can verify the receipt-bound scalar
// and actual native MAC prefix before a restored physical root becomes writable.
func (p *DiskProjection) restorePhysicalUsageLocked(ctx context.Context, source ledger.SelectedStartupSource, publisher *authenticatedstate.SealCandidatePublisher, noDebt *ledger.ReplayAnchor) (ledger.ReplayAnchor, bool, error) {
	s, ok := source.(ledger.SelectedPhysicalStartupSource)
	if p.engine != nil && p.engine.selectedHA {
		ha, found := source.(ledger.SelectedHAPhysicalStartupSource)
		ok = found
		if found {
			s = haPhysicalStartupUsageSourceAdapter{ha}
		}
	}
	if !ok || p.engine == nil {
		return ledger.ReplayAnchor{}, false, authenticatedstate.ErrRecovery
	}
	raw, err := json.Marshal(source.SelectedSeal())
	if err != nil {
		return ledger.ReplayAnchor{}, false, err
	}
	selection := sha256.Sum256(raw)
	native, err := s.PhysicalNativeReference(ctx)
	if err != nil {
		return ledger.ReplayAnchor{}, false, err
	}
	if p.engine.selectedHA {
		allowed := source.NativeHead()
		if native.Sequence != allowed.Head.Sequence || native.Generation != allowed.Head.Generation || native.Hash != allowed.Hash {
			// Empty Ledger recovery indexes at Applied do not prove an authentic
			// received future suffix is debt-free. Preserve its original targets.
			noDebt = nil
		}
	}
	saved := source.SavedAnchor()
	anchor := saved
	cut, found, err := p.engine.cow.WithPhysicalRecoveryRoot(ctx, selection, native, func(pin *authenticatedstate.Snapshot, _ [32]byte, recorded authenticatedstate.PhysicalNativeReference) ([32]byte, error) {
		end, err := s.PhysicalNativePreparedEnd(ctx)
		if err != nil {
			return [32]byte{}, err
		}
		key, err := typedUsageKey("meta", []byte("state"))
		if err != nil {
			return [32]byte{}, err
		}
		raw, exists, err := pin.Get(ctx, key)
		if err != nil || !exists {
			return [32]byte{}, errors.Join(ErrDiskUsageCorrupt, err)
		}
		state, err := decodePhysicalUsageState(raw)
		if err != nil {
			return [32]byte{}, err
		}
		anchor = usageStateAnchor(state)
		if anchor.Head.Sequence < saved.Head.Sequence || anchor.Head.Generation < saved.Head.Generation || anchor.Head.After(source.NativeHead().Head) || anchor.Head.Sequence > source.NativeHead().Head.Sequence {
			return [32]byte{}, authenticatedstate.ErrRecovery
		}
		if err := source.VerifyAnchors(ctx, [2]ledger.ReplayAnchor{anchor, source.NativeHead()}); err != nil {
			return [32]byte{}, err
		}
		if err := s.VerifyPhysicalNativePrefix(ctx, recorded, end); err != nil {
			return [32]byte{}, err
		}
		return typedUsageCut(anchor), nil
	})
	if errors.Is(err, authenticatedstate.ErrSealIneligible) || errors.Is(err, authenticatedstate.ErrPhysicalReceiptRequiresNoDebt) {
		actual, readErr := physicalUsageState(ctx, p.engine.cow)
		if readErr != nil || noDebt == nil || *noDebt != source.NativeHead() || actual != saved || actual != source.NativeHead() {
			return ledger.ReplayAnchor{}, false, errors.Join(authenticatedstate.ErrRecovery, err, readErr)
		}
		if _, err := p.engine.cow.EnablePhysicalTerminalAfterQualifiedNoDebt(ctx, selection, native, typedUsageCut(actual), func(prior authenticatedstate.PhysicalNativeReference, end int64) error {
			return s.VerifyPhysicalNativePrefix(ctx, prior, end)
		}); err != nil {
			return ledger.ReplayAnchor{}, false, err
		}
		q := &typedPhysicalUsageOwner{native: p.engine.native, ledger: p.engine.owner, store: p.engine.cow, manager: p.engine.budget, publisher: publisher, selection: selection, applied: actual, ha: p.engine.selectedHA}
		p.physical = q
		if err := q.acquireWork(ctx); err != nil {
			return ledger.ReplayAnchor{}, false, err
		}
		return actual, true, source.Check(ctx)
	}
	if err != nil {
		return ledger.ReplayAnchor{}, false, err
	}
	if !found {
		cut = typedUsageCut(saved)
	}
	window, _, restored, err := p.engine.cow.RestorePhysicalTerminalForRecovery(ctx, selection, native, cut, func(prior authenticatedstate.PhysicalNativeReference, end int64) error {
		return s.VerifyPhysicalNativePrefix(ctx, prior, end)
	})
	if err != nil || restored != found {
		return ledger.ReplayAnchor{}, false, errors.Join(authenticatedstate.ErrRecovery, err)
	}
	if !found {
		return saved, false, nil
	}
	q := &typedPhysicalUsageOwner{native: p.engine.native, ledger: p.engine.owner, store: p.engine.cow, manager: p.engine.budget, publisher: publisher, selection: selection, applied: anchor, ha: p.engine.selectedHA}
	q.windows[0] = window
	q.counts[0] = window.Count()
	q.remaining = window.Count()
	p.physical = q
	if window.Count() == 0 {
		// Restore registered a real but empty borrowed window. Join that exact
		// owner; this touches no slot, extent, receipt or fee and grants no
		// NoDebt permission for a received suffix beyond independent Allowed.
		if err := window.Close(); err != nil {
			q.failure = err
			return ledger.ReplayAnchor{}, false, err
		}
		q.windows[0] = nil
	}
	if err := q.acquireWork(ctx); err != nil {
		return ledger.ReplayAnchor{}, false, err
	}
	return anchor, true, source.Check(ctx)
}
