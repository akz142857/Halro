package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/statecut"
)

// Installed before any transport callback. Actual peer Hellos and real send
// windows start the existing completion worker; merely reopening a confirmed
// baseline never releases the Runtime producer channel.
func (f *haSelectedRuntimeFoundation) prepareHASelectedPrimaryStartup(ctx context.Context) error {
	if f == nil || f.reopened == nil || f.replication == nil || f.replication.role != replication.RolePrimary || f.business == nil || f.business.accounting == nil || f.business.collector == nil || f.replication.manager != nil || f.replication.startupCtx != nil {
		return authenticatedstate.ErrSealIneligible
	}
	c, r := f.reopened.Owners.Components, f.replication
	validate := func(ctx context.Context) error {
		return c.Ledger.ValidateHASelectedNativeWriter(ctx, c.NativeLedger, c.Lease, c.Budget)
	}
	var err error
	if f.cfg.ResourceBudget.PhysicalTerminalReserve {
		err = c.Directory.WithPhysicalCatchupWorkspace(ctx, validate)
	} else {
		err = validate(ctx)
	}
	if err != nil {
		return err
	}
	member := r.publisher.Snapshot()
	if member.Role != replication.RolePrimary || member.Term != member.PromisedTerm || len(member.Peers) == 0 {
		return replication.ErrMemberAuthority
	}
	r.startupPeers = make(map[string]replication.Hello, len(member.Peers))
	r.startupIndex = member.DurableIndex
	r.startupPeerQuorum = len(member.Peers)
	r.startupCtx, r.startupStop = context.WithCancel(f.ctx)
	r.startupProducers = make(chan struct{})
	r.startupFinish = f.finishHASelectedPrimaryStartup
	return nil
}

func (f *haSelectedRuntimeFoundation) finishHASelectedPrimaryStartup(ctx context.Context) (resultErr error) {
	if f.cfg.ResourceBudget.PhysicalTerminalReserve {
		return f.reopened.Owners.Components.Directory.WithPhysicalCatchupWorkspace(ctx, f.finishHASelectedPrimaryStartupOwned)
	}
	return f.finishHASelectedPrimaryStartupOwned(ctx)
}

func (f *haSelectedRuntimeFoundation) finishHASelectedPrimaryStartupOwned(ctx context.Context) (resultErr error) {
	c := f.reopened.Owners.Components
	defer func() {
		if resultErr != nil {
			c.NativeLedger.Status().RequireRecovery()
			f.applyErr = errors.Join(f.applyErr, resultErr)
		}
	}()
	if _, err := f.recoverHASelectedAccountingMode(ctx, c.Lease, true); err != nil {
		return fmt.Errorf("HA selected startup two-level accounting recovery: %w", err)
	}
	if f.applyPhase != nil {
		f.applyPhase("startup-accounting-recovered")
	}
	if _, err := f.reopened.Derivative.CatchUpHASelectedStartup(ctx, c.NativeLedger, c.Lease, c.Budget); err != nil {
		return fmt.Errorf("HA selected startup Usage catch-up: %w", err)
	}
	if _, err := c.Directory.AdvanceHASelectedMaintenanceFromNative(ctx, c.NativeLedger, c.Lease, c.Ledger); err != nil {
		return fmt.Errorf("HA selected startup Directory catch-up: %w", err)
	}
	if err := c.Lease.WithHASelectedRecoveryContext(ctx, func(ctx context.Context) error { return f.metadataStore.RecoverDeploymentPricePins(ctx, c.Ledger) }); err != nil {
		return fmt.Errorf("HA selected startup price pins: %w", err)
	}
	if err := c.Lease.WithHASelectedRecoveryHead(ctx, func(ctx context.Context, head ledger.Watermark, hash [32]byte) error {
		actual, actualHash, err := c.Ledger.ReadPosition(ctx)
		if err != nil || actual != head || actualHash != hash {
			return errors.Join(authenticatedstate.ErrSealIneligible, err)
		}
		target, err := f.replication.coordinator.CaptureRecoveryTargetWhenAvailable(ctx, replication.StoreCursor{Generation: head.Generation, Sequence: head.Sequence})
		if err != nil {
			return err
		}
		return f.replication.coordinator.WaitRecoveryTargetWithPeer(ctx, target)
	}); err != nil {
		return fmt.Errorf("HA selected startup complete Ordering target: %w", err)
	}
	if err := statecut.ValidateHASelectedMaintenance(ctx, f.reopened.Owners); err != nil {
		return fmt.Errorf("HA selected startup all seven current roots: %w", err)
	}
	return c.Lease.Close()
}
