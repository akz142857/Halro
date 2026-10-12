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
	"golang.org/x/sys/unix"
)

// E1 point 2 (board §40): a non-physical selected Standalone checkpoints at
// real ENOSPC with dead pages accumulated, so the compact candidate would be
// written beside the selected packs. The checkpoint fails without latching
// recovery, new work completes or is shed, accepted facts survive, and the
// checkpoint and a restart succeed once space returns.
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
	request := func(id string) (int, error) {
		var businessErr error
		handler := r.withMemoryCheckpointRequest(r.withProjectionPackAdmission(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			q, err := r.accounting.BeginRequestDetailed(ctx, credentials.ProjectID, credentials.KeyID, id, "chat")
			if err == nil {
				err = r.accounting.Finalize(ctx, q, "success")
			}
			businessErr = err
		})))
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
		return recorder.Code, businessErr
	}
	// Dead pages: overwrite the same rows across checkpoints.
	settled := 0
	for i := 0; i < 240; i++ {
		code, err := request(fmt.Sprintf("e1_seed_%d", i))
		if code == http.StatusServiceUnavailable {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if err != nil {
			t.Fatal("seed request", i, err)
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
	checkpointErr := r.CheckpointMemoryState(ctx)
	t.Log("checkpoint at ENOSPC", checkpointErr, "status", r.status.Load())
	if checkpointErr == nil || !errors.Is(checkpointErr, syscall.ENOSPC) {
		t.Fatal("checkpoint at actual ENOSPC did not fail with ENOSPC", checkpointErr)
	}
	if r.status.Load() != ledger.AccountingHealthy {
		t.Fatal("checkpoint ENOSPC latched accounting recovery", r.status.Load())
	}
	shed, completed := 0, 0
	for i := 0; i < 3; i++ {
		code, err := request(fmt.Sprintf("e1_full_%d", i))
		t.Log("request at ENOSPC", i, code, err)
		switch {
		case code == http.StatusServiceUnavailable:
			shed++
		case err == nil:
			completed++
		default:
			t.Fatal("request at ENOSPC neither completed nor shed", code, err)
		}
	}
	if now := listing(); !slices.Equal(now, before) {
		t.Fatal("failed checkpoint left files", before, now)
	}
	if err := os.Remove(fillerPath); err != nil {
		t.Fatal(err)
	}
	if err := r.CheckpointMemoryState(ctx); err != nil {
		t.Fatal("checkpoint after space returned", err)
	}
	if r.status.Load() != ledger.AccountingHealthy {
		t.Fatal("status after recovery", r.status.Load())
	}
	if err := r.Close(); err != nil {
		t.Fatal("close", err)
	}
	r = open()
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	stats, err = r.usage.disk.ReadStats(ctx)
	if err != nil || stats.Totals.Requests != baseRequests+int64(completed) {
		t.Fatal("accepted Usage facts after restart", baseRequests, completed, stats.Totals.Requests, err)
	}
	after, _, ok := r.ledger.ChainHead()
	if !ok || after.Sequence < head.Sequence {
		t.Fatal("Ledger head regressed", head, after)
	}
	t.Log("settled before fill", settled, "at ENOSPC completed", completed, "shed", shed)
}
