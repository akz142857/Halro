package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/usage"
)

func TestUsageDatePolicyRejectsUnsignedOrChangedDecisionOnFreshReopen(t *testing.T) {
	for _, fault := range []string{"unsigned", "changed-date"} {
		t.Run(fault, func(t *testing.T) {
			cfg, entry, cutoff := offlineDateFileReclaimFixture(t, usage.FormatNDJSON)
			path := filepath.Join(cfg.UsagePath(), ".archive-date-activation-v1.json")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var state struct {
				Version          int
				Phase            string
				OldRoot, NewRoot []byte
				Receipt          json.RawMessage `json:",omitempty"`
			}
			if err := json.Unmarshal(raw, &state); err != nil || state.Version != 2 || len(state.Receipt) == 0 {
				t.Fatal("activation did not persist authenticated receipt", err)
			}
			if fault == "unsigned" {
				state.Version, state.Receipt = 1, nil
			} else {
				state.NewRoot = bytes.ReplaceAll(state.NewRoot, []byte("2030-01-01"), []byte("2031-01-01"))
				cutoff = time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
				if err := os.WriteFile(filepath.Join(cfg.UsagePath(), "manifest.json"), state.NewRoot, 0600); err != nil {
					t.Fatal(err)
				}
			}
			raw, err = json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			nativeBefore, err := os.ReadFile(cfg.LedgerPath())
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if out, err := ReclaimUsageDateFile(ctx, cfg, cutoff, entry); !errors.Is(err, usage.ErrArchiveDateProof) || out != (UsageDateFileReclaimReport{}) {
				t.Fatal("single-file accepted unauthenticated policy", out, err)
			}
			if out, err := ReclaimUsageDateStep(ctx, cfg, cutoff); !errors.Is(err, usage.ErrArchiveDateProof) || out != (UsageDateReclaimStepReport{}) {
				t.Fatal("batch accepted unauthenticated policy", out, err)
			}
			if out, err := ActivateUsageDateRetention(ctx, cfg, cutoff); !errors.Is(err, usage.ErrArchiveDateProof) || out != (UsageDateActivationReport{}) {
				t.Fatal("retry backfilled unauthenticated policy", out, err)
			}
			nativeAfter, err := os.ReadFile(cfg.LedgerPath())
			if err != nil || !bytes.Equal(nativeBefore, nativeAfter) {
				t.Fatal("native facts changed", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(raw, after) {
				t.Fatal("refusal changed unsigned/forged obligation", err)
			}
			if _, err := os.Stat(filepath.Join(cfg.UsagePath(), filepath.FromSlash(entry.Path))); err != nil {
				t.Fatal("refusal removed data file", err)
			}
			checkDateReclaimCleanup(t, cfg)
		})
	}
}
