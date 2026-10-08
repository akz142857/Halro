package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

func TestHASelectedActualTLSAccountingRecoveryUsesRealBudgetCollectorAndOrderingTarget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fixture, owners, _ := prepareHASelectedTransportFixture(t, ctx)
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 2)
	for _, owner := range owners {
		go func(f *haSelectedRuntimeFoundation) { done <- f.serveSelectedTransport(runCtx) }(owner)
	}
	defer func() {
		stop()
		for range 2 {
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		}
	}()
	for !owners[0].replication.manager.PeerConnections()[fixture.configs[1].Replication.NodeID] {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	primary := owners[0]
	if primary.business == nil || primary.business.accounting == nil || primary.business.collector == nil || owners[1].business.accounting != nil {
		t.Fatal("actual role services missing or Replica acquired writer")
	}
	for i, name := range []string{"request-without-attempt", "request-pending", "request-started"} {
		request, err := primary.business.accounting.BeginRequestDetailed(ctx, "prj_ha_recovery", "key_ha_recovery", name, "model")
		if err != nil {
			t.Fatal("actual selected budget accepted request", err)
		}
		if i == 0 {
			continue
		}
		attempt, err := primary.business.accounting.ReserveAttempt(ctx, request, 100000, 10)
		if err != nil {
			t.Fatal("actual selected budget durable lease", err)
		}
		if i == 2 {
			if err := primary.business.accounting.MarkStarted(ctx, attempt); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := primary.business.collector.CatchUp(ctx); err != nil {
		t.Fatal("actual bounded observer collector", err)
	}
	c := primary.reopened.Owners.Components
	input, _, err := c.Ledger.ReadPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := c.NativeLedger.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	primary.owners.Components.Lease, primary.reopened.Owners.Components.Lease = lease, lease
	result, err := primary.recoverHASelectedAccounting(ctx, lease)
	if err != nil || result.InputHead != input || result.AttemptsSettled != 2 || result.RequestsFinalized != 3 || result.ConfirmedHead.Sequence != input.Sequence+5 {
		t.Fatal("actual two-level Primary recovery", result, err)
	}
	if replicated, err := lease.ValidateOwner(c.NativeLedger); err != nil || !replicated {
		t.Fatal("recovery released generation's actual maintenance lease", err)
	}
	again, err := primary.recoverHASelectedAccounting(ctx, lease)
	if err != nil || again.AttemptsSettled != 0 || again.RequestsFinalized != 0 || again.ConfirmedHead != result.ConfirmedHead {
		t.Fatal("actual recovery not idempotent", again, err)
	}
	member := primary.replication.publisher.Snapshot()
	if member.DurableIndex != member.ConfirmedIndex || member.ConfirmedIndex != member.AppliedIndex || primary.replication.startupReady.Load() {
		t.Fatal("recovery target incomplete or manufactured Runtime Ready", member)
	}
	if _, err := owners[1].recoverHASelectedAccounting(ctx, owners[1].owners.Components.Lease); err == nil {
		t.Fatal("Replica acquired semantic recovery")
	}
	for owners[1].replication.publisher.Snapshot().AppliedIndex < member.AppliedIndex {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := primary.installHASelectedAccountingService(ctx); err != nil {
		t.Fatal("reconstruct actual selected budget", err)
	}
	if err := primary.installHASelectedUsageCollector(ctx); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := primary.business.collector.CatchUp(ctx); err != nil {
		t.Fatal("Usage follows recovered actual selected owner", err)
	}
	for _, owner := range owners {
		p := owner.reopened.Owners.Components.Ledger
		for _, name := range []string{"request-without-attempt", "request-pending", "request-started"} {
			final, found, err := p.ReadFinalization(ctx, name)
			if err != nil || !found || final.Event.Kind != ledger.EventRequestFinalized {
				t.Fatal("actual transferable recovered request", name, found, err)
			}
		}
	}
}
