package ledger

import (
	"bytes"
	"context"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/domain"
)

var _ AccountingAdmissionReader = (*AuthenticatedLedgerProjection)(nil)

// Shared canonical cohort validators operate on verified raw copies. This
// adapter has no write capability and does not import ordinary DB hints.
type authenticatedReadRows struct {
	ctx context.Context
	pin *authenticatedstate.Snapshot
}

func (r authenticatedReadRows) get(bucket string, key []byte) ([]byte, bool, error) {
	k, err := authenticatedLedgerKey(bucket, key)
	if err != nil {
		return nil, false, err
	}
	raw, found, err := r.pin.Get(r.ctx, k)
	if err != nil {
		return nil, false, err
	}
	if found && len(raw) > projectionMaxValue {
		return nil, false, ErrProjectionCorrupt
	}
	return raw, found, nil
}
func (r authenticatedReadRows) floor(bucket string, key []byte) ([]byte, []byte, bool, error) {
	k, err := authenticatedLedgerKey(bucket, key)
	if err != nil {
		return nil, nil, false, err
	}
	row, found, err := r.pin.Floor(r.ctx, k)
	if err != nil || !found {
		return nil, nil, false, err
	}
	if len(row.Key) < 2 || row.Key[0] != k[0] {
		return nil, nil, false, nil
	}
	if len(row.Value) > projectionMaxValue {
		return nil, nil, false, ErrProjectionCorrupt
	}
	return row.Key[1:], row.Value, true, nil
}
func (authenticatedReadRows) put(string, []byte, []byte) error { return ErrProjectionUnavailable }
func (authenticatedReadRows) delete(string, []byte) error      { return ErrProjectionUnavailable }

func authenticatedGovernancePage[T any](ctx context.Context, p *AuthenticatedLedgerProjection, options GovernanceListOptions, index, scope, bucket string, decode func(T, string) (bool, error)) (GovernanceListPage[T], error) {
	page := GovernanceListPage[T]{Items: make([]T, 0)}
	err := authenticatedPointJob(ctx, p, func(c context.Context, pin *authenticatedstate.Snapshot, _ ReplayAnchor) error {
		prefix := []byte(nil)
		if scope != "" {
			prefix = projectionTuple(scope)
		}
		readBytes, last := 0, ""
		rows := authenticatedReadRows{c, pin}
		return authenticatedIndexWalk(c, pin, index, prefix, projectionListKey(scope, options.After), func(k, rawID []byte) (bool, error) {
			if options.After != "" && bytes.Equal(k, projectionListKey(scope, options.After)) {
				return false, nil
			}
			if page.Scanned == 4*options.Limits.Rows+64 || len(page.Items) == options.Limits.Rows {
				page.NextID = last
				return true, nil
			}
			if len(rawID) == 0 || len(rawID) > domain.MaxGovernanceIDLength || !bytes.Equal(k, projectionListKey(scope, string(rawID))) {
				return false, ErrProjectionCorrupt
			}
			raw, found, err := rows.get(bucket, projectionIDKey(string(rawID)))
			if err != nil {
				return false, err
			}
			if !found {
				return false, ErrProjectionCorrupt
			}
			if len(raw) > options.Limits.Bytes-readBytes {
				if page.Scanned == 0 {
					return false, ErrProjectionPageLimit
				}
				page.NextID = last
				return true, nil
			}
			row, err := recoveryDecode[T](raw)
			if err != nil {
				return false, err
			}
			matches, err := decode(row, string(rawID))
			if err != nil {
				return false, err
			}
			readBytes += len(raw)
			page.Scanned++
			last = string(rawID)
			if matches {
				page.Items = append(page.Items, row)
				page.Bytes += len(raw)
			}
			return false, nil
		})
	})
	if err != nil {
		return GovernanceListPage[T]{}, err
	}
	return page, nil
}

func (p *AuthenticatedLedgerProjection) ReadWorkUnitPage(ctx context.Context, q WorkUnitListQuery) (GovernanceListPage[domain.WorkUnit], error) {
	if err := validateWorkUnitList(q); err != nil {
		return GovernanceListPage[domain.WorkUnit]{}, err
	}
	if len(q.ProjectID) > maxPayloadSize {
		return GovernanceListPage[domain.WorkUnit]{}, ErrProjectionPageLimit
	}
	index := "work_units_list"
	if q.ProjectID != "" {
		index = "work_units_list_project"
	}
	return authenticatedGovernancePage(ctx, p, q.GovernanceListOptions, index, q.ProjectID, "work_units", func(row domain.WorkUnit, id string) (bool, error) {
		if row.ID != id || row.Validate() != nil || q.ProjectID != "" && row.ProjectID != q.ProjectID {
			return false, ErrProjectionCorrupt
		}
		return q.Status == "" || row.Status == q.Status, nil
	})
}
func (p *AuthenticatedLedgerProjection) ReadRunPage(ctx context.Context, q RunListQuery) (GovernanceListPage[domain.Run], error) {
	if err := validateRunList(q); err != nil {
		return GovernanceListPage[domain.Run]{}, err
	}
	if len(q.ProjectID) > maxPayloadSize || len(q.WorkUnitID) > maxPayloadSize {
		return GovernanceListPage[domain.Run]{}, ErrProjectionPageLimit
	}
	index, scope := "runs_list", ""
	if q.ProjectID != "" {
		index, scope = "runs_list_project", q.ProjectID
	}
	if q.WorkUnitID != "" {
		index, scope = "runs_list_work_unit", q.WorkUnitID
	}
	return authenticatedGovernancePage(ctx, p, q.GovernanceListOptions, index, scope, "runs", func(row domain.Run, id string) (bool, error) {
		if row.ID != id || row.Validate() != nil || index == "runs_list_project" && row.ProjectID != q.ProjectID || q.WorkUnitID != "" && row.WorkUnitID != q.WorkUnitID {
			return false, ErrProjectionCorrupt
		}
		return (q.ProjectID == "" || row.ProjectID == q.ProjectID) && (q.Status == "" || domain.EffectiveRunStatus(row, q.At) == q.Status), nil
	})
}

func (p *AuthenticatedLedgerProjection) ReadWorkUnitMaturityPage(ctx context.Context, refs []WorkUnitReference) (WorkUnitMaturityPage, error) {
	if err := validateMaturityReferences(refs); err != nil {
		return WorkUnitMaturityPage{}, err
	}
	var page WorkUnitMaturityPage
	err := authenticatedPointJob(ctx, p, func(c context.Context, pin *authenticatedstate.Snapshot, head ReplayAnchor) error {
		rows := authenticatedReadRows{c, pin}
		readBytes := 0
		page = WorkUnitMaturityPage{Items: make([]WorkUnitMaturity, 0, len(refs)), Watermark: head.Head}
		cache := make(map[WorkUnitReference]WorkUnitMaturity, len(refs))
		for _, ref := range refs {
			if err := c.Err(); err != nil {
				return err
			}
			if cached, ok := cache[ref]; ok {
				page.Items = append(page.Items, cached)
				continue
			}
			row := WorkUnitMaturity{Reference: ref}
			raw, found, err := rows.get("work_units", projectionIDKey(ref.WorkUnitID))
			if err != nil {
				return err
			}
			if !found {
				cache[ref] = row
				page.Items = append(page.Items, row)
				continue
			}
			if len(raw) > WorkUnitMaturityMaxBytes-readBytes {
				return ErrProjectionPageLimit
			}
			readBytes += len(raw)
			wu, err := recoveryDecode[domain.WorkUnit](raw)
			if err != nil {
				return err
			}
			if wu.ID != ref.WorkUnitID || wu.Validate() != nil {
				return ErrProjectionCorrupt
			}
			if wu.ProjectID != ref.ProjectID {
				cache[ref] = row
				page.Items = append(page.Items, row)
				continue
			}
			raw, seq, err := cohortVersionRowsRaw(rows, ref.WorkUnitID, head.Head.Sequence)
			if err != nil {
				return err
			}
			if len(raw) > WorkUnitMaturityMaxBytes-readBytes {
				return ErrProjectionPageLimit
			}
			readBytes += len(raw)
			v, err := readCohortRowsVersion(rows, ref.WorkUnitID, seq)
			if err != nil {
				return err
			}
			if v.Status != wu.Status {
				return ErrProjectionCorrupt
			}
			row.Found, row.Status, row.Inflight = true, v.Status, v.ActiveRequests != 0 || v.PendingAttempts != 0
			cache[ref] = row
			page.Items = append(page.Items, row)
		}
		return nil
	})
	if err != nil {
		return WorkUnitMaturityPage{}, err
	}
	return page, nil
}
