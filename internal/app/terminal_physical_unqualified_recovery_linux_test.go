//go:build linux

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
	"github.com/akz142857/Halro/internal/resourcebudget"
	"golang.org/x/sys/unix"
)

func TestStandalonePhysicalUnqualifiedTraceSIGKILLHelper(t *testing.T) {
	path := os.Getenv("HALRO_PHYSICAL_UNQUALIFIED_CHILD")
	if path == "" {
		t.Skip("subprocess helper")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	// The trace deliberately carries no receipt or ownership authority. This
	// tests Runtime's independent NoDebt gate, not a fabricated signed header.
	trace := filepath.Join(cfg.Storage.DataDir, "memory-state", "ledger", os.Getenv("HALRO_PHYSICAL_UNQUALIFIED_TRACE"))
	f, err := os.OpenFile(trace, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		t.Fatal(err)
	}
	directory, err := os.Open(filepath.Dir(trace))
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(directory.Sync(), directory.Close()); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	t.Fatal("SIGKILL returned")
}

func TestStandalonePhysicalNoDebtRestartsActualUnqualifiedTrace(t *testing.T) {
	var fs unix.Statfs_t
	if err := unix.Statfs(os.TempDir(), &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Type != unix.EXT4_SUPER_MAGIC {
		t.Skip("requires dedicated ext4")
	}
	for _, trace := range []string{"terminal.physical.0", "terminal-root-00"} {
		t.Run(trace, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			legacy, credentials, _ := openBootstrappedRuntimeShaped(t, "gpt-test", nil)
			cfg, instance := legacy.config, legacy.instanceID
			if err := legacy.Close(); err != nil {
				t.Fatal(err)
			}
			limits := physicalTerminalBudgetFixture()
			limits.PhysicalTerminalReserve = true
			cfg.ResourceBudget = &limits
			cfg = activateClosedRuntimeMemoryStateFixture(t, ctx, cfg, instance, false)
			path := filepath.Join(t.TempDir(), "config.json")
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStandalonePhysicalUnqualifiedTraceSIGKILLHelper$", "-test.count=1")
			child.Env = append(os.Environ(), "HALRO_PHYSICAL_UNQUALIFIED_CHILD="+path, "HALRO_PHYSICAL_UNQUALIFIED_TRACE="+trace)
			output, err := child.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || !exit.ProcessState.Sys().(syscall.WaitStatus).Signaled() || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("not actual trace SIGKILL: %v %s", err, output)
			}
			original, err := os.Stat(filepath.Join(cfg.Storage.DataDir, "memory-state", "ledger", trace))
			if err != nil {
				t.Fatal(err)
			}
			r, err := OpenWithOptions(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{resourceSampleSource: pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() })})
			if err != nil {
				t.Fatal("actual independent NoDebt gate did not recover", err)
			}
			defer r.Close()
			r.stopBackgroundWork()
			preserved, err := os.Stat(filepath.Join(cfg.Storage.DataDir, "memory-state", "ledger", "terminal-unqualified-0", trace))
			if err != nil || !os.SameFile(original, preserved) {
				t.Fatal("Runtime trusted, deleted or replaced unknown trace", err)
			}
			window, err := r.ledger.ReservePhysicalTerminals(ctx, 2)
			if err != nil {
				t.Fatal(err)
			}
			if err := window.Close(); err != nil {
				t.Fatal(err)
			}
			request, err := r.accounting.BeginRequestDetailed(ctx, credentials.ProjectID, credentials.KeyID, "req_after_unqualified_trace", "chat")
			if err != nil {
				t.Fatal(err)
			}
			if err := r.accounting.Finalize(ctx, request, "success"); err != nil {
				t.Fatal(err)
			}
			if err := r.drainAccountingBusiness(ctx, nil); err != nil {
				t.Fatal(err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
