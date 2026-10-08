package app

import (
	"context"
	"errors"
	"os"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
)

// The first two original HA capsule slots retain their original one-file Disk
// ceilings. The other immutable slots stay intact. Workspace fees are retained
// independently before lending a temporary whole-publication workspace.
type haPhysicalCacheOwner struct {
	targets      [4]*resourcebudget.PhysicalCandidate
	work         [4]resourcebudget.Permit
	publications [2]statecut.StartupCacheCandidates
}

func (f *haSelectedRuntimeFoundation) prepareHAPhysicalPublicationTargets(ctx context.Context) error {
	if f.cfg.ResourceBudget == nil || !f.cfg.ResourceBudget.PhysicalTerminalReserve {
		return nil
	}
	if f.reopened == nil {
		return authenticatedstate.ErrSealIneligible
	}
	var selected authenticatedstate.CandidateSeal
	if err := f.withSelectedCut(ctx, func(ctx context.Context, cut statecut.HASelectedCut) error {
		if err := statecut.RequireNoHAOrderingPeerPromises(ctx, f.owners, f.physicalPublication); err != nil {
			return err
		}
		selected = cut.Seal
		return f.owners.Components.Publisher.EnableHAQualifiedPhysicalSealPreparation(ctx, selected, f.physicalSealDisk, cut.Authority)
	}); err != nil {
		return err
	}
	return f.prepareHAPhysicalCacheTargets(ctx, selected)
}

func (o *haPhysicalCacheOwner) close() error {
	if o == nil {
		return nil
	}
	var err error
	for _, p := range o.publications {
		err = errors.Join(err, p.Audit.Close(), p.Governance.Close())
	}
	for _, p := range o.targets {
		err = errors.Join(err, p.ClosePreserving())
	}
	if err != nil {
		return err
	}
	for i := range o.work {
		if o.work[i] != (resourcebudget.Permit{}) {
			if e := o.work[i].Release(); e != nil {
				return e
			}
			o.work[i] = resourcebudget.Permit{}
		}
	}
	return nil
}

func (f *haSelectedRuntimeFoundation) haPhysicalCacheSlot(index int) (string, int64, resourcebudget.Permit) {
	gov, slot := index >= 2, index%2
	if gov {
		return haSelectedCachePath(f.cfg.Storage.DataDir, slot, true), governance.MaxStartupCacheBytes, f.cacheDisks[slot][1]
	}
	return haSelectedCachePath(f.cfg.Storage.DataDir, slot, false), audit.MaxStartupCacheBytes, f.cacheDisks[slot][0]
}

func (f *haSelectedRuntimeFoundation) prepareHAPhysicalCacheTargets(ctx context.Context, selected authenticatedstate.CandidateSeal) error {
	c := f.owners.Components
	refs, found, err := c.Publisher.PhysicalCacheTargetReferences(selected)
	if err != nil {
		return err
	}
	if f.physicalCaches == nil {
		f.physicalCaches = &haPhysicalCacheOwner{}
	}
	o := f.physicalCaches
	for i := range o.targets {
		path, limit, fee := f.haPhysicalCacheSlot(i)
		if o.targets[i] != nil {
			if err := o.targets[i].ValidatePrepared(path, limit, c.Budget); err != nil {
				return err
			}
			continue
		}
		if found {
			if refs[i].Bytes != limit {
				return authenticatedstate.ErrRecovery
			}
			if o.work[i] == (resourcebudget.Permit{}) {
				o.targets[i], err = resourcebudget.RestorePhysicalCandidate(ctx, path, refs[i], c.Budget, fee)
			} else {
				o.targets[i], err = resourcebudget.RestorePhysicalCandidateWithOwnedWorkspace(ctx, path, refs[i], c.Budget, fee, o.work[i])
			}
		} else {
			verify := func(file *os.File, size int64) (resultErr error) {
				if i < 2 {
					cache, err := audit.ReadStartupCache(ctx, file, size, f.bootstrap.AuditKey, selected.Instance, c.Budget)
					if err != nil {
						return err
					}
					defer func() { resultErr = errors.Join(resultErr, cache.Close()) }()
					_, err = c.Audit.SealReference(ctx, c.Budget, cache.Head())
					return err
				}
				cache, err := governance.ReadStartupCache(ctx, file, size, f.bootstrap.GovernanceKey, selected.Instance, c.Budget)
				if err != nil {
					return err
				}
				defer func() { resultErr = errors.Join(resultErr, cache.Close()) }()
				_, err = c.NativeGovernance.SealReference(ctx, c.Budget, cache.Head())
				return err
			}
			info, e := inspectInactiveRuntimeCache(ctx, path, verify)
			if e != nil {
				return e
			}
			if info == nil {
				o.targets[i], err = resourcebudget.PreparePhysicalCandidate(ctx, path, limit, c.Budget, fee)
			} else {
				o.targets[i], err = resourcebudget.PrepareCapturedPhysicalCandidate(ctx, path, info, limit, c.Budget, fee)
			}
			if err == nil {
				refs[i], err = o.targets[i].Reference()
			}
		}
		if err != nil {
			return err
		}
		if o.work[i] == (resourcebudget.Permit{}) {
			o.work[i], err = o.targets[i].RetainIndependentWorkspace(c.Budget)
			if err != nil {
				return err
			}
		}
	}
	if !found {
		return c.Publisher.BindPhysicalCacheTargets(selected, refs)
	}
	return nil
}

// A zero-length capsule is skipped only when the actual authenticated physical
// receipt owns precisely this FD birth. Unknown zero files still fail parsing.
func (f *haSelectedRuntimeFoundation) validateEmptyHAPhysicalCache(selected authenticatedstate.CandidateSeal, path string, file *os.File) error {
	if f.cfg.ResourceBudget == nil || !f.cfg.ResourceBudget.PhysicalTerminalReserve {
		return authenticatedstate.ErrRecovery
	}
	refs, found, err := f.owners.Components.Publisher.PhysicalCacheTargetReferences(selected)
	if err != nil || !found {
		return errors.Join(authenticatedstate.ErrRecovery, err)
	}
	id, err := resourcebudget.CapturePhysicalIdentity(file)
	if err != nil {
		return err
	}
	for i := range refs {
		actualPath, limit, _ := f.haPhysicalCacheSlot(i)
		if actualPath == path && refs[i].File == id && refs[i].Bytes == limit {
			return errors.Join(resourcebudget.CheckPhysicalRange(file, 0, limit), resourcebudget.CheckPhysicalTail(file, 0, limit))
		}
	}
	return authenticatedstate.ErrRecovery
}

func (f *haSelectedRuntimeFoundation) prepareHAPhysicalCheckpointCaches(ctx context.Context, selected authenticatedstate.CandidateSeal) (result statecut.StartupCacheCandidates, resultErr error) {
	if err := f.prepareHAPhysicalCacheTargets(ctx, selected); err != nil {
		return result, err
	}
	c, o := f.owners.Components, f.physicalCaches
	slot := int(selected.Revision % 2)
	prior := o.publications[slot]
	if err := errors.Join(prior.Audit.CloseWithPersistentReservation(f.cacheDisks[slot][0]), prior.Governance.CloseWithPersistentReservation(f.cacheDisks[slot][1])); err != nil {
		return result, err
	}
	o.publications[slot] = statecut.StartupCacheCandidates{}
	ac, err := c.Audit.CaptureHAStartupCache(ctx, c.Budget, selected.Instance, c.Audit.Summary())
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, ac.Close()) }()
	gc, err := c.NativeGovernance.CaptureHAStartupCache(ctx, c.Budget, selected.Instance, c.NativeGovernance.Summary())
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, gc.Close()) }()
	ap, _, ad := f.haPhysicalCacheSlot(slot)
	gp, _, gd := f.haPhysicalCacheSlot(2 + slot)
	o.publications[slot].Audit, err = ac.PersistNew(ctx, ap, audit.StartupCachePersistOptions{Guard: c.Guard, Budget: c.Budget, Identity: selected.Instance, Head: ac.Head(), DiskReservation: ad, PhysicalCandidate: o.targets[slot]})
	if err != nil {
		return result, err
	}
	o.targets[slot] = nil
	o.publications[slot].Governance, err = gc.PersistNew(ctx, gp, governance.StartupCachePersistOptions{Guard: c.Guard, Budget: c.Budget, Identity: selected.Instance, Head: gc.Head(), DiskReservation: gd, PhysicalCandidate: o.targets[2+slot]})
	if err != nil {
		return result, err
	}
	o.targets[2+slot] = nil
	return o.publications[slot], nil
}
