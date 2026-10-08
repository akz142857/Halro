package backup

import (
	"context"
	"errors"
	"math"
)

type workspaceBudgetKey struct{}

// WorkspaceBudget is immutable admission for operation-owned indexes/spools.
// Reserve must charge one complete file ceiling and its working RAM to the
// caller's shared manager. The release follows physical close and exact unlink.
// A scope grants neither archive authenticity nor live-data publication.
type WorkspaceBudget struct {
	parent  context.Context
	bytes   int64
	reserve func(context.Context, int64) (func() error, error)
}

func NewWorkspaceBudget(ctx context.Context, maxFileBytes int64, reserve func(context.Context, int64) (func() error, error)) (*WorkspaceBudget, error) {
	if ctx == nil || maxFileBytes <= 0 || maxFileBytes > math.MaxInt64-(32<<20) || uint64(maxFileBytes) > uint64(^uint(0)>>1) || reserve == nil {
		return nil, errors.New("backup workspace requires a positive file ceiling and shared reservation")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &WorkspaceBudget{parent: ctx, bytes: maxFileBytes, reserve: reserve}, nil
}

func (b *WorkspaceBudget) Bind(ctx context.Context) (context.Context, error) {
	if b == nil || ctx == nil {
		return nil, errors.New("backup workspace requires its original scope")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if previous := backupWorkspace(ctx); previous != nil && previous != b {
		return nil, errors.New("backup operation substituted its workspace budget")
	}
	return context.WithValue(ctx, workspaceBudgetKey{}, b), nil
}

func backupWorkspace(ctx context.Context) *WorkspaceBudget {
	if ctx == nil {
		return nil
	}
	b, _ := ctx.Value(workspaceBudgetKey{}).(*WorkspaceBudget)
	return b
}

func (b *WorkspaceBudget) acquire(ctx context.Context) (func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if b == nil {
		return func() error { return nil }, nil
	}
	if err := b.parent.Err(); err != nil {
		return nil, err
	}
	release, err := b.reserve(ctx, b.bytes)
	if err != nil {
		return nil, err
	}
	if release == nil {
		return nil, errors.New("backup workspace reservation returned no physical owner release")
	}
	if err := errors.Join(ctx.Err(), b.parent.Err()); err != nil {
		return nil, errors.Join(err, release())
	}
	return release, nil
}

// A pinned handle retains its original manager even if a method receives a
// context without values. An explicitly different budget is never substituted.
func (b *WorkspaceBudget) methodContext(ctx context.Context) (context.Context, func(), error) {
	if ctx == nil {
		return nil, nil, errors.New("backup method requires context")
	}
	if selected := backupWorkspace(ctx); selected != nil && selected != b {
		return nil, nil, errors.New("backup handle workspace identity changed")
	}
	if b == nil {
		return ctx, func() {}, ctx.Err()
	}
	bound, err := b.Bind(ctx)
	if err != nil {
		return nil, nil, err
	}
	merged, cancel := context.WithCancel(bound)
	stop := context.AfterFunc(b.parent, cancel)
	if b.parent.Err() != nil {
		cancel()
	}
	return merged, func() { stop(); cancel() }, nil
}
