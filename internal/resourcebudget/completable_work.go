package resourcebudget

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/ownedfile"
)

type completionWorkspaceKey struct{}

// CompletableWork pre-admits the actual terminal RAM slots and their control
// claims atomically with active work. Credits belong to the same Manager and
// are reused by sequential terminal facts, without new admission. This is not
// native disk allocation, confirmation, role permission or durable acceptance.
type CompletableWork struct {
	Active Permit
	pool   *ramWorkspace
	shared *SharedCompletableWorkspace
}

func (m *Manager) TryAcquireCompletableWork(class Class, active, completion Cost, requirements []WorkspaceRequirement) (*CompletableWork, error) {
	if m == nil {
		return nil, ErrInvalid
	}
	m.mu.Lock()
	shared := m.sharedCompletion
	m.mu.Unlock()
	if shared != nil {
		if rule, disk, ok := shared.matches(class, active, completion, requirements); ok {
			if !shared.rules[rule].priority {
				return shared.acquire(class, active, rule, disk)
			}
			// A request whose private terminal pool fits keeps it: its terminal
			// continuation may wait for confirmation, and a private pool does not
			// hold the lane through that wait. Only a capacity shortfall falls
			// back to the guaranteed shared lane; pressure still refuses.
			w, err := m.tryAcquirePrivateCompletableWorkCounted(class, active, completion, requirements, false)
			if !errors.Is(err, ErrExhausted) {
				return w, err
			}
			return shared.acquire(class, active, rule, disk)
		}
	}
	return m.tryAcquirePrivateCompletableWork(class, active, completion, requirements)
}

func (m *Manager) tryAcquirePrivateCompletableWork(class Class, active, completion Cost, requirements []WorkspaceRequirement) (*CompletableWork, error) {
	return m.tryAcquirePrivateCompletableWorkCounted(class, active, completion, requirements, true)
}

func (m *Manager) tryAcquirePrivateCompletableWorkCounted(class Class, active, completion Cost, requirements []WorkspaceRequirement, countReject bool) (*CompletableWork, error) {
	if m == nil || len(requirements) == 0 || len(requirements) > MaxWorkspaceSlots {
		return nil, ErrInvalid
	}
	var ram int64
	for _, r := range requirements {
		if (r.Class != IO && r.Class != Background) || r.RAM <= 0 || r.RAM > completion.RAM-ram {
			return nil, ErrInvalid
		}
		ram += r.RAM
	}
	excess := completion.sub(Cost{RAM: ram})
	claims := 1 + len(requirements)
	if excess != (Cost{}) {
		claims++
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !normalClass(class) || !active.valid() || active == (Cost{}) || !completion.valid() || m.limits.MaxOwners < 2 {
		return nil, ErrInvalid
	}
	admit := m.check
	if !countReject {
		admit = m.fit
	}
	if err := admit(class, active, completion, false, claims); err != nil {
		return nil, err
	}
	w := &ramWorkspace{manager: m, reusable: true, done: make(chan struct{})}
	for _, r := range requirements {
		p := m.charge(Completion, Cost{RAM: r.RAM}, false, true)
		slot := &w.slots[w.count]
		*slot = workspaceSlot{workspace: w, required: r, permit: p}
		p.claim.workspace = slot
		w.count++
	}
	if excess != (Cost{}) {
		w.excess = m.charge(Completion, excess, false, true)
	}
	return &CompletableWork{Active: m.charge(class, active, false, false), pool: w}, nil
}

// Bind carries private credits through the request. Ordinary operations cannot
// borrow them until a terminal continuation explicitly enters the capability.
func (w *CompletableWork) Bind(ctx context.Context) context.Context {
	ctx = context.WithValue(ctx, admittedWorkspaceKey{}, w)
	if w.shared != nil {
		return context.WithValue(ctx, completionWorkspaceKey{}, w)
	}
	return context.WithValue(ctx, completionWorkspaceKey{}, w.pool)
}

// WithWorkspace lends this original pool, or continues its own actual active
// loan through a synchronous nested operation. A different or expired loan
// cannot be replaced with this work's credits. This carries RAM ownership only.
func (w *CompletableWork) WithWorkspace(ctx context.Context, visit func(context.Context) error) error {
	if w == nil || w.pool == nil || visit == nil {
		return ErrInvalid
	}
	if w.shared != nil {
		return w.shared.withLoan(ctx, w, true, visit)
	}
	if pool, generation, scoped := workspaceFromContext(ctx); scoped {
		if pool != w.pool {
			return ErrInvalid
		}
		pool.mu.Lock()
		valid := pool.active && pool.inUse && pool.reusable && pool.generation == generation
		pool.mu.Unlock()
		if !valid {
			return ErrClosed
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return visit(ctx)
	}
	return WithCompletionWorkspace(w.Bind(ctx), visit)
}

// WithCompletionWorkspace activates previously owned credits once, preserving
// their values after caller cancellation. The caller supplies its cleanup
// deadline. Only one continuation may borrow this fixed pool at a time.
func WithCompletionWorkspace(ctx context.Context, visit func(context.Context) error) (resultErr error) {
	if visit == nil {
		return ErrInvalid
	}
	ctx = context.WithValue(ctx, completionContinuationKey{}, true)
	if work, found := ctx.Value(completionWorkspaceKey{}).(*CompletableWork); found {
		if work == nil || work.shared == nil {
			return ErrInvalid
		}
		// Explicit completion has always replaced an independent caller's RAM
		// scope. A shared descendant still needs its genuine live parent loan.
		return work.shared.withLoan(context.WithValue(ctx, workspaceKey{}, struct{}{}), work, true, visit)
	}
	w, scoped := ctx.Value(completionWorkspaceKey{}).(*ramWorkspace)
	if !scoped {
		return visit(ctx)
	}
	if w == nil {
		return ErrInvalid
	}
	return withRAMWorkspaceLoan(ctx, w, true, visit)
}

// Both phases borrow the same original slots. Healthy admitted work leaves
// their terminal reservation intact; only explicit completion activates it.
func withRAMWorkspaceLoan(ctx context.Context, w *ramWorkspace, activate bool, visit func(context.Context) error) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	if !w.reusable || w.inUse || w.children != 0 || w.generation == ^uint64(0) {
		w.mu.Unlock()
		return ErrClosed
	}
	for i := 0; activate && i < w.count; i++ {
		p := &w.slots[i].permit
		w.manager.mu.Lock()
		reserved := p.validLocked() && p.claim.reserved
		w.manager.mu.Unlock()
		if reserved {
			next, err := p.ActivateCompletion()
			if err != nil {
				w.mu.Unlock()
				return err
			}
			*p = next
		}
	}
	// Excess RAM/disk remains reserved for the full response lifetime. It is
	// never lent as an arbitrary persistent claim or silently returned early.
	w.inUse, w.active = true, true
	w.ordinary = !activate
	w.generation++
	generation := w.generation
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.inUse, w.active = false, false
		w.ordinary = false
		children := w.children
		var debt *WorkspaceRetirement
		if children != 0 {
			w.reusable = false
			debt = newWorkspaceRetirementLocked(errors.Join(resultErr, ownedfile.ErrRetirement), w)
		}
		w.mu.Unlock()
		if children != 0 {
			resultErr = debt
		}
	}()
	return visit(context.WithValue(ctx, workspaceKey{}, workspaceCapability{owner: w, generation: generation}))
}

// Close withdraws unused credits after the real handler/continuation exits.
// Unknown physical child retirement keeps the same pool alive; its final child
// retires the original claims. No cancellation, HTTP return or retry erases it.
func (w *CompletableWork) Close() error {
	if w == nil || w.pool == nil {
		return ErrInvalid
	}
	if w.shared != nil {
		return w.shared.closeMember(w)
	}
	p := w.pool
	p.mu.Lock()
	if p.inUse {
		p.mu.Unlock()
		return ErrExhausted
	}
	p.active, p.reusable = false, false
	children := p.children
	var debt *WorkspaceRetirement
	if children != 0 {
		debt = newWorkspaceRetirementLocked(ownedfile.ErrRetirement, p)
	}
	p.mu.Unlock()
	if children != 0 {
		return debt
	}
	p.finish()
	<-p.done
	return p.closeErr
}
