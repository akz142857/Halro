package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/config"
)

// The memory-bounded redesign's fixed probe (docs/todo/memory-bounded-redesign.zh-CN.md
// §6, phase P0). It grows the accounting history to each requested size and,
// at every size, measures what the budgets are written against: live heap
// after GC, cold-start time, request throughput and latency at concurrency 1,
// 16 and 64, and how far the usage history index trails the Ledger. Every later phase reruns this same probe, so its
// shape is fixed here rather than reinvented per phase.
//
// Requests take the default Gateway path's accounting lifecycle — begin,
// reserve, start, settle, finalize — which is what writes the Ledger and feeds
// the Usage aggregates. Run governance is opt-in per Project and is not
// exercised. All history lands inside the console window, the worst case for
// the window-resident aggregates.
//
//	HALRO_MEMORY_BASELINE=<output dir> go test ./internal/app -run TestMemoryBaseline -timeout 6h -v
//
// HALRO_MEMORY_BASELINE_SIZES (default 10000,100000,1000000) and
// HALRO_MEMORY_BASELINE_WINDOW (default 10s per concurrency level) shape it.
// Results go to <output dir>/results.jsonl, with a heap profile and a CPU
// profile of the reopen per size.

const (
	memoryBaselineProjects = 16
	memoryBaselineKeys     = 64
	memoryBaselineModels   = 4
)

type memoryBaselineLevel struct {
	Concurrency       int     `json:"concurrency"`
	Requests          int64   `json:"requests"`
	Throughput        float64 `json:"requests_per_second"`
	P50Ms             float64 `json:"p50_ms"`
	P99Ms             float64 `json:"p99_ms"`
	LedgerSyncsPerReq float64 `json:"ledger_syncs_per_request"`
	// HistoryMaxLagSeconds is how far the usage history index trailed the
	// Ledger head at worst during the window, in seconds of Ledger writes.
	HistoryMaxLagSeconds float64 `json:"history_max_lag_seconds"`
	// What writing the history index cost during the window: batch
	// transactions, their mean duration with fsync, and pages per batch.
	HistoryCommits        uint64  `json:"history_commits"`
	HistoryCommitMeanMs   float64 `json:"history_commit_mean_ms"`
	HistoryPagesPerCommit float64 `json:"history_pages_per_commit"`
	HistoryFreePages      int     `json:"history_free_pages"`
	HistoryFileBytes      int64   `json:"history_file_bytes"`
}

type memoryBaselineResult struct {
	History       int64   `json:"history_requests"`
	FillPerSecond float64 `json:"fill_requests_per_second"`
	OpenSeconds   float64 `json:"open_seconds"`
	// OpenFromSnapshot reports that the timed start resumed from the
	// accounting state snapshot rather than replaying the whole Ledger.
	OpenFromSnapshot bool `json:"open_from_snapshot"`
	// ActiveLedgerBytes is the generation Open reads end to end.
	ActiveLedgerBytes int64 `json:"active_ledger_bytes"`
	// RebuildSeconds and RebuildHeapAllocBytes time a start with the
	// snapshot removed — the first start after an upgrade, or after a
	// snapshot is set aside — and the heap it leaves.
	RebuildSeconds        float64               `json:"rebuild_seconds"`
	RebuildHeapAllocBytes uint64                `json:"rebuild_heap_alloc_bytes"`
	HeapAllocBytes        uint64                `json:"heap_alloc_bytes"`
	HeapInuseBytes        uint64                `json:"heap_inuse_bytes"`
	SysBytes              uint64                `json:"sys_bytes"`
	LedgerBytes           int64                 `json:"ledger_bytes"`
	DataDirBytes          int64                 `json:"data_dir_bytes"`
	Levels                []memoryBaselineLevel `json:"levels"`
}

func TestMemoryBaseline(t *testing.T) {
	out := os.Getenv("HALRO_MEMORY_BASELINE")
	if out == "" {
		t.Skip("set HALRO_MEMORY_BASELINE=<output dir> to run the memory baseline probe")
	}
	sizes := []int64{10_000, 100_000, 1_000_000}
	if raw := os.Getenv("HALRO_MEMORY_BASELINE_SIZES"); raw != "" {
		sizes = sizes[:0]
		for _, field := range strings.Split(raw, ",") {
			size, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
			if err != nil || size <= 0 {
				t.Fatalf("HALRO_MEMORY_BASELINE_SIZES: %q is not a positive integer", field)
			}
			sizes = append(sizes, size)
		}
	}
	window := 10 * time.Second
	if raw := os.Getenv("HALRO_MEMORY_BASELINE_WINDOW"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			t.Fatalf("HALRO_MEMORY_BASELINE_WINDOW: %q is not a positive duration", raw)
		}
		window = parsed
	}
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatal(err)
	}
	results, err := os.Create(filepath.Join(out, "results.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer results.Close()

	root := t.TempDir()
	cfg := crashConfig(root)
	cfg.Usage.Durability = config.Default().Usage.Durability
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r, err := Open(ctx, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { r.Close() }()

	var sequence atomic.Int64
	for _, size := range sizes {
		var result memoryBaselineResult
		if fill := size - sequence.Load(); fill > 0 {
			started := time.Now()
			memoryBaselineDrive(t, r, &sequence, 64, fill, 0)
			result.FillPerSecond = float64(fill) / time.Since(started).Seconds()
		}
		result.History = sequence.Load()

		if err := r.Close(); err != nil {
			t.Fatalf("close at %d: %v", result.History, err)
		}
		cpuProfile, err := os.Create(filepath.Join(out, fmt.Sprintf("cpu-open-%d.pb.gz", result.History)))
		if err != nil {
			t.Fatal(err)
		}
		if err := pprof.StartCPUProfile(cpuProfile); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		r, err = Open(ctx, cfg, logger)
		result.OpenSeconds = time.Since(started).Seconds()
		pprof.StopCPUProfile()
		cpuProfile.Close()
		if err != nil {
			t.Fatalf("reopen at %d: %v", result.History, err)
		}
		result.OpenFromSnapshot, result.ActiveLedgerBytes = r.accountingState.fromSnapshot, r.ledger.ActiveBytes()

		runtime.GC()
		runtime.GC()
		var memory runtime.MemStats
		runtime.ReadMemStats(&memory)
		result.HeapAllocBytes, result.HeapInuseBytes, result.SysBytes = memory.HeapAlloc, memory.HeapInuse, memory.Sys
		memoryBaselineHeapProfile(t, filepath.Join(out, fmt.Sprintf("heap-%d.pb.gz", result.History)))
		result.LedgerBytes = memoryBaselineDirBytes(filepath.Dir(cfg.LedgerPath()))
		result.DataDirBytes = memoryBaselineDirBytes(cfg.Storage.DataDir)

		for _, concurrency := range []int{1, 16, 64} {
			syncsBefore := r.ledger.Stats().Syncs
			writesBefore := r.usage.History().WriteStats()
			headBefore, _, _ := r.ledger.ChainHead()
			stopSampling := make(chan struct{})
			sampled := make(chan uint64)
			go func() {
				var maxLag uint64
				ticker := time.NewTicker(100 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-ticker.C:
						head, _, _ := r.ledger.ChainHead()
						if committed := r.usage.HistoryStats().Committed.Sequence; head.Sequence > committed {
							maxLag = max(maxLag, head.Sequence-committed)
						}
					case <-stopSampling:
						sampled <- maxLag
						return
					}
				}
			}()
			latencies := memoryBaselineDrive(t, r, &sequence, concurrency, 0, window)
			close(stopSampling)
			maxLagRecords := <-sampled
			headAfter, _, _ := r.ledger.ChainHead()
			level := memoryBaselineLevel{
				Concurrency: concurrency,
				Requests:    int64(len(latencies)),
				Throughput:  float64(len(latencies)) / window.Seconds(),
			}
			if len(latencies) > 0 {
				slices.Sort(latencies)
				level.P50Ms = float64(latencies[len(latencies)/2]) / float64(time.Millisecond)
				level.P99Ms = float64(latencies[len(latencies)*99/100]) / float64(time.Millisecond)
				level.LedgerSyncsPerReq = float64(r.ledger.Stats().Syncs-syncsBefore) / float64(len(latencies))
			}
			if rate := float64(headAfter.Sequence-headBefore.Sequence) / window.Seconds(); rate > 0 {
				level.HistoryMaxLagSeconds = float64(maxLagRecords) / rate
			}
			writesAfter := r.usage.History().WriteStats()
			if commits := writesAfter.Commits - writesBefore.Commits; commits > 0 {
				level.HistoryCommits = commits
				level.HistoryCommitMeanMs = (writesAfter.CommitSeconds - writesBefore.CommitSeconds) * 1000 / float64(commits)
				level.HistoryPagesPerCommit = float64(writesAfter.PageWrites-writesBefore.PageWrites) / float64(commits)
			}
			level.HistoryFreePages, level.HistoryFileBytes = writesAfter.FreePages, writesAfter.FileBytes
			result.Levels = append(result.Levels, level)
		}

		// The rebuild comes last, so the windows above ran on the start that
		// resumed from the snapshot.
		if err := r.Close(); err != nil {
			t.Fatalf("close before rebuild at %d: %v", result.History, err)
		}
		if err := os.Remove(cfg.LedgerStatePath()); err != nil {
			t.Fatalf("remove snapshot at %d: %v", result.History, err)
		}
		started = time.Now()
		r, err = Open(ctx, cfg, logger)
		result.RebuildSeconds = time.Since(started).Seconds()
		if err != nil {
			t.Fatalf("rebuild at %d: %v", result.History, err)
		}
		if r.accountingState.fromSnapshot {
			t.Fatalf("rebuild at %d resumed from a snapshot", result.History)
		}
		runtime.GC()
		runtime.GC()
		runtime.ReadMemStats(&memory)
		result.RebuildHeapAllocBytes = memory.HeapAlloc

		line, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintf(results, "%s\n", line); err != nil {
			t.Fatal(err)
		}
		t.Logf("history=%d snapshot=%t active=%dMiB rebuild=%.2fs rebuild-heap=%.1fMiB", result.History, result.OpenFromSnapshot,
			result.ActiveLedgerBytes>>20, result.RebuildSeconds, float64(result.RebuildHeapAllocBytes)/(1<<20))
		t.Logf("history=%d heap=%.1fMiB open=%.2fs fill=%.0f/s c1=%.0f/s c16=%.0f/s c64=%.0f/s p99(c64)=%.1fms lag(c64)=%.2fs commit(c1)=%.1fms pages(c1)=%.0f free=%d file=%dMiB",
			result.History, float64(result.HeapAllocBytes)/(1<<20), result.OpenSeconds, result.FillPerSecond,
			result.Levels[0].Throughput, result.Levels[1].Throughput, result.Levels[2].Throughput, result.Levels[2].P99Ms,
			result.Levels[2].HistoryMaxLagSeconds, result.Levels[0].HistoryCommitMeanMs, result.Levels[0].HistoryPagesPerCommit,
			result.Levels[0].HistoryFreePages, result.Levels[0].HistoryFileBytes>>20)
	}
}

// memoryBaselineDrive bills requests from concurrency workers until count
// requests are done (count > 0) or the duration elapses, and returns each
// request's latency. Any accounting error fails the probe: a baseline that
// silently counted refusals as throughput would be measuring the wrong thing.
func memoryBaselineDrive(t *testing.T, r *Runtime, sequence *atomic.Int64, concurrency int, count int64, duration time.Duration) []time.Duration {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if duration > 0 {
		ctx, cancel = context.WithTimeout(ctx, duration)
		defer cancel()
	}
	var issued atomic.Int64
	var mu sync.Mutex
	var latencies []time.Duration
	var failure error
	var wg sync.WaitGroup
	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]time.Duration, 0, 1024)
			defer func() {
				mu.Lock()
				latencies = append(latencies, local...)
				mu.Unlock()
			}()
			for ctx.Err() == nil && (count == 0 || issued.Add(1) <= count) {
				index := sequence.Add(1)
				started := time.Now()
				if err := memoryBaselineRequest(r, index); err != nil {
					if ctx.Err() != nil {
						return
					}
					mu.Lock()
					if failure == nil {
						failure = err
					}
					mu.Unlock()
					cancel()
					return
				}
				local = append(local, time.Since(started))
			}
		}()
	}
	wg.Wait()
	if failure != nil {
		t.Fatal(failure)
	}
	return latencies
}

func memoryBaselineRequest(r *Runtime, index int64) error {
	ctx := context.Background()
	project := fmt.Sprintf("prj_baseline_%02d", index%memoryBaselineProjects)
	key := fmt.Sprintf("key_baseline_%02d", index%memoryBaselineKeys)
	model := fmt.Sprintf("model_%d", index%memoryBaselineModels)
	request, err := r.accounting.BeginRequestDetailed(ctx, project, key, fmt.Sprintf("req_baseline_%09d", index), "chat")
	if err != nil {
		return fmt.Errorf("begin %d: %w", index, err)
	}
	attempt, err := r.accounting.ReserveAttemptDetailed(ctx, request, 1_000_000_000, 100, budget.AttemptMetadata{
		RouteID: "route_baseline", ProviderID: "provider_baseline", ProviderModel: model, AttemptNumber: 1,
	})
	if err != nil {
		return fmt.Errorf("reserve %d: %w", index, err)
	}
	if err := r.accounting.MarkStarted(ctx, attempt); err != nil {
		return fmt.Errorf("start %d: %w", index, err)
	}
	if err := r.accounting.Settle(ctx, attempt, budget.Settlement{
		CommittedMicrosUSD: 90, ProviderInputTokens: 700, ProviderOutputTokens: 300,
		Outcome: "success", LatencyMillis: 120,
	}); err != nil {
		return fmt.Errorf("settle %d: %w", index, err)
	}
	if err := r.accounting.Finalize(ctx, request, "success"); err != nil {
		return fmt.Errorf("finalize %d: %w", index, err)
	}
	return nil
}

func memoryBaselineHeapProfile(t *testing.T, path string) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := pprof.Lookup("heap").WriteTo(file, 0); err != nil {
		t.Fatal(err)
	}
}

func memoryBaselineDirBytes(root string) int64 {
	var total int64
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}
