package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// Native primitive tests kill a real process at every Roll/recovery boundary.
// This integration supplies those exact durable layouts to the ordinary
// Runtime constructor, exercising inventory, seven roots and actual services.
func TestSelectedRuntimeInterruptedRollReopensActualBusiness(t *testing.T) {
	exerciseSelectedRuntimeInterruptedRoll(t, false)
}

func TestSelectedRuntimeRejectsCommittedDirectoryRollback(t *testing.T) {
	exerciseSelectedRuntimeInterruptedRoll(t, true)
}

func exerciseSelectedRuntimeInterruptedRoll(t *testing.T, committed bool) {
	for _, phase := range []string{"intent", "renamed", "successor"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			legacy, credentials, _ := openBootstrappedRuntimeForModel(t, "gpt-test")
			cfg, instance := legacy.config, legacy.instanceID
			if err := legacy.Close(); err != nil {
				t.Fatal(err)
			}
			cfg = activateClosedRuntimeMemoryStateFixture(t, ctx, cfg, instance, false)
			opts := OpenOptions{AllowGeneratedSetupToken: true, resourceSampleSource: pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() })}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			r, err := OpenWithOptions(ctx, cfg, logger, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if r != nil {
					if err := r.Close(); err != nil {
						t.Error(err)
					}
				}
			}()
			if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
				t.Fatal(err)
			}
			request, err := r.accounting.BeginRequestDetailed(ctx, credentials.ProjectID, credentials.KeyID, "pending_roll_request", "chat")
			if err != nil {
				t.Fatal(err)
			}
			attempt, err := r.accounting.ReserveAttempt(ctx, request, 100, 3)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.accounting.Settle(ctx, attempt, budget.Settlement{CommittedMicrosUSD: 7, Outcome: "success"}); err != nil {
				t.Fatal(err)
			}
			if err := r.accounting.Finalize(ctx, request, "success"); err != nil {
				t.Fatal(err)
			}
			if err := r.usageCollector.CatchUp(ctx); err != nil {
				t.Fatal(err)
			}
			want, wantHash, _ := r.ledger.ChainHead()
			// Runtime.Close checkpoints the latest directory. Crash layouts must
			// be created after closing that saved cut, not by rolling back a
			// directory already covered by a later authenticated common seal.
			var roll ledger.SealResult
			if committed {
				roll, err = r.ledger.Roll()
				if closeErr := r.Close(); closeErr != nil {
					t.Fatal(closeErr)
				}
			} else {
				key := bytes.Clone(r.selectedState.bootstrap.LedgerKey)
				defer clear(key)
				if closeErr := r.Close(); closeErr != nil {
					t.Fatal(closeErr)
				}
				native, openErr := ledger.OpenWithOptions(cfg.LedgerPath(), ledger.NewStatus(), ledger.Options{ChainKey: key})
				if openErr != nil {
					t.Fatal(openErr)
				}
				roll, err = native.Roll()
				if closeErr := native.Close(); closeErr != nil {
					t.Fatal(closeErr)
				}
			}
			if err != nil || !roll.Rolled {
				t.Fatal("actual post-seal Roll", roll, err)
			}
			directory := filepath.Dir(cfg.LedgerPath())
			manifestPath := filepath.Join(directory, "segments.json")
			var manifest struct {
				Version  int              `json:"version"`
				Segments []ledger.Segment `json:"segments"`
				Pending  *ledger.Segment  `json:"pending,omitempty"`
			}
			raw, err := os.ReadFile(manifestPath)
			if err != nil || json.Unmarshal(raw, &manifest) != nil || len(manifest.Segments) == 0 {
				t.Fatal("actual completed manifest", err)
			}
			last := manifest.Segments[len(manifest.Segments)-1]
			if last != roll.Sealed {
				t.Fatal("wrong actual sealed generation", last)
			}
			manifest.Segments = manifest.Segments[:len(manifest.Segments)-1]
			manifest.Pending = &last
			if phase != "successor" {
				if err := os.Remove(cfg.LedgerPath()); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "intent" {
				if err := os.Rename(filepath.Join(directory, last.File), cfg.LedgerPath()); err != nil {
					t.Fatal(err)
				}
			}
			raw, err = json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			file, err := os.OpenFile(manifestPath, os.O_WRONLY|os.O_TRUNC, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := file.Write(raw)
			if err := errors.Join(writeErr, file.Sync(), file.Close(), durable.SyncDirectory(directory)); err != nil {
				t.Fatal(err)
			}
			r, err = OpenWithOptions(ctx, cfg, logger, opts)
			if committed {
				if !errors.Is(err, ledger.ErrProjectionSource) {
					t.Fatal("committed directory rollback accepted", err)
				}
				return
			}
			if err != nil {
				t.Fatal("ordinary seven-root Runtime interrupted Roll reopen", err)
			}
			if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
				t.Fatal(err)
			}
			actual, hash, _ := r.ledger.ChainHead()
			if actual.Sequence != want.Sequence || hash != wantHash {
				t.Fatal("ordinary recovery changed facts", actual, want)
			}
			stats, err := r.usage.disk.ReadStats(ctx)
			if err != nil || stats.Totals.Requests != 1 || stats.Totals.CostMicrosUSD != 7 || stats.Metrics.ActiveRequests != 0 {
				t.Fatal("ordinary recovery lost or duplicated Usage", stats, err)
			}
			if _, err := r.accounting.BeginRequestDetailed(ctx, credentials.ProjectID, credentials.KeyID, "pending_roll_after_recovery", "chat"); err != nil {
				t.Fatal("actual recovered business admission", err)
			}
		})
	}
}
