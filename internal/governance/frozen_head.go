package governance

import (
	"context"
	"errors"
	"sync"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

var ErrNativeFrozen = errors.New("governance native mutation is frozen by a read job")

type frozenHeadScopeKey struct{}

type frozenHeadScope struct {
	mu        sync.Mutex
	native    *Log
	budget    *resourcebudget.Manager
	head      Summary
	ctx       context.Context
	active    bool
	borrowers uint64
	drained   chan struct{}
	failure   error
}

// Nested cache capture borrows the original physical freeze. A copied context
// cannot start a successor freeze; accepted borrowers join before its release.
func (s *frozenHeadScope) borrow(ctx context.Context, native *Log, budget *resourcebudget.Manager, visit func(context.Context, Summary) error) (resultErr error) {
	s.mu.Lock()
	if !s.active || s.failure != nil || s.native != native || s.budget != budget || s.ctx.Err() != nil || ctx.Err() != nil {
		failure := errors.Join(ErrNativeFrozen, s.failure, s.ctx.Err(), ctx.Err())
		s.mu.Unlock()
		return failure
	}
	if s.borrowers >= 4 {
		s.mu.Unlock()
		return ErrNativeFrozen
	}
	s.borrowers++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			s.failure = errors.Join(s.failure, resultErr)
		}
		s.borrowers--
		if !s.active && s.borrowers == 0 {
			close(s.drained)
		}
		s.mu.Unlock()
	}()
	jobCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	defer func() { stop(); cancel() }()
	if s.ctx.Err() != nil {
		cancel()
	}
	return errors.Join(visit(jobCtx, s.head), jobCtx.Err())
}
func (s *frozenHeadScope) join() error {
	s.mu.Lock()
	s.active = false
	if s.borrowers == 0 {
		close(s.drained)
	}
	s.mu.Unlock()
	<-s.drained
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failure
}

// WithFrozenStandaloneHead refuses Replica and all installed replication
// callbacks inside the actual native freeze before lending its receipt.
func (l *Log) WithFrozenStandaloneHead(ctx context.Context, budget *resourcebudget.Manager, visit func(context.Context, Summary) error) error {
	if visit == nil {
		return ErrGovernanceSealSource
	}
	return l.WithFrozenHead(ctx, budget, func(ctx context.Context, head Summary) error {
		l.mu.Lock()
		standalone := !l.replica && l.afterDurable == nil && l.beforeDurable == nil && !l.closing && l.file != nil
		l.mu.Unlock()
		if !standalone {
			return ErrGovernanceSealSource
		}
		return visit(ctx, head)
	})
}

// WithFrozenHead joins earlier native writes, excludes semantic and physical
// appends, and authenticates the complete frozen source before and after visit.
// Close cancels visit and joins its actual lifetime before closing native IO.
// The callback may read this log and lend its context to at most four nested
// borrowers of this same physical freeze. An independent context cannot start
// another freeze. Close must never be called from an accepted callback. Borrowed
// callbacks are joined before release; saved contexts cannot admit later work.
// Its context and frozen head must not escape the job. This component job
// does not grant Runtime identity, role/term, common-cut or publication authority.
func (l *Log) WithFrozenHead(ctx context.Context, budget *resourcebudget.Manager, visit func(context.Context, Summary) error) (resultErr error) {
	if l == nil || budget == nil || visit == nil {
		return ErrGovernanceSealSource
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if scope, _ := ctx.Value(frozenHeadScopeKey{}).(*frozenHeadScope); scope != nil {
		return scope.borrow(ctx, l, budget, visit)
	}
	credit, err := budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 4096})
	if err != nil {
		return err
	}
	opCtx, cancel := context.WithCancel(ctx)
	l.mu.Lock()
	if l.startupAudit != nil && !l.startupAudit.Snapshot().Complete {
		l.mu.Unlock()
		cancel()
		return errors.Join(ErrStartupAuditPending, credit.Release())
	}
	if l.file == nil || l.closing || l.terminalErr != nil || l.freezeCancel != nil {
		failure := errors.Join(ErrGovernanceSealSource, l.terminalErr)
		if l.freezeCancel != nil {
			failure = ErrNativeFrozen
		}
		l.mu.Unlock()
		cancel()
		return errors.Join(failure, credit.Release())
	}
	frozen := Summary{Records: l.sequence, LastHash: l.lastHash, Bytes: l.offset}
	l.freezeCancel = cancel
	l.readerWG.Add(1)
	l.mu.Unlock()
	defer l.readerWG.Done()
	defer func() {
		cancel()
		l.mu.Lock()
		l.freezeCancel = nil
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			l.freezeDebt = credit
			l.terminalErr = errors.Join(l.terminalErr, resultErr)
			l.startupFault.Store(true)
			l.mu.Unlock()
			return
		}
		// Clear the registered lifetime only while holding mu; Close already marked
		// closing before its Wait, so no job can be added after withdrawal.
		l.mu.Unlock()
		if err := credit.Release(); err != nil {
			resultErr = errors.Join(resultErr, err)
			l.mu.Lock()
			l.freezeDebt = credit
			l.terminalErr = errors.Join(l.terminalErr, err)
			l.startupFault.Store(true)
			l.mu.Unlock()
		}
	}()
	validate := func() error {
		_, err := l.SealReference(opCtx, budget, frozen)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			// Failure to authenticate actual source bytes withdraws all later writes.
			// Capacity rejection before IO and callback errors leave native health intact.
			if !errors.Is(err, resourcebudget.ErrExhausted) && !errors.Is(err, resourcebudget.ErrPressure) && !errors.Is(err, resourcebudget.ErrClosed) && !errors.Is(err, resourcebudget.ErrInvalid) && !errors.Is(err, resourcebudget.ErrOwners) {
				l.mu.Lock()
				l.terminalErr = errors.Join(l.terminalErr, err)
				l.startupFault.Store(true)
				l.mu.Unlock()
			}
		}
		if err != nil {
			return err
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.closing || l.file == nil || (Summary{Records: l.sequence, LastHash: l.lastHash, Bytes: l.offset}) != frozen {
			return ErrGovernanceSealSource
		}
		return opCtx.Err()
	}
	if err := validate(); err != nil {
		return err
	}
	scope := &frozenHeadScope{native: l, budget: budget, head: frozen, ctx: opCtx, active: true, drained: make(chan struct{})}
	callbackErr := func() (err error) {
		defer func() { err = errors.Join(err, scope.join()) }()
		return visit(context.WithValue(opCtx, frozenHeadScopeKey{}, scope), frozen)
	}()
	if err := opCtx.Err(); err != nil {
		return errors.Join(callbackErr, err)
	}
	return errors.Join(callbackErr, validate(), opCtx.Err())
}
