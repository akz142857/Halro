package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/app"
	"github.com/akz142857/Halro/internal/config"
	"gopkg.in/yaml.v3"
)

func TestLedgerSealCLIStreamsCompatibleJSON(t *testing.T) {
	root := t.TempDir()
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	cfg := config.Default()
	cfg.Storage.DataDir = filepath.Join(root, "data")
	cfg.Storage.MasterKey.File = filepath.Join(root, "master.key")
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if err := app.Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	output, err := os.Create(filepath.Join(root, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	os.Stdout = output
	defer func() { os.Stdout = previous; _ = output.Close() }()
	err = run([]string{"ledger", "seal", "--config", path}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Stdout = previous
	if err != nil {
		t.Fatal(err)
	}
	if _, err := output.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(output)
	var report app.SealReport
	if err := decoder.Decode(&report); err != nil {
		t.Fatal(err)
	}
	if report.Rolled || report.ActiveGeneration != 1 || report.SealedGenerations != 0 {
		t.Fatalf("report=%+v", report)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		t.Fatalf("extra report data: %v", err)
	}
	files, err := os.ReadDir(scratch)
	if err != nil || len(files) != 0 {
		t.Fatalf("CLI leaked temporary files: %v %v", files, err)
	}
}
