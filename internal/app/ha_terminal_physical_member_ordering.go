package app

import (
	"context"
	"fmt"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
)

// This precedes the recovered Primary coordinator's original state publication.
// An equal recovered progress callback can still rewrite a legacy state file;
// restoring the authentic physical bank first preserves its captured birth pair.
func (f *haSelectedRuntimeFoundation) prepareHAPhysicalMemberOrdering(ctx context.Context) error {
	if f == nil || f.cfg.ResourceBudget == nil {
		return authenticatedstate.ErrSealIneligible
	}
	if !f.cfg.ResourceBudget.PhysicalTerminalReserve {
		return nil
	}
	c, r := f.owners.Components, f.replication
	if r == nil || r.publisher == nil || r.journal == nil {
		return authenticatedstate.ErrSealIneligible
	}
	if err := f.withSelectedCut(ctx, func(ctx context.Context, _ statecut.HASelectedCut) error { return ctx.Err() }); err != nil {
		return err
	}
	if f.physicalPublication != nil {
		return f.physicalPublication.ValidateParents(ctx, r.publisher, r.journal, c.Budget)
	}
	bank, err := c.Budget.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: replication.PhysicalPublicationDiskBytes})
	if err != nil {
		return err
	}
	f.fees = append(f.fees, bank)
	f.physicalPublication, err = r.publisher.PreparePhysicalPublication(ctx, r.journal, c.Budget, replication.PhysicalPublicationOptions{Records: resourcebudget.MaxCompletionBatch, OrderingDisk: f.orderingDisk, BankDisk: bank, Restart: true})
	if err != nil {
		return fmt.Errorf("HA physical original Member/Ordering: %w", err)
	}
	return nil
}

func (f *haSelectedRuntimeFoundation) replenishHAPhysicalStartupOrdering(ctx context.Context) error {
	if f == nil || f.reopened == nil || f.replication == nil || f.replication.manager != nil || f.replication.peerPhysical.Load() != nil || f.physicalPublication == nil {
		return authenticatedstate.ErrSealIneligible
	}
	// A restored receipt preserves its old end; it does not replenish consumed
	// records. Derived owners must already exist so current roots can be checked.
	// Before any transport/credit producer exists, renew only from an independently
	// complete common cut with no peer obligations. A suffix keeps its old bound.
	return f.reopened.Owners.Components.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		return f.withSelectedCut(ctx, func(ctx context.Context, cut statecut.HASelectedCut) error {
			if cut.Seal.OrderingIndex != cut.Member.DurableIndex {
				return nil
			}
			if err := statecut.RequireNoHAOrderingPeerPromises(ctx, f.owners, f.physicalPublication); err != nil {
				return err
			}
			return statecut.ReplenishHAOrderingWindow(ctx, f.reopened.Owners, f.physicalPublication, resourcebudget.MaxCompletionBatch)
		})
	})
}
