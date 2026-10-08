package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/usage"
)

func TestPrimaryUsageDefaultRetainedStandaloneArchiveAndTermWithdrawal(t *testing.T) {
	for _, format := range []string{usage.FormatNDJSON, usage.FormatParquet} {
		t.Run(format, func(t *testing.T) {
			ctx := context.Background()
			cfg := testConfig(t)
			cfg.Usage.ExportFormat = format
			if err := Initialize(cfg); err != nil {
				t.Fatal(err)
			}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			r, err := Open(ctx, cfg, logger)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			now := time.Now().UTC()
			seedRetentionUsage(t, r, 2, now.AddDate(0, 0, -120))
			billOneRequest(t, r, "member-baseline")
			for i := 0; i < 2; i++ {
				if err := r.maintainUsageDiskCycle(ctx, now); err != nil {
					t.Fatal(err)
				}
			}
			before, err := r.usage.disk.ReadStats(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			primary, _ := openPrimaryDiskPair(t, cfg)
			defer primary.Close()
			primary.stopBackgroundWork()
			after, err := primary.usage.disk.ReadStats(ctx)
			if err != nil || primary.usage.Aggregate != nil || primary.usage.owner.accounting == nil || before.Metrics != after.Metrics || before.Totals != after.Totals || before.Window != after.Window {
				t.Fatal("Primary changed retained baseline or selected a resident Usage", before, after, err)
			}
			if err := primary.maintainUsageDiskCycle(ctx, now.AddDate(0, 0, 1)); err != nil {
				t.Fatal("Primary could not complete next retained policy", err)
			}
			archive := doctorTree(t, cfg.UsagePath())
			nextTerm := primary.replication.publisher.Snapshot().Term + 1
			if _, err := primary.replication.publisher.AdoptHigherTerm(nextTerm, nextTerm); err != nil {
				t.Fatal(err)
			}
			if err := primary.usageCollector.CatchUp(ctx); err == nil {
				t.Fatal("old term collector remained authorized")
			}
			if err := primary.maintainUsageDiskCycle(ctx, now.AddDate(0, 0, 2)); err == nil {
				t.Fatal("old term retained-policy maintenance remained authorized")
			}
			actual := doctorTree(t, cfg.UsagePath())
			if len(archive) != len(actual) {
				t.Fatal("withdrawn term changed archive files")
			}
			for path, hash := range archive {
				if actual[path] != hash {
					t.Fatal("withdrawn term changed archive", path)
				}
			}
			if err := primary.Close(); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{standaloneUsageProjectionPath(cfg), primaryUsageLedgerPath(cfg)} {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("Primary close retained derived DB", path, err)
				}
			}
		})
	}
}
