package governance

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/nativephysical"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replicationlimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// Coverage is privately installed only by the actual selected native/projection
// pair. All counters are guarded independently of projection.mu: actual native
// callbacks already hold native.mu and must not invert the reducer lock order.
type haPhysicalGovernanceProjection struct {
	mu                 sync.Mutex
	projection         *GovernanceProjection
	native             *Log
	store              *authenticatedstate.Store
	work               *resourcebudget.CompletableWork
	window             *authenticatedstate.PhysicalTerminalWindow
	remaining, pending int
	applied            Summary
	selection          [32]byte
	failure            error
	closedQualified    bool
}

// TypedProjectionResidentRAMBytes covers the selected projection's lifetime,
// independently of the original physical completion pool.
const TypedProjectionResidentRAMBytes int64 = governanceSealWorkspaceRAMBytes

// HAPhysicalProjectionWorkspaceRequirements describes the actual original
// completion pool; it is not a permission to open or mutate a selected root.
func HAPhysicalProjectionWorkspaceRequirements() []resourcebudget.WorkspaceRequirement {
	return []resourcebudget.WorkspaceRequirement{
		{Class: resourcebudget.IO, RAM: replicationlimits.MemberAuthorityRAMBytes},
		{Class: resourcebudget.IO, RAM: replicationlimits.OrderingFrozenReadRAMBytes},
		{Class: resourcebudget.IO, RAM: governanceSealWorkspaceRAMBytes},
		{Class: resourcebudget.IO, RAM: authenticatedstate.ReadRAMBytes},
		{Class: resourcebudget.IO, RAM: 4096},
		{Class: resourcebudget.IO, RAM: 4096},
		{Class: resourcebudget.IO, RAM: 4096},
		{Class: resourcebudget.Background, RAM: authenticatedstate.UpdateRAMBytes},
		{Class: resourcebudget.Background, RAM: 4096},
	}
}

func (q *haPhysicalGovernanceProjection) acquireWork() (err error) {
	requirements := HAPhysicalProjectionWorkspaceRequirements()
	var ram int64
	for _, r := range requirements {
		ram += r.RAM
	}
	q.work, err = q.projection.cowBudget.TryAcquireCompletableWork(resourcebudget.IO, resourcebudget.Cost{RAM: 4096}, resourcebudget.Cost{RAM: ram}, requirements)
	return err
}

func physicalGovReference(r nativephysical.Reference) authenticatedstate.PhysicalNativeReference {
	return authenticatedstate.PhysicalNativeReference{File: r.Native, Parent: r.Parent, Generation: 1, Sequence: r.Head.Sequence, Hash: r.Head.Hash, EOF: r.Head.EOF}
}
func (n *Log) verifyPhysicalGovernancePrefixLocked(ctx context.Context, prior authenticatedstate.PhysicalNativeReference, end int64) error {
	if n.physical == nil || prior.Generation != 1 {
		return ErrStartupCache
	}
	actual, err := n.physical.Reference(n.physicalHeadLocked())
	if err != nil || end < prior.EOF || end > actual.End {
		return errors.Join(ErrStartupCache, err)
	}
	// The native receipt independently owns its complete range. A first COW
	// receipt initially records EOF; it cannot erase that actual native range.
	return n.physical.VerifyReference(ctx, n.physicalHeadLocked(), nativephysical.Reference{Native: prior.File, Parent: prior.Parent, Head: nativephysical.Position{Sequence: prior.Sequence, Hash: prior.Hash, EOF: prior.EOF}, End: actual.End})
}

// EnableHAPhysicalProjection qualifies the exact selected root and actual
// role-permitted prefix before preparing finite COW targets. Both sources must
// be caught up to the original native descriptor; received tails alone do not
// qualify retirement, NoDebt, or writable root selection.
func (p *GovernanceProjection) EnableHAPhysicalProjection(ctx context.Context, count int) error {
	if p == nil || count < 1 || count > resourcebudget.MaxCompletionBatch || p.cowNative == nil {
		return ErrStartupCache
	}
	n := p.cowNative
	n.mu.Lock()
	var h *haSelectedStartupProvenance
	if n.startupProvenance != nil {
		h = n.startupProvenance.ha
	}
	n.mu.Unlock()
	if h == nil {
		return ErrStartupCache
	}
	return h.withScope(ctx, func(ctx context.Context, a authenticatedstate.SealEligibility, allowed HASelectedNativePosition) error {
		n.mu.Lock()
		defer n.mu.Unlock()
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.retired.Load() || !p.ready || !p.selectedStartup || p.cow == nil || p.cowClosed || p.cowBudget != n.cacheBudget || n.physical == nil || n.closing || n.terminalErr != nil || p.physical != nil || n.physicalProjection != nil || n.startupProvenance.ha != h || !n.selectedProjectionMode(ctx, a, true) {
			return ErrStartupCache
		}
		full := Summary{Records: n.sequence, Bytes: n.offset, LastHash: n.lastHash}
		if full.Records != allowed.Sequence || full.LastHash != allowed.Hash {
			return ErrUnavailable
		}
		var head Summary
		if err := p.viewRows(ctx, func(_ any, v Summary) error { head = v; return nil }); err != nil {
			return err
		}
		if head != full {
			return ErrUnavailable
		}
		provenance := n.startupProvenance
		seal, found, err := provenance.publisher.SelectContext(ctx, a)
		if err != nil || !found {
			return errors.Join(ErrStartupCache, err)
		}
		root, err := p.cow.SealRootReference(ctx, typedGovernanceSchema())
		if err != nil {
			return err
		}
		selected := false
		for _, r := range seal.Roots[:seal.RootCount] {
			selected = selected || r == root
		}
		if !selected {
			return authenticatedstate.ErrSealIneligible
		}
		raw, err := json.Marshal(seal)
		if err != nil {
			return err
		}
		selection := sha256.Sum256(raw)
		reference, err := n.physical.Reference(n.physicalHeadLocked())
		if err != nil {
			return err
		}
		q := &haPhysicalGovernanceProjection{projection: p, native: n, store: p.cow, applied: head, selection: selection, closedQualified: true}
		p.physical = q // every actual fee/candidate remains reachable on failure
		err = q.acquireWork()
		if err != nil {
			return err
		}
		if _, err := q.store.EnablePhysicalTerminalQualified(ctx, selection, physicalGovReference(reference), func(prior authenticatedstate.PhysicalNativeReference, end int64) error {
			return n.verifyPhysicalGovernancePrefixLocked(ctx, prior, end)
		}); err != nil {
			q.failure = err
			return err
		}
		if err := q.store.RecordPhysicalNativeRange(physicalGovReference(reference), reference.End); err != nil {
			q.failure = err
			return err
		}
		q.window, err = q.store.ReservePhysicalTerminals(ctx, count)
		if err != nil {
			q.failure = err
			return err
		}
		q.remaining = count
		n.physicalProjection = q
		return nil
	})
}

// PrepareHAPhysicalProjection also handles a selected factory that restored
// the original physical owner. Reuse first joins its unused targets through
// the actual current common root and full allowed native prefix. A receipt,
// copied head, or repeated Enable call cannot replace that qualification.
func (p *GovernanceProjection) PrepareHAPhysicalProjection(ctx context.Context, count int) error {
	if p == nil || count < 1 || count > resourcebudget.MaxCompletionBatch {
		return ErrStartupCache
	}
	p.mu.RLock()
	q := p.physical
	p.mu.RUnlock()
	if q == nil {
		return p.EnableHAPhysicalProjection(ctx, count)
	}
	return p.withPhysicalProjectionWork(ctx, func(ctx context.Context) error {
		return p.ReplenishHAPhysicalProjection(ctx, count)
	})
}

// Called by the actual selected factory before its first suffix mutation.
// A receipt can name an original inode, but cannot authorize a candidate prefix
// beyond the independently permitted source head supplied by that factory.
func (p *GovernanceProjection) restoreHAPhysicalProjection(ctx context.Context, seal authenticatedstate.CandidateSeal, saved, full, permitted Summary) (Summary, error) {
	n := p.cowNative
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.physical == nil || n.physicalProjection != nil || n.startupProvenance == nil || n.startupProvenance.ha == nil || !n.selectedProjectionMode(ctx, n.startupProvenance.ha.initialAuthority, true) {
		return saved, ErrStartupCache
	}
	raw, err := json.Marshal(seal)
	if err != nil {
		return saved, err
	}
	selection := sha256.Sum256(raw)
	reference, err := n.physical.Reference(n.physicalHeadLocked())
	if err != nil {
		return saved, err
	}
	native := physicalGovReference(reference)
	projected := saved
	cut, found, err := p.cow.WithPhysicalRecoveryRoot(ctx, selection, native, func(pin *authenticatedstate.Snapshot, comparison [32]byte, prior authenticatedstate.PhysicalNativeReference) ([32]byte, error) {
		if err := n.verifyPhysicalGovernancePrefixLocked(ctx, prior, reference.End); err != nil {
			return [32]byte{}, err
		}
		candidate, err := govPosition(&govRows{ctx: ctx, snapshot: pin})
		if err != nil || candidate.Records < saved.Records || candidate.Bytes < saved.Bytes || candidate.Records > permitted.Records || candidate.Bytes > permitted.Bytes {
			return [32]byte{}, errors.Join(ErrCorrupt, err)
		}
		actual, partial, err := scanFrom(io.NewSectionReader(n.file, 0, candidate.Bytes), n.key, Summary{}, func(Record) error { return ctx.Err() })
		if err != nil || partial || actual != candidate {
			return [32]byte{}, errors.Join(ErrCorrupt, err)
		}
		projected = candidate
		return typedGovernanceCut(candidate), nil
	})
	qualifiedNoDebt := false
	if errors.Is(err, authenticatedstate.ErrSealIneligible) || errors.Is(err, authenticatedstate.ErrPhysicalReceiptRequiresNoDebt) {
		// The selected factory has already verified the exact common root. Only
		// a completely equal, independently permitted native prefix may retire
		// preparation from an older common selection.
		if saved != full || permitted != full {
			return saved, err
		}
		_, err = p.cow.EnablePhysicalTerminalAfterQualifiedNoDebt(ctx, selection, native, typedGovernanceCut(saved), func(prior authenticatedstate.PhysicalNativeReference, end int64) error {
			return n.verifyPhysicalGovernancePrefixLocked(ctx, prior, end)
		})
		qualifiedNoDebt = err == nil
	}
	if err != nil {
		return saved, err
	}
	if !found && !qualifiedNoDebt {
		_, _, _, err = p.cow.RestorePhysicalTerminalForRecovery(ctx, selection, native, typedGovernanceCut(saved), func(prior authenticatedstate.PhysicalNativeReference, end int64) error {
			return n.verifyPhysicalGovernancePrefixLocked(ctx, prior, end)
		})
		return saved, err
	}
	q := &haPhysicalGovernanceProjection{projection: p, native: n, store: p.cow, applied: projected, selection: selection, closedQualified: projected == saved && saved == full}
	p.physical = q
	if err = q.acquireWork(); err != nil {
		q.failure = err
		return saved, err
	}
	if found {
		var restored bool
		q.window, _, restored, err = p.cow.RestorePhysicalTerminalForRecovery(ctx, selection, native, cut, func(prior authenticatedstate.PhysicalNativeReference, end int64) error {
			return n.verifyPhysicalGovernancePrefixLocked(ctx, prior, end)
		})
		if err != nil || !restored {
			q.failure = errors.Join(ErrStartupCache, err)
			return saved, q.failure
		}
		q.remaining = q.window.Count()
		pending := full.Records - projected.Records
		if pending > uint64(q.remaining) {
			q.failure = authenticatedstate.ErrRecovery
			return saved, q.failure
		}
		q.pending = int(pending)
	}
	n.physicalProjection = q
	return projected, nil
}

// This is called only after the actual source producer's independent admission
// and authentication, before its first native byte. A physical counter cannot
// replace the coordinator's role, Ordering or unconfirmed-byte permission.
func (q *haPhysicalGovernanceProjection) reserveNativeLocked(count int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.failure != nil {
		return errors.Join(ErrUnavailable, q.failure)
	}
	if count < 1 || count > q.remaining-q.pending || q.native.sequence != q.applied.Records+uint64(q.pending) {
		return resourcebudget.ErrExhausted
	}
	q.pending += count
	q.closedQualified = false
	return nil
}
func (q *haPhysicalGovernanceProjection) rollbackNativeLocked(count int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending -= count
}

func (p *GovernanceProjection) withPhysicalProjectionWork(ctx context.Context, visit func(context.Context) error) error {
	p.mu.RLock()
	q := p.physical
	p.mu.RUnlock()
	if q == nil {
		return visit(ctx)
	}
	if q.work == nil {
		return ErrUnavailable
	}
	// Both original pools stay charged. Masking a caller's workspace changes no
	// context domain/cancellation or source authority, and creates no new fee.
	return q.work.WithWorkspace(resourcebudget.WithoutRAMWorkspace(ctx), visit)
}

// Actual native authentication and independently permitted confirmed reduction
// precede this callback. One bounded reducer batch consumes one prepared root;
// its exact contiguous source record count retires the pending frame counter.
func (p *GovernanceProjection) preparePhysicalGovernanceUpdate(ctx context.Context, records []Record) (context.Context, func(error) error, error) {
	q := p.physical
	if q == nil {
		return ctx, func(error) error { return nil }, nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.failure != nil || q.window == nil || len(records) == 0 || q.pending < len(records) || q.remaining < 1 || records[0].Sequence != q.applied.Records+1 {
		return ctx, nil, errors.Join(ErrUnavailable, q.failure)
	}
	for i := 1; i < len(records); i++ {
		if records[i].Sequence != records[i-1].Sequence+1 {
			return ctx, nil, ErrCorrupt
		}
	}
	u, err := q.store.PreparePhysicalUpdate(ctx, q.window)
	if err != nil {
		return ctx, nil, err
	}
	last := records[len(records)-1]
	return u.Bind(ctx), func(cause error) error {
		q.mu.Lock()
		defer q.mu.Unlock()
		if err := u.Finish(true, cause); err != nil {
			q.failure = errors.Join(q.failure, ownedfile.ErrRetirement, err)
			return q.failure
		}
		q.pending -= len(records)
		q.remaining--
		q.applied = Summary{Records: last.Sequence, Bytes: last.Offset, LastHash: last.Hash}
		if q.remaining == 0 {
			if err := q.window.Close(); err != nil {
				q.failure = err
				return err
			}
			q.window = nil
		}
		return nil
	}, nil
}

// RetireHAPhysicalProjectionAfterCommon requires the exact newly selected root,
// full allowed native prefix and zero pending frames. It preserves controls;
// neither native equality nor an unauthenticated receipt grants retirement.
func (p *GovernanceProjection) RetireHAPhysicalProjectionAfterCommon(ctx context.Context) error {
	if p == nil || p.cowNative == nil {
		return ErrStartupCache
	}
	n := p.cowNative
	n.mu.Lock()
	var h *haSelectedStartupProvenance
	if n.startupProvenance != nil {
		h = n.startupProvenance.ha
	}
	n.mu.Unlock()
	if h == nil {
		return ErrStartupCache
	}
	return h.withScope(ctx, func(ctx context.Context, a authenticatedstate.SealEligibility, allowed HASelectedNativePosition) error {
		n.mu.Lock()
		defer n.mu.Unlock()
		p.mu.Lock()
		defer p.mu.Unlock()
		q := p.physical
		if q == nil {
			return nil
		}
		q.mu.Lock()
		defer q.mu.Unlock()
		if q.projection != p || q.native != n || q.store != p.cow || n.physicalProjection != q || p.cowBudget != n.cacheBudget || n.startupProvenance == nil || n.startupProvenance.ha != h || !n.selectedProjectionMode(ctx, a, true) || q.failure != nil || q.pending != 0 || n.physical == nil || n.terminalErr != nil || n.closing || p.retired.Load() {
			return ErrUnavailable
		}
		full := Summary{Records: n.sequence, Bytes: n.offset, LastHash: n.lastHash}
		if q.applied != full || full.Records != allowed.Sequence || full.LastHash != allowed.Hash {
			return ErrUnavailable
		}
		var actual Summary
		if err := p.viewRows(ctx, func(_ any, v Summary) error { actual = v; return nil }); err != nil {
			return err
		}
		if actual != full {
			return ErrCorrupt
		}
		provenance := n.startupProvenance
		seal, found, err := provenance.publisher.SelectContext(ctx, a)
		if err != nil || !found {
			return errors.Join(ErrStartupCache, err)
		}
		root, err := p.cow.SealRootReference(ctx, typedGovernanceSchema())
		if err != nil {
			return err
		}
		selected := false
		for _, r := range seal.Roots[:seal.RootCount] {
			selected = selected || r == root
		}
		if !selected {
			return authenticatedstate.ErrSealIneligible
		}
		if q.window != nil {
			if err := q.window.Close(); err != nil {
				q.failure = err
				return err
			}
			q.window = nil
		}
		q.remaining = 0
		if err := q.store.RetirePhysicalPreparationKeepingControls(ctx); err != nil {
			q.failure = err
			return err
		}
		raw, err := json.Marshal(seal)
		if err != nil {
			return err
		}
		selection := sha256.Sum256(raw)
		reference, err := n.physical.Reference(n.physicalHeadLocked())
		if err != nil {
			return err
		}
		if err := q.store.BindPhysicalSelectionDigest(selection, physicalGovReference(reference)); err != nil {
			q.failure = err
			return err
		}
		if err := n.physical.Checkpoint(ctx, n.physicalHeadLocked()); err != nil {
			q.failure = err
			return err
		}
		reference, err = n.physical.Reference(n.physicalHeadLocked())
		if err != nil {
			q.failure = err
			return err
		}
		if err := q.store.RecordPhysicalNativeRange(physicalGovReference(reference), reference.End); err != nil {
			q.failure = err
			return err
		}
		q.selection = selection
		q.closedQualified = true
		return nil
	})
}

// ReplenishHAPhysicalProjection is ordinary admission preparation after exact
// common selection and zero pending native frames. It reuses the original
// control/work owners, allocates no new ceiling and never runs under pressure.
func (p *GovernanceProjection) ReplenishHAPhysicalProjection(ctx context.Context, count int) error {
	if p == nil || count < 1 || count > resourcebudget.MaxCompletionBatch || p.cowNative == nil {
		return ErrStartupCache
	}
	if err := p.RetireHAPhysicalProjectionAfterCommon(ctx); err != nil {
		return err
	}
	n := p.cowNative
	n.mu.Lock()
	var h *haSelectedStartupProvenance
	if n.startupProvenance != nil {
		h = n.startupProvenance.ha
	}
	n.mu.Unlock()
	if h == nil {
		return ErrStartupCache
	}
	return h.withScope(ctx, func(ctx context.Context, a authenticatedstate.SealEligibility, allowed HASelectedNativePosition) error {
		n.mu.Lock()
		defer n.mu.Unlock()
		p.mu.Lock()
		defer p.mu.Unlock()
		q := p.physical
		if q == nil || q.projection != p || q.native != n || q.store != p.cow || n.physicalProjection != q || p.cowBudget != n.cacheBudget || n.startupProvenance == nil || n.startupProvenance.ha != h || n.physical == nil || n.closing || n.terminalErr != nil || p.retired.Load() || p.cowClosed || p.cowBudget.Snapshot().Pressure != resourcebudget.Healthy || !n.selectedProjectionMode(ctx, a, true) {
			return ErrUnavailable
		}
		q.mu.Lock()
		defer q.mu.Unlock()
		if q.failure != nil || !q.closedQualified || q.pending != 0 || q.window != nil || q.remaining != 0 || q.applied.Records != n.sequence || q.applied.LastHash != n.lastHash || allowed.Sequence != n.sequence || allowed.Hash != n.lastHash {
			return ErrUnavailable
		}
		seal, found, err := n.startupProvenance.publisher.SelectContext(ctx, a)
		if err != nil || !found {
			return errors.Join(ErrStartupCache, err)
		}
		raw, err := json.Marshal(seal)
		if err != nil {
			return err
		}
		if sha256.Sum256(raw) != q.selection {
			return authenticatedstate.ErrSealIneligible
		}
		reference, err := n.physical.Reference(n.physicalHeadLocked())
		if err != nil {
			return err
		}
		foundPreparation, err := q.store.ValidatePhysicalTerminalPreparation(ctx, q.selection, physicalGovReference(reference), func(prior authenticatedstate.PhysicalNativeReference, end int64) error {
			return n.verifyPhysicalGovernancePrefixLocked(ctx, prior, end)
		})
		if err != nil || !foundPreparation {
			return errors.Join(authenticatedstate.ErrRecovery, err)
		}
		if err = n.physical.Replenish(ctx, n.physicalHeadLocked()); err != nil {
			q.failure = err
			return err
		}
		reference, err = n.physical.Reference(n.physicalHeadLocked())
		if err != nil {
			q.failure = err
			return err
		}
		if err = q.store.RecordPhysicalNativeRange(physicalGovReference(reference), reference.End); err != nil {
			q.failure = err
			return err
		}
		q.window, err = q.store.ReservePhysicalTerminals(ctx, count)
		if err != nil {
			return err
		}
		q.remaining = count
		return nil
	})
}
