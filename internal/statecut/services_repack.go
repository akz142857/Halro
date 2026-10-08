package statecut

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

type selectedRepackOwner interface {
	PrepareSelectedRepack(context.Context, *authenticatedstate.SealCandidatePublisher, *lock.Lock, authenticatedstate.CandidateSeal, resourcebudget.Permit) (*authenticatedstate.StoreRepack, error)
}

func (r *ReopenedStandalone) repackSelectedRootsLocked(ctx context.Context, prepare func(context.Context) (StartupCacheCandidates, error), reservation resourcebudget.Permit, checkpoint Publication) (result Publication, resultErr error) {
	result = checkpoint
	owners := [5]selectedRepackOwner{r.Owners.Ledger, r.Owners.Metadata, r.Owners.Governance, r.Owners.Usage, r.Owners.Directory}
	var candidates [5]*authenticatedstate.StoreRepack
	committed := false
	defer func() {
		if committed {
			return
		}
		var cleanupErr error
		cleanupErr = r.Owners.Directory.WithPhysicalCatchupWorkspace(context.WithoutCancel(ctx), func(ctx context.Context) error {
			var err error
			for i := len(candidates) - 1; i >= 0; i-- {
				err = errors.Join(err, candidates[i].RollbackContext(ctx))
			}
			return err
		})
		if cleanupErr != nil {
			// A possibly durable common selection or unknown FD retirement must
			// keep both packs and their fees; no business promise survives it.
			r.Owners.NativeLedger.Status().RequireRecovery()
		}
		resultErr = errors.Join(resultErr, cleanupErr)
	}()
	count := 0
	for i, owner := range owners {
		// Each candidate retains its own 4 KiB control claim until commit or
		// rollback. These persistent lifetimes cannot consume the publication
		// pool's finite simultaneous root-freeze slots.
		var candidate *authenticatedstate.StoreRepack
		var err error
		if i == 0 {
			// The actual native owner's observed pre-admission vector can need
			// compaction before half the ceiling. This preserves independent
			// common-root/NoDebt and benefit qualification in the Store.
			candidate, err = r.Owners.Ledger.PrepareSelectedRepackForPhysicalHeadroom(resourcebudget.WithoutRAMWorkspace(ctx), r.Owners.Publisher, r.Owners.Guard, r.Seal, reservation)
		} else {
			candidate, err = owner.PrepareSelectedRepack(resourcebudget.WithoutRAMWorkspace(ctx), r.Owners.Publisher, r.Owners.Guard, r.Seal, reservation)
		}
		candidates[i] = candidate
		if err != nil {
			return result, err
		}
		if candidate != nil {
			count++
		}
	}
	if count == 0 {
		committed = true
		return result, nil
	}
	result, resultErr = r.publishCurrentSelectedRootsLocked(ctx, prepare)
	if resultErr != nil {
		return result, resultErr
	}
	resultErr = r.Owners.Directory.WithPhysicalCatchupWorkspace(context.WithoutCancel(ctx), func(ctx context.Context) error {
		var err error
		for _, candidate := range candidates {
			err = errors.Join(err, candidate.CommitContext(ctx))
		}
		return err
	})
	committed = true
	if resultErr == nil {
		resultErr = r.finishPhysicalCommonSelectionLocked(context.WithoutCancel(ctx))
	}
	if resultErr != nil {
		r.Owners.NativeLedger.Status().RequireRecovery()
	}
	return result, resultErr
}
