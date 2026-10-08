package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/usage"
)

func offlineOrphanPageFixture(t *testing.T, format string, n int) (config.Config, time.Time, []string) {
	t.Helper()
	cfg, entry, cutoff := offlineDateFileReclaimFixture(t, format)
	if entry.Format != format {
		t.Fatal("wrong actual source format", entry.Format)
	}
	pages, err := filepath.Glob(filepath.Join(cfg.UsagePath(), "catalog", "v1", "page-*.json"))
	if err != nil || len(pages) != 1 {
		t.Fatal(pages, err)
	}
	raw, err := os.ReadFile(pages[0])
	if err != nil {
		t.Fatal(err)
	}
	out, err := ReclaimUsageDatePagesStep(context.Background(), cfg, cutoff)
	if err != nil || out.Reclamation.Phase != "ready" || out.Reclamation.PagesRemoved != 1 {
		t.Fatal(out, err)
	}
	var page struct {
		Version  int                  `json:"version"`
		Previous usage.ArchivePageRef `json:"previous"`
		Files    []usage.ManifestFile `json:"files"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	result := []string{}
	for i := 0; i < n; i++ {
		page.Files[0].Path = fmt.Sprintf("date=%s/offline-orphan-page-%d%s", page.Files[0].Date, i, filepath.Ext(entry.Path))
		payload, err := json.Marshal(page)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(payload)
		path := filepath.Join(cfg.UsagePath(), "catalog", "v1", "page-"+hex.EncodeToString(digest[:])+".json")
		if err := os.WriteFile(path, payload, 0600); err != nil {
			t.Fatal(err)
		}
		result = append(result, path)
	}
	return cfg, cutoff, result
}

func checkOrphanPageCleanup(t *testing.T, cfg config.Config) {
	t.Helper()
	checkDateReclaimCleanup(t, cfg)
	paths, err := filepath.Glob(filepath.Join(cfg.UsagePath(), ".archive-date-orphan-page-index-*"))
	if err != nil || len(paths) != 0 {
		t.Fatal("candidate protection scratch escaped physical join", paths, err)
	}
}

func TestUsageDateOrphanPagesFreshFiniteBatchesBothActualFormats(t *testing.T) {
	for _, format := range []string{usage.FormatNDJSON, usage.FormatParquet} {
		t.Run(format, func(t *testing.T) {
			cfg, cutoff, paths := offlineOrphanPageFixture(t, format, 10)
			before := map[string][]byte{}
			for _, path := range []string{cfg.LedgerPath(), filepath.Join(cfg.UsagePath(), "manifest.json"), filepath.Join(cfg.UsagePath(), ".archive-date-activation-v1.json"), filepath.Join(cfg.UsagePath(), ".archive-date-pages-v1.json")} {
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				before[path] = raw
			}
			removed := 0
			for i := 0; i < 10; i++ {
				out, err := ReclaimUsageDateOrphanPagesStep(context.Background(), cfg, cutoff)
				if err != nil || out.Reclamation.PagesRemoved > 4 || !out.Reclamation.ObligationPending || out.Verification.Role != "standalone" || out.ComparisonSteps == 0 || out.ReferenceSteps == 0 {
					t.Fatal(out, err)
				}
				removed += out.Reclamation.PagesRemoved
				checkOrphanPageCleanup(t, cfg)
				if out.Reclamation.Phase == "scan-complete" {
					break
				}
				if i == 9 {
					t.Fatal("fresh inventories stalled")
				}
			}
			if removed != len(paths) {
				t.Fatal(removed)
			}
			for _, path := range paths {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal(path, err)
				}
			}
			for path, raw := range before {
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(after, raw) {
					t.Fatal("native/root/signed authority changed", path, err)
				}
			}
		})
	}
}

func TestUsageDateOrphanPagesReadOnlyPrefixContinuesSameOwner(t *testing.T) {
	cfg, cutoff, _ := offlineOrphanPageFixture(t, usage.FormatNDJSON, 1)
	for i := 0; i < 300; i++ {
		if err := os.WriteFile(filepath.Join(cfg.UsagePath(), "catalog", "v1", fmt.Sprintf(".protected-prefix-%03d", i)), []byte("preserve"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	l := usage.DefaultDateOrphanPageReclaimLimits()
	l.Entries = 1
	out, err := reclaimUsageDateOrphanPagesStep(context.Background(), cfg, cutoff, l, nil, nil)
	if err != nil || out.Reclamation.PagesRemoved != 1 {
		t.Fatal(out, err)
	}
	out, err = reclaimUsageDateOrphanPagesStep(context.Background(), cfg, cutoff, l, nil, nil)
	if err != nil || out.Reclamation.Phase != "scan-complete" || out.ReadOnlyEntries < 300 || out.ReadOnlyScanSteps < 300 || out.Reclamation.LogicalBytesUnlinked != 0 {
		t.Fatal(out, err)
	}
	checkOrphanPageCleanup(t, cfg)
}

func TestUsageDateOrphanPagesIndependentAuthorityAndLateFailure(t *testing.T) {
	for _, fault := range []string{"native-before", "metadata-before", "cancel-before", "native-after", "metadata-after", "cancel-after"} {
		t.Run(fault, func(t *testing.T) {
			cfg, cutoff, paths := offlineOrphanPageFixture(t, usage.FormatNDJSON, 1)
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
			out, err := reclaimUsageDateOrphanPagesStep(ctx, cfg, cutoff, usage.DefaultDateOrphanPageReclaimLimits(), before, after)
			if err == nil || out != (UsageDateOrphanPageReclaimReport{}) {
				t.Fatal("independent late failure retained report", out, err)
			}
			_, statErr := os.Stat(paths[0])
			if before != nil && statErr != nil {
				t.Fatal("pre-delete failure removed metadata", statErr)
			}
			if after != nil && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("unlink missing before late failure", statErr)
			}
			checkOrphanPageCleanup(t, cfg)
		})
	}
}

func TestUsageDateOrphanPagesUnfinishedOldChainAndHAMemberRefused(t *testing.T) {
	cfg, _, cutoff := offlineDateFileReclaimFixture(t, usage.FormatNDJSON)
	out, err := ReclaimUsageDateOrphanPagesStep(context.Background(), cfg, cutoff)
	if err == nil || out != (UsageDateOrphanPageReclaimReport{}) {
		t.Fatal("missing signed old-chain completion accepted", out, err)
	}
	checkOrphanPageCleanup(t, cfg)
	cfg, _, _, _, _ = offlineUsageMemberFixture(t)
	for _, downgrade := range []bool{false, true} {
		if downgrade {
			cfg.Replication = nil
		}
		if out, err := ReclaimUsageDateOrphanPagesStep(context.Background(), cfg, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)); err == nil || out != (UsageDateOrphanPageReclaimReport{}) {
			t.Fatal(downgrade, out, err)
		}
	}
}
