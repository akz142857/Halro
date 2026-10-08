package usage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sync"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// All mutable coverage fields are protected by the original native source
// lane. Native callbacks must never take Usage.mu: a collector already holds
// that mutex while borrowing the same native lane.
type typedPhysicalUsageOwner struct {
	mu              sync.Mutex
	native          *ledger.Log
	ledger          *ledger.AuthenticatedLedgerProjection
	store           *authenticatedstate.Store
	manager         *resourcebudget.Manager
	publisher       *authenticatedstate.SealCandidatePublisher
	selection       [32]byte
	applied         ledger.ReplayAnchor
	windows         [resourcebudget.MaxCompletionBatch]*authenticatedstate.PhysicalTerminalWindow
	counts          [resourcebudget.MaxCompletionBatch]int
	remaining       int
	work            *resourcebudget.CompletableWork
	failure         error
	closedQualified bool
	installed       bool
	ha              bool
}

func (q *typedPhysicalUsageOwner) acquireWork(ctx context.Context) error {
	if q.work != nil {
		// Qualify this original pool independently of a caller's Directory
		// checkpoint loan. The genuine source scope and cancellation survive;
		// no foreign RAM credits are rebound or used by the validation.
		return q.work.WithWorkspace(resourcebudget.WithoutRAMWorkspace(ctx), func(context.Context) error {
			loan, err := q.work.Active.RetainCovering(q.manager, resourcebudget.Cost{RAM: usageSealWorkspaceRAMBytes})
			if err != nil {
				return err
			}
			return loan.Release()
		})
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	requirements := typedLiveWorkspaceRequirements()
	if q.ha {
		requirements = HAPhysicalCatchupWorkspaceRequirements()
	}
	var total int64
	for _, r := range requirements {
		total += r.RAM
	}
	work, err := q.manager.TryAcquireCompletableWork(resourcebudget.IO, resourcebudget.Cost{RAM: PhysicalCatchupActiveRAMBytes}, resourcebudget.Cost{RAM: total}, requirements)
	if err == nil {
		q.work = work
	}
	return err
}

func (p *DiskProjection) withPhysicalSourceLocked(ctx context.Context, visit func(context.Context, ledger.AuthenticatedPhysicalUsageSource) error) error {
	e := p.engine
	if e != nil && e.selectedHA {
		return p.withHAPhysicalSourceLocked(ctx, visit)
	}
	if e == nil || e.cow == nil || e.native == nil || e.owner == nil || e.selectedHA || !e.selectedStartup {
		return authenticatedstate.ErrRecovery
	}
	ref, err := e.owner.ReadUsageSourceReference(ctx)
	if err != nil {
		return err
	}
	callback := func(ctx context.Context, s ledger.AuthenticatedUsageSource) error {
		source, ok := s.(ledger.AuthenticatedPhysicalUsageSource)
		if !ok {
			return authenticatedstate.ErrRecovery
		}
		return visit(ctx, source)
	}
	if e.live {
		return e.owner.WithFrozenUsageSource(ctx, e.native, e.budget, ref.Scope, callback)
	}
	return e.owner.WithSelectedMaintenanceUsageSource(ctx, e.native, e.lease, e.budget, ref.Scope, callback)
}

// EnablePhysicalCatchup captures the actual selected Standalone source before
// admission. The fixed work pool outlives individual HTTP request envelopes;
// each possible native frame gets its own COW target before native acceptance.
func (p *DiskProjection) EnablePhysicalCatchup(ctx context.Context, native *ledger.Log, owner *ledger.AuthenticatedLedgerProjection, selection [32]byte) error {
	return p.enablePhysicalCatchup(ctx, native, owner, nil, selection, false)
}

func (p *DiskProjection) enablePhysicalCatchup(ctx context.Context, native *ledger.Log, owner *ledger.AuthenticatedLedgerProjection, lease *ledger.MaintenanceLease, selection [32]byte, ha bool) error {
	if p == nil || native == nil || owner == nil || selection == ([32]byte{}) {
		return authenticatedstate.ErrRecovery
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.retired.Load() || p.engine == nil || p.engine.native != native || p.engine.owner != owner || p.engine.selectedHA != ha || ha && (lease == nil || p.engine.lease != lease) {
		return authenticatedstate.ErrRecovery
	}
	withSource := p.withPhysicalSourceLocked
	if ha {
		withSource = p.withHAPhysicalMaintenanceSourceLocked
	}
	err := withSource(ctx, func(ctx context.Context, source ledger.AuthenticatedPhysicalUsageSource) error {
		if err := source.VerifyPhysicalNoDebt(ctx); err != nil {
			return err
		}
		digest, err := source.PhysicalSelectionDigest(ctx)
		if err != nil || digest != selection {
			return errors.Join(authenticatedstate.ErrRecovery, err)
		}
		reference, err := source.PhysicalNativeReference(ctx)
		if err != nil {
			return err
		}
		anchor, err := physicalUsageState(ctx, p.engine.cow)
		if err != nil || anchor != source.Reference().Anchor || anchor != source.NativeHead() {
			return errors.Join(authenticatedstate.ErrRecovery, err)
		}
		q := p.physical
		existing := q != nil
		if q == nil {
			q = &typedPhysicalUsageOwner{native: native, ledger: owner, store: p.engine.cow, manager: p.engine.budget, publisher: p.physicalPublisher, selection: selection, applied: anchor, ha: ha}
		}
		if q.native != native || q.ledger != owner || q.store != p.engine.cow || q.manager != p.engine.budget || q.selection != selection || q.failure != nil || q.publisher == nil || q.publisher != p.physicalPublisher || q.ha != ha {
			return authenticatedstate.ErrRecovery
		}
		if err := source.ValidatePhysicalPublisher(ctx, q.publisher); err != nil {
			return err
		}
		seal, found, err := q.publisher.CurrentContext(ctx)
		if err != nil || !found || !qualifiedPhysicalUsageRole(seal.Role, ha) {
			return errors.Join(authenticatedstate.ErrRecovery, err)
		}
		root, err := q.store.SealRootReference(ctx, typedUsageSchema())
		if err != nil {
			return err
		}
		rootFound, positionFound := false, false
		for _, r := range seal.Roots[:seal.RootCount] {
			rootFound = rootFound || r == root
		}
		for _, position := range seal.Positions[:seal.PositionCount] {
			positionFound = positionFound || position == authenticatedUsagePosition(ledger.AuthenticatedLedgerSealReference{Anchor: anchor})
		}
		if !rootFound || !positionFound {
			return authenticatedstate.ErrRecovery
		}
		// Reuse requires every original control byte, extent, root and native
		// birth to match the independently qualified current common. Existing
		// fields or an unqualified receipt never manufacture a fresh owner.
		found, err = q.store.ValidatePhysicalTerminalPreparation(ctx, selection, reference, func(recorded authenticatedstate.PhysicalNativeReference, end int64) error {
			return source.VerifyPhysicalNativePrefix(ctx, recorded, end)
		})
		if err != nil {
			if errors.Is(err, ownedfile.ErrReplaced) || errors.Is(err, ownedfile.ErrRetirement) || errors.Is(err, resourcebudget.ErrPhysicalAllocationUncertain) {
				q.mu.Lock()
				q.closedQualified = false
				q.failure = errors.Join(q.failure, ownedfile.ErrRetirement, err, q.store.HoldPhysicalPreparationForRecovery(ctx))
				q.mu.Unlock()
				return q.failure
			}
			return err
		}
		if existing {
			if !found {
				return authenticatedstate.ErrRecovery
			}
		} else {
			if found {
				return authenticatedstate.ErrRecovery
			}
			p.physical = q // retains actual cleanup ownership on unknown Enable
			if _, err := q.store.EnablePhysicalTerminalQualified(ctx, selection, reference, func(recorded authenticatedstate.PhysicalNativeReference, end int64) error {
				return source.VerifyPhysicalNativePrefix(ctx, recorded, end)
			}); err != nil {
				return err
			}
		}
		q.mu.Lock()
		q.closedQualified = true
		q.mu.Unlock()
		return q.acquireWork(ctx)
	})
	q := p.physical
	if err != nil {
		return err
	}
	if q.installed {
		return nil
	}
	if ha {
		err = native.BindHAPhysicalDerivedCoverage(ctx, owner, lease, ledger.PhysicalDerivedUsage, q.coverage)
	} else {
		err = native.BindPhysicalDerivedCoverage(ctx, owner, ledger.PhysicalDerivedUsage, q.coverage)
	}
	if err == nil {
		q.installed = true
	}
	return err
}

func physicalUsageState(ctx context.Context, store *authenticatedstate.Store) (anchor ledger.ReplayAnchor, resultErr error) {
	pin, err := store.Snapshot(ctx)
	if err != nil {
		return anchor, err
	}
	defer func() { resultErr = errors.Join(resultErr, pin.Close()) }()
	key, err := typedUsageKey("meta", []byte("state"))
	if err != nil {
		return anchor, err
	}
	raw, found, err := pin.Get(ctx, key)
	if err != nil || !found {
		return anchor, errors.Join(ErrDiskUsageCorrupt, err)
	}
	state, err := decodePhysicalUsageState(raw)
	if err != nil {
		return anchor, err
	}
	return usageStateAnchor(state), ctx.Err()
}

func (q *typedPhysicalUsageOwner) coverage(ctx context.Context, native *ledger.Log, reference authenticatedstate.PhysicalNativeReference, end int64, stage ledger.PhysicalDerivedStage, futureFrames int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if native != q.native || q.failure != nil {
		return errors.Join(authenticatedstate.ErrRecovery, q.failure)
	}
	if stage == ledger.PhysicalDerivedClose {
		if !q.closedQualified {
			return authenticatedstate.ErrRecovery
		}
		return nil
	}
	q.closedQualified = false
	if reference.Sequence < q.applied.Head.Sequence || futureFrames < 0 {
		return authenticatedstate.ErrRecovery
	}
	required := reference.Sequence - q.applied.Head.Sequence + uint64(futureFrames)
	if stage == ledger.PhysicalDerivedRoll {
		required++
	}
	if required > resourcebudget.MaxCompletionBatch {
		return resourcebudget.ErrExhausted
	}
	if stage == ledger.PhysicalDerivedSuccessor {
		return native.TransferPhysicalDerivedNativeReceipt(ctx, q.store)
	}
	if err := q.store.RecordPhysicalNativeRange(reference, end); err != nil {
		return err
	}
	for q.remaining < int(required) {
		slot := -1
		for i, w := range q.windows {
			if w == nil {
				slot = i
				break
			}
		}
		if slot < 0 {
			return resourcebudget.ErrExhausted
		}
		w, err := q.store.ReservePhysicalTerminals(ctx, 1)
		if err != nil {
			return err
		}
		q.windows[slot], q.counts[slot] = w, 1
		q.remaining++
	}
	return ctx.Err()
}

// The caller has authenticated the durable source fact under the original
// source loan. Failures after candidate preparation retain the actual debt;
// no query/watermark or a cancelled callback can refund an accepted fact.
func (p *DiskProjection) preparePhysicalUsageUpdate(ctx context.Context) (context.Context, func(error) error, error) {
	q := p.physical
	if q == nil {
		return ctx, func(error) error { return nil }, nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.failure != nil {
		return ctx, nil, errors.Join(authenticatedstate.ErrRecovery, q.failure)
	}
	index := -1
	for i, w := range q.windows {
		if w != nil && q.counts[i] > 0 {
			index = i
			break
		}
	}
	if index < 0 {
		return ctx, nil, resourcebudget.ErrExhausted
	}
	u, err := q.store.PreparePhysicalUpdate(ctx, q.windows[index])
	if err != nil {
		return ctx, nil, err
	}
	return u.Bind(ctx), func(cause error) error {
		q.mu.Lock()
		defer q.mu.Unlock()
		err := u.Finish(true, cause)
		if err != nil {
			q.failure = errors.Join(q.failure, ownedfile.ErrRetirement, err)
			return q.failure
		}
		q.counts[index]--
		q.remaining--
		if q.counts[index] == 0 {
			if err := q.windows[index].Close(); err != nil {
				q.failure = errors.Join(ownedfile.ErrRetirement, err)
				return q.failure
			}
			q.windows[index] = nil
		}
		return nil
	}, nil
}

func (q *typedPhysicalUsageOwner) retireWindows(ctx context.Context) error {
	for i, w := range q.windows {
		if w == nil {
			continue
		}
		if err := w.Close(); err != nil {
			return err
		}
		q.windows[i], q.counts[i] = nil, 0
	}
	q.remaining = 0
	return q.store.RetirePhysicalPreparationKeepingControls(ctx)
}

// Retirement happens only after the caller's actual maintenance lease proves
// Ledger NoDebt and the original publisher selects this exact current Usage
// root and position. The publisher argument is checked against native provenance.
func (p *DiskProjection) RetirePhysicalCatchupAfterCommon(ctx context.Context, native *ledger.Log, owner *ledger.AuthenticatedLedgerProjection, lease *ledger.MaintenanceLease, publisher *authenticatedstate.SealCandidatePublisher) error {
	return p.retirePhysicalCatchupAfterCommon(ctx, native, owner, lease, publisher, false)
}

func (p *DiskProjection) retirePhysicalCatchupAfterCommon(ctx context.Context, native *ledger.Log, owner *ledger.AuthenticatedLedgerProjection, lease *ledger.MaintenanceLease, publisher *authenticatedstate.SealCandidatePublisher, ha bool) error {
	if p == nil {
		return authenticatedstate.ErrRecovery
	}
	p.mu.RLock()
	enabled := p.physical != nil
	p.mu.RUnlock()
	if !enabled {
		return nil
	}
	if native == nil || owner == nil || lease == nil || publisher == nil {
		return authenticatedstate.ErrRecovery
	}
	if !ha {
		if err := owner.VerifyPhysicalNoDebt(ctx, native, lease); err != nil {
			return err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.physical == nil {
		return nil
	}
	q := p.physical
	if q.native != native || q.ledger != owner || q.ha != ha || p.engine == nil || p.engine.selectedHA != ha || ha && p.engine.lease != lease {
		return authenticatedstate.ErrRecovery
	}
	withSource := p.withPhysicalSourceLocked
	if ha {
		withSource = p.withHAPhysicalMaintenanceSourceLocked
	}
	return withSource(ctx, func(ctx context.Context, source ledger.AuthenticatedPhysicalUsageSource) error {
		if ha {
			s, ok := source.(haPhysicalUsageSourceAdapter)
			if !ok {
				return authenticatedstate.ErrRecovery
			}
			if err := s.VerifyHAPhysicalNoDebtAfterCommon(ctx); err != nil {
				return err
			}
		}
		q.mu.Lock()
		defer q.mu.Unlock()
		if q.failure != nil {
			return q.failure
		}
		if err := source.ValidatePhysicalPublisher(ctx, publisher); err != nil {
			return err
		}
		anchor, err := physicalUsageState(ctx, q.store)
		if err != nil || anchor != source.NativeHead() || anchor != source.Reference().Anchor {
			return errors.Join(authenticatedstate.ErrRecovery, err)
		}
		seal, found, err := publisher.CurrentContext(ctx)
		if err != nil || !found {
			return errors.Join(authenticatedstate.ErrRecovery, err)
		}
		root, err := q.store.SealRootReference(ctx, typedUsageSchema())
		if err != nil {
			return err
		}
		rootFound, positionFound := false, false
		for _, r := range seal.Roots[:seal.RootCount] {
			rootFound = rootFound || r == root
		}
		for _, position := range seal.Positions[:seal.PositionCount] {
			positionFound = positionFound || position == authenticatedUsagePosition(ledger.AuthenticatedLedgerSealReference{Anchor: anchor})
		}
		if !rootFound || !positionFound || !qualifiedPhysicalUsageRole(seal.Role, ha) {
			return authenticatedstate.ErrRecovery
		}
		if err := q.retireWindows(ctx); err != nil {
			q.failure = errors.Join(ownedfile.ErrRetirement, err)
			return q.failure
		}
		q.applied = anchor
		q.publisher = publisher
		q.closedQualified = true
		return source.Check(ctx)
	})
}

func (p *DiskProjection) BindPhysicalCommonSelection(ctx context.Context, native *ledger.Log, owner *ledger.AuthenticatedLedgerProjection, selection [32]byte) error {
	return p.bindPhysicalCommonSelection(ctx, native, owner, selection, false)
}

func (p *DiskProjection) bindPhysicalCommonSelection(ctx context.Context, native *ledger.Log, owner *ledger.AuthenticatedLedgerProjection, selection [32]byte, ha bool) error {
	if p == nil {
		return authenticatedstate.ErrRecovery
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	q := p.physical
	if q == nil {
		return nil
	}
	if q.native != native || q.ledger != owner || q.ha != ha || p.engine == nil || p.engine.selectedHA != ha {
		return authenticatedstate.ErrRecovery
	}
	withSource := p.withPhysicalSourceLocked
	if ha {
		withSource = p.withHAPhysicalMaintenanceSourceLocked
	}
	return withSource(ctx, func(ctx context.Context, source ledger.AuthenticatedPhysicalUsageSource) error {
		q.mu.Lock()
		defer q.mu.Unlock()
		if q.remaining != 0 || q.failure != nil {
			return authenticatedstate.ErrRecovery
		}
		actual, err := source.PhysicalSelectionDigest(ctx)
		if err != nil || actual != selection {
			return errors.Join(authenticatedstate.ErrRecovery, err)
		}
		ref, err := source.PhysicalNativeReference(ctx)
		if err != nil {
			return err
		}
		if err := q.store.BindPhysicalSelectionDigest(selection, ref); err != nil {
			return err
		}
		end, err := source.PhysicalNativePreparedEnd(ctx)
		if err != nil {
			return err
		}
		if err := q.store.RecordPhysicalNativeRange(ref, end); err != nil {
			return err
		}
		q.selection = selection
		// The next real native coverage hook withdraws this qualification before IO.
		return source.Check(ctx)
	})
}

func (p *DiskProjection) closePhysicalUsageLocked(ctx context.Context) error {
	q := p.physical
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.closedQualified || q.failure != nil {
		q.failure = errors.Join(ownedfile.ErrRetirement, authenticatedstate.ErrRecovery, q.failure, q.store.HoldPhysicalPreparationForRecovery(ctx))
		return q.failure
	}
	if q.work == nil || q.publisher == nil {
		return authenticatedstate.ErrRecovery
	}
	err := resourcebudget.WithCompletionWorkspace(q.work.Bind(ctx), func(ctx context.Context) error {
		seal, found, err := q.publisher.CurrentContext(ctx)
		if err != nil || !found {
			return errors.Join(authenticatedstate.ErrRecovery, err)
		}
		raw, err := json.Marshal(seal)
		if err != nil || sha256.Sum256(raw) != q.selection {
			return errors.Join(authenticatedstate.ErrRecovery, err)
		}
		root, err := q.store.SealRootReference(ctx, typedUsageSchema())
		if err != nil {
			return err
		}
		rootFound, positionFound := false, false
		for _, r := range seal.Roots[:seal.RootCount] {
			rootFound = rootFound || r == root
		}
		for _, pos := range seal.Positions[:seal.PositionCount] {
			positionFound = positionFound || pos == authenticatedUsagePosition(ledger.AuthenticatedLedgerSealReference{Anchor: q.applied})
		}
		if !rootFound || !positionFound {
			return authenticatedstate.ErrRecovery
		}
		return q.retireWindows(ctx)
	})
	if err != nil {
		q.failure = errors.Join(ownedfile.ErrRetirement, err, q.store.HoldPhysicalPreparationForRecovery(ctx))
		return q.failure
	}
	return nil
}

func (p *DiskProjection) updatePhysicalUsageFrame(ctx context.Context, target ledger.ReplayAnchor, apply func(*authenticatedstate.Transaction) error, durable func() error) (receipt authenticatedstate.UpdateReceipt, resultErr error) {
	prepared, finish, err := p.preparePhysicalUsageUpdate(ctx)
	if err != nil {
		return receipt, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, finish(resultErr))
		if resultErr == nil && p.physical != nil {
			p.physical.mu.Lock()
			p.physical.applied = target
			p.physical.closedQualified = false
			p.physical.mu.Unlock()
		}
	}()
	return p.engine.cow.UpdateNativeFrame(prepared, typedUsageCut(target), apply, durable)
}

func physicalUsageNoDebt(ctx context.Context, owner *ledger.AuthenticatedLedgerProjection) error {
	return owner.WithRecoveryView(ctx, func(v ledger.AuthenticatedRecoveryView) error {
		pending, err := v.PendingLeasePage(ctx, "", ledger.ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
		if err != nil {
			return err
		}
		open, err := v.UnfinalizedRequestPage(ctx, "", ledger.ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
		if err != nil {
			return err
		}
		if len(pending.Items) != 0 || len(open.Items) != 0 || !pending.Done || !open.Done {
			return authenticatedstate.ErrRecovery
		}
		return v.Complete(ctx)
	})
}

// Registration precedes pool activation. Close cancels this original lifetime
// and joins even the interval before the inner collector installs catchUpDone.
func (p *DiskProjection) beginPhysicalUsageJob(ctx context.Context) (context.Context, func(), error) {
	p.physicalJobMu.Lock()
	defer p.physicalJobMu.Unlock()
	if p.retired.Load() || p.physicalCtx == nil || p.physicalCtx.Err() != nil {
		return ctx, nil, ErrDiskUsageClosed
	}
	p.physicalJobs.Add(1)
	op, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(p.physicalCtx, cancel)
	return op, func() { stop(); cancel(); p.physicalJobs.Done() }, nil
}
