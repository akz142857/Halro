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
	"github.com/akz142857/Halro/internal/domain"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"gopkg.in/yaml.v3"
)

func TestUsageSummaryRebuildCLIReportsPublishedDiskRootOnEmptyReopen(t *testing.T) {
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
	for _, slot := range []int{1, 2} {
		out, err := os.Create(filepath.Join(root, "summary-report.json"))
		if err != nil {
			t.Fatal(err)
		}
		previous := os.Stdout
		os.Stdout = out
		err = run([]string{"usage", "rebuild-summary", "--config", path}, logger)
		os.Stdout = previous
		if err != nil {
			_ = out.Close()
			t.Fatal(err)
		}
		if _, err := out.Seek(0, 0); err != nil {
			_ = out.Close()
			t.Fatal(err)
		}
		var report app.SummaryRebuildReport
		if err := json.NewDecoder(out).Decode(&report); err != nil {
			_ = out.Close()
			t.Fatal(err)
		}
		if err := out.Close(); err != nil {
			t.Fatal(err)
		}
		if report.RollupRows != 0 || report.AccountingDays != 0 || report.Watermark.Sequence != 0 || report.RollupVersion != domain.RollupVersion {
			t.Fatal(report)
		}
		s, err := boltstore.OpenReadOnly(cfg.MetadataPath())
		if err != nil {
			t.Fatal(err)
		}
		state, err := s.UsageRollupState()
		if err != nil || state.Slot != slot || state.Watermark != report.Watermark {
			t.Fatal("CLI did not publish the reported root", state, err)
		}
		if _, _, err := s.UsageCheckpoint(); err == nil {
			t.Fatal("empty rebuild fabricated restorable checkpoint")
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
