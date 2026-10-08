package governance

import (
	"context"
	"errors"
)

type governanceOperationKey struct{}

// Private tickets cover the full public call, including external publication.
// Nested calls are allowed only while their particular parent ticket is live.
type governanceOperation struct {
	manager *Manager
	active  bool // protected by manager.operationMu
}

func (m *Manager) enterOperation(ctx context.Context) (context.Context, func(), error) {
	if m.log != nil && m.log.startupProvenance != nil && m.log.startupProvenance.ha != nil && !m.haSelected {
		return nil, nil, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	parent, _ := ctx.Value(governanceOperationKey{}).(*governanceOperation)
	m.operationMu.Lock()
	if (m.retiring || m.paused != nil) && (parent == nil || parent.manager != m || !parent.active) {
		m.operationMu.Unlock()
		return nil, nil, ErrRetired
	}
	if m.activeOperations == 0 {
		m.operationDrain = make(chan struct{})
	}
	ticket := &governanceOperation{manager: m, active: true}
	m.activeOperations++
	m.operationMu.Unlock()
	return context.WithValue(ctx, governanceOperationKey{}, ticket), func() {
		m.operationMu.Lock()
		defer m.operationMu.Unlock()
		if !ticket.active {
			return
		}
		ticket.active = false
		m.activeOperations--
		if m.activeOperations == 0 {
			if m.retiring {
				m.releaseOwner()
			}
			close(m.operationDrain)
		}
	}, nil
}

type governancePause struct{ marker byte }

// PauseOperations excludes fresh calls and joins accepted Report/export calls.
// Resume preserves this same manager and cannot reverse terminal retirement.
// Callers first join whole HTTP/provider work and keep its accounting and DAO
// dependencies admitted until this join finishes.
func (m *Manager) PauseOperations(ctx context.Context) (func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.operationMu.Lock()
	if m.retiring || m.paused != nil {
		m.operationMu.Unlock()
		return nil, ErrRetired
	}
	pause := &governancePause{}
	m.paused = pause
	drained, active := m.operationDrain, m.activeOperations
	m.operationMu.Unlock()
	resume := func() error {
		m.operationMu.Lock()
		defer m.operationMu.Unlock()
		if m.paused != pause {
			return ErrRetired
		}
		m.paused = nil
		if m.retiring {
			return ErrRetired
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

// Retire permanently withdraws new Manager calls and joins accepted reads,
// Report durable Apply/checkpoint/maturity, and complete export publication.
// A timeout does not reopen admission; the last operation releases references
// even without a retry. A context canceled before entry does not withdraw.
//
// First drain whole HTTP requests/background producers. Do not call Retire from
// this Manager's callbacks. This does not close native/DB/Metadata/accounting
// resources, establish D-03 trust, or publish a replacement Runtime generation.
func (m *Manager) Retire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.operationMu.Lock()
	m.retiring = true
	if m.activeOperations == 0 {
		m.releaseOwner()
		m.operationMu.Unlock()
		return nil
	}
	drained := m.operationDrain
	m.operationMu.Unlock()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// operationMu is held and all accepted calls are gone. Public methods cannot
// access these fields without a ticket. External State aliases are separate.
func (m *Manager) releaseOwner() {
	m.log, m.state, m.projection = nil, nil, nil
	m.accounting, m.definitions, m.checkpoint, m.now = nil, nil, nil, nil
	m.unavailable = nil // External errors may themselves capture historical owners.
	clear(m.pageKey)
	m.pageKey = nil
}
