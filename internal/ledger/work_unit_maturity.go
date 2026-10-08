package ledger

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

const WorkUnitMaturityMaxRows = 200
const WorkUnitMaturityMaxBytes = 1 << 20

type WorkUnitReference struct{ ProjectID, WorkUnitID string }
type WorkUnitMaturity struct {
	Reference WorkUnitReference
	Found     bool
	Status    domain.WorkUnitStatus
	Inflight  bool
}
type WorkUnitMaturityPage struct {
	Items     []WorkUnitMaturity
	Watermark Watermark
}

type AccountingMaturityReader interface {
	ReadWorkUnitMaturityPage(context.Context, []WorkUnitReference) (WorkUnitMaturityPage, error)
}

func validateMaturityReferences(refs []WorkUnitReference) error {
	if len(refs) > WorkUnitMaturityMaxRows {
		return ErrProjectionPageLimit
	}
	size := 0
	for _, ref := range refs {
		if ref.ProjectID == "" || !domain.ValidWorkUnitID(ref.WorkUnitID) {
			return ErrProjectionPageLimit
		}
		size += len(ref.ProjectID) + len(ref.WorkUnitID)
		if size > WorkUnitMaturityMaxBytes {
			return ErrProjectionPageLimit
		}
	}
	return nil
}

// One finite page, one accounting cut; missing or cross-project references
// are successful absence. IO/decode/index failure is never successful absence.
func (s *State) ReadWorkUnitMaturityPage(ctx context.Context, refs []WorkUnitReference) (WorkUnitMaturityPage, error) {
	if err := ctx.Err(); err != nil {
		return WorkUnitMaturityPage{}, err
	}
	if err := validateMaturityReferences(refs); err != nil {
		return WorkUnitMaturityPage{}, err
	}
	if s == nil {
		return WorkUnitMaturityPage{}, ErrProjectionUnavailable
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	page := WorkUnitMaturityPage{Items: make([]WorkUnitMaturity, 0, len(refs)), Watermark: s.watermark}
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return WorkUnitMaturityPage{}, err
		}
		row := WorkUnitMaturity{Reference: ref}
		if w, ok := s.workUnits[ref.WorkUnitID]; ok && w.ProjectID == ref.ProjectID {
			if w.ID != ref.WorkUnitID || w.Validate() != nil {
				return WorkUnitMaturityPage{}, ErrProjectionCorrupt
			}
			row.Found, row.Status = true, w.Status
			row.Inflight = len(s.activeByWorkUnit[w.ID]) != 0 || len(s.pendingByWorkUnit[w.ID]) != 0
		}
		page.Items = append(page.Items, row)
	}
	if err := ctx.Err(); err != nil {
		return WorkUnitMaturityPage{}, err
	}
	return page, nil
}

func (p *LedgerProjection) ReadWorkUnitMaturityPage(ctx context.Context, refs []WorkUnitReference) (WorkUnitMaturityPage, error) {
	if err := validateMaturityReferences(refs); err != nil {
		return WorkUnitMaturityPage{}, err
	}
	var result WorkUnitMaturityPage
	err := p.governanceCapacityView(ctx, func(tx *bbolt.Tx) error {
		pos, ok, err := projectionGet[projectionPosition](tx, "meta", []byte("position"))
		if err != nil || !ok {
			return errors.Join(ErrProjectionCorrupt, err)
		}
		units := tx.Bucket([]byte("work_units"))
		if units == nil || tx.Bucket([]byte("cohort_versions")) == nil {
			return ErrProjectionCorrupt
		}
		result = WorkUnitMaturityPage{Items: make([]WorkUnitMaturity, 0, len(refs)), Watermark: pos.Watermark}
		readBytes := 0
		// A WorkUnit may have many short Outcome revisions but wide native
		// creation metadata. Read/decode each scoped reference only once in
		// this same transaction; duplicate outputs retain their input order.
		cache := make(map[WorkUnitReference]WorkUnitMaturity, len(refs))
		for _, ref := range refs {
			if err := ctx.Err(); err != nil {
				return err
			}
			if cached, ok := cache[ref]; ok {
				result.Items = append(result.Items, cached)
				continue
			}
			row := WorkUnitMaturity{Reference: ref}
			raw := units.Get(projectionIDKey(ref.WorkUnitID))
			if raw == nil {
				cache[ref] = row
				result.Items = append(result.Items, row)
				continue
			}
			if len(raw) > WorkUnitMaturityMaxBytes-readBytes {
				return ErrProjectionPageLimit
			}
			readBytes += len(raw)
			w, ok, err := projectionGet[domain.WorkUnit](tx, "work_units", projectionIDKey(ref.WorkUnitID))
			if err != nil || !ok || w.ID != ref.WorkUnitID || w.Validate() != nil {
				return errors.Join(ErrProjectionCorrupt, err)
			}
			if w.ProjectID != ref.ProjectID {
				cache[ref] = row
				result.Items = append(result.Items, row)
				continue
			}
			raw, seq, err := cohortVersionRaw(tx, ref.WorkUnitID, pos.Watermark.Sequence)
			if err != nil {
				return err
			}
			if len(raw) > WorkUnitMaturityMaxBytes-readBytes {
				return ErrProjectionPageLimit
			}
			readBytes += len(raw)
			v, err := readCohortVersion(tx, ref.WorkUnitID, seq)
			if err != nil {
				return err
			}
			if v.Status != w.Status {
				return ErrProjectionCorrupt
			}
			row.Found, row.Status, row.Inflight = true, v.Status, v.ActiveRequests != 0 || v.PendingAttempts != 0
			cache[ref] = row
			result.Items = append(result.Items, row)
		}
		return nil
	})
	if err != nil {
		return WorkUnitMaturityPage{}, err
	}
	return result, nil
}
