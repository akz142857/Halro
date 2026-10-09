//go:build linux

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/usage"
)

// This is an opt-in capacity experiment, not an ordinary unit test. The runner
// seeds an isolated volume and reopens it in a fresh process/cgroup. No Serve,
// network requests, historical slices or production paths are involved.
func TestRuntimeCapacityLinux(t *testing.T) {
	phase := os.Getenv("HALRO_RUNTIME_CAPACITY_PHASE")
	if phase == "" {
		t.Skip("opt-in isolated Linux Runtime capacity experiment")
	}
	if phase != "seed" && phase != "restart" {
		t.Fatal("unknown capacity phase", phase)
	}
	mode := os.Getenv("HALRO_RUNTIME_CAPACITY_MODE")
	if mode == "" {
		mode = "fresh"
	}
	if mode != "fresh" && mode != "selected" {
		t.Fatal("unknown Runtime capacity mode", mode)
	}
	root := os.Getenv("HALRO_RUNTIME_CAPACITY_ROOT")
	if root != "/scratch/fixture" && !(root == "/halro-fixture/data/fixture" && os.Getenv("HALRO_EXT4_ROOT") == "/halro-fixture/data") {
		t.Fatal("capacity experiment requires its isolated runner filesystem")
	}
	count, err := strconv.Atoi(os.Getenv("HALRO_RUNTIME_CAPACITY_REQUESTS"))
	if err != nil || count < 1 || count > 1_000_000 {
		t.Fatal("invalid fixture request count")
	}
	cfg := testConfig(t)
	cfg.Storage.DataDir = filepath.Join(root, "data")
	cfg.Storage.MasterKey.File = filepath.Join(root, "master.key")
	if mode == "selected" {
		limits, pressure := selectedCapacityBudget(), selectedCapacityPressure()
		if err := limits.Validate(&pressure, cfg.Server.MaxRequestBytes); err != nil {
			t.Fatal("fixed selected capacity profile is not a deployable configuration", err)
		}
	}
	if mode == "selected" && phase == "restart" {
		limits, pressure := selectedCapacityBudget(), selectedCapacityPressure()
		cfg.ResourceBudget, cfg.ResourcePressure = &limits, &pressure
	}
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manifestPath := filepath.Join(root, "fixture.json")
	manifest := struct {
		Requests int       `json:"requests"`
		Start    time.Time `json:"start"`
	}{Requests: count, Start: time.Now().UTC().Truncate(24 * time.Hour).Add(time.Hour)}
	if phase == "seed" {
		if _, err := os.Lstat(root); !os.IsNotExist(err) {
			t.Fatal("seed requires a new volume", err)
		}
		if err := Initialize(cfg); err != nil {
			t.Fatal(err)
		}
	} else {
		raw, err := os.ReadFile(manifestPath)
		if err != nil || json.Unmarshal(raw, &manifest) != nil || manifest.Requests != count {
			t.Fatal("missing/mismatched complete seed", err)
		}
	}
	before := runtimeCapacityMemory(t)
	// The child sampler owns its points until joined; report data is never read
	// concurrently. Maxima are sampled per-field, not simultaneous measurements.
	stop, joined := make(chan struct{}), make(chan struct{})
	peak := before
	go func() {
		defer close(joined)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				var mem runtime.MemStats
				runtime.ReadMemStats(&mem)
				peak.HeapAlloc = max(peak.HeapAlloc, mem.HeapAlloc)
				peak.HeapInuse = max(peak.HeapInuse, mem.HeapInuse)
			}
		}
	}()
	var stopOnce sync.Once
	stopSampler := func() { stopOnce.Do(func() { close(stop); <-joined }) }
	defer stopSampler()
	var startupAuditElapsed, migrationReopenElapsed time.Duration
	started := time.Now()
	r, err := Open(ctx, cfg, logger)
	openElapsed := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	if mode == "selected" && phase == "restart" {
		if r.selectedState == nil || r.accountingState.projection != nil || r.accountingState.reader != r.selectedState.generation.Owners.Ledger {
			t.Fatal("restart did not select authenticated seven-root generation")
		}
		auditStarted := time.Now()
		if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
			t.Fatal(err)
		}
		startupAuditElapsed += time.Since(auditStarted)
	} else if r.accountingState.projection == nil || r.accountingState.reader != r.accountingState.projection || r.usage.disk == nil || r.usage.owner == nil || r.usage.owner.derivative == nil || r.usage.owner.native != r.ledger {
		t.Fatal("default Runtime did not select disk accounting and native-backed disk Usage")
	}
	seedStages := map[string]int64{}
	if phase == "seed" {
		r.stopBackgroundWork()
		// Join the ordinary shutdown export, then establish the same published
		// empty archive before any facts. Background startup scheduling must
		// not choose different backup verification branches between arms.
		if mode == "selected" {
			if _, err := r.usageExporter.ExportDiskStep(ctx, r.usage.disk, usage.DefaultDiskExportStepLimits()); err != nil {
				t.Fatal("fixed empty archive precondition", err)
			}
			pos, err := r.usageExporter.ReadArchivePosition(ctx)
			if err != nil || pos.SchemaVersion <= 0 || pos.LastSequence != 0 || pos.Files != 0 {
				t.Fatal("empty archive precondition differs", pos, err)
			}
		}
		seedStarted := time.Now()
		kinds := [...]ledger.EventKind{ledger.EventRequestAccepted, ledger.EventReservationCreated, ledger.EventAttemptStarted, ledger.EventAttemptSettled, ledger.EventRequestFinalized}
		appendRequest := func(i int) error {
			base := manifest.Start.Add(time.Duration(i) * 10 * time.Millisecond)
			for step, kind := range kinds {
				event := ledger.Event{EventID: fmt.Sprintf("capacity_%09d_%d", i, step), Kind: kind,
					RequestID: fmt.Sprintf("capacity_req_%09d", i), AttemptID: fmt.Sprintf("capacity_att_%09d", i),
					ProjectID: "prj_capacity", KeyID: "key_capacity", RouteID: "route_capacity", DeploymentID: "dep_capacity", ProviderID: "provider_capacity",
					RequestedModel: "synthetic", ProviderModel: "synthetic", PeriodID: base.Format("2006-01-02"), PeriodTimezone: "UTC", PeriodTimezoneVersion: 1,
					PeriodStartMicros: manifest.Start.Truncate(24 * time.Hour).UnixMicro(), PeriodEndMicros: manifest.Start.Truncate(24 * time.Hour).Add(24 * time.Hour).UnixMicro(),
					OccurredAt: base.Add(time.Duration(step) * time.Millisecond)}
				switch kind {
				case ledger.EventRequestAccepted, ledger.EventRequestFinalized:
					event.AttemptID = ""
				case ledger.EventReservationCreated:
					event.ReservationMicrosUSD = ledger.MicrosUSD(100)
				case ledger.EventAttemptSettled:
					event.CommittedMicrosUSD = ledger.MicrosUSD(80)
					event.ProviderInputTokens, event.ProviderOutputTokens = 7, 3
					event.TokenUsageSource = ledger.TokenUsageSourceProvider
					event.AttemptNumber, event.LatencyMillis = 1, 3
				}
				if kind == ledger.EventAttemptSettled || kind == ledger.EventRequestFinalized {
					event.Outcome = "success"
				}
				if _, err := r.ledger.AppendWithReceipt(ctx, event); err != nil {
					return fmt.Errorf("request %d step %d: %w", i, step, err)
				}
			}
			return nil
		}
		// Bound preparation to 64 live requests. Every request retains its
		// five durable receipt barriers; the normal native writer can group
		// independent requests into its existing finite fsync batches.
		for first := 0; first < count; first += 64 {
			results := make(chan error, 64)
			last := min(first+64, count)
			for i := first; i < last; i++ {
				go func(i int) { results <- appendRequest(i) }(i)
			}
			var joinedErr error
			for i := first; i < last; i++ {
				joinedErr = errors.Join(joinedErr, <-results)
			}
			if joinedErr != nil {
				t.Fatal(joinedErr)
			}
			if mode == "selected" && last%4096 == 0 {
				lease, err := r.ledger.BeginMaintenance(ctx)
				if err != nil {
					t.Fatal(err)
				}
				cacheErr := r.ledger.PrepareColdMaintenanceRead(ctx, lease)
				if err := errors.Join(cacheErr, lease.Close()); err != nil {
					t.Fatal(err)
				}
			}
		}
		seedStages["native_append"] = time.Since(seedStarted).Nanoseconds()
		t.Log("seed native append joined", seedStages["native_append"])
		if mode != "selected" {
			seedStarted = time.Now()
			lease, err := r.ledger.BeginMaintenance(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := lease.Close(); err != nil {
					t.Error(err)
				}
			}()
			// Authenticate native replay and use the public finite projection
			// barrier. The fixture never installs fabricated balances or roots.
			records := make([]ledger.Record, 0, 64)
			flush := func() error {
				if len(records) == 0 {
					return nil
				}
				err := r.accountingState.projection.ApplyBatch(ctx, records)
				clear(records)
				records = records[:0]
				return err
			}
			if _, partial, err := ledger.InspectReplayAuthenticatedContext(ctx, cfg.LedgerPath(), r.usage.owner.nativeKey[:], func(record ledger.Record) error {
				records = append(records, record)
				if len(records) == cap(records) {
					return flush()
				}
				return nil
			}); err != nil || partial {
				t.Fatal("incomplete authenticated fixture replay", partial, err)
			}
			if err := flush(); err != nil {
				t.Fatal(err)
			}
			seedStages["accounting_replay"] = time.Since(seedStarted).Nanoseconds()
			t.Log("seed accounting replay joined", seedStages["accounting_replay"])
			seedStarted = time.Now()
			// Preparation uses the actual existing cold Runtime Usage path. The
			// process has no business traffic and its background workers are joined.
			// The ordinary collector below still checks the finished native cut.
			if _, err := replayDiskUsageBatches(ctx, r.ledger, ledger.ReplayAnchor{}, r.usage.owner.derivative, nil); err != nil {
				t.Fatal(err)
			}
			seedStages["usage_cold_replay"] = time.Since(seedStarted).Nanoseconds()
			t.Log("seed Usage replay joined", seedStages["usage_cold_replay"])
			if err := lease.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	var migrationElapsed, migrationBudget time.Duration
	if phase == "seed" && mode == "selected" {
		// Seed facts using the actual native writer, then measure the real new
		// migration. Rebuilding two obsolete whole-mmap derived DBs first would
		// measure a different engine and contaminate this selected capacity run.
		instanceID := r.instanceID
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		migrationCtx := ctx
		cancelMigration := func() {}
		if raw := os.Getenv("HALRO_RUNTIME_CAPACITY_MIGRATION_TIMEOUT"); raw != "" {
			var parseErr error
			migrationBudget, parseErr = time.ParseDuration(raw)
			if parseErr != nil || migrationBudget <= 0 {
				t.Fatal("invalid explicit migration budget", raw, parseErr)
			}
			migrationCtx, cancelMigration = context.WithTimeout(ctx, migrationBudget)
		}
		defer cancelMigration()
		migrationStarted := time.Now()
		limits, pressure := selectedCapacityBudget(), selectedCapacityPressure()
		cfg.ResourceBudget, cfg.ResourcePressure = &limits, &pressure
		activateClosedRuntimeMemoryStateFixtureWithPressure(t, migrationCtx, cfg, instanceID, false, &pressure)
		cancelMigration()
		migrationElapsed = time.Since(migrationStarted)
		reopenStarted := time.Now()
		r, err = Open(ctx, cfg, logger)
		migrationReopenElapsed = time.Since(reopenStarted)
		if err != nil {
			t.Fatal("actual selected reopen after cold migration", err)
		}
		if r.selectedState == nil {
			t.Fatal("cold migration did not select actual seven-root generation")
		}
		auditStarted := time.Now()
		if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
			t.Fatal(err)
		}
		startupAuditElapsed += time.Since(auditStarted)
	}
	reconcileStarted := time.Now()
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	head, hash, ok := r.ledger.ChainHead()
	position, projectionHash, err := r.accountingState.reader.ReadPosition(ctx)
	stats, statsErr := r.usage.disk.ReadStats(ctx)
	anchor, anchorErr := r.usage.disk.ReadReplayAnchor(ctx)
	if !ok || err != nil || statsErr != nil || anchorErr != nil || head.Sequence != uint64(count*5) || position != head || stats.Watermark != head || anchor.Head != head || projectionHash != hash || anchor.Hash != hash {
		t.Fatal("rebuilt prefix differs from authenticated native head", head, position, stats.Watermark, err, statsErr, anchorErr)
	}
	balance, err := r.accountingState.reader.ReadBalance(ctx, ledger.BalanceKey{ProjectID: "prj_capacity", PeriodID: manifest.Start.Format("2006-01-02"), TimezoneVersion: 1})
	if err != nil || balance.ReservedMicrosUSD != 0 || balance.CommittedMicrosUSD != int64(80*count) || balance.InputTokens != int64(7*count) || balance.OutputTokens != int64(3*count) {
		t.Fatal("rebuilt balance differs", balance, err)
	}
	if stats.Totals.Requests != int64(count) || stats.Totals.Attempts != int64(count) || stats.Totals.CostMicrosUSD != int64(80*count) || stats.Metrics.ActiveRequests != 0 || stats.Metrics.RequestsSuccess != uint64(count) || stats.Metrics.AttemptsSuccess != uint64(count) {
		t.Fatal("rebuilt Usage differs", stats)
	}
	for _, index := range []int{0, count - 1} {
		id := fmt.Sprintf("capacity_req_%09d", index)
		reader, ok := r.accountingState.reader.(interface {
			ReadRequest(context.Context, string) (ledger.ProjectedRequest, bool, error)
		})
		if !ok {
			t.Fatal("capacity owner lacks exact request evidence")
		}
		request, found, err := reader.ReadRequest(ctx, id)
		if err != nil || !found || request.Finalized == nil {
			t.Fatal("missing final request", id, err)
		}
		if _, found, err := r.usage.disk.ReadRequest(ctx, id); err != nil || !found {
			t.Fatal("missing Usage request", id, err)
		}
	}
	reconcileElapsed := time.Since(reconcileStarted)
	// Keep all actual owners alive through GC. The normal restart keeps default
	// background workers running until Close; seed alone stops them for fixture IO.
	runtime.GC()
	after := runtimeCapacityMemory(t)
	runtime.KeepAlive(r)
	stopSampler()
	peak.HeapAlloc = max(peak.HeapAlloc, after.HeapAlloc)
	peak.HeapInuse = max(peak.HeapInuse, after.HeapInuse)
	report := map[string]any{"phase": phase, "requests": count, "events": count * 5,
		"startup_audit_ns": startupAuditElapsed.Nanoseconds(), "reconcile_ns": reconcileElapsed.Nanoseconds(), "migration_reopen_ns": migrationReopenElapsed.Nanoseconds(), "seed_archive_precondition": "published-empty-before-native",
		"runtime_mode": mode, "authenticated_selected": r.selectedState != nil,
		"open_ns": openElapsed.Nanoseconds(), "phase_ns": time.Since(started).Nanoseconds(), "before": before, "after_gc": after,
		"sampled_heap_peak_bytes": peak.HeapAlloc, "sampled_heap_inuse_peak_bytes": peak.HeapInuse,
		"allocated_bytes": after.TotalAlloc - before.TotalAlloc, "native_head": head, "usage": stats,
		"balance": balance, "resource_budget_configured": cfg.ResourceBudget != nil, "resource_pressure_configured": cfg.ResourcePressure != nil,
		"go_version": runtime.Version(), "gomaxprocs": runtime.GOMAXPROCS(0), "gomemlimit": os.Getenv("GOMEMLIMIT"),
		"fixture": "legacy-unpriced-success-v1", "seed_max_live_requests": 64, "seed_projection_batch_rows": 64, "seed_usage_replay": "existing-runtime-cold-batch", "seed_stage_ns": seedStages, "restart_background_workers": phase == "restart"}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if phase == "seed" && mode == "selected" {
		limits := selectedCapacityBudget()
		pressure := selectedCapacityPressure()
		report["migration_ns"] = migrationElapsed.Nanoseconds()
		report["migration_budget_ns"] = migrationBudget.Nanoseconds()
		report["seed_legacy_projection_rebuild"] = false
		report["migration_real_pressure"] = true
		report["migration_resource_budget"] = limits
		report["migration_resource_pressure"] = pressure
	}
	// Include actual shutdown and all joined physical owners in the cgroup peak.
	// This is a fresh cgroup, so memory.peak covers the entire phase even after
	// the Go sampler stopped; counters also detect pressure without a killed PID.
	report["after_close"] = runtimeCapacityMemory(t)
	if phase == "seed" {
		raw, _ := json.Marshal(manifest)
		if err := os.WriteFile(manifestPath, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(root), phase+".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Log("RUNTIME_CAPACITY " + string(raw))
}

// Capacity runs use the actual OS/cgroup sampler; the tiny controlled samples
// in functional fixtures must never authorize this physical-memory experiment.
func selectedCapacityPressure() config.ResourcePressure {
	return config.ResourcePressure{RuntimeEnterBytes: 1 << 30, RuntimeExitBytes: 900 << 20,
		CgroupEnterBytes: 1536 << 20, CgroupExitBytes: 1280 << 20,
		HeadroomEnterBytes: 256 << 20, HeadroomExitBytes: 512 << 20,
		DiskEnterBytes: 128 << 20, DiskExitBytes: 256 << 20, InodesEnter: 100, InodesExit: 200,
		MaxAge: config.Duration(2 * time.Second), Cooldown: config.Duration(time.Nanosecond), SampleInterval: config.Duration(100 * time.Millisecond)}
}

func selectedCapacityBudget() config.ResourceBudget {
	b := selectedRuntimeBudgetFixture()
	b.MemoryStateRequired = true
	// One fixed profile for every history sample, including 50,000 native
	// facts. Disk capacity is explicit; the physical cgroup and peak acceptance
	// ceilings stay 2 GiB / 1.5 GiB. The 64 MiB small functional fixture cannot
	// hold the full 10,000-request scratch/row namespace.
	b.TotalRAMBytes, b.RecoveryRAMBytes, b.RecoveryReserveRAMBytes = 2<<30, 1<<30, 1<<30
	b.BackgroundRAMBytes = 1 << 30
	// Disk capacities are declared once for the million-request profile. They
	// are logical ceilings, not allocations; the same fixed RAM/OS thresholds
	// apply at every history size, including scratch+old/new coexistence.
	b.TotalDiskBytes, b.BackgroundDiskBytes = 192<<30, 96<<30
	b.RecoveryDiskBytes, b.RecoveryReserveDiskBytes = 96<<30, 96<<30
	b.IODiskBytes, b.BackupWorkspaceDiskBytes = 8<<30, 8<<30
	b.LedgerAppendDiskBytes = 8 << 30
	b.LedgerProjectionDiskBytes, b.UsageProjectionDiskBytes = 8<<30, 8<<30
	b.PersistentInventoryDiskBytes = b.MinimumPersistentInventoryDiskBytes() + 8<<30
	return b
}

type runtimeCapacityPoint struct {
	HeapAlloc     uint64            `json:"heap_alloc_bytes"`
	HeapInuse     uint64            `json:"heap_inuse_bytes"`
	Sys           uint64            `json:"runtime_sys_bytes"`
	TotalAlloc    uint64            `json:"total_alloc_bytes"`
	RSS           uint64            `json:"rss_bytes"`
	CgroupCurrent uint64            `json:"cgroup_current_bytes"`
	CgroupPeak    uint64            `json:"cgroup_peak_bytes"`
	CgroupMax     string            `json:"cgroup_max"`
	CgroupStat    map[string]uint64 `json:"cgroup_stat"`
	CgroupEvents  map[string]uint64 `json:"cgroup_events"`
}

func runtimeCapacityMemory(t *testing.T) runtimeCapacityPoint {
	t.Helper()
	read := func(path string) string {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(raw))
	}
	number := func(value string) uint64 {
		n, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	values := func(path string) map[string]uint64 {
		result := make(map[string]uint64)
		for _, line := range strings.Split(read(path), "\n") {
			parts := strings.Fields(line)
			if len(parts) != 2 {
				t.Fatal("invalid cgroup counters", path)
			}
			result[parts[0]] = number(parts[1])
		}
		return result
	}
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	point := runtimeCapacityPoint{HeapAlloc: mem.HeapAlloc, HeapInuse: mem.HeapInuse, Sys: mem.Sys, TotalAlloc: mem.TotalAlloc,
		CgroupCurrent: number(read("/sys/fs/cgroup/memory.current")), CgroupPeak: number(read("/sys/fs/cgroup/memory.peak")),
		CgroupMax: read("/sys/fs/cgroup/memory.max"), CgroupStat: values("/sys/fs/cgroup/memory.stat"), CgroupEvents: values("/sys/fs/cgroup/memory.events")}
	for _, line := range strings.Split(read("/proc/self/status"), "\n") {
		parts := strings.Fields(line)
		if len(parts) == 3 && parts[0] == "VmRSS:" && parts[2] == "kB" {
			point.RSS = number(parts[1]) * 1024
		}
	}
	if point.RSS == 0 {
		t.Fatal("missing process RSS")
	}
	return point
}
