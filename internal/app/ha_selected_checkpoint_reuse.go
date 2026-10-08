package app

import (
	"context"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/governance"
)

// Called only inside the actual complete publication freeze. The capsules
// were fully authenticated on startup and must still match their real native
// owners here. Each read-only file loan independently verifies current bytes,
// inode and parent and retains the original fixed-slot Disk reservation.
func (c *HASelectedCheckpoint) reuseUnchangedFrozenCaches(ctx context.Context) (bool, error) {
	f := c.owner
	actual := f.reopened.Owners.Components
	if f.auditCache == nil || f.governanceCache == nil || f.auditCachePath == "" || f.governanceCachePath == "" || f.auditCache.Head() != actual.Audit.Summary() || f.governanceCache.Head() != actual.NativeGovernance.Summary() {
		return false, nil
	}
	as, gs := -1, -1
	for i := range haSelectedCacheSlots {
		if haSelectedCachePath(f.cfg.Storage.DataDir, i, false) == f.auditCachePath {
			as = i
		}
		if haSelectedCachePath(f.cfg.Storage.DataDir, i, true) == f.governanceCachePath {
			gs = i
		}
	}
	if as < 0 || gs < 0 {
		return false, authenticatedstate.ErrSealIneligible
	}
	var err error
	c.caches.Audit, err = f.auditCache.ReuseFrozenFile(ctx, actual.Audit, f.auditCachePath, audit.StartupCachePersistOptions{Guard: actual.Guard, Budget: actual.Budget, DiskReservation: f.cacheDisks[as][0], Identity: f.bootstrap.Evidence.Identity, Head: f.auditCache.Head()})
	if err != nil {
		return false, err
	}
	observeHASelectedCheckpoint(ctx, "audit-capsule")
	c.caches.Governance, err = f.governanceCache.ReuseFrozenFile(ctx, actual.NativeGovernance, f.governanceCachePath, governance.StartupCachePersistOptions{Guard: actual.Guard, Budget: actual.Budget, DiskReservation: f.cacheDisks[gs][1], Identity: f.bootstrap.Evidence.Identity, Head: f.governanceCache.Head()})
	if err == nil {
		observeHASelectedCheckpoint(ctx, "governance-capsule")
	}
	return err == nil, err
}
