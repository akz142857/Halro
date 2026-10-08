package ledger

import (
	"bytes"
	"context"
	"math"
	"sort"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/domain"
)

// Up to one WorkUnit and the domain lifetime limit of Runs are returned.
// This logical workspace is distinct from per-query costs and still requires
// physical calibration. It does not cover caller retention after return.
const AuthenticatedWorkUnitRAMBytes int64 = 128 << 20

var _ GovernanceCapacityReader = (*AuthenticatedLedgerProjection)(nil)
var _ AccountingWorkUnitReader = (*AuthenticatedLedgerProjection)(nil)
var _ AccountingObservationReader = (*AuthenticatedLedgerProjection)(nil)

func authenticatedPrefixEnd(prefix []byte) []byte {
	hi := bytes.Clone(prefix)
	for i := len(hi) - 1; i >= 0; i-- {
		hi[i]++
		if hi[i] != 0 {
			return hi[:i+1]
		}
	}
	return nil
}

// One authenticated row and cursor at a time, no historical ID map. The scope
// and its successor exclude all other buckets, even when the hash ends in ff.
func authenticatedIndexWalk(ctx context.Context, pin *authenticatedstate.Snapshot, bucket string, prefix, start []byte, visit func([]byte, []byte) (bool, error)) error {
	ordinal, err := authenticatedLedgerBucket(bucket)
	if err != nil {
		return err
	}
	scope := append([]byte{ordinal}, prefix...)
	lo := scope
	if start != nil {
		lo = append([]byte{ordinal}, start...)
		if !bytes.HasPrefix(lo, scope) {
			return ErrProjectionCorrupt
		}
	}
	hi := authenticatedPrefixEnd(scope)
	cursor := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := pin.Range(ctx, authenticatedstate.RangeOptions{Lo: lo, Hi: hi, Cursor: cursor, MaxRows: 1, MaxBytes: 4 << 20, MaxPages: 4096})
		if err != nil {
			return err
		}
		if len(page.Rows) > 1 || len(page.Rows) == 0 && !page.Done {
			return ErrProjectionCorrupt
		}
		for _, row := range page.Rows {
			if !bytes.HasPrefix(row.Key, scope) {
				return ErrProjectionCorrupt
			}
			stop, err := visit(row.Key[1:], row.Value)
			if err != nil {
				return err
			}
			if stop {
				return nil
			}
		}
		if page.Done {
			return nil
		}
		if page.Cursor == "" || page.Cursor == cursor {
			return ErrProjectionCorrupt
		}
		cursor = page.Cursor
	}
}

func authenticatedIndexID(key, raw []byte, size int) (string, error) {
	if len(key) != size || len(raw) == 0 || len(raw) > maxPayloadSize {
		return "", ErrProjectionCorrupt
	}
	return string(raw), nil
}

func authenticatedRunIndex(ctx context.Context, pin *authenticatedstate.Snapshot, workUnitID string, visit func(domain.Run) error) error {
	count := 0
	return authenticatedIndexWalk(ctx, pin, "runs_by_work_unit", projectionTuple(workUnitID), nil, func(k, raw []byte) (bool, error) {
		id, err := authenticatedIndexID(k, raw, 64)
		if err != nil {
			return false, err
		}
		count++
		if count > domain.MaxRunsPerWorkUnit || !bytes.Equal(k, projectionTuple(workUnitID, id)) {
			return false, ErrProjectionCorrupt
		}
		run, found, err := authenticatedPointGet[domain.Run](ctx, pin, "runs", projectionIDKey(id))
		if err != nil {
			return false, err
		}
		if !found || run.ID != id || run.WorkUnitID != workUnitID || run.Validate() != nil {
			return false, ErrProjectionCorrupt
		}
		return false, visit(run)
	})
}

func (p *AuthenticatedLedgerProjection) ReadOpenWorkUnitCount(ctx context.Context, projectID string) (int64, error) {
	if err := authenticatedPointID(projectID); err != nil {
		return 0, err
	}
	var count int64
	err := authenticatedPointJob(ctx, p, func(c context.Context, pin *authenticatedstate.Snapshot, head ReplayAnchor) error {
		return authenticatedIndexWalk(c, pin, "open_work_units_by_project", projectionTuple(projectID), nil, func(k, raw []byte) (bool, error) {
			id, err := authenticatedIndexID(k, raw, 64)
			if err != nil {
				return false, err
			}
			wu, found, err := authenticatedPointGet[domain.WorkUnit](c, pin, "work_units", projectionIDKey(id))
			if err != nil {
				return false, err
			}
			if !found || wu.ID != id || wu.ProjectID != projectID || wu.Status != domain.WorkUnitOpen || wu.Validate() != nil || !bytes.Equal(k, projectionTuple(projectID, id)) || count == math.MaxInt64 {
				return false, ErrProjectionCorrupt
			}
			count++
			if uint64(count) > head.Head.Sequence {
				return false, ErrProjectionCorrupt
			}
			return false, nil
		})
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (p *AuthenticatedLedgerProjection) ReadRunCapacity(ctx context.Context, projectID, workUnitID string, at time.Time) (RunCapacity, error) {
	if err := authenticatedPointID(projectID); err != nil {
		return RunCapacity{}, err
	}
	if len(workUnitID) > maxPayloadSize {
		return RunCapacity{}, ErrProjectionPageLimit
	}
	var result RunCapacity
	err := authenticatedPointJob(ctx, p, func(c context.Context, pin *authenticatedstate.Snapshot, head ReplayAnchor) error {
		if err := authenticatedIndexWalk(c, pin, "active_runs_by_project", projectionTuple(projectID), projectionAfterExpiryPrefix(projectID, at), func(k, raw []byte) (bool, error) {
			id, err := authenticatedIndexID(k, raw, 76)
			if err != nil {
				return false, err
			}
			run, found, err := authenticatedPointGet[domain.Run](c, pin, "runs", projectionIDKey(id))
			if err != nil {
				return false, err
			}
			if !found || run.ID != id || run.ProjectID != projectID || run.Status != domain.RunActive || run.Validate() != nil || !bytes.Equal(k, projectionActiveRunKey(run)) || !at.Before(run.ExpiresAt) || result.ActiveRuns == math.MaxInt64 {
				return false, ErrProjectionCorrupt
			}
			result.ActiveRuns++
			if uint64(result.ActiveRuns) > head.Head.Sequence {
				return false, ErrProjectionCorrupt
			}
			return false, nil
		}); err != nil {
			return err
		}
		if workUnitID == "" {
			return nil
		}
		return authenticatedRunIndex(c, pin, workUnitID, func(run domain.Run) error {
			if run.ProjectID == projectID {
				result.WorkUnitRuns++
			}
			return nil
		})
	})
	if err != nil {
		return RunCapacity{}, err
	}
	return result, nil
}

func authenticatedWorkUnitInflight(ctx context.Context, pin *authenticatedstate.Snapshot, wu domain.WorkUnit, head ReplayAnchor) (bool, error) {
	inflight := false
	for _, bucket := range []string{"active_requests_by_work_unit", "pending_by_work_unit"} {
		err := authenticatedIndexWalk(ctx, pin, bucket, projectionTuple(wu.ID), nil, func(k, raw []byte) (bool, error) {
			id, err := authenticatedIndexID(k, raw, 64)
			if err != nil {
				return false, err
			}
			if !bytes.Equal(k, projectionTuple(wu.ID, id)) {
				return false, ErrProjectionCorrupt
			}
			if bucket == "active_requests_by_work_unit" {
				r, found, err := authenticatedPointGet[ProjectedRequest](ctx, pin, "requests", projectionIDKey(id))
				if err != nil {
					return false, err
				}
				if !found || authenticatedRequestValid(r, id, head) != nil || r.Finalized != nil || r.Accepted.Event.WorkUnitID != wu.ID || r.Accepted.Event.ProjectID != wu.ProjectID || r.Accepted.Event.RunID == "" {
					return false, ErrProjectionCorrupt
				}
			} else {
				r, found, err := authenticatedPointGet[projectionReservation](ctx, pin, "pending", projectionIDKey(id))
				if err != nil {
					return false, err
				}
				e := r.Evidence.Event
				if !found || r.Evidence.Sequence == 0 || r.Evidence.Sequence > head.Head.Sequence || e.Kind != EventReservationCreated || e.Validate() != nil || e.AttemptID != id || e.WorkUnitID != wu.ID || e.ProjectID != wu.ProjectID || r.Reservation.Lease.AttemptID != id || r.Reservation.Lease.RequestID != e.RequestID {
					return false, ErrProjectionCorrupt
				}
			}
			inflight = true
			return true, nil
		})
		if err != nil {
			return false, err
		}
	}
	return inflight, nil
}

func (p *AuthenticatedLedgerProjection) ReadWorkUnitAccounting(ctx context.Context, projectID, workUnitID string, includeRuns bool) (WorkUnitAccountingView, bool, error) {
	if err := authenticatedPointID(workUnitID); err != nil {
		return WorkUnitAccountingView{}, false, err
	}
	if len(projectID) > maxPayloadSize {
		return WorkUnitAccountingView{}, false, ErrProjectionPageLimit
	}
	var result WorkUnitAccountingView
	var found bool
	err := authenticatedReadJob(ctx, p, AuthenticatedWorkUnitRAMBytes, func(c context.Context, pin *authenticatedstate.Snapshot, head ReplayAnchor) error {
		wu, ok, err := authenticatedPointGet[domain.WorkUnit](c, pin, "work_units", projectionIDKey(workUnitID))
		if err != nil || !ok {
			return err
		}
		if wu.ID != workUnitID || wu.Validate() != nil {
			return ErrProjectionCorrupt
		}
		if projectID != "" && wu.ProjectID != projectID {
			return nil
		}
		result = WorkUnitAccountingView{WorkUnit: wu, Watermark: head.Head}
		result.Inflight, err = authenticatedWorkUnitInflight(c, pin, wu, head)
		if err != nil {
			return err
		}
		if includeRuns {
			result.Runs = make([]domain.Run, 0)
			if err := authenticatedRunIndex(c, pin, workUnitID, func(run domain.Run) error {
				if run.ProjectID != wu.ProjectID {
					return ErrProjectionCorrupt
				}
				result.Runs = append(result.Runs, run)
				return nil
			}); err != nil {
				return err
			}
			sort.Slice(result.Runs, func(i, j int) bool { return result.Runs[i].ID < result.Runs[j].ID })
		}
		found = true
		return nil
	})
	if err != nil {
		return WorkUnitAccountingView{}, false, err
	}
	return result, found, nil
}

func (p *AuthenticatedLedgerProjection) ReadPendingLeaseStats(ctx context.Context, now time.Time) (PendingLeaseStatistics, error) {
	var result PendingLeaseStatistics
	err := authenticatedPointJob(ctx, p, func(c context.Context, pin *authenticatedstate.Snapshot, head ReplayAnchor) error {
		count, found, err := authenticatedPointGet[int64](c, pin, "meta", []byte("pending_count"))
		if err != nil {
			return err
		}
		if !found || count < 0 || uint64(count) > head.Head.Sequence {
			return ErrProjectionCorrupt
		}
		result = PendingLeaseStatistics{Count: count, Watermark: head.Head}
		var timeFound, pendingFound bool
		if err := authenticatedIndexWalk(c, pin, "pending", nil, nil, func(k, raw []byte) (bool, error) { pendingFound = true; return true, nil }); err != nil {
			return err
		}
		if err := authenticatedIndexWalk(c, pin, "pending_by_time", nil, nil, func(k, raw []byte) (bool, error) {
			timeFound = true
			id, err := authenticatedIndexID(k, raw, 44)
			if err != nil {
				return false, err
			}
			r, found, err := authenticatedPointGet[projectionReservation](c, pin, "pending", projectionIDKey(id))
			if err != nil {
				return false, err
			}
			e := r.Evidence.Event
			if !found || r.Evidence.Sequence == 0 || r.Evidence.Sequence > head.Head.Sequence || e.Kind != EventReservationCreated || e.AttemptID != id || e.Validate() != nil || r.Reservation.Lease.AttemptID != id || !bytes.Equal(k, projectionPendingTimeKey(e)) {
				return false, ErrProjectionCorrupt
			}
			if !now.Before(e.OccurredAt) {
				result.OldestAge = now.Sub(e.OccurredAt)
			}
			return true, nil
		}); err != nil {
			return err
		}
		if (count == 0) == timeFound || timeFound != pendingFound {
			return ErrProjectionCorrupt
		}
		return nil
	})
	if err != nil {
		return PendingLeaseStatistics{}, err
	}
	return result, nil
}
