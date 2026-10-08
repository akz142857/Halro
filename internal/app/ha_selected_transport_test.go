package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
)

func TestHASelectedActualTLSMetadataLedgerUsageAndAppliedReadGate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	fixture, owners, metadata := prepareHASelectedTransportFixture(t, ctx)
	stagedRefusal := make(chan error, 2)
	owners[1].applyPhase = func(phase string) {
		if phase != "ledger-applied" {
			return
		}
		member := owners[1].replication.publisher.Snapshot()
		_, _, err := owners[1].reopened.Owners.Components.Ledger.ReadPosition(context.Background())
		if member.Projection.Index != member.AppliedIndex {
			err = errors.Join(err, errors.New("Member projection no longer matches applied"))
		}
		stagedRefusal <- err
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 2)
	joined := false
	for _, owner := range owners {
		go func(f *haSelectedRuntimeFoundation) { done <- f.serveSelectedTransport(runCtx) }(owner)
	}
	t.Cleanup(func() {
		stop()
		if !joined {
			for range 2 {
				if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
					t.Error(err)
				}
			}
		}
	})
	for !owners[0].replication.manager.PeerConnections()[fixture.configs[1].Replication.NodeID] {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	project, err := metadata[0].PutProject(ctx, domain.Project{ID: "actual-selected-ha-project", Name: "Actual selected TLS", Enabled: true}, 0, nil)
	if err != nil {
		t.Fatal("actual selected Metadata confirmation", err)
	}
	if _, err := metadata[1].PutProject(ctx, domain.Project{ID: "replica-forbidden", Name: "Forbidden"}, 0, nil); err == nil {
		t.Fatal("actual selected Replica accepted semantic Metadata write")
	}
	// This first apply contains no new Ledger fact, so old applied readers remain
	// valid. The later semantic frame must refuse its staged root until Member.
	if err := <-stagedRefusal; err != nil {
		t.Fatal("unchanged applied Ledger unnecessarily refused", err)
	}
	c := owners[0].reopened.Owners.Components
	now := time.Now().UTC()
	event := ledger.Event{EventID: "actual-ha-unit-create", Kind: ledger.EventWorkUnitCreated, WorkUnitID: "wku_actualha", ProjectID: project.ID, KeyID: "key_actual_ha", OccurredAt: now, PeriodID: now.Format("2006-01-02"), PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, PeriodStartMicros: now.Truncate(24 * time.Hour).UnixMicro(), PeriodEndMicros: now.Truncate(24 * time.Hour).Add(24 * time.Hour).UnixMicro(), Operation: "workunit.create", IdempotencyKeyHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RequestFingerprint: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	record, receipt, err := c.NativeLedger.AppendApplyRecordWithReceipt(ctx, event, func(ctx context.Context, r ledger.Record) error {
		_, err := c.Ledger.AdvanceOneFromNative(ctx, c.NativeLedger, ledger.ProjectionPrefix{Head: ledger.Watermark{Generation: r.Generation, Sequence: r.Sequence, Offset: r.Offset}, Hash: r.Hash})
		return err
	})
	if err != nil {
		t.Fatal("actual selected native ordering Apply confirmation", err)
	}
	if receipt.ReplicationIndex == 0 {
		t.Fatal("actual durable ordering receipt is absent")
	}
	if err := owners[0].replication.coordinator.WaitConfirmed(ctx, receipt.ReplicationIndex); err != nil {
		t.Fatal("actual native receipt confirmation", err)
	}
	if err := <-stagedRefusal; !errors.Is(err, ledger.ErrProjectionSource) {
		t.Fatal("ordinary Replica read observed staged confirmed root", err)
	}
	if _, err := owners[0].reopened.Derivative.CatchUpTypedLive(ctx, c.NativeLedger, c.Budget); err != nil {
		t.Fatal("actual Primary live Usage", err)
	}
	through := receipt.ReplicationIndex
	for owners[1].replication.publisher.Snapshot().AppliedIndex < through {
		select {
		case <-ctx.Done():
			t.Fatal("actual seven-root Replica application", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	for i, owner := range owners {
		p := owner.reopened.Owners.Components
		got, err := metadata[i].GetProject(ctx, project.ID)
		if err != nil || got.Name != project.Name {
			t.Fatal("actual selected DAO read", i, got, err)
		}
		head, hash, err := p.Ledger.ReadPosition(ctx)
		if err != nil || head.Sequence != record.Sequence || hash != record.Hash {
			t.Fatal("actual selected permitted Ledger", i, head, err)
		}
		ref, err := p.Usage.TypedSealReference(ctx)
		if err != nil || ref.Ledger.Anchor.Head != head || ref.Ledger.Anchor.Hash != hash {
			t.Fatal("actual selected Usage applied source", i, err)
		}
		if _, err := p.Usage.ReadStats(ctx); err != nil {
			t.Fatal("actual selected live Usage query", i, err)
		}
		if owner.replication.startupReady.Load() {
			t.Fatal("transport-only slice manufactured default Runtime Ready")
		}
		if s := owner.replication.publisher.Snapshot(); s.Projection.Index != s.AppliedIndex || s.Role != ([]replication.Role{replication.RolePrimary, replication.RoleReplica})[i] {
			t.Fatal("actual role/application progress", s)
		}
	}
	// Four real DAO callers share the actual Member/Ordering/common publisher.
	// Contention owns bounded wait credits rather than reporting lost authority.
	reads := make(chan error, 4)
	for range 4 {
		go func() {
			for range 16 {
				got, err := metadata[1].GetProject(ctx, project.ID)
				if got.Name != project.Name {
					err = errors.Join(err, errors.New("actual concurrent Replica DAO value differs"))
				}
				if err != nil {
					reads <- err
					return
				}
			}
			reads <- nil
		}()
	}
	for range 4 {
		if err := <-reads; err != nil {
			t.Fatal("actual concurrent Member-qualified DAO", err)
		}
	}
	stop()
	for range 2 {
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	joined = true
	for _, owner := range owners {
		if err := owner.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for i, cfg := range fixture.configs {
		checkpoint, err := CheckpointHASelectedRuntime(ctx, cfg, fixture.keys.Master, owners[i].owners.Components.Guard, functionalHAMetadataManager(t, cfg), slog.New(slog.NewTextHandler(io.Discard, nil)))
		if checkpoint != nil {
			defer func() {
				if err := checkpoint.Close(); err != nil {
					t.Error(err)
				}
			}()
		}
		if err != nil {
			t.Fatal("actual offline seven-root publication", i, err)
		}
		if checkpoint.Publication.Seal.RootCount != 7 || checkpoint.Publication.Seal.Revision != 2 || checkpoint.Publication.Seal.OrderingIndex != receipt.ReplicationIndex {
			t.Fatal("actual seven-root successor did not cover receipt", checkpoint.Publication.Seal)
		}
		if err := checkpoint.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// A fresh actual selected owner must replay the authenticated applied
	// Metadata suffix before issuing its DAO; merely opening the old common
	// Metadata root would silently omit the confirmed project after restart.
	cfg := fixture.configs[1]
	fresh, err := openHASelectedRuntimeFoundation(ctx, cfg, fixture.keys.Master, owners[1].owners.Components.Guard, functionalHAMetadataManager(t, cfg), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal("actual applied metadata source reopen", err)
	}
	defer func() {
		if err := fresh.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := fresh.prepareSelectedServices(ctx); err != nil {
		t.Fatal("actual Metadata suffix before DAO", err)
	}
	got, err := fresh.metadataStore.GetProject(ctx, project.ID)
	if err != nil || got.Name != project.Name {
		t.Fatal("fresh applied Metadata DAO lost native facts", got, err)
	}
}

func prepareHASelectedTransportFixture(t *testing.T, ctx context.Context) (haMetadataMaintenanceFixture, [2]*haSelectedRuntimeFoundation, [2]*boltstore.Store) {
	t.Helper()
	fixture := prepareHAMetadataMaintenanceFixture(t)
	return prepareHASelectedTransportFixtureFromMetadataFixture(t, ctx, fixture)
}

func prepareHASelectedTransportFixtureFromMetadataFixture(t *testing.T, ctx context.Context, fixture haMetadataMaintenanceFixture) (haMetadataMaintenanceFixture, [2]*haSelectedRuntimeFoundation, [2]*boltstore.Store) {
	t.Helper()
	prepareHAMetadataMaintenanceArtifacts(t, fixture)
	completeHAMemoryActivationBoundary(t, fixture)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, cfg := range fixture.configs {
		guard, err := lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		activation, err := ActivateHAMemoryRuntime(ctx, cfg, fixture.keys, guard, functionalHAMetadataManager(t, cfg), logger)
		if err != nil {
			t.Fatal(err)
		}
		if err := activation.Close(); err != nil {
			t.Fatal(err)
		}
		if err := guard.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(cfg.MetadataPath(), cfg.MetadataPath()+".preserved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(cfg.MetadataPath(), 0700); err != nil {
			t.Fatal(err)
		}
	}
	var owners [2]*haSelectedRuntimeFoundation
	var guards [2]*lock.Lock
	var metadata [2]*boltstore.Store
	for i, cfg := range fixture.configs {
		var err error
		guards[i], err = lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		owners[i], err = openHASelectedRuntimeFoundation(ctx, cfg, fixture.keys.Master, guards[i], functionalHAMetadataManager(t, cfg), logger)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := owners[i].Close(); err != nil {
				t.Error(err)
			}
			if err := guards[i].Close(); err != nil {
				t.Error(err)
			}
		})
		if err := owners[i].prepareSelectedServices(ctx); err != nil {
			t.Fatal("actual selected services", i, err)
		}
		metadata[i] = owners[i].metadataStore
		if err := owners[i].prepareSelectedTransport(logger); err != nil {
			t.Fatal("actual selected transport", i, err)
		}
	}
	return fixture, owners, metadata
}

func TestHASelectedActualTLSPartialLedgerReopensConfirmedPrefixBeforeServices(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fixture, owners, _ := prepareHASelectedTransportFixture(t, ctx)
	staged := make(chan error, 1)
	owners[1].applyPhase = func(phase string) {
		if phase != "ledger-applied" {
			return
		}
		_, _, err := owners[1].reopened.Owners.Components.Ledger.ReadPosition(ctx)
		staged <- err
		owners[1].cancel() // Withdraw the real owner before any Member applied publication.
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 2)
	for _, owner := range owners {
		go func(f *haSelectedRuntimeFoundation) { done <- f.serveSelectedTransport(runCtx) }(owner)
	}
	joined := false
	defer func() {
		stop()
		if !joined {
			for range 2 {
				<-done
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
	c := owners[0].reopened.Owners.Components
	now := time.Now().UTC()
	event := ledger.Event{EventID: "partial-ha-unit-create", Kind: ledger.EventWorkUnitCreated, WorkUnitID: "wku_partialha", ProjectID: "project_partial", KeyID: "key_partial", OccurredAt: now, PeriodID: now.Format("2006-01-02"), PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, PeriodStartMicros: now.Truncate(24 * time.Hour).UnixMicro(), PeriodEndMicros: now.Truncate(24 * time.Hour).Add(24 * time.Hour).UnixMicro(), Operation: "workunit.create", IdempotencyKeyHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RequestFingerprint: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	record, receipt, err := c.NativeLedger.AppendApplyRecordWithReceipt(ctx, event, func(ctx context.Context, r ledger.Record) error {
		_, err := c.Ledger.AdvanceOneFromNative(ctx, c.NativeLedger, ledger.ProjectionPrefix{Head: ledger.Watermark{Generation: r.Generation, Sequence: r.Sequence, Offset: r.Offset}, Hash: r.Hash})
		return err
	})
	if err != nil {
		t.Fatal("actual confirmed native fact", err)
	}
	if receipt.ReplicationIndex == 0 {
		t.Fatal("actual durable ordering receipt is absent")
	}
	// This case intentionally disconnects before a Primary ACK can return.
	// Replica's actual Receive persisted the two durable frame votes already;
	// fresh apply below proves its own confirmed prefix, not Primary completion.
	select {
	case err := <-staged:
		if !errors.Is(err, ledger.ErrProjectionSource) {
			t.Fatal("staged ordinary root admitted", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	stop()
	for range 2 {
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal("actual transport join", err)
		}
	}
	joined = true
	member := owners[1].replication.publisher.Snapshot()
	if member.AppliedIndex >= member.ConfirmedIndex || member.ConfirmedIndex != member.DurableIndex || member.Projection.Index != member.AppliedIndex {
		t.Fatal("partial callback fabricated applied progress", member)
	}
	for _, owner := range owners {
		if err := owner.Close(); err != nil {
			t.Fatal("partial actual owner Close", err)
		}
	}
	cfg := fixture.configs[1]
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	fresh, err := openHASelectedRuntimeFoundation(ctx, cfg, fixture.keys.Master, owners[1].owners.Components.Guard, functionalHAMetadataManager(t, cfg), logger)
	if err != nil {
		t.Fatal("received native tail cannot grant applied selected roots", err)
	}
	defer func() {
		if err := fresh.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := fresh.prepareSelectedServices(ctx); err != nil {
		t.Fatal("actual applied prefix service reopen", err)
	}
	p := fresh.reopened.Owners.Components
	head, _, err := p.Ledger.ReadPosition(ctx)
	if err != nil || head.Sequence != record.Sequence {
		t.Fatal("original startup applier did not restore actual confirmed root", head, err)
	}
	if actual := fresh.replication.publisher.Snapshot(); actual.AppliedIndex != member.ConfirmedIndex || actual.Projection.Index != member.ConfirmedIndex {
		t.Fatal("ordinary service opened before whole confirmed publication", actual)
	}
	if _, err := p.Usage.ReadStats(ctx); err != nil {
		t.Fatal("old actual applied Usage unreadable", err)
	}
	if err := fresh.prepareSelectedTransport(logger); err != nil {
		t.Fatal("actual pending receiver recovery", err)
	}
	through, err := fresh.replication.applier.ApplyConfirmed(ctx)
	if err != nil || through != member.ConfirmedIndex {
		t.Fatal("actual confirmed recovery", through, err)
	}
	head, hash, err := p.Ledger.ReadPosition(ctx)
	if err != nil || head.Sequence != record.Sequence || hash != record.Hash {
		t.Fatal("resumed actual applied Ledger", head, err)
	}
	ref, err := p.Usage.TypedSealReference(ctx)
	if err != nil || ref.Ledger.Anchor.Head != head || ref.Ledger.Anchor.Hash != hash {
		t.Fatal("resumed actual applied Usage", err)
	}
	if actual := fresh.replication.publisher.Snapshot(); actual.AppliedIndex != through || actual.Projection.Index != through {
		t.Fatal("recovered actual Member publication", actual)
	}
}
