package metadatastate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/metadatalimits"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

// A request reserves one PricePin commit for each possible accepted attempt.
// Prepare remains above this debt; unchanged local rejection cleanup consumes
// the original prepaid terminal just like commit.
type TerminalPhysicalWindow struct {
	owner        *Baseline
	cow          *authenticatedstate.PhysicalTerminalWindow
	remaining    int
	closed       atomic.Bool
	work         resourcebudget.Permit
	completion   *resourcebudget.CompletableWork
	closeErr     error
	preparedPins [resourcebudget.MaxCompletionBatch]preparedPinDebt
}
type terminalWindowKey struct{}
type pricePinTerminalKey struct{}

func (w *TerminalPhysicalWindow) Bind(ctx context.Context) context.Context {
	return context.WithValue(ctx, terminalWindowKey{}, w)
}
func (w *TerminalPhysicalWindow) Close() error {
	if w == nil {
		return nil
	}
	b := w.owner
	b.mu.Lock()
	defer b.mu.Unlock()
	return w.closeLocked()
}
func (w *TerminalPhysicalWindow) closeLocked() error {
	if w.closed.Load() {
		return w.closeErr
	}
	if w.hasPreparedPins() {
		return w.retainForRecoveryLocked(authenticatedstate.ErrRecovery)
	}
	w.closed.Store(true)
	b := w.owner
	err := w.cow.Close()
	if err == nil {
		b.physicalDebt -= w.remaining
		w.remaining = 0
		err = w.completion.Close()
		if err == nil {
			err = w.work.Release()
		}
	}
	if err != nil {
		b.ready.Store(false)
		b.retirementErr = errors.Join(b.retirementErr, err)
	}
	w.closeErr = err
	return err
}
func (b *Baseline) EnableQualifiedPhysicalTerminals(ctx context.Context, publisher *authenticatedstate.SealCandidatePublisher, guard *lock.Lock, authority authenticatedstate.SealEligibility) error {
	if b == nil || !b.mu.TryLock() {
		return authenticatedstate.ErrBusy
	}
	defer b.mu.Unlock()
	if !b.ready.Load() || !b.selectedStartup || b.readOnly || b.haPrimaryPrepared != nil || b.haReplicaPrepared != nil || b.selectedPublisher != publisher || b.selectedGuard != guard {
		return ErrSource
	}
	if err := publisher.ValidateOwner(guard, b.budget); err != nil {
		return err
	}
	seal, found, err := publisher.Select(authority)
	if err != nil || !found || authority.Role != authenticatedstate.SealStandalone {
		return errors.Join(ErrSource, err)
	}
	if b.physicalEnabled {
		return nil
	}
	actual, err := b.sealReferenceLocked(ctx)
	if err != nil {
		return err
	}
	qualified := false
	for _, r := range seal.Roots[:seal.RootCount] {
		if r.Component == authenticatedstate.SealComponentMetadata && r.State.Scope == actual.Root.State.Scope {
			qualified = true
		}
	}
	if !qualified {
		return ErrSource
	}
	raw, err := json.Marshal(seal)
	if err != nil {
		return err
	}
	selection := sha256.Sum256(raw)
	err = b.journal.WithPhysicalNative(ctx, b.budget, func(q *metadatajournal.PhysicalNativeLoan) error {
		ref, err := q.Reference()
		if err != nil {
			return err
		}
		_, err = b.store.EnablePhysicalTerminalQualified(ctx, selection, ref, func(prior authenticatedstate.PhysicalNativeReference, end int64) error {
			return q.VerifyPrefix(ctx, prior, end)
		})
		if err != nil {
			return err
		}
		if err := q.RetireUnused(); err != nil {
			return err
		}
		return q.Enable()
	})
	if err != nil {
		return err
	}
	b.physicalEnabled = true
	return nil
}
func (b *Baseline) ReservePhysicalTerminals(ctx context.Context, count int) (*TerminalPhysicalWindow, error) {
	if b == nil || !b.mu.TryLock() {
		return nil, authenticatedstate.ErrBusy
	}
	defer b.mu.Unlock()
	if !b.ready.Load() || !b.physicalEnabled || count < 1 || count > resourcebudget.MaxCompletionBatch || b.physicalDebt > resourcebudget.MaxCompletionBatch-count {
		return nil, resourcebudget.ErrPhysicalAllocation
	}
	completion, err := b.acquirePhysicalCompletion()
	var work resourcebudget.Permit
	if completion != nil {
		work = completion.Active
	}
	if err != nil {
		return nil, err
	}
	err = b.withTerminalPhysicalNativeLocked(ctx, func(q *metadatajournal.PhysicalNativeLoan) error {
		ref, err := q.Reference()
		if err != nil {
			return err
		}
		n := int64(b.physicalDebt+count) * metadatajournal.PhysicalAppendBytes
		prior, err := q.PreparedEnd()
		if err != nil {
			return err
		}
		end := max(prior, ref.EOF+n)
		if err = b.store.RecordPhysicalNativeRange(ref, end); err != nil {
			return err
		}
		return q.Allocate(end)
	})
	if err != nil {
		return nil, errors.Join(err, completion.Close(), work.Release())
	}
	cow, err := b.store.ReservePhysicalTerminals(ctx, count)
	if err != nil {
		return nil, errors.Join(err, completion.Close(), work.Release())
	}
	w := &TerminalPhysicalWindow{owner: b, cow: cow, remaining: count, work: work, completion: completion}
	for i, prior := range b.physicalWindows {
		if prior == nil || prior.closed.Load() && prior.closeErr == nil {
			b.physicalWindows[i] = w
			break
		}
	}
	b.physicalDebt += count
	return w, nil
}
func (b *Baseline) physicalWork(ctx context.Context, cost resourcebudget.Cost) (resourcebudget.Permit, error) {
	if w, ok := ctx.Value(terminalWindowKey{}).(*TerminalPhysicalWindow); ok && w.owner == b && !w.closed.Load() {
		return w.work.RetainCovering(b.budget, cost)
	}
	return b.budget.TryAcquireContext(ctx, resourcebudget.IO, cost)
}
func (b *Baseline) preparePhysicalAppendLocked(ctx context.Context, ops []metadatajournal.Op) (context.Context, func(error) error, error) {
	if !b.physicalEnabled {
		return ctx, func(error) error { return nil }, nil
	}
	var w *TerminalPhysicalWindow
	if ctx.Value(pricePinTerminalKey{}) != nil {
		w, _ = ctx.Value(terminalWindowKey{}).(*TerminalPhysicalWindow)
		if w == nil || w.owner != b || w.closed.Load() || w.remaining <= 0 {
			return ctx, nil, resourcebudget.ErrPhysicalAllocation
		}
		if err := b.validatePhysicalPricePinLocked(ctx, ops); err != nil {
			return ctx, nil, err
		}
	}
	plan, err := b.preparePinPlanLocked(ctx, ops)
	if err != nil {
		return ctx, nil, err
	}
	var native *metadatajournal.PhysicalAppend
	err = b.withTerminalPhysicalNativeLocked(ctx, func(q *metadatajournal.PhysicalNativeLoan) error {
		ref, err := q.Reference()
		if err != nil {
			return err
		}
		extra := 0
		if w == nil {
			extra = 1
		}
		end := ref.EOF + int64(b.physicalDebt+extra)*metadatajournal.PhysicalAppendBytes
		prior, err := q.PreparedEnd()
		if err != nil {
			return err
		}
		end = max(prior, end)
		if err = b.store.RecordPhysicalNativeRange(ref, end); err != nil {
			return err
		}
		if err = q.Allocate(end); err != nil {
			return err
		}
		native, err = q.Append()
		return err
	})
	if err != nil {
		return ctx, nil, err
	}
	var cw *authenticatedstate.PhysicalTerminalWindow
	if w != nil {
		cw = w.cow
	}
	cow, err := b.store.PreparePhysicalUpdate(ctx, cw)
	if err != nil {
		return ctx, nil, err
	}
	finish := func(operationErr error) error {
		err := cow.Finish(native.Accepted(), operationErr)
		plan.finish(native.Accepted(), err == nil && operationErr == nil)
		if err == nil && w != nil {
			w.remaining--
			b.physicalDebt--
		}
		if err != nil {
			b.ready.Store(false)
			b.retirementErr = errors.Join(b.retirementErr, err)
		}
		return err
	}
	return cow.Bind(native.Bind(ctx)), finish, nil
}

// BindPhysicalCommonSelection is called after actual common publication. The
// publisher's authenticated bytes and this owner's current native/root must
// agree; a caller-provided seal is never accepted as selection authority.
func (b *Baseline) BindPhysicalCommonSelection(ctx context.Context) error {
	if b == nil || !b.mu.TryLock() {
		return authenticatedstate.ErrBusy
	}
	defer b.mu.Unlock()
	if !b.physicalEnabled {
		return nil
	}
	if b.physicalDebt != 0 || !b.ready.Load() {
		return authenticatedstate.ErrRecovery
	}
	seal, found, err := b.selectedPublisher.CurrentContext(ctx)
	if err != nil || !found {
		return errors.Join(ErrSource, err)
	}
	actual, err := b.sealReferenceLocked(ctx)
	if err != nil {
		return err
	}
	rootFound, positionFound := false, false
	for _, r := range seal.Roots[:seal.RootCount] {
		if r == actual.Root {
			rootFound = true
		}
	}
	for _, p := range seal.Positions[:seal.PositionCount] {
		if p == actual.Position {
			positionFound = true
		}
	}
	if !rootFound || !positionFound {
		return ErrSource
	}
	raw, err := json.Marshal(seal)
	if err != nil {
		return err
	}
	selection := sha256.Sum256(raw)
	return b.journal.WithPhysicalNative(ctx, b.budget, func(q *metadatajournal.PhysicalNativeLoan) error {
		ref, err := q.Reference()
		if err != nil {
			return err
		}
		return b.store.BindPhysicalSelectionDigest(selection, ref)
	})
}

type pricePinRecoveryKey struct{}

func (f *standaloneFacade) PricePinRecoveryContext(ctx context.Context, count int) (context.Context, error) {
	b := f.owner
	b.mu.Lock()
	defer b.mu.Unlock()
	w := b.physicalRecovery
	if w == nil {
		return ctx, nil
	}
	if !b.ready.Load() || w.closed.Load() || count < 0 || count > w.remaining {
		return ctx, resourcebudget.ErrPhysicalAllocation
	}
	if count == 0 {
		return context.WithValue(w.Bind(ctx), pricePinRecoveryKey{}, true), nil
	}
	return context.WithValue(context.WithValue(w.Bind(ctx), pricePinTerminalKey{}, true), pricePinRecoveryKey{}, true), nil
}
func (f *standaloneFacade) FinishPricePinRecovery(ctx context.Context) error {
	b := f.owner
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if b.physicalRecovery == nil {
		return nil
	}
	if !b.ready.Load() {
		return authenticatedstate.ErrRecovery
	}
	// The DAO reached this entry only after all bounded pages and its unchanged
	// actual frozen AccountingLeaseReader head/hash authenticated. Whole-vector
	// Ledger NoDebt/common publication are required by the maintenance caller.
	b.physicalPinRecoveryVerified = true
	return nil
}

// PhysicalRetirementFailure keeps the actual Metadata keys, native/root owners
// and whole original fees reachable when Close cannot grant retirement.
type PhysicalRetirementFailure struct {
	Owner *Baseline
	Cause error
}

func (e *PhysicalRetirementFailure) Error() string { return e.Cause.Error() }
func (e *PhysicalRetirementFailure) Unwrap() error { return e.Cause }

// These fixed slots are paid before upstream work, then lent only to the
// authenticated terminal DAO or actual recovery lane. Ordinary writes cannot
// activate them by merely carrying a window.
func (b *Baseline) acquirePhysicalCompletion() (*resourcebudget.CompletableWork, error) {
	active, requirements := metadatalimits.PhysicalCompletionProfile(b.haPhysicalSource != nil)
	var completionRAM int64
	for _, r := range requirements {
		completionRAM += r.RAM
	}
	return b.budget.TryAcquireCompletableWork(resourcebudget.IO,
		active,
		resourcebudget.Cost{RAM: completionRAM}, requirements)
}

type metadataPhysicalCompletionKey struct{}
type metadataPhysicalCompletionScope struct {
	owner  *Baseline
	window *TerminalPhysicalWindow
	active atomic.Bool
}

func (f *standaloneFacade) withPhysicalCompletion(ctx context.Context, visit func(context.Context) error) error {
	if ctx.Value(pricePinTerminalKey{}) == nil && ctx.Value(pricePinRecoveryKey{}) == nil {
		return visit(ctx)
	}
	w, ok := ctx.Value(terminalWindowKey{}).(*TerminalPhysicalWindow)
	if !ok {
		return visit(ctx)
	} // disabled physical profile retains ordinary semantics
	if w == nil || w.owner != f.owner || w.closed.Load() || w.completion == nil {
		return resourcebudget.ErrClosed
	}
	if scope, ok := ctx.Value(metadataPhysicalCompletionKey{}).(*metadataPhysicalCompletionScope); ok && scope.owner == f.owner && scope.window == w {
		if !scope.active.Load() {
			return resourcebudget.ErrClosed
		}
		return visit(ctx)
	}
	return resourcebudget.WithCompletionWorkspace(w.completion.Bind(ctx), func(ctx context.Context) error {
		scope := &metadataPhysicalCompletionScope{owner: f.owner, window: w}
		scope.active.Store(true)
		defer scope.active.Store(false)
		ctx = context.WithValue(ctx, metadataPhysicalCompletionKey{}, scope)
		if err := visit(ctx); err != nil {
			return fmt.Errorf("metadata price pin terminal callback: %w", err)
		}
		return nil
	})
}

func (b *Baseline) physicalCommonSelectionLocked(ctx context.Context) error {
	if b.haPhysicalSource != nil {
		return b.haPhysicalCommonSelectionLocked(ctx)
	}
	selected, found, err := b.selectedPublisher.CurrentContext(ctx)
	if err != nil || !found || selected.Role != authenticatedstate.SealStandalone {
		return fmt.Errorf("metadata physical retirement current selection (found=%t role=%d): %w", found, selected.Role, errors.Join(ErrSource, err))
	}
	actual, err := b.sealReferenceLockedMode(ctx, false)
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
		return fmt.Errorf("metadata physical retirement differs from current common selection (root=%t position=%t applied_sequence=%d applied_offset=%d): %w", rootFound, positionFound, actual.Position.Sequence, actual.Position.Offset, ErrSource)
	}
	return nil
}

// RetainForRecovery withdraws this original window after the companion actual
// Ledger owner reports unresolved acceptance or uncertain retirement. It grants
// no IO or deletion authority; all candidate and completion owners stay held.
func (w *TerminalPhysicalWindow) RetainForRecovery(cause error) error {
	if w == nil || w.owner == nil || cause == nil {
		return ErrSource
	}
	b := w.owner
	b.mu.Lock()
	defer b.mu.Unlock()
	return w.retainForRecoveryLocked(cause)
}

func (w *TerminalPhysicalWindow) retainForRecoveryLocked(cause error) error {
	b := w.owner
	if w.closed.Load() && w.closeErr != nil {
		return w.closeErr
	}
	w.closed.Store(true)
	b.ready.Store(false)
	b.journal.HoldPhysicalForRecovery()
	failure := errors.Join(ownedfile.ErrRetirement, cause, b.store.HoldPhysicalPreparationForRecovery(context.Background()))
	b.retirementErr = errors.Join(b.retirementErr, failure)
	w.closeErr = failure
	return &PhysicalRetirementFailure{Owner: b, Cause: failure}
}
