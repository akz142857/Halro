//go:build linux

package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/usage"
	"golang.org/x/sys/unix"
)

func TestSelectedPhysicalUsageHistoricalStartupPruneAndReopen(t *testing.T) {
	root := os.Getenv("HALRO_EXT4_ROOT")
	if root == "" {
		t.Skip("requires explicit dedicated ext4 fixture")
	}
	var fs unix.Statfs_t
	if !filepath.IsAbs(root) {
		t.Fatal("fixture root must be absolute")
	}
	if err := unix.Statfs(root, &fs); err != nil || fs.Type != unix.EXT4_SUPER_MAGIC {
		t.Fatal("requires actual ext4", fs, err)
	}
	t.Setenv("TMPDIR", root)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cfg, _, _, sample := selectedLegacyExportFixture(t, ctx, true)
	limits := physicalTerminalBudgetFixture()
	limits.PhysicalTerminalReserve, limits.MemoryStateRequired = true, true
	cfg.ResourceBudget = &limits
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r, err := OpenWithOptions(ctx, cfg, logger, OpenOptions{offlineVerification: true, AllowGeneratedSetupToken: true, resourceSampleSource: sample})
	if err != nil {
		t.Fatal("physical Runtime with historical completed request", err)
	}
	defer r.Close()
	before, err := r.usage.disk.ReadStats(ctx)
	if err != nil || before.Window.Attempts != 1 || before.Window.Summaries != 1 {
		t.Fatal("historical fixture", before, err)
	}
	manifest := filepath.Join(cfg.UsagePath(), "manifest.json")
	archive, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	head, hash, _ := r.ledger.ChainHead()
	lease, err := r.ledger.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().UTC().Add(24 * time.Hour)
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if _, err := r.usageExporter.PruneVerifiedWindowStepUnderMaintenance(cancelled, r.usage.disk, lease, cutoff, usage.DiskWindowLimits{Rows: 128, InputBytes: 8 << 20}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled job accepted", err)
	}
	for step := 0; ; step++ {
		if step > 4 {
			t.Fatal("finite historical prune did not complete")
		}
		out, err := r.usageExporter.PruneVerifiedWindowStepUnderMaintenance(ctx, r.usage.disk, lease, cutoff, usage.DiskWindowLimits{Rows: 128, InputBytes: 8 << 20})
		if err != nil {
			t.Fatal("original lease and actual physical candidate", err)
		}
		if out.Done {
			break
		}
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.usageExporter.PruneVerifiedWindowStepUnderMaintenance(ctx, r.usage.disk, lease, cutoff, usage.DiskWindowLimits{Rows: 128, InputBytes: 8 << 20}); err == nil {
		t.Fatal("closed original maintenance lease authorized a physical update")
	}
	after, err := r.usage.disk.ReadStats(ctx)
	if err != nil || after.Totals != before.Totals || after.Metrics != before.Metrics || after.Watermark != before.Watermark || after.Window.TrimmedAttempts != 1 || after.Window.Attempts != 0 || after.Window.Summaries != 0 {
		t.Fatal("derived prune changed native cumulative facts", before, after, err)
	}
	actualHead, actualHash, _ := r.ledger.ChainHead()
	if actualHead != head || actualHash != hash {
		t.Fatal("window prune changed native authority")
	}
	if err := r.CheckpointMemoryState(ctx); err != nil {
		t.Fatal("physical common after derived prune", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal("original physical close", err)
	}
	fresh, err := OpenWithOptions(ctx, cfg, logger, OpenOptions{offlineVerification: true, AllowGeneratedSetupToken: true, resourceSampleSource: sample})
	if err != nil {
		t.Fatal("fresh trimmed physical Runtime", err)
	}
	defer fresh.Close()
	reopened, err := fresh.usage.disk.ReadStats(ctx)
	if err != nil || reopened.Totals != after.Totals || reopened.Metrics != after.Metrics || reopened.Watermark != after.Watermark || reopened.Window != after.Window {
		t.Fatal("reopen lost verified trimmed state", reopened, after, err)
	}
	actualArchive, err := os.ReadFile(manifest)
	if err != nil || !bytes.Equal(actualArchive, archive) {
		t.Fatal("prune changed authoritative archive", err)
	}
	if err := fresh.Close(); err != nil {
		t.Fatal("fresh physical close", err)
	}
}
