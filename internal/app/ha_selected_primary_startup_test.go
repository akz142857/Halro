package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

func TestHASelectedActualTLSPrimaryStartupRecoversPendingBeforeSevenRootReady(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fixture, old, _ := prepareHASelectedTransportFixture(t, ctx)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 2)
	for _, owner := range old {
		go func(f *haSelectedRuntimeFoundation) { done <- f.serveSelectedTransport(runCtx) }(owner)
	}
	for !old[0].replication.manager.PeerConnections()[fixture.configs[1].Replication.NodeID] {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	for i, name := range []string{"startup-empty", "startup-pending", "startup-started"} {
		request, err := old[0].business.accounting.BeginRequestDetailed(ctx, "prj_startup", "key_startup", name, "model")
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			continue
		}
		attempt, err := old[0].business.accounting.ReserveAttempt(ctx, request, 100000, 10)
		if err != nil {
			t.Fatal(err)
		}
		if i == 2 {
			if err := old[0].business.accounting.MarkStarted(ctx, attempt); err != nil {
				t.Fatal(err)
			}
		}
	}
	input, _, err := old[0].reopened.Owners.Components.Ledger.ReadPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	target := old[0].replication.publisher.Snapshot().AppliedIndex
	for old[1].replication.publisher.Snapshot().AppliedIndex < target {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	stop()
	for range 2 {
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	for _, owner := range old {
		if err := owner.Close(); err != nil {
			t.Fatal(err)
		}
	}
	var fresh [2]*haSelectedRuntimeFoundation
	for i, cfg := range fixture.configs {
		fresh[i], err = openHASelectedRuntimeFoundation(ctx, cfg, fixture.keys.Master, old[i].owners.Components.Guard, functionalHAMetadataManager(t, cfg), logger)
		if err != nil {
			t.Fatal("actual pending saved source reopen", i, err)
		}
		t.Cleanup(func() {
			if err := fresh[i].Close(); err != nil {
				t.Error(err)
			}
		})
		if err := fresh[i].prepareSelectedServicesWithLease(ctx, i == 0); err != nil {
			t.Fatal("actual pending services", i, err)
		}
		if err := fresh[i].installHASelectedGovernanceService(ctx); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err := fresh[i].prepareHASelectedPrimaryStartup(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if err := fresh[i].prepareSelectedTransport(logger); err != nil {
			t.Fatal(err)
		}
	}
	primary := fresh[0]
	if primary.replication.startupReady.Load() {
		t.Fatal("saved confirmed baseline manufactured Ready")
	}
	select {
	case <-primary.replication.startupProducers:
		t.Fatal("producers admitted before peer adjudication")
	default:
	}
	paused, resume := make(chan struct{}), make(chan struct{})
	primary.applyPhase = func(phase string) {
		if phase == "startup-accounting-recovered" {
			close(paused)
			select {
			case <-resume:
			case <-ctx.Done():
			}
		}
	}
	released := false
	defer func() {
		if !released {
			close(resume)
		}
	}()
	accounting, governance, collector := primary.business.accounting, primary.business.governance, primary.business.collector
	runCtx, stop = context.WithCancel(ctx)
	defer stop()
	for _, owner := range fresh {
		go func(f *haSelectedRuntimeFoundation) { done <- f.serveSelectedTransport(runCtx) }(owner)
	}
	defer func() {
		if !released {
			close(resume)
			released = true
		}
		stop()
		for range 2 {
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		}
	}()
	select {
	case <-paused:
	case <-ctx.Done():
		t.Fatal("actual startup recovery did not finish", ctx.Err())
	}
	if primary.replication.startupReady.Load() {
		t.Fatal("accounting completion bypassed actual seven-root validation")
	}
	close(resume)
	released = true
	for !primary.replication.startupReady.Load() {
		select {
		case err := <-primary.replication.fatal:
			t.Fatal("actual startup failed", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	select {
	case <-primary.replication.startupProducers:
	default:
		t.Fatal("real completion did not admit producers")
	}
	if primary.business.accounting != accounting || primary.business.governance != governance || primary.business.collector != collector {
		t.Fatal("startup replaced a live Runtime service handle")
	}
	if _, err := primary.owners.Components.Lease.ValidateOwner(primary.owners.Components.NativeLedger); err == nil {
		t.Fatal("Ready kept actual startup maintenance installed")
	}
	head, _, err := primary.reopened.Owners.Components.Ledger.ReadPosition(ctx)
	if err != nil || head.Sequence != input.Sequence+5 {
		t.Fatal("Ready did not cover two-level recovery", head, err)
	}
	if err := collector.CatchUp(ctx); err != nil {
		t.Fatal("actual post-Ready collector", err)
	}
	for fresh[1].replication.publisher.Snapshot().AppliedIndex < primary.replication.publisher.Snapshot().AppliedIndex {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	for _, owner := range fresh {
		for _, name := range []string{"startup-empty", "startup-pending", "startup-started"} {
			record, found, err := owner.reopened.Owners.Components.Ledger.ReadFinalization(ctx, name)
			if err != nil || !found || record.Event.Kind != ledger.EventRequestFinalized {
				t.Fatal("Ready recovery not transferable", name, found, err)
			}
		}
	}
}
