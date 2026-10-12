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

func TestStandalonePhysicalPreparationSIGKILLHelper(t *testing.T) {
	path := os.Getenv("HALRO_PHYSICAL_PREPARATION_CHILD")
	if path == "" {
		t.Skip("subprocess helper")
	}
	var cfg config.Config
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	options := OpenOptions{resourceSampleSource: pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() })}
	if os.Getenv("HALRO_TERMINAL_ACTUAL_OS_PRESSURE") == "1" {
		// Fresh-process evidence must use the same real OS/cgroup sampler as
		// its parent. Deterministic mechanism fixtures retain their own sample.
		options.resourceSampleSource = nil
	}
	if phase := os.Getenv("HALRO_PHYSICAL_FIRST_SEAL_PHASE"); phase != "" {
		options.physicalPreparationPhase = func(reached string) error {
			if reached == phase {
				if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
					return err
				}
				// kill may return before delivery; park so no later step runs.
				select {}
			}
			return nil
		}
	}
	r, err := OpenWithOptions(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), options)
	if err != nil {
		t.Fatal(err)
	}
	r.stopBackgroundWork()
	if requestID := os.Getenv("HALRO_PHYSICAL_CRASH_REQUEST"); requestID != "" {
		request, found, err := r.selectedState.generation.Owners.Ledger.ReadRequest(ctx, requestID)
		if err != nil || !found || request.Finalized == nil {
			t.Fatal("post-repack crash source lost terminal", request, found, err)
		}
	}
	if requestID := os.Getenv("HALRO_PHYSICAL_VERIFY_REQUEST"); requestID != "" {
		request, found, err := r.selectedState.generation.Owners.Ledger.ReadRequest(ctx, requestID)
		if err != nil || !found || request.Finalized == nil {
			t.Fatal("fresh process lost completed terminal", request, found, err)
		}
		window, err := r.ledger.ReservePhysicalTerminals(ctx, 2)
		if err != nil || r.ledger.PhysicalTerminalDebt() != 2 {
			t.Fatal("fresh process lacks actual prepaid vector", err)
		}
		if err := window.Close(); err != nil {
			t.Fatal(err)
		}
		if err := r.drainAccountingBusiness(ctx, nil); err != nil {
			t.Fatal(err)
		}
		if err := r.Close(); err != nil {
			t.Fatal("fresh process owner exit", err)
		}
		return
	}
	if _, err := r.ledger.ReservePhysicalTerminals(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if r.ledger.PhysicalTerminalDebt() != 2 || r.ledgerAppendDisk.physicalUnused <= 0 {
		t.Fatal("actual vector debt was not captured")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	t.Fatal("SIGKILL returned")
}

func TestStandalonePhysicalPreparationSIGKILLRecoversActualOwners(t *testing.T) {
	var filesystem unix.Statfs_t
	if err := unix.Statfs(os.TempDir(), &filesystem); err != nil {
		t.Fatal(err)
	}
	if filesystem.Type != unix.EXT4_SUPER_MAGIC {
		t.Skip("requires dedicated ext4 physical ownership fixture")
	}
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
	configPath := filepath.Join(t.TempDir(), "child.json")
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStandalonePhysicalPreparationSIGKILLHelper$", "-test.count=1")
	child.Env = append(os.Environ(), "HALRO_PHYSICAL_PREPARATION_CHILD="+configPath)
	output, err := child.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || !exit.ProcessState.Sys().(syscall.WaitStatus).Signaled() || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("physical preparation did not reach actual SIGKILL: %v %s", err, output)
	}
	for i := 0; i < 2; i++ {
		if _, err := os.Lstat(filepath.Join(cfg.Storage.DataDir, "memory-state", "ledger", "terminal-root-0"+string(rune('0'+i)))); err != nil {
			t.Fatal("prebuilt inode did not survive", err)
		}
	}
	r, err := OpenWithOptions(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{resourceSampleSource: pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() })})
	if err != nil {
		t.Fatal("authenticated physical preparation recovery", err)
	}
	defer r.Close()
	r.stopBackgroundWork()
	if r.ledger.PhysicalTerminalDebt() != 0 || r.ledgerAppendDisk.physicalUnused != 0 {
		t.Fatal("restart guessed away or reloaned native target debt", r.ledger.PhysicalTerminalDebt(), r.ledgerAppendDisk.physicalUnused)
	}
	request, err := r.accounting.BeginRequestDetailed(ctx, credentials.ProjectID, credentials.KeyID, "req_after_physical_preparation_crash", "chat")
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
		t.Fatal("recovered actual owners did not join", err)
	}
}
