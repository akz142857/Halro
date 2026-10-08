package ledger

import (
	"bytes"
	"context"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

var projectionListBuckets = []string{"work_units_list", "work_units_list_project", "runs_list", "runs_list_project", "runs_list_work_unit"}

func projectionListKey(scope, id string) []byte {
	if scope == "" {
		return []byte(id)
	}
	return append(projectionTuple(scope), []byte(id)...)
}
func projectionStoreListIndexes(tx *bbolt.Tx, e Event) error {
	return projectionRowsStoreListIndexes(boltProjectionRows{tx}, e)
}

func projectionRowsStoreListIndexes(tx projectionRows, e Event) error {
	put := func(bucket, scope, id string) error {
		return tx.put(bucket, projectionListKey(scope, id), []byte(id))
	}
	switch e.Kind {
	case EventWorkUnitCreated:
		if err := put("work_units_list", "", e.WorkUnitID); err != nil {
			return err
		}
		return put("work_units_list_project", e.ProjectID, e.WorkUnitID)
	case EventRunCreated:
		if err := put("runs_list", "", e.RunID); err != nil {
			return err
		}
		if err := put("runs_list_project", e.ProjectID, e.RunID); err != nil {
			return err
		}
		return put("runs_list_work_unit", e.WorkUnitID, e.RunID)
	}
	return nil
}

func projectionGovernancePage[T any](ctx context.Context, p *LedgerProjection, options GovernanceListOptions, index, scope, bucket string, decode func([]byte, []byte, T) (bool, error)) (GovernanceListPage[T], error) {
	page := GovernanceListPage[T]{Items: make([]T, 0)}
	err := p.governanceCapacityView(ctx, func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(index))
		rows := tx.Bucket([]byte(bucket))
		if b == nil || rows == nil {
			return ErrProjectionCorrupt
		}
		prefix := []byte(nil)
		if scope != "" {
			prefix = projectionTuple(scope)
		}
		c := b.Cursor()
		k, v := c.Seek(projectionListKey(scope, options.After))
		if options.After != "" && bytes.Equal(k, projectionListKey(scope, options.After)) {
			k, v = c.Next()
		}
		readBytes := 0
		last := ""
		for ; k != nil && (len(prefix) == 0 || bytes.HasPrefix(k, prefix)); k, v = c.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			// Limit scanned rows as well as returned rows; sparse status filters
			// may legitimately return an empty page with a continuation.
			if page.Scanned == 4*options.Limits.Rows+64 || len(page.Items) == options.Limits.Rows {
				page.NextID = last
				return nil
			}
			if len(v) == 0 || len(v) > domain.MaxGovernanceIDLength || !bytes.Equal(k, projectionListKey(scope, string(v))) {
				return ErrProjectionCorrupt
			}
			raw := rows.Get(projectionIDKey(string(v)))
			if raw == nil || len(raw) > projectionMaxValue {
				return ErrProjectionCorrupt
			}
			// Bound raw input before decoding, including filtered rows.
			if len(raw) > options.Limits.Bytes-readBytes {
				if page.Scanned == 0 {
					return ErrProjectionPageLimit
				}
				page.NextID = last
				return nil
			}
			row, found, err := projectionGet[T](tx, bucket, projectionIDKey(string(v)))
			if err != nil {
				return err
			}
			if !found {
				return ErrProjectionCorrupt
			}
			matches, err := decode(k, v, row)
			if err != nil {
				return err
			}
			readBytes += len(raw)
			page.Scanned++
			last = string(v)
			if matches {
				page.Items = append(page.Items, row)
				page.Bytes += len(raw)
			}
		}
		return nil
	})
	if err != nil {
		return GovernanceListPage[T]{}, err
	}
	return page, nil
}

func (p *LedgerProjection) ReadWorkUnitPage(ctx context.Context, q WorkUnitListQuery) (GovernanceListPage[domain.WorkUnit], error) {
	if err := validateWorkUnitList(q); err != nil {
		return GovernanceListPage[domain.WorkUnit]{}, err
	}
	index := "work_units_list"
	if q.ProjectID != "" {
		index = "work_units_list_project"
	}
	return projectionGovernancePage(ctx, p, q.GovernanceListOptions, index, q.ProjectID, "work_units", func(k, v []byte, row domain.WorkUnit) (bool, error) {
		if row.ID != string(v) || row.Validate() != nil || (q.ProjectID != "" && row.ProjectID != q.ProjectID) {
			return false, ErrProjectionCorrupt
		}
		return q.Status == "" || row.Status == q.Status, nil
	})
}
func (p *LedgerProjection) ReadRunPage(ctx context.Context, q RunListQuery) (GovernanceListPage[domain.Run], error) {
	if err := validateRunList(q); err != nil {
		return GovernanceListPage[domain.Run]{}, err
	}
	index, scope := "runs_list", ""
	if q.ProjectID != "" {
		index, scope = "runs_list_project", q.ProjectID
	}
	if q.WorkUnitID != "" {
		index, scope = "runs_list_work_unit", q.WorkUnitID
	}
	return projectionGovernancePage(ctx, p, q.GovernanceListOptions, index, scope, "runs", func(k, v []byte, row domain.Run) (bool, error) {
		if row.ID != string(v) || row.Validate() != nil || (index == "runs_list_project" && row.ProjectID != q.ProjectID) || (q.WorkUnitID != "" && row.WorkUnitID != q.WorkUnitID) {
			return false, ErrProjectionCorrupt
		}
		return (q.ProjectID == "" || row.ProjectID == q.ProjectID) && (q.Status == "" || domain.EffectiveRunStatus(row, q.At) == q.Status), nil
	})
}
