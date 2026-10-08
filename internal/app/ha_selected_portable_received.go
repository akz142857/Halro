package app

import (
	"context"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
)

func (f *haSelectedRuntimeFoundation) preparePortableReceivedPublication(ctx context.Context) error {
	if f.cfg.ResourceBudget.PhysicalTerminalReserve || f.replication.role != replication.RoleReplica {
		return nil
	}
	if f.peerPromotionRecovery != nil || f.promotionRecovery != nil || f.reopened == nil || f.ctx.Err() != nil {
		return authenticatedstate.ErrSealIneligible
	}
	if err := f.withSelectedCut(ctx, func(ctx context.Context, _ statecut.HASelectedCut) error { return ctx.Err() }); err != nil {
		return err
	}
	r, c := f.replication, f.owners.Components
	// Recovery may already have restored the original accepted receipt. Reuse
	// must prove the same immutable parents; the pointer alone is no permission.
	if err := r.publisher.ValidatePortableReceivedPublication(ctx, r.journal, c.Budget); err == nil {
		return nil
	} else if err != replication.ErrNoReceivedPublication {
		return err
	}
	fee, err := c.Budget.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: replication.PortableReceivedBankDiskBytes})
	if err != nil {
		return err
	}
	f.fees = append(f.fees, fee)
	_, err = r.publisher.PreparePortableReceivedPublication(ctx, r.journal, r.source, f.owners.Baseline, c.Budget, fee)
	return err
}
