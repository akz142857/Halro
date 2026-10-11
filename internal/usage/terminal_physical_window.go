package usage

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// The private reducer already holds the original archive witness and Usage
// mutexes. Borrow the actual source lane, then allocate an ordinary candidate
// above all existing terminal debt. A window update cannot consume a native
// frame's reserved root or advance the native cut.
func (p *DiskProjection) updatePhysicalUsageWindow(ctx context.Context, lease *ledger.MaintenanceLease, anchor ledger.ReplayAnchor, apply func(context.Context) error) error {
	q, e := p.physical, p.engine
	if q == nil || e == nil || q.ha || e.selectedHA || q.native != e.native || q.ledger != e.owner || q.store != e.cow || q.manager != e.budget || q.work == nil {
		return authenticatedstate.ErrRecovery
	}
	visit := func(ctx context.Context, source ledger.AuthenticatedUsageSource) error {
		s, ok := source.(ledger.AuthenticatedPhysicalUsageSource)
		if !ok || source.Reference().Anchor != anchor || source.NativeHead() != anchor {
			return authenticatedstate.ErrRecovery
		}
		if err := s.ValidatePhysicalPublisher(ctx, q.publisher); err != nil {
			return err
		}
		selection, err := s.PhysicalSelectionDigest(ctx)
		if err != nil || selection != q.selection {
			return errors.Join(authenticatedstate.ErrRecovery, err)
		}
		q.mu.Lock()
		defer q.mu.Unlock()
		// Every accepted native frame is applied (applied == anchor == native
		// head, the lane frozen) and live catch-up consumes one prepaid root
		// per frame, so what remains prepaid is for frames not yet written: a
		// Roll prepays the successor's first. The candidate below is allocated
		// above that debt and never consumes it, so it is no reason to refuse.
		if q.failure != nil || q.applied != anchor {
			return errors.Join(authenticatedstate.ErrRecovery, q.failure)
		}
		u, err := q.store.PreparePhysicalUpdate(ctx, nil)
		if err != nil {
			return err
		}
		err = apply(u.Bind(ctx))
		// Once the derived update enters, cancellation cannot prove that its
		// root publication did not happen. Retain the original uncertain owner.
		finishErr := u.Finish(true, err)
		q.closedQualified = false
		if finishErr != nil {
			q.failure = errors.Join(q.failure, ownedfile.ErrRetirement, finishErr)
		}
		return errors.Join(err, finishErr)
	}
	return q.work.WithWorkspace(resourcebudget.WithoutRAMWorkspace(ctx), func(ctx context.Context) error {
		if lease == nil {
			return p.withPhysicalSourceLocked(ctx, func(ctx context.Context, source ledger.AuthenticatedPhysicalUsageSource) error {
				return visit(ctx, source)
			})
		}
		ref, err := e.owner.ReadUsageSourceReference(ctx)
		if err != nil {
			return err
		}
		return e.owner.WithSelectedMaintenanceUsageSource(ctx, e.native, lease, e.budget, ref.Scope, visit)
	})
}
