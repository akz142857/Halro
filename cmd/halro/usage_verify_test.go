package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/app"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/usage"
	"gopkg.in/yaml.v3"
)

func TestUsageVerifyCLIReportsExplicitEmptyNativeInterval(t *testing.T) {
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
	if err := os.MkdirAll(cfg.UsagePath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.UsagePath(), "manifest.json"), []byte(`{"schema_version":8,"last_sequence":0,"files":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, err := os.Create(filepath.Join(root, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	os.Stdout = stdout
	defer func() { os.Stdout = previous; _ = stdout.Close() }()
	err = run([]string{"usage", "verify", "--config", path, "--after-sequence", "0"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Stdout = previous
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stdout.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := json.NewDecoder(stdout).Decode(&report); err != nil {
		t.Fatal(err)
	}
	if report["role"] != "standalone" || report["ledger_records"] != float64(0) || report["parquet_records"] != float64(0) || report["after_sequence"] != float64(0) || report["through_sequence"] != float64(0) || report["ordering_index"] != float64(0) {
		t.Fatal(report)
	}
	for _, field := range []string{"source_head", "allowed_ledger_head", "native_head"} {
		if report[field] == nil {
			t.Fatal("scope omitted from empty verification", report)
		}
	}
	if err := run([]string{"usage", "verify", "--config", path, "--after-sequence", "1"}, slog.New(slog.NewTextHandler(io.Discard, nil))); !errors.Is(err, usage.ErrDiskUsageCursor) {
		t.Fatal("CLI ignored explicit interval beyond archive H", err)
	}
}

func TestUsageVerifyCLIRejectsRangeFlagForOtherCommandsAndNegativeSequence(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, command := range []string{"compact", "prune", "rebuild-summary"} {
		if err := run([]string{"usage", command, "--after-sequence", "10"}, logger); err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
			t.Fatal(command, err)
		}
	}
	if err := run([]string{"usage", "verify", "--after-sequence", "-1"}, logger); err == nil || !strings.Contains(err.Error(), "invalid value") {
		t.Fatal(err)
	}
}

func TestUsageVerifyCLIRejectsAmbiguousOrInvalidDateScope(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, date := range []string{"", "2026-2-1", "2026-02-30", "0000-01-01", "0001-01-01", "2026-10-05T00:00:00Z"} {
		if err := run([]string{"usage", "verify", "--retain-from", date}, logger); err == nil || !strings.Contains(err.Error(), "invalid --retain-from") {
			t.Fatal(date, err)
		}
	}
	if err := run([]string{"usage", "verify", "--retain-from", "2026-10-05", "--after-sequence", "0"}, logger); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatal(err)
	}
	for _, command := range []string{"compact", "prune", "rebuild-summary"} {
		if err := run([]string{"usage", command, "--retain-from", "2026-10-05"}, logger); err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
			t.Fatal(command, err)
		}
	}
}

func TestUsageVerifyCLIReportsExplicitDateScopeOnEmptyNative(t *testing.T) {
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
	if err := os.MkdirAll(cfg.UsagePath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.UsagePath(), "manifest.json"), []byte(`{"schema_version":8,"last_sequence":0,"files":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, err := os.Create(filepath.Join(root, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	os.Stdout = stdout
	defer func() { os.Stdout = previous; _ = stdout.Close() }()
	err = run([]string{"usage", "verify", "--config", path, "--retain-from", "2026-10-05"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Stdout = previous
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stdout.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	var report usage.ReconciliationReport
	if err := json.NewDecoder(stdout).Decode(&report); err != nil {
		t.Fatal(err)
	}
	if report.RetainFromDate != "2026-10-05" || report.Role != "standalone" || report.LedgerRecords != 0 || report.ParquetRecords != 0 || report.AfterSequence != 0 || report.ThroughSequence != 0 {
		t.Fatal(report)
	}
}
