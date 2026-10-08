package app

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
)

// Capture the old inode only after authenticating its complete bounded capsule
// and its actual native prefix. The process lock excludes other Halro owners;
// ReplaceCaptured checks that same inode again at the replacement boundary.
func inspectInactiveRuntimeCache(ctx context.Context, path string, visit func(*os.File, int64) error) (info os.FileInfo, resultErr error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, ownedfile.ErrReplaced
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, ownedfile.CloseFile(file)) }()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(info, actual) {
		return nil, errors.Join(ownedfile.ErrReplaced, err)
	}
	if err := visit(file, actual.Size()); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	named, err := os.Lstat(path)
	if err != nil || !os.SameFile(actual, named) || named.Size() != actual.Size() || !named.ModTime().Equal(actual.ModTime()) {
		return nil, errors.Join(ownedfile.ErrReplaced, err)
	}
	return actual, nil
}

func (r *Runtime) checkpointSelectedState(ctx context.Context) (resultErr error) {
	s := r.selectedState
	if s == nil || r.backgroundWait.drainState.Load() != businessDrainComplete {
		return nil
	}
	return checkpointDrainedSelectedState(ctx, r.config, s)
}

// Caller owns and has joined every producer and DAO borrower of this generation.
func checkpointDrainedSelectedState(ctx context.Context, cfg config.Config, s *runtimeSelectedState) error {
	if err := s.generation.WaitStartupAudit(ctx); err != nil {
		return err
	}
	var caches statecut.StartupCacheCandidates
	err := s.generation.Owners.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		var err error
		caches, err = prepareSelectedCheckpointCaches(ctx, cfg, s)
		return err
	})
	if err != nil {
		return err
	}
	_, err = s.generation.CheckpointSelectedServices(ctx, caches)
	return err
}

// Called under the Runtime checkpoint job and after its complete request drain.
// Live publication additionally holds the selected service/DAO/collector pauses.
func prepareSelectedCheckpointCaches(ctx context.Context, cfg config.Config, s *runtimeSelectedState) (result statecut.StartupCacheCandidates, resultErr error) {
	if cfg.ResourceBudget != nil && cfg.ResourceBudget.PhysicalTerminalReserve {
		return preparePhysicalSelectedCheckpointCaches(ctx, cfg, s)
	}
	o := s.generation.Owners
	current := s.generation.Seal
	if current.Revision == math.MaxUint64 {
		return result, authenticatedstate.ErrSealSequence
	}
	authority := s.bootstrap.Authority
	copy(authority.Positions[:], current.Positions[:])
	selected, found, err := s.publisher.Select(authority)
	if err != nil || !found || selected != current {
		return result, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	// The next revision always uses the inactive slot. An interrupted candidate
	// never replaces the slot named by the currently selected seal.
	slot := (current.Revision + 1) % 2
	directory := cfg.Storage.DataDir
	ap := filepath.Join(directory, fmt.Sprintf("memory-audit-%d.cache", slot))
	gp := filepath.Join(directory, fmt.Sprintf("memory-governance-%d.cache", slot))
	auditVerifier := func(file *os.File, size int64) (err error) {
		c, err := audit.ReadStartupCache(ctx, file, size, s.bootstrap.AuditKey, current.Instance, o.Budget)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, c.Close()) }()
		h := c.Head()
		head := o.Audit.Summary()
		if h.Records > head.Records || h.Bytes < 0 || h.Bytes > head.Bytes {
			return authenticatedstate.ErrSealIneligible
		}
		_, err = o.Audit.SealReference(ctx, o.Budget, h)
		return err
	}
	ai, err := inspectInactiveRuntimeCache(ctx, ap, auditVerifier)
	if err != nil {
		return result, err
	}
	governanceVerifier := func(file *os.File, size int64) (err error) {
		c, err := governance.ReadStartupCache(ctx, file, size, s.bootstrap.GovernanceKey, current.Instance, o.Budget)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, c.Close()) }()
		h := c.Head()
		head := o.NativeGovernance.Summary()
		if h.Records > head.Records || h.Bytes < 0 || h.Bytes > head.Bytes {
			return authenticatedstate.ErrSealIneligible
		}
		_, err = o.NativeGovernance.SealReference(ctx, o.Budget, h)
		return err
	}
	gi, err := inspectInactiveRuntimeCache(ctx, gp, governanceVerifier)
	if err != nil {
		return result, err
	}
	for _, candidate := range []struct {
		path   string
		verify func(*os.File, int64) error
	}{{ap + ".pending", auditVerifier}, {gp + ".pending", governanceVerifier}} {
		info, err := inspectInactiveRuntimeCache(ctx, candidate.path, candidate.verify)
		if err != nil {
			return result, err
		}
		if info == nil {
			continue
		}
		actual, found, err := s.publisher.Select(authority)
		if err != nil || !found || actual != current {
			return result, errors.Join(authenticatedstate.ErrSealIneligible, err)
		}
		named, err := os.Lstat(candidate.path)
		if err != nil || !os.SameFile(info, named) || !o.Guard.CoversDataDirectory(directory) {
			return result, errors.Join(ownedfile.ErrReplaced, err)
		}
		if err := os.Remove(candidate.path); err != nil {
			return result, err
		}
		if err := durable.SyncDirectory(directory); err != nil {
			return result, err
		}
	}
	ac, err := o.Audit.CaptureStartupCache(ctx, o.Budget, current.Instance)
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, ac.Close()) }()
	gc, err := o.NativeGovernance.CaptureStartupCache(ctx, o.Budget, current.Instance)
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, gc.Close()) }()
	// Four fixed file extents cover both selected/inactive slots and their
	// replacement candidates. These two claims are reused for every revision.
	if _, err := s.cacheAuditDisk.Cost(); err != nil {
		s.cacheAuditDisk, err = o.Budget.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: 4 * audit.MaxStartupCacheBytes})
		if err != nil {
			return result, err
		}
		s.fees = append(s.fees, s.cacheAuditDisk)
	}
	if _, err := s.cacheGovernanceDisk.Cost(); err != nil {
		s.cacheGovernanceDisk, err = o.Budget.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: 4 * governance.MaxStartupCacheBytes})
		if err != nil {
			return result, err
		}
		s.fees = append(s.fees, s.cacheGovernanceDisk)
	}
	ad, gd := s.cacheAuditDisk, s.cacheGovernanceDisk
	previous := s.cacheSlots[slot]
	if err := errors.Join(previous.Audit.CloseWithPersistentReservation(ad), previous.Governance.CloseWithPersistentReservation(gd)); err != nil {
		return result, err
	}
	s.cacheSlots[slot] = statecut.StartupCacheCandidates{}
	newAudit, newGov := ap, gp
	if ai != nil {
		newAudit += ".pending"
	}
	if gi != nil {
		newGov += ".pending"
	}
	s.cacheSlots[slot].Audit, err = ac.PersistNew(ctx, newAudit, audit.StartupCachePersistOptions{Guard: o.Guard, Budget: o.Budget, Identity: current.Instance, Head: ac.Head(), DiskReservation: ad})
	if err != nil {
		return result, err
	}
	s.cacheSlots[slot].Governance, err = gc.PersistNew(ctx, newGov, governance.StartupCachePersistOptions{Guard: o.Guard, Budget: o.Budget, Identity: current.Instance, Head: gc.Head(), DiskReservation: gd})
	if err != nil {
		return result, err
	}
	if ai != nil {
		if err := s.cacheSlots[slot].Audit.ReplacePendingCandidate(ctx, ap, ai); err != nil {
			return result, err
		}
	}
	if gi != nil {
		if err := s.cacheSlots[slot].Governance.ReplacePendingCandidate(ctx, gp, gi); err != nil {
			return result, err
		}
	}
	s.checkpointCaches = s.cacheSlots[slot]
	return s.checkpointCaches, nil
}
