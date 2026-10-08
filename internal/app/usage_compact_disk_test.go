package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/usage"
)

func compactDiskFixture(t *testing.T, format, catalog string) config.Config {
	t.Helper()
	r, _ := summaryRuntime(t, 3)
	r.stopBackgroundWork()
	cfg := r.config
	cfg.Usage.ExportFormat = format
	// summaryRuntime starts real maintenance, which may have exported its
	// initial three attempts before the worker join. Explicitly establish the
	// missing-root branch in this disposable fixture after that join.
	if catalog == "missing" {
		if err := os.RemoveAll(cfg.UsagePath()); err != nil {
			t.Fatal(err)
		}
	}
	if catalog != "missing" {
		if err := r.usageCollector.CatchUp(context.Background()); err != nil {
			t.Fatal(err)
		}
		initial := usageSnapshotForTest(t, r)
		if err := r.usageExporter.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(cfg.UsagePath()); err != nil {
			t.Fatal(err)
		}
		var err error
		r.usageExporter, err = usage.NewExporterWithOptions(cfg.UsagePath(), usage.Options{Format: format})
		if err != nil {
			t.Fatal(err)
		}
		// These branches deliberately cover the legacy exporter and its explicit
		// migration. Default Runtime now exports paged catalogs, so construct the
		// intended old format from the actual three-request native projection.
		if _, err := r.usageExporter.Export(initial); err != nil {
			t.Fatal(err)
		}
		if catalog == "paged" {
			if _, err := r.usageExporter.MigrateArchiveCatalog(context.Background(), time.Minute); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := 0; i < 4; i++ {
		billOneRequest(t, r, "compact_tail_"+string(rune('a'+i)))
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func smallCompactionLimits() usageCompactionLimits {
	limits := defaultUsageCompactionLimits()
	limits.Step.Source.Rows = 2
	return limits
}

func compactAuthorityTree(t *testing.T, cfg config.Config) map[string][32]byte {
	t.Helper()
	tree := doctorTree(t, cfg.Storage.DataDir)
	archive, err := filepath.Rel(cfg.Storage.DataDir, cfg.UsagePath())
	if err != nil {
		t.Fatal(err)
	}
	for path := range tree {
		if path == archive || strings.HasPrefix(path, archive+string(filepath.Separator)) {
			delete(tree, path)
		}
	}
	return tree
}

func TestCompactUsageDiskCompletesFiniteStepsAcrossCatalogsAndFormats(t *testing.T) {
	for _, format := range []string{usage.FormatParquet, usage.FormatNDJSON} {
		for _, catalog := range []string{"missing", "legacy", "paged"} {
			t.Run(format+"/"+catalog, func(t *testing.T) {
				cfg := compactDiskFixture(t, format, catalog)
				scratch := t.TempDir()
				t.Setenv("TMPDIR", scratch)
				before := compactAuthorityTree(t, cfg)
				var steps int
				var through uint64
				report, err := compactUsageDisk(context.Background(), cfg, smallCompactionLimits(), func(step int, cut usage.DiskArchiveCut) error {
					if cut.Rows > 2 || cut.ThroughSequence > cut.Head.Sequence || step > 1 && cut.AfterSequence != through {
						t.Fatal("step did not preserve its finite native prefix", step, cut)
					}
					steps, through = step, cut.ThroughSequence
					return nil
				}, nil)
				wantRows, wantSteps := uint64(4), 2
				if catalog == "missing" {
					wantRows, wantSteps = 7, 4
				}
				if err != nil || report.Steps != wantSteps || steps != wantSteps || report.WrittenRows != wantRows || report.Archive.CatalogVersion != map[string]int{"missing": 0, "legacy": 0, "paged": 1}[catalog] || !report.Archive.CompletePrefix || report.ThroughSequence != through || report.ThroughSequence != report.Verification.SourceHead.Sequence || report.Verification.LedgerRecords != 7 || report.Verification.ParquetRecords != 7 {
					t.Fatal(report, steps, err)
				}
				if !reflect.DeepEqual(before, compactAuthorityTree(t, cfg)) {
					t.Fatal("disk compaction changed native authority or old Usage derivatives")
				}
				entries, err := os.ReadDir(scratch)
				if err != nil || len(entries) != 0 {
					t.Fatal("reference cleanup incomplete", entries, err)
				}
				// Reopen the actual public entry. Nothing new is credited and the
				// already committed root/files keep their bytes.
				archive := doctorTree(t, cfg.UsagePath())
				reopened, err := CompactUsage(context.Background(), cfg)
				if err != nil || reopened.WrittenRows != 0 || reopened.Steps != 1 || reopened.AfterSequence != through || !reflect.DeepEqual(archive, doctorTree(t, cfg.UsagePath())) {
					t.Fatal("idempotent reopen changed archive", reopened, err)
				}
				if catalog != "paged" {
					started, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
					if err != nil {
						t.Fatal("compatible compact prevented ordinary startup", err)
					}
					if err := started.Close(); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func TestCompactUsageDiskInterruptionAndStepLimitResumePublishedPrefix(t *testing.T) {
	for _, scenario := range []string{"callback", "cancel", "step-limit"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := compactDiskFixture(t, usage.FormatNDJSON, "missing")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			limits := smallCompactionLimits()
			stop := errors.New("fixture stopped after a committed prefix")
			var afterStep func(int, usage.DiskArchiveCut) error
			want := error(stop)
			switch scenario {
			case "callback":
				afterStep = func(int, usage.DiskArchiveCut) error { return stop }
			case "cancel":
				afterStep = func(int, usage.DiskArchiveCut) error { cancel(); return nil }
				want = context.Canceled
			case "step-limit":
				limits.Steps = 1
				want = usage.ErrDiskUsageLimit
			}
			report, err := compactUsageDisk(ctx, cfg, limits, afterStep, nil)
			if !errors.Is(err, want) || report != (UsageCompactionReport{}) {
				t.Fatal("partial prefix reported as completed compact", report, err)
			}
			prefix, err := VerifyUsage(context.Background(), cfg)
			if err != nil || prefix.ParquetRecords != 2 || prefix.ThroughSequence >= prefix.SourceHead.Sequence {
				t.Fatal("interruption left an incomplete or overstated prefix", prefix, err)
			}
			before := doctorTree(t, cfg.UsagePath())
			retried, err := CompactUsage(context.Background(), cfg)
			if err != nil || retried.AfterSequence != prefix.ThroughSequence || retried.WrittenRows != 5 || retried.Verification.ParquetRecords != 7 || retried.ThroughSequence != retried.Verification.SourceHead.Sequence {
				t.Fatal("retry did not resume committed prefix", retried, err)
			}
			after := doctorTree(t, cfg.UsagePath())
			for path, digest := range before {
				if path != "manifest.json" && after[path] != digest {
					t.Fatal("retry changed an immutable prior partition/page", path)
				}
			}
		})
	}
}

func TestCompactUsageDiskRejectsBadExistingArchiveBeforeMigration(t *testing.T) {
	for _, scenario := range []string{"partition", "missing-prefix", "empty-coverage"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := compactDiskFixture(t, usage.FormatNDJSON, "legacy")
			e, err := usage.NewExporterWithOptions(cfg.UsagePath(), usage.Options{Format: usage.FormatNDJSON})
			if err != nil {
				t.Fatal(err)
			}
			m, err := e.LoadManifest()
			if err != nil || len(m.Files) == 0 {
				t.Fatal(m, err)
			}
			switch scenario {
			case "partition":
				path := filepath.Join(cfg.UsagePath(), m.Files[0].Path)
				if err := os.WriteFile(path, []byte("bad partition"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing-prefix":
				if err := os.Remove(filepath.Join(cfg.UsagePath(), m.Files[0].Path)); err != nil {
					t.Fatal(err)
				}
			case "empty-coverage":
				// A syntactically valid root claims the old nonzero prefix,
				// but supplies no native attempts for it.
				if err := os.WriteFile(filepath.Join(cfg.UsagePath(), "manifest.json"), []byte(`{"schema_version":8,"last_sequence":15,"files":[]}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := doctorTree(t, cfg.Storage.DataDir)
			report, err := CompactUsage(context.Background(), cfg)
			if err == nil || report != (UsageCompactionReport{}) || !reflect.DeepEqual(before, doctorTree(t, cfg.Storage.DataDir)) {
				t.Fatal("bad archive was migrated/appended", report, err)
			}
		})
	}
}

func TestCompactUsageDiskFinalNativeChangeWithdrawsReportAndCleansReference(t *testing.T) {
	cfg := compactDiskFixture(t, usage.FormatNDJSON, "missing")
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	report, err := compactUsageDisk(context.Background(), cfg, defaultUsageCompactionLimits(), nil, func() error {
		f, err := os.OpenFile(cfg.LedgerPath(), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		_, err = f.Write([]byte("late partial source"))
		return errors.Join(err, f.Close())
	})
	if err == nil || report != (UsageCompactionReport{}) {
		t.Fatal("fresh DB/archive certified changed native source", report, err)
	}
	entries, err := os.ReadDir(scratch)
	if err != nil || len(entries) != 0 {
		t.Fatal("failed final authority check retained reference", entries, err)
	}
}

func TestCompactUsageDiskRefusesMemberAndPreCancelledMissingDirectory(t *testing.T) {
	cfg, _, _, _, _ := offlineUsageMemberFixture(t)
	before := doctorTree(t, cfg.Storage.DataDir)
	for _, configured := range []bool{true, false} {
		if !configured {
			cfg.Replication = nil
		}
		if report, err := CompactUsage(context.Background(), cfg); err == nil || report != (UsageCompactionReport{}) || !reflect.DeepEqual(before, doctorTree(t, cfg.Storage.DataDir)) {
			t.Fatal("offline compact mutated member", configured, report, err)
		}
	}
	cfg = testConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if report, err := CompactUsage(ctx, cfg); !errors.Is(err, context.Canceled) || report != (UsageCompactionReport{}) {
		t.Fatal(report, err)
	}
	if _, err := os.Stat(cfg.Storage.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled compact created a directory", err)
	}
}

func TestUsageDiskReferenceCancellationDuringPhysicalCleanupWithdrawsReport(t *testing.T) {
	r, _ := summaryRuntime(t, 1)
	r.stopBackgroundWork()
	cfg := r.config
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	selected := make(chan *usage.DiskProjection, 1)
	finished := make(chan error, 1)
	done := make(chan error, 1)
	go func() {
		scope, err := withUsageDiskReference(ctx, cfg, func(_ context.Context, p *usage.DiskProjection, _ *usage.Exporter, _ usage.ReconciliationReport) error {
			go func() {
				_, err := p.StreamArchive(context.Background(), 0, usage.DefaultDiskExportLimits().Source, func(context.Context, []usage.AttemptEvent) error {
					close(entered)
					<-release // Actual work holds the projection lifetime after cancel.
					return nil
				})
				finished <- err
			}()
			<-entered
			selected <- p
			return nil
		}, nil)
		if scope != (usage.ReconciliationReport{}) {
			err = errors.Join(err, errors.New("cleanup cancellation returned a success scope"))
		}
		done <- err
	}()
	p := <-selected
	deadline := time.Now().Add(5 * time.Second)
	for {
		// Probe the zero-queue heavy-job gate. A point-read probe could race a
		// queued Close writer and wait behind the deliberately blocked reader.
		_, err := p.StreamArchive(context.Background(), 0, usage.DefaultDiskExportLimits().Source, func(context.Context, []usage.AttemptEvent) error {
			return errors.New("unexpected second source admission")
		})
		if errors.Is(err, usage.ErrDiskUsageClosed) {
			break // Close withdrew reads but is still joining the admitted stream.
		}
		if !errors.Is(err, usage.ErrDiskUsageBusy) || time.Now().After(deadline) {
			t.Fatal("reference did not reach physical Close", err)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		t.Fatal("reference returned before actual source job exited", err)
	default:
	}
	if next, err := lock.AcquireExistingReadOnly(cfg.Storage.DataDir); err == nil {
		_ = next.Close()
		t.Fatal("external directory ownership released before physical DB join")
	}
	unblock()
	if err := <-finished; err == nil {
		t.Fatal("retired source produced an accepted receipt")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("late cleanup cancellation did not withdraw the report", err)
	}
	entries, err := os.ReadDir(scratch)
	if err != nil || len(entries) != 0 {
		t.Fatal("physical cleanup retained a reference", entries, err)
	}
}
