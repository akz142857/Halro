package usage

import (
	"context"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

func (p *DiskProjection) PrepareSelectedRepack(ctx context.Context, publisher *authenticatedstate.SealCandidatePublisher, guard *lock.Lock, selected authenticatedstate.CandidateSeal, reservation resourcebudget.Permit) (*authenticatedstate.StoreRepack, error) {
	if p == nil || !p.mu.TryLock() {
		return nil, ErrDiskUsageBusy
	}
	defer p.mu.Unlock()
	if p.retired.Load() || p.engine == nil || p.engine.cow == nil || p.engine.live {
		return nil, authenticatedstate.ErrRecovery
	}
	schema := authenticatedstate.SealRootSchema{Kind: authenticatedstate.SealRootState, Component: authenticatedstate.SealComponentUsage, IndexID: AuthenticatedUsageSealIndexID, Codec: TypedUsageSealCodec}
	return p.engine.cow.PrepareRepack(ctx, publisher, guard, selected, schema, reservation)
}
