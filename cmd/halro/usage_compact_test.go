package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/app"
	"github.com/akz142857/Halro/internal/config"
	"gopkg.in/yaml.v3"
)

func TestUsageCompactCLIReportsCompletedDiskArchiveAndReopensRoot(t *testing.T) {
	cfg := config.Default()
	root := t.TempDir()
	cfg.Storage.DataDir = filepath.Join(root, "data")
	cfg.Storage.MasterKey.File = filepath.Join(root, "master.key")
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if err := app.Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for i := 0; i < 2; i++ {
		out, err := os.Create(filepath.Join(root, "compact-report.json"))
		if err != nil {
			t.Fatal(err)
		}
		previous := os.Stdout
		os.Stdout = out
		err = run([]string{"usage", "compact", "--config", path}, logger)
		os.Stdout = previous
		if err != nil {
			_ = out.Close()
			t.Fatal(err)
		}
		if _, err := out.Seek(0, 0); err != nil {
			_ = out.Close()
			t.Fatal(err)
		}
		var report app.UsageCompactionReport
		if err := json.NewDecoder(out).Decode(&report); err != nil {
			_ = out.Close()
			t.Fatal(err)
		}
		if err := out.Close(); err != nil {
			t.Fatal(err)
		}
		if report.Steps != 1 || report.WrittenRows != 0 || report.ThroughSequence != 0 || report.Archive.CatalogVersion != 0 || !report.Archive.CompletePrefix || report.Verification.Role != "standalone" || report.Verification.LedgerRecords != 0 || report.Verification.ParquetRecords != 0 {
			t.Fatal("CLI did not report the complete authenticated empty target", report)
		}
	}
}
