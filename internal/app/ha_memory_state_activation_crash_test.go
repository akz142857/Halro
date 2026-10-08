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

type haMemoryActivationCrashSpec struct {
	Config config.Config
	Keys   HAMetadataKeys
	Phase  string
	Marker string
}

func TestHAMemoryActivationSIGKILLChild(t *testing.T) {
	path := os.Getenv("HALRO_HA_MEMORY_ACTIVATION_CHILD")
	if path == "" {
		t.Skip("private actual SIGKILL child")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s haMemoryActivationCrashSpec
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	defer clear(raw)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	guard, err := lock.Acquire(s.Config.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	_, err = activateHAMemoryRuntime(ctx, s.Config, s.Keys, guard, functionalHAMetadataManager(t, s.Config), slog.New(slog.NewTextHandler(io.Discard, nil)), func(name string) error {
		if name != s.Phase {
			return nil
		}
		file, err := os.OpenFile(s.Marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, writeErr := file.WriteString(name)
		err = errors.Join(writeErr, file.Sync(), file.Close())
		if err != nil {
			return err
		}
		if err := durable.SyncDirectory(filepath.Dir(s.Marker)); err != nil {
			return err
		}
		if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
			return err
		}
		// Signal delivery may be asynchronous. Do not let the operation write
		// beyond the recorded crash boundary while the kernel terminates us.
		select {}
	})
	t.Fatal("actual phase was not killed", err)
}

func TestHAMemoryActivationActualSIGKILLResume(t *testing.T) {
	for _, phase := range []string{"sources-qualified", "namespace-created", "seven-roots-built", "bootstrap-pending-synced", "common-seal-published", "bootstrap-activated"} {
		t.Run(phase, func(t *testing.T) {
			f := prepareHAMetadataMaintenanceFixture(t)
			prepareHAMetadataMaintenanceArtifacts(t, f)
			completeHAMemoryActivationBoundary(t, f)
			cfg := f.configs[0]
			dir := t.TempDir()
			s := haMemoryActivationCrashSpec{Config: cfg, Keys: f.keys, Phase: phase, Marker: filepath.Join(dir, "actual-phase")}
			raw, err := json.Marshal(s)
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
			cmd := exec.Command(executable, "-test.run", "^TestHAMemoryActivationSIGKILLChild$", "-test.timeout=45s")
			cmd.Env = append(os.Environ(), "HALRO_HA_MEMORY_ACTIVATION_CHILD="+path)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("child did not exit by actual SIGKILL: %v %s", err, output)
			}
			status, ok := exit.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatalf("wrong child termination: %v %s", err, output)
			}
			marker, err := os.ReadFile(s.Marker)
			if err != nil || string(marker) != phase {
				t.Fatal("actual persisted kill phase differs", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			guard, err := lock.Acquire(cfg.Storage.DataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			manager := functionalHAMetadataManager(t, cfg)
			if phase != "sources-qualified" {
				if a, err := ActivateHAMemoryRuntime(ctx, cfg, f.keys, guard, manager, logger); err == nil {
					a.Close()
					t.Fatal("default activation overwrote interrupted state")
				}
			}
			a, err := ActivateHAMemoryRuntimeWithOptions(ctx, cfg, f.keys, guard, manager, logger, HAMemoryActivationOptions{Restart: true})
			if err != nil {
				t.Fatal("independently qualified actual restart", err)
			}
			if a.Receipt.Digest == ([32]byte{}) || a.BootstrapPath != haMemoryStateBootstrapPath(cfg) {
				t.Fatal("missing durable actual recovery receipt")
			}
			if err := a.Close(); err != nil {
				t.Fatal("recovery native/readers joined", err)
			}
			fresh, reopened, err := openHAMemoryActivationRead(ctx, cfg, f.keys, guard, functionalHAMetadataManager(t, cfg), logger, haMemoryStateBootstrapPath(cfg))
			if err != nil {
				t.Fatal("post-crash fresh seven-root/native audit", err)
			}
			if err := reopened.Close(); err != nil {
				t.Fatal(err)
			}
			if err := fresh.Close(); err != nil {
				t.Fatal(err)
			}
			if phase == "namespace-created" || phase == "seven-roots-built" || phase == "bootstrap-pending-synced" {
				if _, err := os.Stat(filepath.Join(cfg.Storage.DataDir, "ha-memory-state.unqualified-0", "roots")); err != nil {
					t.Fatal("interrupted roots were not preserved opaquely", err)
				}
			}
		})
	}
}
