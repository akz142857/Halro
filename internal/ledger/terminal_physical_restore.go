package ledger

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

type PhysicalStartupRecoveryOptions struct {
	GrowthBytes               int64
	GrowthFee                 resourcebudget.Permit
	Capacity                  PhysicalNativeCapacity
	CompletionRAM             int64
	PublicationRequiresNoDebt bool
}

// Called inside the actual selected factory, before delta reduction, holding
// native mutation ownership and mu. No ordinary constructor can lend this.
func (p *AuthenticatedLedgerProjection) restorePhysicalStartupLocked(ctx context.Context, native *Log, selection [32]byte, target ReplayAnchor, o *PhysicalStartupRecoveryOptions) (bool, error) {
	return p.restorePhysicalStartupModeLocked(ctx, native, selection, target, o, false)
}

// ha is set only by the private independently qualified HA factory below.
func (p *AuthenticatedLedgerProjection) restorePhysicalStartupModeLocked(ctx context.Context, native *Log, selection [32]byte, target ReplayAnchor, o *PhysicalStartupRecoveryOptions, ha bool) (bool, error) {
	if o == nil {
		return false, nil
	}
	if ha {
		if err := qualifyHAPhysicalStartupRestoreLocked(ctx, native, selection, target); err != nil {
			return false, err
		}
	} else if native.startupProvenance == nil || native.startupProvenance.ha != nil {
		return false, ErrProjectionSource
	}
	profile := CompletionWorkspaceRequirements()
	var completionRAM int64
	for _, requirement := range profile {
		completionRAM += requirement.RAM
	}
	if o.CompletionRAM < completionRAM {
		return false, resourcebudget.ErrInvalid
	}
	if !native.selectedStartupStructuralAllowedLocked() || native.physical.Load() != nil || o.GrowthBytes <= 0 || o.GrowthBytes > 1<<50 || o.Capacity == nil || o.CompletionRAM <= 0 || !ha && native.options.Replica || native.status.Load() != AccountingHealthy {
		return false, ErrProjectionSource
	}
	proveNoDebt := func() error {
		if ha && target != (ReplayAnchor{Head: Watermark{native.generation, native.offset, native.sequence}, Hash: native.chainHash, Epoch: native.chainEpoch}) {
			return authenticatedstate.ErrRecovery
		}
		return p.provePhysicalRecoveryNoDebt(ctx)
	}
	if o.PublicationRequiresNoDebt {
		anchor, err := p.ReadReplayAnchor(ctx)
		if err != nil || anchor != target {
			return false, errors.Join(authenticatedstate.ErrRecovery, err)
		}
		if err := proveNoDebt(); err != nil {
			return false, err
		}
	}
	actual, statErr := native.file.Stat()
	named, nameErr := os.Lstat(native.path)
	parent, parentErr := os.Lstat(native.directory)
	if statErr != nil || nameErr != nil || parentErr != nil || !actual.Mode().IsRegular() || !named.Mode().IsRegular() || !os.SameFile(actual, named) || actual.Size() != native.offset || !parent.IsDir() || !os.SameFile(parent, native.directoryInfo) {
		return false, errors.Join(ErrProjectionSource, statErr, nameErr, parentErr)
	}
	fee, err := o.GrowthFee.RetainCovering(p.budget, resourcebudget.Cost{Disk: o.GrowthBytes})
	if err != nil {
		return false, err
	}
	reference, err := native.physicalNativeReferenceLocked()
	if err != nil {
		return false, errors.Join(err, fee.Release())
	}
	owner := &nativePhysicalOwner{projection: p, selection: selection, file: actual, generation: native.generation, ceiling: native.offset + o.GrowthBytes, allocatedEnd: native.offset, capacity: o.Capacity, fee: fee, feeHeld: true, restoring: true}
	// Publish a cleanup owner before the Store opens any persisted target FD.
	native.physical.Store(owner)
	verify := func(prior authenticatedstate.PhysicalNativeReference, end int64) error {
		verifier := &chainVerifier{key: native.chainKey, hash: native.chainAnchor, sequence: native.startSequence}
		head, partial, err := scan(io.NewSectionReader(native.file, 0, prior.EOF), native.generation, 0, native.startSequence, nil, verifier)
		if err != nil || partial || head.Offset != prior.EOF || head.Sequence != prior.Sequence || verifier.hash != prior.Hash {
			return errors.Join(ErrProjectionSource, err)
		}
		if end < native.offset || end > owner.ceiling {
			return resourcebudget.ErrExhausted
		}
		if err := resourcebudget.CheckPhysicalTail(native.file, native.offset, end); err != nil {
			return err
		}
		if end > native.offset {
			// A recorded upper bound alone cannot prove completion targets still
			// exist, including a zero-slot receipt left by an interrupted probe.
			if err := resourcebudget.CheckPhysicalRange(native.file, native.offset, end); err != nil {
				return err
			}
			if err := o.Capacity.ReservePhysicalRange(end - native.offset); err != nil {
				return err
			}
			owner.allocatedEnd = end
		}
		return nil
	}
	restoreCut := authenticatedLedgerCut(p.scope, target)
	if ha {
		var foundRoot bool
		restoreCut, foundRoot, err = p.store.WithPhysicalRecoveryRoot(ctx, selection, reference, func(pin *authenticatedstate.Snapshot, _ [32]byte, _ authenticatedstate.PhysicalNativeReference) ([32]byte, error) {
			position, found, err := authenticatedPointGet[projectionPosition](ctx, pin, "meta", []byte("position"))
			if err != nil || !found {
				return [32]byte{}, errors.Join(ErrProjectionCorrupt, err)
			}
			candidate := ReplayAnchor{Head: position.Watermark, Hash: position.Hash, Epoch: position.Epoch}
			if err := verifyHAPhysicalRecoveryAnchorLocked(ctx, native, candidate, target); err != nil {
				return [32]byte{}, err
			}
			return authenticatedLedgerCut(p.scope, candidate), nil
		})
		if err != nil && !errors.Is(err, authenticatedstate.ErrSealIneligible) && !errors.Is(err, authenticatedstate.ErrPhysicalReceiptRequiresNoDebt) {
			owner.failure = err
			native.status.RequireRecovery()
			return false, err
		}
		if !foundRoot {
			restoreCut = authenticatedLedgerCut(p.scope, target)
		}
	}
	cow, end, found, err := p.store.RestorePhysicalTerminalForRecovery(ctx, selection, reference, restoreCut, verify)
	if errors.Is(err, authenticatedstate.ErrSealIneligible) {
		anchor, anchorErr := p.ReadReplayAnchor(ctx)
		if anchorErr == nil && anchor == target {
			if noDebtErr := proveNoDebt(); noDebtErr == nil {
				end, err = p.store.EnablePhysicalTerminalAfterQualifiedNoDebt(ctx, selection, reference, authenticatedLedgerCut(p.scope, target), verify)
				found = err == nil
			}
		}
	}
	if errors.Is(err, authenticatedstate.ErrPhysicalReceiptRequiresNoDebt) {
		// An old receipt must never promote a scanned newer root. Only this
		// exact independently selected root at the fully authenticated native
		// head may prove every original obligation already resolved.
		anchor, anchorErr := p.ReadReplayAnchor(ctx)
		if anchorErr != nil || anchor != target {
			err = errors.Join(authenticatedstate.ErrRecovery, anchorErr)
		} else if err = proveNoDebt(); err == nil {
			end, err = p.store.EnablePhysicalTerminalQualified(ctx, selection, reference, verify)
			found = err == nil
		}
	}
	if err != nil {
		owner.failure = err
		native.status.RequireRecovery()
		return false, err
	}
	if !found {
		if err := resourcebudget.CheckPhysicalTail(native.file, native.offset, native.offset); err != nil {
			owner.failure = err
			native.status.RequireRecovery()
			return false, err
		}
		native.physical.Store(nil)
		return false, fee.Release()
	}
	count := cow.Count()
	owner.allocatedEnd, owner.debt = end, count
	futureNativeCount := count
	if ha {
		// COW slots also include ordinary candidates whose actual source frame
		// became durable before SIGKILL. Their source bytes already exist; only
		// the independently authenticated candidate-to-Allowed delta may
		// consume them. This does not grant any extra future native range.
		candidate, anchorErr := p.ReadReplayAnchor(ctx)
		if anchorErr != nil || candidate.Head.Sequence > target.Head.Sequence || projectionPrefixBefore(target.Head, candidate.Head) {
			owner.failure = errors.Join(ErrProjectionSource, anchorErr)
			native.status.RequireRecovery()
			return false, owner.failure
		}
		durableDelta := target.Head.Sequence - candidate.Head.Sequence
		if durableDelta > uint64(count) {
			owner.failure = authenticatedstate.ErrRecovery
			native.status.RequireRecovery()
			return false, owner.failure
		}
		futureNativeCount -= int(durableDelta)
	}
	if futureNativeCount > 0 {
		required := int64(futureNativeCount) * terminalNativePhysicalBytes
		if required > end-native.offset {
			owner.failure = authenticatedstate.ErrRecovery
			native.status.RequireRecovery()
			return false, owner.failure
		}
		if err := resourcebudget.CheckPhysicalRange(native.file, native.offset, native.offset+required); err != nil {
			owner.failure = err
			native.status.RequireRecovery()
			return false, err
		}
	}
	work, err := p.budget.TryAcquireCompletableWork(resourcebudget.IO, resourcebudget.Cost{RAM: 4096}, resourcebudget.Cost{RAM: completionRAM}, profile)
	if err != nil {
		owner.failure = err
		native.status.RequireRecovery()
		return false, err
	}
	p.physicalRecovery = &TerminalPhysicalWindow{native: native, cow: cow, remaining: count}
	p.physicalRecoveryWork = work
	p.selectedNative.Store(native)
	return true, nil
}

func (p *AuthenticatedLedgerProjection) PhysicalStartupRecoveryPending() bool {
	if p == nil {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.physicalRecovery != nil
}

func (p *AuthenticatedLedgerProjection) WithPhysicalStartupRecovery(ctx context.Context, kind EventKind, visit func(context.Context) error) error {
	p.mu.RLock()
	window, work := p.physicalRecovery, p.physicalRecoveryWork
	p.mu.RUnlock()
	if window == nil {
		return visit(ctx)
	}
	if kind != EventAttemptSettled && kind != EventRequestFinalized {
		return authenticatedstate.ErrRecovery
	}
	if work == nil {
		return authenticatedstate.ErrRecovery
	}
	ctx = window.Bind(work.Bind(WithPhysicalAppendKind(ctx, kind)))
	return resourcebudget.WithCompletionWorkspace(ctx, func(ctx context.Context) error { return p.WithPreparedPhysicalAppend(ctx, visit) })
}

func (p *AuthenticatedLedgerProjection) provePhysicalRecoveryNoDebt(ctx context.Context) error {
	return p.WithRecoveryView(ctx, func(v AuthenticatedRecoveryView) error {
		pending, err := v.PendingLeasePage(ctx, "", ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
		if err != nil {
			return err
		}
		open, err := v.UnfinalizedRequestPage(ctx, "", ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
		if err != nil {
			return err
		}
		if len(pending.Items) != 0 || len(open.Items) != 0 || !pending.Done || !open.Done {
			return authenticatedstate.ErrRecovery
		}
		return v.Complete(ctx)
	})
}

func (p *AuthenticatedLedgerProjection) FinishPhysicalStartupRecovery(ctx context.Context, native *Log) error {
	if !p.PhysicalStartupRecoveryPending() {
		return nil
	}
	if err := p.provePhysicalRecoveryNoDebt(ctx); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.ready || p.closeContext.Err() != nil || p.physicalRecovery == nil || p.physicalRecoveryWork == nil {
		return authenticatedstate.ErrRecovery
	}
	if err := p.physicalRecovery.Close(); err != nil {
		return err
	}
	if err := p.physicalRecoveryWork.Close(); err != nil {
		return err
	}
	if err := p.physicalRecoveryWork.Active.Release(); err != nil {
		return err
	}
	p.physicalRecoveryWork = nil
	p.physicalRecovery = nil
	native.mu.Lock()
	defer native.mu.Unlock()
	if native.physical.Load() == nil || native.physical.Load().projection != p || !native.physical.Load().restoring {
		return ErrProjectionSource
	}
	// All request-owned continuations have joined, but the original common
	// selection still needs its secondary targets and receipt for cold replay.
	// Whole seven-root publication owns later unused-tail/control rollover.
	return native.checkPhysicalNativeLocked()
}
