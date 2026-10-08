package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/usage"
)

func bindStartupUsageArchive(t *testing.T, r *Runtime, format string) *usage.Exporter {
	t.Helper()
	master, err := unlockMasterKey(context.Background(), r.config)
	if err != nil {
		t.Fatal(err)
	}
	key, err := loadLedgerHMACKey(r.store, r.vault, master)
	clear(master)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	if err := r.usageExporter.Close(); err != nil {
		t.Fatal(err)
	}
	r.config.Usage.ExportFormat = format
	e, err := r.usage.owner.bindArchive(context.Background(), r.config, r.ledger, key)
	if err != nil {
		t.Fatal(err)
	}
	r.usageExporter = e
	return e
}

func restoreStartupUsageWindow(t *testing.T, r *Runtime, now time.Time) usageStartupWindowReport {
	t.Helper()
	lease, err := r.ledger.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.restoreUsageDiskWindow(context.Background(), lease, now)
	if err = errors.Join(err, lease.Close()); err != nil {
		t.Fatal(out, err)
	}
	return out
}

func TestUsageStartupWindowRebuildBothCatalogsAndFormatsPreservesUnexportedSuffix(t *testing.T) {
	for _, format := range []string{usage.FormatNDJSON, usage.FormatParquet} {
		for _, legacy := range []bool{false, true} {
			name := format + "/paged"
			if legacy {
				name = format + "/legacy"
			}
			t.Run(name, func(t *testing.T) {
				r, _ := summaryRuntime(t, 0)
				now := time.Now().UTC()
				seedRetentionUsage(t, r, 129, now.AddDate(0, 0, -60))
				p := installDiskUsageQueries(t, r)
				e := bindStartupUsageArchive(t, r, format)
				ctx := context.Background()
				var err error
				if legacy {
					// Default worker shutdown may publish an empty paged root.
					// The legacy fixture explicitly starts from a missing catalog.
					position, readErr := e.ReadArchivePosition(ctx)
					if readErr == nil {
						if position.LastSequence != 0 || position.Files != 0 || position.CatalogHead != (usage.ArchivePageRef{}) {
							t.Fatal("legacy fixture would discard data", position)
						}
						if err := os.Remove(filepath.Join(r.config.UsagePath(), "manifest.json")); err != nil {
							t.Fatal(err)
						}
					} else if !errors.Is(readErr, os.ErrNotExist) {
						t.Fatal(readErr)
					}
					_, err = e.ExportDiskStepPreservingCatalog(ctx, p, usage.DefaultDiskExportStepLimits())
				} else {
					_, err = e.ExportDiskStep(ctx, p, usage.DefaultDiskExportStepLimits())
				}
				if err != nil {
					t.Fatal(err)
				}
				// Facts after archive H must survive startup trimming, even
				// though the fresh reducer includes the whole native suffix.
				seedLedgerActivity(t, r, "unexported-startup-tail")
				if err := r.usageCollector.CatchUp(ctx); err != nil {
					t.Fatal(err)
				}
				before, err := p.ReadStats(ctx)
				if err != nil {
					t.Fatal(err)
				}
				rootPath := filepath.Join(r.config.UsagePath(), "manifest.json")
				root, err := os.ReadFile(rootPath)
				if err != nil {
					t.Fatal(err)
				}
				for cycle := 0; cycle < 2; cycle++ {
					if cycle != 0 {
						if err := r.usage.closeOwner(); err != nil {
							t.Fatal(err)
						}
						p = installDiskUsageQueries(t, r)
						bindStartupUsageArchive(t, r, format)
					}
					out := restoreStartupUsageWindow(t, r, now)
					if !out.Verified || out.Steps < 2 || out.Window.Attempts != 0 || out.Window.Summaries != 1 || out.Window.TrimmedAttempts != 129 || (out.Archive.CatalogVersion == 0) != legacy {
						t.Fatal("startup did not restore the finite window", cycle, out)
					}
					after, err := p.ReadStats(ctx)
					if err != nil || after.Totals != before.Totals || after.Metrics != before.Metrics || after.Watermark != before.Watermark {
						t.Fatal("rebuild/resweep changed cumulative facts", before, after, err)
					}
					actual, err := os.ReadFile(rootPath)
					if err != nil || !bytes.Equal(root, actual) {
						t.Fatal("startup resweep rewrote the archive", err)
					}
				}
			})
		}
	}
}

func TestUsageStartupWindowRebuildRetainedPolicyNeedsFreshNativeKeyAndComparison(t *testing.T) {
	for _, format := range []string{usage.FormatNDJSON, usage.FormatParquet} {
		t.Run(format, func(t *testing.T) {
			r, _ := summaryRuntime(t, 0)
			now := time.Now().UTC()
			seedRetentionUsage(t, r, 2, now.AddDate(0, 0, -60))
			billOneRequest(t, r, "retained-current")
			p := installDiskUsageQueries(t, r)
			e := bindStartupUsageArchive(t, r, format)
			ctx := context.Background()
			if _, err := e.ExportDiskStep(ctx, p, usage.DefaultDiskExportStepLimits()); err != nil {
				t.Fatal(err)
			}
			cutoff := now.AddDate(0, 0, -45)
			for {
				out, err := e.PrepareRetainedDateCatalogStep(ctx, cutoff, usage.DefaultDateCatalogBuildLimits())
				if err != nil {
					t.Fatal(err)
				}
				if out.Phase == "ready" {
					break
				}
			}
			for {
				out, err := e.VerifyRetainedDateCatalogStep(ctx, p, cutoff, usage.DefaultDateCatalogVerifyLimits())
				if err != nil {
					t.Fatal(err)
				}
				if out.Phase == "ready" {
					break
				}
			}
			if _, err := e.ActivateRetainedDateCatalog(ctx, p, cutoff); err != nil {
				t.Fatal(err)
			}
			if err := r.usage.closeOwner(); err != nil {
				t.Fatal(err)
			}
			p = installDiskUsageQueries(t, r)
			bindStartupUsageArchive(t, r, format)
			before, err := p.ReadStats(ctx)
			if err != nil {
				t.Fatal(err)
			}
			out := restoreStartupUsageWindow(t, r, now)
			if !out.Verified || out.ComparisonSteps == 0 || out.Archive.Retention.RetainFromDate != cutoff.Format("2006-01-02") || out.Window.Attempts != 1 || out.Window.Summaries != 1 || out.Window.TrimmedAttempts != 2 {
				t.Fatal("saved date root was not independently revalidated", out)
			}
			after, err := p.ReadStats(ctx)
			if err != nil || after.Totals != before.Totals || after.Metrics != before.Metrics {
				t.Fatal("date resweep changed cumulative facts", before, after, err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if r.usage.owner.nativeKey != ([32]byte{}) {
				t.Fatal("closed owner retained native key")
			}
		})
	}
}

func TestUsageStartupWindowMissingArchiveAndSourceFailuresWithdrawResult(t *testing.T) {
	for _, fault := range []string{"missing", "cancel", "archive-corrupt", "native-corrupt", "foreign-lease", "member", "trimmed"} {
		t.Run(fault, func(t *testing.T) {
			r, _ := summaryRuntime(t, 0)
			now := time.Now().UTC()
			seedRetentionUsage(t, r, 1, now.AddDate(0, 0, -60))
			p := installDiskUsageQueries(t, r)
			e := bindStartupUsageArchive(t, r, usage.FormatNDJSON)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if fault != "missing" {
				if _, err := e.ExportDiskStep(ctx, p, usage.DefaultDiskExportStepLimits()); err != nil {
					t.Fatal(err)
				}
			} else {
				// Empty shutdown may have created a legacy zero-row manifest.
				if err := os.Remove(filepath.Join(r.config.UsagePath(), "manifest.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
			}
			switch fault {
			case "cancel":
				cancel()
			case "archive-corrupt":
				files, err := e.ReadArchiveFiles(ctx, "", 1)
				if err != nil || len(files.Files) != 1 {
					t.Fatal(files, err)
				}
				if err := os.WriteFile(filepath.Join(r.config.UsagePath(), filepath.FromSlash(files.Files[0].Path)), []byte("corrupt"), 0600); err != nil {
					t.Fatal(err)
				}
			case "native-corrupt":
				forgeUsageFrame(t, r.config.LedgerPath())
			case "member":
				r.replication = &replicationRuntime{role: replication.RoleReplica}
				defer func() { r.replication = nil }()
			case "trimmed":
				restoreStartupUsageWindow(t, r, now)
			}
			var lease *ledger.MaintenanceLease
			var err error
			if fault == "foreign-lease" {
				other, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "other.wal"), ledger.NewStatus(), ledger.Options{ChainKey: bytes.Repeat([]byte{1}, 32)})
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
				lease, err = other.BeginMaintenance(ctx)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				lease, err = r.ledger.BeginMaintenance(context.Background())
				if err != nil {
					t.Fatal(err)
				}
			}
			out, err := r.restoreUsageDiskWindow(ctx, lease, now)
			_ = lease.Close()
			if fault == "missing" {
				stats, statErr := p.ReadStats(context.Background())
				if err != nil || out.Verified || out.Steps != 0 || stats.Window.Attempts != 1 || statErr != nil {
					t.Fatal("missing archive authorized trimming", out, stats, err, statErr)
				}
			} else if err == nil || out != (usageStartupWindowReport{}) {
				t.Fatal("failed startup returned usable scope", fault, out, err)
			}
		})
	}
}

func TestUsageStartupArchiveCloseJoinsPolicySourceBeforeClearingKeyAndNative(t *testing.T) {
	r, _ := summaryRuntime(t, 0)
	now := time.Now().UTC()
	seedRetentionUsage(t, r, 1, now.AddDate(0, 0, -60))
	p := installDiskUsageQueries(t, r)
	e := bindStartupUsageArchive(t, r, usage.FormatNDJSON)
	ctx := context.Background()
	if _, err := e.ExportDiskStep(ctx, p, usage.DefaultDiskExportStepLimits()); err != nil {
		t.Fatal(err)
	}
	cutoff := now.AddDate(0, 0, -45)
	for {
		out, err := e.PrepareRetainedDateCatalogStep(ctx, cutoff, usage.DefaultDateCatalogBuildLimits())
		if err != nil {
			t.Fatal(err)
		}
		if out.Phase == "ready" {
			break
		}
	}
	for {
		out, err := e.VerifyRetainedDateCatalogStep(ctx, p, cutoff, usage.DefaultDateCatalogVerifyLimits())
		if err != nil {
			t.Fatal(err)
		}
		if out.Phase == "ready" {
			break
		}
	}
	if _, err := e.ActivateRetainedDateCatalog(ctx, p, cutoff); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	// Install a controlled resolver only after the old Exporter joined. The
	// same owned key and real native prefix authenticate the existing receipt;
	// its callback deliberately continues real IO after retirement starts.
	owner := r.usage.owner
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	dependency := make(chan error, 1)
	e, err := usage.NewExporterWithOptions(r.config.UsagePath(), usage.Options{Format: usage.FormatNDJSON, DatePolicyKey: owner.nativeKey[:], DatePolicyPrefix: func(_ context.Context, head ledger.Watermark) (ledger.ProjectionPrefix, error) {
		close(entered)
		<-release
		if owner.nativeKey == ([32]byte{}) {
			err := errors.New("native key cleared before actual resolver exit")
			dependency <- err
			return ledger.ProjectionPrefix{}, err
		}
		_, nativeErr := r.ledger.ReplayAuthenticated(context.Background(), ledger.ReplayAnchor{}, nil)
		prefix, _, inspectErr := ledger.InspectProjectionCursor(context.Background(), r.config.LedgerPath(), owner.nativeKey[:], head.Generation, head.Sequence)
		dependency <- errors.Join(nativeErr, inspectErr)
		return prefix, errors.Join(nativeErr, inspectErr)
	}})
	if err != nil {
		t.Fatal(err)
	}
	owner.archive, r.usageExporter = e, e
	read := make(chan error, 1)
	go func() {
		_, err := e.VerifyPublishedDateCatalogStep(ctx, p, cutoff, usage.DefaultDateCatalogVerifyLimits())
		read <- err
	}()
	awaitDrainSignal(t, entered)
	closed := make(chan error, 1)
	go func() { closed <- r.Close() }()
	awaitArchiveRetired(t, e)
	select {
	case err := <-closed:
		t.Fatal("Runtime closed before real resolver IO", err)
	default:
	}
	unblock()
	if err := <-dependency; err != nil {
		t.Fatal("native/key dependency retired before actual resolver exit", err)
	}
	if err := <-read; err == nil {
		t.Fatal("retired published comparison returned authority")
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if owner.nativeKey != ([32]byte{}) || owner.native != nil {
		t.Fatal("closed owner retained native key/source")
	}
}

func TestUsageStartupOwnerCannotBorrowForeignLeaseOrReplicatedNativeMode(t *testing.T) {
	for _, replicated := range []bool{false, true} {
		t.Run(map[bool]string{false: "foreign-lease", true: "replicated-mode"}[replicated], func(t *testing.T) {
			r, _ := summaryRuntime(t, 0)
			r.stopBackgroundWork()
			before, err := os.Lstat(standaloneUsageProjectionPath(r.config))
			if err != nil {
				t.Fatal(err)
			}
			options := ledger.Options{ChainKey: bytes.Repeat([]byte{1}, 32)}
			if replicated {
				options.AfterDurable = func(ledger.DurableBatch) (uint64, error) { return 1, nil }
				options.WaitConfirmed = func(context.Context, uint64) error { return nil }
			}
			other, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "other.wal"), ledger.NewStatus(), options)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			lease, err := other.BeginMaintenance(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			native := r.ledger
			if replicated {
				native = other
			}
			view, collector, err := prepareStandaloneUsage(context.Background(), r.config, native, r.accountingState.projection, lease, r.resourcePressure)
			if err == nil || view != nil || collector != nil {
				t.Fatal("foreign or replicated maintenance authorized Standalone startup", view, collector, err)
			}
			after, err := os.Lstat(standaloneUsageProjectionPath(r.config))
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("refusal changed selected Usage owner", err)
			}
		})
	}
}
