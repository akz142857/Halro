//go:build linux || darwin

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
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/usage"
)

type recoveryCrashReceipt struct {
	Stage, WorkUnit, Run, Attempt string
	Head                          ledger.Watermark
}

func TestTwoLevelRecoverySIGKILLHelper(t *testing.T) {
	stage := os.Getenv("HALRO_RECOVERY_CRASH_STAGE")
	if stage == "" {
		t.Skip("subprocess-only real SIGKILL fixture")
	}
	root := os.Getenv("HALRO_RECOVERY_CRASH_ROOT")
	ctx := context.Background()
	r, err := Open(ctx, crashConfig(root), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	receipt := recoveryCrashReceipt{Stage: stage}
	if os.Getenv("HALRO_RECOVERY_CRASH_RUN") == "1" {
		intent := func(name string) budget.GovernanceIntent {
			return budget.GovernanceIntent{Operation: name, IdempotencyKeyHash: outcomePageTestLabel(name), RequestFingerprint: outcomePageTestLabel("fp-" + name)}
		}
		unit, _, err := r.accounting.CreateWorkUnit(ctx, "project_1", "key_1", 10, intent("create-unit"))
		if err != nil {
			t.Fatal(err)
		}
		run, _, err := r.accounting.CreateRun(ctx, "project_1", "key_1", unit.ID, 1_000_000_000, time.Hour, 10, intent("create-run"))
		if err != nil {
			t.Fatal(err)
		}
		receipt.WorkUnit, receipt.Run = unit.ID, run.ID
	}
	request, err := r.accounting.BeginRequestAttributed(ctx, "project_1", "key_1", "req_recovery_crash", "model_1", receipt.WorkUnit, receipt.Run)
	if err != nil {
		t.Fatal(err)
	}
	if stage != "accepted" {
		attempt, err := r.accounting.ReserveAttemptDetailed(ctx, request, 1_000_000_000, 100, budget.AttemptMetadata{AttemptNumber: 1})
		if err != nil {
			t.Fatal(err)
		}
		receipt.Attempt = attempt.AttemptID
		if stage != "unstarted" {
			if err := r.accounting.MarkStarted(ctx, attempt); err != nil {
				t.Fatal(err)
			}
		}
		if stage == "settled" {
			if err := r.accounting.Settle(ctx, attempt, budget.Settlement{CommittedMicrosUSD: 90, Outcome: "success"}); err != nil {
				t.Fatal(err)
			}
		}
		if stage == "finalized-pending" {
			if err := r.accounting.Finalize(ctx, request, "interrupted"); err != nil {
				t.Fatal(err)
			}
		}
	}
	receipt.Head, _, err = r.accountingState.reader.ReadPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "phase.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// The child cannot run any deferred DB/Collector/Runtime cleanup.
	if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	// Successful delivery does not give this goroutine a synchronous process
	// exit fence. Park without running test/Runtime defers; the parent still
	// requires a real SIGKILL wait status before its own timeout. Returning or
	// calling Fatal here can win the race and turn the crash into exit status 1.
	for {
		time.Sleep(time.Hour)
	}
}

func TestTwoLevelRecoveryActualSIGKILLClosesOrphansAndLatePending(t *testing.T) {
	for _, withRun := range []bool{false, true} {
		for _, stage := range []string{"accepted", "unstarted", "started", "settled", "finalized-pending"} {
			name := "plain/" + stage
			if withRun {
				name = "run/" + stage
			}
			t.Run(name, func(t *testing.T) {
				root := t.TempDir()
				cfg := crashConfig(root)
				if err := Initialize(cfg); err != nil {
					t.Fatal(err)
				}
				executable, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				child := exec.CommandContext(ctx, executable, "-test.run=^TestTwoLevelRecoverySIGKILLHelper$", "-test.count=1")
				child.Env = append(os.Environ(), "HALRO_RECOVERY_CRASH_ROOT="+root, "HALRO_RECOVERY_CRASH_STAGE="+stage)
				if withRun {
					child.Env = append(child.Env, "HALRO_RECOVERY_CRASH_RUN=1")
				}
				output, err := child.CombinedOutput()
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatalf("child did not terminate at crash: %v %s", err, output)
				}
				status, ok := exit.Sys().(syscall.WaitStatus)
				if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL || ctx.Err() != nil {
					t.Fatalf("not planned SIGKILL: %v %s", err, output)
				}
				raw, err := os.ReadFile(filepath.Join(root, "phase.json"))
				if err != nil {
					t.Fatal(err)
				}
				var receipt recoveryCrashReceipt
				if err := json.Unmarshal(raw, &receipt); err != nil || receipt.Stage != stage || receipt.Head.Sequence == 0 {
					t.Fatalf("unproven crash phase: %+v %v", receipt, err)
				}
				logger := slog.New(slog.NewTextHandler(io.Discard, nil))
				r, err := Open(context.Background(), cfg, logger)
				if err != nil {
					t.Fatal(err)
				}
				defer r.Close()
				if r.accountingState.projection == nil || r.usage.disk == nil {
					t.Fatal("recovery did not use disk owners")
				}
				pending, err := r.accounting.ReadPendingLeaseStats(context.Background(), time.Now())
				if err != nil || pending.Count != 0 {
					t.Fatalf("pending survived recovery: %+v %v", pending, err)
				}
				metrics := usageMetricsForTest(t, r)
				if metrics.ActiveRequests != 0 {
					t.Fatalf("active request survived recovery: %+v", metrics)
				}
				if withRun {
					view, found, err := r.accounting.ReadWorkUnitAccounting(context.Background(), "project_1", receipt.WorkUnit, true)
					if err != nil || !found || view.Inflight {
						t.Fatalf("Run retained in-flight state: %+v %t %v", view, found, err)
					}
				}
				finalized, settled := 0, 0
				_, err = r.ledger.Replay(ledger.Watermark{}, func(record ledger.Record) error {
					if record.Event.RequestID == "req_recovery_crash" && record.Event.Kind == ledger.EventRequestFinalized {
						finalized++
					}
					if receipt.Attempt != "" && record.Event.AttemptID == receipt.Attempt && record.Event.Kind == ledger.EventAttemptSettled {
						settled++
					}
					return nil
				})
				if err != nil || finalized != 1 || stage == "accepted" && settled != 0 || stage != "accepted" && settled != 1 {
					t.Fatalf("terminal facts missing/duplicated: finalized=%d settled=%d err=%v", finalized, settled, err)
				}
				oracle, err := usage.NewAggregateWithFinalizationReader(r.accountingState.projection)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := r.ledger.Replay(ledger.Watermark{}, oracle.Apply); err != nil {
					t.Fatal(err)
				}
				if metrics != oracle.Metrics() || !reflect.DeepEqual(usageSnapshotForTest(t, r), oracle.Snapshot()) {
					t.Fatal("disk Usage differs from complete native reducer")
				}
				head := accountingReaderHead(t, r.accountingState.reader)
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
				again, err := Open(context.Background(), cfg, logger)
				if err != nil {
					t.Fatal(err)
				}
				defer again.Close()
				if accountingReaderHead(t, again.accountingState.reader) != head || usageMetricsForTest(t, again) != metrics {
					t.Fatal("second restart duplicated facts or summary")
				}
			})
		}
	}
}
