//go:build linux

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
	"golang.org/x/sys/unix"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Actual same-owner online renewal with real TLS and full 32-frame Metadata,
// Ledger and Usage banks. Functional Manager evidence is not cfg.Validate or
// real OS pressure / public Default Runtime capacity evidence.
func TestHAPhysicalOnlineCheckpointKeepsActualServicesAndTLS(t *testing.T) {
	ctx, fixture, logger := prepareHAPhysicalOnlineFixture(t, 2*time.Minute)
	runHAPhysicalOnlineGeneration(t, ctx, fixture, logger, 0, nil, [2]uint64{}, false)
}

// Both roles run start, replicated requests, online checkpoints and the full
// terminal Close three times over the same data directories: the original
// generation and two restarts, each from a fresh Manager. Every earlier
// generation's facts must be served by the restarted Replica.
func TestHAPhysicalLifecycleSurvivesTwoRestarts(t *testing.T) {
	ctx, fixture, logger := prepareHAPhysicalOnlineFixture(t, 6*time.Minute)
	var written []string
	var revisions [2]uint64
	for generation := 0; generation < 3; generation++ {
		var ids []string
		ids, revisions = runHAPhysicalOnlineGeneration(t, ctx, fixture, logger, generation, written, revisions, false)
		written = append(written, ids...)
	}
}

// A start that fails after both roles opened their physical windows closes
// fail-closed: Close keeps every name, extent and fee for replay and reports
// ErrRecovery, and the process exits. The next process on the same
// directories must replay it, reclaim the held space and run two complete
// generations on the same fixed ext4 capacity.
func TestHAPhysicalFailedStartupIsReplayedByTheNextStart(t *testing.T) {
	ctx, fixture, logger := prepareHAPhysicalOnlineFixture(t, 6*time.Minute)
	raw, err := json.Marshal(haPhysicalFailedStartRequest{Configs: fixture.configs, Keys: fixture.keys})
	if err != nil {
		t.Fatal(err)
	}
	requestPath := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(requestPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHAPhysicalFailedStartupHelper$", "-test.count=1", "-test.v")
	command.Env = append(os.Environ(), "HALRO_HA_PHYSICAL_FAILED_START_REQUEST="+requestPath)
	output, err := command.CombinedOutput()
	if err != nil || !bytes.Contains(output, []byte("--- PASS: TestHAPhysicalFailedStartupHelper")) {
		t.Fatalf("failed-start process: %v\n%s", err, output)
	}
	held := haPhysicalFixtureSpace(t)
	t.Log("ext4 available after the failed start exited", held)
	var written []string
	var revisions [2]uint64
	var after [3]uint64
	for generation := 1; generation < 3; generation++ {
		var ids []string
		ids, revisions = runHAPhysicalOnlineGeneration(t, ctx, fixture, logger, generation, written, revisions, false)
		written = append(written, ids...)
		after[generation] = haPhysicalFixtureSpace(t)
	}
	if after[1] <= held {
		t.Fatal("next start did not reclaim the failed start's held space", held, after[1])
	}
	if diff := int64(after[1]) - int64(after[2]); diff < -(1<<20) || diff > 1<<20 {
		t.Fatal("restart after replay accumulated space", after[1], after[2])
	}
}

type haPhysicalFailedStartRequest struct {
	Configs [2]config.Config
	Keys    HAMetadataKeys
}

func TestHAPhysicalFailedStartupHelper(t *testing.T) {
	path := os.Getenv("HALRO_HA_PHYSICAL_FAILED_START_REQUEST")
	if path == "" {
		t.Skip("actual subprocess only")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var request haPhysicalFailedStartRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	fixture := haMetadataMaintenanceFixture{configs: request.Configs, keys: request.Keys}
	runHAPhysicalOnlineGeneration(t, ctx, fixture, logger, 0, nil, [2]uint64{}, true)
}

func prepareHAPhysicalOnlineFixture(t *testing.T, timeout time.Duration) (context.Context, haMetadataMaintenanceFixture, *slog.Logger) {
	t.Helper()
	root := os.Getenv("HALRO_EXT4_ROOT")
	if root == "" {
		t.Skip("requires isolated ext4 with original 32-frame bank capacity")
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(root, &fs); err != nil || fs.Type != unix.EXT4_SUPER_MAGIC {
		t.Fatal("actual ext4", err)
	}
	if uint64(fs.Blocks)*uint64(fs.Bsize) < 2700<<20 {
		t.Skip("original full 32-frame Ledger and Usage vector requires at least 2700 MiB usable ext4 capacity (3072 MiB backing)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	fixture := prepareHAMetadataMaintenanceFixture(t)
	// The independent baseline binds this ceiling. Set it before publication;
	// changing it only at reopen must be rejected by production qualification.
	for i, cfg := range fixture.configs {
		limits := *cfg.ResourceBudget
		limits.MetadataJournalDiskBytes = 576 << 20
		cfg.ResourceBudget = &limits
		fixture.configs[i] = cfg
	}
	prepareHAMetadataMaintenanceArtifacts(t, fixture)
	completeHAMemoryActivationBoundary(t, fixture)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	for _, cfg := range fixture.configs {
		guard, err := lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		activation, err := ActivateHAMemoryRuntime(ctx, cfg, fixture.keys, guard, functionalHAMetadataManager(t, cfg), logger)
		if err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(activation.Close(), guard.Close()); err != nil {
			t.Fatal(err)
		}
	}
	return ctx, fixture, logger
}

// One generation opens both roles from disk, requires every earlier fact on
// the Replica, replicates three new facts with two online checkpoints between
// them, then finishes, joins and releases both data directories. It returns
// the new facts and each role's final Seal revision. failStartup instead
// pressures the Replica before its Metadata receive window and closes both
// roles without the terminal sequence.
func runHAPhysicalOnlineGeneration(t *testing.T, ctx context.Context, fixture haMetadataMaintenanceFixture, logger *slog.Logger, generation int, earlier []string, previous [2]uint64, failStartup bool) ([]string, [2]uint64) {
	t.Helper()
	var owners [2]*haSelectedRuntimeFoundation
	var guards [2]*lock.Lock
	closed := false
	for i, cfg := range fixture.configs {
		limits := *cfg.ResourceBudget
		limits.TotalRAMBytes = 2 << 30
		reserve := haPhysicalRecoveryReserveFixture(t)
		limits.RecoveryRAMBytes, limits.RecoveryReserveRAMBytes = reserve, reserve
		limits.CompletionRAMBytes = 1 << 30
		limits.TotalUnconfirmedBytes = 64 << 20
		limits.IOUnconfirmedBytes = 32 << 20
		limits.CompletionUnconfirmedBytes = 32 << 20
		limits.RecoveryUnconfirmedBytes = 16 << 20
		limits.RecoveryReserveUnconfirmedBytes = 16 << 20
		limits.UnconfirmedMaxEntries = 64
		limits.MaxClaims += 32
		limits.ControlRAMBytes += 32 * resourcebudget.ClaimRAMBytes
		limits.PhysicalTerminalReserve = true
		// 32 actual Metadata COW extents alone occupy 512 MiB; current root,
		// controls and native prefix need additional space in the paid ceiling.
		limits.MetadataStoreDiskBytes = 576 << 20
		limits.MetadataJournalDiskBytes = 576 << 20
		limits.LedgerProjectionDiskBytes = 80 << 20
		limits.LedgerSegmentIndexDiskBytes = 48 << 20
		limits.UsageProjectionDiskBytes = 128 << 20
		if i == 1 {
			limits.LedgerProjectionDiskBytes = 576 << 20
			limits.UsageProjectionDiskBytes = 576 << 20
		}
		// The startup inventory counts held replacements of every derived domain
		// (about 1.1 GiB on the Replica after a failed start) beside native bytes.
		limits.PersistentInventoryDiskBytes = limits.MinimumPersistentInventoryDiskBytes() + 512<<20
		cfg.ResourceBudget = &limits
		guard, err := lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		guards[i] = guard
		manager := functionalHAMetadataManager(t, cfg)
		f, err := openHASelectedRuntimeFoundation(ctx, cfg, fixture.keys.Master, guard, manager, logger)
		if err != nil {
			t.Fatal("actual foundation", generation, i, err)
		}
		owners[i] = f
		t.Cleanup(func() {
			if closed {
				return
			}
			if err := f.Close(); err != nil {
				t.Error("actual foundation join", generation, i, err)
			}
			if stats := f.sharedPhysicalWork.Snapshot(); !stats.Closed || stats.Members != 0 || stats.Waiting != 0 || stats.InUse {
				t.Error("actual shared final owner retained", generation, i, stats)
			}
			if err := guard.Close(); err != nil {
				t.Error(err)
			}
		})
		if f.reopened != nil && f.reopened.Seal.Revision < previous[i] {
			t.Fatal("restart selected an older Seal", generation, i, f.reopened.Seal.Revision, previous[i])
		}
		if err := f.prepareSelectedServicesWithLease(ctx, i == 0); err != nil {
			t.Fatal("actual original vector", generation, i, err)
		}
		if err := f.installHASelectedGovernanceService(ctx); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err := f.prepareHASelectedPrimaryStartup(ctx); err != nil {
				t.Fatal(generation, err)
			}
		}
		if i == 1 && failStartup {
			if err := manager.SetPressure(resourcebudget.Pressured); err != nil {
				t.Fatal(err)
			}
			if err := f.reopened.Owners.Components.Metadata.PrepareHAReplicaPhysicalEscrow(ctx, resourcebudget.MaxCompletionBatch); err == nil {
				t.Fatal("pressured Replica receive window admitted")
			}
			var revisions [2]uint64
			for j, f := range owners {
				revisions[j] = f.reopened.Seal.Revision
				if err := f.Close(); err != nil && !errors.Is(err, authenticatedstate.ErrRecovery) {
					t.Fatal("failed start closed without a replayable hold", j, err)
				}
				if err := guards[j].Close(); err != nil {
					t.Fatal(j, err)
				}
			}
			closed = true
			logHAPhysicalFixtureSpace(t, generation)
			return nil, revisions
		}
		if i == 1 {
			if err := f.reopened.Owners.Components.Metadata.PrepareHAReplicaPhysicalEscrow(ctx, resourcebudget.MaxCompletionBatch); err != nil {
				if m, merr := f.reopened.Owners.Components.Ledger.ResourceManager(ctx); merr == nil {
					t.Logf("receive window refusal: stats=%+v limits=%+v", m.Snapshot(), *cfg.ResourceBudget)
				}
				t.Fatal("actual pre-exposure Metadata receive window", generation, err)
			}
		}
		if err := f.prepareSelectedTransportWithPhysicalCredit(logger); err != nil {
			t.Fatal("actual transport consumer", generation, i, err)
		}
		if f.sharedPhysicalWork == nil {
			t.Fatal("actual foundation omitted shared RAM owner", generation, i)
		}
		if stats := f.sharedPhysicalWork.Snapshot(); stats.Members != 3 || stats.Closing || stats.Closed {
			t.Fatal("actual Directory/Usage/Governance did not share", generation, i, stats)
		}
		t.Log("actual background shared ownership", generation, i, f.sharedPhysicalWork.Snapshot())
	}

	primary, replica := owners[0], owners[1]
	q := primary.replication.peerPhysical.Load()
	if q == nil || replica.replication.peerPhysical.Load() == nil {
		t.Fatal("original strict controllers absent")
	}
	managers := [2]*replication.ConnectionManager{primary.replication.manager, replica.replication.manager}
	business := [2]*haSelectedBusinessServices{primary.business, replica.business}
	stores := [2]*boltstore.Store{primary.metadataStore, replica.metadataStore}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 2)
	for _, f := range owners {
		go func(f *haSelectedRuntimeFoundation) { done <- f.serveSelectedTransport(runCtx) }(f)
	}
	joined := false
	join := func() {
		if joined {
			return
		}
		joined = true
		stop()
		for range 2 {
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		}
	}
	defer join()
	waitWindow := func() {
		t.Helper()
		deadline := time.NewTimer(10 * time.Second)
		defer deadline.Stop()
		for {
			q.mu.Lock()
			ready := q.peers[0].window.Operation == replication.PhysicalCreditWindow
			q.mu.Unlock()
			if ready && primary.replication.startupReady.Load() {
				return
			}
			select {
			case <-deadline.C:
				t.Fatal("actual TLS fresh bank absent", generation)
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	waitWindow()
	for _, id := range earlier {
		if got, err := replica.metadataStore.GetProject(ctx, id); err != nil || got.ID != id {
			t.Fatal("restarted Replica lost an earlier fact", generation, id, err)
		}
	}
	cost, err := haPeerPhysicalTerminalCost(2)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for cycle := 0; cycle < 3; cycle++ {
		capCtx, ticket, err := q.reserve(ctx, sha256.Sum256([]byte{byte(generation), byte(100 + cycle)}), cost)
		if err != nil {
			t.Fatal("actual future after original online renewal", generation, cycle, err)
		}
		id := fmt.Sprintf("prj_online_physical_%d_%d", generation, cycle)
		if _, err := primary.metadataStore.PutProject(capCtx, domain.Project{ID: id, Name: "same online owner", Enabled: true}, 0, nil); err != nil {
			t.Fatal("actual native Metadata after renewal", generation, cycle, err)
		}
		if err := q.finishTicket(ctx, ticket); err != nil {
			t.Fatal("actual full applied peer join", generation, cycle, err)
		}
		if got, err := replica.metadataStore.GetProject(ctx, id); err != nil || got.ID != id {
			t.Fatal("actual Replica fact", generation, got, err)
		}
		ids = append(ids, id)
		if cycle == 2 {
			break
		}
		for i, f := range owners {
			before := f.reopened.Seal.Revision
			if err := f.checkpointHAPhysicalOnline(ctx); err != nil {
				t.Fatal("actual online same owner checkpoint", generation, cycle, i, err)
			}
			if f.reopened.Seal.Revision != before+1 || f.business != business[i] || f.metadataStore != stores[i] || f.replication.manager != managers[i] || f.recovery.Load() {
				t.Fatal("online maintenance replaced or retired actual service/transport owner", generation, cycle, i)
			}
		}
		waitWindow()
	}
	for _, f := range owners {
		if err := f.finishHAPhysicalBusiness(ctx); err != nil {
			t.Fatal("actual final whole common", generation, err)
		}
		if err := f.closeHAPhysicalNative(ctx); err != nil {
			t.Fatal("actual final native join", generation, err)
		}
	}
	join()
	var revisions [2]uint64
	for i, f := range owners {
		revisions[i] = f.reopened.Seal.Revision
		if err := f.Close(); err != nil {
			t.Fatal("actual foundation join", generation, i, err)
		}
		if stats := f.sharedPhysicalWork.Snapshot(); !stats.Closed || stats.Members != 0 || stats.Waiting != 0 || stats.InUse {
			t.Fatal("actual shared final owner retained", generation, i, stats)
		}
		if err := guards[i].Close(); err != nil {
			t.Fatal(generation, i, err)
		}
	}
	closed = true
	logHAPhysicalFixtureSpace(t, generation)
	return ids, revisions
}

func logHAPhysicalFixtureSpace(t *testing.T, generation int) {
	t.Helper()
	t.Log("ext4 available after generation", generation, haPhysicalFixtureSpace(t))
}

func haPhysicalFixtureSpace(t *testing.T) uint64 {
	t.Helper()
	var fs unix.Statfs_t
	if err := unix.Statfs(os.Getenv("HALRO_EXT4_ROOT"), &fs); err != nil {
		t.Fatal(err)
	}
	return uint64(fs.Bavail) * uint64(fs.Bsize)
}
