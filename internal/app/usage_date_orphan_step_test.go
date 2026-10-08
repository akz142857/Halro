package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/usage"
)

func TestUsageDateOrphanStepFiniteBatchesBothPhysicalFormats(t *testing.T) {
	for _, format := range []string{usage.FormatNDJSON, usage.FormatParquet} {
		t.Run(format, func(t *testing.T) {
			cfg, original, cutoff := offlineDateFileReclaimFixture(t, format)
			if original.Format != format {
				t.Fatal("wrong actual container fixture", original.Format)
			}
			raw, err := os.ReadFile(filepath.Join(cfg.UsagePath(), filepath.FromSlash(original.Path)))
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 9; i++ {
				path := filepath.Join(cfg.UsagePath(), "date="+original.Date, fmt.Sprintf("orphan-%02d%s", i, filepath.Ext(original.Path)))
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			before := make(map[string][]byte)
			for _, path := range []string{cfg.LedgerPath(), filepath.Join(cfg.UsagePath(), "manifest.json"), filepath.Join(cfg.UsagePath(), ".archive-date-activation-v1.json")} {
				before[path], err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			removed := 0
			for i := 0; i < 20; i++ {
				out, err := ReclaimUsageDateOrphansStep(context.Background(), cfg, cutoff)
				if err != nil || !out.Reclamation.ObligationPending || out.Reclamation.FilesRemoved > 4 || out.Reclamation.RowsVerified > 4096 || out.Verification.Role != "standalone" || out.ComparisonSteps == 0 || out.ReferenceSteps == 0 {
					t.Fatal(out, err)
				}
				removed += out.Reclamation.FilesRemoved
				checkDateReclaimCleanup(t, cfg)
				if out.Reclamation.Phase == "scan-complete" {
					break
				}
				if i == 19 {
					t.Fatal("offline fresh scans stalled")
				}
			}
			if removed != 10 {
				t.Fatal("missed actual data/orphans", removed)
			}
			for path, expected := range before {
				actual, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(actual, expected) {
					t.Fatal("native/root/policy changed", path, err)
				}
			}
		})
	}
}

func TestUsageDateOrphanStepSameOwnerReadOnlyChunks(t *testing.T) {
	cfg, _, cutoff := offlineDateFileReclaimFixture(t, usage.FormatNDJSON)
	for i := 0; i < 300; i++ {
		if err := os.WriteFile(filepath.Join(cfg.UsagePath(), fmt.Sprintf(".readonly-prefix-%03d", i)), []byte("preserved"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	limits := usage.DefaultDateOrphanReclaimLimits()
	limits.Entries = 1
	out, err := reclaimUsageDateOrphansStep(context.Background(), cfg, cutoff, limits, nil, nil)
	if err != nil || out.Reclamation.FilesRemoved != 1 || out.ReadOnlyScanSteps == 0 || !out.Reclamation.ObligationPending {
		t.Fatal("finite prefix repeatedly reopened instead of continuing same owner", out, err)
	}
	// With all candidates gone, the whole read-only prefix must reach actual EOF
	// on one fresh owner instead of returning a perpetually restarting first chunk.
	out, err = reclaimUsageDateOrphansStep(context.Background(), cfg, cutoff, limits, nil, nil)
	if err != nil || out.Reclamation.Phase != "scan-complete" || out.ReadOnlyScanSteps < 300 || out.ReadOnlyEntries < 300 || out.Reclamation.LogicalBytesUnlinked != 0 {
		t.Fatal(out, err)
	}
	checkDateReclaimCleanup(t, cfg)
}

func TestUsageDateOrphanStepIndependentAuthorityAndLateFailure(t *testing.T) {
	for _, fault := range []string{"native-before", "metadata-before", "cancel-before", "native-after", "metadata-after", "cancel-after"} {
		t.Run(fault, func(t *testing.T) {
			cfg, original, cutoff := offlineDateFileReclaimFixture(t, usage.FormatNDJSON)
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
			out, err := reclaimUsageDateOrphansStep(ctx, cfg, cutoff, usage.DefaultDateOrphanReclaimLimits(), before, after)
			if err == nil || out != (UsageDateOrphanReclaimReport{}) {
				t.Fatal("late/source failure retained scan scope", out, err)
			}
			_, statErr := os.Stat(filepath.Join(cfg.UsagePath(), filepath.FromSlash(original.Path)))
			if after != nil {
				if !errors.Is(statErr, os.ErrNotExist) {
					t.Fatal("actual unlink did not happen before late failure", statErr, err)
				}
			} else if statErr != nil {
				t.Fatal("pre-delete failure removed data", statErr)
			}
			if _, err := os.Stat(filepath.Join(cfg.UsagePath(), ".archive-date-activation-v1.json")); err != nil {
				t.Fatal("policy disappeared", err)
			}
			checkDateReclaimCleanup(t, cfg)
		})
	}
}

func TestUsageDateOrphanStepRefusesHAMemberAndDowngrade(t *testing.T) {
	cfg, _, _, _, _ := offlineUsageMemberFixture(t)
	for _, downgrade := range []bool{false, true} {
		if downgrade {
			cfg.Replication = nil
		}
		if out, err := ReclaimUsageDateOrphansStep(context.Background(), cfg, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)); err == nil || out != (UsageDateOrphanReclaimReport{}) {
			t.Fatal(downgrade, out, err)
		}
	}
}
