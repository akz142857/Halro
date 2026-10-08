package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/app"
	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/config"
	"gopkg.in/yaml.v3"
)

func TestBackupCLIUsesPagedCreationAndCompleteVerification(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
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
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{83}, 32)
	keyPath := filepath.Join(root, "backup.key")
	if err := os.WriteFile(keyPath, key, 0600); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(root, "backup.hmbk")
	var backupID string
	for _, command := range [][]string{{"backup", "create", "--config", configPath, "--output", archivePath, "--key-file", keyPath}, {"backup", "verify", "--file", archivePath, "--key-file", keyPath}, {"backup", "verify", "--config", configPath, "--file", archivePath, "--key-file", keyPath}} {
		output, err := os.Create(filepath.Join(t.TempDir(), "report.json"))
		if err != nil {
			t.Fatal(err)
		}
		previous := os.Stdout
		os.Stdout = output
		runErr := run(command, slog.New(slog.NewTextHandler(io.Discard, nil)))
		os.Stdout = previous
		if runErr != nil {
			output.Close()
			t.Fatal(runErr)
		}
		if _, err := output.Seek(0, io.SeekStart); err != nil {
			output.Close()
			t.Fatal(err)
		}
		var report struct {
			backup.ArchiveSummary
			Files []backup.File `json:"files"`
		}
		decoder := json.NewDecoder(output)
		err = decoder.Decode(&report)
		tailErr := decoder.Decode(new(any))
		output.Close()
		if err != nil || !errors.Is(tailErr, io.EOF) {
			t.Fatal(err, tailErr)
		}
		if report.FormatVersion != 5 || report.CompatibilityVersion != 3 || len(report.Files) < 5 || uint64(len(report.Files)) != report.Catalog.Files {
			t.Fatal(report.FormatVersion, report.Catalog, len(report.Files))
		}
		if backupID == "" {
			backupID = report.BackupID
		} else if backupID != report.BackupID {
			t.Fatal("create and verify IDs differ")
		}
	}
	h, err := backup.OpenVerified(context.Background(), archivePath, key)
	if err != nil {
		t.Fatal(err)
	}
	if h.Summary().FormatVersion != 5 || h.Summary().BackupID != backupID {
		t.Fatal(h.Summary().FormatVersion)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
}
