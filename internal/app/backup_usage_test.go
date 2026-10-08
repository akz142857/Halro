package app

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/usage"
)

func TestRetainedDiskUsageBackupRestoreKeepsPolicyAndWindow(t *testing.T) {
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
			billOneRequest(t, r, "retained-backup-current")
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
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(configPath, []byte("version: 2\n"), 0600); err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(t.TempDir(), "retained.hmbk")
			key := bytes.Repeat([]byte{0x53}, 32)
			manifest, err := CreateBackup(ctx, cfg, configPath, archive, key)
			if err != nil {
				t.Fatal(err)
			}
			var pages, controls int
			for _, file := range manifest.Files {
				if strings.HasPrefix(file.Path, "data/usage/catalog/v1/") {
					pages++
				}
				if strings.HasPrefix(file.Path, "data/usage/.archive-date-") {
					controls++
				}
			}
			if pages == 0 || controls != 3 || manifest.UsageManifestVersion != 10 {
				t.Fatal("backup omitted retained catalog/control state", pages, controls, manifest.UsageManifestVersion)
			}
			if _, err := RestoreBackup(ctx, cfg, archive, key, manifest.BackupID); err != nil {
				t.Fatal(err)
			}
			next, err := Open(ctx, cfg, logger)
			if err != nil {
				t.Fatal(err)
			}
			defer next.Close()
			next.stopBackgroundWork()
			after, err := next.usage.disk.ReadStats(ctx)
			if err != nil || after.Metrics != before.Metrics || after.Totals != before.Totals || after.Window != before.Window {
				t.Fatal("restore changed cumulative facts/window", before, after, err)
			}
			if _, err := next.maintainUsageDiskDates(ctx, now.AddDate(0, 0, 1)); err != nil {
				t.Fatal("restored policy cannot advance", err)
			}
		})
	}
}
