package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/usage"
)

func TestPruneUsageDiskKeepsNativeFactsAndResumesDatePolicy(t *testing.T) {
	for _, format := range []string{usage.FormatNDJSON, usage.FormatParquet} {
		t.Run(format, func(t *testing.T) {
			cfg := offlineDateActivationFixture(t, format)
			native, err := os.ReadFile(cfg.LedgerPath())
			if err != nil {
				t.Fatal(err)
			}
			// Preserve all rows first, then advance the policy and finish the old
			// original-chain obligation before publishing its successor.
			first := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
			out, err := PruneUsage(context.Background(), cfg, first)
			if err != nil || out.FilesRemoved != 0 || out.RowsRemoved != 0 {
				t.Fatal(out, err)
			}
			cutoff := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
			out, err = PruneUsage(context.Background(), cfg, cutoff)
			if err != nil || out.FilesRemoved != 1 || out.RowsRemoved != 2 || out.Cutoff != "2100-01-01" {
				t.Fatal(out, err)
			}
			again, err := PruneUsage(context.Background(), cfg, cutoff)
			if err != nil || again.FilesRemoved != 0 || again.RowsRemoved != 0 {
				t.Fatal(again, err)
			}
			if _, err := PruneUsage(context.Background(), cfg, first); err == nil {
				t.Fatal("decreasing retention date accepted")
			}
			after, err := os.ReadFile(cfg.LedgerPath())
			if err != nil || !bytes.Equal(native, after) {
				t.Fatal("prune changed native facts", err)
			}
			verified, err := VerifyUsage(context.Background(), cfg)
			if err != nil || verified.RequiredRows != 0 || verified.ExpiredOmittedRows != 2 {
				t.Fatal(verified, err)
			}
			checkDateReclaimCleanup(t, cfg)
		})
	}
}

func TestPruneUsageDiskRefusesCanceledAndHAMemberWithoutWriting(t *testing.T) {
	cfg := offlineDateActivationFixture(t, usage.FormatNDJSON)
	before := doctorTree(t, cfg.Storage.DataDir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out, err := PruneUsage(ctx, cfg, time.Now()); !errors.Is(err, context.Canceled) || out != (usage.RetentionReport{}) {
		t.Fatal(out, err)
	}
	if !reflect.DeepEqual(before, doctorTree(t, cfg.Storage.DataDir)) {
		t.Fatal("canceled prune changed the directory")
	}
	member, _, _, _, _ := offlineUsageMemberFixture(t)
	for _, downgrade := range []bool{false, true} {
		if downgrade {
			member.Replication = nil
		}
		if out, err := PruneUsage(context.Background(), member, time.Now()); err == nil || out != (usage.RetentionReport{}) {
			t.Fatal(downgrade, out, err)
		}
	}
}

func TestVerifyUsageCurrentPolicyRequiresIndependentReceiptAndStaysReadOnly(t *testing.T) {
	for _, fault := range []string{"none", "root-date", "receipt", "missing-receipts"} {
		t.Run(fault, func(t *testing.T) {
			cfg := offlineDateActivationFixture(t, usage.FormatNDJSON)
			cutoff := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
			if _, err := PruneUsage(context.Background(), cfg, cutoff); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(cfg.UsagePath(), "manifest.json")
			marker := filepath.Join(cfg.UsagePath(), ".archive-date-activation-v1.json")
			active := filepath.Join(cfg.UsagePath(), ".archive-date-active-policy-v1.json")
			switch fault {
			case "root-date":
				raw, err := os.ReadFile(root)
				if err != nil {
					t.Fatal(err)
				}
				raw = bytes.ReplaceAll(raw, []byte("2100-01-01"), []byte("2099-01-01"))
				if err := os.WriteFile(root, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "receipt":
				raw, err := os.ReadFile(marker)
				if err != nil {
					t.Fatal(err)
				}
				changed := bytes.Replace(raw, []byte(`"MAC":"`), []byte(`"MAC":"0`), 1)
				if bytes.Equal(raw, changed) {
					t.Fatal("fixture did not alter the receipt MAC")
				}
				raw = changed
				if err := os.WriteFile(marker, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "missing-receipts":
				if err := errors.Join(os.Remove(marker), os.Remove(active)); err != nil {
					t.Fatal(err)
				}
			}
			before := doctorTree(t, cfg.Storage.DataDir)
			out, err := VerifyUsage(context.Background(), cfg)
			if fault == "none" {
				if err != nil || out.ExpiredOmittedRows != 2 || out.RetainFromDate != "2100-01-01" {
					t.Fatal(out, err)
				}
				// Explicit genesis verification still requires every historical row.
				if _, err := VerifyUsageAfter(context.Background(), cfg, 0); err == nil {
					t.Fatal("explicit all-history scope silently inferred a date")
				}
			} else if err == nil || out != (usage.ReconciliationReport{}) {
				t.Fatal("derivative root supplied omission authority", out, err)
			}
			if !reflect.DeepEqual(before, doctorTree(t, cfg.Storage.DataDir)) {
				t.Fatal("verification wrote existing derivatives")
			}
		})
	}
}
