package governance

import (
	"context"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// CaptureHAStartupCache captures one complete role-permitted frozen native
// cache. The HA caller independently authenticates permitted through actual
// Member/Ordering/source evidence. A physical suffix cannot enter this cache;
// no source/header fields grant role, confirmation or deletion permission.
func (l *Log) CaptureHAStartupCache(ctx context.Context, budget *resourcebudget.Manager, identity [32]byte, permitted Summary) (*StartupCache, error) {
	if l == nil {
		return nil, ErrStartupCache
	}
	return l.captureStartupCache(ctx, budget, identity, func(ctx context.Context, budget *resourcebudget.Manager, visit func(context.Context, Summary) error) error {
		return l.WithFrozenHead(ctx, budget, func(ctx context.Context, head Summary) error {
			l.mu.Lock()
			ha := l.replica || l.beforeDurable != nil || l.afterDurable != nil
			l.mu.Unlock()
			if !ha || head != permitted {
				return ErrStartupCache
			}
			return visit(ctx, head)
		})
	})
}
