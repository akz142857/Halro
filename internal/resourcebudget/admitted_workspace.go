package resourcebudget

import (
	"context"
	"errors"
)

type admittedWorkspaceKey struct{}

// WithAdmittedWorkspace lets an already admitted request reuse its original
// serialized RAM slots for ordinary work while normal admission is Healthy.
// It grants no terminal continuation, native disk, write or confirmation right.
// Explicit completion callbacks keep their independent pressure semantics.
func WithAdmittedWorkspace(ctx context.Context, manager *Manager, visit func(context.Context) error) (resultErr error) {
	if visit == nil || manager == nil {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if InCompletionContinuation(ctx) {
		return visit(ctx)
	}
	w, found := ctx.Value(admittedWorkspaceKey{}).(*CompletableWork)
	if !found {
		return visit(ctx)
	}
	if w == nil || w.pool == nil || w.pool.manager != manager || w.Active.claim == nil || w.Active.claim.manager != manager {
		return ErrInvalid
	}
	manager.mu.Lock()
	if !w.Active.validLocked() || w.Active.claim.reserved {
		manager.mu.Unlock()
		return ErrClosed
	}
	if err := manager.check(w.Active.claim.class, Cost{}, Cost{}, false, 0); err != nil {
		manager.mu.Unlock()
		return err
	}
	active, err := w.Active.retainLocked()
	manager.mu.Unlock()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, active.Release()) }()
	ctx = context.WithValue(ctx, completionContinuationKey{}, false)
	if w.shared != nil {
		return w.shared.withLoan(ctx, w, false, visit)
	}
	if _, _, scoped := workspaceFromContext(ctx); scoped {
		return w.WithWorkspace(ctx, visit)
	}
	return withRAMWorkspaceLoan(ctx, w.pool, false, visit)
}
