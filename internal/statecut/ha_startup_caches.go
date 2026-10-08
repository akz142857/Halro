package statecut

import (
	"context"
	"math"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/governance"
)

// PublishHAWithStartupCaches lends both actual durable native caches through
// the seven-root publication. Their file headers never select HA permission.
func PublishHAWithStartupCaches(ctx context.Context, o HAOwners, base authenticatedstate.CandidateSeal, caches StartupCacheCandidates) (Publication, error) {
	if caches.Audit == nil || caches.Governance == nil {
		return Publication{}, authenticatedstate.ErrSealIneligible
	}
	c := o.Components
	return publishHA(ctx, o, base, func(ctx context.Context, candidate authenticatedstate.CandidateSeal, persist func(context.Context) error) error {
		g, a := candidate.Positions[2], candidate.Positions[3]
		if g.Offset > math.MaxInt64 || a.Offset > math.MaxInt64 {
			return authenticatedstate.ErrSealIneligible
		}
		return caches.Audit.WithFrozenOwner(ctx, c.Audit, c.Budget, candidate.Instance, audit.Summary{Records: a.Sequence, Bytes: int64(a.Offset), LastHash: a.Hash}, func(ctx context.Context) error {
			return caches.Governance.WithFrozenOwner(ctx, c.NativeGovernance, c.Budget, candidate.Instance, governance.Summary{Records: g.Sequence, Bytes: int64(g.Offset), LastHash: g.Hash}, persist)
		})
	})
}
