package ledger

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

// Signed Unix seconds followed by nanoseconds preserve the entire native time
// range, including dates outside UnixNano's range, and ignore display timezone.
func projectionExpiryPrefix(projectID string, at time.Time) []byte {
	key := binary.BigEndian.AppendUint64(projectionTuple(projectID), uint64(at.Unix())^(uint64(1)<<63))
	return binary.BigEndian.AppendUint32(key, uint32(at.Nanosecond()))
}

func projectionActiveRunKey(run domain.Run) []byte {
	return append(projectionExpiryPrefix(run.ProjectID, run.ExpiresAt), projectionIDKey(run.ID)...)
}

func projectionAfterExpiryPrefix(projectID string, at time.Time) []byte {
	key := projectionExpiryPrefix(projectID, at)
	// Seek is inclusive. The strict lexicographic successor of the entire
	// time prefix also skips every ID expiring at the exact same nanosecond.
	// Nanosecond is <= 999999999, so carry always stops in the time suffix
	// and cannot alter the project's fixed hash prefix.
	for i := len(key) - 1; i >= 32; i-- {
		key[i]++
		if key[i] != 0 {
			break
		}
	}
	return key
}

// Each query keeps one finite row at a time and one read transaction. It must
// finish before calling an external writer/reader or returning to admission.
// These indexes are atomically derived with rows and the native position;
// they are not content/key-set authentication of a reopened DB (D-03).
func (p *LedgerProjection) governanceCapacityView(ctx context.Context, read func(*bbolt.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil {
		return ErrProjectionUnavailable
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.ready || p.db == nil {
		return ErrProjectionUnavailable
	}
	return p.db.View(func(tx *bbolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := read(tx); err != nil {
			return err
		}
		return ctx.Err()
	})
}

func (p *LedgerProjection) ReadOpenWorkUnitCount(ctx context.Context, projectID string) (int64, error) {
	if projectID == "" {
		return 0, errors.New("project is required for governance capacity")
	}
	var count int64
	err := p.governanceCapacityView(ctx, func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("open_work_units_by_project"))
		if b == nil {
			return ErrProjectionCorrupt
		}
		prefix := projectionTuple(projectID)
		c := b.Cursor()
		for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			if len(k) != 64 || len(v) == 0 || len(v) > maxPayloadSize {
				return ErrProjectionCorrupt
			}
			id := string(v)
			row, found, err := projectionGet[domain.WorkUnit](tx, "work_units", projectionIDKey(id))
			if err != nil {
				return err
			}
			if !found || row.ID != id || row.ProjectID != projectID || row.Status != domain.WorkUnitOpen || !bytes.Equal(k, projectionTuple(projectID, id)) {
				return ErrProjectionCorrupt
			}
			count++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (p *LedgerProjection) ReadRunCapacity(ctx context.Context, projectID, workUnitID string, at time.Time) (RunCapacity, error) {
	if projectID == "" {
		return RunCapacity{}, errors.New("project is required for governance capacity")
	}
	var result RunCapacity
	err := p.governanceCapacityView(ctx, func(tx *bbolt.Tx) error {
		active := tx.Bucket([]byte("active_runs_by_project"))
		byWorkUnit := tx.Bucket([]byte("runs_by_work_unit"))
		if active == nil || byWorkUnit == nil {
			return ErrProjectionCorrupt
		}
		// Seeking past the expired portion avoids scanning historical TTLs.
		prefix := projectionTuple(projectID)
		c := active.Cursor()
		for k, v := c.Seek(projectionAfterExpiryPrefix(projectID, at)); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			if len(k) != 76 || len(v) == 0 || len(v) > maxPayloadSize {
				return ErrProjectionCorrupt
			}
			id := string(v)
			row, found, err := projectionGet[domain.Run](tx, "runs", projectionIDKey(id))
			if err != nil {
				return err
			}
			if !found || row.ID != id || row.ProjectID != projectID || row.Status != domain.RunActive || !bytes.Equal(k, projectionActiveRunKey(row)) {
				return ErrProjectionCorrupt
			}
			if at.Before(row.ExpiresAt) {
				result.ActiveRuns++
			}
		}
		if workUnitID == "" {
			return nil
		}
		prefix = projectionTuple(workUnitID)
		c = byWorkUnit.Cursor()
		for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			if len(k) != 64 || len(v) == 0 || len(v) > maxPayloadSize {
				return ErrProjectionCorrupt
			}
			id := string(v)
			row, found, err := projectionGet[domain.Run](tx, "runs", projectionIDKey(id))
			if err != nil {
				return err
			}
			if !found || row.ID != id || row.WorkUnitID != workUnitID || !bytes.Equal(k, projectionTuple(workUnitID, id)) {
				return ErrProjectionCorrupt
			}
			if row.ProjectID == projectID {
				result.WorkUnitRuns++
			}
		}
		return nil
	})
	if err != nil {
		return RunCapacity{}, err
	}
	return result, nil
}
