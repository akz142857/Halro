//go:build linux || darwin

package budget

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/usage"
)

type legacyRecoveryCrashMarker struct {
	Phase   string
	Record  ledger.Record
	Applied ledger.Watermark
}

type legacyRecoveryCrashWriter struct {
	RecoveryWriter
	t         *testing.T
	native    *ledger.Log
	reader    legacyRecoveryReader
	candidate *authenticatedRecoveryTestWriter
	phase     string
	marker    string
	count     int
}

func (w *legacyRecoveryCrashWriter) crash(record ledger.Record) {
	w.t.Helper()
	applied, _, err := w.reader.ReadPosition(context.Background())
	if err != nil {
		w.t.Fatal(err)
	}
	raw, err := json.Marshal(legacyRecoveryCrashMarker{Phase: w.phase, Record: record, Applied: applied})
	if err != nil {
		w.t.Fatal(err)
	}
	f, err := os.OpenFile(w.marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		w.t.Fatal(err)
	}
	_, writeErr := f.Write(raw)
	if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
		w.t.Fatal(err)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
		w.t.Fatal(err)
	}
	w.t.Fatal("SIGKILL unexpectedly returned")
}

func (w *legacyRecoveryCrashWriter) AppendApplyRecovery(ctx context.Context, event ledger.Event) (ledger.Record, error) {
	target := event.Kind == ledger.EventAttemptSettled && strings.HasSuffix(w.phase, "settlement") || event.Kind == ledger.EventRequestFinalized && strings.HasSuffix(w.phase, "finalization")
	if target {
		w.count++
	}
	if !target || w.count != 64 {
		return w.RecoveryWriter.AppendApplyRecovery(ctx, event)
	}
	if strings.HasPrefix(w.phase, "durable-") {
		if w.candidate != nil {
			r, _, err := w.candidate.lease.AppendApply(ctx, event, func(_ context.Context, record ledger.Record) error {
				w.crash(record) // Native is synced; no projection apply has run.
				return nil
			})
			return r, err
		}
		r, _, err := w.native.AppendRecordWithReceipt(ctx, event)
		if err == nil {
			w.crash(r)
		}
		return r, err
	}
	record, err := w.RecoveryWriter.AppendApplyRecovery(ctx, event)
	if err == nil {
		w.crash(record) // Applied, but recovery has not observed success.
	}
	return record, err
}

func TestLegacyRecoveryActualSIGKILLHelper(t *testing.T) {
	path := os.Getenv("HALRO_LEGACY_RECOVERY_CRASH_PATH")
	if path == "" {
		t.Skip("subprocess-only two-level recovery crash fixture")
	}
	ctx := context.Background()
	key := bytes.Repeat([]byte{0x51}, 32)
	native, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: key})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { native.Close() })
	phase := os.Getenv("HALRO_LEGACY_RECOVERY_CRASH_PHASE")
	p := rebuildRecoveryTestProjection(t, path, key, "child-"+phase+"-projection.db")
	t.Cleanup(func() { p.Discard() })
	base := &recoveryTestWriter{t: t, log: native, p: p}
	resources := legacyRecoveryResources(t)
	writer := &legacyRecoveryCrashWriter{RecoveryWriter: base, t: t, native: native, reader: p, phase: phase, marker: filepath.Join(filepath.Dir(path), "recovery-crash-"+phase+".json")}
	auth := os.Getenv("HALRO_LEGACY_RECOVERY_CRASH_AUTH") == "true"
	if auth {
		writer.candidate = legacyAuthenticatedCandidate(t, native, path, key, resources)
		writer.RecoveryWriter, writer.reader = writer.candidate, writer.candidate.p
	}
	occupyLegacyNewAdmission(t, resources)
	limits := ledger.ProjectionPageLimits{Rows: 64, Bytes: 1 << 20}
	if auth {
		_, err = RecoverAuthenticatedRequests(ctx, writer.candidate.p, writer, RecoveryStandalone, limits)
	} else {
		_, err = RecoverProjectedRequests(ctx, p, writer, RecoveryStandalone, limits)
	}
	t.Fatal("crash boundary was not reached", err)
}

func TestLegacyRecoveryActualSIGKILLAtDurableAndAppliedPageBoundaries(t *testing.T) {
	for _, auth := range []bool{false, true} {
		for _, phase := range []string{"durable-settlement", "applied-settlement", "durable-finalization", "applied-finalization"} {
			t.Run(fmt.Sprintf("authenticated=%v/%s", auth, phase), func(t *testing.T) {
				// Both backends; one settlement and one (twice-killed) finalization point.
				legacyRecoveryCase(t, auth && phase == "durable-finalization" || !auth && phase == "applied-settlement")
				ctx := context.Background()
				base, path, key := newProjectedRecoveryTest(t, legacyRecoveryEvents(t, true))
				beforeUsage := legacyDiskUsage(t, base.log, base.p)
				originals := make(map[string]usage.RequestSummary)
				for i := 4; i < legacyRecoveryRequests; i += 5 {
					id := fmt.Sprint("req_", i)
					row, found, err := beforeUsage.ReadRequest(ctx, id)
					if err != nil || !found {
						t.Fatal(found, err)
					}
					originals[id] = row
				}
				if err := errors.Join(beforeUsage.Discard(), base.p.Discard(), base.log.Close()); err != nil {
					t.Fatal(err)
				}
				crashes := []string{phase}
				if strings.HasSuffix(phase, "finalization") {
					// Crash while settling, rebuild in another process, then crash again
					// while finalizing. Neither candidate may survive as authority.
					crashes = []string{"durable-settlement", phase}
				}
				var err error
				for _, crashPhase := range crashes {
					executable, err := os.Executable()
					if err != nil {
						t.Fatal(err)
					}
					// The child normally reaches its boundary in seconds, but under a
					// parallel `go test ./...` it took over 30 s; a timeout kill is still
					// told apart from the intended SIGKILL by childCtx.Err below.
					childCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
					defer cancel()
					cmd := exec.CommandContext(childCtx, executable, "-test.run=^TestLegacyRecoveryActualSIGKILLHelper$", "-test.count=1")
					// Keep all abandoned child candidates under the owned case directory.
					cmd.Env = append(os.Environ(), "TMPDIR="+filepath.Dir(path), "HALRO_LEGACY_RECOVERY_CRASH_PATH="+path, "HALRO_LEGACY_RECOVERY_CRASH_PHASE="+crashPhase, fmt.Sprint("HALRO_LEGACY_RECOVERY_CRASH_AUTH=", auth))
					output, crashErr := cmd.CombinedOutput()
					var exit *exec.ExitError
					if !errors.As(crashErr, &exit) {
						t.Fatalf("child did not crash: %v %s", crashErr, output)
					}
					status, ok := exit.Sys().(syscall.WaitStatus)
					if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL || childCtx.Err() != nil {
						t.Fatalf("not the intended SIGKILL: %v %s", crashErr, output)
					}
					raw, err := os.ReadFile(filepath.Join(filepath.Dir(path), "recovery-crash-"+crashPhase+".json"))
					var marker legacyRecoveryCrashMarker
					if err != nil || json.Unmarshal(raw, &marker) != nil || marker.Phase != crashPhase {
						t.Fatal("unproven recovery crash boundary", err)
					}
					report, partial, err := ledger.InspectReplayAuthenticated(path, key, nil)
					if err != nil || partial || report.Head != (ledger.Watermark{Generation: marker.Record.Generation, Offset: marker.Record.Offset, Sequence: marker.Record.Sequence}) || report.ChainHash != marker.Record.Hash {
						t.Fatal("crash lost durable fact or left an unexpected tail", report, partial, err)
					}
					wantApplied := marker.Record.Sequence
					if strings.HasPrefix(crashPhase, "durable-") {
						wantApplied--
					}
					if marker.Applied.Sequence != wantApplied {
						t.Fatal("crashed at wrong projection boundary", marker)
					}
					t.Logf("SIGKILL phase=%s native_sequence=%d applied_sequence=%d", crashPhase, marker.Record.Sequence, marker.Applied.Sequence)
				}
				base.log, err = ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: key})
				if err != nil {
					t.Fatal(err)
				}
				base.p = rebuildRecoveryTestProjection(t, path, key, "parent-retry.db")
				resources := legacyRecoveryResources(t)
				var reader legacyRecoveryReader = base.p
				var candidate *authenticatedRecoveryTestWriter
				if auth {
					candidate = legacyAuthenticatedCandidate(t, base.log, path, key, resources)
					reader = candidate.p
				}
				occupyLegacyNewAdmission(t, resources)
				limits := ledger.ProjectionPageLimits{Rows: 64, Bytes: 1 << 20}
				var result ProjectedRecoveryResult
				if auth {
					result, err = RecoverAuthenticatedRequests(ctx, candidate.p, candidate, RecoveryStandalone, limits)
				} else {
					result, err = RecoverProjectedRequests(ctx, base.p, base, RecoveryStandalone, limits)
				}
				wantSettled, wantFinalized := uint64(13), uint64(104)
				if strings.HasSuffix(phase, "finalization") {
					wantSettled, wantFinalized = 0, 40
				}
				if err != nil || result.AttemptsSettled != wantSettled || result.RequestsFinalized != wantFinalized {
					t.Fatal("actual SIGKILL retry repeated/lost facts", result, err)
				}
				freshUsage := legacyDiskUsage(t, base.log, reader)
				verifyLegacyRecovery(t, base.log, reader, freshUsage, originals)
			})
		}
	}
}
