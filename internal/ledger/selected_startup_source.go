package ledger

import (
	"context"
	"errors"
	"os"
	"sync"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// SelectedStartupSource exists only inside WithSelectedStartupOwner. Its seal
// is selected by the actual publisher and its anchor comes from the private
// selected native constructor. Replay/VerifyAnchors never authenticate genesis.
type SelectedStartupSource interface {
	Check(context.Context) error
	SelectedSeal() authenticatedstate.CandidateSeal
	SavedAnchor() ReplayAnchor
	NativeHead() ReplayAnchor
	ReplayAuthenticated(context.Context, ReplayAnchor, func(Record) error) (Watermark, error)
	VerifyAnchors(context.Context, [2]ReplayAnchor) error
}
type selectedStartupSource struct {
	mu          sync.Mutex
	replayMu    sync.Mutex
	active      bool
	ctx         context.Context
	native      *Log
	seal        authenticatedstate.CandidateSeal
	saved, head ReplayAnchor
	physical    ReplayAnchor
	info        os.FileInfo
}
type selectedStartupSourceDebt struct {
	error
	work   resourcebudget.Permit
	source *selectedStartupSource
}

func (e *selectedStartupSourceDebt) Unwrap() error { return e.error }

// WithSelectedStartupOwner holds the actual native lifecycle, maintenance gate
// (when installed), source IO credit and native mutex for the complete callback.
// Close cancels/joins it. Callback source methods use this freeze and must not
// reenter native methods or Close native/lease. Source capabilities expire
// before the mutex is released; retaining one cannot authorize later IO.
func (l *Log) WithSelectedStartupOwner(ctx context.Context, budget *resourcebudget.Manager, o SelectedLedgerProjectionOpenOptions, identity [32]byte, visit func(context.Context, SelectedStartupSource) error) (resultErr error) {
	if l == nil || l.startupProvenance == nil || l.startupProvenance.ha != nil {
		return ErrProjectionSource
	}
	return l.withSelectedStartupOwner(ctx, budget, o, identity, visit)
}

func (l *Log) withSelectedStartupOwner(ctx context.Context, budget *resourcebudget.Manager, o SelectedLedgerProjectionOpenOptions, identity [32]byte, visit func(context.Context, SelectedStartupSource) error) (resultErr error) {
	if l == nil || budget == nil || visit == nil {
		return ErrProjectionSource
	}
	l.admission.Lock()
	owner := l.maintenance
	l.admission.Unlock()
	if owner != nil && !owner.borrowsHASelectedMaintenance(ctx) {
		select {
		case owner.gate <- struct{}{}:
		default:
			return authenticatedstate.ErrBusy
		}
		defer func() { <-owner.gate }()
	}
	finish, err := l.beginMutation(owner)
	if err != nil {
		return err
	}
	defer finish()
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(l.closeContext, cancel)
	defer func() { stop(); cancel() }()
	if owner != nil {
		stopOwner := context.AfterFunc(owner.ctx, cancel)
		defer stopOwner()
	}
	work, err := budget.TryAcquireContext(opCtx, resourcebudget.IO, resourcebudget.Cost{RAM: AuthenticatedLedgerSourceRAMBytes})
	if err != nil {
		return err
	}
	var source *selectedStartupSource
	defer func() {
		if errors.Is(resultErr, ownedfile.ErrRetirement) || errors.Is(resultErr, ErrSegmentRetirement) {
			resultErr = &selectedStartupSourceDebt{error: resultErr, work: work, source: source}
			l.mu.Lock()
			l.closeErr = errors.Join(l.closeErr, resultErr)
			l.mu.Unlock()
			return
		}
		if err := work.Release(); err != nil {
			resultErr = &selectedStartupSourceDebt{error: errors.Join(resultErr, ownedfile.ErrRetirement, err), work: work, source: source}
			l.mu.Lock()
			l.closeErr = errors.Join(l.closeErr, resultErr)
			l.mu.Unlock()
		}
	}()
	// The already charged source job joins short descriptor lookups performed
	// by its actual historical auditor. Those immutable reads do not revoke
	// this owner. The maintenance gate still rejects a second source callback;
	// native Close joins this admitted job before retiring its resources.
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := opCtx.Err(); err != nil {
		return err
	}
	seal, err := l.validateSelectedOwnerLocked(opCtx, budget, identity, o)
	if err != nil {
		return err
	}
	info, err := l.file.Stat()
	if err != nil {
		return err
	}
	physical := ReplayAnchor{Head: Watermark{Generation: l.generation, Sequence: l.sequence, Offset: l.offset}, Hash: l.chainHash, Epoch: l.chainEpoch}
	allowed := physical
	if l.startupProvenance.ha != nil {
		scope, ok := opCtx.Value(haSelectedStartupScopeKey{}).(*haSelectedStartupScope)
		if !ok {
			return ErrProjectionSource
		}
		allowed, err = l.resolveHAAllowedAnchorLocked(opCtx, scope.allowed)
		if err != nil {
			return err
		}
	}
	source = &selectedStartupSource{active: true, ctx: opCtx, native: l, seal: seal, saved: l.startupSaved, head: allowed, physical: physical, info: info}
	defer func() {
		source.replayMu.Lock()
		defer source.replayMu.Unlock()
		source.mu.Lock()
		source.active = false
		source.mu.Unlock()
	}()
	if err := source.Check(opCtx); err != nil {
		return err
	}
	scopeCtx := context.WithValue(opCtx, selectedStartupScopeKey{}, source)
	if err := visit(scopeCtx, source); err != nil {
		return err
	}
	current, err := l.validateSelectedOwnerLocked(opCtx, budget, identity, o)
	if err != nil || current != seal {
		return errors.Join(ErrProjectionSource, err)
	}
	return source.Check(opCtx)
}

func (s *selectedStartupSource) checkLocked(ctx context.Context) error {
	if s == nil || !s.active || s.native == nil || s.ctx.Err() != nil || ctx.Err() != nil {
		return errors.Join(ErrProjectionUnavailable, ctx.Err())
	}
	l := s.native
	if audit := l.startupAuditPtr.Load(); audit != nil {
		if err := audit.Snapshot().Err; err != nil {
			return errors.Join(ErrStartupAuditPending, err)
		}
	}
	if l.file == nil || l.closeContext.Err() != nil || l.status.Load() != AccountingHealthy || s.physical != (ReplayAnchor{Head: Watermark{Generation: l.generation, Sequence: l.sequence, Offset: l.offset}, Hash: l.chainHash, Epoch: l.chainEpoch}) {
		return ErrProjectionUnavailable
	}
	actual, err := l.file.Stat()
	named, nameErr := os.Lstat(l.path)
	if err != nil || nameErr != nil || !named.Mode().IsRegular() || !os.SameFile(s.info, actual) || !os.SameFile(actual, named) || actual.Size() != s.physical.Head.Offset || named.Size() != actual.Size() {
		return errors.Join(ErrProjectionSource, err, nameErr)
	}
	return nil
}
func (s *selectedStartupSource) Check(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkLocked(ctx)
}
func (s *selectedStartupSource) SelectedSeal() authenticatedstate.CandidateSeal {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return authenticatedstate.CandidateSeal{}
	}
	return s.seal
}
func (s *selectedStartupSource) SavedAnchor() ReplayAnchor {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return ReplayAnchor{}
	}
	return s.saved
}
func (s *selectedStartupSource) NativeHead() ReplayAnchor {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return ReplayAnchor{}
	}
	return s.head
}

func (s *selectedStartupSource) verifyAnchorsLocked(ctx context.Context, anchors [2]ReplayAnchor) error {
	if err := s.checkLocked(ctx); err != nil {
		return err
	}
	seen := [2]bool{}
	observe := func(a ReplayAnchor) error {
		for i, want := range anchors {
			if a == want {
				seen[i] = true
			}
		}
		return nil
	}
	_ = observe(s.saved)
	for _, a := range anchors {
		if projectionPrefixBefore(a.Head, s.saved.Head) || projectionPrefixBefore(s.head.Head, a.Head) {
			return ErrProjectionCursor
		}
	}
	if !seen[0] || !seen[1] {
		_, err := s.native.replayAuthenticatedLocked(ctx, s.saved, 0, func(r Record) error {
			return observe(ReplayAnchor{Head: Watermark{Generation: r.Generation, Sequence: r.Sequence, Offset: r.Offset}, Hash: r.Hash, Epoch: r.Epoch})
		}, observe)
		if err != nil {
			return err
		}
	}
	if !seen[0] || !seen[1] {
		return ErrProjectionCursor
	}
	return s.checkLocked(ctx)
}
func (s *selectedStartupSource) VerifyAnchors(ctx context.Context, anchors [2]ReplayAnchor) error {
	s.replayMu.Lock()
	defer s.replayMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.verifyAnchorsLocked(ctx, anchors)
}
func (s *selectedStartupSource) ReplayAuthenticated(ctx context.Context, from ReplayAnchor, visit func(Record) error) (Watermark, error) {
	s.replayMu.Lock()
	defer s.replayMu.Unlock()
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	defer func() { stop(); cancel() }()
	s.mu.Lock()
	err := s.verifyAnchorsLocked(opCtx, [2]ReplayAnchor{from, from})
	s.mu.Unlock()
	if err != nil {
		return Watermark{}, err
	}
	head, err := s.native.replayAuthenticatedLocked(opCtx, from, 0, func(r Record) error {
		position := Watermark{Generation: r.Generation, Sequence: r.Sequence, Offset: r.Offset}
		if projectionPrefixBefore(s.head.Head, position) {
			return opCtx.Err()
		}
		if visit != nil {
			return visit(r)
		}
		return opCtx.Err()
	}, nil)
	if err != nil {
		return Watermark{}, err
	}
	if head != s.physical.Head {
		return Watermark{}, ErrProjectionSource
	}
	return s.head.Head, s.Check(opCtx)
}
