package app

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/statecut"
)

var errHACacheAfterSelected = errors.New("HA inactive cache is newer than the selected common")

// This private callback is used only by the actual full Member/Ordering/four
// source publication freeze. A cache MAC alone cannot authorize replacement:
// the original selected common, actual native prefix and joined reader must
// independently qualify an inactive destination. Interrupted pending files are
// opaque and are never adopted or deleted. Four fixed pending extents are paid
// by the same foundation even on a later process's startup.
func (c *HASelectedCheckpoint) prepareChangedFrozenCaches(ctx context.Context) (result statecut.StartupCacheCandidates, resultErr error) {
	f := c.owner
	actual := f.reopened.Owners.Components
	if f.cfg.ResourceBudget.PhysicalTerminalReserve {
		return result, authenticatedstate.ErrSealIneligible
	}
	var selected authenticatedstate.CandidateSeal
	err := actual.NativeLedger.WithHASelectedSealAuthority(ctx, func(ctx context.Context, a authenticatedstate.SealEligibility, _ ledger.HASelectedLedgerPosition) error {
		var found bool
		var err error
		selected, found, err = actual.Publisher.SelectContext(ctx, a)
		if err != nil || !found || selected != f.reopened.Seal {
			return errors.Join(authenticatedstate.ErrSealIneligible, err)
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	// Prefer a genuinely empty pair, preserving the established first-publication
	// and interrupted half-pair behavior. Only after those slots fill do we rotate.
	slot := -1
	var previous [2]os.FileInfo
	for n := range haSelectedCacheSlots {
		_, ae := os.Lstat(haSelectedCachePath(f.cfg.Storage.DataDir, n, false))
		_, ge := os.Lstat(haSelectedCachePath(f.cfg.Storage.DataDir, n, true))
		if ae != nil && !errors.Is(ae, os.ErrNotExist) {
			return result, ae
		}
		if ge != nil && !errors.Is(ge, os.ErrNotExist) {
			return result, ge
		}
		if errors.Is(ae, os.ErrNotExist) && errors.Is(ge, os.ErrNotExist) {
			slot = n
			break
		}
	}
	if slot < 0 {
		for n := range haSelectedCacheSlots {
			ap, gp := haSelectedCachePath(f.cfg.Storage.DataDir, n, false), haSelectedCachePath(f.cfg.Storage.DataDir, n, true)
			if ap == f.auditCachePath || gp == f.governanceCachePath {
				continue
			}
			occupied := false
			for _, p := range []string{ap + ".pending", gp + ".pending"} {
				if _, err := os.Lstat(p); err == nil {
					occupied = true
				} else if !errors.Is(err, os.ErrNotExist) {
					return result, err
				}
			}
			if occupied {
				continue
			}
			ai, err := c.inspectInactiveCache(ctx, ap, func(file *os.File, size int64) (e error) {
				cache, e := audit.ReadStartupCache(ctx, file, size, f.bootstrap.AuditKey, selected.Instance, actual.Budget)
				if e != nil {
					return e
				}
				defer func() { e = errors.Join(e, cache.Close()) }()
				h := cache.Head()
				p := selected.Positions[3]
				if h.Records > p.Sequence || h.Bytes < 0 || uint64(h.Bytes) > p.Offset {
					return errHACacheAfterSelected
				}
				_, e = actual.Audit.SealReference(ctx, actual.Budget, h)
				return e
			})
			if err != nil {
				if errors.Is(err, errHACacheAfterSelected) && !errors.Is(err, ownedfile.ErrRetirement) {
					continue
				}
				return result, err
			}
			gi, err := c.inspectInactiveCache(ctx, gp, func(file *os.File, size int64) (e error) {
				cache, e := governance.ReadStartupCache(ctx, file, size, f.bootstrap.GovernanceKey, selected.Instance, actual.Budget)
				if e != nil {
					return e
				}
				defer func() { e = errors.Join(e, cache.Close()) }()
				h := cache.Head()
				p := selected.Positions[2]
				if h.Records > p.Sequence || h.Bytes < 0 || uint64(h.Bytes) > p.Offset {
					return errHACacheAfterSelected
				}
				_, e = actual.NativeGovernance.SealReference(ctx, actual.Budget, h)
				return e
			})
			if err != nil {
				if errors.Is(err, errHACacheAfterSelected) && !errors.Is(err, ownedfile.ErrRetirement) {
					continue
				}
				return result, err
			}
			slot = n
			previous = [2]os.FileInfo{ai, gi}
			break
		}
	}
	if slot < 0 {
		return result, fmt.Errorf("HA capsule pending slots exhausted: %w", authenticatedstate.ErrLimit)
	}
	c.auditCapture, err = actual.Audit.CaptureHAStartupCache(ctx, actual.Budget, selected.Instance, actual.Audit.Summary())
	if err != nil {
		return result, err
	}
	c.governanceCapture, err = actual.NativeGovernance.CaptureHAStartupCache(ctx, actual.Budget, selected.Instance, actual.NativeGovernance.Summary())
	if err != nil {
		return result, err
	}
	ap, gp := haSelectedCachePath(f.cfg.Storage.DataDir, slot, false), haSelectedCachePath(f.cfg.Storage.DataDir, slot, true)
	ad, gd := f.cacheDisks[slot][0], f.cacheDisks[slot][1]
	if previous[0] != nil {
		ap += ".pending"
		ad = f.cachePendingDisk
	}
	if previous[1] != nil {
		gp += ".pending"
		gd = f.cachePendingDisk
	}
	c.caches.Audit, err = c.auditCapture.PersistNew(ctx, ap, audit.StartupCachePersistOptions{Guard: actual.Guard, Budget: actual.Budget, DiskReservation: ad, Identity: selected.Instance, Head: c.auditCapture.Head()})
	if err != nil {
		return c.caches, err
	}
	observeHASelectedCheckpoint(ctx, "audit-pending")
	if previous[0] != nil {
		if err := c.caches.Audit.ReplacePendingCandidate(ctx, ap[:len(ap)-len(".pending")], previous[0]); err != nil {
			return c.caches, err
		}
	}
	observeHASelectedCheckpoint(ctx, "audit-capsule")
	c.caches.Governance, err = c.governanceCapture.PersistNew(ctx, gp, governance.StartupCachePersistOptions{Guard: actual.Guard, Budget: actual.Budget, DiskReservation: gd, Identity: selected.Instance, Head: c.governanceCapture.Head()})
	if err != nil {
		return c.caches, err
	}
	observeHASelectedCheckpoint(ctx, "governance-pending")
	if previous[1] != nil {
		if err := c.caches.Governance.ReplacePendingCandidate(ctx, gp[:len(gp)-len(".pending")], previous[1]); err != nil {
			return c.caches, err
		}
	}
	observeHASelectedCheckpoint(ctx, "governance-capsule")
	return c.caches, nil
}
