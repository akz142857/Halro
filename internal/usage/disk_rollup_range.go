package usage

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

var diskRollupDimensions = [...]string{domain.RollupDimensionTotal, domain.RollupDimensionProject, domain.RollupDimensionRequestedModel, domain.RollupDimensionProvider, domain.RollupDimensionDeployment, domain.RollupDimensionProviderModel}

const diskRollupRangeKeySize = 51

type diskRollupVersion struct {
	Key      domain.RollupKey
	Row      domain.DailyRollup
	Sequence uint64
	// Only the successor witness changes after publication. Row and Sequence
	// remain immutable; a later successor above a cursor's H is ignored.
	NextSequence uint64
}
type diskRollupScope struct {
	Group         string
	FirstSequence uint64
}

func diskRollupValidRow(row domain.DailyRollup, sequence uint64) bool {
	if row.Version != domain.RollupVersion || row.FirstSequence == 0 || row.FirstSequence > sequence {
		return false
	}
	for _, n := range []int64{row.Requests, row.RequestErrors, row.Attempts, row.Errors, row.InputTokens, row.OutputTokens, row.EstimatedInputTokens, row.EstimatedOutputTokens, row.CostMicrosUSD, row.EstimatedCostMicrosUSD, row.UnknownAttempts, row.LatencyMillis, row.ProviderCachedInputTokens, row.ProviderCacheWriteInputTokens, row.ProviderReasoningTokens, row.RequestLatencySamples, row.RequestLatencyMillis, row.AttemptLatencySamples} {
		if n < 0 {
			return false
		}
	}
	if row.RequestErrors > row.Requests || row.Errors > row.Attempts || row.EstimatedInputTokens > row.InputTokens || row.EstimatedOutputTokens > row.OutputTokens || row.EstimatedCostMicrosUSD > row.CostMicrosUSD || row.UnknownAttempts > row.Attempts || row.RequestLatencySamples > row.Requests || row.AttemptLatencySamples > row.Attempts {
		return false
	}
	countsMatch := func(buckets [domain.RollupLatencyBuckets]uint64, overflow uint64, samples int64) bool {
		if overflow > uint64(samples) {
			return false
		}
		count := overflow
		for _, n := range buckets {
			if n > uint64(samples)-count {
				return false
			}
			count += n
		}
		return count == uint64(samples)
	}
	return countsMatch(row.RequestLatencyBuckets, row.RequestLatencyOverflow, row.RequestLatencySamples) && countsMatch(row.AttemptLatencyBuckets, row.AttemptLatencyOverflow, row.AttemptLatencySamples)
}

func diskRollupDimensionCode(dimension string) byte {
	for i, name := range diskRollupDimensions {
		if name == dimension {
			return byte(i + 1)
		}
	}
	return 0
}
func diskRollupRangeKey(key domain.RollupKey) []byte {
	out := make([]byte, diskRollupRangeKeySize)
	// Fixed label prefix preserves date range seeking without bbolt's 32KiB
	// key ceiling. Complete period and dimension identities remain in versions.
	copy(out[:10], key.PeriodID)
	binary.BigEndian.PutUint64(out[10:18], key.TimezoneVersion)
	out[18] = diskRollupDimensionCode(key.Dimension)
	copy(out[19:], diskUsageID(key.Encode()))
	return out
}
func diskRollupVersionKey(id []byte, sequence uint64) []byte {
	out := make([]byte, 40)
	copy(out, id)
	binary.BigEndian.PutUint64(out[32:], sequence)
	return out
}
func diskRollupBounds(raw []byte) (first, last uint64, err error) {
	if len(raw) != 16 {
		return 0, 0, ErrDiskUsageCorrupt
	}
	first, last = binary.BigEndian.Uint64(raw[:8]), binary.BigEndian.Uint64(raw[8:])
	if first == 0 || last < first {
		return 0, 0, ErrDiskUsageCorrupt
	}
	return first, last, nil
}

// Every cumulative value has an applied-sequence version. The range witness,
// previous successor, new version, current row and semantic head share Apply's
// transaction. Missing history is never initialized over an existing row.
func diskUsagePutRollupVersion(input any, key domain.RollupKey, previous domain.DailyRollup, hadPrevious bool, row domain.DailyRollup, sequence uint64) error {
	tx := usageTransaction(input)
	if sequence == 0 || key.Validate() != nil || !diskRollupValidRow(row, sequence) {
		return ErrDiskUsageCorrupt
	}
	index, versions := tx.Bucket([]byte("rollup_range")), tx.Bucket([]byte("rollup_versions"))
	if index == nil || versions == nil {
		return ErrDiskUsageCorrupt
	}
	rk := diskRollupRangeKey(key)
	raw := index.Get(rk)
	if (raw != nil) != hadPrevious {
		return ErrDiskUsageCorrupt
	}
	first := sequence
	if raw != nil {
		var last uint64
		var err error
		first, last, err = diskRollupBounds(raw)
		if err != nil {
			return err
		}
		if sequence <= last {
			return ErrDiskUsageCorrupt
		}
		vk := diskRollupVersionKey(rk[19:], last)
		old, found, err := diskUsageGet[diskRollupVersion](tx, "rollup_versions", vk)
		if err != nil {
			return err
		}
		if !found || old.Key != key || old.Row != previous || old.Sequence != last || old.NextSequence != 0 {
			return ErrDiskUsageCorrupt
		}
		old.NextSequence = sequence
		if err := diskUsagePut(tx, "rollup_versions", vk, old); err != nil {
			return err
		}
	}
	vk := diskRollupVersionKey(rk[19:], sequence)
	if versions.Get(vk) != nil {
		return ErrDiskUsageCorrupt
	}
	if err := diskUsagePut(tx, "rollup_versions", vk, diskRollupVersion{Key: key, Row: row, Sequence: sequence}); err != nil {
		return err
	}
	bounds := make([]byte, 16)
	binary.BigEndian.PutUint64(bounds, first)
	binary.BigEndian.PutUint64(bounds[8:], sequence)
	return index.Put(rk, bounds)
}

// Dimensions contains the report dimension and optionally one grouped dimension.
// One filter selects a value of its sole non-total dimension; marginal rollups
// cannot answer combined filters or group/filter cross terms.
type DiskRollupQuery struct {
	Start, End  string
	Dimensions  []string
	FilterValue string
	Limit       int
}
type DiskRollupCursor struct {
	generation [16]byte
	query      [32]byte
	head       ledger.Watermark
	after      [diskRollupRangeKeySize]byte
	positioned bool
}
type DiskRollupItem struct {
	Key      domain.RollupKey
	Row      domain.DailyRollup
	Sequence uint64
}
type DiskRollupPage struct {
	Items                            []DiskRollupItem
	Next                             DiskRollupCursor
	Head                             ledger.Watermark
	Done                             bool
	Scanned, InputBytes, OutputBytes int
}

func diskRollupValidateQuery(q DiskRollupQuery, limits DiskUsagePageLimits) error {
	if q.Limit < 1 || q.Limit > 100 || len(q.Dimensions) < 1 || len(q.Dimensions) > 2 || len(q.FilterValue) > 1<<20 || limits.ScanRows < 1 || limits.ScanRows > 256 || limits.InputBytes < 1 || limits.InputBytes > 8<<20 || limits.OutputBytes < 1 || limits.OutputBytes > 8<<20 {
		return ErrDiskUsageLimit
	}
	if len(q.Start) != 10 || len(q.End) != 10 {
		return ErrDiskUsageLimit
	}
	start, err := time.Parse("2006-01-02", q.Start)
	if err != nil {
		return ErrDiskUsageLimit
	}
	end, err := time.Parse("2006-01-02", q.End)
	if err != nil || start.After(end) || end.Sub(start) > 3660*24*time.Hour {
		return ErrDiskUsageLimit
	}
	for i, d := range q.Dimensions {
		if diskRollupDimensionCode(d) == 0 || (i > 0 && d == q.Dimensions[0]) {
			return ErrDiskUsageLimit
		}
	}
	if q.FilterValue != "" && (len(q.Dimensions) != 1 || q.Dimensions[0] == domain.RollupDimensionTotal) {
		return ErrDiskUsageLimit
	}
	return nil
}
func diskRollupDimensionSelected(code byte, dimensions []string) bool {
	for _, d := range dimensions {
		if code == diskRollupDimensionCode(d) {
			return true
		}
	}
	return false
}

// One candidate index association and its as-of companion. Both physical reads
// are separately counted; absent seeks are counted too. Decode follows byte
// admission. Immutable values have a successor witness, so removing a selected
// middle version cannot make Seek silently fall back to an older cumulative row.
func diskRollupCandidate(input any, q DiskRollupQuery, cursor DiskRollupCursor, current uint64, budget *diskFailureBudget) (DiskRollupItem, []byte, bool, bool, error) {
	tx := usageTransaction(input)
	index, versions := tx.Bucket([]byte("rollup_range")), tx.Bucket([]byte("rollup_versions"))
	if index == nil || versions == nil {
		return DiskRollupItem{}, nil, false, false, ErrDiskUsageCorrupt
	}
	seek := make([]byte, diskRollupRangeKeySize)
	copy(seek[:10], q.Start)
	if cursor.positioned {
		copy(seek, cursor.after[:])
	}
	c := index.Cursor()
	k, v := c.Seek(seek)
	if cursor.positioned && bytes.Equal(k, cursor.after[:]) {
		k, v = c.Next()
	}
	if err := budget.take(v, len(k)); err != nil {
		return DiskRollupItem{}, nil, false, false, err
	}
	if k == nil || (len(k) >= 10 && string(k[:10]) > q.End) {
		return DiskRollupItem{}, nil, false, true, nil
	}
	if len(k) != diskRollupRangeKeySize || k[18] < 1 || int(k[18]) > len(diskRollupDimensions) {
		return DiskRollupItem{}, nil, false, false, ErrDiskUsageCorrupt
	}
	next := bytes.Clone(k)
	first, last, err := diskRollupBounds(v)
	if err != nil {
		return DiskRollupItem{}, nil, false, false, err
	}
	if last > current {
		return DiskRollupItem{}, nil, false, false, ErrDiskUsageCorrupt
	}
	if first > cursor.head.Sequence || !diskRollupDimensionSelected(k[18], q.Dimensions) {
		return DiskRollupItem{}, next, false, false, nil
	}
	vc := versions.Cursor()
	want := diskRollupVersionKey(k[19:], cursor.head.Sequence)
	vk, raw := vc.Seek(want)
	if vk == nil || bytes.Compare(vk, want) > 0 {
		vk, raw = vc.Prev()
	}
	if err := budget.take(raw, len(vk)); err != nil {
		return DiskRollupItem{}, nil, false, false, err
	}
	if len(vk) != 40 || !bytes.Equal(vk[:32], k[19:]) || raw == nil {
		return DiskRollupItem{}, nil, false, false, ErrDiskUsageCorrupt
	}
	sequence := binary.BigEndian.Uint64(vk[32:])
	var version diskRollupVersion
	if json.Unmarshal(raw, &version) != nil || version.Sequence != sequence || sequence < first || sequence > last || sequence > cursor.head.Sequence || version.Key.Validate() != nil || version.Key.PeriodID != version.Row.PeriodID || version.Key.TimezoneVersion != version.Row.TimezoneVersion || !diskRollupValidRow(version.Row, sequence) || !bytes.Equal(diskRollupRangeKey(version.Key), k) {
		return DiskRollupItem{}, nil, false, false, ErrDiskUsageCorrupt
	}
	if last <= cursor.head.Sequence {
		if sequence != last || version.NextSequence != 0 {
			return DiskRollupItem{}, nil, false, false, ErrDiskUsageCorrupt
		}
	} else if version.NextSequence <= cursor.head.Sequence || version.NextSequence > last {
		return DiskRollupItem{}, nil, false, false, ErrDiskUsageCorrupt
	}
	// Preserve inclusive full-label filtering, including legal wide PeriodID.
	// Physical order is prefix/TZ/dimension/hash, not complete group-key order.
	if version.Key.PeriodID < q.Start || version.Key.PeriodID > q.End || (q.FilterValue != "" && version.Key.DimensionKey != q.FilterValue) {
		return DiskRollupItem{}, next, false, false, nil
	}
	return DiskRollupItem{Key: version.Key, Row: version.Row, Sequence: sequence}, next, true, false, nil
}

// RollupPage freezes mutable daily values at H without retaining a transaction
// between pages. Histograms retain approximate-summary semantics. The fixed
// local cursor is generation/query bound; it is not an authenticated HTTP token.
// Versions stay until generation retirement; retention needs cursor ownership.
func (p *DiskProjection) RollupPage(ctx context.Context, q DiskRollupQuery, input DiskRollupCursor, limits DiskUsagePageLimits) (DiskRollupPage, error) {
	if err := diskRollupValidateQuery(q, limits); err != nil {
		return DiskRollupPage{}, err
	}
	var page DiskRollupPage
	err := p.readRows(ctx, func(db *usageEngine) error {
		signatureQuery := q
		signatureQuery.Limit = 0
		raw, err := json.Marshal(signatureQuery)
		if err != nil {
			return err
		}
		if len(raw) > 1<<20 {
			return ErrDiskUsageLimit
		}
		signature := diskUsageID(string(raw))
		cursor := input
		var current uint64
		if err := wrapUsageDatabase(db).View(func(tx *usageTx) error {
			state, found, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
			if err != nil || !found || state.Version != diskUsageVersion {
				return errors.Join(err, ErrDiskUsageCorrupt)
			}
			current = state.Watermark.Sequence
			if cursor == (DiskRollupCursor{}) {
				cursor.generation = p.generation
				copy(cursor.query[:], signature)
				cursor.head = state.Watermark
			} else if cursor.generation != p.generation || !bytes.Equal(cursor.query[:], signature) || cursor.head.Sequence > state.Watermark.Sequence || (cursor.positioned && (string(cursor.after[:10]) < q.Start || string(cursor.after[:10]) > q.End)) {
				return ErrDiskUsageCorrupt
			}
			return nil
		}); err != nil {
			return err
		}
		page.Head = cursor.head
		page.Items = make([]DiskRollupItem, 0, q.Limit)
		budget := diskFailureBudget{limits: limits}
		output := 0
		progress := false
		for len(page.Items) < q.Limit {
			if err := ctx.Err(); err != nil {
				return err
			}
			var item DiskRollupItem
			var next []byte
			var selected, done bool
			err := wrapUsageDatabase(db).View(func(tx *usageTx) error {
				var err error
				item, next, selected, done, err = diskRollupCandidate(tx, q, cursor, current, &budget)
				return err
			})
			if errors.Is(err, errDiskFailureBudget) {
				if !progress {
					return ErrDiskUsageLimit
				}
				break
			}
			if err != nil {
				return err
			}
			if done {
				page.Done = true
				break
			}
			if selected {
				encoded, err := json.Marshal(item)
				if err != nil {
					return err
				}
				if len(encoded) > limits.OutputBytes-output {
					if !progress {
						return ErrDiskUsageLimit
					}
					break
				}
				output += len(encoded)
				page.Items = append(page.Items, item)
			}
			copy(cursor.after[:], next)
			cursor.positioned = true
			progress = true
		}
		if !page.Done {
			page.Next = cursor
		}
		page.Scanned = budget.rows
		page.InputBytes = budget.bytes
		page.OutputBytes = output
		return nil
	})
	if err != nil {
		return DiskRollupPage{}, err
	}
	return page, nil
}
