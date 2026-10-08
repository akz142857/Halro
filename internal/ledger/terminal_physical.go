package ledger

import (
	"context"
	"errors"
	"io"
	"os"
	"syscall"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"sync/atomic"
)

const terminalNativePhysicalBytes int64 = maxPayloadSize + 256

// PhysicalNativeCapacity is the same persistent growth owner used by native
// frames and auxiliary outputs. Held unused target bytes cannot be loaned to
// either producer; only an actual write or joined tail retirement converts them.
type PhysicalNativeCapacity interface {
	ReservePhysicalRange(int64) error
	ConsumePhysicalRange(int64) error
	RetirePhysicalRange(int64) error
}

type nativePhysicalOwner struct {
	selection    [32]byte
	restoring    bool
	projection   *AuthenticatedLedgerProjection
	file         os.FileInfo
	generation   uint64
	ceiling      int64
	allocatedEnd int64
	capacity     PhysicalNativeCapacity
	debt         int
	failure      error
	active       int
	fee          resourcebudget.Permit
	feeHeld      bool
	derived      [2]PhysicalDerivedCoverage
	// Only the original Directory installer can register this identity. It
	// survives retiring its windows; a replacement owner cannot inherit it.
	derivedDirectory  *directoryPhysicalOwner
	derivedRequired   bool
	rollFrom          authenticatedstate.PhysicalNativeReference
	commonQualified   bool
	haCloseQualified  bool
	derivedReaderDebt *startupAuditReaderDebt
}

type terminalPhysicalKey struct{}
type physicalAppendKey struct{}
type physicalAppendKindKey struct{}

// WithPhysicalAppendKind describes the impending domain event. The actual
// encoded writer rechecks it; this marker grants no resource/role authority.
func WithPhysicalAppendKind(ctx context.Context, kind EventKind) context.Context {
	return context.WithValue(ctx, physicalAppendKindKey{}, kind)
}

// TerminalPhysicalWindow is one finite request's native/COW/control vector.
// Only the actual selected Runtime owner can create it. Context values never
// substitute for native/projection eligibility or physical allocation.
type TerminalPhysicalWindow struct {
	native                     *Log
	cow                        *authenticatedstate.PhysicalTerminalWindow
	remaining, claimed, active int // protected by native.mu
	replicaReceived            int // authenticated native records awaiting actual Apply
	closed                     bool
	done                       chan struct{}
	closeErr                   error
	closeDone                  chan struct{}
}

type physicalAppend struct {
	native   *Log
	window   *TerminalPhysicalWindow
	cow      *authenticatedstate.PhysicalUpdate
	offset   int64
	accepted atomic.Bool // actual writer acceptance, never observer cancellation
}

func (p *AuthenticatedLedgerProjection) EnablePhysicalTerminals(ctx context.Context, native *Log, growthBytes int64, growthFee resourcebudget.Permit, capacity PhysicalNativeCapacity) error {
	return p.enablePhysicalTerminals(ctx, native, growthBytes, growthFee, capacity, [32]byte{})
}

// EnableQualifiedPhysicalTerminals requires an independently selected complete
// common seal fingerprint. Complete actual startup recovery must prove that
// any prior accepted terminal obligations are resolved before spare retirement.
func (p *AuthenticatedLedgerProjection) EnableQualifiedPhysicalTerminals(ctx context.Context, native *Log, growthBytes int64, growthFee resourcebudget.Permit, capacity PhysicalNativeCapacity, selection [32]byte) error {
	if selection == ([32]byte{}) {
		return ErrProjectionSource
	}
	if err := p.WithRecoveryView(ctx, func(v AuthenticatedRecoveryView) error {
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
	}); err != nil {
		return err
	}
	return p.enablePhysicalTerminals(ctx, native, growthBytes, growthFee, capacity, selection)
}

func (p *AuthenticatedLedgerProjection) enablePhysicalTerminals(ctx context.Context, native *Log, growthBytes int64, growthFee resourcebudget.Permit, capacity PhysicalNativeCapacity, selection [32]byte) error {
	if p == nil || native == nil || growthBytes <= 0 || growthBytes > 1<<50 || capacity == nil {
		return resourcebudget.ErrInvalid
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.ready || p.selectedNative.Load() != native || p.closeContext.Err() != nil {
		return ErrProjectionSource
	}
	native.mu.Lock()
	defer native.mu.Unlock()
	if native.options.Replica || native.options.BeforeDurable != nil || native.options.AfterDurable != nil || native.options.ReserveAppendDisk == nil || native.status.Load() != AccountingHealthy || len(native.chainKey) != 32 {
		return ErrProjectionSource
	}
	return p.installPhysicalTerminalsLocked(ctx, native, growthBytes, growthFee, capacity, selection)
}

// Caller owns the original projection/native frozen lane and has independently
// established its own mode-specific source and complete NoDebt qualification.
// Public Standalone entry points retain their original strict mode guards.
func (p *AuthenticatedLedgerProjection) installPhysicalTerminalsLocked(ctx context.Context, native *Log, growthBytes int64, growthFee resourcebudget.Permit, capacity PhysicalNativeCapacity, selection [32]byte) error {
	if existing := native.physical.Load(); existing != nil {
		if !existing.restoring && existing.projection == p && existing.selection == selection && existing.debt == 0 && existing.active == 0 && existing.failure == nil {
			return native.checkPhysicalNativeLocked()
		}
		return resourcebudget.ErrInvalid
	}
	info, err := native.file.Stat()
	named, nameErr := os.Lstat(native.path)
	parent, parentErr := os.Lstat(native.directory)
	if err != nil || nameErr != nil || parentErr != nil || !info.Mode().IsRegular() || !named.Mode().IsRegular() || !os.SameFile(info, named) || info.Size() != native.offset || native.directoryInfo == nil || !parent.IsDir() || !os.SameFile(parent, native.directoryInfo) {
		return errors.Join(ErrProjectionSource, err, nameErr, parentErr)
	}
	fee, err := growthFee.RetainCovering(p.budget, resourcebudget.Cost{Disk: growthBytes})
	if err != nil {
		return err
	}
	physical := &nativePhysicalOwner{projection: p, file: info, generation: native.generation, ceiling: native.offset + growthBytes, fee: fee, feeHeld: true, capacity: capacity, allocatedEnd: native.offset, selection: selection}
	native.physical.Store(physical)
	if selection != ([32]byte{}) {
		reference, err := native.physicalNativeReferenceLocked()
		if err == nil {
			var end int64
			end, err = p.store.EnablePhysicalTerminalQualified(ctx, selection, reference, func(recorded authenticatedstate.PhysicalNativeReference, end int64) error {
				workspace, err := p.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 2 << 20})
				if err != nil {
					return err
				}
				verifier := &chainVerifier{key: native.chainKey, hash: native.chainAnchor, sequence: native.startSequence}
				head, partial, scanErr := scan(io.NewSectionReader(native.file, 0, recorded.EOF), native.generation, 0, native.startSequence, nil, verifier)
				if partial || head.Offset != recorded.EOF || head.Sequence != recorded.Sequence || verifier.hash != recorded.Hash {
					scanErr = errors.Join(scanErr, ErrProjectionSource)
				}
				if err := errors.Join(scanErr, workspace.Release()); err != nil {
					return err
				}
				if err := resourcebudget.CheckPhysicalTail(native.file, native.offset, end); err != nil {
					return err
				}
				bytes := end - native.offset
				if bytes > physical.ceiling-native.offset {
					return resourcebudget.ErrExhausted
				}
				if bytes == 0 {
					return nil
				}
				if err := capacity.ReservePhysicalRange(bytes); err != nil {
					return err
				}
				physical.allocatedEnd = native.offset + bytes
				return nil
			})
			if err == nil {
				physical.allocatedEnd = end
			}
		}
		if err != nil {
			physical.failure = err
			native.status.RequireRecovery()
			return err
		}
		if physical.allocatedEnd > native.offset {
			if err := resourcebudget.RetirePhysicalTail(native.file, native.offset, physical.allocatedEnd); err != nil {
				physical.failure = err
				native.status.RequireRecovery()
				return err
			}
			if err := capacity.RetirePhysicalRange(physical.allocatedEnd - native.offset); err != nil {
				physical.failure = err
				native.status.RequireRecovery()
				return err
			}
			physical.allocatedEnd = native.offset
		}
	} else if err := p.store.EnablePhysicalTerminal(ctx); err != nil {
		physical.failure = err
		native.status.RequireRecovery()
		return err
	}
	if err := native.allocatePhysicalTargetLocked(native.offset + 4096); err != nil {
		physical.failure = err
		native.status.RequireRecovery()
		return err
	}
	physical.allocatedEnd = native.offset + 4096
	// Qualify both allocation and retirement on this actual filesystem before
	// any upstream work is admitted. A failed probe retains its whole owner.
	if err := resourcebudget.RetirePhysicalTail(native.file, native.offset, physical.allocatedEnd); err != nil {
		physical.failure = err
		native.status.RequireRecovery()
		return err
	}
	if err := capacity.RetirePhysicalRange(physical.allocatedEnd - native.offset); err != nil {
		physical.failure = err
		native.status.RequireRecovery()
		return err
	}
	physical.allocatedEnd = native.offset
	if selection != ([32]byte{}) {
		reference, err := native.physicalNativeReferenceLocked()
		if err == nil {
			err = p.store.RecordPhysicalNativeRange(reference, native.offset)
		}
		if err != nil {
			physical.failure = err
			native.status.RequireRecovery()
			return err
		}
	}
	return nil
}

func (l *Log) physicalNativeReferenceLocked() (authenticatedstate.PhysicalNativeReference, error) {
	id, err := resourcebudget.CapturePhysicalIdentity(l.file)
	if err != nil {
		return authenticatedstate.PhysicalNativeReference{}, err
	}
	f, err := os.Open(l.directory)
	if err != nil {
		return authenticatedstate.PhysicalNativeReference{}, err
	}
	parent, identityErr := resourcebudget.CapturePhysicalIdentity(f)
	closeErr := ownedfile.CloseFile(f)
	if err := errors.Join(identityErr, closeErr); err != nil {
		return authenticatedstate.PhysicalNativeReference{}, err
	}
	return authenticatedstate.PhysicalNativeReference{File: id, Parent: parent, Generation: l.generation, Sequence: l.chainSequence, Hash: l.chainHash, EOF: l.offset}, nil
}

func (l *Log) BindPhysicalSelectionDigest(selection [32]byte) error {
	return l.BindPhysicalSelectionDigestContext(context.Background(), selection)
}

func (l *Log) BindPhysicalSelectionDigestContext(ctx context.Context, selection [32]byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	o := l.physical.Load()
	if o == nil {
		return nil
	}
	if selection == ([32]byte{}) || o.failure != nil || o.debt != 0 || o.active != 0 {
		o.failure = errors.Join(authenticatedstate.ErrRecovery, o.failure)
		l.status.RequireRecovery()
		return o.failure
	}
	if err := l.checkPhysicalNativeLocked(); err != nil {
		o.failure = err
		l.status.RequireRecovery()
		return errors.Join(authenticatedstate.ErrRecovery, err)
	}
	if err := l.checkPhysicalCommonSelectionLocked(ctx, selection); err != nil {
		o.failure = err
		l.status.RequireRecovery()
		return errors.Join(authenticatedstate.ErrRecovery, err)
	}
	if l.startupProvenance != nil {
		if err := o.projection.provePhysicalRecoveryNoDebt(ctx); err != nil {
			o.failure = err
			l.status.RequireRecovery()
			return errors.Join(authenticatedstate.ErrRecovery, err)
		}
	}
	reference, err := l.physicalNativeReferenceLocked()
	if err == nil {
		err = o.projection.store.BindPhysicalSelectionDigest(selection, reference)
	}
	if err != nil {
		o.failure = err
		l.status.RequireRecovery()
		return errors.Join(authenticatedstate.ErrRecovery, err)
	}
	o.selection = selection
	o.restoring = false
	o.commonQualified = true
	return nil
}

func (l *Log) checkPhysicalNativeLocked() error {
	o := l.physical.Load()
	if o == nil {
		return resourcebudget.ErrPhysicalAllocation
	}
	if o.failure != nil {
		return errors.Join(authenticatedstate.ErrRecovery, o.failure)
	}
	if l.closeContext.Err() != nil || l.file == nil || l.status.Load() != AccountingHealthy {
		return authenticatedstate.ErrRecovery
	}
	return l.verifyPhysicalNativeFileLocked()
}

func (l *Log) verifyPhysicalNativeFileLocked() error {
	o := l.physical.Load()
	actual, err := l.file.Stat()
	named, nameErr := os.Lstat(l.path)
	parent, parentErr := os.Lstat(l.directory)
	if err != nil || nameErr != nil || parentErr != nil || !os.SameFile(o.file, actual) || !os.SameFile(o.file, named) || !named.Mode().IsRegular() || o.generation != l.generation || actual.Size() != l.offset || !parent.IsDir() || !os.SameFile(parent, l.directoryInfo) {
		o.failure = errors.Join(ErrProjectionSource, err, nameErr, parentErr)
		l.status.RequireRecovery()
		return o.failure
	}
	return nil
}

// RetirePhysicalPreparationForMaintenance is a real owner handoff boundary.
// It requires the current native maintenance lease and joins all unused target
// ranges before a common selection or COW pack can change. Any uncertain IO
// stops new upstream admission while preserving its fee and debt.
func (l *Log) RetirePhysicalPreparationForMaintenance(ctx context.Context, lease *MaintenanceLease) error {
	if l.physical.Load() == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if replicated, err := lease.ValidateOwner(l); err != nil || replicated {
		return errors.Join(ErrProjectionSource, err)
	}
	// A zero window/debt count is insufficient: accepted requests may still
	// lack a terminal fact. Authenticate both projection streams while the
	// real maintenance lease excludes native mutation, before taking l.mu.
	if err := l.physical.Load().projection.VerifyPhysicalNoDebt(ctx, l, lease); err != nil {
		l.status.RequireRecovery()
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	o := l.physical.Load()
	if o.failure != nil || o.debt != 0 || o.active != 0 {
		return errors.Join(authenticatedstate.ErrRecovery, o.failure)
	}
	err := l.checkPhysicalNativeLocked()
	if err == nil {
		err = l.retirePhysicalTailLocked(true)
	}
	if err != nil {
		o.failure = err
		l.status.RequireRecovery()
	}
	return err
}

func (l *Log) allocatePhysicalNativeLocked(extra int) error {
	if err := l.checkPhysicalNativeLocked(); err != nil {
		return err
	}
	o := l.physical.Load()
	if o.restoring {
		return authenticatedstate.ErrRecovery
	}
	// At most 32 accepted terminal facts plus the one serialized ordinary
	// native append may overlap. The latter must live above all terminal debt.
	if extra < 1 || extra > resourcebudget.MaxCompletionBatch || o.debt > resourcebudget.MaxCompletionBatch+1-extra {
		return resourcebudget.ErrExhausted
	}
	bytes := int64(o.debt+extra) * terminalNativePhysicalBytes
	if bytes > o.ceiling-l.offset {
		return resourcebudget.ErrExhausted
	}
	return l.allocatePhysicalTargetLocked(l.offset + bytes)
}

func (l *Log) allocatePhysicalTargetLocked(end int64) error {
	o := l.physical.Load()
	delta := end - o.allocatedEnd
	if delta > 0 {
		if err := o.capacity.ReservePhysicalRange(delta); err != nil {
			return err
		}
	}
	if o.selection != ([32]byte{}) {
		reference, err := l.physicalNativeReferenceLocked()
		if err == nil {
			err = o.projection.store.EnsurePhysicalReceipt(o.selection, reference)
		}
		if err == nil {
			recordedEnd := end
			if o.allocatedEnd > recordedEnd {
				recordedEnd = o.allocatedEnd
			}
			err = o.projection.store.RecordPhysicalNativeRange(reference, recordedEnd)
		}
		if err != nil {
			o.failure = err
			l.status.RequireRecovery()
			if end > o.allocatedEnd {
				o.allocatedEnd = end
			}
			return err
		}
	}
	err := resourcebudget.AllocatePhysicalRange(l.file, l.offset, end-l.offset)
	// A failed syscall/Sync can still have allocated blocks. Retain the upper
	// bound and stop structural retirement unless the actual operation joined.
	definiteCapacityRefusal := err != nil && !errors.Is(err, resourcebudget.ErrPhysicalAllocationUncertain) && (errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT))
	if definiteCapacityRefusal && delta > 0 {
		retireErr := o.capacity.RetirePhysicalRange(delta)
		err = errors.Join(err, retireErr)
		if retireErr != nil {
			definiteCapacityRefusal = false
			o.allocatedEnd = end
		}
	} else if delta > 0 {
		o.allocatedEnd = end
	}
	if err != nil && !definiteCapacityRefusal {
		o.failure = err
		l.status.RequireRecovery()
	}
	return err
}

func (l *Log) retirePhysicalRollTailLocked() error {
	return l.retirePhysicalTailLocked(false)
}

func (l *Log) retirePhysicalTailLocked(keepControls bool) error {
	o := l.physical.Load()
	if o == nil {
		return nil
	}
	if o.debt != 0 || o.active != 0 {
		return resourcebudget.ErrPhysicalAllocation
	}
	if o.failure != nil || l.status.Load() != AccountingHealthy {
		return errors.Join(authenticatedstate.ErrRecovery, o.failure)
	}
	if err := l.verifyPhysicalNativeFileLocked(); err != nil {
		return err
	}
	if o.allocatedEnd > l.offset {
		if err := resourcebudget.RetirePhysicalTail(l.file, l.offset, o.allocatedEnd); err != nil {
			o.failure = err
			l.status.RequireRecovery()
			return err
		}
		if err := o.capacity.RetirePhysicalRange(o.allocatedEnd - l.offset); err != nil {
			o.failure = err
			l.status.RequireRecovery()
			return err
		}
	}
	o.allocatedEnd = l.offset
	if keepControls {
		if err := o.projection.store.RetirePhysicalPreparationKeepingControls(context.Background()); err != nil {
			return err
		}
		reference, err := l.physicalNativeReferenceLocked()
		if err != nil {
			return err
		}
		return o.projection.store.RecordPhysicalNativeRange(reference, l.offset)
	}
	return o.projection.store.RetirePhysicalPreparation(context.Background())
}

// Called only after the real previous descriptor's Close succeeded and the
// authenticated Roll selected this exact successor. Spending the old file's
// committed bytes reduces the original fee's remaining growth allowance.
func (l *Log) transferPhysicalRollOwnerLocked(previousBytes int64) error {
	o := l.physical.Load()
	if o == nil {
		return nil
	}
	remaining := o.ceiling - previousBytes
	if remaining < terminalNativePhysicalBytes {
		return resourcebudget.ErrExhausted
	}
	actual, err := l.file.Stat()
	named, nameErr := os.Lstat(l.path)
	parent, parentErr := os.Lstat(l.directory)
	if err != nil || nameErr != nil || parentErr != nil || !actual.Mode().IsRegular() || !named.Mode().IsRegular() || !os.SameFile(actual, named) || actual.Size() != 0 || l.offset != 0 || !parent.IsDir() || !os.SameFile(parent, l.directoryInfo) {
		return errors.Join(ErrProjectionSource, err, nameErr, parentErr)
	}
	// The old unused tail has actually retired. Keep the same capacity owner;
	// auxiliary products reserved during Roll reduce what can be lent here.
	o.allocatedEnd = 0
	if err := l.allocatePhysicalTargetLocked(terminalNativePhysicalBytes); err != nil {
		return err
	}
	o.file, o.generation, o.ceiling, o.allocatedEnd = actual, l.generation, remaining, terminalNativePhysicalBytes
	return l.preparePhysicalDerivedLocked(context.Background(), PhysicalDerivedSuccessor, 0)
}

func (l *Log) ReservePhysicalTerminals(ctx context.Context, count int) (*TerminalPhysicalWindow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.allocatePhysicalNativeLocked(count); err != nil {
		return nil, err
	}
	o := l.physical.Load()
	if err := l.preparePhysicalDerivedLocked(ctx, PhysicalDerivedReserve, o.debt+count); err != nil {
		return nil, err
	}
	cow, err := o.projection.store.ReservePhysicalTerminals(ctx, count)
	if err != nil {
		l.retainPhysicalSourceFailureLocked(err)
		return nil, err
	}
	o.debt += count
	return &TerminalPhysicalWindow{native: l, cow: cow, remaining: count}, nil
}

func (l *Log) retainPhysicalSourceFailureLocked(err error) {
	if errors.Is(err, ownedfile.ErrRetirement) || errors.Is(err, ownedfile.ErrReplaced) || errors.Is(err, authenticatedstate.ErrRecovery) || errors.Is(err, authenticatedstate.ErrCorrupt) || errors.Is(err, resourcebudget.ErrPhysicalAllocationUncertain) {
		l.physical.Load().failure = err
		l.status.RequireRecovery()
	}
}

func (w *TerminalPhysicalWindow) Bind(ctx context.Context) context.Context {
	return context.WithValue(ctx, terminalPhysicalKey{}, w)
}

func (w *TerminalPhysicalWindow) Close() error {
	if w == nil {
		return nil
	}
	l := w.native
	l.mu.Lock()
	if w.closed {
		done := w.closeDone
		l.mu.Unlock()
		<-done
		l.mu.Lock()
		err := w.closeErr
		l.mu.Unlock()
		return err
	}
	w.closed = true
	w.closeDone = make(chan struct{})
	done := w.done
	l.mu.Unlock()
	if done != nil {
		<-done
	}
	err := w.cow.Close()
	l.mu.Lock()
	defer l.mu.Unlock()
	if err == nil {
		l.physical.Load().debt -= w.remaining
		w.remaining = 0
	} else {
		l.physical.Load().failure = err
		l.status.RequireRecovery()
	}
	w.closeErr = err
	close(w.closeDone)
	return err
}

// WithPreparedPhysicalAppend runs under the accounting append lane. Ordinary
// writes allocate their maximum extent ABOVE outstanding terminal debt and
// prepare the actual root inode before accepting any native bytes.
func (p *AuthenticatedLedgerProjection) WithPreparedPhysicalAppend(ctx context.Context, visit func(context.Context) error) (resultErr error) {
	native := p.selectedNative.Load()
	if native == nil || native.physical.Load() == nil {
		return visit(ctx)
	}
	p.mu.RLock()
	if !p.ready || p.closeContext.Err() != nil {
		p.mu.RUnlock()
		return ErrProjectionUnavailable
	}
	native.mu.Lock()
	o := native.physical.Load()
	var w *TerminalPhysicalWindow
	if resourcebudget.InCompletionContinuation(ctx) {
		kind, _ := ctx.Value(physicalAppendKindKey{}).(EventKind)
		if kind != EventAttemptSettled && kind != EventRequestFinalized {
			native.mu.Unlock()
			p.mu.RUnlock()
			return resourcebudget.ErrPhysicalAllocation
		}
		w, _ = ctx.Value(terminalPhysicalKey{}).(*TerminalPhysicalWindow)
		if w == nil || w.native != native || w.closed || w.claimed >= w.remaining || o.failure != nil {
			native.mu.Unlock()
			p.mu.RUnlock()
			return resourcebudget.ErrPhysicalAllocation
		}
		if err := native.checkPhysicalNativeLocked(); err != nil {
			native.mu.Unlock()
			p.mu.RUnlock()
			return err
		}
	} else if err := native.allocatePhysicalNativeLocked(1); err != nil {
		native.mu.Unlock()
		p.mu.RUnlock()
		return err
	}
	futureFrames := o.debt
	if w == nil {
		futureFrames++
	}
	if err := native.preparePhysicalDerivedLocked(ctx, PhysicalDerivedAppend, futureFrames); err != nil {
		native.mu.Unlock()
		p.mu.RUnlock()
		return err
	}
	var cw *authenticatedstate.PhysicalTerminalWindow
	if w != nil {
		cw = w.cow
	}
	u, err := p.store.PreparePhysicalUpdate(ctx, cw)
	if err != nil {
		native.retainPhysicalSourceFailureLocked(err)
		native.mu.Unlock()
		p.mu.RUnlock()
		return err
	}
	op := &physicalAppend{native: native, window: w, cow: u, offset: native.offset}
	o.active++
	if w != nil {
		w.claimed++
		if w.active == 0 {
			w.done = make(chan struct{})
		}
		w.active++
	}
	native.mu.Unlock()
	p.mu.RUnlock()
	returned := false
	defer func() {
		if !returned {
			resultErr = errors.Join(resultErr, authenticatedstate.ErrRecovery)
		}
		accepted := op.accepted.Load() || !returned
		finishErr := op.cow.Finish(accepted, resultErr)
		native.mu.Lock()
		o.active--
		if accepted {
			o.commonQualified = false
		}
		if accepted && resultErr != nil || finishErr != nil {
			o.failure = errors.Join(authenticatedstate.ErrRecovery, resultErr, finishErr)
			native.status.RequireRecovery()
		} else if w != nil {
			o.debt--
			w.remaining--
			w.claimed--
		}
		if w != nil {
			w.active--
			if w.active == 0 {
				close(w.done)
				w.done = nil
			}
		}
		native.mu.Unlock()
		resultErr = errors.Join(resultErr, finishErr)
	}()
	resultErr = visit(op.cow.Bind(context.WithValue(ctx, physicalAppendKey{}, op)))
	returned = true
	return resultErr
}

func (l *Log) validatePhysicalAppendLocked(batch []appendRequest, bytes int64) (*physicalAppend, error) {
	if l.physical.Load() == nil {
		return nil, nil
	}
	if len(batch) != 1 || bytes <= 0 || bytes > terminalNativePhysicalBytes {
		return nil, resourcebudget.ErrPhysicalAllocation
	}
	op, _ := batch[0].ctx.Value(physicalAppendKey{}).(*physicalAppend)
	if op == nil || op.native != l || op.offset != l.offset || op.accepted.Load() {
		return nil, resourcebudget.ErrPhysicalAllocation
	}
	if op.window != nil && batch[0].event.Kind != EventAttemptSettled && batch[0].event.Kind != EventRequestFinalized {
		return nil, resourcebudget.ErrPhysicalAllocation
	}
	if err := l.checkPhysicalNativeLocked(); err != nil {
		return nil, err
	}
	return op, nil
}

// PhysicalTerminalDebt exposes only finite outstanding work, never permission
// to retire bytes or to use a different inode/generation.
func (l *Log) PhysicalTerminalDebt() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if o := l.physical.Load(); o != nil {
		return o.debt
	}
	return 0
}
