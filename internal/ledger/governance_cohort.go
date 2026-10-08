package ledger

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"math/bits"
	"sort"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

var ErrGovernanceSummaryOverflow = errors.New("governance summary amount overflow")

// VisitGovernanceCohort returns minimal accounting inputs at one fixed cut.
// Visitors run without State locks or DB transactions. Any error invalidates
// ALL visited inputs; callers must not publish partial results. The owner must
// retain its lifetime ticket through the last visitor. This is not cache trust.
type AccountingCohortReader interface {
	VisitGovernanceCohort(context.Context, GovernanceCohortQuery, func(GovernanceCohortRow) error) (Watermark, error)
}

type GovernanceCohortQuery struct {
	ProjectID         string
	DefinitionID      string
	DefinitionVersion uint64
	CohortStart       string
	CohortEnd         string
	MaxWorkUnits      int
	Limits            ProjectionPageLimits
}

type GovernanceCohortRow struct {
	WorkUnitID             string
	Status                 domain.WorkUnitStatus
	Inflight               bool
	KnownCostMicrosUSD     int64
	EstimatedCostMicrosUSD int64
	UnknownAttempts        int64
}

func validateGovernanceCohort(q *GovernanceCohortQuery, visit func(GovernanceCohortRow) error) error {
	start, startErr := time.Parse("2006-01-02", q.CohortStart)
	end, endErr := time.Parse("2006-01-02", q.CohortEnd)
	if visit == nil || q.ProjectID == "" || len(q.ProjectID) > maxPayloadSize || !domain.ValidOutcomeDefinitionID(q.DefinitionID) || q.DefinitionVersion == 0 || startErr != nil || endErr != nil || end.Before(start) || end.Sub(start) > 89*24*time.Hour {
		return ErrProjectionPageLimit
	}
	if q.MaxWorkUnits == 0 {
		q.MaxWorkUnits = 100_000
	}
	if q.Limits == (ProjectionPageLimits{}) {
		q.Limits = ProjectionPageLimits{Rows: 200, Bytes: 8 << 20}
	}
	if q.MaxWorkUnits < 1 || q.MaxWorkUnits > 100_000 || q.Limits.Rows < 1 || q.Limits.Rows > 1000 || q.Limits.Bytes < 1 || q.Limits.Bytes > 8<<20 {
		return ErrProjectionPageLimit
	}
	return nil
}

func cohortEligible(w domain.WorkUnit, q GovernanceCohortQuery) bool {
	if w.ProjectID != q.ProjectID || w.PeriodID < q.CohortStart || w.PeriodID > q.CohortEnd {
		return false
	}
	for _, ref := range w.OutcomeDefinitions {
		if ref.ID == q.DefinitionID && ref.Version == q.DefinitionVersion {
			return true
		}
	}
	return false
}

// Costs can legitimately exceed int64 across different periods/runs. Store
// unsigned 128-bit sums so a query overflow cannot reject a durable settlement.
// Native sequence bounds the number of terms, each at most MaxInt64.
type cohortAmount struct{ Hi, Lo uint64 }

func (a cohortAmount) add(value int64) (cohortAmount, error) {
	if value < 0 {
		return cohortAmount{}, ErrProjectionCorrupt
	}
	lo, carry := bits.Add64(a.Lo, uint64(value), 0)
	hi, carry := bits.Add64(a.Hi, 0, carry)
	if carry != 0 {
		return cohortAmount{}, ErrProjectionCorrupt
	}
	return cohortAmount{hi, lo}, nil
}

func (a cohortAmount) greater(b cohortAmount) bool {
	return a.Hi > b.Hi || a.Hi == b.Hi && a.Lo > b.Lo
}

type cohortIdentity struct {
	WorkUnit domain.WorkUnit // immutable original creation, never current status
	Sequence uint64
}

type cohortVersion struct {
	WorkUnitID      string
	Sequence        uint64
	Status          domain.WorkUnitStatus
	ActiveRequests  uint64
	PendingAttempts uint64
	Known           cohortAmount
	Estimated       cohortAmount
	Unknown         uint64
}

func (v cohortVersion) valid(id string, sequence uint64) bool {
	hi, lo := bits.Mul64(v.Sequence, math.MaxInt64)
	return v.WorkUnitID == id && v.Sequence > 0 && v.Sequence <= sequence &&
		(v.Status == domain.WorkUnitOpen || v.Status == domain.WorkUnitClosed) &&
		v.ActiveRequests <= v.Sequence && v.PendingAttempts <= v.Sequence && v.Unknown <= v.Sequence &&
		!v.Estimated.greater(v.Known) && !v.Known.greater(cohortAmount{hi, lo})
}

func (v cohortVersion) row() (GovernanceCohortRow, error) {
	if v.Known.Hi != 0 || v.Known.Lo > math.MaxInt64 || v.Estimated.Hi != 0 || v.Estimated.Lo > math.MaxInt64 || v.Unknown > math.MaxInt64 {
		return GovernanceCohortRow{}, ErrGovernanceSummaryOverflow
	}
	return GovernanceCohortRow{WorkUnitID: v.WorkUnitID, Status: v.Status,
		Inflight:           v.ActiveRequests != 0 || v.PendingAttempts != 0,
		KnownCostMicrosUSD: int64(v.Known.Lo), EstimatedCostMicrosUSD: int64(v.Estimated.Lo), UnknownAttempts: int64(v.Unknown)}, nil
}

func (s *State) VisitGovernanceCohort(ctx context.Context, q GovernanceCohortQuery, visit func(GovernanceCohortRow) error) (Watermark, error) {
	if err := ctx.Err(); err != nil {
		return Watermark{}, err
	}
	if err := validateGovernanceCohort(&q, visit); err != nil {
		return Watermark{}, err
	}
	if s == nil {
		return Watermark{}, ErrProjectionUnavailable
	}
	// Legacy fallback retains at most 100000 compact rows with domain-bounded
	// IDs. It scans costs under one lock, without cloning historical attempts.
	// Its historical CPU and the full resident State remain migration work.
	var rows []GovernanceCohortRow
	var head Watermark
	err := func() error {
		s.mu.RLock()
		defer s.mu.RUnlock()
		head = s.watermark
		for _, w := range s.workUnits {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !cohortEligible(w, q) {
				continue
			}
			if len(rows) >= q.MaxWorkUnits {
				return ErrGovernanceCohortLimit
			}
			v := cohortVersion{WorkUnitID: w.ID, Status: w.Status, ActiveRequests: uint64(len(s.activeByWorkUnit[w.ID])), PendingAttempts: uint64(len(s.pendingByWorkUnit[w.ID]))}
			for _, id := range s.settledByWorkUnit[w.ID] {
				if err := ctx.Err(); err != nil {
					return err
				}
				a, ok := s.settled[id]
				if !ok || a.Settlement.WorkUnitID != w.ID {
					return ErrProjectionCorrupt
				}
				if a.CostKnown {
					var err error
					v.Known, err = v.Known.add(a.CostMicrosUSD)
					if err != nil {
						return err
					}
					if a.Settlement.CostEstimated {
						v.Estimated, err = v.Estimated.add(a.CostMicrosUSD)
						if err != nil {
							return err
						}
					}
				} else {
					v.Unknown++
				}
			}
			row, err := v.row()
			if err != nil {
				return err
			}
			rows = append(rows, row)
		}
		return ctx.Err()
	}()
	if err != nil {
		return Watermark{}, err
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].WorkUnitID < rows[j].WorkUnitID })
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return Watermark{}, err
		}
		if err := visit(row); err != nil {
			return Watermark{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Watermark{}, err
	}
	return head, nil
}

func cohortVersionKey(id string, sequence uint64) []byte {
	return binary.BigEndian.AppendUint64(projectionIDKey(id), sequence)
}

// Lookup only the latest immutable version at/before cut. No historical scan.
func cohortVersionRaw(tx *bbolt.Tx, id string, cut uint64) ([]byte, uint64, error) {
	return cohortVersionRowsRaw(boltProjectionRows{tx}, id, cut)
}
func readCohortVersion(tx *bbolt.Tx, id string, sequence uint64) (cohortVersion, error) {
	return readCohortRowsVersion(boltProjectionRows{tx}, id, sequence)
}

func cohortVersionRowsRaw(tx projectionRows, id string, cut uint64) ([]byte, uint64, error) {
	target := cohortVersionKey(id, cut)
	k, raw, found, err := tx.floor("cohort_versions", target)
	if err != nil {
		return nil, 0, err
	}
	if !found {
		return nil, 0, ErrProjectionCorrupt
	}
	if len(k) != 40 || !bytes.HasPrefix(k, target[:32]) || len(raw) == 0 || len(raw) > projectionMaxValue {
		return nil, 0, ErrProjectionCorrupt
	}
	sequence := binary.BigEndian.Uint64(k[32:])
	if sequence == 0 || sequence > cut {
		return nil, 0, ErrProjectionCorrupt
	}
	return raw, sequence, nil
}

func readCohortRowsVersion(tx projectionRows, id string, sequence uint64) (cohortVersion, error) {
	v, ok, err := projectionRowsGet[cohortVersion](tx, "cohort_versions", cohortVersionKey(id, sequence))
	if err != nil || !ok || !v.valid(id, sequence) || v.Sequence != sequence {
		return cohortVersion{}, errors.Join(ErrProjectionCorrupt, err)
	}
	return v, nil
}

func projectionUpdateCohort(tx projectionRows, s *State, record Record, request *ProjectedRequest) error {
	e := record.Event
	if e.WorkUnitID == "" {
		return nil
	}
	changed := e.Kind == EventWorkUnitCreated || e.Kind == EventWorkUnitClosed || e.Kind == EventReservationCreated || e.Kind == EventAttemptSettled ||
		e.Kind == EventRequestAccepted && e.RunID != "" && request == nil ||
		e.Kind == EventRequestFinalized && request != nil && request.Finalized == nil && request.Accepted.Event.RunID != ""
	if !changed {
		return nil
	}
	var v cohortVersion
	if e.Kind == EventWorkUnitCreated {
		key := projectionListKey(e.ProjectID, e.WorkUnitID)
		_, found, err := tx.get("cohort_units", key)
		if err != nil {
			return err
		}
		if found {
			return ErrProjectionCorrupt
		}
		w, ok := s.workUnits[e.WorkUnitID]
		if !ok || w.Validate() != nil {
			return ErrProjectionCorrupt
		}
		if err := projectionRowsPut(tx, "cohort_units", key, cohortIdentity{WorkUnit: w, Sequence: record.Sequence}); err != nil {
			return err
		}
		v = cohortVersion{WorkUnitID: e.WorkUnitID, Status: domain.WorkUnitOpen}
	} else {
		_, seq, err := cohortVersionRowsRaw(tx, e.WorkUnitID, record.Sequence-1)
		if err != nil {
			return err
		}
		v, err = readCohortRowsVersion(tx, e.WorkUnitID, seq)
		if err != nil {
			return err
		}
	}
	switch e.Kind {
	case EventWorkUnitClosed:
		v.Status = domain.WorkUnitClosed
	case EventRequestAccepted:
		v.ActiveRequests++
	case EventRequestFinalized:
		if v.ActiveRequests == 0 {
			return ErrProjectionCorrupt
		}
		v.ActiveRequests--
	case EventReservationCreated:
		v.PendingAttempts++
	case EventAttemptSettled:
		if v.PendingAttempts == 0 {
			return ErrProjectionCorrupt
		}
		v.PendingAttempts--
		a, ok := s.settled[e.AttemptID]
		if !ok {
			return ErrProjectionCorrupt
		}
		if a.CostKnown {
			var err error
			v.Known, err = v.Known.add(a.CostMicrosUSD)
			if err != nil {
				return err
			}
			if a.Settlement.CostEstimated {
				v.Estimated, err = v.Estimated.add(a.CostMicrosUSD)
				if err != nil {
					return err
				}
			}
		} else {
			v.Unknown++
		}
	}
	v.Sequence = record.Sequence
	if !v.valid(e.WorkUnitID, record.Sequence) {
		return ErrProjectionCorrupt
	}
	return projectionRowsPut(tx, "cohort_versions", cohortVersionKey(e.WorkUnitID, record.Sequence), v)
}

func (p *LedgerProjection) VisitGovernanceCohort(ctx context.Context, q GovernanceCohortQuery, visit func(GovernanceCohortRow) error) (Watermark, error) {
	if err := validateGovernanceCohort(&q, visit); err != nil {
		return Watermark{}, err
	}
	var head Watermark
	var generation [16]byte
	if err := p.governanceCapacityView(ctx, func(tx *bbolt.Tx) error {
		pos, ok, err := projectionGet[projectionPosition](tx, "meta", []byte("position"))
		if err != nil || !ok {
			return errors.Join(ErrProjectionCorrupt, err)
		}
		head, generation = pos.Watermark, p.generation
		return nil
	}); err != nil {
		return Watermark{}, err
	}
	after, eligible := "", 0
	for {
		var rows []GovernanceCohortRow
		last, done := after, true
		err := p.governanceCapacityView(ctx, func(tx *bbolt.Tx) error {
			pos, ok, err := projectionGet[projectionPosition](tx, "meta", []byte("position"))
			if err != nil || !ok || generation != p.generation || head.After(pos.Watermark) || pos.Watermark.Sequence == head.Sequence && pos.Watermark != head {
				return errors.Join(ErrProjectionCorrupt, err)
			}
			b := tx.Bucket([]byte("cohort_units"))
			if b == nil || tx.Bucket([]byte("cohort_versions")) == nil {
				return ErrProjectionCorrupt
			}
			prefix := projectionTuple(q.ProjectID)
			c := b.Cursor()
			k, raw := c.Seek(projectionListKey(q.ProjectID, after))
			if after != "" && bytes.Equal(k, projectionListKey(q.ProjectID, after)) {
				k, raw = c.Next()
			}
			readBytes, scanned := 0, 0
			for ; k != nil && bytes.HasPrefix(k, prefix); k, raw = c.Next() {
				if err := ctx.Err(); err != nil {
					return err
				}
				if scanned == q.Limits.Rows {
					done = false
					return nil
				}
				if len(k) <= 32 || len(k) > 32+domain.MaxGovernanceIDLength || len(raw) == 0 || len(raw) > projectionMaxValue {
					return ErrProjectionCorrupt
				}
				if len(raw) > q.Limits.Bytes-readBytes {
					if scanned == 0 {
						return ErrProjectionPageLimit
					}
					done = false
					return nil
				}
				id := string(k[32:])
				identity, ok, err := projectionGet[cohortIdentity](tx, "cohort_units", k)
				if err != nil || !ok || identity.Sequence == 0 || identity.Sequence > pos.Watermark.Sequence || identity.WorkUnit.ID != id || identity.WorkUnit.ProjectID != q.ProjectID || identity.WorkUnit.Status != domain.WorkUnitOpen || identity.WorkUnit.Validate() != nil {
					return errors.Join(ErrProjectionCorrupt, err)
				}
				rowBytes := len(raw)
				if identity.Sequence <= head.Sequence && cohortEligible(identity.WorkUnit, q) {
					versionRaw, seq, err := cohortVersionRaw(tx, id, head.Sequence)
					if err != nil || seq < identity.Sequence {
						return errors.Join(ErrProjectionCorrupt, err)
					}
					if len(versionRaw) > q.Limits.Bytes-readBytes-rowBytes {
						if scanned == 0 {
							return ErrProjectionPageLimit
						}
						done = false
						return nil
					}
					rowBytes += len(versionRaw)
					v, err := readCohortVersion(tx, id, seq)
					if err != nil {
						return err
					}
					row, err := v.row()
					if err != nil {
						return err
					}
					if eligible >= q.MaxWorkUnits {
						return ErrGovernanceCohortLimit
					}
					eligible++
					rows = append(rows, row)
				}
				readBytes += rowBytes
				scanned++
				last = id
			}
			return nil
		})
		if err != nil {
			return Watermark{}, err
		}
		for _, row := range rows {
			if err := ctx.Err(); err != nil {
				return Watermark{}, err
			}
			if err := visit(row); err != nil {
				return Watermark{}, err
			}
		}
		if err := ctx.Err(); err != nil {
			return Watermark{}, err
		}
		if done {
			return head, nil
		}
		after = last
	}
}
