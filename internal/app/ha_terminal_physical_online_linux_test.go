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
	"strings"
	"testing"
	"time"
)

// Actual same-owner online renewal with real TLS and full 32-frame Metadata,
// Ledger and Usage banks. Functional Manager evidence is not cfg.Validate or
// real OS pressure / public Default Runtime capacity evidence.
func TestHAPhysicalOnlineCheckpointKeepsActualServicesAndTLS(t *testing.T) {
	ctx, fixture, logger := prepareHAPhysicalOnlineFixture(t, 2*time.Minute)
	runHAPhysicalOnlineGeneration(t, ctx, fixture.configs[:], fixture.keys.Master, logger, 0, nil, nil, haPhysicalGenerationNormal)
}

// Both roles run start, replicated requests, online checkpoints and the full
// terminal Close three times over the same data directories: the original
// generation and two restarts, each from a fresh Manager. Every earlier
// generation's facts must be served by the restarted Replica.
func TestHAPhysicalLifecycleSurvivesTwoRestarts(t *testing.T) {
	ctx, fixture, logger := prepareHAPhysicalOnlineFixture(t, 6*time.Minute)
	var written []string
	var revisions []uint64
	for generation := 0; generation < 3; generation++ {
		var ids []string
		ids, revisions = runHAPhysicalOnlineGeneration(t, ctx, fixture.configs[:], fixture.keys.Master, logger, generation, written, revisions, haPhysicalGenerationNormal)
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
	var revisions []uint64
	var after [3]uint64
	for generation := 1; generation < 3; generation++ {
		var ids []string
		ids, revisions = runHAPhysicalOnlineGeneration(t, ctx, fixture.configs[:], fixture.keys.Master, logger, generation, written, revisions, haPhysicalGenerationNormal)
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

// Both roles finish and close at actual ENOSPC after replicated facts and
// online checkpoints. Once an operator frees the foreign fill, the next start
// must serve every fact on the Replica from a Seal no older than the close
// and reclaim nothing: a complete close holds no replayable vector.
func TestHAPhysicalActualENOSPCTerminalAndBothRolesRestart(t *testing.T) {
	ctx, fixture, logger := prepareHAPhysicalOnlineFixture(t, 6*time.Minute)
	written, revisions := runHAPhysicalOnlineGeneration(t, ctx, fixture.configs[:], fixture.keys.Master, logger, 0, nil, nil, haPhysicalGenerationENOSPCClose)
	closed := haPhysicalFixtureSpace(t)
	runHAPhysicalOnlineGeneration(t, ctx, fixture.configs[:], fixture.keys.Master, logger, 1, written, revisions, haPhysicalGenerationNormal)
	if reclaimed := int64(haPhysicalFixtureSpace(t)) - int64(closed); reclaimed > 1<<20 {
		t.Fatal("restart reclaimed space the ENOSPC close held", reclaimed)
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
	runHAPhysicalOnlineGeneration(t, ctx, request.Configs[:], request.Keys.Master, logger, 0, nil, nil, haPhysicalGenerationFailedStart)
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
// the new facts and each role's final Seal revision. Member 0 is the Primary;
// every other member is a Replica (two Replicas make a three-node group).
// The mode selects how the generation ends; see haPhysicalGenerationMode.
func runHAPhysicalOnlineGeneration(t *testing.T, ctx context.Context, configs []config.Config, master []byte, logger *slog.Logger, generation int, earlier []string, previous []uint64, mode haPhysicalGenerationMode) ([]string, []uint64) {
	t.Helper()
	owners := make([]*haSelectedRuntimeFoundation, len(configs))
	guards := make([]*lock.Lock, len(configs))
	budgets := make([]*resourcebudget.Manager, len(configs))
	closed := false
	for i, cfg := range configs {
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
		if i > 0 {
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
		budgets[i] = manager
		f, err := openHASelectedRuntimeFoundation(ctx, cfg, master, guard, manager, logger)
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
		if f.reopened != nil && i < len(previous) && f.reopened.Seal.Revision < previous[i] {
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
		if i == 1 && mode == haPhysicalGenerationFailedStart {
			if err := manager.SetPressure(resourcebudget.Pressured); err != nil {
				t.Fatal(err)
			}
			if err := f.reopened.Owners.Components.Metadata.PrepareHAReplicaPhysicalEscrow(ctx, resourcebudget.MaxCompletionBatch); err == nil {
				t.Fatal("pressured Replica receive window admitted")
			}
			revisions := make([]uint64, len(owners))
			for j, f := range owners[:i+1] {
				revisions[j] = f.reopened.Seal.Revision
				if err := f.Close(); err != nil && !errors.Is(err, authenticatedstate.ErrRecovery) {
					t.Fatal("failed start closed without a replayable hold", j, err)
				}
				if err := guards[j].Close(); err != nil {
					t.Fatal(j, err)
				}
			}
			if len(owners) > i+1 {
				t.Fatal("failed start leaves later members unopened; only two-member groups are supported")
			}
			closed = true
			logHAPhysicalFixtureSpace(t, generation)
			return nil, revisions
		}
		if i > 0 {
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

	primary, replicas := owners[0], owners[1:]
	q := primary.replication.peerPhysical.Load()
	if q == nil || len(replicas) == 0 || len(replicas) > len(q.peers) {
		t.Fatal("original strict controllers absent")
	}
	managers := make([]*replication.ConnectionManager, len(owners))
	business := make([]*haSelectedBusinessServices, len(owners))
	stores := make([]*boltstore.Store, len(owners))
	for i, f := range owners {
		if i > 0 && f.replication.peerPhysical.Load() == nil {
			t.Fatal("original strict Replica controller absent", i)
		}
		managers[i], business[i], stores[i] = f.replication.manager, f.business, f.metadataStore
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, len(owners))
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
		for range owners {
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
			ready := true
			for p := range replicas {
				ready = ready && q.peers[p].window.Operation == replication.PhysicalCreditWindow
			}
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
	for r, replica := range replicas {
		for _, id := range earlier {
			if got, err := replica.metadataStore.GetProject(ctx, id); err != nil || got.ID != id {
				t.Fatal("restarted Replica lost an earlier fact", generation, r, id, err)
			}
		}
	}
	// Reads are refused at pressure too, so a write refused at an earlier
	// generation's ENOSPC is proved absent here, on every member.
	for g := 0; g < generation; g++ {
		refused := fmt.Sprintf("prj_refused_physical_%d", g)
		for i, f := range owners {
			if _, err := f.metadataStore.GetProject(ctx, refused); !errors.Is(err, boltstore.ErrNotFound) {
				t.Fatal("refused fact observed after restart", generation, i, refused, err)
			}
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
		for r, replica := range replicas {
			if got, err := replica.metadataStore.GetProject(ctx, id); err != nil || got.ID != id {
				t.Fatal("actual Replica fact", generation, r, got, err)
			}
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
	root := os.Getenv("HALRO_EXT4_ROOT")
	if mode == haPhysicalGenerationENOSPCClose {
		if err := exhaustHAPhysicalFixture(filepath.Join(root, fmt.Sprintf("foreign-ha-terminal-fill-%d", generation)), haPhysicalFixtureFillBound); err != nil {
			t.Fatal(err)
		}
	}
	if mode == haPhysicalGenerationPressuredClose || mode == haPhysicalGenerationENOSPCClose {
		for _, m := range budgets {
			if err := m.SetPressure(resourcebudget.Pressured); err != nil {
				t.Fatal(err)
			}
		}
	}
	if mode == haPhysicalGenerationENOSPCClose {
		// The Replica grants credit from its paid lanes, so the session stays
		// up; the write itself is refused by pressure and its credit released.
		// A refused write leaves no fact, proved after the restart. The bound
		// turns a dropped session, which would leave the reply unanswered,
		// into a failure rather than a wait for the test deadline.
		refused := fmt.Sprintf("prj_refused_physical_%d", generation)
		reserveCtx, cancelReserve := context.WithTimeout(ctx, 30*time.Second)
		capCtx, ticket, err := q.reserve(reserveCtx, sha256.Sum256([]byte{byte(generation), 200}), cost)
		if err != nil {
			t.Fatal("peer credit at actual ENOSPC", generation, err)
		}
		_, err = primary.metadataStore.PutProject(capCtx, domain.Project{ID: refused, Name: "refused at ENOSPC", Enabled: true}, 0, nil)
		cancelReserve()
		if !errors.Is(err, resourcebudget.ErrPressure) {
			t.Fatal("new replicated fact not refused by pressure at actual ENOSPC", generation, err)
		}
		if err := q.finishTicket(ctx, ticket); err != nil {
			t.Fatal("refused write kept its peer credit", generation, err)
		}
		q.mu.Lock()
		live := q.live
		q.mu.Unlock()
		if live != nil {
			t.Fatal("refused write left a live peer ticket", generation)
		}
	}
	for i, f := range owners {
		finishCtx := ctx
		if mode == haPhysicalGenerationENOSPCClose {
			// A member's retirement can return its own blocks. Take them back
			// before the next member finishes so no member's terminal sequence
			// borrows capacity another member freed.
			if err := exhaustHAPhysicalFixture(filepath.Join(root, fmt.Sprintf("foreign-ha-terminal-fill-%d-%d", generation, i)), haPhysicalFixtureFillBound); err != nil {
				t.Fatal(err)
			}
			finishCtx = context.WithValue(ctx, haPhysicalFinishObserverKey{}, func(stage string) error {
				if stage == "common-ready" {
					return exhaustHAPhysicalFixture(filepath.Join(root, fmt.Sprintf("foreign-ha-terminal-publication-fill-%d-%d", generation, i)), haPhysicalFixtureFillBound)
				}
				return nil
			})
		}
		if err := f.finishHAPhysicalBusiness(finishCtx); err != nil {
			t.Fatal("actual final whole common", generation, err)
		}
		if err := f.closeHAPhysicalNative(ctx); err != nil {
			t.Fatal("actual final native join", generation, err)
		}
	}
	join()
	revisions := make([]uint64, len(owners))
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
	if mode == haPhysicalGenerationENOSPCClose {
		for i, cfg := range configs {
			if left := haPhysicalOwnedTemporaries(t, cfg.Storage.DataDir); len(left) != 0 {
				t.Fatal("terminal close at actual ENOSPC left owned temporaries", generation, i, left)
			}
		}
		fills, err := filepath.Glob(filepath.Join(root, "foreign-ha-terminal-*"))
		if err != nil || len(fills) == 0 {
			t.Fatal("foreign fills absent", fills, err)
		}
		for _, fill := range fills {
			if err := os.Remove(fill); err != nil {
				t.Fatal(err)
			}
		}
	}
	logHAPhysicalFixtureSpace(t, generation)
	return ids, revisions
}

// haPhysicalGenerationMode selects how runHAPhysicalOnlineGeneration ends.
type haPhysicalGenerationMode int

const (
	// The terminal sequence and Close run with admission open.
	haPhysicalGenerationNormal haPhysicalGenerationMode = iota
	// The first Replica is pressured before its Metadata receive window and
	// every opened role closes without the terminal sequence.
	haPhysicalGenerationFailedStart
	// The terminal sequence and Close run with every member at full pressure,
	// where any new normal admission is refused.
	haPhysicalGenerationPressuredClose
	// As pressured, on a filesystem a foreign file has filled to actual
	// ENOSPC, refilled before each member finishes. A new replicated write
	// must be refused, and Close must leave no owned temporary behind. The
	// fills are removed once every member has closed.
	haPhysicalGenerationENOSPCClose
)

// haPhysicalFixtureFillBound bounds a foreign fill of the 3 GiB and 6 GiB
// lifecycle images.
const haPhysicalFixtureFillBound = 6 << 30

// haPhysicalOwnedTemporaries lists files under dataDir named by the
// temporary patterns the persistent owners create before a rename.
func haPhysicalOwnedTemporaries(t *testing.T, dataDir string) []string {
	t.Helper()
	prefixes := []string{".metadata-journal-", ".selected-rotation-", ".seed-manifest-", ".reseed-required-", ".totp-watermark-", ".maintenance-", ".state-", ".object-source-", ".capture-", ".archive-", ".ndjson-", ".usage-", ".manifest-"}
	var left []string
	err := filepath.WalkDir(dataDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		name := d.Name()
		if strings.HasSuffix(name, ".tmp") {
			left = append(left, path)
			return nil
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(name, prefix) {
				left = append(left, path)
				return nil
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return left
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
