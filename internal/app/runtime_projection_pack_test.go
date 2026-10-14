package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// A burst of accepted requests used to fill the 64 MiB Usage page pack long
// before the scheduled checkpoint: the reduction then met the ceiling part-way,
// poisoned the pack and retired the projection. Fill now triggers a common
// checkpoint whose repack reclaims obsolete pages, admission sheds new work
// while a pack lacks room for admitted work, and live reduction shares one page
// rewrite across a batch, so the burst lands whole.
func TestSelectedRuntimeUsageBurstReclaimsPackBeforeItFills(t *testing.T) {
	runProjectionPackBurst(t, nil, 800)
}

// The Ledger projection applies every record inside the append, where a
// refused update latches accounting recovery. With a large Usage pack that no
// longer drives checkpoints, its own small pack must trigger reclamation and,
// if needed, shed new work before it fills.
func TestSelectedRuntimeLedgerBurstReclaimsPackBeforeItFills(t *testing.T) {
	runProjectionPackBurst(t, func(b *config.ResourceBudget) {
		b.UsageProjectionDiskBytes = 256 << 20
		b.LedgerProjectionDiskBytes = 16 << 20
		fundUsageMaintenanceIO(b)
	}, 600)
}

func runProjectionPackBurst(t *testing.T, configure func(*config.ResourceBudget), requests int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	legacy, credentials, _ := openBootstrappedRuntimeForModel(t, "gpt-test")
	cfg, instance := legacy.config, legacy.instanceID
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	if configure != nil {
		// The independent baseline binds the ceilings; set them before activation.
		limits := selectedRuntimeBudgetFixture()
		configure(&limits)
		cfg.ResourceBudget = &limits
	}
	cfg = activateClosedRuntimeMemoryStateFixture(t, ctx, cfg, instance, false)
	// Only the fill trigger and admission may checkpoint during the burst.
	cfg.Usage.CheckpointInterval = config.Duration(time.Hour)
	opts := OpenOptions{AllowGeneratedSetupToken: true, resourceSampleSource: pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() })}
	r, err := OpenWithOptions(ctx, cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if r != nil {
			if err := r.Close(); err != nil {
				t.Error(err)
			}
		}
	}()
	if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := r.usage.disk.ReadStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	revision := selectedSealRevision(r)
	packs := r.projectionPacks()
	// Business work holds the same tickets a gateway handler does: a
	// checkpoint joins it, and either gate sheds new work with 503.
	shed := 0
	for i := 0; i < requests; {
		var businessErr error
		handler := r.withMemoryCheckpointRequest(r.withProjectionPackAdmission(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			q, err := r.accounting.BeginRequestDetailed(ctx, credentials.ProjectID, credentials.KeyID, fmt.Sprintf("burst_%d", i), "chat")
			if err == nil {
				err = r.accounting.Finalize(ctx, q, "success")
			}
			businessErr = err
		})))
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
		if recorder.Code == http.StatusServiceUnavailable {
			shed++
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if businessErr != nil {
			t.Fatal("request", i, businessErr)
		}
		i++
		if i%100 == 0 {
			for n, p := range packs {
				var used, usable uint64
				var ok bool
				for try := 0; try < 200 && !ok; try++ {
					if used, usable, ok = p.PackUsage(); !ok {
						time.Sleep(time.Millisecond)
					}
				}
				t.Log("progress", i, "revision", selectedSealRevision(r), "pack", n, used, usable, ok, "shed", shed)
				if ok && used > usable {
					t.Fatal("pack filled past its usable room", n, used, usable)
				}
			}
		}
	}
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		t.Fatal("catch-up after the burst", err)
	}
	stats, err := r.usage.disk.ReadStats(ctx)
	if err != nil || stats.Totals.Requests != before.Totals.Requests+int64(requests) {
		t.Fatal("burst Usage", before.Totals.Requests, stats.Totals.Requests, err)
	}
	if selectedSealRevision(r) <= revision {
		t.Fatal("fill never triggered a checkpoint", revision)
	}
	t.Log("checkpoints", selectedSealRevision(r)-revision, "shed", shed)
	if err := r.Close(); err != nil {
		t.Fatal("close after the burst", err)
	}
	r = nil
}

type fakeProjectionPack struct {
	used, usable uint64
	ok           bool
}

func (f *fakeProjectionPack) PackUsage() (uint64, uint64, bool) { return f.used, f.usable, f.ok }

func TestProjectionPackFillTriggersFromTheReclaimedBaselineAndSpacesCheckpoints(t *testing.T) {
	p := &fakeProjectionPack{used: 39, usable: 100, ok: true}
	packs := [2]projectionPackReader{nil, p}
	var f projectionPackFill
	now := time.Unix(1000, 0)
	if f.due(packs, now) {
		t.Fatal("triggered below half of the room")
	}
	p.used = 50
	if !f.due(packs, now) {
		t.Fatal("half of the room did not trigger")
	}
	if f.due(packs, now.Add(projectionPackMinCheckpointGap-time.Nanosecond)) {
		t.Fatal("triggered inside the gap")
	}
	// Live rows that survive reclamation raise the baseline, so the trigger
	// measures the room that remains rather than firing every gap.
	p.used = 80
	f.reclaimed(packs)
	later := now.Add(projectionPackMinCheckpointGap)
	if f.due(packs, later) {
		t.Fatal("triggered at the reclaimed baseline")
	}
	p.used = 90
	if !f.due(packs, later) {
		t.Fatal("half of the remaining room did not trigger")
	}
	p.ok = false
	if f.due(packs, later.Add(projectionPackMinCheckpointGap)) {
		t.Fatal("triggered while the pack was busy")
	}
	if !f.dueWhileShedding(later.Add(projectionPackShedCheckpointGap)) || f.dueWhileShedding(later.Add(projectionPackShedCheckpointGap)) {
		t.Fatal("shedding checkpoint spacing")
	}
}

func TestPackShortfallBounds(t *testing.T) {
	for _, c := range []struct {
		used, usable, perRecord, owed uint64
		short, unobserved             bool
	}{
		{0, 100, 0, 1000, false, true},          // no observed record: bootstrap slot decides
		{100, 100, 0, 1, true, false},           // no observed record, no usable room
		{0, 0, 10, 1, false, false},             // no observed pack
		{80, 100, 10, 1, false, false},          // room 20 == 2*10*1
		{81, 100, 10, 1, true, false},           // room 19 < 20
		{120, 100, 1, 1, true, false},           // past usable: no room
		{0, 100, 1 << 40, 1 << 40, true, false}, // saturating reserve
	} {
		if short, unobserved := packShortfall(c.used, c.usable, c.perRecord, c.owed); short != c.short || unobserved != c.unobserved {
			t.Fatal(c, short, unobserved)
		}
	}
}

// Board §58: while a pack has no observed record, concurrent requests pass one
// at a time through the bootstrap slot, and every one of them eventually does.
func TestProjectionPackBootstrapAdmitsOneAtATimeWithoutStarving(t *testing.T) {
	var a projectionPackAdmission
	if _, ok := a.admit(true, false); ok {
		t.Fatal("short pack admitted")
	}
	var inside, most atomic.Int64
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				release, ok := a.admit(false, true)
				if !ok {
					time.Sleep(time.Millisecond)
					continue
				}
				n := inside.Add(1)
				for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
				}
				time.Sleep(2 * time.Millisecond)
				inside.Add(-1)
				release()
				return
			}
		}()
	}
	wg.Wait()
	if most.Load() != 1 {
		t.Fatal("unobserved packs admitted concurrent requests", most.Load())
	}
	if release, ok := a.admit(false, false); !ok {
		t.Fatal("observed pack with room refused")
	} else {
		release()
	}
}

// selectedSealRevision reads the published revision under the live checkpoint
// job lock: the fill trigger checkpoints in the background during the burst.
func selectedSealRevision(r *Runtime) uint64 {
	r.liveCheckpoint.job.Lock()
	defer r.liveCheckpoint.job.Unlock()
	return r.selectedState.generation.Seal.Revision
}
