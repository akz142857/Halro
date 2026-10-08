package usage

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

var diskDashboardDimensions = [...]string{"project", "provider", "offering", "requested_model", "provider_model"}
var diskDashboardMetrics = [...]string{"calls", "cost", "errors"}

type diskDashboardPeriod struct {
	Period    Period
	Totals    Bucket
	Anomalies []uint64
	Populated [5]bool
}
type diskDashboardGroupScope struct {
	Period    Period
	Dimension string
}
type diskDashboardGroup struct {
	Scope diskDashboardGroupScope
	Item  Breakdown
}
type diskDashboardTop struct {
	Scope  diskDashboardGroupScope
	Metric string
	// Fixed references avoid multiplying arbitrarily wide group strings in caches.
	Refs [][32]byte
}

func diskDashboardKey(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return diskUsageID(string(raw)), nil
}
func diskDashboardHourKey(hour int64) []byte {
	return diskUsageSequence(uint64(hour) ^ (uint64(1) << 63))
}
func diskDashboardGroupKey(scope diskDashboardGroupScope, key string) ([]byte, error) {
	return diskDashboardKey(struct {
		Scope diskDashboardGroupScope
		Key   string
	}{scope, key})
}
func diskDashboardTopKey(scope diskDashboardGroupScope, metric string) ([]byte, error) {
	return diskDashboardKey(struct {
		Scope  diskDashboardGroupScope
		Metric string
	}{scope, metric})
}
func diskDashboardMetric(b Breakdown, metric string) int64 {
	switch metric {
	case "cost":
		return b.CostMicrosUSD
	case "errors":
		return b.Errors
	default:
		return b.Calls
	}
}
func diskDashboardLess(a, b Breakdown, metric string) bool {
	x, y := diskDashboardMetric(a, metric), diskDashboardMetric(b, metric)
	if x != y {
		return x > y
	}
	return a.Key < b.Key
}
func diskDashboardValidGroup(g diskDashboardGroup) bool {
	b := g.Item
	return b.Key != "" && b.Calls > 0 && b.InputTokens >= 0 && b.OutputTokens >= 0 && b.CostMicrosUSD >= 0 && b.EstimatedCostMicros >= 0 && b.EstimatedCostMicros <= b.CostMicrosUSD && b.UnknownAttempts >= 0 && b.UnknownAttempts <= b.Calls && b.Errors >= 0 && b.Errors <= b.Calls
}
func diskDashboardAddGroup(b *Breakdown, a AttemptEvent) error {
	deltas := []struct {
		p *int64
		n int64
	}{{&b.Calls, 1}, {&b.InputTokens, a.ProviderInputTokens}, {&b.OutputTokens, a.ProviderOutputTokens}}
	if cost, ok := a.KnownCostMicrosUSD(); ok {
		deltas = append(deltas, struct {
			p *int64
			n int64
		}{&b.CostMicrosUSD, cost})
		if a.CostEstimated {
			deltas = append(deltas, struct {
				p *int64
				n int64
			}{&b.EstimatedCostMicros, cost})
		}
	} else {
		deltas = append(deltas, struct {
			p *int64
			n int64
		}{&b.UnknownAttempts, 1})
	}
	if a.Status != "success" {
		deltas = append(deltas, struct {
			p *int64
			n int64
		}{&b.Errors, 1})
	}
	for _, d := range deltas {
		if d.n < 0 {
			return ErrDiskUsageCorrupt
		}
		if err := addInt64(d.p, d.n); err != nil {
			return err
		}
	}
	return nil
}
func diskDashboardAddTotals(b *Bucket, a AttemptEvent) error {
	var g Breakdown
	if err := diskDashboardAddGroup(&g, a); err != nil {
		return err
	}
	deltas := []struct {
		p *int64
		n int64
	}{{&b.Attempts, g.Calls}, {&b.InputTokens, g.InputTokens}, {&b.OutputTokens, g.OutputTokens}, {&b.CostMicrosUSD, g.CostMicrosUSD}, {&b.EstimatedCostMicrosUSD, g.EstimatedCostMicros}, {&b.UnknownAttempts, g.UnknownAttempts}, {&b.Errors, g.Errors}, {&b.LatencyMillis, a.LatencyMillis}}
	if a.TokensEstimated {
		deltas = append(deltas, struct {
			p *int64
			n int64
		}{&b.EstimatedInputTokens, a.ProviderInputTokens}, struct {
			p *int64
			n int64
		}{&b.EstimatedOutputTokens, a.ProviderOutputTokens})
	}
	for _, d := range deltas {
		if d.n < 0 {
			return ErrDiskUsageCorrupt
		}
		if err := addInt64(d.p, d.n); err != nil {
			return err
		}
	}
	return nil
}
func diskDashboardAnomalous(a AttemptEvent) bool {
	return a.Status != "success" || a.RetryCount > 0 || a.FallbackCount > 0
}

// All measures used for ranking increase monotonically. With one changed group,
// the old top five plus that candidate contains the exact new top five. This
// does not fold groups into rollup's "other" or scan historical groups.
func diskUsageAddDashboardAttempt(input any, a AttemptEvent) error {
	tx := usageTransaction(input)
	period := Period{ID: a.PeriodID, TimezoneVersion: a.PeriodTimezoneVersion}
	pk, err := diskDashboardKey(period)
	if err != nil {
		return err
	}
	p, found, err := diskUsageGet[diskDashboardPeriod](tx, "dashboard_period", pk)
	if err != nil {
		return err
	}
	if found && (p.Period != period || !diskDashboardValidPeriod(p)) {
		return ErrDiskUsageCorrupt
	}
	p.Period = period
	hadPeriod, populatedBefore := found, p.Populated
	keys := [...]string{a.ProjectID, a.ProviderID, string(a.OfferingID), a.RequestedModel, a.ProviderModel}
	for i, key := range keys {
		if key != "" {
			p.Populated[i] = true
		}
	}
	if err := diskDashboardAddTotals(&p.Totals, a); err != nil {
		return err
	}
	if diskDashboardAnomalous(a) {
		if len(p.Anomalies) > 0 && a.Sequence <= p.Anomalies[0] {
			return ErrDiskUsageCorrupt
		}
		p.Anomalies = append([]uint64{a.Sequence}, p.Anomalies...)
		if len(p.Anomalies) > 5 {
			p.Anomalies = p.Anomalies[:5]
		}
	}
	if err := diskUsagePut(tx, "dashboard_period", pk, p); err != nil {
		return err
	}
	for i, dimension := range diskDashboardDimensions {
		if keys[i] == "" {
			continue
		}
		scope := diskDashboardGroupScope{period, dimension}
		key, err := diskDashboardGroupKey(scope, keys[i])
		if err != nil {
			return err
		}
		candidate, found, err := diskUsageGet[diskDashboardGroup](tx, "dashboard_group", key)
		if err != nil {
			return err
		}
		if found && (candidate.Scope != scope || candidate.Item.Key != keys[i] || !diskDashboardValidGroup(candidate)) {
			return ErrDiskUsageCorrupt
		}
		if found && (!hadPeriod || !populatedBefore[i]) {
			return ErrDiskUsageCorrupt
		}
		candidate.Scope = scope
		candidate.Item.Key = keys[i]
		if err := diskDashboardAddGroup(&candidate.Item, a); err != nil {
			return err
		}
		// Read old cache companions before updating candidate, so every old cache
		// order can be checked against the previous committed counters.
		for _, metric := range diskDashboardMetrics {
			tk, err := diskDashboardTopKey(scope, metric)
			if err != nil {
				return err
			}
			top, hasTop, err := diskUsageGet[diskDashboardTop](tx, "dashboard_top", tk)
			if err != nil {
				return err
			}
			if hasTop && (top.Scope != scope || top.Metric != metric || len(top.Refs) < 1 || len(top.Refs) > 5) {
				return ErrDiskUsageCorrupt
			}
			if hasTop && (!hadPeriod || !populatedBefore[i]) {
				return ErrDiskUsageCorrupt
			}
			if populatedBefore[i] && !hasTop {
				return ErrDiskUsageCorrupt
			}
			items := make([]diskDashboardGroup, 0, 6)
			seen := make(map[[32]byte]bool, 5)
			for _, ref := range top.Refs {
				if seen[ref] {
					return ErrDiskUsageCorrupt
				}
				seen[ref] = true
				g, ok, err := diskUsageGet[diskDashboardGroup](tx, "dashboard_group", ref[:])
				if err != nil {
					return err
				}
				gk, _ := diskDashboardGroupKey(scope, g.Item.Key)
				if !ok || g.Scope != scope || !diskDashboardValidGroup(g) || string(gk) != string(ref[:]) {
					return ErrDiskUsageCorrupt
				}
				if len(items) > 0 && !diskDashboardLess(items[len(items)-1].Item, g.Item, metric) {
					return ErrDiskUsageCorrupt
				}
				items = append(items, g)
			}
			changed := false
			for j := range items {
				if items[j].Item.Key == keys[i] {
					items[j] = candidate
					changed = true
				}
			}
			if !changed {
				items = append(items, candidate)
			}
			sort.Slice(items, func(i, j int) bool { return diskDashboardLess(items[i].Item, items[j].Item, metric) })
			if len(items) > 5 {
				items = items[:5]
			}
			top = diskDashboardTop{Scope: scope, Metric: metric, Refs: make([][32]byte, 0, 5)}
			for _, g := range items {
				k, err := diskDashboardGroupKey(scope, g.Item.Key)
				if err != nil {
					return err
				}
				var ref [32]byte
				copy(ref[:], k)
				top.Refs = append(top.Refs, ref)
			}
			if err := diskUsagePut(tx, "dashboard_top", tk, top); err != nil {
				return err
			}
		}
		if err := diskUsagePut(tx, "dashboard_group", key, candidate); err != nil {
			return err
		}
	}
	return nil
}
func diskDashboardValidPeriod(p diskDashboardPeriod) bool {
	b := p.Totals
	if !p.Populated[0] || b.Attempts <= 0 || b.InputTokens < 0 || b.OutputTokens < 0 || b.EstimatedInputTokens < 0 || b.EstimatedInputTokens > b.InputTokens || b.EstimatedOutputTokens < 0 || b.EstimatedOutputTokens > b.OutputTokens || b.CostMicrosUSD < 0 || b.EstimatedCostMicrosUSD < 0 || b.EstimatedCostMicrosUSD > b.CostMicrosUSD || b.UnknownAttempts < 0 || b.UnknownAttempts > b.Attempts || b.Errors < 0 || b.Errors > b.Attempts || b.LatencyMillis < 0 || len(p.Anomalies) > 5 {
		return false
	}
	for i, s := range p.Anomalies {
		if s == 0 || (i > 0 && s >= p.Anomalies[i-1]) {
			return false
		}
	}
	return b.Hour.IsZero() && requestQualityOnlyInternal(b) == (Bucket{})
}
func requestQualityOnlyInternal(b Bucket) Bucket {
	return Bucket{Requests: b.Requests, RequestErrors: b.RequestErrors, RequestLatencySamples: b.RequestLatencySamples, RequestLatencyP50Millis: b.RequestLatencyP50Millis, RequestLatencyP95Millis: b.RequestLatencyP95Millis}
}

// Limits cover the entire Dashboard, including all hours and repeated top-five
// companions. This is not the 256-row budget of an individual detail page.
type DiskDashboardLimits struct{ Hours, ScanRows, InputBytes, OutputBytes int }
type DiskDashboard struct {
	Dashboard                        Dashboard
	Metrics                          Metrics
	Head                             ledger.Watermark
	Scanned, InputBytes, OutputBytes int
}

func diskDashboardGet[T any](input any, bucket string, key []byte, budget *diskFailureBudget) (T, bool, error) {
	tx := usageTransaction(input)
	var result T
	b := tx.Bucket([]byte(bucket))
	if b == nil {
		return result, false, ErrDiskUsageCorrupt
	}
	raw := b.Get(key)
	if err := budget.take(raw, len(key)); err != nil {
		return result, false, err
	}
	if raw == nil {
		return result, false, nil
	}
	if json.Unmarshal(raw, &result) != nil {
		return result, false, ErrDiskUsageCorrupt
	}
	return result, true, nil
}
func diskDashboardQuality(ctx context.Context, input any, scope diskQualityScope, budget *diskFailureBudget) (Bucket, error) {
	tx := usageTransaction(input)
	key, err := diskQualityKey(scope)
	if err != nil {
		return Bucket{}, err
	}
	q, found, err := diskDashboardGet[diskRequestQuality](tx, "request_quality", key, budget)
	if err != nil {
		return Bucket{}, err
	}
	if found && (q.Scope != scope || !diskQualityValid(q)) {
		return Bucket{}, ErrDiskUsageCorrupt
	}
	b := Bucket{Requests: q.Requests, RequestErrors: q.Errors, RequestLatencySamples: q.Samples}
	b.RequestLatencyP50Millis, err = diskRankPercentile(ctx, tx, key, q.Samples, 50, budget)
	if err != nil {
		return Bucket{}, err
	}
	b.RequestLatencyP95Millis, err = diskRankPercentile(ctx, tx, key, q.Samples, 95, budget)
	return b, err
}
func diskDashboardSetQuality(b *Bucket, q Bucket) {
	b.Requests = q.Requests
	b.RequestErrors = q.RequestErrors
	b.RequestLatencySamples = q.RequestLatencySamples
	b.RequestLatencyP50Millis = q.RequestLatencyP50Millis
	b.RequestLatencyP95Millis = q.RequestLatencyP95Millis
}

// ReadDashboard holds one admitted read lock across short Views and encoding,
// so mutable counters, top references, hours, and current watermark share a cut.
// No View survives encoding or return. Apply waits for the bounded call to exit.
// History older than seven days is skipped by an ordered index. Legacy future
// hours are retained: a result exceeding Hours returns a limit, never truncation.
func (p *DiskProjection) ReadDashboard(ctx context.Context, now time.Time, period Period, limits DiskDashboardLimits) (DiskDashboard, error) {
	if now.IsZero() || period.ID == "" || len(period.ID) > 1<<20 || limits.Hours < 1 || limits.Hours > 256 || limits.ScanRows < 1 || limits.ScanRows > 65536 || limits.InputBytes < 1 || limits.InputBytes > 8<<20 || limits.OutputBytes < 1 || limits.OutputBytes > 8<<20 {
		return DiskDashboard{}, ErrDiskUsageLimit
	}
	var result DiskDashboard
	err := p.readRows(ctx, func(db *usageEngine) error {
		raw, err := json.Marshal(period)
		if err != nil {
			return err
		}
		if len(raw) > 1<<20 {
			return ErrDiskUsageLimit
		}
		budget := diskFailureBudget{limits: DiskUsagePageLimits{ScanRows: limits.ScanRows, InputBytes: limits.InputBytes, OutputBytes: limits.OutputBytes}}
		usedOutput := 0
		charge := func(value any) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			raw, err := json.Marshal(value)
			if err != nil {
				return err
			}
			if len(raw) > limits.OutputBytes-usedOutput {
				return ErrDiskUsageLimit
			}
			usedOutput += len(raw)
			return nil
		}
		view := func(fn func(*usageTx) error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return wrapUsageDatabase(db).View(fn)
		}
		result.Dashboard = Dashboard{Hourly: make([]Bucket, 0, min(limits.Hours, 169)), RecentAnomalies: make([]Anomaly, 0, 5), Breakdowns: make(map[string]map[string][]Breakdown, 5)}
		var facts diskDashboardPeriod
		pk, err := diskDashboardKey(period)
		if err != nil {
			return err
		}
		if err := view(func(tx *usageTx) error {
			state, found, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
			if err != nil || !found || state.Version != diskUsageVersion {
				return errors.Join(err, ErrDiskUsageCorrupt)
			}
			result.Head, result.Metrics = state.Watermark, state.Metrics
			result.Dashboard.Watermark = state.Watermark.Sequence
			result.Dashboard.Active = state.Metrics.ActiveRequests
			facts, found, err = diskDashboardGet[diskDashboardPeriod](tx, "dashboard_period", pk, &budget)
			if err != nil {
				return err
			}
			if found && (facts.Period != period || !diskDashboardValidPeriod(facts)) {
				return ErrDiskUsageCorrupt
			}
			result.Dashboard.Today = facts.Totals
			quality, err := diskDashboardQuality(ctx, tx, diskQualityScope{Kind: "period", PeriodID: period.ID, TimezoneVersion: period.TimezoneVersion}, &budget)
			if err != nil {
				return err
			}
			diskDashboardSetQuality(&result.Dashboard.Today, quality)
			return nil
		}); err != nil {
			return err
		}
		if err := charge(result.Dashboard.Today); err != nil {
			return err
		}
		for dimensionIndex, dimension := range diskDashboardDimensions {
			result.Dashboard.Breakdowns[dimension] = make(map[string][]Breakdown, 3)
			for _, metric := range diskDashboardMetrics {
				scope := diskDashboardGroupScope{period, dimension}
				tk, err := diskDashboardTopKey(scope, metric)
				if err != nil {
					return err
				}
				items := make([]Breakdown, 0, 5)
				if err := view(func(tx *usageTx) error {
					top, found, err := diskDashboardGet[diskDashboardTop](tx, "dashboard_top", tk, &budget)
					if err != nil {
						return err
					}
					if !found {
						if facts.Populated[dimensionIndex] {
							return ErrDiskUsageCorrupt
						}
						return nil
					}
					if !facts.Populated[dimensionIndex] {
						return ErrDiskUsageCorrupt
					}
					if top.Scope != scope || top.Metric != metric || len(top.Refs) < 1 || len(top.Refs) > 5 {
						return ErrDiskUsageCorrupt
					}
					seen := make(map[[32]byte]bool, 5)
					for _, ref := range top.Refs {
						if seen[ref] {
							return ErrDiskUsageCorrupt
						}
						seen[ref] = true
						g, found, err := diskDashboardGet[diskDashboardGroup](tx, "dashboard_group", ref[:], &budget)
						if err != nil {
							return err
						}
						key, _ := diskDashboardGroupKey(scope, g.Item.Key)
						if !found || g.Scope != scope || !diskDashboardValidGroup(g) || string(key) != string(ref[:]) || (len(items) > 0 && !diskDashboardLess(items[len(items)-1], g.Item, metric)) {
							return ErrDiskUsageCorrupt
						}
						items = append(items, g.Item)
					}
					return nil
				}); err != nil {
					return err
				}
				if err := charge(items); err != nil {
					return err
				}
				result.Dashboard.Breakdowns[dimension][metric] = items
			}
		}
		for _, seq := range facts.Anomalies {
			var a AttemptEvent
			if err := view(func(tx *usageTx) error {
				var found bool
				var err error
				a, found, err = diskDashboardGet[AttemptEvent](tx, "attempts", diskUsageSequence(seq), &budget)
				if err != nil {
					return err
				}
				if !found || a.Sequence != seq || seq > result.Head.Sequence || !period.Includes(a.PeriodID, a.PeriodTimezoneVersion) || !diskDashboardAnomalous(a) {
					return ErrDiskUsageCorrupt
				}
				return nil
			}); err != nil {
				return err
			}
			anomaly := Anomaly{RequestID: a.RequestID, AttemptID: a.AttemptID, CompletedAt: a.CompletedAt, ProjectID: a.ProjectID, DeploymentID: a.DeploymentID, ProviderID: a.ProviderID, RequestedModel: a.RequestedModel, ProviderModel: a.ProviderModel, Status: a.Status, ErrorClass: a.ErrorClass, HTTPStatus: a.HTTPStatus, RetryCount: a.RetryCount, FallbackCount: a.FallbackCount}
			if err := charge(anomaly); err != nil {
				return err
			}
			result.Dashboard.RecentAnomalies = append(result.Dashboard.RecentAnomalies, anomaly)
		}
		since := now.Add(-7 * 24 * time.Hour).UTC().Truncate(time.Hour).Unix()
		var after []byte
		for {
			var bucket Bucket
			var next []byte
			found := false
			if err := view(func(tx *usageTx) error {
				index := tx.Bucket([]byte("hourly_order"))
				if index == nil {
					return ErrDiskUsageCorrupt
				}
				cursor := index.Cursor()
				seek := diskDashboardHourKey(since)
				if after != nil {
					seek = after
				}
				k, v := cursor.Seek(seek)
				if after != nil && string(k) == string(after) {
					k, v = cursor.Next()
				}
				if err := budget.take(v, len(k)); err != nil {
					return err
				}
				if k == nil {
					return nil
				}
				if len(k) != 8 || len(v) != 8 {
					return ErrDiskUsageCorrupt
				}
				hour := int64(binary.BigEndian.Uint64(k) ^ (uint64(1) << 63))
				if hour < since || binary.BigEndian.Uint64(v) != uint64(hour) {
					return ErrDiskUsageCorrupt
				}
				if len(result.Dashboard.Hourly) == limits.Hours {
					return ErrDiskUsageLimit
				}
				var err error
				bucket, found, err = diskDashboardGet[Bucket](tx, "hourly", v, &budget)
				if err != nil {
					return err
				}
				if !found || bucket.Hour.Unix() != hour || !bucket.Hour.Equal(bucket.Hour.UTC().Truncate(time.Hour)) {
					return ErrDiskUsageCorrupt
				}
				quality, err := diskDashboardQuality(ctx, tx, diskQualityScope{Kind: "hour", Hour: hour}, &budget)
				if err != nil {
					return err
				}
				diskDashboardSetQuality(&bucket, quality)
				next = append([]byte(nil), k...)
				return nil
			}); err != nil {
				return err
			}
			if !found {
				break
			}
			if err := charge(bucket); err != nil {
				return err
			}
			result.Dashboard.Hourly = append(result.Dashboard.Hourly, bucket)
			after = next
		}
		encoded, err := json.Marshal(result.Dashboard)
		if err != nil {
			return err
		}
		if len(encoded) > limits.OutputBytes {
			return ErrDiskUsageLimit
		}
		result.Scanned = budget.rows
		result.InputBytes = budget.bytes
		result.OutputBytes = len(encoded)
		return nil
	})
	if err != nil {
		if errors.Is(err, errDiskFailureBudget) {
			err = ErrDiskUsageLimit
		}
		return DiskDashboard{}, err
	}
	return result, nil
}
