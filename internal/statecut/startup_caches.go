package statecut

import (
	"context"
	"fmt"
	"math"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/governance"
)

// StartupCacheCandidates are immutable, durably persisted creation owners.
// They supplement the selected native positions, never select startup authority
// or grant a reader deletion permission. The caller retains their long-term
// Disk obligations across Close and any possibly durable common publication.
type StartupCacheCandidates struct {
	Audit      *audit.StartupCacheFile
	Governance *governance.StartupCacheFile
}

// PublishStandaloneWithStartupCaches verifies both actual candidate files
// inside the complete native/root freeze and holds their loans through the
// actual durable seal publication. A late validation/retirement error returns
// a zero Seal; Receipt still records any actual publication so cleanup cannot
// mistake a possibly active artifact for an unpublished disposable candidate.
func PublishStandaloneWithStartupCaches(ctx context.Context, o StandaloneOwners, base authenticatedstate.CandidateSeal, authority authenticatedstate.SealEligibility, caches StartupCacheCandidates) (Publication, error) {
	if caches.Audit == nil || caches.Governance == nil {
		return Publication{}, authenticatedstate.ErrSealIneligible
	}
	return publishStandaloneWithLoan(ctx, o, base, authority, nil, func(ctx context.Context, candidate authenticatedstate.CandidateSeal, persist func(context.Context) error) error {
		g, a := candidate.Positions[2], candidate.Positions[3]
		if g.Offset > math.MaxInt64 || a.Offset > math.MaxInt64 {
			return authenticatedstate.ErrSealIneligible
		}
		return caches.Audit.WithFrozenOwner(ctx, o.Audit, o.Budget, authority.Instance, audit.Summary{Records: a.Sequence, Bytes: int64(a.Offset), LastHash: a.Hash}, func(ctx context.Context) error {
			return caches.Governance.WithFrozenOwner(ctx, o.NativeGovernance, o.Budget, authority.Instance, governance.Summary{Records: g.Sequence, Bytes: int64(g.Offset), LastHash: g.Hash}, persist)
		})
	})
}

// PublishStandaloneCapturedStartupCaches captures candidate positions and both
// caches inside the same original native/component freeze. Background Audit
// appends cannot split capture and Publish. This grants only Standalone cuts;
// identity/incarnation/versions remain independently selected by the caller.
func PublishStandaloneCapturedStartupCaches(ctx context.Context, o StandaloneOwners, base authenticatedstate.CandidateSeal, authority authenticatedstate.SealEligibility, prepare func(context.Context) (StartupCacheCandidates, error)) (Publication, error) {
	if prepare == nil {
		return Publication{}, authenticatedstate.ErrSealIneligible
	}
	return publishStandaloneFrozen(ctx, o, base, authority, true, nil, func(ctx context.Context, candidate authenticatedstate.CandidateSeal, persist func(context.Context) error) error {
		caches, err := prepare(ctx)
		if err != nil {
			return fmt.Errorf("capture actual native startup caches: %w", err)
		}
		if caches.Audit == nil || caches.Governance == nil {
			return authenticatedstate.ErrSealIneligible
		}
		g, a := candidate.Positions[2], candidate.Positions[3]
		if g.Offset > math.MaxInt64 || a.Offset > math.MaxInt64 {
			return authenticatedstate.ErrSealIneligible
		}
		err = caches.Audit.WithFrozenOwner(ctx, o.Audit, o.Budget, authority.Instance, audit.Summary{Records: a.Sequence, Bytes: int64(a.Offset), LastHash: a.Hash}, func(ctx context.Context) error {
			if err := caches.Governance.WithFrozenOwner(ctx, o.NativeGovernance, o.Budget, authority.Instance, governance.Summary{Records: g.Sequence, Bytes: int64(g.Offset), LastHash: g.Hash}, persist); err != nil {
				return fmt.Errorf("verify actual Governance cache through common publication: %w", err)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("verify actual Audit cache through common publication: %w", err)
		}
		return nil
	})
}
