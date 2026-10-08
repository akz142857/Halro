package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

func TestHASelectedPromotionActualCommandContinuousOrderingAndFreshSevenRoots(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	fixture, owners, _ := prepareHASelectedTransportFixture(t, ctx)
	old := owners[1].replication.publisher.Snapshot()
	for _, f := range owners {
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if err := f.owners.Components.Guard.Close(); err != nil {
			t.Fatal(err)
		}
	}
	cfg := fixture.configs[1]
	pressure, err := newResourcePressureWithSource(cfg, pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() }))
	if err != nil {
		t.Fatal(err)
	}
	ctx = context.WithValue(ctx, commandResourceScopeKey{}, &commandResourceScope{pressure: pressure, budget: *cfg.ResourceBudget, policy: *cfg.ResourcePressure})
	opts := PromoteMemberOptions{ExpectedTerm: old.Term, ExpectedAppliedIndex: old.AppliedIndex, OldPrimaryNodeID: fixture.configs[0].Replication.NodeID, FencedBy: replication.FenceNodeIsolated, NoPeerPromise: true, Username: "admin", Password: []byte("wrong password")}
	if _, err := PromoteMember(ctx, cfg, opts); err == nil {
		t.Fatal("wrong selected administrator password accepted")
	}
	if _, err := os.Lstat(haSelectedPromotionPath(cfg, false)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed authentication wrote proposal", err)
	}
	opts.Password = []byte("correct horse battery staple")
	promoted, err := PromoteMember(ctx, cfg, opts)
	if err != nil {
		t.Fatal("actual selected public promotion", err)
	}
	if promoted.State.Term != old.Term+1 || promoted.State.PromisedTerm != old.Term+1 || promoted.State.Role != replication.RolePrimary || promoted.State.DurableIndex != old.DurableIndex+2 || promoted.State.ConfirmedIndex != old.ConfirmedIndex || promoted.State.AppliedIndex != old.AppliedIndex {
		t.Fatal("promotion changed ordering incarnation or confirmed prefix", promoted.State)
	}
	guard, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	f, err := openHASelectedRuntimeFoundation(ctx, cfg, fixture.keys.Master, guard, functionalHAMetadataManager(t, cfg), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if f != nil {
		defer f.Close()
	}
	if err != nil {
		t.Fatal("fresh independent new-term foundation", err)
	}
	if err = f.prepareSelectedRoots(ctx); err != nil {
		t.Fatal("fresh actual seven-root new-term reopen", err)
	}
	anchor, err := f.replication.journal.Record(old.AppliedIndex + 1)
	if err != nil || anchor.Kind != replication.KindLeadershipEstablished || anchor.Term != promoted.State.Term || anchor.PreviousMAC != old.OrderingHeadMAC {
		t.Fatal("new term lacks continuous actual leadership", anchor, err)
	}
	if f.reopened.Seal.Term != promoted.State.Term || f.reopened.Seal.OrderingIndex != anchor.Index+1 || f.reopened.Seal.RootCount != 7 {
		t.Fatal("new common cut was not independently selected", f.reopened.Seal)
	}
	auditRecord, err := f.replication.journal.Record(anchor.Index + 1)
	if err != nil || auditRecord.Store != replication.StoreAudit || auditRecord.StoreSequenceLast != auditRecord.StoreSequenceFirst || f.reopened.Seal.Positions[3].Sequence != auditRecord.StoreSequenceLast {
		t.Fatal("promotion Audit absent from actual ordering/common cut", auditRecord, err)
	}
}
