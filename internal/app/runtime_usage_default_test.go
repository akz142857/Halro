package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/usage"
)

func TestStandaloneUsageDefaultOpenRestoresRetainedDiskWindow(t *testing.T) {
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
			if r.usage.Aggregate != nil || r.usage.disk == nil || r.usage.owner == nil {
				t.Fatal("default retained an Aggregate")
			}
			now := time.Now().UTC()
			seedRetentionUsage(t, r, 3, now.AddDate(0, 0, -120))
			billOneRequest(t, r, "default-current")
			// First tick exports; second tick verifies, publishes the date policy,
			// finishes its original chain and rebinds the live window permission.
			for i := 0; i < 2; i++ {
				if err := r.maintainUsageDiskCycle(ctx, now); err != nil {
					t.Fatal(i, err)
				}
			}
			before, err := r.usage.disk.ReadStats(ctx)
			position, positionErr := r.usageExporter.ReadArchivePosition(ctx)
			if err != nil || positionErr != nil || before.Window.Attempts != 1 || before.Metrics.RequestsSuccess != 4 || position.Retention == (usage.ArchiveDateRetention{}) {
				t.Fatal(before, position, err, positionErr)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(standaloneUsageProjectionPath(cfg)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("clean close retained derivative", err)
			}
			// No test-only selector: ordinary Open must verify the actual retained
			// catalog against a fresh native replay before restoring the window.
			next, err := Open(ctx, cfg, logger)
			if err != nil {
				t.Fatal(err)
			}
			defer next.Close()
			next.stopBackgroundWork()
			after, err := next.usage.disk.ReadStats(ctx)
			if err != nil || next.usage.Aggregate != nil || after.Metrics != before.Metrics || after.Totals != before.Totals || after.Window != before.Window || after.Watermark != before.Watermark {
				t.Fatal("default restart changed facts/window", before, after, err)
			}
			billOneRequest(t, next, "default-after-restart")
			if err := next.maintainUsageDiskCycle(ctx, now.AddDate(0, 0, 1)); err != nil {
				t.Fatal(err)
			}
			stats, err := next.usage.disk.ReadStats(ctx)
			if err != nil || stats.Window.Attempts != 2 || stats.Metrics.RequestsSuccess != 5 {
				t.Fatal(stats, err)
			}
		})
	}
}

func TestStandaloneUsageDefaultFailedArchiveOpenReleasesOwners(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.UsagePath(), 0700); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(cfg.UsagePath(), "manifest.json")
	if err := os.WriteFile(manifest, []byte("invalid archive"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := Open(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || r != nil {
		t.Fatal("bad archive published runtime", err)
	}
	for _, path := range []string{standaloneUsageProjectionPath(cfg), standaloneProjectionPath(cfg)} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("failed Open retained owned DB", path, err)
		}
	}
	guard, err := lock.AcquireExistingReadOnly(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal("failed Open retained directory lock", err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	metadata, err := boltstore.OpenReadOnly(cfg.MetadataPath())
	if err != nil {
		t.Fatal("failed Open retained Metadata writer", err)
	}
	if err := metadata.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(manifest); err != nil {
		t.Fatal(err)
	}
	next, err := Open(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal("repair did not permit fresh Open", err)
	}
	defer next.Close()
}
