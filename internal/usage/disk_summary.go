package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/bits"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	bbolt "go.etcd.io/bbolt"
	berrors "go.etcd.io/bbolt/errors"
)

// These are explicit candidate job limits, not calibrated product defaults.
// Source probes and scratch work are distinct. Scratch counters charge logical
// key/value reads (including repeated comparisons) and writes, not bbolt page
// IO, fsyncs, seek metadata or physical write amplification.
// MaxDuration is cooperative: permits remain held until actual calls join.
type DiskSummaryLimits struct {
	Pages, SourceRows, SourceBytes, OutputBytes                         int
	ScratchReads, ScratchReadBytes, ScratchWriteBytes, ScratchFileBytes int
	Periods, PeriodBytes                                                int
	MaxDuration                                                         time.Duration
	Page                                                                DiskUsagePageLimits
}
type DiskDailySummaryQuery struct {
	Start, End, Granularity, GroupBy, FilterDimension, FilterValue, Sort, Order string
	Limit                                                                       int
}
type SummaryMetrics struct {
	Requests                *int64 `json:"requests,omitempty"`
	RequestErrors           *int64 `json:"request_errors,omitempty"`
	RequestLatencySamples   *int64 `json:"request_latency_samples,omitempty"`
	RequestLatencyP95Millis *int64 `json:"request_latency_p95_millis,omitempty"`
	RequestLatencyOverMax   uint64 `json:"request_latency_over_max,omitempty"`
	Attempts                int64  `json:"attempts"`
	Errors                  int64  `json:"errors"`
	InputTokens             int64  `json:"input_tokens"`
	OutputTokens            int64  `json:"output_tokens"`
	EstimatedInputTokens    int64  `json:"estimated_input_tokens"`
	EstimatedOutputTokens   int64  `json:"estimated_output_tokens"`
	CachedInputTokens       int64  `json:"provider_cached_input_tokens"`
	CacheWriteInputTokens   int64  `json:"provider_cache_write_input_tokens"`
	ReasoningTokens         int64  `json:"provider_reasoning_tokens"`
	CostMicrosUSD           int64  `json:"cost_micros_usd"`
	EstimatedCostMicrosUSD  int64  `json:"estimated_cost_micros_usd"`
	UnknownAttempts         int64  `json:"unknown_attempts"`
	LatencyMillis           int64  `json:"latency_millis"`
	AttemptLatencySamples   int64  `json:"attempt_latency_samples"`
	AttemptLatencyP95Millis int64  `json:"attempt_latency_p95_millis"`
	AttemptLatencyOverMax   uint64 `json:"attempt_latency_over_max,omitempty"`
	LatencyApproximate      bool   `json:"latency_approximate"`
}
type SummaryBucket struct {
	Period string    `json:"period"`
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`
	SummaryMetrics
}
type SummaryGroup struct {
	Key string `json:"key"`
	SummaryMetrics
}
type SummaryTimezoneChange struct {
	PeriodID    string `json:"period_id"`
	FromVersion uint64 `json:"from_version"`
	ToVersion   uint64 `json:"to_version"`
}
type SummaryFilter struct {
	Dimension string `json:"dimension"`
	Value     string `json:"value"`
}
type SummaryPayload struct {
	Granularity       string                  `json:"granularity"`
	Start             string                  `json:"start"`
	End               string                  `json:"end"`
	Buckets           []SummaryBucket         `json:"buckets"`
	Totals            SummaryMetrics          `json:"totals"`
	TimezoneChanges   []SummaryTimezoneChange `json:"timezone_changes"`
	WatermarkSequence uint64                  `json:"watermark_sequence"`
	GroupBy           string                  `json:"group_by,omitempty"`
	Groups            *[]SummaryGroup         `json:"groups,omitempty"`
	GroupsTruncated   *bool                   `json:"groups_truncated,omitempty"`
	GroupsOtherCount  *int                    `json:"groups_other_count,omitempty"`
	Sort              string                  `json:"sort,omitempty"`
	Order             string                  `json:"order,omitempty"`
	Filter            *SummaryFilter          `json:"filter,omitempty"`
}
type DiskSummary struct {
	Payload                                                             SummaryPayload
	Head                                                                ledger.Watermark
	Pages, SourceRows, SourceBytes, OutputBytes                         int
	ScratchReads, ScratchReadBytes, ScratchWriteBytes, ScratchFileBytes int
}
type summaryPeriod struct {
	ID      string
	Version uint64
}
type summaryStored struct {
	Key        string
	Row        domain.DailyRollup
	Start, End int64
}
type summaryTop struct {
	ID   [32]byte
	Rank uint64
}
type summaryWork struct {
	ctx                          context.Context
	limits                       DiskSummaryLimits
	reads, readBytes, writeBytes int
}

func (w *summaryWork) check() error { return w.ctx.Err() }
func (w *summaryWork) get(input any, bucket string, key []byte) (summaryStored, bool, error) {
	tx := usageTransaction(input)
	var item summaryStored
	if err := w.check(); err != nil {
		return item, false, err
	}
	b := tx.Bucket([]byte(bucket))
	if b == nil {
		return item, false, ErrDiskUsageCorrupt
	}
	raw := b.Get(key)
	if len(raw) > diskUsageMaxValue {
		return item, false, ErrDiskUsageCorrupt
	}
	if w.reads == w.limits.ScratchReads || len(raw)+len(key) > w.limits.ScratchReadBytes-w.readBytes {
		return item, false, ErrDiskUsageLimit
	}
	w.reads++
	w.readBytes += len(raw) + len(key)
	if raw == nil {
		return item, false, nil
	}
	if json.Unmarshal(raw, &item) != nil || !bytes.Equal(diskUsageID(item.Key), key) || !diskRollupValidRow(item.Row, ^uint64(0)) {
		return item, false, ErrDiskUsageCorrupt
	}
	return item, true, nil
}
func (w *summaryWork) put(input any, bucket string, item summaryStored) error {
	tx := usageTransaction(input)
	if err := w.check(); err != nil {
		return err
	}
	raw, err := json.Marshal(item)
	if err != nil {
		return err
	}
	if len(raw) > diskUsageMaxValue || len(raw)+32 > w.limits.ScratchWriteBytes-w.writeBytes {
		return ErrDiskUsageLimit
	}
	w.writeBytes += len(raw) + 32
	return tx.Bucket([]byte(bucket)).Put(diskUsageID(item.Key), raw)
}
func summaryLabel(id, granularity string) string {
	switch granularity {
	case "month":
		if len(id) >= 7 {
			return id[:7]
		}
	case "year":
		if len(id) >= 4 {
			return id[:4]
		}
	}
	return id
}
func summaryContribution(row domain.DailyRollup, requestLevel bool) domain.DailyRollup {
	row.PeriodID = ""
	row.TimezoneVersion = 0
	row.Timezone = ""
	row.PeriodStartMicros = 0
	row.PeriodEndMicros = 0
	if !requestLevel {
		row.DropRequestMetrics()
	}
	return row
}
func summaryApproxP95(bucket [domain.RollupLatencyBuckets]uint64, overflow uint64) (int64, bool) {
	var total uint64
	for _, n := range bucket {
		total += n
	}
	total += overflow
	if total == 0 {
		return 0, false
	}
	target := (total/100)*95 + ((total%100)*95+99)/100
	var sum uint64
	for i, n := range bucket {
		sum += n
		if sum >= target {
			return int64(LatencyBucketsMillis[i]), false
		}
	}
	return int64(LatencyBucketsMillis[len(LatencyBucketsMillis)-1]), true
}
func summaryRender(row domain.DailyRollup, requestLevel bool) SummaryMetrics {
	p95, over := summaryApproxP95(row.AttemptLatencyBuckets, row.AttemptLatencyOverflow)
	result := SummaryMetrics{Attempts: row.Attempts, Errors: row.Errors, InputTokens: row.InputTokens, OutputTokens: row.OutputTokens, EstimatedInputTokens: row.EstimatedInputTokens, EstimatedOutputTokens: row.EstimatedOutputTokens, CachedInputTokens: row.ProviderCachedInputTokens, CacheWriteInputTokens: row.ProviderCacheWriteInputTokens, ReasoningTokens: row.ProviderReasoningTokens, CostMicrosUSD: row.CostMicrosUSD, EstimatedCostMicrosUSD: row.EstimatedCostMicrosUSD, UnknownAttempts: row.UnknownAttempts, LatencyMillis: row.LatencyMillis, AttemptLatencySamples: row.AttemptLatencySamples, AttemptLatencyP95Millis: p95, LatencyApproximate: true}
	if over {
		result.AttemptLatencyOverMax = row.AttemptLatencyOverflow
	}
	if requestLevel {
		requests, errors, samples := row.Requests, row.RequestErrors, row.RequestLatencySamples
		result.Requests = &requests
		result.RequestErrors = &errors
		result.RequestLatencySamples = &samples
		p95, over := summaryApproxP95(row.RequestLatencyBuckets, row.RequestLatencyOverflow)
		result.RequestLatencyP95Millis = &p95
		if over {
			result.RequestLatencyOverMax = row.RequestLatencyOverflow
		}
	}
	return result
}
func summaryRank(row domain.DailyRollup, metric string, requestLevel bool) uint64 {
	calls, failures := uint64(row.Attempts), uint64(row.Errors)
	if requestLevel {
		calls, failures = uint64(row.Requests), uint64(row.RequestErrors)
	}
	switch metric {
	case "calls":
		return calls
	case "errors":
		return failures
	case "tokens":
		return uint64(row.InputTokens) + uint64(row.OutputTokens)
	case "success_rate":
		if calls == 0 {
			return 100000
		}
		hi, lo := bits.Mul64(calls-failures, 100000)
		q, _ := bits.Div64(hi, lo, calls)
		return q
	default:
		return uint64(row.CostMicrosUSD)
	}
}

// Only additive metric columns are subtracted; period identities were cleared.
// FirstSequence does not render and is deliberately cleared for the folded tail.
func summarySubtract(row *domain.DailyRollup, other domain.DailyRollup) error {
	pairs := []struct {
		p *int64
		n int64
	}{{&row.Requests, other.Requests}, {&row.RequestErrors, other.RequestErrors}, {&row.Attempts, other.Attempts}, {&row.Errors, other.Errors}, {&row.InputTokens, other.InputTokens}, {&row.OutputTokens, other.OutputTokens}, {&row.EstimatedInputTokens, other.EstimatedInputTokens}, {&row.EstimatedOutputTokens, other.EstimatedOutputTokens}, {&row.CostMicrosUSD, other.CostMicrosUSD}, {&row.EstimatedCostMicrosUSD, other.EstimatedCostMicrosUSD}, {&row.UnknownAttempts, other.UnknownAttempts}, {&row.LatencyMillis, other.LatencyMillis}, {&row.ProviderCachedInputTokens, other.ProviderCachedInputTokens}, {&row.ProviderCacheWriteInputTokens, other.ProviderCacheWriteInputTokens}, {&row.ProviderReasoningTokens, other.ProviderReasoningTokens}, {&row.RequestLatencySamples, other.RequestLatencySamples}, {&row.RequestLatencyMillis, other.RequestLatencyMillis}, {&row.AttemptLatencySamples, other.AttemptLatencySamples}}
	for _, p := range pairs {
		if p.n < 0 || *p.p < p.n {
			return ErrDiskUsageCorrupt
		}
		*p.p -= p.n
	}
	for i := range row.RequestLatencyBuckets {
		if row.RequestLatencyBuckets[i] < other.RequestLatencyBuckets[i] || row.AttemptLatencyBuckets[i] < other.AttemptLatencyBuckets[i] {
			return ErrDiskUsageCorrupt
		}
		row.RequestLatencyBuckets[i] -= other.RequestLatencyBuckets[i]
		row.AttemptLatencyBuckets[i] -= other.AttemptLatencyBuckets[i]
	}
	if row.RequestLatencyOverflow < other.RequestLatencyOverflow || row.AttemptLatencyOverflow < other.AttemptLatencyOverflow {
		return ErrDiskUsageCorrupt
	}
	row.RequestLatencyOverflow -= other.RequestLatencyOverflow
	row.AttemptLatencyOverflow -= other.AttemptLatencyOverflow
	row.FirstSequence = 0
	return nil
}
func summaryValidate(q DiskDailySummaryQuery, l DiskSummaryLimits) (DiskRollupQuery, int, error) {
	ceilings := map[string]int{"day": 400, "month": 36, "year": 10}
	ceiling, ok := ceilings[q.Granularity]
	if !ok || q.Limit < 1 || q.Limit > 100 || (q.Sort != "cost" && q.Sort != "calls" && q.Sort != "tokens" && q.Sort != "errors" && q.Sort != "success_rate") || (q.Order != "asc" && q.Order != "desc") {
		return DiskRollupQuery{}, 0, ErrDiskUsageLimit
	}
	if q.GroupBy != "" && (q.GroupBy == domain.RollupDimensionTotal || !domain.IsRollupDimension(q.GroupBy)) {
		return DiskRollupQuery{}, 0, ErrDiskUsageLimit
	}
	report := domain.RollupDimensionTotal
	if q.FilterDimension != "" {
		if !domain.IsRollupDimension(q.FilterDimension) || q.FilterDimension == domain.RollupDimensionTotal || q.FilterValue == "" || q.GroupBy != "" {
			return DiskRollupQuery{}, 0, ErrDiskUsageLimit
		}
		report = q.FilterDimension
	} else if q.FilterValue != "" {
		return DiskRollupQuery{}, 0, ErrDiskUsageLimit
	}
	dims := []string{report}
	if q.GroupBy != "" {
		dims = append(dims, q.GroupBy)
	}
	source := DiskRollupQuery{Start: q.Start, End: q.End, Dimensions: dims, FilterValue: q.FilterValue, Limit: 100}
	if err := diskRollupValidateQuery(source, l.Page); err != nil {
		return source, 0, err
	}
	start, _ := time.Parse("2006-01-02", q.Start)
	end, _ := time.Parse("2006-01-02", q.End)
	count := int(end.Sub(start)/(24*time.Hour)) + 1
	switch q.Granularity {
	case "month":
		count = (end.Year()-start.Year())*12 + int(end.Month()) - int(start.Month()) + 1
	case "year":
		count = end.Year() - start.Year() + 1
	}
	if count > ceiling || l.Pages < 1 || l.Pages > 4096 || l.SourceRows < 1 || l.SourceRows > 65536 || l.SourceBytes < 1 || l.SourceBytes > 64<<20 || l.OutputBytes < 1 || l.OutputBytes > 8<<20 || l.ScratchReads < 1 || l.ScratchReads > 1<<20 || l.ScratchReadBytes < 1 || l.ScratchReadBytes > 256<<20 || l.ScratchWriteBytes < 1 || l.ScratchWriteBytes > 256<<20 || l.ScratchFileBytes < 32768 || l.ScratchFileBytes > 512<<20 || l.Periods < 1 || l.Periods > 4096 || l.PeriodBytes < 1 || l.PeriodBytes > 8<<20 || l.MaxDuration <= 0 || l.MaxDuration > time.Minute {
		return source, 0, ErrDiskUsageLimit
	}
	return source, ceiling, nil
}
func summaryCleanup(path string, identity, fileIdentity os.FileInfo, db *bbolt.DB) error {
	if db != nil {
		if err := db.Close(); err != nil {
			return err
		}
	}
	now, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(identity, now) {
		return errors.New("Summary scratch directory identity changed")
	}
	if fileIdentity != nil {
		file := filepath.Join(path, "summary.db")
		now, err := os.Lstat(file)
		if err != nil {
			return err
		}
		if !os.SameFile(fileIdentity, now) {
			return errors.New("Summary scratch file identity changed")
		}
		if err := os.Remove(file); err != nil {
			return err
		}
	}
	return os.Remove(path)
}

// ReadSummary owns one exclusive scratch directory for the call, closes/unlinks
// its DB before returning, and joins retirement across source pages and scratch
// work. Caller still owns returned payload lifetime/fees and HTTP encoding.
// Path must not exist. Cleanup never removes other files or an existing path.
func (p *DiskProjection) ReadSummary(ctx context.Context, q DiskDailySummaryQuery, limits DiskSummaryLimits, scratchPath string) (result DiskSummary, err error) {
	return p.readSummary(ctx, q, limits, scratchPath, summaryCleanup)
}

func (p *DiskProjection) readSummary(ctx context.Context, q DiskDailySummaryQuery, limits DiskSummaryLimits, scratchPath string, cleanup func(string, os.FileInfo, os.FileInfo, *bbolt.DB) error) (result DiskSummary, err error) {
	source, ceiling, err := summaryValidate(q, limits)
	if err != nil {
		return DiskSummary{}, err
	}
	if p.retired.Load() || p.summaries == nil {
		return DiskSummary{}, ErrDiskUsageClosed
	}
	select {
	case p.summaries <- struct{}{}:
	default:
		return DiskSummary{}, ErrDiskUsageBusy
	}
	p.summaryMu.RLock()
	defer func() {
		<-p.summaries
		p.summaryMu.RUnlock()
		if errors.Is(err, berrors.ErrMaxSizeReached) {
			err = errors.Join(ErrDiskUsageLimit, err)
		}
		if err != nil {
			result = DiskSummary{}
		}
	}()
	if p.retired.Load() {
		return DiskSummary{}, ErrDiskUsageClosed
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	// Runs after real cleanup and before our own cancel. A deadline/retirement
	// during encoding or IO cannot publish a successful response afterward.
	defer func() {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = errors.Join(err, ctxErr)
		}
		if p.retired.Load() {
			err = errors.Join(err, ErrDiskUsageClosed)
		}
	}()
	if err := ctx.Err(); err != nil {
		return DiskSummary{}, err
	}
	encoded, err := json.Marshal(q)
	if err != nil {
		return DiskSummary{}, err
	}
	if len(encoded) > 1<<20 {
		return DiskSummary{}, ErrDiskUsageLimit
	}
	if err := os.Mkdir(scratchPath, 0o700); err != nil {
		return DiskSummary{}, err
	}
	identity, err := os.Lstat(scratchPath)
	if err != nil {
		_ = os.Remove(scratchPath)
		return DiskSummary{}, err
	}
	var fileIdentity os.FileInfo
	db, err := bbolt.Open(filepath.Join(scratchPath, "summary.db"), 0o600, &bbolt.Options{Timeout: time.Second, MaxSize: limits.ScratchFileBytes, FreelistType: bbolt.FreelistMapType, OpenFile: func(path string, flags int, mode os.FileMode) (*os.File, error) {
		file, err := os.OpenFile(path, flags|os.O_EXCL, mode)
		if err != nil {
			return nil, err
		}
		fileIdentity, err = file.Stat()
		if err != nil {
			_ = file.Close()
			return nil, err
		}
		return file, nil
	}})
	if err != nil {
		return DiskSummary{}, errors.Join(err, cleanup(scratchPath, identity, fileIdentity, nil))
	}
	defer func() {
		if cleanupErr := cleanup(scratchPath, identity, fileIdentity, db); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
	}()
	if err := wrapUsageDatabase(db).Update(func(tx *usageTx) error {
		for _, name := range []string{"buckets", "groups"} {
			if _, err := tx.CreateBucket([]byte(name)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return DiskSummary{}, err
	}
	work := summaryWork{ctx: ctx, limits: limits}
	periods := make(map[summaryPeriod]struct{})
	periodBytes := 0
	bucketCount, groupCount := 0, 0
	reportLevel := domain.RequestMetricDimensions[source.Dimensions[0]]
	groupLevel := domain.RequestMetricDimensions[q.GroupBy]
	var total, groupTotal domain.DailyRollup
	var cursor DiskRollupCursor
	for {
		if p.retired.Load() {
			return DiskSummary{}, ErrDiskUsageClosed
		}
		if err := work.check(); err != nil {
			return DiskSummary{}, err
		}
		if result.Pages == limits.Pages {
			return DiskSummary{}, ErrDiskUsageLimit
		}
		pageLimits := limits.Page
		pageLimits.ScanRows = min(pageLimits.ScanRows, limits.SourceRows-result.SourceRows)
		pageLimits.InputBytes = min(pageLimits.InputBytes, limits.SourceBytes-result.SourceBytes)
		if pageLimits.ScanRows < 1 || pageLimits.InputBytes < 1 {
			return DiskSummary{}, ErrDiskUsageLimit
		}
		page, err := p.RollupPage(ctx, source, cursor, pageLimits)
		if err != nil {
			return DiskSummary{}, err
		}
		result.Pages++
		result.SourceRows += page.Scanned
		result.SourceBytes += page.InputBytes
		if result.Pages == 1 {
			result.Head = page.Head
		} else if result.Head != page.Head {
			return DiskSummary{}, ErrDiskUsageCorrupt
		}
		for _, item := range page.Items {
			period := summaryPeriod{item.Key.PeriodID, item.Key.TimezoneVersion}
			if _, seen := periods[period]; !seen {
				if len(periods) == limits.Periods || len(period.ID)+16 > limits.PeriodBytes-periodBytes {
					return DiskSummary{}, ErrDiskUsageLimit
				}
				periods[period] = struct{}{}
				periodBytes += len(period.ID) + 16
			}
			if item.Key.Dimension == source.Dimensions[0] {
				contribution := summaryContribution(item.Row, reportLevel)
				if err := total.Add(contribution); err != nil {
					return DiskSummary{}, err
				}
				label := summaryLabel(item.Key.PeriodID, q.Granularity)
				if err := wrapUsageDatabase(db).Update(func(tx *usageTx) error {
					stored, found, err := work.get(tx, "buckets", diskUsageID(label))
					if err != nil {
						return err
					}
					if !found {
						if bucketCount == ceiling {
							return ErrDiskUsageLimit
						}
						bucketCount++
						stored.Key = label
						stored.Start = item.Row.PeriodStartMicros
					}
					if item.Row.PeriodStartMicros < stored.Start {
						stored.Start = item.Row.PeriodStartMicros
					}
					if item.Row.PeriodEndMicros > stored.End {
						stored.End = item.Row.PeriodEndMicros
					}
					if err := stored.Row.Add(contribution); err != nil {
						return err
					}
					return work.put(tx, "buckets", stored)
				}); err != nil {
					return DiskSummary{}, err
				}
			}
			if q.GroupBy != "" && item.Key.Dimension == q.GroupBy {
				contribution := summaryContribution(item.Row, groupLevel)
				if err := groupTotal.Add(contribution); err != nil {
					return DiskSummary{}, err
				}
				if err := wrapUsageDatabase(db).Update(func(tx *usageTx) error {
					stored, found, err := work.get(tx, "groups", diskUsageID(item.Key.DimensionKey))
					if err != nil {
						return err
					}
					if !found {
						groupCount++
						stored.Key = item.Key.DimensionKey
					}
					if err := stored.Row.Add(contribution); err != nil {
						return err
					}
					return work.put(tx, "groups", stored)
				}); err != nil {
					return DiskSummary{}, err
				}
			}
		}
		if page.Done {
			break
		}
		if page.Next == cursor {
			return DiskSummary{}, ErrDiskUsageCorrupt
		}
		cursor = page.Next
	}
	payload := SummaryPayload{Granularity: q.Granularity, Start: q.Start, End: q.End, Buckets: make([]SummaryBucket, 0, bucketCount), Totals: summaryRender(total, reportLevel), TimezoneChanges: make([]SummaryTimezoneChange, 0), WatermarkSequence: result.Head.Sequence}
	measured := 0
	charge := func(value any) error {
		if err := work.check(); err != nil {
			return err
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if len(raw) > limits.OutputBytes-measured {
			return ErrDiskUsageLimit
		}
		measured += len(raw)
		return nil
	}
	if err := charge(payload.Totals); err != nil {
		return DiskSummary{}, err
	}
	ordered := make([]summaryPeriod, 0, len(periods))
	for period := range periods {
		ordered = append(ordered, period)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].ID != ordered[j].ID {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].Version < ordered[j].Version
	})
	for i := 1; i < len(ordered); i++ {
		if ordered[i].Version != ordered[i-1].Version {
			change := SummaryTimezoneChange{PeriodID: ordered[i].ID, FromVersion: ordered[i-1].Version, ToVersion: ordered[i].Version}
			if err := charge(change); err != nil {
				return DiskSummary{}, err
			}
			payload.TimezoneChanges = append(payload.TimezoneChanges, change)
		}
	}
	// Only bounded output buckets are resident; every group stays on scratch disk.
	if err := summaryWalk(db, &work, "buckets", func(id [32]byte, stored summaryStored) error {
		bucket := SummaryBucket{Period: stored.Key, Start: time.UnixMicro(stored.Start).UTC(), End: time.UnixMicro(stored.End).UTC(), SummaryMetrics: summaryRender(stored.Row, reportLevel)}
		if err := charge(bucket); err != nil {
			return err
		}
		payload.Buckets = append(payload.Buckets, bucket)
		return nil
	}); err != nil {
		return DiskSummary{}, err
	}
	if len(payload.Buckets) != bucketCount {
		return DiskSummary{}, ErrDiskUsageCorrupt
	}
	sort.Slice(payload.Buckets, func(i, j int) bool { return payload.Buckets[i].Period < payload.Buckets[j].Period })
	if q.GroupBy != "" {
		top := make([]summaryTop, 0, q.Limit)
		walked := 0
		if err := summaryWalk(db, &work, "groups", func(id [32]byte, stored summaryStored) error {
			walked++
			candidate := summaryTop{id, summaryRank(stored.Row, q.Sort, groupLevel)}
			pos := len(top)
			// Binary insertion reads a companion only for a complete-key lexical tie.
			lo, hi := 0, len(top)
			for lo < hi {
				mid := (lo + hi) / 2
				better := candidate.Rank > top[mid].Rank
				if q.Order == "asc" {
					better = candidate.Rank < top[mid].Rank
				}
				if candidate.Rank == top[mid].Rank {
					var peer summaryStored
					if err := wrapUsageDatabase(db).View(func(tx *usageTx) error {
						var found bool
						var err error
						peer, found, err = work.get(tx, "groups", top[mid].ID[:])
						if err == nil && !found {
							return ErrDiskUsageCorrupt
						}
						return err
					}); err != nil {
						return err
					}
					better = stored.Key < peer.Key
				}
				if better {
					hi = mid
				} else {
					lo = mid + 1
				}
			}
			pos = lo
			if pos < q.Limit {
				if len(top) < q.Limit {
					top = append(top, summaryTop{})
				}
				copy(top[pos+1:], top[pos:len(top)-1])
				top[pos] = candidate
			}
			return nil
		}); err != nil {
			return DiskSummary{}, err
		}
		if walked != groupCount {
			return DiskSummary{}, ErrDiskUsageCorrupt
		}
		groups := make([]SummaryGroup, 0, len(top)+1)
		kept := domain.DailyRollup{}
		var keptOther domain.DailyRollup
		otherIndex := -1
		for _, ref := range top {
			var stored summaryStored
			if err := wrapUsageDatabase(db).View(func(tx *usageTx) error {
				var found bool
				var err error
				stored, found, err = work.get(tx, "groups", ref.ID[:])
				if err == nil && !found {
					return ErrDiskUsageCorrupt
				}
				return err
			}); err != nil {
				return DiskSummary{}, err
			}
			if err := kept.Add(stored.Row); err != nil {
				return DiskSummary{}, err
			}
			group := SummaryGroup{Key: stored.Key, SummaryMetrics: summaryRender(stored.Row, groupLevel)}
			if err := charge(group); err != nil {
				return DiskSummary{}, err
			}
			if stored.Key == domain.RollupOtherKey {
				otherIndex = len(groups)
				keptOther = stored.Row
			}
			groups = append(groups, group)
		}
		truncated := groupCount > len(top)
		otherCount := groupCount - len(top)
		if truncated {
			tail := groupTotal
			if err := summarySubtract(&tail, kept); err != nil {
				return DiskSummary{}, err
			}
			if otherIndex >= 0 {
				if err := keptOther.Add(tail); err != nil {
					return DiskSummary{}, err
				}
				groups[otherIndex] = SummaryGroup{Key: domain.RollupOtherKey, SummaryMetrics: summaryRender(keptOther, groupLevel)}
			} else {
				group := SummaryGroup{Key: domain.RollupOtherKey, SummaryMetrics: summaryRender(tail, groupLevel)}
				if err := charge(group); err != nil {
					return DiskSummary{}, err
				}
				groups = append(groups, group)
			}
		}
		payload.GroupBy = q.GroupBy
		payload.Groups = &groups
		payload.GroupsTruncated = &truncated
		payload.GroupsOtherCount = &otherCount
		payload.Sort = q.Sort
		payload.Order = q.Order
	}
	if q.FilterDimension != "" {
		payload.Filter = &SummaryFilter{Dimension: q.FilterDimension, Value: q.FilterValue}
	}
	if err := work.check(); err != nil {
		return DiskSummary{}, err
	}
	if p.retired.Load() {
		return DiskSummary{}, ErrDiskUsageClosed
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return DiskSummary{}, err
	}
	if len(raw) > limits.OutputBytes {
		return DiskSummary{}, ErrDiskUsageLimit
	}
	stat, err := os.Stat(filepath.Join(scratchPath, "summary.db"))
	if err != nil {
		return DiskSummary{}, err
	}
	if stat.Size() > int64(limits.ScratchFileBytes) {
		return DiskSummary{}, ErrDiskUsageLimit
	}
	result.Payload = payload
	result.OutputBytes = len(raw)
	result.ScratchReads = work.reads
	result.ScratchReadBytes = work.readBytes
	result.ScratchWriteBytes = work.writeBytes
	result.ScratchFileBytes = int(stat.Size())
	return result, nil
}
func summaryWalk(db *bbolt.DB, work *summaryWork, bucket string, visit func([32]byte, summaryStored) error) error {
	var after []byte
	for {
		var item summaryStored
		var id [32]byte
		found := false
		if err := wrapUsageDatabase(db).View(func(tx *usageTx) error {
			c := tx.Bucket([]byte(bucket)).Cursor()
			k, _ := c.First()
			if after != nil {
				k, _ = c.Seek(after)
				if bytes.Equal(k, after) {
					k, _ = c.Next()
				}
			}
			if k == nil {
				return work.check()
			}
			if len(k) != 32 {
				return ErrDiskUsageCorrupt
			}
			copy(id[:], k)
			var err error
			item, found, err = work.get(tx, bucket, k)
			return err
		}); err != nil {
			return err
		}
		if !found {
			return nil
		}
		if err := visit(id, item); err != nil {
			return err
		}
		after = bytes.Clone(id[:])
	}
}
