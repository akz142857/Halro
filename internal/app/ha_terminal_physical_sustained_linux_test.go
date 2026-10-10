//go:build linux

package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"golang.org/x/sys/unix"
)

// Thresholds of the HA physical-reserve sustained-pressure experiment, fixed
// before any run (design §9.2, board §33). They are screening values for a
// local two-member fixture, not production SLOs, and are never widened after
// a run.
const (
	haSustainedRounds          = 24 // alternating unobserved and observed ENOSPC
	haSustainedCheckpointEvery = 3  // online checkpoint on both members
	haSustainedTerminalWithin  = 5 * time.Second
	haSustainedAppliedWithin   = 5 * time.Second
	haSustainedQuiesceWithin   = 5 * time.Second
	// v4: a checkpoint refused with ErrPhysicalCredit while a Replica
	// promise is outstanding is the designed deferral the scheduled loop
	// retries on its next tick; it may be retried here and must clear
	// within this bound.
	haSustainedDeferWithin = 5 * time.Second
	haSustainedHeapDrift   = 32 << 20
	haSustainedSpaceDrift  = 32 << 20
	// The cgroup peak ≤ 1.5 GiB with no max/oom/oom_kill event is read from
	// the runner's cgroup record after the run.
)

// TestHAPhysicalReserveSustainedPressure repeats the §31 reserve cycle on an
// HA pair over the strict TLS transport the Runtime serves with the physical
// reserve, admitting through peer credit and the local reserve together
// (v5: the ordinary business pair has no credit controller, so its online
// checkpoints cannot run at all): each round accepts one request, fills the
// isolated ext4 to actual ENOSPC, refuses a new admission beside it, finishes
// the accepted terminal from the reserve at zero free blocks, has the Replica
// apply it, refuses a new admission at the physical stop with nothing in
// flight, and recovers. Even rounds leave pressure unobserved (Healthy, the
// sampling-delay window); odd rounds also set both members Pressured, as an
// observed stop line would. Online checkpoints run
// throughout. Fees, claims, ext4 space, FDs and live heap must not drift; both
// members must close at ENOSPC without owned temporaries, and a fresh open of
// the same directories must serve every terminal on both members.
func TestHAPhysicalReserveSustainedPressure(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	profile := ledger.HASelectedCompletionWorkspaceRequirements()
	var completionRAM int64
	for _, slot := range profile {
		completionRAM += slot.RAM
	}
	// v5: the fixed physical profile, with the strict pair's Metadata and
	// inventory ceilings; one request is in flight at a time.
	var adjust func(int, *config.ResourceBudget)
	fixture := activateHAPhysicalBusinessFixture(t, ctx, true)
	pair := openHAPhysicalBusinessPair(t, ctx, fixture, true, adjust)
	primary, replica := pair.owners[0], pair.owners[1]
	c := primary.reopened.Owners.Components
	fills := []string{filepath.Join(root, "foreign-sustained-fill"), filepath.Join(root, "foreign-sustained-refill"), filepath.Join(root, "foreign-sustained-idle-fill")}
	t.Cleanup(func() {
		for _, fill := range fills {
			_ = os.Remove(fill)
		}
	})
	var (
		finalized     []string
		baselineHeap  uint64
		baselineSpace uint64
		baselineFDs   int
		baselineStats [2]resourcebudget.Stats
		terminalMax   time.Duration
		checkpoints   int
		deferrals     int
		deferMax      time.Duration
	)
	// v2: the HA fixture's resident owners hold Completion RAM and a
	// completion reservation from startup, so each round's fees are compared
	// with this idle snapshot rather than with zero.
	var opened [2]resourcebudget.Stats
	for i, f := range pair.owners {
		opened[i] = f.reopened.Owners.Components.Budget.Snapshot()
		t.Logf("HA_SUSTAINED_OPENED %d completion_reserved=%+v active=%+v completion=%+v", i, opened[i].CompletionReserved, opened[i].Classes[resourcebudget.Active], opened[i].Classes[resourcebudget.Completion])
	}
	started := time.Now()
	for round := range haSustainedRounds {
		observed := round%2 == 1
		admitted, retire, err := primary.peerPhysicalTerminalAdmission(ctx, 2)
		if err != nil {
			t.Fatal("physical admission with space", round, err)
		}
		work, err := c.Budget.TryAcquireCompletableWork(resourcebudget.Active, resourcebudget.Cost{RAM: 4096}, resourcebudget.Cost{RAM: completionRAM}, profile)
		if err != nil {
			t.Fatalf("accepted request completion RAM %d: %v %+v", round, err, c.Budget.Snapshot())
		}
		request, err := primary.business.accounting.BeginRequestDetailed(work.Bind(admitted), "prj_physical", "key_physical", fmt.Sprintf("sustained-accepted-%d", round), "model")
		if err != nil {
			t.Fatal("RequestAccepted", round, err)
		}
		waitHASustainedApplied(t, ctx, primary, replica, round)
		if err := exhaustHAPhysicalFixture(fills[0], haPhysicalFixtureFillBound); err != nil {
			t.Fatal(round, err)
		}
		for i, f := range pair.owners {
			m := f.reopened.Owners.Components.Budget
			if p := m.Snapshot().Pressure; p != resourcebudget.Healthy {
				t.Fatal("pressure before the round's transition", round, i, p)
			}
			if observed {
				if err := m.SetPressure(resourcebudget.Pressured); err != nil {
					t.Fatal(round, i, err)
				}
				if _, err := m.TryAcquire(resourcebudget.IO, resourcebudget.Cost{RAM: 1}); !errors.Is(err, resourcebudget.ErrPressure) {
					t.Fatal("ordinary new IO admitted at observed ENOSPC", round, i, err)
				}
			}
		}
		// v5: the Primary's credit controller holds one live ticket, so this
		// refusal need not be physical; the physical one is checked once the
		// accepted ticket is released, below.
		if _, closeNew, err := primary.peerPhysicalTerminalAdmission(ctx, 2); err == nil {
			_ = closeNew()
			t.Fatal("new admission succeeded beside an accepted request at actual ENOSPC", round, observed)
		} else {
			t.Log("HA_SUSTAINED_REFUSAL_LIVE", round, observed, err)
		}
		for i, f := range pair.owners {
			checkpointCtx, stop := context.WithTimeout(ctx, 5*time.Second)
			err := f.checkpointHAPhysicalOnline(checkpointCtx)
			stop()
			if f.recovery.Load() {
				t.Fatal("background checkpoint at ENOSPC forced recovery on an accepted reserve", round, i, err)
			}
		}
		if err := exhaustHAPhysicalFixture(fills[1], haPhysicalFixtureFillBound); err != nil {
			t.Fatal(round, err)
		}
		if err := unix.Statfs(root, &fs); err != nil || fs.Bavail != 0 {
			t.Fatal("terminal would not run at ENOSPC", round, fs.Bavail, err)
		}
		terminalStart := time.Now()
		terminalCtx, stopTerminal := context.WithTimeout(admitted, haSustainedTerminalWithin)
		err = work.WithWorkspace(terminalCtx, func(ctx context.Context) error {
			return primary.business.accounting.Finalize(ctx, request, "success")
		})
		stopTerminal()
		if err != nil {
			t.Fatal("accepted terminal at ENOSPC", round, err)
		}
		terminalMax = max(terminalMax, time.Since(terminalStart))
		if err := errors.Join(work.Close(), work.Active.Release()); err != nil {
			t.Fatal("accepted request RAM retirement", round, err)
		}
		waitHASustainedApplied(t, ctx, primary, replica, round)
		if err := retire(); err != nil {
			t.Fatal("accepted completion owners", round, err)
		}
		finalized = append(finalized, request.RequestID)
		// Nothing is in flight. Releasing the accepted reserve can return its
		// blocks (v6), so fill again: at actual ENOSPC a new admission must
		// stop at the physical reserve, unobserved or not.
		if err := exhaustHAPhysicalFixture(fills[2], haPhysicalFixtureFillBound); err != nil {
			t.Fatal(round, err)
		}
		if err := unix.Statfs(root, &fs); err != nil || fs.Bavail != 0 {
			t.Fatal("idle admission would not run at ENOSPC", round, fs.Bavail, err)
		}
		if _, closeNew, err := primary.peerPhysicalTerminalAdmission(ctx, 2); err == nil {
			_ = closeNew()
			t.Fatal("new admission succeeded at actual ENOSPC with nothing in flight", round, observed)
		} else {
			if !observed && !errors.Is(err, resourcebudget.ErrPhysicalAllocation) {
				t.Fatal("unobserved ENOSPC refusal is not the physical stop", round, err)
			}
			t.Log("HA_SUSTAINED_REFUSAL_IDLE", round, observed, err)
		}
		for _, fill := range fills {
			if err := os.Remove(fill); err != nil {
				t.Fatal(round, err)
			}
		}
		if observed {
			for i, f := range pair.owners {
				if err := f.reopened.Owners.Components.Budget.SetPressure(resourcebudget.Healthy); err != nil {
					t.Fatal(round, i, err)
				}
			}
		}
		// Reading the fact is new work, refused while pressured; check it
		// after recovery (v3).
		if _, found, err := replica.reopened.Owners.Components.Ledger.ReadFinalization(ctx, request.RequestID); err != nil || !found {
			t.Fatal("Replica did not apply the accepted terminal", round, found, err)
		}
		// Neither refusal may outlive the shortage: the first admission
		// after recovery must succeed.
		if _, closeAgain, err := primary.peerPhysicalTerminalAdmission(ctx, 2); err != nil {
			t.Fatal("admission after recovery", round, err)
		} else if err := closeAgain(); err != nil {
			t.Fatal(round, err)
		}
		// v3: retirement settles asynchronously, so the fees get the fixed
		// quiescence bound to come back to the opened snapshot.
		for i, f := range pair.owners {
			waitHASustainedQuiescent(t, ctx, f, opened[i], resourcebudget.Stats{}, false)
		}
		if (round+1)%haSustainedCheckpointEvery == 0 {
			for i, f := range pair.owners {
				before := f.reopened.Seal.Revision
				deferStart := time.Now()
				for {
					err := f.checkpointHAPhysicalOnline(ctx)
					if err == nil {
						break
					}
					if !errors.Is(err, replication.ErrPhysicalCredit) || f.recovery.Load() || time.Since(deferStart) > haSustainedDeferWithin {
						t.Fatal("online checkpoint with space", round, i, time.Since(deferStart), err)
					}
					deferrals++
					time.Sleep(50 * time.Millisecond)
				}
				deferMax = max(deferMax, time.Since(deferStart))
				if f.reopened.Seal.Revision != before+1 || f.recovery.Load() {
					t.Fatal("online checkpoint did not renew in place", round, i, before, f.reopened.Seal.Revision)
				}
			}
			waitHAPhysicalCreditWindow(t, ctx, primary)
			checkpoints++
			if checkpoints == 1 {
				runtime.GC()
				var ms runtime.MemStats
				runtime.ReadMemStats(&ms)
				baselineHeap = ms.HeapAlloc
				baselineSpace = haPhysicalFixtureSpace(t)
				baselineFDs = len(sustainedOpenFDs(t))
				for i, f := range pair.owners {
					baselineStats[i] = waitHASustainedQuiescent(t, ctx, f, opened[i], resourcebudget.Stats{}, false)
				}
			}
		}
		t.Logf("HA_SUSTAINED_ROUND %d observed=%t terminal_max=%s space=%d", round, observed, terminalMax, haPhysicalFixtureSpace(t))
	}
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	if ms.HeapAlloc > baselineHeap+haSustainedHeapDrift {
		t.Fatal("live heap drifted across sustained pressure", baselineHeap, ms.HeapAlloc)
	}
	if space := haPhysicalFixtureSpace(t); space+haSustainedSpaceDrift < baselineSpace {
		t.Fatal("ext4 space drifted across sustained pressure", baselineSpace, space)
	}
	if fds := sustainedOpenFDs(t); len(fds) > baselineFDs {
		t.Fatal("FD drift across sustained pressure", baselineFDs, fds)
	}
	for i, f := range pair.owners {
		waitHASustainedQuiescent(t, ctx, f, opened[i], baselineStats[i], true)
	}
	t.Logf("HA_SUSTAINED_SUMMARY rounds=%d checkpoints=%d deferrals=%d defer_max=%s terminal_max=%s heap=%d/%d space=%d/%d elapsed=%s",
		haSustainedRounds, checkpoints, deferrals, deferMax, terminalMax, ms.HeapAlloc, baselineHeap, haPhysicalFixtureSpace(t), baselineSpace, time.Since(started))
	for i, f := range pair.owners {
		fill := filepath.Join(root, fmt.Sprintf("foreign-sustained-finish-fill-%d", i))
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
	pair.release()
	for i, cfg := range fixture.configs {
		if left := haPhysicalOwnedTemporaries(t, cfg.Storage.DataDir); len(left) != 0 {
			t.Fatal("owned temporaries survived", i, left)
		}
	}
	for _, fill := range fills[3:] {
		if err := os.Remove(fill); err != nil {
			t.Fatal(err)
		}
	}
	reopened := openHAPhysicalBusinessPair(t, ctx, fixture, true, adjust)
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

// waitHASustainedApplied is waitHAPhysicalApplied under the experiment's
// fixed bound.
func waitHASustainedApplied(t *testing.T, ctx context.Context, primary, replica *haSelectedRuntimeFoundation, round int) {
	t.Helper()
	bounded, stop := context.WithTimeout(ctx, haSustainedAppliedWithin)
	defer stop()
	through := primary.replication.publisher.Snapshot().DurableIndex
	if err := primary.replication.coordinator.WaitConfirmed(bounded, through); err != nil {
		t.Fatal("durable prefix confirmation", round, err)
	}
	for replica.replication.publisher.Snapshot().AppliedIndex < through {
		select {
		case <-bounded.Done():
			t.Fatal("Replica apply", round, bounded.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// waitHASustainedQuiescent returns a member's budget once its admission fees
// are back at the opened snapshot. With compare set, Used and the normal
// claims must also come back to the baseline within the bound: background
// owners hold steady, so anything above it is a fee the rounds left behind.
func waitHASustainedQuiescent(t *testing.T, ctx context.Context, f *haSelectedRuntimeFoundation, opened, baseline resourcebudget.Stats, compare bool) resourcebudget.Stats {
	t.Helper()
	deadline := time.Now().Add(haSustainedQuiesceWithin)
	for {
		got := f.reopened.Owners.Components.Budget.Snapshot()
		idle := got.CompletionReserved == opened.CompletionReserved && got.Classes[resourcebudget.Active] == opened.Classes[resourcebudget.Active] && got.Classes[resourcebudget.Completion] == opened.Classes[resourcebudget.Completion]
		if idle && (!compare || got.Used.RAM <= baseline.Used.RAM && got.Used.Disk <= baseline.Used.Disk && got.NormalClaims <= baseline.NormalClaims) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatal("budget did not return to its baseline", baseline, got)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}
