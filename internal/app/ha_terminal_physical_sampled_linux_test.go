//go:build linux

package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"golang.org/x/sys/unix"
)

// Thresholds of the sampled HA reserve experiment, fixed before any run
// (design §9.2, board §35). The pressure bounds are §32's, the rest §33's;
// they are screening values for a local fixture, never widened after a run.
const (
	haSampledRounds         = 24 // alternating real disk and memory pressure
	haSampledEnterWithin    = 5 * time.Second
	haSampledExitWithin     = 10 * time.Second
	haSampledHysteresisHold = time.Second
	haSampledMemoryBuffer   = 1 << 30
	// The cgroup peak ≤ 1.5 GiB with no max/oom/oom_kill event is read from
	// the runner's cgroup record after the run; the peak includes the buffer.
)

// TestHAPhysicalThreeMemberReserveSampledPressure runs the §34 reserve cycle
// on the target three-member topology with every member's pressure owned by a
// real sampler on its data directory, as the Runtime wires it: the test never
// sets pressure. Even rounds fill the shared ext4 to actual ENOSPC; odd rounds
// hold a touched 1 GiB buffer. Each round accepts one request, waits for every
// sampler to close admission, refuses a new admission beside the accepted one,
// finishes it from the reserve (at zero free blocks on disk rounds), has both
// Replicas apply it, refuses a new admission with nothing in flight, and
// recovers through the sampler. Disk rounds first release only into the band
// between the stop and resume lines, where every member must stay closed.
// Online checkpoints, drift, ENOSPC close and reopen are as in §33.
func TestHAPhysicalThreeMemberReserveSampledPressure(t *testing.T) {
	if os.Getenv("HALRO_HA_SUSTAINED_PRESSURE") != "1" {
		t.Skip("opt-in sustained HA reserve experiment")
	}
	root := os.Getenv("HALRO_EXT4_ROOT")
	if root == "" {
		t.Skip("requires explicitly isolated ext4 runner")
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(root, &fs); err != nil || fs.Type != unix.EXT4_SUPER_MAGIC {
		t.Fatal("actual ext4 fixture", err)
	}
	policy := selectedCapacityPressure()
	profile := ledger.HASelectedCompletionWorkspaceRequirements()
	var completionRAM int64
	for _, slot := range profile {
		completionRAM += slot.RAM
	}
	ctx, fixture, _ := prepareHAPhysicalThreeMemberFixture(t, 8*time.Minute)
	configs, master := fixture.configs[:], fixture.keys.Master
	group := openHAPhysicalBusinessGroup(t, ctx, configs, master, true, &policy, nil)
	primary, replicas := group.owners[0], group.owners[1:]
	c := primary.reopened.Owners.Components
	fills := []string{filepath.Join(root, "foreign-sampled-fill"), filepath.Join(root, "foreign-sampled-refill")}
	t.Cleanup(func() {
		for _, fill := range fills {
			_ = os.Remove(fill)
		}
	})
	members := len(group.owners)
	opened := make([]resourcebudget.Stats, members)
	for i, f := range group.owners {
		opened[i] = f.reopened.Owners.Components.Budget.Snapshot()
	}
	var (
		finalized              []string
		baselineHeap           uint64
		baselineSpace          uint64
		baselineFDs            int
		baselineStats          = make([]resourcebudget.Stats, members)
		terminalMax            time.Duration
		enterMax, exitMax      time.Duration
		checkpoints, deferrals int
		deferMax               time.Duration
	)
	started := time.Now()
	for round := range haSampledRounds {
		disk := round%2 == 0
		admitted, retire, err := primary.peerPhysicalTerminalAdmission(ctx, 2)
		if err != nil {
			t.Fatal("admission with space", round, err)
		}
		work, err := c.Budget.TryAcquireCompletableWork(resourcebudget.Active, resourcebudget.Cost{RAM: 4096}, resourcebudget.Cost{RAM: completionRAM}, profile)
		if err != nil {
			t.Fatalf("accepted request completion RAM %d: %v %+v", round, err, c.Budget.Snapshot())
		}
		request, err := primary.business.accounting.BeginRequestDetailed(work.Bind(admitted), "prj_physical", "key_physical", fmt.Sprintf("sampled-accepted-%d", round), "model")
		if err != nil {
			t.Fatal("RequestAccepted", round, err)
		}
		waitHASustainedApplied(t, ctx, primary, replicas, round)
		var buffer []byte
		enterStart := time.Now()
		if disk {
			if err := exhaustHAPhysicalFixture(fills[0], haPhysicalFixtureFillBound); err != nil {
				t.Fatal(round, err)
			}
		} else {
			buffer = make([]byte, haSampledMemoryBuffer)
			for i := 0; i < len(buffer); i += 4096 {
				buffer[i] = 1
			}
		}
		enter := waitHASampledPressure(t, ctx, group.owners, resourcebudget.Pressured, haSampledEnterWithin, round)
		enterMax = max(enterMax, time.Since(enterStart))
		if _, closeNew, err := primary.peerPhysicalTerminalAdmission(ctx, 2); err == nil {
			_ = closeNew()
			t.Fatal("new admission succeeded beside an accepted request under sampled pressure", round, disk)
		} else {
			t.Log("HA_SAMPLED_REFUSAL_LIVE", round, disk, err)
		}
		for i, f := range group.owners {
			checkpointCtx, stop := context.WithTimeout(ctx, 5*time.Second)
			err := f.checkpointHAPhysicalOnline(checkpointCtx)
			stop()
			if f.recovery.Load() {
				t.Fatal("background checkpoint under sampled pressure forced recovery", round, i, err)
			}
		}
		if disk {
			if err := exhaustHAPhysicalFixture(fills[1], haPhysicalFixtureFillBound); err != nil {
				t.Fatal(round, err)
			}
			if err := unix.Statfs(root, &fs); err != nil || fs.Bavail != 0 {
				t.Fatal("terminal would not run at ENOSPC", round, fs.Bavail, err)
			}
		}
		terminalStart := time.Now()
		terminalCtx, stopTerminal := context.WithTimeout(admitted, haSustainedTerminalWithin)
		err = work.WithWorkspace(terminalCtx, func(ctx context.Context) error {
			return primary.business.accounting.Finalize(ctx, request, "success")
		})
		stopTerminal()
		if err != nil {
			t.Fatal("accepted terminal under sampled pressure", round, disk, err)
		}
		terminalMax = max(terminalMax, time.Since(terminalStart))
		if err := errors.Join(work.Close(), work.Active.Release()); err != nil {
			t.Fatal("accepted request RAM retirement", round, err)
		}
		waitHASustainedApplied(t, ctx, primary, replicas, round)
		if err := retire(); err != nil {
			t.Fatal("accepted completion owners", round, err)
		}
		finalized = append(finalized, request.RequestID)
		if _, closeNew, err := primary.peerPhysicalTerminalAdmission(ctx, 2); err == nil {
			_ = closeNew()
			t.Fatal("new admission succeeded under sampled pressure with nothing in flight", round, disk)
		} else {
			if !errors.Is(err, resourcebudget.ErrPressure) && !errors.Is(err, resourcebudget.ErrPhysicalAllocation) {
				t.Fatal("refusal with nothing in flight is neither the pressure nor the physical stop", round, err)
			}
			t.Log("HA_SAMPLED_REFUSAL_IDLE", round, disk, err)
		}
		exitStart := time.Now()
		if disk {
			if err := os.Remove(fills[1]); err != nil {
				t.Fatal(round, err)
			}
			fill, err := os.OpenFile(fills[0], os.O_RDWR, 0)
			if err != nil {
				t.Fatal(round, err)
			}
			band := (policy.DiskEnterBytes + policy.DiskExitBytes) / 2
			setSustainedFreeBytes(t, fill, root, band)
			holdHASampledPressure(t, ctx, group.owners, haSampledHysteresisHold, round)
			if err := errors.Join(fill.Close(), os.Remove(fills[0])); err != nil {
				t.Fatal(round, err)
			}
			exitStart = time.Now()
		} else {
			// The buffer is live until here, through the whole pressured span.
			runtime.KeepAlive(buffer)
			buffer = nil
			runtime.GC()
			debug.FreeOSMemory()
		}
		exit := waitHASampledPressure(t, ctx, group.owners, resourcebudget.Healthy, haSampledExitWithin, round)
		exitMax = max(exitMax, time.Since(exitStart))
		for r, replica := range replicas {
			if _, found, err := replica.reopened.Owners.Components.Ledger.ReadFinalization(ctx, request.RequestID); err != nil || !found {
				t.Fatal("Replica did not apply the accepted terminal", round, r, found, err)
			}
		}
		if _, closeAgain, err := primary.peerPhysicalTerminalAdmission(ctx, 2); err != nil {
			t.Fatal("admission after recovery", round, err)
		} else if err := closeAgain(); err != nil {
			t.Fatal(round, err)
		}
		for i, f := range group.owners {
			waitHASustainedQuiescent(t, ctx, f, opened[i], resourcebudget.Stats{}, false)
		}
		if (round+1)%haSustainedCheckpointEvery == 0 {
			d, m := checkpointHASustained(t, ctx, group.owners, round)
			deferrals, deferMax = deferrals+d, max(deferMax, m)
			waitHAPhysicalCreditWindow(t, ctx, primary, len(replicas))
			checkpoints++
			if checkpoints == 1 {
				runtime.GC()
				var ms runtime.MemStats
				runtime.ReadMemStats(&ms)
				baselineHeap = ms.HeapAlloc
				baselineSpace = haPhysicalFixtureSpace(t)
				baselineFDs = len(sustainedOpenFDs(t))
				for i, f := range group.owners {
					baselineStats[i] = waitHASustainedQuiescent(t, ctx, f, opened[i], resourcebudget.Stats{}, false)
				}
			}
		}
		t.Logf("HA_SAMPLED_ROUND %d disk=%t enter=%s exit=%s terminal_max=%s space=%d", round, disk, enter, exit, terminalMax, haPhysicalFixtureSpace(t))
	}
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	if ms.HeapAlloc > baselineHeap+haSustainedHeapDrift {
		t.Fatal("live heap drifted across sampled pressure", baselineHeap, ms.HeapAlloc)
	}
	if space := haPhysicalFixtureSpace(t); space+haSustainedSpaceDrift < baselineSpace {
		t.Fatal("ext4 space drifted across sampled pressure", baselineSpace, space)
	}
	if fds := sustainedOpenFDs(t); len(fds) > baselineFDs {
		t.Fatal("FD drift across sampled pressure", baselineFDs, fds)
	}
	for i, f := range group.owners {
		waitHASustainedQuiescent(t, ctx, f, opened[i], baselineStats[i], true)
	}
	t.Logf("HA_SAMPLED_SUMMARY rounds=%d checkpoints=%d deferrals=%d defer_max=%s enter_max=%s exit_max=%s terminal_max=%s heap=%d/%d space=%d/%d elapsed=%s",
		haSampledRounds, checkpoints, deferrals, deferMax, enterMax, exitMax, terminalMax, ms.HeapAlloc, baselineHeap, haPhysicalFixtureSpace(t), baselineSpace, time.Since(started))
	for i, f := range group.owners {
		fill := filepath.Join(root, fmt.Sprintf("foreign-sampled-finish-fill-%d", i))
		fills = append(fills, fill)
		if err := exhaustHAPhysicalFixture(fill, haPhysicalFixtureFillBound); err != nil {
			t.Fatal(err)
		}
		if err := f.finishHAPhysicalBusiness(ctx); err != nil {
			t.Fatal("confirmed terminal whole common at ENOSPC", i, err)
		}
		if err := f.closeHAPhysicalNative(ctx); err != nil {
			t.Fatal("physical native close at ENOSPC", i, err)
		}
	}
	group.release()
	for i, cfg := range configs {
		if left := haPhysicalOwnedTemporaries(t, cfg.Storage.DataDir); len(left) != 0 {
			t.Fatal("owned temporaries survived", i, left)
		}
	}
	for _, fill := range fills[2:] {
		if err := os.Remove(fill); err != nil {
			t.Fatal(err)
		}
	}
	reopened := openHAPhysicalBusinessGroup(t, ctx, configs, master, true, &policy, nil)
	for i, f := range reopened.owners {
		for _, id := range finalized {
			if _, found, err := f.reopened.Owners.Components.Ledger.ReadFinalization(ctx, id); err != nil || !found {
				t.Fatal("reopened member lost an accepted terminal", i, id, found, err)
			}
		}
	}
	if _, closeAgain, err := reopened.owners[0].peerPhysicalTerminalAdmission(ctx, 2); err != nil {
		t.Fatal("admission after reopen", err)
	} else if err := closeAgain(); err != nil {
		t.Fatal(err)
	}
	for i, f := range reopened.owners {
		if err := f.finishHAPhysicalBusiness(ctx); err != nil {
			t.Fatal("reopened whole common", i, err)
		}
		if err := f.closeHAPhysicalNative(ctx); err != nil {
			t.Fatal("reopened native close", i, err)
		}
	}
	reopened.release()
}

// waitHASampledPressure waits until every member's sampler-owned Manager
// reports state, failing past within. It returns the time taken.
func waitHASampledPressure(t *testing.T, ctx context.Context, owners []*haSelectedRuntimeFoundation, state resourcebudget.Pressure, within time.Duration, round int) time.Duration {
	t.Helper()
	start := time.Now()
	for {
		all := true
		for _, f := range owners {
			all = all && f.reopened.Owners.Components.Budget.Snapshot().Pressure == state
		}
		if all {
			return time.Since(start)
		}
		if time.Since(start) > within {
			got := make([]resourcebudget.Pressure, len(owners))
			for i, f := range owners {
				got[i] = f.reopened.Owners.Components.Budget.Snapshot().Pressure
			}
			t.Fatal("sampled pressure did not reach the expected state", round, state, got)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// holdHASampledPressure requires every member to stay Pressured for hold.
func holdHASampledPressure(t *testing.T, ctx context.Context, owners []*haSelectedRuntimeFoundation, hold time.Duration, round int) {
	t.Helper()
	until := time.Now().Add(hold)
	for time.Now().Before(until) {
		for i, f := range owners {
			if p := f.reopened.Owners.Components.Budget.Snapshot().Pressure; p != resourcebudget.Pressured {
				t.Fatal("member left pressure inside the hysteresis band", round, i, p)
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// checkpointHASustained runs the online checkpoint with space on every
// member, retrying the designed ErrPhysicalCredit deferral within its bound
// (§33 v4). It returns the deferral count and the longest checkpoint.
func checkpointHASustained(t *testing.T, ctx context.Context, owners []*haSelectedRuntimeFoundation, round int) (int, time.Duration) {
	t.Helper()
	var deferrals int
	var longest time.Duration
	for i, f := range owners {
		before := f.reopened.Seal.Revision
		start := time.Now()
		for {
			err := f.checkpointHAPhysicalOnline(ctx)
			if err == nil {
				break
			}
			if !errors.Is(err, replication.ErrPhysicalCredit) || f.recovery.Load() || time.Since(start) > haSustainedDeferWithin {
				t.Fatal("online checkpoint with space", round, i, time.Since(start), err)
			}
			deferrals++
			time.Sleep(50 * time.Millisecond)
		}
		longest = max(longest, time.Since(start))
		if f.reopened.Seal.Revision != before+1 || f.recovery.Load() {
			t.Fatal("online checkpoint did not renew in place", round, i, before, f.reopened.Seal.Revision)
		}
	}
	return deferrals, longest
}
