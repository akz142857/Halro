package ledger

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"

	"github.com/akz142857/Halro/internal/auditlimits"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/governancelimits"
	"github.com/akz142857/Halro/internal/replicationlimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// Catch-up owns its targets independently of HTTP callbacks: directory writes
// happen at a common checkpoint, after the original accepted request exits.
// A page and the final scalar each consume a real prepaid candidate inode.
const directoryPhysicalCatchupSlots = 2

type directoryPhysicalOwner struct {
	native          *Log
	ledger          *AuthenticatedLedgerProjection
	selection       [32]byte
	window          *authenticatedstate.PhysicalTerminalWindow
	remaining       int
	work            *resourcebudget.CompletableWork
	closedQualified bool
	ha              bool
}

// This runs inside the selected factory's real native source loan, before any
// suffix page is written. The temporary root is not selected until its exact
// scalar prefix and every descriptor are independently checked against native.
func (d *AuthenticatedDirectory) restorePhysicalDirectoryLocked(ctx context.Context, source SelectedStartupSource) (bool, error) {
	s, ok := source.(SelectedPhysicalStartupSource)
	if !ok || d.selectedNative == nil || d.selectedNative.startupProvenance.ha != nil {
		return false, ErrProjectionSource
	}
	return d.restorePhysicalDirectorySourceLocked(ctx, source, s, false)
}

// The HA adapter is private and can only wrap the constructor's actual HA loan.
func (d *AuthenticatedDirectory) restoreHAPhysicalDirectoryLocked(ctx context.Context, source SelectedStartupSource) (bool, error) {
	s, ok := source.(SelectedHAPhysicalStartupSource)
	if !ok || d.selectedNative == nil || d.selectedNative.startupProvenance.ha == nil {
		return false, ErrProjectionSource
	}
	return d.restorePhysicalDirectorySourceLocked(ctx, source, haDirectoryPhysicalStartupSource{s}, true)
}

type directoryPhysicalStartupSource interface {
	PhysicalNativeReference(context.Context) (authenticatedstate.PhysicalNativeReference, error)
	VerifyPhysicalNativePrefix(context.Context, authenticatedstate.PhysicalNativeReference, int64) error
	PhysicalNativePreparedEnd(context.Context) (int64, error)
}

func (d *AuthenticatedDirectory) restorePhysicalDirectorySourceLocked(ctx context.Context, source SelectedStartupSource, s directoryPhysicalStartupSource, ha bool) (bool, error) {
	raw, err := json.Marshal(source.SelectedSeal())
	if err != nil {
		return false, err
	}
	selection := sha256.Sum256(raw)
	reference, err := s.PhysicalNativeReference(ctx)
	if err != nil {
		return false, err
	}
	var candidate authenticatedDirectoryState
	cut, found, err := d.store.WithPhysicalRecoveryRoot(ctx, selection, reference, func(pin *authenticatedstate.Snapshot, comparison [32]byte, recorded authenticatedstate.PhysicalNativeReference) ([32]byte, error) {
		// A fresh first-run source has no physical owner yet. Only an actual
		// receipt-bound root needs its native owner's already allocated end.
		preparedEnd, err := s.PhysicalNativePreparedEnd(ctx)
		if err != nil {
			return [32]byte{}, err
		}
		stateRaw, exists, err := pin.Get(ctx, []byte{0})
		if err != nil || !exists {
			return [32]byte{}, errors.Join(ErrProjectionCorrupt, err)
		}
		if err := json.Unmarshal(stateRaw, &candidate); err != nil || candidate.Version != 1 || candidate.Anchor.Head.Generation == 0 || candidate.Segments != candidate.Anchor.Head.Generation-1 || candidate.Anchor.Head.Generation < d.anchor.Head.Generation || candidate.Anchor.Head.Sequence < d.anchor.Head.Sequence || candidate.Anchor.Head.Generation > source.NativeHead().Head.Generation || candidate.Anchor.Head.Sequence > source.NativeHead().Head.Sequence {
			return [32]byte{}, errors.Join(ErrProjectionSource, err)
		}
		if err := source.VerifyAnchors(ctx, [2]ReplayAnchor{candidate.Anchor, source.NativeHead()}); err != nil {
			return [32]byte{}, err
		}
		if err := s.VerifyPhysicalNativePrefix(ctx, recorded, preparedEnd); err != nil {
			return [32]byte{}, err
		}
		// Interrupted descriptor pages can legitimately precede their final
		// scalar. Require one exact contiguous native-derived namespace, with
		// no missing selected descriptor or unrelated key, before resuming it.
		var cursor string
		generation := uint64(0)
		for {
			page, err := pin.Range(ctx, authenticatedstate.RangeOptions{Cursor: cursor, MaxRows: 63, MaxBytes: 1 << 20, MaxPages: 4096})
			if err != nil {
				return [32]byte{}, err
			}
			for _, row := range page.Rows {
				if generation == 0 {
					if !bytes.Equal(row.Key, []byte{0}) || !bytes.Equal(row.Value, stateRaw) {
						return [32]byte{}, ErrProjectionCorrupt
					}
					generation = 1
					continue
				}
				if len(row.Key) != 9 || row.Key[0] != 1 || binary.BigEndian.Uint64(row.Key[1:]) != generation || generation >= source.NativeHead().Head.Generation {
					return [32]byte{}, ErrProjectionCorrupt
				}
				var stored Segment
				if err := json.Unmarshal(row.Value, &stored); err != nil {
					return [32]byte{}, err
				}
				actual, exists, err := d.selectedNative.lookupSegmentLocked(generation)
				if err != nil || !exists || !directorySameNativeDescriptor(stored, actual) {
					return [32]byte{}, errors.Join(ErrProjectionSource, err)
				}
				generation++
			}
			if page.Done {
				break
			}
			cursor = page.Cursor
		}
		if generation == 0 || generation-1 < candidate.Segments {
			return [32]byte{}, ErrProjectionCorrupt
		}
		return authenticatedDirectoryCut(d.scope, candidate.Anchor), nil
	})
	if errors.Is(err, authenticatedstate.ErrSealIneligible) || errors.Is(err, authenticatedstate.ErrPhysicalReceiptRequiresNoDebt) {
		// Only the independently selected complete directory at the full native
		// head may roll an older/unfinished preparation forward without debt.
		p := d.selectedNative.physical.Load()
		if p == nil || p.projection == nil || d.anchor != source.NativeHead() || d.segments != d.anchor.Head.Generation-1 {
			return false, errors.Join(authenticatedstate.ErrRecovery, err)
		}
		if ha {
			if err := qualifyHAPhysicalStartupRestoreLocked(ctx, d.selectedNative, selection, d.anchor); err != nil || d.anchor.Head != (Watermark{d.selectedNative.generation, d.selectedNative.offset, d.selectedNative.sequence}) || d.anchor.Hash != d.selectedNative.chainHash {
				return false, errors.Join(authenticatedstate.ErrRecovery, err)
			}
		}
		if err := p.projection.provePhysicalRecoveryNoDebt(ctx); err != nil {
			return false, err
		}
		_, err = d.store.EnablePhysicalTerminalAfterQualifiedNoDebt(ctx, selection, reference, authenticatedDirectoryCut(d.scope, d.anchor), func(prior authenticatedstate.PhysicalNativeReference, end int64) error {
			return s.VerifyPhysicalNativePrefix(ctx, prior, end)
		})
		if err != nil {
			return false, err
		}
		d.physical = &directoryPhysicalOwner{native: d.selectedNative, ledger: p.projection, selection: selection, ha: ha}
		if err := d.acquirePhysicalCatchupWork(ctx, d.physical); err != nil {
			return false, err
		}
		return true, source.Check(ctx)
	}
	if err != nil {
		return false, err
	}
	if !found {
		// A trace-free source still needs the actual Restore boundary to clear
		// OpenStore's physical recovery write gate. No candidate is promoted.
		_, _, found, err = d.store.RestorePhysicalTerminalForRecovery(ctx, selection, reference, authenticatedDirectoryCut(d.scope, d.anchor), func(prior authenticatedstate.PhysicalNativeReference, end int64) error {
			return s.VerifyPhysicalNativePrefix(ctx, prior, end)
		})
		if found {
			return false, authenticatedstate.ErrRecovery
		}
		return false, err
	}
	window, _, found, err := d.store.RestorePhysicalTerminalForRecovery(ctx, selection, reference, cut, func(recorded authenticatedstate.PhysicalNativeReference, end int64) error {
		return s.VerifyPhysicalNativePrefix(ctx, recorded, end)
	})
	if err != nil || !found {
		return false, errors.Join(authenticatedstate.ErrRecovery, err)
	}
	p := d.selectedNative.physical.Load()
	if p == nil || p.projection == nil || p.selection != selection {
		return false, ErrProjectionSource
	}
	d.physical = &directoryPhysicalOwner{native: d.selectedNative, ledger: p.projection, selection: selection, window: window, remaining: window.Count(), ha: ha}
	d.anchor, d.segments = candidate.Anchor, candidate.Segments
	if err := d.acquirePhysicalCatchupWork(ctx, d.physical); err != nil {
		return false, err
	}
	return true, source.Check(ctx)
}

func directorySameNativeDescriptor(a, b Segment) bool {
	// Physical raw/gzip locators are refreshed only from the fully qualified
	// native owner after this gate; they never authorize a different plaintext.
	return a.Generation == b.Generation && a.FirstSequence == b.FirstSequence && a.LastSequence == b.LastSequence && a.Length == b.Length && a.StartHash == b.StartHash && a.EndHash == b.EndHash && a.EndEpoch == b.EndEpoch && a.PlainChecksum == b.PlainChecksum && a.SealedAt.Equal(b.SealedAt)
}

// Caller holds d.mu then native.mu. The actual selected Ledger physical owner
// supplies the birth and allocated end; caller fields and disk ceilings cannot
// substitute for an allocated target or independently selected provenance.
func (d *AuthenticatedDirectory) physicalSourceLocked(native *Log, owner *AuthenticatedLedgerProjection, selection [32]byte) (authenticatedstate.PhysicalNativeReference, int64, error) {
	if d.selectedNative != native || native == nil || owner == nil || owner.selectedNative.Load() != native || owner.budget != d.budget || native.startupBudget != d.budget || native.startupProvenance == nil || native.startupProvenance.ha != nil || selection == ([32]byte{}) || native.options.Replica {
		return authenticatedstate.PhysicalNativeReference{}, 0, ErrProjectionSource
	}
	p := native.physical.Load()
	if p == nil || p.projection != owner || p.selection != selection || p.failure != nil {
		return authenticatedstate.PhysicalNativeReference{}, 0, ErrProjectionSource
	}
	if err := native.checkPhysicalNativeLocked(); err != nil {
		return authenticatedstate.PhysicalNativeReference{}, 0, err
	}
	reference, err := native.physicalNativeReferenceLocked()
	return reference, p.allocatedEnd, err
}

func directoryVerifyPhysicalPrefixLocked(ctx context.Context, native *Log, current authenticatedstate.PhysicalNativeReference, recorded authenticatedstate.PhysicalNativeReference, end int64) error {
	if recorded.File != current.File || recorded.Parent != current.Parent || recorded.Generation != current.Generation || recorded.EOF > current.EOF || recorded.Sequence > current.Sequence || end < current.EOF {
		return ErrProjectionSource
	}
	verifier := &chainVerifier{key: native.chainKey, hash: native.chainAnchor, sequence: native.startSequence}
	head, partial, err := scan(io.NewSectionReader(native.file, 0, recorded.EOF), native.generation, 0, native.startSequence, func(Record) error { return ctx.Err() }, verifier)
	if err != nil || partial || head.Offset != recorded.EOF || head.Sequence != recorded.Sequence || verifier.hash != recorded.Hash {
		return errors.Join(ErrProjectionSource, err)
	}
	if p := native.physical.Load(); p == nil || end > p.allocatedEnd {
		return resourcebudget.ErrExhausted
	}
	return errors.Join(ctx.Err(), resourcebudget.CheckPhysicalTail(native.file, native.offset, end))
}

// EnablePhysicalCatchup requires the actual Ledger's independently qualified
// physical owner and a complete directory at its current native prefix. It
// allocates every initial target before the Runtime can admit upstream work.
func (d *AuthenticatedDirectory) EnablePhysicalCatchup(ctx context.Context, native *Log, owner *AuthenticatedLedgerProjection, selection [32]byte) error {
	if d == nil || native == nil || owner == nil {
		return ErrProjectionSource
	}
	if err := owner.provePhysicalRecoveryNoDebt(ctx); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	native.mu.Lock()
	defer native.mu.Unlock()
	if d.anchor.Head != (Watermark{Generation: native.generation, Sequence: native.sequence, Offset: native.offset}) || d.anchor.Hash != native.chainHash || d.anchor.Epoch != native.chainEpoch || d.segments != native.generation-1 {
		return ErrProjectionSource
	}
	reference, end, err := d.physicalSourceLocked(native, owner, selection)
	if err != nil {
		return err
	}
	if p := d.physical; p != nil {
		if err := d.acquirePhysicalCatchupWork(ctx, p); err != nil {
			return err
		}
		if p.native != native || p.ledger != owner || p.selection != selection {
			return ErrProjectionSource
		}
		if p.window != nil {
			return nil
		}
		p.window, err = d.store.ReservePhysicalTerminals(ctx, directoryPhysicalCatchupSlots)
		if err != nil {
			d.sealErr = errors.Join(d.sealErr, err)
			return err
		}
		p.remaining = directoryPhysicalCatchupSlots
		return nil
	}
	d.physical = &directoryPhysicalOwner{native: native, ledger: owner, selection: selection}
	if err := d.acquirePhysicalCatchupWork(ctx, d.physical); err != nil {
		return err
	}
	_, err = d.store.EnablePhysicalTerminalQualified(ctx, selection, reference, func(prior authenticatedstate.PhysicalNativeReference, end int64) error {
		return directoryVerifyPhysicalPrefixLocked(ctx, native, reference, prior, end)
	})
	if err == nil {
		err = d.store.RecordPhysicalNativeRange(reference, end)
	}
	if err == nil {
		d.physical.window, err = d.store.ReservePhysicalTerminals(ctx, directoryPhysicalCatchupSlots)
	}
	if err != nil {
		d.sealErr = errors.Join(d.sealErr, err)
		return err
	}
	d.physical.remaining = directoryPhysicalCatchupSlots
	return nil
}

// Called after actual native terminal allocation and before upstream admission.
// An exhausted catch-up vector refuses the next request until whole common
// publication permits renewing its targets. No request Close releases it.
func (d *AuthenticatedDirectory) EnsurePhysicalCatchup(ctx context.Context, native *Log, owner *AuthenticatedLedgerProjection) error {
	if d == nil || native == nil || owner == nil {
		return ErrProjectionSource
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := errors.Join(ctx.Err(), d.closeCtx.Err(), d.sealErr); err != nil {
		return err
	}
	p := d.physical
	if p == nil || p.native != native || p.ledger != owner {
		return resourcebudget.ErrExhausted
	}
	native.mu.Lock()
	defer native.mu.Unlock()
	reference, end, err := d.physicalSourceLocked(native, owner, p.selection)
	if err == nil && p.window == nil && p.remaining == 0 {
		p.window, err = d.store.ReservePhysicalTerminals(ctx, directoryPhysicalCatchupSlots)
		if err == nil {
			p.remaining = directoryPhysicalCatchupSlots
		}
	}
	if err == nil && p.remaining < directoryPhysicalCatchupSlots {
		return resourcebudget.ErrExhausted
	}
	if err == nil {
		err = d.store.RecordPhysicalNativeRange(reference, end)
	}
	if err != nil {
		d.sealErr = errors.Join(d.sealErr, err)
	}
	return err
}

func (d *AuthenticatedDirectory) physicalCatchupUpdateLocked(ctx context.Context, apply func(context.Context) error) (resultErr error) {
	p := d.physical
	if p == nil {
		return apply(ctx)
	}
	defer func() {
		if resultErr != nil {
			d.sealErr = errors.Join(d.sealErr, resultErr)
		}
	}()
	if p.window == nil || p.remaining < 1 {
		return resourcebudget.ErrExhausted
	}
	u, err := d.store.PreparePhysicalUpdate(ctx, p.window)
	if err != nil {
		return err
	}
	p.remaining--
	err = apply(u.Bind(ctx))
	// The source facts already exist durably. A failed derived update retains
	// accepted debt, regardless of this worker's cancellation or return value.
	return errors.Join(err, u.Finish(true, err))
}

// Retire only after the independently authenticated common publisher selects
// this exact completed directory. Ordinary component publication is not enough.
func (d *AuthenticatedDirectory) RetirePhysicalCatchupAfterCommon(ctx context.Context, native *Log, owner *AuthenticatedLedgerProjection, lease *MaintenanceLease, publisher *authenticatedstate.SealCandidatePublisher) error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	enabled := d.physical != nil
	d.mu.Unlock()
	if !enabled {
		return nil
	}
	if native == nil || owner == nil || lease == nil {
		return ErrProjectionSource
	}
	if replicated, err := lease.ValidateOwner(native); err != nil || replicated {
		return errors.Join(ErrProjectionSource, err)
	}
	if err := owner.provePhysicalRecoveryNoDebt(ctx); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	native.mu.Lock()
	defer native.mu.Unlock()
	if publisher == nil || native.startupProvenance == nil || publisher != native.startupProvenance.publisher || d.physical.native != native || d.physical.ledger != owner || d.anchor.Head != (Watermark{Generation: native.generation, Sequence: native.sequence, Offset: native.offset}) || d.anchor.Hash != native.chainHash || d.anchor.Epoch != native.chainEpoch || d.segments != native.generation-1 {
		return ErrProjectionSource
	}
	seal, found, err := publisher.CurrentContext(ctx)
	if err != nil || !found {
		return errors.Join(ErrProjectionSource, err)
	}
	root, err := d.store.SealRootReference(ctx, authenticatedDirectorySchema)
	if err != nil {
		return err
	}
	selected := false
	for _, saved := range seal.Roots[:seal.RootCount] {
		selected = selected || saved == root
	}
	if !selected || seal.Positions[0] != authenticatedDirectoryPosition(d.anchor) {
		return ErrProjectionSource
	}
	if err := d.physical.window.Close(); err != nil {
		d.sealErr = errors.Join(d.sealErr, err)
		return err
	}
	d.physical.window, d.physical.remaining = nil, 0
	if err := d.store.RetirePhysicalPreparationKeepingControls(ctx); err != nil {
		d.sealErr = errors.Join(d.sealErr, err)
		return err
	}
	d.physical.closedQualified = true
	return nil
}

// Bind only after NativeLedger has consumed the actual whole publication and
// adopted its fingerprint. The next admission, rather than shutdown, pays for
// a new catch-up pool. Captured control banks survive this selection change.
func (d *AuthenticatedDirectory) BindPhysicalCommonSelection(ctx context.Context, native *Log, owner *AuthenticatedLedgerProjection, selection [32]byte) error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	enabled := d.physical != nil
	d.mu.Unlock()
	if !enabled {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	native.mu.Lock()
	defer native.mu.Unlock()
	if d.physical.window != nil || d.physical.remaining != 0 {
		return authenticatedstate.ErrRecovery
	}
	reference, end, err := d.physicalSourceLocked(native, owner, selection)
	if err == nil {
		err = d.store.BindPhysicalSelectionDigest(selection, reference)
	}
	if err == nil {
		err = d.store.RecordPhysicalNativeRange(reference, end)
	}
	if err != nil {
		d.sealErr = errors.Join(d.sealErr, err)
		return err
	}
	d.physical.selection = selection
	return nil
}

// Close may retire an unused pool only when its actual native owner is still
// at the independently selected completed root with no original obligations.
// An unpublished catch-up or unknown exit retains the signed pool for recovery.
// Caller holds d.mu; the saved anchor closes the race with a new native append.
func (d *AuthenticatedDirectory) closePhysicalCatchupLocked(ctx context.Context) error {
	p := d.physical
	if p == nil {
		return nil
	}
	if p.ha && (p.window != nil || !p.closedQualified) {
		return authenticatedstate.ErrRecovery
	}
	if p.window == nil && p.closedQualified {
		p.native.mu.Lock()
		defer p.native.mu.Unlock()
		original := p.native.physical.Load()
		if original == nil || original.selection != p.selection || p.native.ioClosed && p.native.closeErr != nil {
			return authenticatedstate.ErrRecovery
		}
		// A successful original common proof survives actual native FD exit,
		// but never substitutes for the publisher's current independent root.
		if err := p.native.checkPhysicalCloseSelectionLocked(ctx); err != nil {
			return err
		}
		return d.verifyPhysicalCloseCommonRootLocked(ctx, p, d.anchor)
	}
	anchor, err := p.ledger.ReadReplayAnchor(ctx)
	if err != nil {
		return err
	}
	if err := p.ledger.provePhysicalRecoveryNoDebt(ctx); err != nil {
		return err
	}
	p.native.mu.Lock()
	defer p.native.mu.Unlock()
	if anchor != d.anchor || anchor.Head != (Watermark{Generation: p.native.generation, Sequence: p.native.sequence, Offset: p.native.offset}) || anchor.Hash != p.native.chainHash || anchor.Epoch != p.native.chainEpoch || p.native.startupProvenance == nil {
		return authenticatedstate.ErrRecovery
	}
	if err := d.verifyPhysicalCloseCommonRootLocked(ctx, p, anchor); err != nil {
		return err
	}
	if err := p.window.Close(); err != nil {
		return err
	}
	p.window, p.remaining = nil, 0
	p.closedQualified = true
	return nil
}

// Both component Close orders use the same independently current root check.
// Caller holds Directory.mu and its actual native owner's mutex.
func (d *AuthenticatedDirectory) verifyPhysicalCloseCommonRootLocked(ctx context.Context, p *directoryPhysicalOwner, anchor ReplayAnchor) error {
	if p.native.startupProvenance == nil || p.native.startupProvenance.publisher == nil {
		return ErrProjectionSource
	}
	seal, found, err := p.native.startupProvenance.publisher.CurrentContext(ctx)
	if err != nil || !found {
		return errors.Join(ErrProjectionSource, err)
	}
	root, err := d.store.SealRootReference(ctx, authenticatedDirectorySchema)
	if err != nil {
		return err
	}
	selected := false
	for _, r := range seal.Roots[:seal.RootCount] {
		selected = selected || r == root
	}
	if !selected || seal.Positions[0] != authenticatedDirectoryPosition(anchor) {
		return authenticatedstate.ErrRecovery
	}
	return nil
}

// InstallPhysicalCatchupCoverage captures this actual selected domain owner.
// Native invokes it under native.mu before source bytes may be accepted; it
// never takes Directory.mu or borrows caller-supplied references as authority.
func (d *AuthenticatedDirectory) InstallPhysicalCatchupCoverage(ctx context.Context, native *Log, owner *AuthenticatedLedgerProjection) error {
	if d == nil || native == nil || owner == nil {
		return ErrProjectionSource
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	p := d.physical
	if p == nil || p.native != native || p.ledger != owner {
		return ErrProjectionSource
	}
	return native.BindPhysicalDerivedCoverage(ctx, owner, PhysicalDerivedDirectory, d.physicalCatchupCoverage(p, owner))
}

// This fixed callback never acquires Directory.mu. Its original captured
// owner is registered only by the independently qualified domain constructor.
func (d *AuthenticatedDirectory) physicalCatchupCoverage(p *directoryPhysicalOwner, owner *AuthenticatedLedgerProjection) PhysicalDerivedCoverage {
	return func(ctx context.Context, actual *Log, reference authenticatedstate.PhysicalNativeReference, end int64, stage PhysicalDerivedStage, futureFrames int) error {
		if actual != p.native || d.physical != p || p.ledger != owner || owner.selectedNative.Load() != actual || owner.budget != d.budget {
			return ErrProjectionSource
		}
		original := actual.physical.Load()
		if original == nil || original.projection != owner || original.selection != p.selection || original.failure != nil {
			return ErrProjectionSource
		}
		if stage == PhysicalDerivedClose {
			if !p.closedQualified {
				return authenticatedstate.ErrRecovery
			}
			return nil
		}
		// Every new native mutation withdraws the previous common-close proof.
		// Only the original pre-acceptance lane may fund a replacement pool;
		// an already accepted append cannot allocate its completion targets.
		p.closedQualified = false
		if futureFrames < 0 {
			return ErrProjectionSource
		}
		if (stage == PhysicalDerivedReserve || stage == PhysicalDerivedRoll) && p.window == nil && p.remaining == 0 {
			if err := d.store.RecordPhysicalNativeRange(reference, end); err != nil {
				return err
			}
			window, err := d.store.ReservePhysicalTerminals(ctx, directoryPhysicalCatchupSlots)
			if err != nil {
				return err
			}
			p.window, p.remaining = window, directoryPhysicalCatchupSlots
		}
		if p.window == nil || p.remaining == 0 {
			return resourcebudget.ErrExhausted
		}
		if actual.generation-1 < d.segments {
			return ErrProjectionSource
		}
		descriptors := actual.generation - 1 - d.segments
		if stage == PhysicalDerivedRoll {
			descriptors++
		}
		if (descriptors+62)/63+1 > uint64(p.remaining) {
			return resourcebudget.ErrExhausted
		}
		if stage == PhysicalDerivedSuccessor {
			// A general range update cannot qualify a different native birth.
			// The original native Roll lane must transfer that specific receipt.
			return actual.TransferPhysicalDerivedNativeReceipt(ctx, d.store)
		}
		return d.store.RecordPhysicalNativeRange(reference, end)
	}
}

func (d *AuthenticatedDirectory) acquirePhysicalCatchupWork(ctx context.Context, p *directoryPhysicalOwner) error {
	if p.work != nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	requirements := physicalDirectoryCheckpointWorkspaceRequirements()
	if p.ha {
		requirements = HAPhysicalCheckpointWorkspaceRequirements()
	}
	var ram int64
	for _, r := range requirements {
		ram += r.RAM
	}
	work, err := d.budget.TryAcquireCompletableWork(resourcebudget.IO, resourcebudget.Cost{RAM: 4096}, resourcebudget.Cost{RAM: ram}, requirements)
	if err != nil {
		return err
	}
	p.work = work
	return nil
}

func physicalDirectoryCheckpointWorkspaceRequirements() []resourcebudget.WorkspaceRequirement {
	requirements := NativeApplyWorkspaceRequirements()
	// The publication's source buffer remains live through all seven root
	// freezes. Their actual IO tickets and the publisher need separate slots.
	requirements = append(requirements,
		resourcebudget.WorkspaceRequirement{Class: resourcebudget.IO, RAM: AuthenticatedLedgerSourceRAMBytes + 128<<10},
		resourcebudget.WorkspaceRequirement{Class: resourcebudget.IO, RAM: 128 << 10})
	for i := 0; i < 8; i++ {
		requirements = append(requirements, resourcebudget.WorkspaceRequirement{Class: resourcebudget.IO, RAM: 4096})
	}
	// Both native retry capsules remain live together through common Publish.
	// Each captured FD and each decoded capsule therefore needs its own IO
	// slot, in addition to the source and publisher buffers above.
	requirements = append(requirements,
		resourcebudget.WorkspaceRequirement{Class: resourcebudget.IO, RAM: auditlimits.StartupCachePersistRAMBytes},
		resourcebudget.WorkspaceRequirement{Class: resourcebudget.IO, RAM: auditlimits.StartupCacheReadRAMBytes},
		resourcebudget.WorkspaceRequirement{Class: resourcebudget.IO, RAM: governancelimits.StartupCachePersistRAMBytes},
		resourcebudget.WorkspaceRequirement{Class: resourcebudget.IO, RAM: governancelimits.StartupCacheReadRAMBytes})
	// Returning the live Usage owner to maintenance overlaps its original
	// Ledger snapshot with a Usage snapshot. Both have their own Background
	// pin; the COW update slot must remain available while the source is pinned.
	for i := 0; i < 2; i++ {
		requirements = append(requirements, resourcebudget.WorkspaceRequirement{Class: resourcebudget.Background, RAM: 4096})
	}
	return requirements
}

// HAPhysicalCheckpointWorkspaceRequirements includes the actual simultaneously
// held Member/Ordering qualification and bound Metadata native proof. It is a
// profile only; the original Directory owner must pay it before admission.
func HAPhysicalCheckpointWorkspaceRequirements() []resourcebudget.WorkspaceRequirement {
	return append(physicalDirectoryCheckpointWorkspaceRequirements(),
		resourcebudget.WorkspaceRequirement{Class: resourcebudget.IO, RAM: replicationlimits.MemberAuthorityRAMBytes},
		resourcebudget.WorkspaceRequirement{Class: resourcebudget.IO, RAM: replicationlimits.OrderingFrozenReadRAMBytes},
		resourcebudget.WorkspaceRequirement{Class: resourcebudget.IO, RAM: 4096},
		resourcebudget.WorkspaceRequirement{Class: resourcebudget.IO, RAM: 32 << 20})
}

// The actual shared owner calls this for its joined checkpoint/recovery work.
// The pool was paid before upstream admission, independently of HTTP lifetimes.
func (d *AuthenticatedDirectory) WithPhysicalCatchupWorkspace(ctx context.Context, visit func(context.Context) error) error {
	if d == nil || visit == nil {
		return ErrProjectionSource
	}
	d.mu.Lock()
	p := d.physical
	if p == nil {
		d.mu.Unlock()
		return visit(ctx)
	}
	if err := errors.Join(ctx.Err(), d.closeCtx.Err(), d.sealErr); err != nil {
		d.mu.Unlock()
		return err
	}
	work := p.work
	d.mu.Unlock()
	if work == nil {
		return authenticatedstate.ErrRecovery
	}
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(d.closeCtx, cancel)
	defer func() { stop(); cancel() }()
	return work.WithWorkspace(opCtx, visit)
}
