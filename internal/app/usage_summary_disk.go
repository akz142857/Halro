package app

import (
	"context"
	"path/filepath"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/usage"
)

// One fixed, exclusive directory bounds abandoned scratch names. ReadSummary
// creates/removes only its own directory/DB and checks physical identities.
// A preexisting path is preserved and refuses the query. Startup migration
// still needs explicit, authority-safe reconciliation of crash leftovers.
func usageSummaryScratchPath(cfg config.Config) string {
	return filepath.Join(cfg.Storage.DataDir, "usage-summary-scratch")
}

// Explicit job ceilings cover source pages, reduction work and the final
// response. They do not authorize disk space or charge shared physical fees.
func usageSummaryDiskLimits() usage.DiskSummaryLimits {
	return usage.DiskSummaryLimits{
		Pages: 4096, SourceRows: 65536, SourceBytes: 64 << 20, OutputBytes: 8 << 20,
		ScratchReads: 1 << 20, ScratchReadBytes: 256 << 20, ScratchWriteBytes: 256 << 20,
		ScratchFileBytes: 64 << 20, Periods: 4096, PeriodBytes: 8 << 20,
		MaxDuration: time.Minute, Page: usageHTTPPageLimits(),
	}
}

func (r *Runtime) readDiskUsageSummary(ctx context.Context, q usage.DiskDailySummaryQuery, timing timeContext) (usageSummaryResponse, []summaryGroup, error) {
	path := usageSummaryScratchPath(r.config)
	// Protect both the directory namespace and the actual file before any
	// scratch mutation. Reuse the context-aware configured/computed authority
	// identity checks; never remove a preexisting path to make this job fit.
	for _, candidate := range []string{path, filepath.Join(path, "summary.db")} {
		if err := validateRuntimeDerivedPath(ctx, r.config, candidate); err != nil {
			return usageSummaryResponse{}, nil, err
		}
	}
	summary, err := r.usage.disk.ReadSummary(ctx, q, usageSummaryDiskLimits(), path)
	if err != nil {
		return usageSummaryResponse{}, nil, err
	}
	payload := summary.Payload
	ceiling, known := summaryBucketCeilings[payload.Granularity]
	if !known || len(payload.Buckets) > ceiling || len(payload.TimezoneChanges) > 4096 || payload.Groups != nil && len(*payload.Groups) > 101 || payload.WatermarkSequence != summary.Head.Sequence {
		return usageSummaryResponse{}, nil, usage.ErrDiskUsageCorrupt
	}
	body := usageSummaryResponse{
		Granularity: payload.Granularity, Start: payload.Start, End: payload.End,
		Totals: summaryMetrics(payload.Totals), WatermarkSequence: summary.Head.Sequence,
		TimeContext: timing, GroupBy: payload.GroupBy, Sort: payload.Sort, Order: payload.Order,
		GroupsTruncated: payload.GroupsTruncated, GroupsOtherCount: payload.GroupsOtherCount,
		Buckets:         make([]summaryBucket, len(payload.Buckets)),
		TimezoneChanges: make([]summaryTimezoneChange, len(payload.TimezoneChanges)),
	}
	for i, bucket := range payload.Buckets {
		body.Buckets[i] = summaryBucket{Period: bucket.Period, Start: bucket.Start, End: bucket.End, summaryMetrics: summaryMetrics(bucket.SummaryMetrics)}
	}
	for i, change := range payload.TimezoneChanges {
		body.TimezoneChanges[i] = summaryTimezoneChange(change)
	}
	var groups []summaryGroup
	if payload.Groups != nil {
		groups = make([]summaryGroup, len(*payload.Groups))
		for i, group := range *payload.Groups {
			groups[i] = summaryGroup{Key: group.Key, summaryMetrics: summaryMetrics(group.SummaryMetrics)}
		}
		body.Groups = &groups
	}
	if payload.Filter != nil {
		filter := map[string]string{"dimension": payload.Filter.Dimension, "value": payload.Filter.Value}
		body.Filter = &filter
	}
	return body, groups, ctx.Err()
}

func (r *Runtime) readLegacyUsageSummary(start, end, granularity, groupBy, filterDimension, filterValue, sortKey string, ascending bool, limit int, timing timeContext) (usageSummaryResponse, []summaryGroup, error) {
	reportDimension := domain.RollupDimensionTotal
	if filterDimension != "" {
		reportDimension = filterDimension
	}
	wanted := []string{reportDimension}
	if groupBy != "" && groupBy != reportDimension {
		wanted = append(wanted, groupBy)
	}
	rows, changes, err := r.collectSummaryRows(start, end, wanted, filterValue)
	if err != nil {
		return usageSummaryResponse{}, nil, err
	}
	buckets, totals, groups, truncated, otherCount := summarize(
		rows[reportDimension], rows[groupBy], granularity, reportDimension, groupBy,
		summaryOrder{key: sortKey, ascending: ascending}, limit)
	body := usageSummaryResponse{Granularity: granularity, Start: start, End: end, Buckets: buckets, Totals: totals, TimezoneChanges: changes, WatermarkSequence: r.usage.Watermark().Sequence, TimeContext: timing}
	if groupBy != "" {
		body.GroupBy, body.Groups, body.GroupsTruncated, body.GroupsOtherCount = groupBy, &groups, &truncated, &otherCount
		body.Sort, body.Order = sortKey, map[bool]string{true: "asc", false: "desc"}[ascending]
	}
	if filterDimension != "" {
		filter := map[string]string{"dimension": filterDimension, "value": filterValue}
		body.Filter = &filter
	}
	return body, groups, nil
}
