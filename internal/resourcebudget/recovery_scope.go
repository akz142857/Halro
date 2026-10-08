package resourcebudget

import (
	"context"
	"errors"
	"sync"
)

type recoveryScopeKey struct{}
type recoveryScope struct {
	mu       sync.Mutex
	manager  *Manager
	active   bool
	children int
	done     chan struct{}
}

// WithRecoveryScope lends a live, RAM-only Recovery permit to one maintenance
// callback. Temporary descendant claims use the SAME Manager's Recovery class,
// each charged separately. No disk/unconfirmed or nested authority is granted.
// The callback must release all descendants; return, cancellation or panic
// withdraws admission and joins their final retained/transferred owners before
// releasing the borrowed parent. Calling it from its own descendant is invalid.
func (p Permit) WithRecoveryScope(ctx context.Context, visit func(context.Context) error) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.claim == nil || visit == nil {
		return ErrInvalid
	}
	m := p.claim.manager
	m.mu.Lock()
	if !p.validLocked() || !p.claim.recovery || p.claim.reserved || p.claim.class != Recovery || p.claim.cost.RAM <= 0 || p.claim.cost.Disk != 0 || p.claim.cost.Unconfirmed != 0 || p.claim.scope != nil || p.claim.scopeActive {
		m.mu.Unlock()
		return ErrInvalid
	}
	borrowed, err := p.retainLocked()
	if err != nil {
		m.mu.Unlock()
		return err
	}
	p.claim.scopeActive = true
	m.mu.Unlock()
	s := &recoveryScope{manager: m, active: true, done: make(chan struct{})}
	defer func() {
		s.mu.Lock()
		s.active = false
		if s.children == 0 {
			close(s.done)
		}
		s.mu.Unlock()
		<-s.done // Including active acquisition and final retained child owners.
		resultErr = errors.Join(resultErr, borrowed.Release())
		m.mu.Lock()
		p.claim.scopeActive = false
		m.mu.Unlock()
	}()
	resultErr = visit(context.WithValue(ctx, recoveryScopeKey{}, s))
	if resultErr == nil {
		resultErr = ctx.Err()
	}
	return resultErr
}

func (s *recoveryScope) releaseChild() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.children--
	if !s.active && s.children == 0 {
		close(s.done)
	}
}

// TryAcquireContext preserves ordinary admission without a scope. Scoped RAM
// IO/Background work is charged to dedicated Recovery even under pressure;
// Active/Cache/Completion/Control cannot borrow it. An escaped scope, different
// Manager or mixed/persistent cost refuses instead of falling back to normal.
func (m *Manager) TryAcquireContext(ctx context.Context, class Class, cost Cost) (Permit, error) {
	if err := ctx.Err(); err != nil {
		return Permit{}, err
	}
	if workspace, generation, scoped := workspaceFromContext(ctx); scoped {
		if cap, ok := ctx.Value(workspaceKey{}).(workspaceCapability); ok && cap.loan != nil {
			s := cap.loan.owner
			s.mu.Lock()
			defer s.mu.Unlock()
			if !s.validLoanLocked(cap.loan) {
				return Permit{}, ErrClosed
			}
		}
		if workspace == nil {
			return Permit{}, ErrInvalid
		}
		return workspace.acquire(m, class, cost, generation)
	}
	s, scoped := ctx.Value(recoveryScopeKey{}).(*recoveryScope)
	if !scoped {
		return m.TryAcquire(class, cost)
	}
	if s == nil || m == nil || s.manager != m || (class != IO && class != Background) || cost.RAM <= 0 || cost.Disk != 0 || cost.Unconfirmed != 0 {
		return Permit{}, ErrInvalid
	}
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return Permit{}, ErrClosed
	}
	s.children++
	s.mu.Unlock()
	p, err := m.TryAcquireRecovery(cost)
	if err != nil {
		s.releaseChild()
		return Permit{}, err
	}
	m.mu.Lock()
	p.claim.scope = s
	m.mu.Unlock()
	return p, nil
}
