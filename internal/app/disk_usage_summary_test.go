package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/usage"
)

// The reference uses real MAC records, Aggregate checkpoints and Metadata
// Store, then the unchanged HTTP Summary reducer. This tests the candidate
// response core; HTTP time_context/resource_labels/default Runtime are separate.
func TestDiskUsageSummaryMatchesCheckpointSummary(t *testing.T) {
	ctx := context.Background()
	var events []ledger.Event
	for i := 0; i < 24; i++ {
		base := projectedWriterAcceptance(fmt.Sprint("disk_summary_", i))
		base.PeriodID = []string{"2026-09-30", "2026-10-01", "2026-10-03"}[i%3]
		base.PeriodTimezoneVersion = uint64([]int{0, 2, 10, 2}[i%4])
		start, err := time.Parse("2006-01-02", base.PeriodID)
		if err != nil {
			t.Fatal(err)
		}
		base.PeriodStartMicros, base.PeriodEndMicros = start.UnixMicro(), start.Add(24*time.Hour).UnixMicro()
		base.OccurredAt = start.Add(time.Hour)
		base.ProjectID = fmt.Sprint("project_", i%3)
		base.RequestedModel = []string{"a", "b", "c", domain.RollupOtherKey}[i%4]
		kinds := []ledger.EventKind{ledger.EventRequestAccepted, ledger.EventReservationCreated, ledger.EventAttemptStarted, ledger.EventAttemptSettled, ledger.EventRequestFinalized}
		if i%5 == 0 {
			kinds[3], kinds[4] = kinds[4], kinds[3]
		}
		for n, kind := range kinds {
			e := base
			e.Kind = kind
			e.EventID = fmt.Sprintf("%s_%d", base.RequestID, kind)
			e.OccurredAt = base.OccurredAt.Add(time.Duration(n+1) * time.Second)
			if kind == ledger.EventRequestFinalized {
				e.OccurredAt = base.OccurredAt.Add(5 * time.Minute)
				e.Outcome = "success"
				if i%3 == 0 || i%5 == 0 {
					e.Outcome = "interrupted"
				}
			}
			if kind == ledger.EventReservationCreated || kind == ledger.EventAttemptStarted || kind == ledger.EventAttemptSettled {
				e.AttemptID = "att_" + base.RequestID
				e.ProviderID = fmt.Sprint("provider_", i%3)
				e.DeploymentID = fmt.Sprint("deployment_", i%4)
				e.ProviderModel = fmt.Sprint("provider_model_", i%5)
				e.ReservationMicrosUSD = ledger.MicrosUSD(100)
			}
			if kind == ledger.EventAttemptSettled {
				e.Outcome = "success"
				if i%3 == 0 {
					e.Outcome = "provider_error"
					e.FailurePhase = "provider"
					e.ErrorClass = "timeout"
				}
				e.CommittedMicrosUSD = ledger.MicrosUSD(int64(i + 1))
				e.ProviderInputTokens = int64(i + 2)
				e.ProviderOutputTokens = int64(i % 4)
				e.ProviderCachedInputTokens = 1
				e.ProviderCacheWriteInputTokens = 2
				e.ProviderReasoningTokens = 3
				e.LatencyMillis = 180000 + int64(i+1)*1000
				e.TokenEstimated, e.CostEstimated = i%4 == 0, i%4 == 0
				if i%7 == 0 {
					e.CommittedMicrosUSD = ledger.MicrosUSD(0)
				}
				if i%11 == 0 {
					e.CommittedMicrosUSD = nil
					e.LeaseMode = ledger.LeaseModeUnknownAllowed
				}
				if i%5 == 0 {
					e.Outcome = "recovered_started_unknown_result"
					e.FailurePhase = "accounting"
					e.ErrorClass = ""
					e.TokenEstimated, e.CostEstimated, e.Ambiguous = true, true, true
				}
			}
			events = append(events, e)
			if kind == ledger.EventAttemptSettled && i%6 == 0 {
				events = append(events, e)
			}
		}
	}
	native, _, _ := projectedWriterLog(t, events, ledger.Options{})
	owner, _, err := native.BuildProjectionFromCursor(ctx, 1, uint64(len(events)), filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	derivative, disk, err := usage.NewDiskLedgerDerivative(owner, filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	defer derivative.Close()
	ref, err := usage.NewAggregateWithFinalizationReader(owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := native.ReplayAuthenticated(ctx, ledger.ReplayAnchor{}, func(record ledger.Record) error {
		if err := derivative.Apply(record); err != nil {
			return err
		}
		return ref.Apply(record)
	}); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := ref.TakeCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var segments []boltstore.UsageCheckpointSegment
	for _, segment := range checkpoint.Segments {
		segments = append(segments, boltstore.UsageCheckpointSegment{ID: segment.ID, Payload: segment.Payload})
	}
	if err := store.PutUsageCheckpoint(checkpoint.Watermark, checkpoint.Head, segments, checkpoint.RemovedSegments, domain.RollupVersion, checkpoint.Rollup); err != nil {
		t.Fatal(err)
	}
	r := &Runtime{store: store, usage: &runtimeUsage{Aggregate: ref}}
	limits := usage.DiskSummaryLimits{Pages: 4096, SourceRows: 65536, SourceBytes: 64 << 20, OutputBytes: 8 << 20, ScratchReads: 1 << 20, ScratchReadBytes: 256 << 20, ScratchWriteBytes: 256 << 20, ScratchFileBytes: 64 << 20, Periods: 4096, PeriodBytes: 8 << 20, MaxDuration: time.Minute, Page: usage.DiskUsagePageLimits{ScanRows: 16, InputBytes: 8 << 20, OutputBytes: 8 << 20}}
	queries := []usage.DiskDailySummaryQuery{}
	baseQuery := usage.DiskDailySummaryQuery{Start: "2026-09-30", End: "2026-10-03", Granularity: "day", GroupBy: domain.RollupDimensionRequestedModel, Sort: "cost", Order: "desc", Limit: 2}
	for _, granularity := range []string{"day", "month", "year"} {
		for _, metric := range []string{"cost", "calls", "tokens", "errors", "success_rate"} {
			for _, order := range []string{"asc", "desc"} {
				q := baseQuery
				q.Granularity, q.Sort, q.Order = granularity, metric, order
				queries = append(queries, q)
			}
		}
	}
	for _, dimension := range []string{domain.RollupDimensionProject, domain.RollupDimensionProvider, domain.RollupDimensionDeployment, domain.RollupDimensionProviderModel} {
		q := baseQuery
		q.GroupBy = dimension
		queries = append(queries, q)
	}
	q := baseQuery
	q.GroupBy = ""
	queries = append(queries, q)
	q.FilterDimension, q.FilterValue = domain.RollupDimensionProvider, "provider_1"
	queries = append(queries, q)
	q.FilterValue = "does_not_exist"
	queries = append(queries, q)
	for _, q := range queries {
		t.Run(fmt.Sprintf("%s/%s/%s/%s/%s", q.Granularity, q.GroupBy, q.Sort, q.Order, q.FilterValue), func(t *testing.T) {
			reportDimension := domain.RollupDimensionTotal
			if q.FilterDimension != "" {
				reportDimension = q.FilterDimension
			}
			dimensions := []string{reportDimension}
			if q.GroupBy != "" {
				dimensions = append(dimensions, q.GroupBy)
			}
			rows, changes, err := r.collectSummaryRows(q.Start, q.End, dimensions, q.FilterValue)
			if err != nil {
				t.Fatal(err)
			}
			buckets, totals, groups, truncated, otherCount := summarize(rows[reportDimension], rows[q.GroupBy], q.Granularity, reportDimension, q.GroupBy, summaryOrder{key: q.Sort, ascending: q.Order == "asc"}, q.Limit)
			want := map[string]any{"granularity": q.Granularity, "start": q.Start, "end": q.End, "buckets": buckets, "totals": totals, "timezone_changes": changes, "watermark_sequence": checkpoint.Watermark.Sequence}
			if q.GroupBy != "" {
				want["group_by"] = q.GroupBy
				want["groups"] = groups
				want["groups_truncated"] = truncated
				want["groups_other_count"] = otherCount
				want["sort"] = q.Sort
				want["order"] = q.Order
			}
			if q.FilterDimension != "" {
				want["filter"] = map[string]string{"dimension": q.FilterDimension, "value": q.FilterValue}
			}
			path := filepath.Join(t.TempDir(), "scratch")
			got, err := disk.ReadSummary(ctx, q, limits, path)
			if err != nil {
				t.Fatal(err)
			}
			if got.Head != checkpoint.Watermark {
				t.Fatal("checkpoint cut mismatch")
			}
			normalize := func(value any) any {
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				var result any
				if err := json.Unmarshal(raw, &result); err != nil {
					t.Fatal(err)
				}
				return result
			}
			if !reflect.DeepEqual(normalize(got.Payload), normalize(want)) {
				a, _ := json.Marshal(got.Payload)
				b, _ := json.Marshal(want)
				t.Fatalf("disk Summary differs\ngot %s\nwant %s", a, b)
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatal("scratch not retired", err)
			}
		})
	}
}
