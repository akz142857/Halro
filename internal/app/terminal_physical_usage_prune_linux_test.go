//go:build linux

package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/usage"
	"golang.org/x/sys/unix"
)

// TestStandalonePhysicalUsageWindowPrunesBesideTerminalReserve covers board
// §36 v4: a Roll prepays a COW root for the successor's first frame, and the
// window prune refused while any root stayed prepaid, although it allocates
// its own candidate above that debt. The prune must run with that root
// outstanding, and the reserve must still serve the next request, checkpoint
// and reopen.
func TestStandalonePhysicalUsageWindowPrunesBesideTerminalReserve(t *testing.T) {
	root := os.Getenv("HALRO_TERMINAL_ENOSPC_ROOT")
	if root == "" {
		t.Skip("requires explicit dedicated Linux filesystem <=512 MiB")
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(root, &fs); err != nil || fs.Type != unix.EXT4_SUPER_MAGIC || uint64(fs.Blocks)*uint64(fs.Bsize) > 512<<20 {
		t.Fatal("refusing a filesystem outside the explicit dedicated guard", fs, err)
	}
	directory, err := os.MkdirTemp(root, "halro-physical-prune-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
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
	limits := physicalTerminalBudgetFixture()
	limits.LedgerProjectionDiskBytes = 56 << 20
	limits.PhysicalTerminalReserve = true
	cfg.ResourceBudget = &limits
	pressure := physicalTerminalActualOSPressurePolicy()
	cfg = activateClosedRuntimeMemoryStateFixtureWithPressure(t, ctx, cfg, instance, false, &pressure)
	cfg.Ledger.Seal = config.LedgerSeal{Enabled: true, MaxActiveBytes: config.MinLedgerSealMaxActiveBytes, Compress: true}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	open := func() *Runtime {
		t.Helper()
		r, err := OpenWithOptions(ctx, cfg, logger, OpenOptions{AllowGeneratedSetupToken: true})
		if err != nil {
			t.Fatal("open physical selected Runtime", err)
		}
		if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
			t.Fatal(err)
		}
		r.stopBackgroundWork()
		r.config.Ledger.Seal.MaxActiveBytes = 1
		installPhysicalRollCanary(t, r, credentials)
		return r
	}

	r := open()
	a := servePhysicalRollChat(t, ctx, r, credentials, "req_prune_a")
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	b := servePhysicalRollChat(t, ctx, r, credentials, "req_prune_b")
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	// The archive covers sealed generations; roll so both requests export.
	if rolled, err := sealRolled(cfg, r.sealLedgerGeneration); err != nil || !rolled {
		t.Fatal("seal did not roll", rolled, err)
	}
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	// The ordinary cycle binds the verified window and exports; its own prune
	// uses a 30-day cutoff and must not refuse beside the reserve either.
	if err := r.maintainUsageDiskCycle(ctx, time.Now()); err != nil {
		t.Fatal("Usage disk cycle beside the terminal reserve", err)
	}
	before, err := r.usage.disk.ReadStats(ctx)
	if err != nil || before.Window.Attempts == 0 {
		t.Fatal("fixture has no window rows to prune", before, err)
	}
	cutoff := time.Now().UTC().Add(24 * time.Hour)
	for step := 0; ; step++ {
		if step > 8 {
			t.Fatal("finite prune did not complete")
		}
		out, err := r.usageExporter.PruneVerifiedWindowStep(ctx, r.usage.disk, cutoff, usage.DiskWindowLimits{Rows: 128, InputBytes: 8 << 20})
		if err != nil {
			t.Fatal("window prune beside the terminal reserve", step, err)
		}
		if out.Done {
			break
		}
	}
	after, err := r.usage.disk.ReadStats(ctx)
	if err != nil || after.Totals != before.Totals || after.Metrics != before.Metrics || after.Watermark != before.Watermark || after.Window.Attempts != 0 || after.Window.TrimmedAttempts < 2 {
		t.Fatal("prune changed cumulative facts or left exported rows", before, after, err)
	}
	if r.status.Load() != ledger.AccountingHealthy {
		t.Fatal("prune poisoned accounting", r.status.Load())
	}
	// The reserve the prune stood beside still finalizes the next request.
	c := servePhysicalRollChat(t, ctx, r, credentials, "req_prune_c")
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.CheckpointMemoryState(ctx); err != nil {
		t.Fatal("checkpoint after prune", err)
	}
	trimmed, err := r.usage.disk.ReadStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.drainAccountingBusiness(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal("close after prune", err)
	}

	r = open()
	defer r.Close()
	for _, id := range []string{a, b, c} {
		q, found, err := r.selectedState.generation.Owners.Ledger.ReadRequest(ctx, id)
		if err != nil || !found || q.Finalized == nil {
			t.Fatal("terminal not readable after reopen", id, found, err)
		}
	}
	reopened, err := r.usage.disk.ReadStats(ctx)
	if err != nil || reopened.Totals != trimmed.Totals || reopened.Window != trimmed.Window || reopened.Watermark != trimmed.Watermark {
		t.Fatal("reopen lost the trimmed Usage state", reopened, trimmed, err)
	}
	servePhysicalRollChat(t, ctx, r, credentials, "req_prune_d")
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.CheckpointMemoryState(ctx); err != nil {
		t.Fatal("checkpoint after reopen", err)
	}
	if err := r.drainAccountingBusiness(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal("close after reopen", err)
	}
}
