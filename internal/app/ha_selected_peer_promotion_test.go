package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
	"github.com/akz142857/Halro/internal/store/lock"
)

func TestHASelectedPeerPromisePersistsOwnSevenRootsBeforeResponse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fixture, owners, _ := prepareHASelectedTransportFixture(t, ctx)
	f := owners[0]
	before := f.replication.publisher.Snapshot()
	candidate := owners[1].replication.publisher.Snapshot()
	candidate.PromisedTerm++
	p := replication.PromotionProposal{Version: 1, ClusterID: candidate.ClusterID, Incarnation: candidate.Incarnation, CandidateNodeID: candidate.NodeID, ExpectedTerm: candidate.Term, Term: candidate.PromisedTerm, ExpectedAppliedIndex: candidate.AppliedIndex, OldPrimaryNodeID: before.NodeID, FencedBy: replication.FenceNodeIsolated, ActorID: "admin"}
	hello := promotionHello(candidate)
	hello.Protocol = replication.VersionRange{Current: 2, Minimum: 2, Maximum: 2}
	hello.Metadata = hello.Protocol
	hello.MetadataBaselineAuthority = f.owners.Baseline.Descriptor().AuthorityDigest
	encoded, err := p.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	foreign := hello
	foreign.NodeID = "foreign-node"
	if err := f.handleSelectedPromotionProposal(ctx, foreign, encoded, func([]byte) error { t.Fatal("foreign response"); return nil }); err == nil {
		t.Fatal("foreign authenticated candidate accepted")
	}
	if _, err := os.Lstat(haSelectedPeerPromotionPath(fixture.configs[0], false)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("foreign candidate created preparation", err)
	}
	lostResponse := errors.New("actual response was lost")
	var first replication.PromotionPromise
	err = f.handleSelectedPromotionProposal(ctx, hello, encoded, func(response []byte) error {
		var err error
		first, err = replication.UnmarshalPromotionPromise(response)
		if err != nil {
			t.Fatal(err)
		}
		prepared, err := readHASelectedPeerPromotionCarrier(ctx, haSelectedPeerPromotionPath(f.cfg, false), f.replication.clusterKey[:], f.owners.Components.Budget)
		if err != nil {
			t.Fatal("preparation absent before response", err)
		}
		promised, err := readHASelectedPeerPromotionCarrier(ctx, haSelectedPeerPromotionPath(f.cfg, true), f.replication.clusterKey[:], f.owners.Components.Budget)
		if err != nil || promised.Claims().Promise == nil || *promised.Claims().Promise != first || first.PeerPreparationDigest != prepared.Digest() || prepared.Claims().Seal.Role != authenticatedstate.SealPrimary || prepared.Claims().Before.Role != replication.RolePrimary || prepared.Claims().Seal.RootCount != 7 {
			t.Fatal("peer authority substituted candidate/Replica roots", err)
		}
		return lostResponse
	})
	if !errors.Is(err, lostResponse) {
		t.Fatal("actual selected producer", err)
	}
	after := f.replication.publisher.Snapshot()
	if after.Role != replication.RoleReplica || after.Term != before.Term || after.PromisedTerm != p.Term || after.AppliedIndex != before.AppliedIndex || after.OrderingHeadMAC != before.OrderingHeadMAC {
		t.Fatal("Promise mutated old ordering/term", after)
	}
	if err := statecut.WithHASelectedCut(ctx, f.owners, func(context.Context, statecut.HASelectedCut) error { return nil }); err == nil {
		t.Fatal("peer transition borrowed ordinary authority")
	}
	if _, err := f.reopened.Owners.Components.Usage.ReadStats(ctx); err == nil {
		t.Fatal("promised peer retained ordinary Usage query authority")
	}
	if _, _, err := f.reopened.Owners.Components.Ledger.ReadPosition(ctx); err == nil {
		t.Fatal("promised peer retained ordinary Ledger query authority")
	}
	var replay replication.PromotionPromise
	if err := f.handleSelectedPromotionProposal(ctx, hello, encoded, func(response []byte) error {
		var err error
		replay, err = replication.UnmarshalPromotionPromise(response)
		return err
	}); err != nil || replay != first {
		t.Fatal("same durable proposal lost-response replay", replay, err)
	}
	p.ActorID = "different-actor"
	conflict, _ := p.MarshalBinary()
	if err := f.handleSelectedPromotionProposal(ctx, hello, conflict, func([]byte) error { t.Fatal("conflict response"); return nil }); err == nil {
		t.Fatal("conflicting proposal replay accepted")
	}
	peerAuthority, err := readHASelectedPeerPromotionCarrier(ctx, haSelectedPeerPromotionPath(f.cfg, true), f.replication.clusterKey[:], f.owners.Components.Budget)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal("promised old owner actual join", err)
	}
	if err := f.owners.Components.Guard.Close(); err != nil {
		t.Fatal(err)
	}
	guard, err := lock.Acquire(f.cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	cold, err := openHASelectedRuntimeFoundationTransitionMode(ctx, f.cfg, fixture.keys.Master, guard, functionalHAMetadataManager(t, f.cfg), slog.New(slog.NewTextHandler(io.Discard, nil)), nil, peerAuthority)
	if cold != nil {
		defer cold.Close()
	}
	if err != nil {
		t.Fatal("fresh genuine promised peer cold foundation", err)
	}
	if err := cold.prepareSelectedRoots(ctx); err != nil {
		t.Fatal("fresh seven-root peer cold selection", err)
	}
	if cold.reopened.Seal != peerAuthority.Claims().Seal || cold.reopened.Seal.Role != authenticatedstate.SealPrimary || cold.replication.coordinator != nil || cold.metadataJournal == nil {
		t.Fatal("old Primary receipt was relabeled or acquired ordinary writer")
	}
	if err := cold.prepareSelectedServices(ctx); err == nil {
		t.Fatal("cold peer exposed ordinary services")
	}
	if _, err := cold.reopened.OpenHAMetadataStore(ctx); err == nil {
		t.Fatal("cold peer exposed ordinary Metadata DAO")
	}
	if err := cold.serveSelectedTransport(ctx); err == nil {
		t.Fatal("cold peer exposed ordinary transport")
	}
}

func TestHASelectedActualTLSPeerPromiseAndPublicPromotion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	fixture, owners, _ := prepareHASelectedTransportFixture(t, ctx)
	primary, candidate := owners[0], owners[1]
	before := candidate.replication.publisher.Snapshot()
	if err := candidate.Close(); err != nil {
		t.Fatal(err)
	}
	if err := candidate.owners.Components.Guard.Close(); err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- primary.serveSelectedTransport(runCtx) }()
	joined := false
	defer func() {
		stop()
		if !joined {
			<-done
		}
	}()
	cfg := fixture.configs[1]
	pressure, err := newResourcePressureWithSource(cfg, pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() }))
	if err != nil {
		t.Fatal(err)
	}
	commandCtx := context.WithValue(ctx, commandResourceScopeKey{}, &commandResourceScope{pressure: pressure, budget: *cfg.ResourceBudget, policy: *cfg.ResourcePressure})
	// The public command independently authenticates the operator and candidate
	// seven roots while holding its actual stopped-directory lock. The remote
	// response traverses the production TLS control handler installed above.
	promoted, err := PromoteMember(commandCtx, cfg, PromoteMemberOptions{ExpectedTerm: before.Term, ExpectedAppliedIndex: before.AppliedIndex, OldPrimaryNodeID: fixture.configs[0].Replication.NodeID, FencedBy: replication.FenceNodeIsolated, Username: "admin", Password: []byte("correct horse battery staple")})
	if err != nil {
		t.Fatal("actual TLS selected peer promise/public promotion", err)
	}
	if len(promoted.Promises) != 1 || promoted.Promises[0].PeerPreparationDigest == ([32]byte{}) || promoted.State.Role != replication.RolePrimary || promoted.State.Term != before.Term+1 || promoted.State.DurableIndex != before.DurableIndex+2 || promoted.State.ConfirmedIndex != before.ConfirmedIndex {
		t.Fatal("actual promotion lacked genuine own-peer receipt or continuous ordering", promoted)
	}
	select {
	case err := <-primary.replication.fatalErrors():
		if err == nil || primary.replication.publisher.Snapshot().PromisedTerm != promoted.State.Term {
			t.Fatal("old Primary did not publish irreversible term withdrawal", err)
		}
		// Runtime normally consumes this fatal event and cancels/join transport.
		stop()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal("actual old transport join", err)
		}
		joined = true
	case <-ctx.Done():
		t.Fatal("old Primary did not join after Promise", ctx.Err())
	}
	prepared, err := readHASelectedPeerPromotionCarrier(ctx, haSelectedPeerPromotionPath(primary.cfg, false), primary.replication.clusterKey[:], primary.owners.Components.Budget)
	if err != nil || prepared.Digest() != promoted.Promises[0].PeerPreparationDigest || prepared.Claims().Seal.Role != authenticatedstate.SealPrimary {
		t.Fatal("actual TLS peer did not persist its own old Primary roots", err)
	}
	if err := primary.Close(); err != nil {
		t.Fatal("actual old Primary join before stopped rejoin", err)
	}
	if err := primary.owners.Components.Guard.Close(); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	openOrdinary := func(index int) *haSelectedRuntimeFoundation {
		cfg := fixture.configs[index]
		guard, err := lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		f, err := openHASelectedRuntimeFoundation(ctx, cfg, fixture.keys.Master, guard, functionalHAMetadataManager(t, cfg), logger)
		if err != nil {
			guard.Close()
			t.Fatal("fresh post-promotion ordinary foundation", index, err)
		}
		t.Cleanup(func() {
			if err := f.Close(); err != nil {
				t.Error(err)
			}
			guard.Close()
		})
		if err := f.prepareSelectedServices(ctx); err != nil {
			t.Fatal("fresh role business factory", index, err)
		}
		if err := f.prepareSelectedTransport(logger); err != nil {
			t.Fatal("fresh role TLS factory", index, err)
		}
		return f
	}
	newPrimary := openOrdinary(1)
	newCtx, newStop := context.WithCancel(ctx)
	defer newStop()
	newDone := make(chan error, 2)
	go func() { newDone <- newPrimary.serveSelectedTransport(newCtx) }()
	newCount := 1
	t.Cleanup(func() {
		newStop()
		for range newCount {
			if err := <-newDone; err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		}
	})
	oldcfg := fixture.configs[0]
	oldPressure, err := newResourcePressureWithSource(oldcfg, pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() }))
	if err != nil {
		t.Fatal(err)
	}
	oldCtx := context.WithValue(ctx, commandResourceScopeKey{}, &commandResourceScope{pressure: oldPressure, budget: *oldcfg.ResourceBudget, policy: *oldcfg.ResourcePressure})
	rejoined, err := RejoinHASelectedMember(oldCtx, oldcfg)
	if err != nil {
		t.Fatal("actual TLS peer carrier/originalReceiver/Applier/new Replica common", err)
	}
	if rejoined.Role != replication.RoleReplica || rejoined.Term != promoted.State.Term || rejoined.DurableIndex != before.AppliedIndex+1 || rejoined.ConfirmedIndex != rejoined.DurableIndex || rejoined.AppliedIndex != rejoined.DurableIndex {
		t.Fatal("peer root opened before real Leadership applied", rejoined)
	}
	newReplica := openOrdinary(0)
	if newReplica.reopened.Seal.Role != authenticatedstate.SealReplica || newReplica.reopened.Seal.Term != promoted.State.Term || newReplica.reopened.Seal.OrderingIndex != rejoined.AppliedIndex {
		t.Fatal("new ordinary Replica borrowed old Primary seal")
	}
	go func() { newDone <- newReplica.serveSelectedTransport(newCtx) }()
	newCount++
	for {
		p, r := newPrimary.replication.publisher.Snapshot(), newReplica.replication.publisher.Snapshot()
		if p.ConfirmedIndex == p.DurableIndex && r.AppliedIndex == p.DurableIndex {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("real rejoined Replica did not confirm original promotion Audit", ctx.Err(), p, r)
		case <-time.After(10 * time.Millisecond):
		}
	}
	projects, err := newPrimary.metadataStore.ListProjects(ctx)
	if err != nil || len(projects) != 1 {
		t.Fatal("actual existing post-promotion project", err)
	}
	updated := projects[0]
	updated.Name = "new Primary actual TLS"
	project, err := newPrimary.metadataStore.PutProject(ctx, updated, updated.Revision, nil)
	if err != nil {
		t.Fatal("new Primary semantic caller", err)
	}
	for {
		got, e := newReplica.metadataStore.GetProject(ctx, project.ID)
		if e == nil && got.ID == project.ID && got.Name == project.Name {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("rejoined Replica actual Metadata query", ctx.Err(), e)
		case <-time.After(10 * time.Millisecond):
		}
	}
	newStop()
	for range newCount {
		if err := <-newDone; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	newCount = 0
	// Fresh default Runtime must discover the durable transition itself. The
	// fixture helper checks actual Ready only after real peer confirmation,
	// Pending recovery, Gateway accounting and replicated business completion.
	fixture.configs[0], fixture.configs[1] = fixture.configs[1], fixture.configs[0]
	proveDefaultHASelectedRuntime(t, ctx, fixture, [2]*haSelectedRuntimeFoundation{newPrimary, newReplica})
}
