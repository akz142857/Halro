package ledger

import (
	"context"
	"errors"
	"github.com/akz142857/Halro/internal/boundedfilecache"
)

// PrepareColdMaintenanceRead syncs and advises only this real frozen native
// prefix. It frees no logical fee or physical extent, and grants no source or
// role authority. Capacity preparation uses it between joined finite batches.
func (l *Log) PrepareColdMaintenanceRead(ctx context.Context, lease *MaintenanceLease) error {
	if l == nil || lease == nil {
		return ErrLedgerMaintenance
	}
	if _, err := lease.ValidateOwner(l); err != nil {
		return err
	}
	return lease.ReadFrozenHead(ctx, func(ctx context.Context, head Watermark, _ [32]byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.file == nil || l.offset != head.Offset {
			return ErrLedgerMaintenance
		}
		info, err := l.file.Stat()
		if err != nil || info.Size() < head.Offset {
			return errors.Join(ErrCorrupt, err)
		}
		writer := boundedfilecache.Writer{File: l.file}
		return writer.Advance(head.Offset, true)
	})
}
