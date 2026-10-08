package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/app"
	"github.com/akz142857/Halro/internal/config"
	"gopkg.in/yaml.v3"
)

func TestKeyRotateGroupActualDispatchAndFixedFlagBounds(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, args := range [][]string{
		{"key", "rotate-group"},
		{"key", "rotate-group", "--member-config", "a", "--member-config", "b", "--new-key-file", "new", "--kms", "--operation-id", "op"},
		{"key", "rotate-group", "--member-config", "a", "--new-key-file", "new", "--operation-id", "op"},
		{"key", "rotate-group", "--member-config", "a", "--member-config", "a", "--new-key-file", "new", "--operation-id", "op"},
		{"key", "rotate-group", "--member-config", "a", "--member-config", "b", "--member-config", "c", "--member-config", "d", "--new-key-file", "new", "--operation-id", "op"},
		{"key", "rotate-group", "--member-config", "a", "--member-config", "b", "--new-key-file", "new", "--operation-id", "op", "--timeout", "0"},
		{"key", "rotate-group", "--member-config", "a", "--member-config", "b", "--new-key-file", "new", "--operation-id", "op", "unexpected"},
	} {
		if err := run(args, logger); err == nil || !strings.Contains(err.Error(), "group") {
			t.Fatalf("real dispatch bounds failed: %v %v", args, err)
		}
	}
}
func TestKeyRotateGroupLoadsEveryConfigAndForwardsFiniteFileCommand(t *testing.T) {
	dir := t.TempDir()
	var names [3]string
	for n := range names {
		cfg := config.Default()
		cfg.Storage.DataDir = filepath.Join(dir, string(rune('a'+n)))
		cfg.Storage.MasterKey.File = filepath.Join(cfg.Storage.DataDir, "master.key")
		raw, err := yaml.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		names[n] = filepath.Join(dir, string(rune('a'+n))+".yaml")
		if err := os.WriteFile(names[n], raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, count := range []int{2, 3} {
		args := []string{"--new-key-file", "replacement.key", "--operation-id", "real-group-op", "--timeout", "5m"}
		for n := 0; n < count; n++ {
			args = append(args, "--member-config", names[n])
		}
		called := 0
		expected := errors.New("actual delegated File operation error")
		err := runKeyGroupRotationWithCommand(args, func(ctx context.Context, cfgs []config.Config, key, op string) (app.HAGroupKeyRotationResult, error) {
			called++
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 5*time.Minute || time.Until(deadline) < 4*time.Minute || len(cfgs) != count || key != "replacement.key" || op != "real-group-op" {
				t.Fatal("wrong bounded command", count, key, op)
			}
			for n, cfg := range cfgs {
				if cfg.Storage.DataDir != filepath.Join(dir, string(rune('a'+n))) || cfg.Storage.MasterKey.Mode != config.MasterKeyModeFile {
					t.Fatal("wrong actual loaded config", n, cfg.Storage)
				}
			}
			return app.HAGroupKeyRotationResult{}, expected
		})
		if !errors.Is(err, expected) || called != 1 {
			t.Fatal("command result lost", count, called, err)
		}
	}
}
