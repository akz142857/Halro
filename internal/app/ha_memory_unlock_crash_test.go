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

	"github.com/akz142857/Halro/internal/store/lock"
)

func TestHAMemoryKMSUnlockActualSIGKILLRestartsFromIndependentSources(t *testing.T) {
	for _, phase := range []string{"unlock-carrier-created", "unlock-carrier-synced", "common-seal-published"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			f, harness := prepareHAKMSMetadataFixture(t, ctx)
			prepareHAMetadataMaintenanceArtifacts(t, f)
			completeHAMemoryActivationBoundary(t, f)
			cfg := f.configs[0]
			dir := t.TempDir()
			spec := haMemoryActivationCrashSpec{Config: cfg, Keys: f.keys, Phase: phase, Marker: filepath.Join(dir, "actual-phase")}
			raw, err := json.Marshal(spec)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "spec.json")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			clear(raw)
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			child := exec.Command(executable, "-test.run", "^TestHAMemoryActivationSIGKILLChild$", "-test.timeout=45s")
			child.Env = append(os.Environ(), "HALRO_HA_MEMORY_ACTIVATION_CHILD="+path)
			output, err := child.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("no actual SIGKILL: %v %s", err, output)
			}
			status, ok := exit.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatalf("wrong termination: %v %s", err, output)
			}
			marker, err := os.ReadFile(spec.Marker)
			if err != nil || string(marker) != phase {
				t.Fatal("wrong persisted kill phase", err)
			}
			guard, err := lock.Acquire(cfg.Storage.DataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			manager := functionalHAMetadataManager(t, cfg)
			a, err := ActivateHAMemoryRuntimeWithOptions(ctx, cfg, f.keys, guard, manager, logger, HAMemoryActivationOptions{Restart: true})
			if err != nil {
				t.Fatal("actual source restart", err)
			}
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
			if phase != "common-seal-published" {
				kept := filepath.Join(cfg.Storage.DataDir, "ha-memory-state.unqualified-0", "roots", "unlock.json")
				info, err := os.Lstat(kept)
				if err != nil || !info.Mode().IsRegular() {
					t.Fatal("unknown prior carrier was not opaquely preserved", err)
				}
				if phase == "unlock-carrier-created" && info.Size() != 0 {
					t.Fatal("creation phase was not the genuine empty inode window")
				}
			}
			key, p, err := unlockHAMemoryCandidate(ctx, cfg, manager, harness.factory)
			if err != nil || len(key) != 32 {
				t.Fatal("fresh independent candidate", err)
			}
			clear(key)
			if err := p.Close(); err != nil {
				t.Fatal(err)
			}
			fresh, selected, err := openHAMemoryActivationRead(ctx, cfg, f.keys, guard, functionalHAMetadataManager(t, cfg), logger, haMemoryStateBootstrapPath(cfg))
			if err != nil {
				t.Fatal("fresh full seven-root qualification", err)
			}
			if err := selected.Close(); err != nil {
				t.Fatal(err)
			}
			if err := fresh.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
