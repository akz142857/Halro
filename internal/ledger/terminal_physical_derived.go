package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

type PhysicalDerivedRole uint8

const (
	PhysicalDerivedDirectory PhysicalDerivedRole = iota + 1
	PhysicalDerivedUsage
)

type PhysicalDerivedStage uint8

const (
	PhysicalDerivedReserve PhysicalDerivedStage = iota + 1
	PhysicalDerivedAppend
	PhysicalDerivedRoll
	PhysicalDerivedSuccessor
	PhysicalDerivedClose
)

// PhysicalDerivedCoverage runs under actual native.mu. A borrower must capture
// its real selected owner and verify it on every call, touch its finite counters
// only under this same lane, and may acquire Store.mu; it MUST NOT acquire its
// projection/Directory/Usage mutex. reference/end come from the actual native
// descriptor, never from caller fields. futureFrames includes outstanding debt.
// Close supplies a zero reference/end: it can only inspect the borrower's
// already-recorded closed qualification and cannot authorize source IO.
type PhysicalDerivedCoverage func(context.Context, *Log, authenticatedstate.PhysicalNativeReference, int64, PhysicalDerivedStage, int) error

type physicalDerivedScopeKey struct{}
type physicalDerivedScope struct {
	native    *Log
	owner     *nativePhysicalOwner
	stage     PhysicalDerivedStage
	reference authenticatedstate.PhysicalNativeReference
	end       int64
	active    atomic.Bool
}

func (l *Log) BindPhysicalDerivedCoverage(ctx context.Context, projection *AuthenticatedLedgerProjection, role PhysicalDerivedRole, visit PhysicalDerivedCoverage) error {
	if l == nil || projection == nil || visit == nil || role < PhysicalDerivedDirectory || role > PhysicalDerivedUsage {
		return ErrProjectionSource
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	o := l.physical.Load()
	p := l.startupProvenance
	if o == nil || o.projection != projection || projection.selectedNative.Load() != l || projection.budget != l.startupBudget || p == nil || p.ha != nil || !l.selectedStartupStructuralAllowedLocked() || o.failure != nil || l.closeContext.Err() != nil {
		return ErrProjectionSource
	}
	if err := p.publisher.ValidateOwner(p.guard, projection.budget); err != nil {
		return err
	}
	seal, found, err := p.publisher.CurrentContext(ctx)
	if err != nil || !found {
		return errors.Join(ErrProjectionSource, err)
	}
	raw, err := json.Marshal(seal)
	if err != nil || sha256.Sum256(raw) != o.selection {
		return errors.Join(ErrProjectionSource, err)
	}
	index := int(role - 1)
	if o.derived[index] != nil {
		return authenticatedstate.ErrBusy
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	o.derived[index] = visit
	return nil
}

// RequirePhysicalDerivedCoverage is called by the actual complete Runtime
// vector owner after both independent pools were installed, before Ready.
func (l *Log) RequirePhysicalDerivedCoverage(ctx context.Context, projection *AuthenticatedLedgerProjection) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	o := l.physical.Load()
	if o == nil || o.projection != projection || o.derived[0] == nil || o.derived[1] == nil {
		return ErrProjectionSource
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	o.derivedRequired = true
	return nil
}

func (l *Log) preparePhysicalDerivedLocked(ctx context.Context, stage PhysicalDerivedStage, futureFrames int) error {
	o := l.physical.Load()
	if o == nil {
		return nil
	}
	if futureFrames < 0 || o.derivedRequired && (o.derived[0] == nil || o.derived[1] == nil) {
		return ErrProjectionSource
	}
	var ref authenticatedstate.PhysicalNativeReference
	end := o.allocatedEnd
	if stage == PhysicalDerivedClose {
		// Native.Close may have already joined the actual descriptor before the
		// projection closes. Re-reading it cannot strengthen the borrower's
		// durable common-root/NoDebt marker and would falsely create new debt.
		end = 0
	} else {
		var err error
		ref, err = l.physicalNativeReferenceLocked()
		if err != nil {
			return err
		}
	}
	if stage != PhysicalDerivedClose {
		o.haCloseQualified = false
	}
	if stage == PhysicalDerivedRoll {
		if err := l.verifyPhysicalRollNoDebtLocked(ctx); err != nil {
			l.retainPhysicalSourceFailureLocked(err)
			return err
		}
		o.rollFrom = ref // actual captured full prefix before any Roll mutation
	}
	if stage == PhysicalDerivedSuccessor {
		o.commonQualified = false
	}
	for _, visit := range o.derived {
		if visit == nil {
			continue
		}
		scope := &physicalDerivedScope{native: l, owner: o, stage: stage, reference: ref, end: end}
		scope.active.Store(true)
		loanCtx := context.WithValue(ctx, physicalDerivedScopeKey{}, scope)
		err := visit(loanCtx, l, ref, end, stage, futureFrames)
		scope.active.Store(false)
		if err != nil {
			if stage == PhysicalDerivedSuccessor {
				err = errors.Join(authenticatedstate.ErrRecovery, err)
			}
			l.retainPhysicalSourceFailureLocked(err)
			return err
		}
	}
	return nil
}

// Called after the original projection's durable/recovery workers have joined,
// under projection.mu then native.mu, before Store.Close can retire controls.
func (l *Log) holdUnselectedPhysicalProjectionLocked(ctx context.Context, p *AuthenticatedLedgerProjection) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	o := l.physical.Load()
	if o == nil || l.startupProvenance == nil {
		return nil
	}
	if o.projection != p {
		return ErrProjectionSource
	}
	var err error
	if !o.commonQualified || o.failure != nil || o.debt != 0 || o.active != 0 {
		err = errors.Join(authenticatedstate.ErrRecovery, o.failure)
	}
	if err == nil {
		err = l.checkPhysicalCloseSelectionLocked(ctx)
	}
	if err == nil {
		err = l.preparePhysicalDerivedLocked(ctx, PhysicalDerivedClose, 0)
	}
	if err != nil {
		err = errors.Join(authenticatedstate.ErrRecovery, err, p.store.HoldPhysicalPreparationForRecovery(context.Background()))
		o.failure = err
		l.status.RequireRecovery()
	}
	return err
}

// TransferPhysicalDerivedNativeReceipt is valid only in the finite callback
// from this Log's actual completed Roll. Neither a scanned successor nor caller
// reference fields can manufacture that private lane or renew a generation.
func (l *Log) TransferPhysicalDerivedNativeReceipt(ctx context.Context, store *authenticatedstate.Store) error {
	scope, _ := ctx.Value(physicalDerivedScopeKey{}).(*physicalDerivedScope)
	if scope == nil || !scope.active.Load() || scope.native != l || scope.stage != PhysicalDerivedSuccessor || scope.owner != l.physical.Load() || store == nil {
		return ErrProjectionSource
	}
	o := scope.owner
	from, to := o.rollFrom, scope.reference
	if from.File.Inode == 0 || from.File == to.File || from.Parent != to.Parent || from.Generation == ^uint64(0) || from.Generation+1 != to.Generation || from.Sequence != to.Sequence || from.Hash != to.Hash || to.EOF != 0 || o.debt != 0 || o.active != 0 || o.failure != nil {
		return ErrProjectionSource
	}
	verify := func(recorded authenticatedstate.PhysicalNativeReference) error {
		if !scope.active.Load() || recorded != from || l.physical.Load() != o || o.generation != to.Generation || o.allocatedEnd != scope.end {
			return ErrProjectionSource
		}
		actual, err := l.physicalNativeReferenceLocked()
		if err != nil || actual != to {
			return errors.Join(ErrProjectionSource, err)
		}
		segment, found, err := l.lookupSegmentLocked(from.Generation)
		endHash, hashErr := decodeChainHash(segment.EndHash)
		if err != nil || hashErr != nil || !found || segment.LastSequence != from.Sequence || endHash != from.Hash || segment.Length != from.EOF || segment.Generation != from.Generation {
			return errors.Join(ErrProjectionSource, err, hashErr)
		}
		if filepath.Base(segment.File) != segment.File || segment.Compressed {
			return ErrProjectionSource
		}
		fee, err := o.projection.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: AuthenticatedLedgerSourceRAMBytes})
		if err != nil {
			return err
		}
		file, err := os.OpenFile(filepath.Join(l.directory, segment.File), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return errors.Join(err, fee.Release())
		}
		birth, birthErr := resourcebudget.CapturePhysicalIdentity(file)
		info, statErr := file.Stat()
		startHash, startErr := decodeChainHash(segment.StartHash)
		var scanErr error
		if birthErr == nil && statErr == nil && startErr == nil && birth == from.File && info.Size() == from.EOF && segment.FirstSequence != 0 {
			v := &chainVerifier{key: l.chainKey, hash: startHash, sequence: segment.FirstSequence - 1}
			head, partial, err := scan(io.NewSectionReader(file, 0, from.EOF), from.Generation, 0, segment.FirstSequence-1, func(Record) error { return ctx.Err() }, v)
			if err != nil || partial || head.Offset != from.EOF || head.Sequence != from.Sequence || v.hash != from.Hash {
				scanErr = errors.Join(ErrProjectionSource, err)
			}
		} else {
			scanErr = errors.Join(ErrProjectionSource, birthErr, statErr, startErr)
		}
		closeErr := ownedfile.CloseFile(file)
		if closeErr != nil {
			// Preserve the actual descriptor as well as its credit. No later
			// retry or GC finalizer can prove this original reader's exit.
			debtErr := errors.Join(ErrSegmentRetirement, ownedfile.ErrRetirement, scanErr, closeErr)
			o.derivedReaderDebt = &startupAuditReaderDebt{file: file, work: fee, err: debtErr}
			l.closeErr = errors.Join(l.closeErr, debtErr)
			l.status.RequireRecovery()
			return debtErr
		}
		if err := errors.Join(scanErr, fee.Release()); err != nil {
			return err
		}
		return l.checkPhysicalNativeLocked()
	}
	return store.RecordPhysicalNativeSuccessorQualified(ctx, from, to, scope.end, verify)
}
