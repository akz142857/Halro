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
)

func TestUsageDateCandidateSchemaGuardAndOfflineExplicitScope(t *testing.T) {
	for _, format := range []string{usage.FormatNDJSON, usage.FormatParquet} {
		t.Run(format, func(t *testing.T) {
			r, _ := summaryRuntime(t, 2)
			r.stopBackgroundWork()
			cfg := r.config
			cfg.Usage.ExportFormat = format
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := exportLegacyUsageFixture(cfg); err != nil {
				t.Fatal(err)
			}
			e, err := usage.NewExporterWithOptions(cfg.UsagePath(), usage.Options{Format: format})
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			if _, err := e.MigrateArchiveCatalog(context.Background(), time.Minute); err != nil {
				t.Fatal(err)
			}
			cutoff := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
			var candidate usage.DateCatalogBuildProgress
			for i := 0; i < 16; i++ {
				candidate, err = e.PrepareRetainedDateCatalogStep(context.Background(), cutoff, usage.DefaultDateCatalogBuildLimits())
				if err != nil {
					t.Fatal(err)
				}
				if candidate.Phase == "ready" {
					break
				}
			}
			if candidate.Phase != "ready" || candidate.Candidate.Files != 0 {
				t.Fatal(candidate)
			}
			rootPath := filepath.Join(cfg.UsagePath(), "manifest.json")
			before, err := os.ReadFile(rootPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(before, []byte(`"schema_version":9`)) {
				t.Fatal("candidate activated live schema", string(before))
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			// Fixture-only activation exercises consumers of the new format. The
			// production candidate API grants no publication/deletion authority.
			if err := os.WriteFile(rootPath, candidate.RootJSON, 0o600); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				opened, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
				if opened != nil {
					_ = opened.Close()
					t.Fatal("unsigned candidate published default Runtime")
				}
				if err == nil || !errors.Is(err, usage.ErrArchiveDateProof) {
					t.Fatal("missing schema/mode gate or leaked directory ownership", err)
				}
			}
			report, err := VerifyUsageRetainedDates(context.Background(), cfg, cutoff)
			if err != nil || report.RequiredRows != 0 || report.ExpiredOmittedRows != 2 || report.ParquetRecords != 0 || report.RetainFromDate != "9999-01-01" {
				t.Fatal(report, err)
			}
			if report, err := VerifyUsage(context.Background(), cfg); err == nil || report.ThroughSequence != 0 {
				t.Fatal("root metadata silently chose verification policy", report, err)
			}
			after, err := os.ReadFile(rootPath)
			if err != nil || !bytes.Equal(after, candidate.RootJSON) {
				t.Fatal("consumer rewrote date coverage", err)
			}
		})
	}
}
