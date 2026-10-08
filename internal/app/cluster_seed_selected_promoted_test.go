package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// This fixture keeps the original peer alive through genuine promotion control,
// then uses the actual rejoin command/Receiver to establish the new term. No
// Member indexes, role, bootstrap or common candidate are edited by the test.
func selectedSeedPromotedPair(t *testing.T, ctx context.Context) (haMetadataMaintenanceFixture, [2]*haSelectedRuntimeFoundation) {
	t.Helper()
	fixture, owners, _ := prepareHASelectedTransportFixture(t, ctx)
	oldPrimary, candidate := owners[0], owners[1]
	before := candidate.replication.publisher.Snapshot()
	if err := errors.Join(candidate.Close(), candidate.owners.Components.Guard.Close()); err != nil {
		t.Fatal(err)
	}

	if err := errors.Join(oldPrimary.Close(), oldPrimary.owners.Components.Guard.Close()); err != nil {
		t.Fatal(err)
	}
	original := fixture.configs[0]
	seedCfg := fixture.configs[1]
	seedCfg.Storage.DataDir = filepath.Join(t.TempDir(), "first-seeded-replica")
	firstManifest := filepath.Join(t.TempDir(), "first-seed.json")
	if _, err := CreateSeedManifest(selectedSeedCommandTestContext(t, ctx, original), original, "halro-1", firstManifest, "admin", []byte("correct horse battery staple"), ""); err != nil {
		t.Fatal("first actual Seed approval", err)
	}
	stage := seedCfg.Storage.DataDir + ".stage"
	copyTestTree(t, original.Storage.DataDir, stage)
	if _, err := InstallSeedSnapshot(selectedSeedCommandTestContext(t, ctx, seedCfg), seedCfg, stage, firstManifest); err != nil {
		t.Fatal("first actual cold Replica Seed", err)
	}
	fixture.configs[1] = seedCfg
	sourceGuard, err := lock.Acquire(original.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	oldPrimary, err = openHASelectedRuntimeFoundation(ctx, original, fixture.keys.Master, sourceGuard, functionalHAMetadataManager(t, original), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := errors.Join(oldPrimary.Close(), sourceGuard.Close()); err != nil {
			t.Error(err)
		}
	})
	if err = oldPrimary.prepareSelectedServices(ctx); err != nil {
		t.Fatal(err)
	}
	if err = oldPrimary.prepareSelectedTransport(slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	oldCtx, oldStop := context.WithCancel(ctx)
	oldDone := make(chan error, 1)
	go func() { oldDone <- oldPrimary.serveSelectedTransport(oldCtx) }()
	oldJoined := false
	defer func() {
		oldStop()
		if !oldJoined {
			<-oldDone
		}
	}()
	candidateCfg := fixture.configs[1]
	pressure, err := newResourcePressureWithSource(candidateCfg, pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() }))
	if err != nil {
		t.Fatal(err)
	}
	commandCtx := context.WithValue(ctx, commandResourceScopeKey{}, &commandResourceScope{pressure: pressure, budget: *candidateCfg.ResourceBudget, policy: *candidateCfg.ResourcePressure})
	promoted, err := PromoteMember(commandCtx, candidateCfg, PromoteMemberOptions{ExpectedTerm: before.Term, ExpectedAppliedIndex: before.AppliedIndex, OldPrimaryNodeID: fixture.configs[0].Replication.NodeID, FencedBy: replication.FenceNodeIsolated, Username: "admin", Password: []byte("correct horse battery staple")})
	if err != nil {
		t.Fatal("genuine TLS promotion", err)
	}
	if len(promoted.Promises) != 1 {
		t.Fatal("promotion did not consume original peer's own proof")
	}
	select {
	case <-oldPrimary.replication.fatalErrors():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	oldStop()
	if err = <-oldDone; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	oldJoined = true
	if err = errors.Join(oldPrimary.Close(), oldPrimary.owners.Components.Guard.Close()); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	open := func(cfg config.Config) *haSelectedRuntimeFoundation {
		guard, err := lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		f, err := openHASelectedRuntimeFoundation(ctx, cfg, fixture.keys.Master, guard, functionalHAMetadataManager(t, cfg), logger)
		if err != nil {
			guard.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := errors.Join(f.Close(), guard.Close()); err != nil {
				t.Error(err)
			}
		})
		if err = f.prepareSelectedServices(ctx); err != nil {
			t.Fatal(err)
		}
		if err = f.prepareSelectedTransport(logger); err != nil {
			t.Fatal(err)
		}
		return f
	}
	newPrimary := open(candidateCfg)
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 2)
	go func() { done <- newPrimary.serveSelectedTransport(runCtx) }()
	count := 1
	defer func() {
		stop()
		for range count {
			if e := <-done; e != nil && !errors.Is(e, context.Canceled) {
				t.Error(e)
			}
		}
	}()
	replicaCfg := fixture.configs[0]
	p, err := newResourcePressureWithSource(replicaCfg, pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() }))
	if err != nil {
		t.Fatal(err)
	}
	rejoinCtx := context.WithValue(ctx, commandResourceScopeKey{}, &commandResourceScope{pressure: p, budget: *replicaCfg.ResourceBudget, policy: *replicaCfg.ResourcePressure})
	if _, err = RejoinHASelectedMember(rejoinCtx, replicaCfg); err != nil {
		t.Fatal("actual peer rejoin into new term", err)
	}
	newReplica := open(replicaCfg)
	go func() { done <- newReplica.serveSelectedTransport(runCtx) }()
	count++
	wait := func() {
		for {
			primary, replica := newPrimary.replication.publisher.Snapshot(), newReplica.replication.publisher.Snapshot()
			if primary.DurableIndex == primary.ConfirmedIndex && primary.DurableIndex == primary.AppliedIndex && replica.AppliedIndex == primary.DurableIndex {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal("independent peer completion", ctx.Err(), primary, replica)
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	wait()
	projects, err := newPrimary.metadataStore.ListProjects(ctx)
	if err != nil || len(projects) != 1 {
		t.Fatal(err)
	}
	project := projects[0]
	project.Name = "Seed actual promoted Primary"
	if _, err = newPrimary.metadataStore.PutProject(ctx, project, project.Revision, nil); err != nil {
		t.Fatal("actual new-term business mutation", err)
	}
	wait()
	stop()
	for range count {
		if e := <-done; e != nil && !errors.Is(e, context.Canceled) {
			t.Fatal(e)
		}
	}
	count = 0
	return fixture, [2]*haSelectedRuntimeFoundation{newPrimary, newReplica}
}

func TestSelectedSeedFulfilledPromotionAndOpaqueReseedUseIndependentNewReplicaRoots(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	fixture, owners := selectedSeedPromotedPair(t, ctx)
	for _, owner := range owners {
		if err := errors.Join(owner.Close(), owner.owners.Components.Guard.Close()); err != nil {
			t.Fatal(err)
		}
	}
	source := fixture.configs[1]
	manifestPath := filepath.Join(t.TempDir(), "promoted-seed.json")
	var originalReceived [5][]byte
	for i, name := range selectedSeedReceivedControls {
		path := filepath.Join(source.Storage.DataDir, name)
		if i == 0 {
			// The fulfilled promotion legitimately retired this node's old
			// receiver bank. Preserve a real bank from the independent peer as
			// opaque source data; even its matching target Node cannot authorize
			// a new target Ordering birth.
			raw, err := os.ReadFile(filepath.Join(fixture.configs[0].Storage.DataDir, name))
			if err != nil {
				t.Fatal("genuine independent Replica receipt", err)
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.WriteFile(path, []byte(fmt.Sprintf("opaque original receiver candidate %d", i)), 0600); err != nil {
				t.Fatal(err)
			}
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal("actual surviving source receiver receipt", err)
		}
		originalReceived[i] = raw
	}

	target := fixture.configs[0]
	target.Storage.DataDir = filepath.Join(t.TempDir(), "replica")
	// Older archives may be opaque. They are included in the independently
	// approved source tree but never read for current root/member qualification.
	for n, suffix := range selectedSeedArchiveSuffixes[1:3] {
		root := haMemoryStateDirectory(source) + suffix
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "opaque"), []byte{byte(n)}, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// The source must be freshly approved after any extra retained bytes exist.
	var manifest SeedManifest
	var err error
	for approval := 0; approval < 7; approval++ {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("promoted-approval-%d.json", approval))
		manifest, err = CreateSeedManifest(selectedSeedCommandTestContext(t, ctx, source), source, "halro-0", path, "admin", []byte("correct horse battery staple"), "")
		if err != nil {
			t.Fatal("repeated actual reseed source approval", approval, err)
		}
		manifestPath = path
	}
	if manifest.Term <= 1 || manifest.Selected.SourceSeal.Role != authenticatedstate.SealPrimary {
		t.Fatal("term/role did not come from genuine fulfilled source")
	}
	stage := target.Storage.DataDir + ".reseed"
	copyTestTree(t, source.Storage.DataDir, stage)
	if _, err = InstallSeedSnapshot(selectedSeedCommandTestContext(t, ctx, target), target, stage, manifestPath); err != nil {
		t.Fatal("genuine promoted cold target with three opaque archives", err)
	}

	originalArchive, err := os.ReadFile(filepath.Join(haMemoryStateDirectory(source)+selectedSeedArchiveSuffixes[0], "ledger", "active.root"))
	if err != nil {
		t.Fatal(err)
	}
	copiedArchive, err := os.ReadFile(filepath.Join(haMemoryStateDirectory(target)+selectedSeedArchiveSuffixes[0], "ledger", "active.root"))
	if err != nil || !bytes.Equal(originalArchive, copiedArchive) {
		t.Fatal("previous actual Seed source root was changed", err)
	}
	for n, suffix := range selectedSeedArchiveSuffixes[1:3] {
		raw, err := os.ReadFile(filepath.Join(haMemoryStateDirectory(target)+suffix, "opaque"))
		if err != nil || len(raw) != 1 || raw[0] != byte(n) {
			t.Fatal("unknown archive was read/changed/deleted as ownership", n, err)
		}
	}
	if _, err = os.Stat(haMemoryStateDirectory(target) + selectedSeedArchiveSuffixes[3]); err != nil {
		t.Fatal("new original source did not use fourth exclusive slot", err)
	}
	for i, name := range selectedSeedReceivedControls {
		path := filepath.Join(haMemoryStateDirectory(target)+selectedSeedArchiveSuffixes[3], "seed-received-controls", filepath.Base(name))
		raw, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(raw, originalReceived[i]) {
			t.Fatal("source receiver bytes not preserved opaque", name, err)
		}
		if _, err := os.Lstat(filepath.Join(target.Storage.DataDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("source receiver granted target authority", name, err)
		}
	}
	guard, err := lock.Acquire(target.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	f, err := openHASelectedRuntimeFoundation(ctx, target, fixture.keys.Master, guard, functionalHAMetadataManager(t, target), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal("fresh target new-term authority", err)
	}
	if err = f.prepareSelectedServices(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.prepareSelectedTransport(slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal("fresh target real receiver preparation", err)
	}
	if err := f.replication.publisher.ValidatePortableReceivedPublication(ctx, f.replication.journal, f.owners.Components.Budget); err != nil {
		t.Fatal("target did not create its own qualified receiver receipt", err)
	}
	if f.reopened.Seal.Role != authenticatedstate.SealReplica || f.reopened.Seal.Term != manifest.Term || f.reopened.Seal.RootCount != 7 || f.reopened.Seal.OrderingIndex != manifest.Index {
		t.Fatal("seed relabeled an old primary candidate")
	}
	projects, err := f.metadataStore.ListProjects(ctx)
	if err != nil || len(projects) != 1 || projects[0].Name != "Seed actual promoted Primary" {
		t.Fatal("fresh target actual new-term business material", err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
}
