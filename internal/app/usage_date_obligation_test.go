package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/usage"
)

func TestUsageDateObligationPreservesCurrentPolicyAndMissingMarkerReopen(t *testing.T) {
	for _, format := range []string{usage.FormatNDJSON, usage.FormatParquet} {
		t.Run(format, func(t *testing.T) {
			cfg, cutoff, _ := offlineOrphanPageFixture(t, format, 0)
			before := map[string][]byte{}
			for _, path := range []string{cfg.LedgerPath(), filepath.Join(cfg.UsagePath(), "manifest.json"), filepath.Join(cfg.UsagePath(), ".archive-date-pages-v1.json"), filepath.Join(cfg.UsagePath(), ".archive-date-activation-v1.json")} {
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				before[path] = raw
			}
			out, err := CompleteUsageDateObligation(context.Background(), cfg, cutoff)
			if err != nil || !out.Completion.OriginalChainComplete || !out.Completion.PolicyPreserved || out.Completion.AlreadyRecorded || !out.Completion.OrphansPending || out.Verification.Role != "standalone" || out.ComparisonSteps == 0 || out.ReferenceSteps == 0 {
				t.Fatal(out, err)
			}
			for path, raw := range before {
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(after, raw) {
					t.Fatal("completion modified native/root/original obligation", path, err)
				}
			}
			active := filepath.Join(cfg.UsagePath(), ".archive-date-active-policy-v1.json")
			activeBefore, err := os.ReadFile(active)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(cfg.UsagePath(), ".archive-date-activation-v1.json")); err != nil {
				t.Fatal(err)
			}
			again, err := CompleteUsageDateObligation(context.Background(), cfg, cutoff)
			if err != nil || !again.Completion.AlreadyRecorded {
				t.Fatal("new owner did not authenticate retained current policy", again, err)
			}
			activated, err := ActivateUsageDateRetention(context.Background(), cfg, cutoff)
			if err != nil || !activated.Activation.AlreadyPublished || !activated.Activation.ReclamationPending {
				t.Fatal(activated, err)
			}
			after, err := os.ReadFile(active)
			if err != nil || !bytes.Equal(after, activeBefore) {
				t.Fatal("retry rewrote authenticated completion", err)
			}
			if _, err := os.Stat(filepath.Join(cfg.UsagePath(), ".archive-date-activation-v1.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("published retry recreated missing marker", err)
			}
			checkOrphanPageCleanup(t, cfg)
		})
	}
}

func TestUsageDateObligationIndependentAuthorityAndLateFailure(t *testing.T) {
	for _, fault := range []string{"native-before", "metadata-before", "cancel-before", "native-after", "metadata-after", "cancel-after"} {
		t.Run(fault, func(t *testing.T) {
			cfg, cutoff, _ := offlineOrphanPageFixture(t, usage.FormatNDJSON, 0)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			inject := func() error {
				switch fault {
				case "cancel-before", "cancel-after":
					cancel()
					return nil
				case "native-before", "native-after":
					f, err := os.OpenFile(cfg.LedgerPath(), os.O_APPEND|os.O_WRONLY, 0600)
					if err != nil {
						return err
					}
					_, err = f.Write([]byte("torn native"))
					return errors.Join(err, f.Close())
				default:
					raw, err := os.ReadFile(cfg.MetadataPath())
					if err != nil {
						return err
					}
					if err := os.WriteFile(cfg.MetadataPath()+".replacement", raw, 0600); err != nil {
						return err
					}
					return os.Rename(cfg.MetadataPath()+".replacement", cfg.MetadataPath())
				}
			}
			var before, after func() error
			if fault == "native-after" || fault == "metadata-after" || fault == "cancel-after" {
				after = inject
			} else {
				before = inject
			}
			out, err := completeUsageDateObligation(ctx, cfg, cutoff, before, after)
			if err == nil || out != (UsageDateObligationCompletionReport{}) {
				t.Fatal("late completion leaked report", out, err)
			}
			_, statErr := os.Stat(filepath.Join(cfg.UsagePath(), ".archive-date-active-policy-v1.json"))
			if before != nil && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("pre-publication failure copied policy", statErr)
			}
			if after != nil && statErr != nil {
				t.Fatal("committed policy copy vanished after late failure", statErr)
			}
			checkOrphanPageCleanup(t, cfg)
		})
	}
}

func TestUsageDateObligationRequiresActualOldChainAndRefusesHAMember(t *testing.T) {
	cfg, _, cutoff := offlineDateFileReclaimFixture(t, usage.FormatNDJSON)
	out, err := CompleteUsageDateObligation(context.Background(), cfg, cutoff)
	if err == nil || out != (UsageDateObligationCompletionReport{}) {
		t.Fatal("missing authentic original-chain completion accepted", out, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.UsagePath(), ".archive-date-active-policy-v1.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	checkOrphanPageCleanup(t, cfg)
	cfg, _, _, _, _ = offlineUsageMemberFixture(t)
	for _, downgrade := range []bool{false, true} {
		if downgrade {
			cfg.Replication = nil
		}
		if out, err := CompleteUsageDateObligation(context.Background(), cfg, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)); err == nil || out != (UsageDateObligationCompletionReport{}) {
			t.Fatal(downgrade, out, err)
		}
	}
}
