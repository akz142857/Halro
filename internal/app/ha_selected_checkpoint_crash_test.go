package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/store/lock"
)

type haSelectedCheckpointCrashSpec struct {
	Config        config.Config
	Keys          HAMetadataKeys
	Phase, Marker string
}

func TestHASelectedCheckpointSIGKILLChild(t *testing.T) {
	path := os.Getenv("HALRO_HA_SELECTED_CHECKPOINT_CHILD")
	if path == "" {
		t.Skip("private actual SIGKILL child")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s haSelectedCheckpointCrashSpec
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	clear(raw)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	guard, err := lock.Acquire(s.Config.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx = context.WithValue(ctx, haSelectedCheckpointPhaseKey{}, func(phase string) {
		if phase != s.Phase {
			return
		}
		file, err := os.OpenFile(s.Marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := file.WriteString(phase)
		if err := errors.Join(writeErr, file.Sync(), file.Close(), durable.SyncDirectory(filepath.Dir(s.Marker))); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
	})
	checkpoint, err := CheckpointHASelectedRuntime(ctx, s.Config, s.Keys.Master, guard, functionalHAMetadataManager(t, s.Config), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if checkpoint != nil {
		defer checkpoint.Close()
	}
	t.Fatal("actual checkpoint kill phase not reached", s.Phase, err)
}

func TestHASelectedCheckpointActualSIGKILLRetainsFixedNamespacesAndReopens(t *testing.T) {
	for role := range 2 {
		for _, phase := range []string{"roots-ready", "audit-capsule", "governance-capsule", "published"} {
			t.Run([]string{"primary", "replica"}[role]+"/"+phase, func(t *testing.T) {
				stressMatrixCase(t, role == 0 && (phase == "roots-ready" || phase == "governance-capsule") || role == 1 && phase == "published")
				ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
				defer cancel()
				fixture, owners, _ := prepareHASelectedTransportFixture(t, ctx)
				for _, owner := range owners {
					if err := owner.Close(); err != nil {
						t.Fatal(err)
					}
					if err := owner.owners.Components.Guard.Close(); err != nil {
						t.Fatal(err)
					}
				}
				directory := t.TempDir()
				s := haSelectedCheckpointCrashSpec{Config: fixture.configs[role], Keys: fixture.keys, Phase: phase, Marker: filepath.Join(directory, "actual-phase")}
				raw, err := json.Marshal(s)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(directory, "spec.json")
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				clear(raw)
				binary, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(binary, "-test.run", "^TestHASelectedCheckpointSIGKILLChild$", "-test.timeout=40s")
				cmd.Env = append(os.Environ(), "HALRO_HA_SELECTED_CHECKPOINT_CHILD="+path)
				output, err := cmd.CombinedOutput()
				var exited *exec.ExitError
				if !errors.As(err, &exited) {
					t.Fatalf("actual child not killed: %v %s", err, output)
				}
				status, ok := exited.Sys().(syscall.WaitStatus)
				if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
					t.Fatalf("wrong child termination: %v %s", err, output)
				}
				marker, err := os.ReadFile(s.Marker)
				if err != nil || string(marker) != phase {
					t.Fatal("unproven physical kill boundary", phase, err)
				}
				// The first candidate is immutable evidence, including a half pair.
				var prior [2][]byte
				for i := range 2 {
					prior[i], err = os.ReadFile(haSelectedCachePath(s.Config.Storage.DataDir, 1, i == 1))
					if err != nil && !errors.Is(err, os.ErrNotExist) {
						t.Fatal(err)
					}
				}
				guard, err := lock.Acquire(s.Config.Storage.DataDir)
				if err != nil {
					t.Fatal(err)
				}
				defer guard.Close()
				logger := slog.New(slog.NewTextHandler(io.Discard, nil))
				if phase != "published" {
					checkpoint, err := CheckpointHASelectedRuntime(ctx, s.Config, s.Keys.Master, guard, functionalHAMetadataManager(t, s.Config), logger)
					if checkpoint != nil {
						defer checkpoint.Close()
					}
					if err != nil || checkpoint.Publication.Seal.Revision != 2 {
						t.Fatal("actual independently requalified retry", phase, err)
					}
					if err := checkpoint.Close(); err != nil {
						t.Fatal(err)
					}
				}
				fresh, err := openHASelectedRuntimeFoundation(ctx, s.Config, s.Keys.Master, guard, functionalHAMetadataManager(t, s.Config), logger)
				if fresh != nil {
					defer fresh.Close()
				}
				if err != nil {
					t.Fatal("post-SIGKILL native/carrier/common permission", phase, err)
				}
				if err := fresh.prepareSelectedServices(ctx); err != nil {
					t.Fatal("post-SIGKILL selected seven-root reopen", phase, err)
				}
				if fresh.reopened.Seal.Revision != 2 || fresh.reopened.Seal.RootCount != 7 || fresh.replication.startupReady.Load() {
					t.Fatal("SIGKILL recovery forged cut or Ready", fresh.reopened.Seal.Revision, fresh.reopened.Seal.RootCount)
				}
				for i, raw := range prior {
					if len(raw) > 0 {
						actual, err := os.ReadFile(haSelectedCachePath(s.Config.Storage.DataDir, 1, i == 1))
						if err != nil || string(actual) != string(raw) {
							t.Fatal("old interrupted creator was guessed/deleted", phase, i, err)
						}
					}
				}
			})
		}
	}
}
