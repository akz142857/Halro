package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/usage"
)

func TestUsageDiskDatesPeriodicSweepsBindTrimmedWindowAndResume(t *testing.T) {
	for _, format := range []string{usage.FormatNDJSON, usage.FormatParquet} {
		t.Run(format, func(t *testing.T) {
			r, _ := summaryRuntime(t, 0)
			now := time.Now().UTC()
			seedRetentionUsage(t, r, 3, now.AddDate(0, 0, -120))
			billOneRequest(t, r, "dates-current")
			p := installDiskUsageQueries(t, r)
			e := bindStartupUsageArchive(t, r, format)
			ctx := context.Background()
			if _, err := e.ExportDiskStep(ctx, p, usage.DefaultDiskExportStepLimits()); err != nil {
				t.Fatal(err)
			}
			totals, err := p.ReadStats(ctx)
			if err != nil {
				t.Fatal(err)
			}
			restoreStartupUsageWindow(t, r, now)
			for round := 0; round < 3; round++ {
				if round > 0 {
					seedLedgerActivity(t, r, fmt.Sprintf("dates-suffix-%d", round))
					if err := r.usageCollector.CatchUp(ctx); err != nil {
						t.Fatal(err)
					}
					if _, err := e.ExportDiskStep(ctx, p, usage.DefaultDiskExportStepLimits()); err != nil {
						t.Fatal(err)
					}
				}
				out, err := r.maintainUsageDiskDates(ctx, now.AddDate(0, 0, round))
				if err != nil || !out.OriginalChainComplete || out.Policies != 1 || out.Current.Retention.RetainFromDate != usage.RetentionCutoff(now.AddDate(0, 0, round), r.config.Usage.RetentionDays).Format("2006-01-02") {
					t.Fatal(round, out, err)
				}
				window, err := e.PruneVerifiedWindowStep(ctx, p, now.AddDate(0, 0, -30), usage.DiskWindowLimits{Rows: 128, InputBytes: 8 << 20})
				if err != nil {
					t.Fatal("maintenance proof not bound to live owner", window, err)
				}
				again, err := r.maintainUsageDiskDates(ctx, now.AddDate(0, 0, round))
				if err != nil || again.Policies != 0 || again.DataSteps != 0 || !again.OriginalChainComplete {
					t.Fatal("completed chain restarted data after pages gone", again, err)
				}
			}
			stats, err := p.ReadStats(ctx)
			if err != nil || stats.Window.Attempts != 1 || stats.Metrics.RequestsSuccess < totals.Metrics.RequestsSuccess {
				t.Fatal(stats, err)
			}
			// Rebuild without saved Usage bytes; existing signed progress still
			// selects the current completed policy and permits a later successor.
			if err := r.usage.closeOwner(); err != nil {
				t.Fatal(err)
			}
			p = installDiskUsageQueries(t, r)
			bindStartupUsageArchive(t, r, format)
			restoreStartupUsageWindow(t, r, now)
			out, err := r.maintainUsageDiskDates(ctx, now.AddDate(0, 0, 4))
			if err != nil || !out.OriginalChainComplete || out.Policies != 1 {
				t.Fatal(out, err)
			}
		})
	}
}

func TestUsageDiskDatesCanceledAndCorruptProgressPreserveFacts(t *testing.T) {
	for _, fault := range []string{"cancel", "signed-pages"} {
		t.Run(fault, func(t *testing.T) {
			r, _ := summaryRuntime(t, 1)
			p := installDiskUsageQueries(t, r)
			e := bindStartupUsageArchive(t, r, usage.FormatNDJSON)
			ctx := context.Background()
			if _, err := e.ExportDiskStep(ctx, p, usage.DefaultDiskExportStepLimits()); err != nil {
				t.Fatal(err)
			}
			if _, err := r.maintainUsageDiskDates(ctx, time.Now()); err != nil {
				t.Fatal(err)
			}
			root, err := os.ReadFile(filepath.Join(r.config.UsagePath(), "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			before, err := p.ReadStats(ctx)
			if err != nil {
				t.Fatal(err)
			}
			jobCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			if fault == "cancel" {
				cancel()
			} else {
				if err := os.WriteFile(filepath.Join(r.config.UsagePath(), ".archive-date-pages-v1.json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			out, err := r.maintainUsageDiskDates(jobCtx, time.Now().AddDate(0, 0, 1))
			if err == nil || out != (usageDateMaintenanceReport{}) {
				t.Fatal(out, err)
			}
			after, readErr := p.ReadStats(ctx)
			if readErr != nil || before != after {
				t.Fatal("failed maintenance changed live facts", before, after, readErr)
			}
			actual, readErr := os.ReadFile(filepath.Join(r.config.UsagePath(), "manifest.json"))
			if readErr != nil || string(actual) != string(root) {
				t.Fatal("failed progress moved root", readErr)
			}
			if _, err := e.PruneVerifiedWindowStep(ctx, p, time.Now(), usage.DiskWindowLimits{Rows: 128, InputBytes: 8 << 20}); !errors.Is(err, usage.ErrArchiveWindowProof) {
				t.Fatal("failure retained live permission", err)
			}
		})
	}
}
