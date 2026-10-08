package ledger

import (
	"bytes"
	"context"
	"errors"
	"sort"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

// AccountingWorkUnitReader returns one coherent accounting cut. Inflight
// includes accepted requests before their first reservation AND unresolved
// attempts after request finalization. Runs, when requested, are bounded by
// the domain lifetime limit; no historical attempt list is materialized.
// The owner establishes trust and lifetime. This does not authenticate a cache.
type AccountingWorkUnitReader interface {
	ReadWorkUnitAccounting(context.Context, string, string, bool) (WorkUnitAccountingView, bool, error)
}

type WorkUnitAccountingView struct {
	WorkUnit  domain.WorkUnit
	Inflight  bool
	Runs      []domain.Run
	Watermark Watermark
}

var _ AccountingWorkUnitReader = (*State)(nil)
var _ AccountingWorkUnitReader = (*LedgerProjection)(nil)

func (s *State) ReadWorkUnitAccounting(ctx context.Context, projectID, workUnitID string, includeRuns bool) (WorkUnitAccountingView, bool, error) {
	if err := ctx.Err(); err != nil {
		return WorkUnitAccountingView{}, false, err
	}
	if s == nil {
		return WorkUnitAccountingView{}, false, ErrProjectionUnavailable
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	wu, found := s.workUnits[workUnitID]
	if !found || (projectID != "" && wu.ProjectID != projectID) {
		return WorkUnitAccountingView{}, false, ctx.Err()
	}
	v := WorkUnitAccountingView{WorkUnit: ownWorkUnit(wu), Inflight: len(s.activeByWorkUnit[workUnitID]) > 0 || len(s.pendingByWorkUnit[workUnitID]) > 0, Watermark: s.watermark}
	if includeRuns {
		if len(s.runsByWorkUnit[workUnitID]) > domain.MaxRunsPerWorkUnit {
			return WorkUnitAccountingView{}, false, ErrProjectionCorrupt
		}
		v.Runs = make([]domain.Run, 0, len(s.runsByWorkUnit[workUnitID]))
		for _, id := range s.runsByWorkUnit[workUnitID] {
			run, ok := s.runs[id]
			if !ok || run.WorkUnitID != workUnitID || run.ProjectID != wu.ProjectID {
				return WorkUnitAccountingView{}, false, ErrProjectionCorrupt
			}
			v.Runs = append(v.Runs, ownRun(run))
		}
		sort.Slice(v.Runs, func(i, j int) bool { return v.Runs[i].ID < v.Runs[j].ID })
	}
	if err := ctx.Err(); err != nil {
		return WorkUnitAccountingView{}, false, err
	}
	return v, true, nil
}

func (p *LedgerProjection) ReadWorkUnitAccounting(ctx context.Context, projectID, workUnitID string, includeRuns bool) (WorkUnitAccountingView, bool, error) {
	var result WorkUnitAccountingView
	var found bool
	err := p.governanceCapacityView(ctx, func(tx *bbolt.Tx) error {
		wu, ok, err := projectionGet[domain.WorkUnit](tx, "work_units", projectionIDKey(workUnitID))
		if err != nil || !ok {
			return err
		}
		if wu.ID != workUnitID {
			return ErrProjectionCorrupt
		}
		if err := wu.Validate(); err != nil {
			return errors.Join(ErrProjectionCorrupt, err)
		}
		if projectID != "" && wu.ProjectID != projectID {
			return nil
		}
		position, ok, err := projectionGet[projectionPosition](tx, "meta", []byte("position"))
		if err != nil || !ok {
			return errors.Join(err, ErrProjectionCorrupt)
		}
		result = WorkUnitAccountingView{WorkUnit: wu, Watermark: position.Watermark}
		prefix := projectionTuple(workUnitID)
		// Only the first matching row is needed for each existence test. Check
		// its source row and scope; a dangling/cross-scope index is a failure.
		for _, bucket := range []string{"active_requests_by_work_unit", "pending_by_work_unit"} {
			b := tx.Bucket([]byte(bucket))
			if b == nil {
				return ErrProjectionCorrupt
			}
			k, rawID := b.Cursor().Seek(prefix)
			if k == nil || !bytes.HasPrefix(k, prefix) {
				continue
			}
			if len(k) != 64 || len(rawID) == 0 || len(rawID) > maxPayloadSize || !bytes.Equal(k, projectionTuple(workUnitID, string(rawID))) {
				return ErrProjectionCorrupt
			}
			if bucket == "active_requests_by_work_unit" {
				r, ok, err := projectionGet[ProjectedRequest](tx, "requests", projectionIDKey(string(rawID)))
				if err != nil {
					return err
				}
				e := r.Accepted.Event
				if !ok || r.Finalized != nil || e.Kind != EventRequestAccepted || e.RequestID != string(rawID) || e.WorkUnitID != workUnitID || e.ProjectID != wu.ProjectID || e.RunID == "" {
					return ErrProjectionCorrupt
				}
			} else {
				r, ok, err := projectionGet[projectionReservation](tx, "pending", projectionIDKey(string(rawID)))
				if err != nil {
					return err
				}
				e := r.Evidence.Event
				if !ok || e.Kind != EventReservationCreated || e.AttemptID != string(rawID) || e.WorkUnitID != workUnitID || e.ProjectID != wu.ProjectID || r.Reservation.Lease.AttemptID != e.AttemptID {
					return ErrProjectionCorrupt
				}
			}
			result.Inflight = true
		}
		if includeRuns {
			b := tx.Bucket([]byte("runs_by_work_unit"))
			if b == nil {
				return ErrProjectionCorrupt
			}
			result.Runs = make([]domain.Run, 0)
			c := b.Cursor()
			for k, rawID := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, rawID = c.Next() {
				if err := ctx.Err(); err != nil {
					return err
				}
				if len(result.Runs) >= domain.MaxRunsPerWorkUnit || len(k) != 64 || len(rawID) == 0 || len(rawID) > maxPayloadSize || !bytes.Equal(k, projectionTuple(workUnitID, string(rawID))) {
					return ErrProjectionCorrupt
				}
				run, ok, err := projectionGet[domain.Run](tx, "runs", projectionIDKey(string(rawID)))
				if err != nil {
					return err
				}
				if !ok || run.ID != string(rawID) || run.WorkUnitID != workUnitID || run.ProjectID != wu.ProjectID {
					return ErrProjectionCorrupt
				}
				if err := run.Validate(); err != nil {
					return errors.Join(ErrProjectionCorrupt, err)
				}
				result.Runs = append(result.Runs, run)
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
