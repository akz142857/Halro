package ledger

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// AuthenticatedLedgerAdvance reports this live candidate's native/component
// barriers. It is not an HA confirmation, cross-component Seal or restart token.
// VerifiedRecords counts decoded suffix facts across both passes, not physical
// IO needed to reach a compressed offset or audit previously committed bytes.
type AuthenticatedLedgerAdvance struct {
	From, To, NativeHead ReplayAnchor
	AppliedRecords       uint64
	VerifiedRecords      uint64
}

func projectionPrefixBefore(a, b Watermark) bool {
	return a.Generation < b.Generation || a.Generation == b.Generation && a.Offset < b.Offset
}

// AdvanceFromNative catches up an already live, native-built candidate. The
// caller supplies a role-permitted prefix from independent ordering/member
// evidence. This method verifies that exact physical native boundary; it never
// derives permission from the durable head. Shrinking a role requires a fresh
// candidate. There is no arbitrary Record Apply or saved-store reopen API.
//
// Two complete suffix passes share one frozen native lock. The first establishes
// source integrity and the permitted anchor before changing rows. The second
// reduces only permitted facts and rechecks the complete tail before allowing
// queries. A failure after reduction starts permanently retires the candidate.
func (p *AuthenticatedLedgerProjection) AdvanceFromNative(ctx context.Context, native *Log, permitted ProjectionPrefix) (receipt AuthenticatedLedgerAdvance, resultErr error) {
	return p.advanceFromNative(ctx, native, permitted, nil)
}

// phase is a private deterministic test seam; no production caller supplies it.
func (p *AuthenticatedLedgerProjection) advanceFromNative(ctx context.Context, native *Log, permitted ProjectionPrefix, phase func(string)) (receipt AuthenticatedLedgerAdvance, resultErr error) {
	return p.advanceFromNativeMode(ctx, native, permitted, phase, false)
}

// AdvanceOneFromNative is the serialized durable-Apply path. It authenticates
// exactly the next semantic fact from the actual native source, never a supplied
// Event. One caller may wait for existing component reads or the native writer;
// another durable caller is rejected rather than queued. The caller must retain
// native mutation ownership and order Apply before observers and confirmation.
// This does not establish a role-permitted prefix or authorize business startup.
func (p *AuthenticatedLedgerProjection) AdvanceOneFromNative(ctx context.Context, native *Log, permitted ProjectionPrefix) (AuthenticatedLedgerAdvance, error) {
	if p == nil || native == nil || p.budget == nil {
		return AuthenticatedLedgerAdvance{}, ErrProjectionUnavailable
	}
	p.durableMu.Lock()
	if p.closeContext.Err() != nil {
		p.durableMu.Unlock()
		return AuthenticatedLedgerAdvance{}, ErrProjectionUnavailable
	}
	if p.durableDone != nil {
		p.durableMu.Unlock()
		return AuthenticatedLedgerAdvance{}, authenticatedstate.ErrBusy
	}
	done := make(chan struct{})
	p.durableDone = done
	p.durableApply.Store(true)
	p.durableMu.Unlock()
	defer func() {
		p.durableMu.Lock()
		p.durableApply.Store(false)
		p.durableDone = nil
		close(done)
		p.durableMu.Unlock()
	}()
	return p.advanceFromNativeMode(ctx, native, permitted, nil, true)
}

func (p *AuthenticatedLedgerProjection) joinDurableApply() {
	p.durableMu.Lock()
	done := p.durableDone
	p.durableMu.Unlock()
	if done != nil {
		<-done
	}
}

func (p *AuthenticatedLedgerProjection) advanceFromNativeMode(ctx context.Context, native *Log, permitted ProjectionPrefix, phase func(string), single bool) (receipt AuthenticatedLedgerAdvance, resultErr error) {
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	if p == nil || native == nil || p.budget == nil {
		return receipt, ErrProjectionUnavailable
	}
	// Reserve source scratch before the sole durable caller waits for readers.
	permit, err := p.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: AuthenticatedLedgerSourceRAMBytes})
	if err != nil {
		return receipt, err
	}
	locked := false
	defer func() {
		resultErr = finishAuthenticatedSourceCredit(permit, resultErr)
		var debt *authenticatedSourceDebt
		if errors.As(resultErr, &debt) {
			if locked {
				p.ready = false
				p.failure = errors.Join(p.failure, resultErr)
				p.sealDebt, p.sealErr = permit, resultErr
				p.readFailure.CompareAndSwap(nil, &authenticatedLedgerReadFault{err: resultErr})
			}
		}
		if resultErr != nil {
			receipt = AuthenticatedLedgerAdvance{}
		}
		if locked {
			p.mu.Unlock()
		}
	}()
	if single {
		p.mu.Lock()
	} else if !p.mu.TryLock() {
		return receipt, authenticatedstate.ErrBusy
	}
	locked = true
	if err := p.selectedGrantError(ctx, false); err != nil {
		return receipt, err
	}
	if !p.ready || p.readFailure.Load() != nil {
		return receipt, errors.Join(ErrProjectionUnavailable, p.failure, p.readFault())
	}
	opCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopOwner := context.AfterFunc(p.closeContext, cancel)
	defer stopOwner()
	stopNative := context.AfterFunc(native.closeContext, cancel)
	defer stopNative()
	ctx = opCtx
	checkContext := func() error {
		if err := p.selectedGrantError(ctx, false); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// AfterFunc cancellation is asynchronous; the owner's synchronous
		// cancellation must also be observed at publication barriers.
		if p.closeContext.Err() != nil || native.closeContext.Err() != nil {
			return context.Canceled
		}
		return nil
	}
	if p.closeContext.Err() != nil || native.closeContext.Err() != nil {
		return receipt, ErrProjectionUnavailable
	}
	if permitted.Head.Offset < 0 || permitted.Head.Generation == 0 && permitted != (ProjectionPrefix{}) {
		return receipt, ErrProjectionCursor
	}
	position, found, err := authenticatedLedgerReadLocked[projectionPosition](ctx, p, "meta", []byte("position"))
	if err != nil {
		if !errors.Is(err, authenticatedstate.ErrCorrupt) && !errors.Is(err, ErrProjectionCorrupt) {
			return receipt, err
		}
		p.ready = false
		p.failure = err
		return receipt, p.failure
	}
	if !found {
		p.ready = false
		p.failure = ErrProjectionCorrupt
		return receipt, p.failure
	}
	from := ReplayAnchor{Head: position.Watermark, Hash: position.Hash, Epoch: position.Epoch}
	if single && (from.Head.Sequence == ^uint64(0) || permitted.Head.Sequence != from.Head.Sequence+1) {
		return receipt, fmt.Errorf("%w: durable Apply requires exactly the next native fact", ErrProjectionCursor)
	}
	if projectionPrefixBefore(permitted.Head, from.Head) || permitted.Head.Sequence < from.Head.Sequence {
		return receipt, fmt.Errorf("%w: a reduced role prefix requires a fresh candidate", ErrProjectionCursor)
	}
	// No wait queue accumulates native readers behind a writer. The owner lock
	// protects both suffix passes against append, Roll, Compact and Close.
	if single {
		native.mu.Lock()
	} else if !native.mu.TryLock() {
		return receipt, authenticatedstate.ErrBusy
	}
	defer native.mu.Unlock()
	if err := checkContext(); err != nil {
		return receipt, err
	}
	path, err := filepath.Abs(native.path)
	if err != nil || path != p.nativePath || sha256.Sum256(native.chainKey) != p.nativeKeyTag {
		return receipt, errors.Join(ErrProjectionSource, err)
	}
	if native.file == nil || len(native.chainKey) != 32 || native.status.Load() != AccountingHealthy {
		return receipt, ErrProjectionUnavailable
	}
	nativeHead := ReplayAnchor{Head: Watermark{Generation: native.generation, Offset: native.offset, Sequence: native.sequence}, Hash: native.chainHash, Epoch: native.chainEpoch}
	if projectionPrefixBefore(nativeHead.Head, permitted.Head) || permitted.Head.Sequence > nativeHead.Head.Sequence {
		return receipt, ErrProjectionCursor
	}
	seen := false
	var target ReplayAnchor
	check := func(anchor ReplayAnchor) error {
		if anchor.Head == permitted.Head {
			if anchor.Hash != permitted.Hash {
				return ErrProjectionCursor
			}
			seen, target = true, anchor
		}
		return nil
	}
	// A wrong caller hash must not short-circuit authentication of an excluded
	// suffix. Record/boundary matching captures mismatch and checks it afterward.
	var cutErr error
	observe := func(anchor ReplayAnchor) error {
		if err := check(anchor); err != nil {
			cutErr = err
		}
		return nil
	}
	_ = observe(from)
	verified := uint64(0)
	_, err = native.replayAuthenticatedLocked(ctx, from, 0, func(r Record) error {
		verified++
		return observe(ReplayAnchor{Head: Watermark{r.Generation, r.Offset, r.Sequence}, Hash: r.Hash, Epoch: r.Epoch})
	}, observe)
	if err != nil {
		if native.status.Load() != AccountingHealthy {
			p.ready = false
			p.failure = errors.Join(ErrProjectionSource, err)
		}
		return receipt, err
	}
	_ = observe(nativeHead)
	if cutErr != nil || !seen {
		return receipt, errors.Join(ErrProjectionCursor, cutErr)
	}
	if phase != nil {
		phase("source-verified")
	}
	if err := checkContext(); err != nil {
		return receipt, err
	}
	if target == from {
		return AuthenticatedLedgerAdvance{From: from, To: target, NativeHead: nativeHead, VerifiedRecords: verified}, nil
	}
	applied, deltaVerified, err := p.applyAuthenticatedNativeDeltaLocked(ctx, native, from, target, phase, checkContext)
	if err != nil {
		return receipt, err
	}
	return AuthenticatedLedgerAdvance{From: from, To: target, NativeHead: nativeHead, AppliedRecords: applied, VerifiedRecords: verified + deltaVerified}, nil
}

// Both component and native locks are held. The same reducer and atomic-root
// barriers serve live Apply and independently authenticated component reopen.
func (p *AuthenticatedLedgerProjection) applyAuthenticatedNativeDeltaLocked(ctx context.Context, native *Log, from, target ReplayAnchor, phase func(string), checkContext func() error) (applied, verified uint64, resultErr error) {
	// Typed reads cannot see an intermediate root from this live owner. A failed
	// owner stays retired; component reopen must independently validate saved
	// contents, native source and role-permitted cut before returning a reader.
	p.ready = false
	failed := true
	defer func() {
		if failed {
			p.failure = errors.Join(ErrProjectionSource, resultErr)
		}
	}()
	current := from
	_, err := native.replayAuthenticatedLocked(ctx, from, 0, func(r Record) error {
		verified++
		anchor := ReplayAnchor{Head: Watermark{r.Generation, r.Offset, r.Sequence}, Hash: r.Hash, Epoch: r.Epoch}
		if projectionPrefixBefore(target.Head, anchor.Head) {
			return nil
		}
		err := p.updateHAPhysicalRestoredDeltaLocked(ctx, native, authenticatedLedgerCut(p.scope, anchor), func(tx *authenticatedstate.Transaction) error {
			return applyProjectionRecord(ctx, authenticatedProjectionRows{tx}, r)
		})
		if err != nil {
			return fmt.Errorf("native record %d: %w", r.Sequence, err)
		}
		current = anchor
		applied++
		if phase != nil {
			phase("record-applied")
		}
		return nil
	}, nil)
	if err != nil {
		return 0, 0, err
	}
	if current != target {
		// The only remaining transition is a native-proven physical roll/start,
		// with exactly the same semantic sequence, chain hash and epoch.
		if target.Head.Offset != 0 || target.Head.Sequence != current.Head.Sequence || target.Hash != current.Hash || target.Epoch != current.Epoch || projectionPrefixBefore(target.Head, current.Head) {
			return 0, 0, ErrProjectionCorrupt
		}
		err = p.updateHAPhysicalRestoredDeltaLocked(ctx, native, authenticatedLedgerCut(p.scope, target), func(tx *authenticatedstate.Transaction) error {
			return projectionRowsPut(authenticatedProjectionRows{tx}, "meta", []byte("position"), projectionPosition{target.Head, target.Hash, target.Epoch})
		})
		if err != nil {
			return 0, 0, err
		}
	}
	if err := checkContext(); err != nil {
		return 0, 0, err
	}
	p.ready, failed = true, false
	return applied, verified, nil
}
