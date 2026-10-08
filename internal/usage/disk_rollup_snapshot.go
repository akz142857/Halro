package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

// DiskRollupSnapshotCursor enumerates a frozen, caller-owned projection. Any
// Apply between pages invalidates it. Unlike a date query this covers every
// legal accounting label and all six dimensions, without a historical key set.
type DiskRollupSnapshotCursor struct {
	generation [16]byte
	head       ledger.Watermark
	after      [32]byte
	positioned bool
	rows, seen uint64
}

type DiskRollupSnapshotRow struct {
	Key domain.RollupKey
	Row domain.DailyRollup
}

type DiskRollupSnapshotPage struct {
	Rows []DiskRollupSnapshotRow
	Head ledger.Watermark
	Next DiskRollupSnapshotCursor
	Done bool
}

// RollupSnapshotPage copies a finite page out of one short transaction. The
// cursor cannot be used on another generation or after the source has changed.
// It grants neither trust across restart nor permission to mutate Metadata.
func (p *DiskProjection) RollupSnapshotPage(ctx context.Context, input DiskRollupSnapshotCursor, limits DiskUsagePageLimits) (page DiskRollupSnapshotPage, err error) {
	if limits.ScanRows < 1 || limits.ScanRows > 100 || limits.InputBytes < 1 || limits.InputBytes > 8<<20 || limits.OutputBytes < 1 || limits.OutputBytes > 8<<20 {
		return page, ErrDiskUsageLimit
	}
	err = p.readRows(ctx, func(db *usageEngine) error {
		return wrapUsageDatabase(db).View(func(tx *usageTx) error {
			state, found, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
			if err != nil || !found || state.Version != diskUsageVersion {
				return errors.Join(err, ErrDiskUsageCorrupt)
			}
			cursor := input
			count, present, err := diskUsageGet[uint64](tx, "meta", []byte("rollup_rows"))
			if err != nil || !present {
				return errors.Join(err, ErrDiskUsageCorrupt)
			}
			if cursor == (DiskRollupSnapshotCursor{}) {
				cursor.generation, cursor.head = p.generation, state.Watermark
				cursor.rows = count
			} else if cursor.generation != p.generation || cursor.head != state.Watermark || cursor.rows != count || cursor.seen > count || !cursor.positioned {
				return ErrDiskUsageCursor
			}
			page.Head = cursor.head
			bucket := tx.Bucket([]byte("rollup"))
			if bucket == nil {
				return ErrDiskUsageCorrupt
			}
			c := bucket.Cursor()
			k, raw := c.First()
			if cursor.positioned {
				k, raw = c.Seek(cursor.after[:])
				if bytes.Equal(k, cursor.after[:]) {
					k, raw = c.Next()
				}
			}
			inputBytes, outputBytes := 0, 0
			for k != nil && len(page.Rows) < limits.ScanRows {
				if err := ctx.Err(); err != nil {
					return err
				}
				if len(k) != 32 || len(raw) == 0 || len(raw) > diskUsageMaxValue {
					return ErrDiskUsageCorrupt
				}
				if len(k)+len(raw) > limits.InputBytes-inputBytes {
					if len(page.Rows) == 0 {
						return ErrDiskUsageLimit
					}
					break
				}
				inputBytes += len(k) + len(raw)
				var row diskRollupRow
				if json.Unmarshal(raw, &row) != nil || row.Key.Validate() != nil || row.Key.PeriodID != row.Row.PeriodID || row.Key.TimezoneVersion != row.Row.TimezoneVersion || !diskRollupValidRow(row.Row, cursor.head.Sequence) || !bytes.Equal(k, diskUsageID(row.Key.Encode())) {
					return ErrDiskUsageCorrupt
				}
				item := DiskRollupSnapshotRow{Key: row.Key, Row: row.Row}
				encoded, err := json.Marshal(item)
				if err != nil {
					return err
				}
				if len(encoded) > limits.OutputBytes-outputBytes {
					if len(page.Rows) == 0 {
						return ErrDiskUsageLimit
					}
					break
				}
				outputBytes += len(encoded)
				page.Rows = append(page.Rows, item)
				if cursor.seen >= cursor.rows {
					return ErrDiskUsageCorrupt
				}
				cursor.seen++
				copy(cursor.after[:], k)
				cursor.positioned = true
				k, raw = c.Next()
			}
			page.Done = k == nil
			if page.Done && cursor.seen != cursor.rows {
				return ErrDiskUsageCorrupt
			}
			if !page.Done {
				page.Next = cursor
			}
			return ctx.Err()
		})
	})
	if err != nil {
		return DiskRollupSnapshotPage{}, err
	}
	return page, nil
}
