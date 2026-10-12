//go:build linux

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"golang.org/x/sys/unix"
)

// E1 point 2 (board §40.2, §40.3, §40.7): a non-physical selected Standalone checkpoints at
// real ENOSPC with dead pages accumulated, so the compact candidate would be
// written beside the selected packs. Without the physical reserve the outcome
// is fail-closed: either the sampler sheds first and nothing latches (branch
// A), or a write meets ENOSPC first and accounting latches until restart
// (branch B). Neither branch loses or invents a fact across the restart.
func TestSelectedCheckpointAtActualENOSPCKeepsAcceptedFacts(t *testing.T) {
	root := os.Getenv("HALRO_TERMINAL_ENOSPC_ROOT")
	if root == "" || os.Getenv("HALRO_TERMINAL_ACTUAL_OS_PRESSURE") != "1" {
		t.Skip("requires explicit dedicated Linux filesystem <=512 MiB and the actual OS sampler")
	}
	var statfs unix.Statfs_t
	if os.Getenv("HALRO_TERMINAL_ENOSPC_FS") != "ext4" || unix.Statfs(root, &statfs) != nil || statfs.Type != unix.EXT4_SUPER_MAGIC || uint64(statfs.Blocks)*uint64(statfs.Bsize) > 512<<20 {
		t.Fatal("refusing to exhaust filesystem outside explicit dedicated guard", statfs)
	}
	directory, err := os.MkdirTemp(root, "halro-e1-checkpoint-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	legacy, credentials, _ := openBootstrappedRuntimeShaped(t, "gpt-test", func(c *config.Config) {
		c.Storage.DataDir = filepath.Join(directory, "data")
		c.Storage.MasterKey.File = filepath.Join(directory, "master.key")
		c.Gateway.MaxTotalAttempts = 1
	})
	cfg, instance := legacy.config, legacy.instanceID
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	// The selected fixture's logical ceilings exceed the 512 MiB mount on
	// purpose: the write must meet the filesystem, not the logical budget.
	limits := selectedRuntimeBudgetFixture()
	cfg.ResourceBudget = &limits
	pressure := physicalTerminalActualOSPressurePolicy()
	cfg = activateClosedRuntimeMemoryStateFixtureWithPressure(t, ctx, cfg, instance, false, &pressure)
	cfg.Usage.CheckpointInterval = config.Duration(time.Hour)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	open := func() *Runtime {
		r, err := OpenWithOptions(ctx, cfg, logger, OpenOptions{AllowGeneratedSetupToken: true})
		if err != nil {
			t.Fatal("selected Runtime", err)
		}
		if r.selectedState == nil || r.config.ResourceBudget.PhysicalTerminalReserve {
			t.Fatal("fixture is not a non-physical selected Runtime")
		}
		if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := open()
	defer func() {
		if r != nil {
			_ = r.Close()
		}
	}()
	r.stopBackgroundWork()
	// stage reports where the request stopped: "shed" (admission 503, or a
	// Begin error, which the gateway turns into a 503 before Provider work),
	// "finalize" (accepted, then failed) or "" (settled).
	request := func(id string) (string, error) {
		var stage string
		var businessErr error
		handler := r.withMemoryCheckpointRequest(r.withProjectionPackAdmission(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			q, err := r.accounting.BeginRequestDetailed(ctx, credentials.ProjectID, credentials.KeyID, id, "chat")
			if err != nil {
				stage, businessErr = "shed", err
				return
			}
			if err := r.accounting.Finalize(ctx, q, "success"); err != nil {
				stage, businessErr = "finalize", err
			}
		})))
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
		if recorder.Code == http.StatusServiceUnavailable {
			return "shed", nil
		}
		return stage, businessErr
	}
	// Dead pages: overwrite the same rows across checkpoints.
	settled := 0
	for i := 0; i < 240; i++ {
		stage, err := request(fmt.Sprintf("e1_seed_%d", i))
		if stage == "shed" && (err == nil || errors.Is(err, resourcebudget.ErrPressure)) {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if err != nil {
			t.Fatal("seed request", i, stage, err)
		}
		settled++
		if i%60 == 59 {
			if err := r.CheckpointMemoryState(ctx); err != nil {
				t.Fatal("seed checkpoint", err)
			}
		}
	}
	for n, p := range r.projectionPacks() {
		used, usable, ok := p.PackUsage()
		t.Log("before fill pack", n, used, usable, ok)
	}
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	stats, err := r.usage.disk.ReadStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	baseRequests := stats.Totals.Requests
	head, _, ok := r.ledger.ChainHead()
	if !ok {
		t.Fatal("chain head")
	}
	dataDir := cfg.Storage.DataDir
	listing := func() []string {
		var names []string
		_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				rel, _ := filepath.Rel(dataDir, path)
				names = append(names, rel)
			}
			return nil
		})
		slices.Sort(names)
		return names
	}
	before := listing()

	fillerPath := filepath.Join(directory, "foreign-filler")
	if n, err := fillForeignUntilENOSPC(fillerPath, int(statfs.Bsize)); err != nil {
		t.Fatal("fill", n, err)
	}
	isLatched := func() bool {
		s := r.status.Load()
		return s == ledger.AccountingUnavailable || s == ledger.AccountingRecoveryRequired
	}
	// The branch is the accounting status (§40.2, §40.7): whichever operation
	// first latches, checkpoint or request, must have met ENOSPC.
	checkpointErr := r.CheckpointMemoryState(ctx)
	t.Log("checkpoint at ENOSPC", checkpointErr, "status", r.status.Load())
	latched := isLatched()
	if latched && !errors.Is(checkpointErr, syscall.ENOSPC) {
		t.Fatal("checkpoint latched accounting without ENOSPC", checkpointErr)
	}
	shed, completed, enospc := 0, 0, 0
	for i := 0; i < 3; i++ {
		wasLatched := latched
		stage, err := request(fmt.Sprintf("e1_full_%d", i))
		latched = isLatched()
		t.Log("request at ENOSPC", i, stage, err, "status", r.status.Load())
		switch {
		case stage == "" && err == nil:
			if latched {
				t.Fatal("request settled under the accounting latch", i)
			}
			completed++
		case stage == "shed" && (err == nil || errors.Is(err, resourcebudget.ErrPressure)):
			shed++
		default:
			if !latched {
				t.Fatal("request failure at ENOSPC did not latch accounting", stage, r.status.Load(), err)
			}
			if !wasLatched && !errors.Is(err, syscall.ENOSPC) {
				t.Fatal("latching request failure is not ENOSPC", stage, err)
			}
			if errors.Is(err, syscall.ENOSPC) {
				enospc++
			}
		}
	}
	if latched {
		if err := r.CheckpointMemoryState(ctx); err == nil {
			t.Fatal("checkpoint succeeded after the accounting latch")
		}
	} else {
		if checkpointErr == nil || !errors.Is(checkpointErr, syscall.ENOSPC) && !errors.Is(checkpointErr, resourcebudget.ErrPressure) {
			t.Fatal("checkpoint at actual ENOSPC neither ENOSPC nor pressure", checkpointErr)
		}
		if r.status.Load() != ledger.AccountingHealthy {
			t.Fatal("unlatched branch left accounting unhealthy", r.status.Load())
		}
	}
	var added []string
	for _, name := range listing() {
		if _, found := slices.BinarySearch(before, name); !found {
			added = append(added, name)
		}
	}
	if len(added) != 0 {
		t.Fatal("files added while full", added)
	}
	if err := os.Remove(fillerPath); err != nil {
		t.Fatal(err)
	}
	healthyBy := time.Now().Add(2 * time.Minute)
	for r.resourcePressure.monitor.Snapshot().State != resourcebudget.Healthy {
		if time.Now().After(healthyBy) {
			t.Fatal("pressure did not return to Healthy after space returned", r.resourcePressure.monitor.Snapshot())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if latched {
		if s := r.status.Load(); s != ledger.AccountingUnavailable && s != ledger.AccountingRecoveryRequired {
			t.Fatal("accounting latch lowered in process", s)
		}
		if stage, err := request("e1_after_space"); stage == "" && err == nil {
			t.Fatal("request settled after the accounting latch once space returned")
		}
		closeErr := r.Close()
		t.Log("E1-2b close after latch", closeErr)
	} else {
		if err := r.CheckpointMemoryState(ctx); err != nil {
			t.Fatal("checkpoint after space returned", err)
		}
		if err := r.Close(); err != nil {
			t.Fatal("close", err)
		}
	}
	r = open()
	if s := r.status.Load(); s != ledger.AccountingHealthy {
		t.Fatal("status after restart", s)
	}
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	stats, err = r.usage.disk.ReadStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	low, high := baseRequests+int64(completed), baseRequests+int64(completed+enospc)
	if stats.Totals.Requests < low || stats.Totals.Requests > high {
		t.Fatal("accepted Usage facts after restart", baseRequests, completed, enospc, stats.Totals.Requests)
	}
	after, _, ok := r.ledger.ChainHead()
	if !ok || after.Sequence < head.Sequence {
		t.Fatal("Ledger head regressed", head, after)
	}
	branch := "A"
	if latched {
		branch = "B"
	}
	t.Log("E1-P2 branch", branch, "usage", baseRequests, "->", stats.Totals.Requests, "head", head, "->", after)
	t.Log("settled before fill", settled, "at ENOSPC completed", completed, "shed", shed)
}
