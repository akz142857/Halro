package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// Board §49: the registered selected-engine capacity probe at 2 GiB. It runs
// only with HALRO_SELECTED_CAPACITY_PROBE=1, on Linux ext4 inside a 2 GiB
// cgroup, against the real pressure sampler.
func selectedCapacityProbeBudget() config.ResourceBudget {
	b := selectedRuntimeBudgetFixture()
	b.MemoryStateRequired = true
	b.TotalRAMBytes, b.RecoveryRAMBytes, b.RecoveryReserveRAMBytes = 2<<30, 1<<30, 1<<30
	b.BackgroundRAMBytes = 1 << 30
	b.IORAMBytes = 1<<30 - 64<<20
	b.LedgerProjectionDiskBytes, b.UsageProjectionDiskBytes = 256<<20, 256<<20
	b.TotalDiskBytes, b.BackgroundDiskBytes, b.RecoveryDiskBytes, b.RecoveryReserveDiskBytes, b.IODiskBytes = 32<<30, 16<<30, 8<<30, 8<<30, 4<<30
	b.PersistentInventoryDiskBytes = b.MinimumPersistentInventoryDiskBytes() + 4<<30
	return b
}

func selectedCapacityProbePressure() config.ResourcePressure {
	return config.ResourcePressure{RuntimeEnterBytes: 1 << 30, RuntimeExitBytes: 900 << 20, CgroupEnterBytes: 1536 << 20, CgroupExitBytes: 1280 << 20, HeadroomEnterBytes: 64 << 20, HeadroomExitBytes: 128 << 20, DiskEnterBytes: 128 << 20, DiskExitBytes: 256 << 20, InodesEnter: 100, InodesExit: 200, MaxAge: config.Duration(time.Minute), Cooldown: config.Duration(time.Second), SampleInterval: config.Duration(100 * time.Millisecond)}
}

type selectedCapacityLevel struct {
	Concurrency      int     `json:"concurrency"`
	Requests         int     `json:"requests"`
	Completed        int     `json:"completed"`
	Rejected         int     `json:"rejected"`
	Errors           int     `json:"errors"`
	FirstError       string  `json:"first_error,omitempty"`
	Seconds          float64 `json:"seconds"`
	Throughput       float64 `json:"requests_per_second"`
	P50Ms            float64 `json:"p50_ms"`
	P99Ms            float64 `json:"p99_ms"`
	MaxMs            float64 `json:"max_ms"`
	Pauses           int     `json:"checkpoint_pauses"`
	MaxPauseMs       float64 `json:"max_checkpoint_pause_ms"`
	LedgerRootBytes  int64   `json:"ledger_root_bytes"`
	UsageRootBytes   int64   `json:"usage_root_bytes"`
	NativeLedgerByte int64   `json:"native_ledger_bytes"`
}

// selectedCapacityAdmit is the business request gate of withMemoryCheckpointRequest:
// a request that meets a checkpoint pause waits and retries, as a client
// honouring its Retry-After would.
func selectedCapacityAdmit(ctx context.Context, r *Runtime) (func(), error) {
	l := &r.liveCheckpoint
	for {
		l.mu.Lock()
		if l.paused == nil {
			l.active++
			l.mu.Unlock()
			return func() {
				l.mu.Lock()
				l.active--
				if l.active == 0 && l.paused != nil && !l.paused.joined {
					close(l.paused.done)
					l.paused.joined = true
				}
				l.mu.Unlock()
			}, nil
		}
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestSelectedCapacityProbe(t *testing.T) {
	if os.Getenv("HALRO_SELECTED_CAPACITY_PROBE") != "1" {
		t.Skip("registered capacity probe (board §49); set HALRO_SELECTED_CAPACITY_PROBE=1 on Linux ext4 in a 2 GiB cgroup")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	cfg, configPath, _ := selectedInitFixture(t)
	limits, pressure := selectedCapacityProbeBudget(), selectedCapacityProbePressure()
	cfg.ResourceBudget, cfg.ResourcePressure = &limits, &pressure
	if err := limits.Validate(&pressure, cfg.Server.MaxRequestBytes); err != nil {
		t.Fatal("probe budget", err)
	}
	if err := limits.ValidateStandaloneUsageMaintenance(); err != nil {
		t.Fatal("probe budget", err)
	}
	if initialized, err := initializeSelected(ctx, cfg, configPath, nil, nil); err != nil || !initialized {
		t.Fatal("initialize", initialized, err)
	}
	r, err := OpenWithOptions(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{AllowGeneratedSetupToken: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.selectedState == nil {
		t.Fatal("probe did not open the selected engine")
	}
	project := domain.Project{ID: "prj_capacity", Name: "capacity", Enabled: true}
	if _, err := r.store.PutProject(ctx, project, 0, nil); err != nil {
		t.Fatal(err)
	}
	size := func(sub string) int64 {
		var total int64
		_ = filepath.WalkDir(filepath.Join(cfg.Storage.DataDir, sub), func(_ string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if info, err := d.Info(); err == nil {
					total += info.Size()
				}
			}
			return nil
		})
		return total
	}
	var pauseMu sync.Mutex
	var pauses []time.Duration
	pollCtx, stopPoll := context.WithCancel(ctx)
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		var since time.Time
		for pollCtx.Err() == nil {
			r.liveCheckpoint.mu.Lock()
			paused := r.liveCheckpoint.paused != nil
			r.liveCheckpoint.mu.Unlock()
			if paused && since.IsZero() {
				since = time.Now()
			} else if !paused && !since.IsZero() {
				pauseMu.Lock()
				pauses = append(pauses, time.Since(since))
				pauseMu.Unlock()
				since = time.Time{}
			}
			time.Sleep(time.Millisecond)
		}
	}()
	var sequence atomic.Int64
	var levels []selectedCapacityLevel
	for _, plan := range []struct{ concurrency, requests int }{{1, 2000}, {16, 20000}, {64, 20000}} {
		pauseMu.Lock()
		pauses = pauses[:0]
		pauseMu.Unlock()
		level := selectedCapacityLevel{Concurrency: plan.concurrency, Requests: plan.requests}
		latencies := make([]time.Duration, 0, plan.requests)
		var mu sync.Mutex
		var next atomic.Int64
		var wg sync.WaitGroup
		start := time.Now()
		for range plan.concurrency {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for next.Add(1) <= int64(plan.requests) {
					began := time.Now()
					rejected, err := selectedCapacityRequest(ctx, r, project.ID, fmt.Sprintf("req_capacity_%07d", sequence.Add(1)))
					elapsed := time.Since(began)
					mu.Lock()
					switch {
					case err != nil:
						level.Errors++
						if level.FirstError == "" {
							level.FirstError = err.Error()
						}
					case rejected:
						level.Rejected++
					default:
						level.Completed++
						latencies = append(latencies, elapsed)
					}
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		level.Seconds = time.Since(start).Seconds()
		level.Throughput = float64(level.Completed) / level.Seconds
		slices.Sort(latencies)
		if len(latencies) > 0 {
			at := func(q float64) float64 {
				return float64(latencies[min(len(latencies)-1, int(q*float64(len(latencies))))]) / float64(time.Millisecond)
			}
			level.P50Ms, level.P99Ms, level.MaxMs = at(0.50), at(0.99), float64(latencies[len(latencies)-1])/float64(time.Millisecond)
		}
		pauseMu.Lock()
		level.Pauses = len(pauses)
		for _, p := range pauses {
			level.MaxPauseMs = max(level.MaxPauseMs, float64(p)/float64(time.Millisecond))
		}
		pauseMu.Unlock()
		level.LedgerRootBytes, level.UsageRootBytes, level.NativeLedgerByte = size("memory-state/ledger"), size("memory-state/usage"), size("ledger")
		levels = append(levels, level)
		t.Logf("%+v", level)
	}
	stopPoll()
	<-pollDone
	peak := int64(-1)
	if raw, err := os.ReadFile("/sys/fs/cgroup/memory.peak"); err == nil {
		peak, _ = strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	}
	result := struct {
		Levels         []selectedCapacityLevel `json:"levels"`
		CgroupPeakByte int64                   `json:"cgroup_peak_bytes"`
	}{levels, peak}
	if out := os.Getenv("HALRO_SELECTED_CAPACITY_PROBE_OUT"); out != "" {
		raw, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(out, raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("cgroup peak %d bytes", peak)

	// Registered thresholds (board §49); any miss is a failed route, never widened.
	var misses []string
	for _, level := range levels {
		if level.Errors > 0 {
			misses = append(misses, fmt.Sprintf("c=%d errors=%d first=%q", level.Concurrency, level.Errors, level.FirstError))
		}
		if float64(level.Rejected) > 0.01*float64(level.Requests) {
			misses = append(misses, fmt.Sprintf("c=%d rejected=%d > 1%%", level.Concurrency, level.Rejected))
		}
		if level.MaxPauseMs > 2000 {
			misses = append(misses, fmt.Sprintf("c=%d checkpoint pause %.0fms > 2s", level.Concurrency, level.MaxPauseMs))
		}
	}
	if last := levels[len(levels)-1]; last.Throughput < 100 || last.P99Ms > 1000 {
		misses = append(misses, fmt.Sprintf("c=64 throughput %.1f/s p99 %.0fms", last.Throughput, last.P99Ms))
	}
	if peak < 0 || peak > 1536<<20 {
		misses = append(misses, fmt.Sprintf("cgroup peak %d", peak))
	}
	if len(misses) > 0 {
		t.Fatal("capacity probe missed registered thresholds:\n" + strings.Join(misses, "\n"))
	}
}

// selectedCapacityRequest runs one accounting-only request through the
// business gate. Designed backpressure (pack reclamation, resource pressure) is
// a rejection; the request is then finalized as an error, as the Gateway does.
func selectedCapacityRequest(ctx context.Context, r *Runtime, projectID, requestID string) (bool, error) {
	done, err := selectedCapacityAdmit(ctx, r)
	if err != nil {
		return false, err
	}
	defer done()
	backpressure := func(err error) bool {
		return errors.Is(err, authenticatedstate.ErrCapacity) || errors.Is(err, resourcebudget.ErrPressure)
	}
	q, err := r.accounting.BeginRequestDetailed(ctx, projectID, "key_capacity", requestID, "chat")
	if err != nil {
		return backpressure(err), ignoreBackpressure(err, backpressure)
	}
	a, err := r.accounting.ReserveAttempt(ctx, q, 1<<40, 3)
	if err != nil {
		if backpressure(err) {
			return true, r.accounting.Finalize(ctx, q, "error")
		}
		return false, err
	}
	if err := r.accounting.Settle(ctx, a, budget.Settlement{CommittedMicrosUSD: 2, Outcome: "success"}); err != nil {
		return false, fmt.Errorf("settle: %w", err)
	}
	if err := r.accounting.Finalize(ctx, q, "success"); err != nil {
		return false, fmt.Errorf("finalize: %w", err)
	}
	return false, nil
}

func ignoreBackpressure(err error, backpressure func(error) bool) error {
	if backpressure(err) {
		return nil
	}
	return err
}
