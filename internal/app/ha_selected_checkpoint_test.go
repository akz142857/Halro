package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/replication"
)

func TestHASelectedActualTLSAuditGovernanceCheckpointAndAppliedQueries(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fixture, owners, _ := prepareHASelectedTransportFixture(t, ctx)
	for i, owner := range owners {
		c := owner.reopened.Owners.Components
		if old, err := governance.NewTypedProjectedManager(ctx, c.NativeGovernance, c.Governance, c.Ledger, owner.metadataStore, c.Budget); err == nil || old != nil {
			t.Fatal("legacy Governance factory accepted selected HA", i, err)
		}
		if err := owner.installHASelectedGovernanceService(ctx); err != nil {
			t.Fatal("actual selected HA Governance service", i, err)
		}
		if ready, err := owner.business.governance.Ready(); err != nil || !ready {
			t.Fatal("actual HA Governance component query", i, ready, err)
		}
	}
	staged := make(chan error, 2)
	owners[1].applyPhase = func(phase string) {
		if phase != "governance-applied" || owners[1].owners.Components.NativeGovernance.Summary().Records == 0 {
			return
		}
		_, _, err := owners[1].reopened.Owners.Components.Governance.ReadOutcome(ctx, "out_actualha")
		staged <- err
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
	if _, _, err := owners[1].business.governance.Report(ctx, governance.ReportInput{}); !errors.Is(err, governance.ErrUnavailable) {
		t.Fatal("Replica read manager accepted semantic report", err)
	}
	c := owners[0].reopened.Owners.Components
	now := time.Now().UTC()
	auditRecord, err := c.Audit.Append(ctx, audit.Event{EventID: "audit-actual-ha-checkpoint", OccurredAt: now, ActorType: "admin", ActorID: "admin_actual", Action: "key.create", TargetType: "key", TargetID: "key_actualha", Outcome: "success"})
	if err != nil {
		t.Fatal("actual Primary Audit ordering", err)
	}
	event := governance.Event{EventID: "gov_actualha", ProjectID: "prj_actualha", WorkUnitID: "wku_actualha", DefinitionID: "odef_result", DefinitionVersion: 1, OutcomeID: "out_actualha", Value: "accepted", ReporterKeyID: "key_actualha", ObservedAt: now, IngestedAt: now, Revision: 1, IdempotencyKeyHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RequestFingerprint: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	gov, err := c.NativeGovernance.Append(ctx, event)
	if err != nil {
		t.Fatal("actual Primary Governance ordering", err)
	}
	index := owners[0].replication.publisher.Snapshot().DurableIndex
	actual, err := owners[0].replication.journal.Record(index)
	if err != nil || actual.Kind != replication.KindData || actual.Store != replication.StoreGovernance || actual.StoreSequenceLast != gov.Sequence {
		t.Fatal("actual Governance ordering receipt", actual, err)
	}
	if _, err := c.Governance.AdvanceAuthenticatedFromNative(ctx, c.NativeGovernance, governance.Summary{Records: gov.Sequence, Bytes: gov.Offset, LastHash: gov.Hash}, c.Budget); err != nil {
		t.Fatal("actual Primary Governance reducer", err)
	}
	if err := owners[0].replication.coordinator.WaitConfirmed(ctx, index); err != nil {
		t.Fatal(err)
	}
	if err := <-staged; !errors.Is(err, governance.ErrUnavailable) {
		t.Fatal("ordinary Replica read observed unapplied Governance root", err)
	}
	for owners[1].replication.publisher.Snapshot().AppliedIndex < index {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	for i, owner := range owners {
		got, found, err := owner.reopened.Owners.Components.Governance.ReadOutcome(ctx, event.OutcomeID)
		if err != nil || !found || got.Value != event.Value {
			t.Fatal("actual Applied Governance read", i, got, found, err)
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
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for i, cfg := range fixture.configs {
		checkpoint, err := CheckpointHASelectedRuntime(ctx, cfg, fixture.keys.Master, owners[i].owners.Components.Guard, functionalHAMetadataManager(t, cfg), logger)
		if checkpoint != nil {
			defer func() {
				if err := checkpoint.Close(); err != nil {
					t.Error(err)
				}
			}()
		}
		if err != nil {
			t.Fatal("actual Audit/Governance successor publication", i, err)
		}
		seal := checkpoint.Publication.Seal
		if seal.OrderingIndex != index || seal.Positions[2].Sequence != gov.Sequence || seal.Positions[3].Sequence != auditRecord.Sequence || seal.RootCount != 7 {
			t.Fatal("new capsules did not enter actual seven-root cut", seal.OrderingIndex, seal.Positions[2].Sequence, seal.Positions[3].Sequence, seal.RootCount)
		}
		if err := checkpoint.Close(); err != nil {
			t.Fatal(err)
		}
		fresh, err := openHASelectedRuntimeFoundation(ctx, cfg, fixture.keys.Master, owners[i].owners.Components.Guard, functionalHAMetadataManager(t, cfg), logger)
		if fresh != nil {
			defer func() {
				if err := fresh.Close(); err != nil {
					t.Error(err)
				}
			}()
		}
		if err != nil {
			t.Fatal("actual successor capsule selection", i, err)
		}
		if fresh.auditCache.Head().Records != auditRecord.Sequence || fresh.governanceCache.Head().Records != gov.Sequence {
			t.Fatal("old capsule supplied new cut", fresh.auditCache.Head(), fresh.governanceCache.Head())
		}
		if err := fresh.prepareSelectedServices(ctx); err != nil {
			t.Fatal("actual successor roots reopen", i, err)
		}
		got, found, err := fresh.reopened.Owners.Components.Governance.ReadOutcome(ctx, event.OutcomeID)
		if err != nil || !found || got.Value != event.Value {
			t.Fatal("actual successor Governance query", i, got, found, err)
		}
		if err := fresh.Close(); err != nil {
			t.Fatal(err)
		}
		// An opaque interrupted file is neither replaced nor a deletion grant.
		path := haSelectedCachePath(cfg.Storage.DataDir, 2, false)
		if err := os.WriteFile(path, []byte("opaque preserved candidate"), 0600); err != nil {
			t.Fatal(err)
		}
		for expected := uint64(3); expected <= 3; expected++ {
			cp, err := CheckpointHASelectedRuntime(ctx, cfg, fixture.keys.Master, owners[i].owners.Components.Guard, functionalHAMetadataManager(t, cfg), logger)
			if cp != nil {
				defer cp.Close()
			}
			if err != nil {
				t.Fatal("fixed fresh slot after opaque interruption", err)
			}
			if cp.Publication.Seal.Revision != expected {
				t.Fatal(cp.Publication.Seal)
			}
			if err := cp.Close(); err != nil {
				t.Fatal(err)
			}
		}
		// Repeated full NoDebt approvals with unchanged Audit/Governance
		// sources reuse the original immutable capsules. They still publish
		// genuine seven-root revisions and preserve the opaque candidate.
		for expected := uint64(4); expected <= 9; expected++ {
			cp, err := CheckpointHASelectedRuntime(ctx, cfg, fixture.keys.Master, owners[i].owners.Components.Guard, functionalHAMetadataManager(t, cfg), logger)
			if cp != nil {
				defer cp.Close()
			}
			if err != nil {
				t.Fatal("unchanged frozen capsules exhausted slots", err)
			}
			if cp.Publication.Seal.Revision != expected || !errors.Is(cp.caches.Audit.Discard(), audit.ErrStartupCache) || !errors.Is(cp.caches.Governance.Discard(), governance.ErrStartupCache) {
				t.Fatal("unchanged capsule did not publish genuine revision", cp.Publication.Seal)
			}
			if err := cp.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if raw, readErr := os.ReadFile(path); readErr != nil || string(raw) != "opaque preserved candidate" {
			t.Fatal("opaque file was discarded", readErr)
		}
	}
}
