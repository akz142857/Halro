//go:build linux

package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"golang.org/x/sys/unix"
)

// Thresholds of the sustained-pressure experiment, fixed before any run
// (design §9.2). They are engineering screening values for a free local
// synthetic upstream, not production SLOs, and are never widened after a run.
const (
	sustainedCycles           = 24 // alternating real memory and disk pressure
	sustainedMaintenanceEvery = 3  // Roll, common checkpoint and repack
	sustainedSteadyCalls      = 32 // sequential latency samples per cycle
	sustainedBurst            = 64
	sustainedRefusals         = 8
	sustainedEnterWithin      = 5 * time.Second
	sustainedExitWithin       = 10 * time.Second
	sustainedHysteresisHold   = time.Second
	sustainedP95              = 500 * time.Millisecond
	sustainedP99              = time.Second
	sustainedHeapDrift        = 32 << 20
	// v2, registered after v1 failed on the designed projection pack
	// reclamation 503 (§26): a call shed by it may wait Retry-After and retry,
	// and each shed must clear within this bound. Any other refusal fails.
	sustainedShedWithin = 5 * time.Second
	// The cgroup peak ≤ 1.5 GiB with no max/oom event is enforced on every
	// sample by memoryLoadPoint, the same fixed acceptance as the load run.
)

// TestRuntimeSustainedPressureLinux keeps the default selected Runtime under
// repeated real OS pressure on the fixed capacity profile: each cycle runs
// steady and burst business, holds one accepted request, enters memory or
// disk pressure through the actual sampler, refuses new upstream work,
// completes the accepted terminal, and recovers. Disk cycles also check the
// hysteresis between the enter and exit lines. Maintenance cycles run
// throughout. At the end accounting, Usage, fees, FDs and live heap must show
// no drift, and a fresh process must reopen the same state.
func TestRuntimeSustainedPressureLinux(t *testing.T) {
	if os.Getenv("HALRO_RUNTIME_SUSTAINED_PRESSURE") != "1" {
		t.Skip("opt-in actual 2 GiB Linux sustained pressure experiment")
	}
	root := os.Getenv("HALRO_EXT4_ROOT")
	if root != "/halro-fixture/data" {
		t.Fatal("experiment requires the isolated ext4 runner", root)
	}
	before := memoryLoadPoint(t)
	// The fresh-process helper reads its manifest from this fixed directory.
	fixture := filepath.Join(root, "memory-load")
	if _, err := os.Lstat(fixture); !os.IsNotExist(err) {
		t.Fatal("experiment requires a new isolated fixture", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()
	legacy, credentials, _ := openBootstrappedRuntimeShaped(t, "local-synthetic", func(cfg *config.Config) {
		cfg.Storage.DataDir = filepath.Join(fixture, "data")
		cfg.Storage.MasterKey.File = filepath.Join(fixture, "master.key")
		cfg.Retry.MaxAttemptsPerTarget = 2
		// Every in-process call shares one source address; the per-source
		// limiter would answer 429 for all of them. 0 is the supported
		// setting for an install behind a shedding proxy.
		unlimited := 0
		cfg.Gateway.SourceRateLimit.RequestsPerMinute = &unlimited
	})
	cfg, instance := legacy.config, legacy.instanceID
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	limits, pressure := selectedCapacityBudget(), selectedCapacityPressure()
	cfg.ResourceBudget, cfg.ResourcePressure = &limits, &pressure
	cfg = activateClosedRuntimeMemoryStateFixtureWithPressure(t, ctx, cfg, instance, false, &pressure)
	fdBefore := memoryLoadFDs(t)
	r, err := Open(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal("default selected Runtime with actual sampler", err)
	}
	defer r.Close()
	if r.selectedState == nil || r.accountingState.projection != nil {
		t.Fatal("experiment did not open actual selected seven roots")
	}
	if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
		t.Fatal(err)
	}
	a := installMemoryLoadProvider(t, r, credentials)
	// Hundreds of calls a minute exceed the fixture Project's RPM/TPM policy,
	// whose 429s would mask pressure behavior. Lift those two rate limits
	// only; the concurrency cap and every resource limit stay as fixed.
	business, err := r.store.GetProject(ctx, credentials.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	business.RPM, business.TPM = 0, 0
	if _, err := r.store.PutProject(ctx, business, business.Revision, nil); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"chat","messages":[{"role":"user","content":"sustained"}],"max_tokens":10}`
	auth := "Bearer " + credentials.GatewayKey
	var latencies []time.Duration
	var enterTimes, exitTimes []time.Duration
	succeeded, refusedUnderPressure, burstRefused, providerCalls, sheds := 0, 0, 0, int64(0), 0
	var maxShedWait time.Duration
	project, err := r.store.PutProject(ctx, domain.Project{ID: "prj_sustained_churn", Name: "sustained metadata churn", Enabled: true, AllowedModels: []string{strings.Repeat("m", 512<<10)}}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	suspension := domain.RouteSuspension{ScopeKind: "credential", ScopeKey: "sustained-local-fixture", Reason: string(provider.FailureReasonInvalidCredential), ProviderCode: strings.Repeat("code", 128<<10), ObservedAt: time.Now().UTC(), Indefinite: true}
	fill, err := os.OpenFile(filepath.Join(root, "sustained-disk-pressure"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fill.Close(); _ = os.Remove(fill.Name()) }()
	var baselineHeap uint64
	started := time.Now()
	maintenance := 0
	for cycle := range sustainedCycles {
		// Steady business: every sequential call must succeed after recovery.
		for range sustainedSteadyCalls {
			var response memoryLoadResponse
			var took time.Duration
			shedStart := time.Now()
			for {
				begin := time.Now()
				response = serveMemoryLoadCall(r, credentials, &memoryLoadCall{}, body, auth)
				took = time.Since(begin)
				if !sustainedPackShed(response) {
					break
				}
				sheds++
				if time.Since(shedStart) > sustainedShedWithin {
					t.Fatal("projection pack shedding did not clear in time", cycle, response)
				}
				time.Sleep(time.Second)
			}
			if waited := time.Since(shedStart) - took; waited > maxShedWait {
				maxShedWait = waited
			}
			latencies = append(latencies, took)
			if response.Status != http.StatusOK || response.Calls != 1 {
				t.Fatalf("steady call failed after recovery: cycle=%d %+v monitor=%+v budget=%+v", cycle, response, r.resourcePressure.monitor.Snapshot(), r.resourcePressure.budget.Snapshot())
			}
			providerCalls += response.Calls
			succeeded++
		}
		// Burst against the unchanged finite admission cap.
		gate, results := make(chan struct{}), make(chan memoryLoadResponse, sustainedBurst)
		for range sustainedBurst {
			go func() { results <- serveMemoryLoadCall(r, credentials, &memoryLoadCall{gate: gate}, body, auth) }()
		}
		time.AfterFunc(100*time.Millisecond, func() { close(gate) })
		admitted := 0
		for range sustainedBurst {
			response := <-results
			providerCalls += response.Calls
			switch {
			case response.Status == http.StatusOK && response.Calls == 1:
				admitted++
				succeeded++
			case (response.Status == 503 || response.Status == 429) && response.Calls == 0:
				burstRefused++
			default:
				t.Fatal("unexpected burst outcome", cycle, response)
			}
		}
		if admitted == 0 {
			t.Fatal("burst made no business progress", cycle)
		}
		// Hold one accepted request across the pressure transition.
		entered, release := make(chan struct{}), make(chan struct{})
		accepted := make(chan memoryLoadResponse, 1)
		go func() {
			state := &memoryLoadCall{gate: release, entered: entered}
			deadline := time.Now().Add(sustainedShedWithin)
			for {
				response := serveMemoryLoadCall(r, credentials, state, body, auth)
				if !sustainedPackShed(response) || time.Now().After(deadline) {
					accepted <- response
					return
				}
				time.Sleep(time.Second)
			}
		}()
		select {
		case <-entered:
		case response := <-accepted:
			t.Fatal("held request was not admitted while healthy", cycle, response, r.resourcePressure.monitor.Snapshot(), r.resourcePressure.budget.Snapshot())
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		disk := cycle%2 == 1
		var buffer []byte
		begin := time.Now()
		if disk {
			setSustainedFreeBytes(t, fill, root, pressure.DiskEnterBytes/2)
		} else {
			buffer = make([]byte, 1<<30)
			for offset := 0; offset < len(buffer); offset += os.Getpagesize() {
				buffer[offset] = 1
			}
		}
		waitSustainedPressure(t, r, resourcebudget.Pressured, sustainedEnterWithin)
		enterTimes = append(enterTimes, time.Since(begin))
		for range sustainedRefusals {
			response := serveMemoryLoadCall(r, credentials, &memoryLoadCall{}, body, auth)
			if (response.Status != 503 && response.Status != 429) || response.Calls != 0 {
				t.Fatal("new upstream work admitted under actual pressure", cycle, disk, response)
			}
			refusedUnderPressure++
		}
		close(release)
		finished := <-accepted
		if finished.Status != http.StatusOK {
			t.Fatal("accepted terminal failed under actual pressure", cycle, disk, finished)
		}
		providerCalls += finished.Calls
		succeeded++
		memoryLoadPoint(t)
		begin = time.Now()
		if disk {
			// Between the enter and exit lines the state must hold.
			setSustainedFreeBytes(t, fill, root, (pressure.DiskEnterBytes+pressure.DiskExitBytes)/2)
			deadline := time.Now().Add(sustainedHysteresisHold)
			for time.Now().Before(deadline) {
				if s := r.resourcePressure.monitor.Snapshot().State; s != resourcebudget.Pressured {
					t.Fatal("disk pressure left before the exit line", cycle, s)
				}
				time.Sleep(10 * time.Millisecond)
			}
			begin = time.Now()
			if err := fill.Truncate(0); err != nil {
				t.Fatal(err)
			}
		} else {
			runtime.KeepAlive(buffer)
			buffer = nil
			debug.FreeOSMemory()
		}
		waitSustainedPressure(t, r, resourcebudget.Healthy, sustainedExitWithin)
		exitTimes = append(exitTimes, time.Since(begin))
		// Reading the fact is new work, refused while pressured; check it now.
		assertMemoryLoadTerminal(t, r, finished, 1)
		t.Logf("SUSTAINED_CYCLE %d disk=%t burst_admitted=%d enter=%s exit=%s", cycle, disk, admitted, enterTimes[len(enterTimes)-1], exitTimes[len(exitTimes)-1])
		if (cycle+1)%sustainedMaintenanceEvery == 0 {
			sustainedMaintenance(t, ctx, r, limits, &suspension, maintenance)
			maintenance++
			if maintenance == 1 {
				runtime.GC()
				baselineHeap = runtimeCapacityMemory(t).HeapAlloc
			}
		}
	}
	elapsed := time.Since(started)
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	stats, err := r.usage.disk.ReadStats(ctx)
	if err != nil || stats.Metrics.ActiveRequests != 0 {
		t.Fatal("accepted work did not close", stats, err)
	}
	key, balance := memoryLoadBalance(t, r, credentials)
	head, err := r.usage.disk.ReadReplayAnchor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	actualHead, hash, ok := r.ledger.ChainHead()
	if !ok || actualHead != head.Head || hash != head.Hash {
		t.Fatal("Usage/native cut mismatch", head, actualHead)
	}
	if stats.Totals.InputTokens != balance.InputTokens || stats.Totals.OutputTokens != balance.OutputTokens || stats.Totals.CostMicrosUSD != balance.CommittedMicrosUSD {
		t.Fatal("Usage/native exact balances differ", stats, balance)
	}
	if a.calls.Load() != providerCalls || a.active.Load() != 0 {
		t.Fatal("provider inventory differs", a.calls.Load(), providerCalls, a.active.Load())
	}
	if got := r.resourcePressure.budget.Snapshot(); got.CompletionReserved != (resourcebudget.Cost{}) || got.Classes[resourcebudget.Active] != (resourcebudget.Cost{}) || got.Classes[resourcebudget.Completion] != (resourcebudget.Cost{}) {
		t.Fatal("admitted work fees did not exit", got)
	}
	runtime.GC()
	live := memoryLoadPoint(t)
	if live.HeapAlloc > baselineHeap+sustainedHeapDrift {
		t.Fatal("live heap drifted across sustained pressure", baselineHeap, live.HeapAlloc)
	}
	slices.Sort(latencies)
	p50, p95, p99 := latencies[len(latencies)/2], latencies[len(latencies)*95/100], latencies[len(latencies)*99/100]
	if p95 > sustainedP95 || p99 > sustainedP99 {
		t.Fatal("steady latency exceeded fixed threshold", p95, p99)
	}
	manifest := memoryLoadManifest{Config: cfg, Credentials: credentials, Usage: stats, BalanceKey: key, Balance: balance, Project: project, Suspension: suspension, Head: head}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(fixture, "load-manifest.json")
	if err := os.WriteFile(manifestPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal("join actual Runtime owners", err)
	}
	if after := memoryLoadFDs(t); after > fdBefore {
		t.Fatal("actual Runtime FD leak", fdBefore, after)
	}
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRuntimeMemoryLoadFreshProcessHelper$", "-test.count=1", "-test.v")
	child.Env = append(os.Environ(), "HALRO_RUNTIME_MEMORY_LOAD_MANIFEST="+manifestPath)
	output, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("fresh process selected reopen failed: %v %s", err, output)
	}
	after := memoryLoadPoint(t)
	report := map[string]any{"scope": "local Linux sustained pressure on the fixed capacity profile; not production", "cycles": sustainedCycles, "maintenance_cycles": maintenance, "elapsed_seconds": elapsed.Seconds(),
		"succeeded": succeeded, "throughput_per_second": float64(succeeded) / elapsed.Seconds(), "burst_refused": burstRefused, "pack_sheds": sheds, "max_shed_wait_ms": maxShedWait.Seconds() * 1000, "refused_under_pressure": refusedUnderPressure, "provider_calls": providerCalls, "provider_peak": a.peak.Load(),
		"steady_p50_ms": p50.Seconds() * 1000, "steady_p95_ms": p95.Seconds() * 1000, "steady_p99_ms": p99.Seconds() * 1000, "enter_max_ms": slices.Max(enterTimes).Seconds() * 1000, "exit_max_ms": slices.Max(exitTimes).Seconds() * 1000,
		"baseline_heap_bytes": baselineHeap, "live_after_gc": live, "before": before, "after_close_and_fresh_process": after, "fd_before": fdBefore, "fd_after": memoryLoadFDs(t), "usage": stats, "balance": balance, "native_head": head}
	raw, err = json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sustained-pressure-report.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Log("RUNTIME_SUSTAINED_PRESSURE " + string(raw))
}

// sustainedMaintenance grows the Metadata pack to its repack threshold with a
// fixed node-local row, then rolls the native log and runs the common
// checkpoint, which must retire the old pack.
func sustainedMaintenance(t *testing.T, ctx context.Context, r *Runtime, limits config.ResourceBudget, suspension *domain.RouteSuspension, round int) {
	t.Helper()
	pack := "pages"
	if round%2 != 0 {
		pack = "pages.next"
	}
	old := filepath.Join(memoryStateDirectory(r.config), "metadata-root", pack)
	for updates := 0; ; updates++ {
		info, err := os.Stat(old)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() >= limits.MetadataStoreDiskBytes/2 {
			break
		}
		if updates >= 128 {
			t.Fatal("fixed metadata churn failed to reach actual repack threshold")
		}
		suspension.CredentialRevision++
		if err := r.store.PutRouteSuspension(ctx, *suspension); err != nil {
			t.Fatal("actual metadata churn", err)
		}
	}
	if _, err := r.ledger.Roll(); err != nil {
		t.Fatal("actual native Roll", err)
	}
	if err := r.CheckpointMemoryState(ctx); err != nil {
		t.Fatal("actual Roll/checkpoint/repack", round, err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("actual selected repack did not retire old pack", round, err)
	}
	memoryLoadPoint(t)
}

// setSustainedFreeBytes sizes the foreign fill so the isolated filesystem has
// about target bytes available to the sampler.
func setSustainedFreeBytes(t *testing.T, fill *os.File, root string, target int64) {
	t.Helper()
	var st unix.Statfs_t
	if err := unix.Statfs(root, &st); err != nil {
		t.Fatal(err)
	}
	info, err := fill.Stat()
	if err != nil {
		t.Fatal(err)
	}
	available := int64(st.Bavail) * st.Bsize
	size := info.Size() + available - target
	if size < 0 {
		t.Fatal("filesystem already below the target", available, target)
	}
	size -= size % st.Bsize
	if size < info.Size() {
		if err := fill.Truncate(size); err != nil {
			t.Fatal(err)
		}
	} else if err := unix.Fallocate(int(fill.Fd()), 0, 0, size); err != nil && !errors.Is(err, syscall.ENOSPC) {
		t.Fatal(err)
	}
	if err := fill.Sync(); err != nil {
		t.Fatal(err)
	}
}

// sustainedPackShed reports the projection pack reclamation refusal (§26).
func sustainedPackShed(response memoryLoadResponse) bool {
	return response.Status == http.StatusServiceUnavailable && response.Calls == 0 && response.RetryAfter == "1" && strings.Contains(response.Body, "memory state page reclamation is in progress")
}

func waitSustainedPressure(t *testing.T, r *Runtime, state resourcebudget.Pressure, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if r.resourcePressure.monitor.Snapshot().State == state {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("actual sampler did not reach the state in time", state, within, r.resourcePressure.monitor.Snapshot())
}
