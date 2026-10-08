package usage

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

// time.Time.Sub saturates at MaxInt64 duration nanoseconds. Its nonnegative
// millisecond result is <2^44, including positive durations rounded down to zero.
// A rank query never builds a latency array or substitutes histogram bounds.
const diskLatencyBits = 44
const diskMaxRequestLatencyMillis = uint64(time.Duration(math.MaxInt64) / time.Millisecond)

type diskQualityScope struct {
	Kind            string
	PeriodID        string
	TimezoneVersion uint64
	Hour            int64
}

type diskRequestQuality struct {
	Scope                     diskQualityScope
	Requests, Errors, Samples int64
}

func diskQualityKey(scope diskQualityScope) ([]byte, error) {
	raw, err := json.Marshal(scope)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	return sum[:], nil
}

func diskRankKey(scope []byte, depth int, prefix uint64) []byte {
	key := make([]byte, 41)
	copy(key, scope)
	key[32] = byte(depth)
	binary.BigEndian.PutUint64(key[33:], prefix)
	return key
}

func diskRankCount(input any, key []byte, budget *diskFailureBudget) (uint64, error) {
	b := usageBucketOf(input)
	if b == nil {
		return 0, ErrDiskUsageCorrupt
	}
	if budget == nil && b.tx.rankScratch != nil {
		return b.tx.rankScratch.get(b, key)
	}
	raw := b.Get(key)
	if raw != nil && len(raw) != 8 {
		return 0, ErrDiskUsageCorrupt
	}
	if budget != nil {
		if err := budget.take(raw, len(key)); err != nil {
			return 0, err
		}
	}
	if raw == nil {
		return 0, nil
	}
	count := binary.BigEndian.Uint64(raw)
	if count == 0 || count > math.MaxInt64 {
		return 0, ErrDiskUsageCorrupt
	}
	return count, nil
}

func diskRankIncrement(input any, scope []byte, latency uint64, expected int64) error {
	tx := usageTransaction(input)
	if latency > diskMaxRequestLatencyMillis || expected < 0 || expected == math.MaxInt64 {
		return ErrDiskUsageCorrupt
	}
	b := tx.Bucket([]byte("request_latency_rank"))
	parent, err := diskRankCount(b, diskRankKey(scope, 0, 0), nil)
	if err != nil {
		return err
	}
	if parent != uint64(expected) {
		return ErrDiskUsageCorrupt
	}
	prefix := uint64(0)
	for depth := 0; depth < diskLatencyBits; depth++ {
		bit := uint64(1) << (diskLatencyBits - depth - 1)
		left, err := diskRankCount(b, diskRankKey(scope, depth+1, prefix), nil)
		if err != nil {
			return err
		}
		right, err := diskRankCount(b, diskRankKey(scope, depth+1, prefix|bit), nil)
		if err != nil {
			return err
		}
		if left > parent || right != parent-left {
			return ErrDiskUsageCorrupt
		}
		if err := diskRankPut(b, diskRankKey(scope, depth, prefix), parent+1); err != nil {
			return err
		}
		parent = left
		if latency&bit != 0 {
			prefix |= bit
			parent = right
		}
	}
	return diskRankPut(b, diskRankKey(scope, diskLatencyBits, prefix), parent+1)
}

func diskUsageAddRequestQuality(input any, s RequestSummary) error {
	tx := usageTransaction(input)
	if s.CompletedAt.IsZero() {
		return nil
	} // Preserve legacy Dashboard exclusion.
	latency, known := requestLatencyMillis(s)
	for _, scope := range []diskQualityScope{{Kind: "period", PeriodID: s.PeriodID, TimezoneVersion: s.PeriodTimezoneVersion}, {Kind: "hour", Hour: s.CompletedAt.UTC().Truncate(time.Hour).Unix()}} {
		key, err := diskQualityKey(scope)
		if err != nil {
			return err
		}
		q, found, err := diskUsageGet[diskRequestQuality](tx, "request_quality", key)
		if err != nil {
			return err
		}
		if found && (q.Scope != scope || !diskQualityValid(q)) {
			return ErrDiskUsageCorrupt
		}
		q.Scope = scope
		if err := addInt64(&q.Requests, 1); err != nil {
			return err
		}
		if s.Outcome != "success" {
			if err := addInt64(&q.Errors, 1); err != nil {
				return err
			}
		}
		if known {
			if err := diskRankIncrement(tx, key, uint64(latency), q.Samples); err != nil {
				return err
			}
			if err := addInt64(&q.Samples, 1); err != nil {
				return err
			}
		}
		if err := diskUsagePut(tx, "request_quality", key, q); err != nil {
			return err
		}
	}
	return nil
}

func diskQualityValid(q diskRequestQuality) bool {
	return q.Requests > 0 && q.Errors >= 0 && q.Errors <= q.Requests && q.Samples >= 0 && q.Samples <= q.Requests
}

// Counted selection returns exactly ceil(N*p/100), avoiding N*p overflow.
func diskRankPercentile(ctx context.Context, input any, scope []byte, samples int64, percent int, budget *diskFailureBudget) (int64, error) {
	tx := usageTransaction(input)
	if samples < 0 || percent < 1 || percent > 100 {
		return 0, ErrDiskUsageCorrupt
	}
	b := tx.Bucket([]byte("request_latency_rank"))
	parent, err := diskRankCount(b, diskRankKey(scope, 0, 0), budget)
	if err != nil {
		return 0, err
	}
	if parent != uint64(samples) {
		return 0, ErrDiskUsageCorrupt
	}
	if samples == 0 {
		return 0, nil
	}
	n := uint64(samples)
	p := uint64(percent)
	rank := (n/100)*p + ((n%100)*p+99)/100
	prefix := uint64(0)
	for depth := 0; depth < diskLatencyBits; depth++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		bit := uint64(1) << (diskLatencyBits - depth - 1)
		left, err := diskRankCount(b, diskRankKey(scope, depth+1, prefix), budget)
		if err != nil {
			return 0, err
		}
		right, err := diskRankCount(b, diskRankKey(scope, depth+1, prefix|bit), budget)
		if err != nil {
			return 0, err
		}
		if left > parent || right != parent-left {
			return 0, ErrDiskUsageCorrupt
		}
		parent = left
		if rank > left {
			rank -= left
			prefix |= bit
			parent = right
		}
		if parent == 0 || rank == 0 || rank > parent {
			return 0, ErrDiskUsageCorrupt
		}
	}
	if prefix > diskMaxRequestLatencyMillis {
		return 0, ErrDiskUsageCorrupt
	}
	return int64(prefix), nil
}

// These are current-View observations of Dashboard request quality, distinct
// from immutable detail cursors and the existing approximate daily histograms.
type DiskRequestQuality struct {
	Bucket                           Bucket
	Watermark                        ledger.Watermark
	Scanned, InputBytes, OutputBytes int
}

func (p *DiskProjection) ReadPeriodRequestQuality(ctx context.Context, period Period, limits DiskUsagePageLimits) (DiskRequestQuality, error) {
	if period.ID == "" || len(period.ID) > 1<<20 {
		return DiskRequestQuality{}, ErrDiskUsageLimit
	}
	return p.readRequestQuality(ctx, diskQualityScope{Kind: "period", PeriodID: period.ID, TimezoneVersion: period.TimezoneVersion}, limits)
}

func (p *DiskProjection) ReadHourRequestQuality(ctx context.Context, hour time.Time, limits DiskUsagePageLimits) (DiskRequestQuality, error) {
	if hour.IsZero() {
		return DiskRequestQuality{}, ErrDiskUsageLimit
	}
	return p.readRequestQuality(ctx, diskQualityScope{Kind: "hour", Hour: hour.UTC().Truncate(time.Hour).Unix()}, limits)
}

func (p *DiskProjection) readRequestQuality(ctx context.Context, scope diskQualityScope, limits DiskUsagePageLimits) (DiskRequestQuality, error) {
	if limits.ScanRows < 1 || limits.ScanRows > 256 || limits.InputBytes < 1 || limits.InputBytes > 8<<20 || limits.OutputBytes < 1 || limits.OutputBytes > 8<<20 {
		return DiskRequestQuality{}, ErrDiskUsageLimit
	}
	result := DiskRequestQuality{}
	err := p.readRows(ctx, func(db *usageEngine) error {
		// Encoding happens after reader admission, outside the View. The caller
		// fields have a raw gate; encoded scope has its independent width gate.
		raw, err := json.Marshal(scope)
		if err != nil {
			return err
		}
		if len(raw) > 1<<20 {
			return ErrDiskUsageLimit
		}
		sum := sha256.Sum256(raw)
		key := sum[:]
		budget := diskFailureBudget{limits: limits}
		if err := wrapUsageDatabase(db).View(func(tx *usageTx) error {
			state, found, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
			if err != nil || !found || state.Version != diskUsageVersion {
				return errors.Join(err, ErrDiskUsageCorrupt)
			}
			b := tx.Bucket([]byte("request_quality"))
			if b == nil {
				return ErrDiskUsageCorrupt
			}
			raw := b.Get(key)
			if err := budget.take(raw, len(key)); err != nil {
				return err
			}
			q := diskRequestQuality{Scope: scope}
			if raw != nil {
				if json.Unmarshal(raw, &q) != nil || q.Scope != scope || !diskQualityValid(q) {
					return ErrDiskUsageCorrupt
				}
			}
			result.Bucket.Requests, result.Bucket.RequestErrors, result.Bucket.RequestLatencySamples = q.Requests, q.Errors, q.Samples
			if scope.Kind == "hour" {
				result.Bucket.Hour = time.Unix(scope.Hour, 0).UTC()
			}
			result.Bucket.RequestLatencyP50Millis, err = diskRankPercentile(ctx, tx, key, q.Samples, 50, &budget)
			if err != nil {
				return err
			}
			result.Bucket.RequestLatencyP95Millis, err = diskRankPercentile(ctx, tx, key, q.Samples, 95, &budget)
			if err != nil {
				return err
			}
			result.Watermark = state.Watermark
			return nil
		}); err != nil {
			if errors.Is(err, errDiskFailureBudget) {
				return ErrDiskUsageLimit
			}
			return err
		}
		encoded, err := json.Marshal(result.Bucket)
		if err != nil {
			return err
		}
		if len(encoded) > limits.OutputBytes {
			return ErrDiskUsageLimit
		}
		result.Scanned, result.InputBytes, result.OutputBytes = budget.rows, budget.bytes, len(encoded)
		return nil
	})
	if err != nil {
		return DiskRequestQuality{}, err
	}
	return result, nil
}
