package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
)

func (s *runtimeSelectedState) preparePhysicalCacheTargets(ctx context.Context, cfg config.Config) error {
	if cfg.ResourceBudget == nil || !cfg.ResourceBudget.PhysicalTerminalReserve {
		return nil
	}
	o := s.generation.Owners
	// Caller has joined the actual four audits. During physical startup this
	// callback runs inside StartSelectedServices under its owner mutex; reentering
	// WaitStartupAudit here would deadlock that real startup lane.
	current := s.generation.Seal
	references, found, err := s.publisher.PhysicalCacheTargetReferences(current)
	if err != nil {
		return err
	}
	if _, err := s.cacheAuditDisk.Cost(); err != nil {
		s.cacheAuditDisk, err = o.Budget.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: 4 * audit.MaxStartupCacheBytes})
		if err != nil {
			return err
		}
		s.fees = append(s.fees, s.cacheAuditDisk)
	}
	if _, err := s.cacheGovernanceDisk.Cost(); err != nil {
		s.cacheGovernanceDisk, err = o.Budget.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: 4 * governance.MaxStartupCacheBytes})
		if err != nil {
			return err
		}
		s.fees = append(s.fees, s.cacheGovernanceDisk)
	}
	for i := 0; i < 4; i++ {
		path, bytes, permit := s.physicalCacheTargetPath(cfg, i)
		if s.physicalCaches[i] != nil {
			if err := s.physicalCaches[i].ValidatePrepared(path, bytes, o.Budget); err != nil {
				return err
			}
			continue
		}
		if found {
			if references[i].Bytes != bytes {
				return authenticatedstate.ErrRecovery
			}
			if s.physicalCacheWork[i] == (resourcebudget.Permit{}) {
				s.physicalCaches[i], err = resourcebudget.RestorePhysicalCandidate(ctx, path, references[i], o.Budget, permit)
			} else {
				s.physicalCaches[i], err = resourcebudget.RestorePhysicalCandidateWithOwnedWorkspace(ctx, path, references[i], o.Budget, permit, s.physicalCacheWork[i])
			}
		} else {
			verify := func(file *os.File, size int64) (resultErr error) {
				if i < 2 {
					c, err := audit.ReadStartupCache(ctx, file, size, s.bootstrap.AuditKey, current.Instance, o.Budget)
					if err != nil {
						return err
					}
					defer func() { resultErr = errors.Join(resultErr, c.Close()) }()
					_, err = o.Audit.SealReference(ctx, o.Budget, c.Head())
					return err
				}
				c, err := governance.ReadStartupCache(ctx, file, size, s.bootstrap.GovernanceKey, current.Instance, o.Budget)
				if err != nil {
					return err
				}
				defer func() { resultErr = errors.Join(resultErr, c.Close()) }()
				_, err = o.NativeGovernance.SealReference(ctx, o.Budget, c.Head())
				return err
			}
			info, err := inspectInactiveRuntimeCache(ctx, path, verify)
			if err != nil {
				return err
			}
			if info == nil {
				s.physicalCaches[i], err = resourcebudget.PreparePhysicalCandidate(ctx, path, bytes, o.Budget, permit)
			} else {
				s.physicalCaches[i], err = resourcebudget.PrepareCapturedPhysicalCandidate(ctx, path, info, bytes, o.Budget, permit)
			}
			if err != nil {
				return err
			}
			references[i], err = s.physicalCaches[i].Reference()
		}
		if err != nil {
			return err
		}
		if s.physicalCacheWork[i] == (resourcebudget.Permit{}) {
			s.physicalCacheWork[i], err = s.physicalCaches[i].RetainIndependentWorkspace(o.Budget)
			if err != nil {
				return err
			}
		}
	}
	if !found {
		return s.publisher.BindPhysicalCacheTargets(current, references)
	}
	return nil
}
func (s *runtimeSelectedState) physicalCacheTargetPath(cfg config.Config, index int) (string, int64, resourcebudget.Permit) {
	if index < 2 {
		return filepath.Join(cfg.Storage.DataDir, fmt.Sprintf("memory-audit-%d.cache", index)), audit.MaxStartupCacheBytes, s.cacheAuditDisk
	}
	return filepath.Join(cfg.Storage.DataDir, fmt.Sprintf("memory-governance-%d.cache", index-2)), governance.MaxStartupCacheBytes, s.cacheGovernanceDisk
}

// Both actual cache slots already exist and retain their allocated ranges.
// Overwrite only the inactive slot fixed by the current common revision. No
// pending link, old-inode unlink or released free-space balance is borrowed.
func preparePhysicalSelectedCheckpointCaches(ctx context.Context, cfg config.Config, s *runtimeSelectedState) (result statecut.StartupCacheCandidates, resultErr error) {
	if err := s.preparePhysicalCacheTargets(ctx, cfg); err != nil {
		return result, err
	}
	o := s.generation.Owners
	current := s.generation.Seal
	slot := (current.Revision + 1) % 2
	previous := s.cacheSlots[slot]
	if err := errors.Join(previous.Audit.CloseWithPersistentReservation(s.cacheAuditDisk), previous.Governance.CloseWithPersistentReservation(s.cacheGovernanceDisk)); err != nil {
		return result, err
	}
	s.cacheSlots[slot] = statecut.StartupCacheCandidates{}
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
	ap, _, ad := s.physicalCacheTargetPath(cfg, int(slot))
	gp, _, gd := s.physicalCacheTargetPath(cfg, 2+int(slot))
	s.cacheSlots[slot].Audit, err = ac.PersistNew(ctx, ap, audit.StartupCachePersistOptions{Guard: o.Guard, Budget: o.Budget, Identity: current.Instance, Head: ac.Head(), DiskReservation: ad, PhysicalCandidate: s.physicalCaches[slot]})
	if err != nil {
		return result, err
	}
	s.physicalCaches[slot] = nil
	s.cacheSlots[slot].Governance, err = gc.PersistNew(ctx, gp, governance.StartupCachePersistOptions{Guard: o.Guard, Budget: o.Budget, Identity: current.Instance, Head: gc.Head(), DiskReservation: gd, PhysicalCandidate: s.physicalCaches[2+slot]})
	if err != nil {
		return result, err
	}
	s.physicalCaches[2+slot] = nil
	s.checkpointCaches = s.cacheSlots[slot]
	return s.checkpointCaches, nil
}
