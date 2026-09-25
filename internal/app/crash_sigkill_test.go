package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/store/lock"
)

// Killing Halro with SIGKILL, with accounting writes in flight.
//
// The 2026-09-18 production validation run did this by hand (R-18) and the
// record says what was wrong with that evidence twice over: it was manual, and
// it ran against a data directory with **no traffic and no accounting records**
// — so it proved the process, the lock and the chains, and said nothing about
// the thing crash recovery exists for, which is the terminal state of
// reservations and settlements that were in flight. The repository had no
// SIGKILL test at all (260918-PV-F-21).
//
// SIGKILL cannot be delivered to oneself and then observed, so the test
// re-executes its own binary: the child opens the data directory and bills
// request lifecycles in a loop, reporting each one it has fully settled; the
// parent kills it at an arbitrary point and then has to find every request the
// child claimed.
//
// It runs in the ordinary suite rather than behind a release gate. The finding
// is that the evidence was manual; moving it to a gate nobody runs on a normal
// day would keep it manual under a different name.

const (
	crashChildEnv  = "HALRO_CRASH_CHILD_ROOT"
	crashChildTest = "TestSIGKILLCrashChild"
	// settledRequests the child must report before the parent kills it. Enough
	// that the Ledger, the metadata journal and the bbolt projection have all
	// been written to several times, and the kill lands in the middle of a
	// workload rather than at its edge.
	settledBeforeKill = 12
)

// crashConfig is the configuration both sides of the test build, from one root
// they agree on. It cannot come from testConfig, which invents its own
// temporary directory — the child has to open the parent's.
func crashConfig(root string) config.Config {
	cfg := config.Default()
	cfg.Storage.DataDir = filepath.Join(root, "data")
	cfg.Storage.MetadataFile = "halro.db"
	cfg.Storage.MasterKey.Mode = config.MasterKeyModeFile
	cfg.Storage.MasterKey.File = filepath.Join(root, "master.key")
	// Every settled request has to be durable before it is reported, or the
	// parent would be hunting for records the child never promised.
	cfg.Usage.Durability = "strict"
	// Nothing in this test serves traffic; the listeners are never bound.
	cfg.Metrics.Enabled = false
	return cfg
}

// TestSIGKILLCrashChild is the process the parent kills. It is a test rather
// than a separate command so there is nothing to build and nothing that can
// drift from the code it is exercising.
func TestSIGKILLCrashChild(t *testing.T) {
	root := os.Getenv(crashChildEnv)
	if root == "" {
		t.Skip("child of TestSIGKILLLeavesEveryReportedRequestRecoverable")
	}
	cfg := crashConfig(root)
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "child could not open: %v\n", err)
		os.Exit(2)
	}
	// Deliberately never closed: the point is to be killed with everything
	// open, which is what a crash looks like.
	for index := 0; ; index++ {
		requestID := fmt.Sprintf("req_crash_%04d", index)
		if err := billOneCrashRequest(runtime, requestID); err != nil {
			fmt.Fprintf(os.Stderr, "child could not bill %s: %v\n", requestID, err)
			os.Exit(3)
		}
		// Reported only after the whole lifecycle is durable, so the parent's
		// assertion is about promises kept rather than work attempted.
		fmt.Printf("settled %s\n", requestID)
		os.Stdout.Sync()
	}
}

func billOneCrashRequest(runtime *Runtime, requestID string) error {
	ctx := context.Background()
	request, err := runtime.accounting.BeginRequestDetailed(ctx, "project_1", "key_1", requestID, "chat")
	if err != nil {
		return err
	}
	// A budget large enough that the loop is bounded by the kill rather than by
	// admission, while still large enough to be checked: zero would disable the
	// arithmetic this path is supposed to perform on every request.
	attempt, err := runtime.accounting.ReserveAttemptDetailed(ctx, request, 1_000_000_000, 100,
		budget.AttemptMetadata{
			RouteID: "route_1", ProviderID: "provider_1", ProviderModel: "model_1", AttemptNumber: 1,
		})
	if err != nil {
		return err
	}
	if err := runtime.accounting.MarkStarted(ctx, attempt); err != nil {
		return err
	}
	if err := runtime.accounting.Settle(ctx, attempt, budget.Settlement{
		CommittedMicrosUSD: 90, ProviderInputTokens: 7, ProviderOutputTokens: 3,
		Outcome: "success", LatencyMillis: 12,
	}); err != nil {
		return err
	}
	return runtime.accounting.Finalize(ctx, request, "success")
}

// TestSIGKILLLeavesEveryReportedRequestRecoverable is the gate.
func TestSIGKILLLeavesEveryReportedRequestRecoverable(t *testing.T) {
	if os.Getenv(crashChildEnv) != "" {
		t.Skip("this process is the child")
	}
	root := t.TempDir()
	cfg := crashConfig(root)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}

	reported := runUntilKilled(t, root)
	if len(reported) < settledBeforeKill {
		t.Fatalf("the child reported %d settled requests before the kill, want at least %d",
			len(reported), settledBeforeKill)
	}

	// 1. No residual lock. A killed process cannot run its own cleanup, so a
	//    lock that only releases on a graceful exit would leave the directory
	//    unopenable — which is the failure R-18 was looking for by hand.
	held, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		t.Fatalf("the data lock survived the kill: %v", err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}

	// 2. The offline diagnostic agrees the directory is intact: Ledger chain
	//    authenticated against its checkpoint, metadata journal level with its
	//    projection, Parquet manifest verified.
	report, err := Doctor(context.Background(), cfg)
	if err != nil || !report.Healthy {
		t.Fatalf("doctor after the kill: healthy=%v err=%v", report.Healthy, err)
	}
	for _, check := range report.Checks {
		if check.Name == "metadata_journal" && check.Status == "fail" {
			t.Fatalf("the journal and its projection diverged across the kill: %s", check.Detail)
		}
	}

	// 3. Every request the child said it had settled is still there. This is
	//    what the idle manual run could not test: the child reported only after
	//    the whole lifecycle was durable, so a missing one is a broken promise
	//    rather than work that was merely in flight.
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("reopen after the kill: %v", err)
	}
	defer runtime.Close()
	recovered := settledRequestIDs(t, runtime)
	for _, requestID := range reported {
		if _, found := recovered[requestID]; !found {
			t.Fatalf("request %s was reported settled and is absent after recovery", requestID)
		}
	}
}

// runUntilKilled starts the child, waits for it to report enough settled
// requests, SIGKILLs it, and returns what it had promised.
func runUntilKilled(t *testing.T, root string) []string {
	t.Helper()
	child := exec.Command(os.Args[0], "-test.run=^"+crashChildTest+"$", "-test.v")
	child.Env = append(os.Environ(), crashChildEnv+"="+root)
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	}()

	var reported []string
	lines := bufio.NewScanner(stdout)
	deadline := time.After(90 * time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for lines.Scan() {
			requestID, ok := strings.CutPrefix(lines.Text(), "settled ")
			if !ok {
				continue
			}
			reported = append(reported, requestID)
			if len(reported) >= settledBeforeKill {
				// SIGKILL, not Signal(os.Interrupt): the whole point is that
				// nothing gets to run on the way out — no deferred Close, no
				// final fsync, no lock release.
				_ = child.Process.Kill()
				return
			}
		}
	}()
	select {
	case <-done:
	case <-deadline:
		t.Fatal("the child never reported enough settled requests")
	}
	// The exit status is checked, not discarded. A child that died of its own
	// accord — a panic, a failed open, a budget refusal — would leave this test
	// passing while proving nothing about SIGKILL, which is precisely the shape
	// of a test that looks like coverage and is not.
	waitErr := child.Wait()
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		t.Fatalf("the child exited without an error status: %v", waitErr)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("the child was not killed by SIGKILL: %v", exitErr)
	}
	return reported
}

// settledRequestIDs reads back which request IDs the recovered instance holds a
// terminal record for.
func settledRequestIDs(t *testing.T, runtime *Runtime) map[string]struct{} {
	t.Helper()
	snapshot := runtime.usage.Snapshot()
	found := make(map[string]struct{}, len(snapshot.Attempts))
	for _, attempt := range snapshot.Attempts {
		found[attempt.RequestID] = struct{}{}
	}
	for _, summary := range snapshot.Requests {
		found[summary.RequestID] = struct{}{}
	}
	if len(found) == 0 {
		t.Fatalf("the recovered instance holds no attempts at all (%d attempts, %d requests)",
			len(snapshot.Attempts), len(snapshot.Requests))
	}
	return found
}
