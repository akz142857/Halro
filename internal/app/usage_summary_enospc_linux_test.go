//go:build linux

package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/usage"
	"golang.org/x/sys/unix"
)

// E1 point 3 (board §40, §40.8): the admin Usage summary reduces into a bbolt
// scratch file beside the selected state. With the filesystem full except for
// a swept headroom, each query either returns the same summary as before the
// fill or fails whole, removes its scratch and leaves accounting healthy. At
// least one headroom must let the scratch exist and then meet ENOSPC.
func TestSelectedUsageSummaryAtActualENOSPCRemovesScratch(t *testing.T) {
	root := os.Getenv("HALRO_TERMINAL_ENOSPC_ROOT")
	if root == "" || os.Getenv("HALRO_TERMINAL_ACTUAL_OS_PRESSURE") != "1" {
		t.Skip("requires explicit dedicated Linux filesystem <=512 MiB and the actual OS sampler")
	}
	var statfs unix.Statfs_t
	if os.Getenv("HALRO_TERMINAL_ENOSPC_FS") != "ext4" || unix.Statfs(root, &statfs) != nil || statfs.Type != unix.EXT4_SUPER_MAGIC || uint64(statfs.Blocks)*uint64(statfs.Bsize) > 512<<20 {
		t.Fatal("refusing to exhaust filesystem outside explicit dedicated guard", statfs)
	}
	directory, err := os.MkdirTemp(root, "halro-e1-summary-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	legacy, credentials, _ := openBootstrappedRuntimeShaped(t, "gpt-test", func(c *config.Config) {
		c.Storage.DataDir = filepath.Join(directory, "data")
		c.Storage.MasterKey.File = filepath.Join(directory, "master.key")
	})
	cfg, instance := legacy.config, legacy.instanceID
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	limits := selectedRuntimeBudgetFixture()
	cfg.ResourceBudget = &limits
	pressure := physicalTerminalActualOSPressurePolicy()
	cfg = activateClosedRuntimeMemoryStateFixtureWithPressure(t, ctx, cfg, instance, false, &pressure)
	r, err := OpenWithOptions(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{AllowGeneratedSetupToken: true})
	if err != nil {
		t.Fatal("selected Runtime", err)
	}
	defer func() { _ = r.Close() }()
	if r.selectedState == nil || r.usage.disk == nil {
		t.Fatal("fixture is not a selected Runtime with the disk Usage view")
	}
	if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
		t.Fatal(err)
	}
	r.stopBackgroundWork()
	for i, settled := 0, 0; settled < 40; i++ {
		if i > 400 {
			t.Fatal("seed requests kept being refused", settled)
		}
		q, err := r.accounting.BeginRequestDetailed(ctx, credentials.ProjectID, credentials.KeyID, fmt.Sprintf("e1_summary_%d", i), "chat")
		if errors.Is(err, resourcebudget.ErrPressure) {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := r.accounting.Finalize(ctx, q, "success"); err != nil {
			t.Fatal(err)
		}
		settled++
	}
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	period, err := r.periods.PeriodAt(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	start, end, err := summaryRange("", "", "day", period.ID)
	if err != nil {
		t.Fatal(err)
	}
	// readDiskUsageSummary is what the admin handler calls; it answers 503 for
	// any error and encodes only a nil-error body.
	summary := func() ([]byte, error) {
		body, _, err := r.readDiskUsageSummary(ctx, usage.DiskDailySummaryQuery{Start: start, End: end, Granularity: "day", GroupBy: "project", Sort: "cost", Order: "desc", Limit: summaryDefaultGroupLimit}, timeContext{})
		if err != nil {
			return nil, err
		}
		return marshalUsageSummaryResponse(body)
	}
	want, err := summary()
	if err != nil {
		t.Fatal("reference summary", err)
	}
	scratch := usageSummaryScratchPath(r.config)
	waitHealthy := func() {
		deadline := time.Now().Add(2 * time.Minute)
		for r.resourcePressure.monitor.Snapshot().State != resourcebudget.Healthy {
			if time.Now().After(deadline) {
				t.Fatal("pressure did not return to Healthy", r.resourcePressure.monitor.Snapshot())
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	fillerPath := filepath.Join(directory, "foreign-filler")
	reachedScratch := 0
	for _, headroom := range []int64{0, 1, 2, 3, 4, 6, 8, 12, 16, 24, 32, 48, 64} {
		if n, err := fillForeignUntilENOSPC(fillerPath, int(statfs.Bsize)); err != nil {
			t.Fatal("fill", n, err)
		}
		info, err := os.Stat(fillerPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(fillerPath, max(0, info.Size()-headroom*int64(statfs.Bsize))); err != nil {
			t.Fatal(err)
		}
		got, err := summary()
		var path *fs.PathError
		atMkdir := errors.As(err, &path) && path.Op == "mkdir"
		t.Log("headroom blocks", headroom, "ok", err == nil, "mkdir", atMkdir, "enospc", errors.Is(err, syscall.ENOSPC), err)
		if err == nil && !bytes.Equal(got, want) {
			t.Fatal("summary at ENOSPC differs from the reference", headroom, string(got), string(want))
		}
		if _, statErr := os.Lstat(scratch); !os.IsNotExist(statErr) {
			t.Fatal("summary scratch survived the query", headroom, statErr)
		}
		if s := r.status.Load(); s != ledger.AccountingHealthy {
			t.Fatal("summary at ENOSPC changed accounting health", headroom, s)
		}
		if err != nil && !atMkdir && errors.Is(err, syscall.ENOSPC) {
			reachedScratch++
		}
	}
	if reachedScratch == 0 {
		t.Fatal("no headroom let the scratch exist and then meet ENOSPC")
	}
	if err := os.Remove(fillerPath); err != nil {
		t.Fatal(err)
	}
	waitHealthy()
	got, err := summary()
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("summary after space returned", err, string(got), string(want))
	}
	if _, err := r.usage.disk.ReadStats(ctx); err != nil {
		t.Fatal("online Usage read after space returned", err)
	}
	if s := r.status.Load(); s != ledger.AccountingHealthy {
		t.Fatal("status after space returned", s)
	}
	t.Log("E1-P3 headrooms that reached the scratch and met ENOSPC", reachedScratch)
}
