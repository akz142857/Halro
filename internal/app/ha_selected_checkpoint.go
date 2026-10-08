package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
	"github.com/akz142857/Halro/internal/store/lock"
)

// Four immutable capsule slots and four paid pending pairs bound both ordinary
// rotation and opaque interruption debt. Full-cut qualification may replace an
// inactive authenticated native prefix; unknown pending candidates stay intact.
const haSelectedCacheSlots = 4

type haSelectedCheckpointPhaseKey struct{}

func observeHASelectedCheckpoint(ctx context.Context, phase string) {
	if observer, ok := ctx.Value(haSelectedCheckpointPhaseKey{}).(func(string)); ok && observer != nil {
		observer(phase)
	}
}

func haSelectedCachePath(directory string, slot int, governance bool) string {
	kind := "audit"
	if governance {
		kind = "governance"
	}
	return filepath.Join(directory, fmt.Sprintf("memory-%s-%d.cache", kind, slot+1))
}

func (f *haSelectedRuntimeFoundation) readSelectedStartupCaches(ctx context.Context, cut statecut.HASelectedCut) error {
	// Every retained interrupted candidate has a separately paid fixed extent.
	// A name grants no creator receipt: restart only checks its bounded envelope.
	if !f.cfg.ResourceBudget.PhysicalTerminalReserve {
		for slot := range haSelectedCacheSlots {
			for _, gov := range []bool{false, true} {
				info, err := os.Lstat(haSelectedCachePath(f.cfg.Storage.DataDir, slot, gov) + ".pending")
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				maximum := int64(audit.MaxStartupCacheBytes)
				if gov {
					maximum = governance.MaxStartupCacheBytes
				}
				if err != nil {
					return err
				}
				if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 0 || info.Size() > maximum {
					return authenticatedstate.ErrSealIneligible
				}
			}
		}
	}
	for slot := range haSelectedCacheSlots {
		for _, gov := range []bool{false, true} {
			path := haSelectedCachePath(f.cfg.Storage.DataDir, slot, gov)
			info, err := os.Lstat(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			maximum := int64(audit.MaxStartupCacheBytes)
			if gov {
				maximum = governance.MaxStartupCacheBytes
			}
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 0 || info.Size() > maximum {
				return authenticatedstate.ErrSealIneligible
			}
		}
	}
	a, g := cut.Seal.Positions[3], cut.Seal.Positions[2]
	if a.Offset > math.MaxInt64 || g.Offset > math.MaxInt64 {
		return authenticatedstate.ErrSealIneligible
	}
	for slot := range haSelectedCacheSlots {
		for _, gov := range []bool{false, true} {
			if (!gov && f.auditCache != nil) || (gov && f.governanceCache != nil) {
				continue
			}
			path := haSelectedCachePath(f.cfg.Storage.DataDir, slot, gov)
			if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
				continue
			} else if err != nil {
				return err
			}
			if err := f.readCacheFile(ctx, path, func(file *os.File, size int64) error {
				if size == 0 {
					return f.validateEmptyHAPhysicalCache(cut.Seal, path, file)
				}
				if gov {
					c, err := governance.ReadStartupCache(ctx, file, size, f.bootstrap.GovernanceKey, f.bootstrap.Evidence.Identity, f.owners.Components.Budget)
					if err != nil {
						return err
					}
					if c.Head() == (governance.Summary{Records: g.Sequence, Bytes: int64(g.Offset), LastHash: g.Hash}) {
						f.governanceCache = c
						f.governanceCachePath = path
						return nil
					}
					if err := c.Close(); err != nil {
						f.governanceCache = c
						f.closeErr = errors.Join(f.closeErr, err)
						return err
					}
					return nil
				}
				c, err := audit.ReadStartupCache(ctx, file, size, f.bootstrap.AuditKey, f.bootstrap.Evidence.Identity, f.owners.Components.Budget)
				if err != nil {
					return err
				}
				if c.Head() == (audit.Summary{Records: a.Sequence, Bytes: int64(a.Offset), LastHash: a.Hash}) {
					f.auditCache = c
					f.auditCachePath = path
					return nil
				}
				if err := c.Close(); err != nil {
					f.auditCache = c
					f.closeErr = errors.Join(f.closeErr, err)
					return err
				}
				return nil
			}); err != nil {
				return err
			}
		}
	}
	if f.auditCache == nil || f.governanceCache == nil {
		return authenticatedstate.ErrSealIneligible
	}
	return nil
}

// HASelectedCheckpoint owns a private, never-served generation, its actual
// descriptors and candidate creator receipts. Closing it joins readers while
// persistent files keep their reservations. Failed publication is not deletion
// authority; candidates remain in fixed slots for authenticated future startup.
type HASelectedCheckpointFailure struct {
	Owner *HASelectedCheckpoint
	Cause error
}

func (e *HASelectedCheckpointFailure) Error() string { return e.Cause.Error() }
func (e *HASelectedCheckpointFailure) Unwrap() error { return e.Cause }

type HASelectedCheckpoint struct {
	owner             *haSelectedRuntimeFoundation
	caches            statecut.StartupCacheCandidates
	auditCapture      *audit.StartupCache
	governanceCapture *governance.StartupCache
	Publication       statecut.Publication
}

func (c *HASelectedCheckpoint) Close() error {
	if c == nil {
		return nil
	}
	err := errors.Join(c.Publication.CleanupError(), c.caches.Audit.Close(), c.caches.Governance.Close(), c.auditCapture.Close(), c.governanceCapture.Close())
	if err != nil {
		return err
	}
	return c.owner.Close()
}

// CheckpointHASelectedRuntime is an offline operation under the actual data
// directory lock. It opens and authenticates every source itself and never
// exposes its private generation to DAO or transport producers. Primary must
// have confirmed all durable facts; Replica must have applied every received
// fact. This full-cut operation does not manufacture recovery or readiness.
func CheckpointHASelectedRuntime(ctx context.Context, cfg config.Config, master []byte, guard *lock.Lock, manager *resourcebudget.Manager, logger *slog.Logger) (result *HASelectedCheckpoint, resultErr error) {
	f, err := openHASelectedRuntimeFoundation(ctx, cfg, master, guard, manager, logger)
	c := &HASelectedCheckpoint{owner: f}
	defer func() {
		if resultErr != nil {
			cleanup := c.Close()
			if cleanup != nil {
				cleanup = errors.Join(ownedfile.ErrRetirement, cleanup)
			}
			resultErr = &HASelectedCheckpointFailure{Owner: c, Cause: errors.Join(resultErr, cleanup)}
		}
	}()
	if err != nil {
		return c, err
	}
	if err = f.prepareSelectedRoots(ctx); err != nil {
		return c, err
	}
	observeHASelectedCheckpoint(ctx, "roots-ready")
	r := f.reopened
	base := r.Seal
	if base.Revision == math.MaxUint64 {
		return c, authenticatedstate.ErrSealIneligible
	}
	base.Revision++
	c.Publication, err = statecut.PublishHASelectedMaintenanceCapturedStartupCaches(ctx, r.Owners, base, func(ctx context.Context) (statecut.StartupCacheCandidates, error) {
		if reused, reuseErr := c.reuseUnchangedFrozenCaches(ctx); reused || reuseErr != nil {
			return c.caches, reuseErr
		}
		return c.prepareChangedFrozenCaches(ctx)
	})
	if err == nil {
		observeHASelectedCheckpoint(ctx, "published")
	}
	return c, err
}
