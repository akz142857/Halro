package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/config"
)

func TestRuntimeRefusesClusterStateWithoutReplicationBeforeOpeningStandaloneStores(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.DataDir = filepath.Join(t.TempDir(), "data")
	clusterPath := filepath.Join(cfg.Storage.DataDir, "cluster")
	if err := os.MkdirAll(clusterPath, 0o700); err != nil {
		t.Fatal(err)
	}
	runtime, err := OpenWithOptions(
		context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{},
	)
	if runtime != nil || err == nil || !strings.Contains(err.Error(), "cannot downgrade") {
		t.Fatalf("runtime=%v downgrade error=%v", runtime, err)
	}
	entries, err := os.ReadDir(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "cluster" {
		t.Fatalf("Standalone open touched member data before refusal: %#v", entries)
	}
}

func TestRuntimeRefusesConfiguredMemberWithoutStateBeforeOpeningStores(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.DataDir = filepath.Join(t.TempDir(), "data")
	cfg.Replication = &config.Replication{}
	runtime, err := OpenWithOptions(
		context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{},
	)
	if runtime != nil || err == nil || !strings.Contains(err.Error(), "join or re-seed") {
		t.Fatalf("runtime=%v missing member-state error=%v", runtime, err)
	}
	if _, statErr := os.Stat(cfg.Storage.DataDir); !os.IsNotExist(statErr) {
		t.Fatalf("configured member open touched data directory: %v", statErr)
	}
}

func TestRuntimeRefusesConfiguredMemberWithoutAuthenticatedStateBeforeOpeningStores(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.DataDir = filepath.Join(t.TempDir(), "data")
	cfg.Replication = &config.Replication{}
	if err := os.MkdirAll(filepath.Join(cfg.Storage.DataDir, "cluster"), 0o700); err != nil {
		t.Fatal(err)
	}
	runtime, err := OpenWithOptions(
		context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{},
	)
	if runtime != nil || err == nil || !strings.Contains(err.Error(), "authenticated member state is absent") {
		t.Fatalf("runtime=%v missing authenticated member-state error=%v", runtime, err)
	}
	entries, err := os.ReadDir(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "cluster" {
		t.Fatalf("member open touched data before refusal: %#v", entries)
	}
}
