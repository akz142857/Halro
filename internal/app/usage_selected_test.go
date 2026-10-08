package app

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/usage"
)

func usageWatermarkForTest(t *testing.T, r *Runtime) ledger.Watermark {
	t.Helper()
	head, err := r.usage.readPosition(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return head
}

func usageMetricsForTest(t *testing.T, r *Runtime) usage.Metrics {
	t.Helper()
	stats, err := r.usage.observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return stats.Metrics
}

func usageFloorForTest(t *testing.T, r *Runtime) uint64 {
	t.Helper()
	if r.usage.disk == nil {
		return r.usage.Aggregate.Floor()
	}
	stats, err := r.usage.disk.ReadStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return stats.Window.Floor
}

// Only small test fixtures are materialized. These rows and counters come from
// the actual selected owner, never from a second replay used as an oracle.
// Production code continues to use finite disk pages and errorful observations.
func usageSnapshotForTest(t *testing.T, r *Runtime) usage.Snapshot {
	t.Helper()
	if r.usage.disk == nil {
		return r.usage.Aggregate.Snapshot()
	}
	ctx := context.Background()
	stats, err := r.usage.disk.ReadStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := usage.Snapshot{Watermark: stats.Watermark, Floor: stats.Window.Floor, Totals: stats.Totals, Hourly: []usage.Bucket{}}
	limits := usageHTTPPageLimits()
	var cursor usage.DiskUsageCursor
	for steps := 0; ; steps++ {
		if steps >= 100 {
			t.Fatal("snapshot fixture exceeded 100 finite pages")
		}
		page, err := r.usage.disk.AttemptPage(ctx, usage.AttemptQuery{Limit: 100}, cursor, limits)
		if err != nil || page.Head != stats.Watermark {
			t.Fatal("attempt fixture read changed cut", err)
		}
		out.Attempts = append(out.Attempts, page.Items...)
		if page.Done {
			break
		}
		cursor = page.Next
	}
	cursor = usage.DiskUsageCursor{}
	for steps := 0; ; steps++ {
		if steps >= 100 {
			t.Fatal("snapshot fixture exceeded 100 finite pages")
		}
		page, err := r.usage.disk.SummaryPage(ctx, usage.DiskSummaryQuery{Limit: 100}, cursor, limits)
		if err != nil || page.Head != stats.Watermark {
			t.Fatal("summary fixture read changed cut", err)
		}
		out.Requests = append(out.Requests, page.Items...)
		if page.Done {
			break
		}
		cursor = page.Next
	}
	sort.Slice(out.Attempts, func(i, j int) bool { return out.Attempts[i].Sequence < out.Attempts[j].Sequence })
	sort.Slice(out.Requests, func(i, j int) bool { return out.Requests[i].Sequence < out.Requests[j].Sequence })
	// ReadDashboard skips older hours but includes future hours. Using the first
	// fixture instant yields every hour in these small fixtures, with an explicit
	// 256-hour cap. Snapshot's legacy contract excludes derived request quality and estimated
	// hour measures; their actual disk values are covered by dashboard tests.
	now := time.Now().UTC()
	for _, a := range out.Attempts {
		if !a.CompletedAt.IsZero() && a.CompletedAt.Before(now) {
			now = a.CompletedAt
		}
	}
	for _, s := range out.Requests {
		if !s.CompletedAt.IsZero() && s.CompletedAt.Before(now) {
			now = s.CompletedAt
		}
	}
	dashboard, err := r.usage.disk.ReadDashboard(ctx, now, usage.Period{ID: "test-fixture"}, usage.DiskDashboardLimits{Hours: 256, ScanRows: 65536, InputBytes: 8 << 20, OutputBytes: 8 << 20})
	if err != nil || dashboard.Head != stats.Watermark {
		t.Fatal("hour fixture read changed cut", err)
	}
	for _, b := range dashboard.Dashboard.Hourly {
		b.RequestErrors, b.RequestLatencySamples, b.RequestLatencyP50Millis, b.RequestLatencyP95Millis = 0, 0, 0, 0
		b.EstimatedInputTokens, b.EstimatedOutputTokens, b.EstimatedCostMicrosUSD = 0, 0, 0
		out.Hourly = append(out.Hourly, b)
	}
	return out
}

// Legacy-only tests exercise the existing checkpoint transaction boundary used
// by member roles. Default Standalone tests keep their real disk selection.
func installLegacyUsageForTest(t *testing.T, r *Runtime) {
	t.Helper()
	r.stopBackgroundWork()
	if err := r.usageCollector.Retire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.usage.closeOwner(); err != nil {
		t.Fatal(err)
	}
	a, err := usage.NewAggregateWithFinalizationReader(r.accountingState.projection)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ledger.Replay(ledger.Watermark{}, a.Apply); err != nil {
		t.Fatal(err)
	}
	c, err := usage.NewCollector(a, r.ledger, r.config.Usage.AnalyticsQueueCapacity)
	if err != nil {
		t.Fatal(err)
	}
	e, err := usage.NewExporterWithOptions(r.config.UsagePath(), usage.Options{Format: r.config.Usage.ExportFormat})
	if err != nil {
		t.Fatal(err)
	}
	r.usage, r.usageCollector, r.usageExporter = &runtimeUsage{Aggregate: a}, c, e
}

func verifyUsageArchiveForTest(t *testing.T, r *Runtime) (usage.ReconciliationReport, error) {
	t.Helper()
	if r.usage.disk == nil {
		return r.usageExporter.Reconcile(r.usage.Aggregate.Snapshot())
	}
	var verified usage.ArchiveVerification
	err := r.withUsageMaintenanceReference(context.Background(), func(ctx context.Context, p *usage.DiskProjection, e *usage.Exporter) error {
		var err error
		verified, err = e.VerifyDiskArchive(ctx, p, usage.DefaultArchiveVerifyLimits())
		return err
	})
	if err != nil {
		return usage.ReconciliationReport{}, err
	}
	return usage.ReconciliationReport{LedgerRecords: int64(verified.SourceRows), ParquetRecords: int64(verified.ArchiveRows)}, nil
}
