package budget

import (
	"context"
	"errors"
)

var ErrAccountingRetired = errors.New("accounting manager is retired")

type accountingOperationKey struct{}

type accountingOperationValidator interface {
	validateAccountingOperation(context.Context) error
}

// Tickets are private to Manager calls. Each nested call has its own lifetime;
// a captured context cannot extend admission after its ticket has finished.
type accountingOperation struct {
	manager *Manager
	active  bool // guarded by manager.operationMu
}

func (m *Manager) enterOperation(ctx context.Context) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	parent, _ := ctx.Value(accountingOperationKey{}).(*accountingOperation)
	m.operationMu.Lock()
	if (m.retiring || m.paused != nil) && (parent == nil || parent.manager != m || !parent.active) {
		m.operationMu.Unlock()
		return nil, nil, ErrAccountingRetired
	}
	if owner, ok := m.owner.(accountingOperationValidator); ok {
		if err := owner.validateAccountingOperation(ctx); err != nil {
			m.operationMu.Unlock()
			return nil, nil, err
		}
	}
	if m.activeOperations == 0 {
		m.operationDrain = make(chan struct{})
	}
	ticket := &accountingOperation{manager: m, active: true}
	m.activeOperations++
	m.operationMu.Unlock()
	return context.WithValue(ctx, accountingOperationKey{}, ticket), func() {
		m.operationMu.Lock()
		defer m.operationMu.Unlock()
		if !ticket.active {
			return
		}
		ticket.active = false
		m.activeOperations--
		if m.activeOperations == 0 {
			if m.retiring {
				m.releaseAccountingOwner()
			}
			close(m.operationDrain)
		}
	}, nil
}

type accountingPause struct{ marker byte }

// PauseOperations joins accepted calls without releasing the borrowed owners.
// Whole requests must be drained first: a provider between Reserve and Settle
// does not own a Manager ticket. The returned resume belongs only to this pause;
// it cannot undo retirement or reopen a subsequent pause.
func (m *Manager) PauseOperations(ctx context.Context) (func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.operationMu.Lock()
	if m.retiring || m.paused != nil {
		m.operationMu.Unlock()
		return nil, ErrAccountingRetired
	}
	pause := &accountingPause{}
	m.paused = pause
	drained := m.operationDrain
	active := m.activeOperations
	m.operationMu.Unlock()
	resume := func() error {
		m.operationMu.Lock()
		defer m.operationMu.Unlock()
		if m.paused != pause {
			return ErrAccountingRetired
		}
		m.paused = nil
		if m.retiring {
			return ErrAccountingRetired
		}
		return nil
	}
	if active != 0 {
		select {
		case <-drained:
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), resume())
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, resume())
	}
	return resume, nil
}

// Retire permanently refuses new calls and joins accepted Manager operations,
// including reads, admission locks, Apply/observers, confirmation waits and
// pending-admission cleanup. A timeout leaves admission withdrawn; the final
// operation releases the owned references even if the caller does not retry.
// A context canceled before entry does not withdraw admission.
//
// Callers must first drain whole business requests and background producers:
// separate Reserve/Settle calls do not share a Manager operation lifetime. Do
// not call Retire from this Manager's reader, observer or other callback. It
// does not close or take ownership of the shared native Log/projection and is
// not an HA role or generation publication gate. During terminal shutdown a
// blocked predecessor needs Log.Close to wake it before Retire can finish.
func (m *Manager) Retire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	drained := m.withdrawAdmission()
	if drained == nil {
		return nil
	}
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WithdrawAdmission permanently refuses fresh calls without joining accepted
// operations. Terminal owners use this BEFORE native Close (which may be needed
// to wake Apply predecessor waits), then Retire to join BEFORE closing readers,
// observers or Metadata. This is not a resource-close or replacement-ready gate.
func (m *Manager) WithdrawAdmission() { m.withdrawAdmission() }

func (m *Manager) withdrawAdmission() <-chan struct{} {
	m.operationMu.Lock()
	defer m.operationMu.Unlock()
	m.retiring = true
	if m.activeOperations == 0 {
		m.releaseAccountingOwner()
		return nil
	}
	return m.operationDrain
}

// Called under operationMu only after all accepted operations have exited.
// Keep immutable period/diagnostic dependencies. Clear direct ownership and
// clock/native-hook references; their closures may capture historical State.
// External aliases/Runtime ownership and actual GC are separate boundaries.
func (m *Manager) releaseAccountingOwner() {
	m.owner = nil
	m.reads = nil
	m.legacyPending = nil
	m.log = nil
	m.now = nil
	m.projectLocks.Clear()
	m.observerMu.Lock()
	m.observers = nil
	m.observerMu.Unlock()
}
