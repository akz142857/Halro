package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/store/lock"
)

func TestHASelectedPromotionRejectsForeignAuthenticatedAuditOrphan(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fixture, owners, _ := prepareHASelectedTransportFixture(t, ctx)
	before := owners[1].replication.publisher.Snapshot()
	for _, f := range owners {
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if err := f.owners.Components.Guard.Close(); err != nil {
			t.Fatal(err)
		}
	}
	cfg := fixture.configs[1]
	guard, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	options := PromoteMemberOptions{ExpectedTerm: before.Term, ExpectedAppliedIndex: before.AppliedIndex, OldPrimaryNodeID: fixture.configs[0].Replication.NodeID, FencedBy: replication.FenceNodeIsolated, NoPeerPromise: true, Username: "admin", Password: []byte("correct horse battery staple")}
	interrupted := errors.New("stop after actual leadership")
	_, err = promoteHASelectedMember(controlledHAPromotionContext(t, ctx, cfg), cfg, guard, options, func(phase string) error {
		if phase == "leadership" {
			return interrupted
		}
		return nil
	})
	if !errors.Is(err, interrupted) {
		t.Fatal("did not reach real leadership", err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	sealPath := filepath.Join(cfg.Storage.DataDir, "active.seal")
	sealBefore, err := os.ReadFile(sealPath)
	if err != nil {
		t.Fatal(err)
	}
	// A fully MACed native suffix still is not this signed transition's Audit.
	foreign, err := audit.OpenWithOptions(cfg.AuditPath(), fixture.keys.Audit, audit.Options{RequireExisting: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := foreign.Append(ctx, audit.Event{EventID: "foreign-promotion-orphan", OccurredAt: time.Now().UTC(), ActorType: "admin", ActorID: "admin", Action: "cluster.promote", TargetType: "cluster_member", TargetID: before.NodeID, Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	if err := foreign.Close(); err != nil {
		t.Fatal(err)
	}
	nativeBefore, err := os.ReadFile(cfg.AuditPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PromoteMember(controlledHAPromotionContext(t, ctx, cfg), cfg, options); err == nil {
		t.Fatal("foreign authenticated Audit orphan became ordering/promotion authority")
	}
	sealAfter, err := os.ReadFile(sealPath)
	if err != nil || string(sealAfter) != string(sealBefore) {
		t.Fatal("refused Audit changed common selection", err)
	}
	nativeAfter, err := os.ReadFile(cfg.AuditPath())
	if err != nil || string(nativeAfter) != string(nativeBefore) {
		t.Fatal("refused Audit was repaired or deleted", err)
	}
	member, key, err := readMemberState(cfg, fixture.keys.Master)
	clear(key[:])
	if err != nil {
		t.Fatal(err)
	}
	if member.Term != before.Term+1 || member.DurableIndex != before.DurableIndex+1 || member.AppliedIndex != before.AppliedIndex {
		t.Fatal("refusal changed original confirmed prefix", member)
	}
}
