package app

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/usage"
)

const (
	summaryDefaultGroupLimit = 50
	summaryMaxGroupLimit     = 100
	// A year view is 12 buckets; two years is the most a single response is
	// allowed to cover. Beyond that the caller is asked to narrow the range
	// rather than being handed a silently truncated one.
	summaryMaxMonths  = 24
	summaryDateLayout = "2006-01-02"
)

// summaryDimensionFilters maps a query parameter to the rollup dimension it
// selects. The names match the attempt detail endpoint so an operator moving
// between the two pages is spelling the same filter the same way.
var summaryDimensionFilters = map[string]string{
	"project_id":     domain.RollupDimensionProject,
	"model":          domain.RollupDimensionRequestedModel,
	"provider_id":    domain.RollupDimensionProvider,
	"deployment_id":  domain.RollupDimensionDeployment,
	"provider_model": domain.RollupDimensionProviderModel,
}

type summaryMetrics struct {
	// Request-level columns are absent, not zero, on a dimension that has no
	// request identity: a zero would read as "no requests here" rather than
	// "this question cannot be answered by this row".
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
	// LatencyApproximate is always true: a stored histogram can only place a
	// percentile inside a bucket. Saying so in the payload keeps the console
	// from presenting it as the exact figure the dashboard shows for today.
	LatencyApproximate bool `json:"latency_approximate"`
}

type summaryBucket struct {
	Period string `json:"period"`
	// The absolute interval the label covers, taken from the periods the rows
	// were stamped with rather than re-derived from the label. The console
	// charts against it and builds drill-down links from it: a date label
	// cannot be turned back into an instant without knowing which generation
	// of the accounting timezone produced it.
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	summaryMetrics
}

type summaryGroup struct {
	Key string `json:"key"`
	summaryMetrics
}

type summaryTimezoneChange struct {
	PeriodID    string `json:"period_id"`
	FromVersion uint64 `json:"from_version"`
	ToVersion   uint64 `json:"to_version"`
}

// adminUsageSummary answers "what did this month cost" from the daily rollup.
//
// Two shapes are allowed and they are mutually exclusive: group by one
// dimension, or filter to one dimension's value. The rollup is a marginal
// aggregate with no cross terms, so "this project's model mix" has no row that
// answers it — refusing the combination is how the endpoint avoids returning a
// number that looks like an answer to a question it never asked.
func (r *Runtime) adminUsageSummary(writer http.ResponseWriter, request *http.Request) {
	if !r.syncUsageAdmin(writer, request) {
		return
	}
	query := request.URL.Query()
	allowed := map[string]struct{}{
		"granularity": {}, "start": {}, "end": {}, "group_by": {}, "limit": {},
		"project_id": {}, "model": {}, "provider_id": {}, "deployment_id": {}, "provider_model": {},
	}
	for name := range query {
		if _, exists := allowed[name]; !exists {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "unsupported summary filter"})
			return
		}
	}
	granularity := query.Get("granularity")
	if granularity == "" {
		granularity = "day"
	}
	if granularity != "day" && granularity != "month" && granularity != "year" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "granularity must be day, month, or year"})
		return
	}
	groupBy := query.Get("group_by")
	if groupBy != "" && (!domain.IsRollupDimension(groupBy) || groupBy == domain.RollupDimensionTotal) {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "unknown group_by dimension"})
		return
	}
	filterDimension, filterValue := "", ""
	for name, dimension := range summaryDimensionFilters {
		value := query.Get(name)
		if value == "" {
			continue
		}
		if filterDimension != "" {
			writeJSON(writer, http.StatusBadRequest, map[string]string{
				"error": "the summary accepts one dimension filter; use the attempt detail view to combine them"})
			return
		}
		filterDimension, filterValue = dimension, value
	}
	if groupBy != "" && filterDimension != "" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{
			"error": "group_by and a dimension filter cannot be combined; the summary stores no cross terms"})
		return
	}
	limit := summaryDefaultGroupLimit
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > summaryMaxGroupLimit {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid group limit"})
			return
		}
		limit = parsed
	}

	now := time.Now()
	timing, ok := r.writeTimeContext(writer, now)
	if !ok {
		return
	}
	period, err := r.periods.PeriodAt(now)
	if err != nil {
		adminStoreError(writer)
		return
	}
	start, end, err := summaryRange(query.Get("start"), query.Get("end"), granularity, period.ID)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	dimension := domain.RollupDimensionTotal
	switch {
	case groupBy != "":
		dimension = groupBy
	case filterDimension != "":
		dimension = filterDimension
	}
	rows, changes, err := r.collectSummaryRows(start, end, dimension, filterValue)
	if err != nil {
		adminStoreError(writer)
		return
	}

	buckets, totals, groups, truncated, otherCount := summarize(rows, granularity, groupBy, limit)
	body := map[string]any{
		"granularity":        granularity,
		"start":              start,
		"end":                end,
		"buckets":            buckets,
		"totals":             totals,
		"timezone_changes":   changes,
		"watermark_sequence": r.usage.Snapshot().Watermark.Sequence,
		"time_context":       timing,
	}
	if groupBy != "" {
		body["group_by"] = groupBy
		body["groups"] = groups
		body["groups_truncated"] = truncated
		body["groups_other_count"] = otherCount
	}
	if filterDimension != "" {
		body["filter"] = map[string]string{"dimension": filterDimension, "value": filterValue}
	}
	if labels, err := r.summaryResourceLabels(request); err == nil {
		body["resource_labels"] = labels
	}
	writeJSON(writer, http.StatusOK, body)
}

// summaryRow is one accounting day's row for the dimension being reported on,
// already merged across the stored and not-yet-stored halves.
type summaryRow struct {
	key domain.RollupKey
	row domain.DailyRollup
}

// collectSummaryRows reads the stored rollup and folds in the increment that
// has not been written yet, so the answer covers the same events the dashboard
// beside it is already showing.
func (r *Runtime) collectSummaryRows(
	start, end, dimension, filterValue string,
) ([]summaryRow, []summaryTimezoneChange, error) {
	merged := map[domain.RollupKey]*domain.DailyRollup{}
	keep := func(key domain.RollupKey, row domain.DailyRollup) error {
		if key.PeriodID < start || key.PeriodID > end {
			return nil
		}
		if key.Dimension != dimension {
			return nil
		}
		if filterValue != "" && key.DimensionKey != filterValue {
			return nil
		}
		existing := merged[key]
		if existing == nil {
			stored := row
			merged[key] = &stored
			return nil
		}
		return existing.Add(row)
	}
	if err := r.store.UsageRollupRange(start, end, keep); err != nil {
		return nil, nil, err
	}
	for encoded, row := range r.usage.PendingRollup() {
		key, err := domain.DecodeRollupKey(encoded)
		if err != nil {
			return nil, nil, err
		}
		if err := keep(key, row); err != nil {
			return nil, nil, err
		}
	}
	rows := make([]summaryRow, 0, len(merged))
	for key, row := range merged {
		rows = append(rows, summaryRow{key: key, row: *row})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].key.PeriodID != rows[j].key.PeriodID {
			return rows[i].key.PeriodID < rows[j].key.PeriodID
		}
		if rows[i].key.TimezoneVersion != rows[j].key.TimezoneVersion {
			return rows[i].key.TimezoneVersion < rows[j].key.TimezoneVersion
		}
		return rows[i].key.DimensionKey < rows[j].key.DimensionKey
	})
	return rows, summaryTimezoneChanges(rows), nil
}

// summaryTimezoneChanges reports where the accounting timezone generation
// changed inside the range. A month that contains one is not a month of
// comparable days, and the console has to say so rather than adding two
// generations of the same date label together in silence.
func summaryTimezoneChanges(rows []summaryRow) []summaryTimezoneChange {
	changes := []summaryTimezoneChange{}
	var previous uint64
	seen := false
	for _, row := range rows {
		if !seen {
			previous, seen = row.key.TimezoneVersion, true
			continue
		}
		if row.key.TimezoneVersion != previous {
			changes = append(changes, summaryTimezoneChange{
				PeriodID: row.key.PeriodID, FromVersion: previous, ToVersion: row.key.TimezoneVersion,
			})
			previous = row.key.TimezoneVersion
		}
	}
	return changes
}

func summarize(
	rows []summaryRow, granularity, groupBy string, limit int,
) ([]summaryBucket, summaryMetrics, []summaryGroup, bool, int) {
	byBucket := map[string]*domain.DailyRollup{}
	bucketSpan := map[string][2]int64{}
	bucketOrder := []string{}
	byGroup := map[string]*domain.DailyRollup{}
	groupOrder := []string{}
	total := domain.DailyRollup{}
	requestLevel := groupBy == "" || domain.RequestMetricDimensions[groupBy]

	for _, row := range rows {
		label := bucketLabel(row.key.PeriodID, granularity)
		bucket := byBucket[label]
		if bucket == nil {
			bucket = &domain.DailyRollup{}
			byBucket[label] = bucket
			bucketOrder = append(bucketOrder, label)
		}
		span, spanned := bucketSpan[label]
		if !spanned || row.row.PeriodStartMicros < span[0] {
			span[0] = row.row.PeriodStartMicros
		}
		if row.row.PeriodEndMicros > span[1] {
			span[1] = row.row.PeriodEndMicros
		}
		bucketSpan[label] = span
		// Cross-period sums are additions of comparable columns, not of period
		// identities, so the identity is cleared before merging.
		contribution := row.row
		contribution.PeriodID, contribution.TimezoneVersion = "", 0
		contribution.Timezone, contribution.PeriodStartMicros, contribution.PeriodEndMicros = "", 0, 0
		if !requestLevel {
			contribution.DropRequestMetrics()
		}
		_ = bucket.Add(contribution)
		_ = total.Add(contribution)
		if groupBy != "" {
			group := byGroup[row.key.DimensionKey]
			if group == nil {
				group = &domain.DailyRollup{}
				byGroup[row.key.DimensionKey] = group
				groupOrder = append(groupOrder, row.key.DimensionKey)
			}
			_ = group.Add(contribution)
		}
	}

	sort.Strings(bucketOrder)
	buckets := make([]summaryBucket, 0, len(bucketOrder))
	for _, label := range bucketOrder {
		span := bucketSpan[label]
		buckets = append(buckets, summaryBucket{
			Period:         label,
			Start:          time.UnixMicro(span[0]).UTC(),
			End:            time.UnixMicro(span[1]).UTC(),
			summaryMetrics: renderMetrics(*byBucket[label], requestLevel),
		})
	}

	groups, truncated, otherCount := renderGroups(byGroup, groupOrder, limit, requestLevel)
	return buckets, renderMetrics(total, requestLevel), groups, truncated, otherCount
}

// renderGroups keeps the largest groups by cost and folds the rest into one
// row, on the server. Handing the console a truncated list instead would let
// the rows it shows add up to less than the total it shows beside them.
func renderGroups(
	byGroup map[string]*domain.DailyRollup, order []string, limit int, requestLevel bool,
) ([]summaryGroup, bool, int) {
	sort.Slice(order, func(i, j int) bool {
		left, right := byGroup[order[i]], byGroup[order[j]]
		if left.CostMicrosUSD != right.CostMicrosUSD {
			return left.CostMicrosUSD > right.CostMicrosUSD
		}
		if left.Attempts != right.Attempts {
			return left.Attempts > right.Attempts
		}
		return order[i] < order[j]
	})
	groups := make([]summaryGroup, 0, len(order))
	for index, key := range order {
		if index < limit {
			groups = append(groups, summaryGroup{Key: key, summaryMetrics: renderMetrics(*byGroup[key], requestLevel)})
			continue
		}
		break
	}
	if len(order) <= limit {
		return groups, false, 0
	}
	other := domain.DailyRollup{}
	for _, key := range order[limit:] {
		_ = other.Add(*byGroup[key])
	}
	// The store may already hold a folded row under the same name. Appending a
	// second one would put one key twice in a list whose whole promise is that
	// its rows add up to the total exactly once.
	for index, group := range groups {
		if group.Key != domain.RollupOtherKey {
			continue
		}
		merged := *byGroup[domain.RollupOtherKey]
		_ = merged.Add(other)
		groups[index] = summaryGroup{Key: domain.RollupOtherKey, summaryMetrics: renderMetrics(merged, requestLevel)}
		return groups, true, len(order) - limit
	}
	groups = append(groups, summaryGroup{
		Key: domain.RollupOtherKey, summaryMetrics: renderMetrics(other, requestLevel),
	})
	return groups, true, len(order) - limit
}

func renderMetrics(row domain.DailyRollup, requestLevel bool) summaryMetrics {
	attemptP95, attemptOverMax := usage.ApproximateLatencyPercentile(
		row.AttemptLatencyBuckets, row.AttemptLatencyOverflow, 95)
	metrics := summaryMetrics{
		Attempts: row.Attempts, Errors: row.Errors,
		InputTokens: row.InputTokens, OutputTokens: row.OutputTokens,
		EstimatedInputTokens: row.EstimatedInputTokens, EstimatedOutputTokens: row.EstimatedOutputTokens,
		CachedInputTokens:     row.ProviderCachedInputTokens,
		CacheWriteInputTokens: row.ProviderCacheWriteInputTokens,
		ReasoningTokens:       row.ProviderReasoningTokens,
		CostMicrosUSD:         row.CostMicrosUSD, EstimatedCostMicrosUSD: row.EstimatedCostMicrosUSD,
		UnknownAttempts: row.UnknownAttempts, LatencyMillis: row.LatencyMillis,
		AttemptLatencySamples: row.AttemptLatencySamples, AttemptLatencyP95Millis: attemptP95,
		LatencyApproximate: true,
	}
	if attemptOverMax {
		metrics.AttemptLatencyOverMax = row.AttemptLatencyOverflow
	}
	if !requestLevel {
		return metrics
	}
	requests, errors := row.Requests, row.RequestErrors
	samples := row.RequestLatencySamples
	requestP95, requestOverMax := usage.ApproximateLatencyPercentile(
		row.RequestLatencyBuckets, row.RequestLatencyOverflow, 95)
	metrics.Requests, metrics.RequestErrors, metrics.RequestLatencySamples = &requests, &errors, &samples
	metrics.RequestLatencyP95Millis = &requestP95
	if requestOverMax {
		metrics.RequestLatencyOverMax = row.RequestLatencyOverflow
	}
	return metrics
}

func bucketLabel(periodID, granularity string) string {
	switch granularity {
	case "month":
		if len(periodID) >= 7 {
			return periodID[:7]
		}
	case "year":
		if len(periodID) >= 4 {
			return periodID[:4]
		}
	}
	return periodID
}

// summaryRange resolves the requested window into two accounting date labels.
//
// The labels are inclusive on both ends, unlike the attempt detail endpoint's
// half-open instants: a date label names a whole day, and asking for August and
// getting the 31st dropped is the kind of quiet arithmetic an operator only
// notices at month end.
func summaryRange(rawStart, rawEnd, granularity, today string) (string, string, error) {
	end := today
	if rawEnd != "" {
		end = rawEnd
	}
	endDate, err := time.Parse(summaryDateLayout, end)
	if err != nil {
		return "", "", fmt.Errorf("end must be an accounting date (YYYY-MM-DD)")
	}
	start := rawStart
	if start == "" {
		switch granularity {
		case "month":
			start = endDate.AddDate(0, -11, 0).Format(summaryDateLayout)
		case "year":
			start = endDate.AddDate(-2, 0, 0).Format(summaryDateLayout)
		default:
			start = endDate.AddDate(0, 0, -29).Format(summaryDateLayout)
		}
	}
	startDate, err := time.Parse(summaryDateLayout, start)
	if err != nil {
		return "", "", fmt.Errorf("start must be an accounting date (YYYY-MM-DD)")
	}
	if startDate.After(endDate) {
		return "", "", fmt.Errorf("start is after end")
	}
	months := (endDate.Year()-startDate.Year())*12 + int(endDate.Month()) - int(startDate.Month()) + 1
	if months > summaryMaxMonths {
		return "", "", fmt.Errorf("range covers %d months; the maximum is %d", months, summaryMaxMonths)
	}
	return startDate.Format(summaryDateLayout), endDate.Format(summaryDateLayout), nil
}

// summaryResourceLabels names the projects and deployments a grouped response
// refers to. History outlives the resource it names, so a missing entry means
// "deleted", and the console falls back to the identifier the ledger carries.
func (r *Runtime) summaryResourceLabels(request *http.Request) (map[string]string, error) {
	labels := map[string]string{}
	projects, err := r.store.ListProjects(request.Context())
	if err != nil {
		return nil, err
	}
	for _, project := range projects {
		labels[project.ID] = project.Name
	}
	deployments, err := r.store.ListDeployments(request.Context())
	if err != nil {
		return nil, err
	}
	for _, deployment := range deployments {
		if strings.TrimSpace(deployment.Name) != "" {
			labels[deployment.ID] = deployment.Name
		}
	}
	return labels, nil
}
