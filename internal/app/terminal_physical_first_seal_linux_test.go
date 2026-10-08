//go:build linux

package app

import (
	"bytes"
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

	"github.com/akz142857/Halro/internal/resourcebudget"
	"golang.org/x/sys/unix"
)

func TestStandalonePhysicalFirstSealActualSIGKILLNoDebtRecovery(t *testing.T) {
	var fs unix.Statfs_t
	if err := unix.Statfs(os.TempDir(), &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Type != unix.EXT4_SUPER_MAGIC {
		t.Skip("requires isolated ext4 physical fixture")
	}
	for _, phase := range []string{"physical-seal-control-created-0", "physical-seal-control-allocated-0", "physical-seal-control-created-1", "physical-seal-control-allocated-1", "physical-seal-physical-target-created", "physical-seal-physical-target-allocated", "physical-seal-first-valid-receipt"} {
		t.Run(phase, func(t *testing.T) {
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
			sealBefore, err := os.ReadFile(filepath.Join(cfg.Storage.DataDir, "active.seal"))
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(t.TempDir(), "child.json")
			if err := os.WriteFile(configPath, raw, 0600); err != nil {
				t.Fatal(err)
			}
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStandalonePhysicalPreparationSIGKILLHelper$", "-test.count=1")
			child.Env = append(os.Environ(), "HALRO_PHYSICAL_PREPARATION_CHILD="+configPath, "HALRO_PHYSICAL_FIRST_SEAL_PHASE="+phase)
			output, err := child.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || !exit.ProcessState.Sys().(syscall.WaitStatus).Signaled() || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("actual phase SIGKILL absent: %v %s", err, output)
			}
			trace := filepath.Join(cfg.Storage.DataDir, "terminal.seal.0")
			captured, err := os.Lstat(trace)
			if err != nil {
				t.Fatal(err)
			}
			sealAfter, err := os.ReadFile(filepath.Join(cfg.Storage.DataDir, "active.seal"))
			if err != nil || !bytes.Equal(sealBefore, sealAfter) {
				t.Fatal("preparation changed independently selected content", err)
			}
			r, err := OpenWithOptions(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{resourceSampleSource: pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() })})
			if err != nil {
				t.Fatal("actual independently selected NoDebt recovery", err)
			}
			defer r.Close()
			r.stopBackgroundWork()
			preserved := trace
			if phase != "physical-seal-first-valid-receipt" {
				preserved = filepath.Join(cfg.Storage.DataDir, "terminal-seal.unqualified-0", "terminal.seal.0")
			}
			after, err := os.Lstat(preserved)
			if err != nil || !os.SameFile(captured, after) {
				t.Fatal("unqualified source inode deleted or replaced", err)
			}
			request, err := r.accounting.BeginRequestDetailed(ctx, credentials.ProjectID, credentials.KeyID, "req_first_physical_seal_crash", "chat")
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
				t.Fatal("actual owners did not join", err)
			}
		})
	}
}
