package ledger

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

var ErrGovernanceExportClosed = errors.New("accounting export view is closed")
var ErrGovernanceExportIncomplete = errors.New("accounting export facts were not completed")
var ErrGovernanceExportBackend = errors.New("accounting export requires a disk owner")

// These normalized rows deliberately expose no additive settlement/balance
// fields. They preserve the schema-1 export fields at an independent Ledger cut.
type GovernanceExportWorkUnit struct {
	ID                 string                        `json:"id"`
	ProjectID          string                        `json:"project_id"`
	Status             domain.WorkUnitStatus         `json:"status"`
	CreatedAt          time.Time                     `json:"created_at"`
	ClosedAt           *time.Time                    `json:"closed_at,omitempty"`
	OutcomeDefinitions []domain.OutcomeDefinitionRef `json:"outcome_definitions"`
}
type GovernanceExportRun struct {
	ID              string           `json:"id"`
	ProjectID       string           `json:"project_id"`
	WorkUnitID      string           `json:"work_unit_id"`
	BudgetMicrosUSD int64            `json:"budget_micros_usd"`
	Status          domain.RunStatus `json:"status"`
	CreatedAt       time.Time        `json:"created_at"`
	ExpiresAt       time.Time        `json:"expires_at"`
	ClosedAt        *time.Time       `json:"closed_at,omitempty"`
}

type GovernanceExportView interface {
	Head() Watermark
	ReadWorkUnitPage(context.Context, GovernanceListOptions) (GovernanceListPage[GovernanceExportWorkUnit], error)
	ReadRunPage(context.Context, GovernanceListOptions) (GovernanceListPage[GovernanceExportRun], error)
	ReadMaturityPage(context.Context, []WorkUnitReference) (WorkUnitMaturityPage, error)
	Complete(context.Context) error
}
type AccountingExportReader interface {
	WithGovernanceExport(context.Context, func(GovernanceExportView) error) error
}

// Complete must succeed AFTER all facts have been read and BEFORE publication.
// It joins finite reads, checks owner/generation, seals and clears references.
// Callbacks run outside all DB transactions; the accounting Manager retains its
// operation ticket through the entire callback, including file publication.
func (p *LedgerProjection) WithGovernanceExport(ctx context.Context, visit func(GovernanceExportView) error) error {
	if visit == nil {
		return ErrGovernanceExportIncomplete
	}
	v := &projectionExportView{p: p, operationCtx: ctx, active: true}
	if err := p.governanceCapacityView(ctx, func(tx *bbolt.Tx) error {
		pos, ok, err := projectionGet[projectionPosition](tx, "meta", []byte("position"))
		if err != nil || !ok {
			return errors.Join(ErrProjectionCorrupt, err)
		}
		v.head, v.hash, v.generation = pos.Watermark, pos.Hash, p.generation
		for _, list := range []*exportProgress{&v.units, &v.runs} {
			name := "export_work_unit_count"
			if list == &v.runs {
				name = "export_run_count"
			}
			if raw := tx.Bucket([]byte("meta")).Get([]byte(name)); len(raw) == 0 || len(raw) > 20 {
				return ErrProjectionCorrupt
			}
			count, found, err := projectionGet[uint64](tx, "meta", []byte(name))
			if err != nil || !found || count > v.head.Sequence {
				return errors.Join(ErrProjectionCorrupt, err)
			}
			list.expected = count
		}
		return nil
	}); err != nil {
		return err
	}
	defer v.close()
	if err := visit(v); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.completed {
		return errors.Join(ErrGovernanceExportIncomplete, v.failure)
	}
	return nil
}

type exportProgress struct {
	next           string
	seen, expected uint64
	done           bool
}

type projectionExportView struct {
	mu                sync.Mutex
	p                 *LedgerProjection
	operationCtx      context.Context
	head              Watermark
	hash              [32]byte
	generation        [16]byte
	active, completed bool
	failure           error
	units, runs       exportProgress
	// A valid source-byte overflow must be completely retried before sealing.
	// While pending, only subsets of this bounded batch may be queried.
	pendingMaturity map[WorkUnitReference]struct{}
}

func (v *projectionExportView) Head() Watermark { return v.head }
func (v *projectionExportView) close() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.active = false
	v.p = nil
	v.operationCtx = nil
	v.pendingMaturity = nil
}
func (v *projectionExportView) read(ctx context.Context, fn func(*bbolt.Tx, projectionPosition) error) error {
	if !v.active || v.p == nil {
		return ErrGovernanceExportClosed
	}
	if v.failure != nil {
		return v.failure
	}
	err := v.operationCtx.Err()
	if err == nil {
		err = v.p.governanceCapacityView(ctx, func(tx *bbolt.Tx) error {
			if err := v.operationCtx.Err(); err != nil {
				return err
			}
			pos, ok, err := projectionGet[projectionPosition](tx, "meta", []byte("position"))
			if err != nil || !ok {
				return errors.Join(ErrProjectionCorrupt, err)
			}
			// Maintenance/generation changes revoke the cut; ordinary same-generation
			// appends may proceed. Same-sequence source positions/hash cannot change.
			if v.generation != v.p.generation || pos.Watermark.Generation != v.head.Generation || v.head.After(pos.Watermark) || pos.Watermark.Sequence == v.head.Sequence && (pos.Watermark != v.head || pos.Hash != v.hash) {
				return ErrProjectionUnavailable
			}
			if err := fn(tx, pos); err != nil {
				return err
			}
			return v.operationCtx.Err()
		})
	}
	// Byte-limit errors permit retrying a smaller page/batch at the same cut.
	// Source/cancellation errors poison completion even if a caller ignores them.
	if err != nil && !errors.Is(err, ErrProjectionPageLimit) {
		v.failure = err
	}
	return err
}
func (v *projectionExportView) Complete(ctx context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.active {
		return ErrGovernanceExportClosed
	}
	err := v.read(ctx, func(*bbolt.Tx, projectionPosition) error {
		if !v.units.done || !v.runs.done || len(v.pendingMaturity) != 0 {
			return ErrGovernanceExportIncomplete
		}
		return nil
	})
	v.active = false
	v.p = nil
	v.operationCtx = nil
	v.pendingMaturity = nil
	v.completed = err == nil
	return err
}

// Lifecycle positions are derived atomically alongside rows and the head. Each
// object creates once and closes once. Idempotent aliases have no new effect.
type governanceExportPosition struct {
	ID              string
	Created, Closed uint64
}

func updateGovernanceExportPositions(tx projectionRows, s *State, r Record) error {
	e := r.Event
	bucket, id, created := "", "", false
	switch e.Kind {
	case EventWorkUnitCreated:
		bucket, id, created = "export_work_units", e.WorkUnitID, true
	case EventWorkUnitClosed:
		bucket, id = "export_work_units", e.WorkUnitID
	case EventRunCreated:
		bucket, id, created = "export_runs", e.RunID, true
	case EventRunClosed:
		bucket, id = "export_runs", e.RunID
	default:
		return nil
	}
	pos, found, err := projectionRowsGet[governanceExportPosition](tx, bucket, []byte(id))
	if err != nil {
		return err
	}
	if created {
		if found {
			return ErrProjectionCorrupt
		}
		pos = governanceExportPosition{ID: id, Created: r.Sequence}
	} else {
		if !found || pos.ID != id || pos.Created == 0 || pos.Created >= r.Sequence || pos.Closed != 0 {
			return ErrProjectionCorrupt
		}
		pos.Closed = r.Sequence
	}
	// The reducer has already verified the lifecycle transition. Check the
	// resulting row while the same transaction still owns all mutations.
	if bucket == "export_work_units" {
		row, ok := s.workUnits[id]
		if !ok || row.ID != id || (row.Status == domain.WorkUnitClosed) != (pos.Closed != 0) {
			return ErrProjectionCorrupt
		}
	} else {
		row, ok := s.runs[id]
		if !ok || row.ID != id || (row.Status == domain.RunClosed) != (pos.Closed != 0) {
			return ErrProjectionCorrupt
		}
	}
	if created {
		name := "export_work_unit_count"
		if bucket == "export_runs" {
			name = "export_run_count"
		}
		raw, ok, err := tx.get("meta", []byte(name))
		if err != nil {
			return err
		}
		if !ok || len(raw) == 0 || len(raw) > 20 {
			return ErrProjectionCorrupt
		}
		count, found, err := projectionRowsGet[uint64](tx, "meta", []byte(name))
		if err != nil || !found || count >= r.Sequence {
			return errors.Join(ErrProjectionCorrupt, err)
		}
		if err := projectionRowsPut(tx, "meta", []byte(name), count+1); err != nil {
			return err
		}
	}
	return projectionRowsPut(tx, bucket, []byte(id), pos)
}
func readExportPosition(tx *bbolt.Tx, bucket, id string, current uint64) (governanceExportPosition, error) {
	raw := tx.Bucket([]byte(bucket)).Get([]byte(id))
	if len(raw) == 0 || len(raw) > 256 {
		return governanceExportPosition{}, ErrProjectionCorrupt
	}
	pos, ok, err := projectionGet[governanceExportPosition](tx, bucket, []byte(id))
	if err != nil || !ok || pos.ID != id || pos.Created == 0 || pos.Created > current || pos.Closed != 0 && (pos.Closed <= pos.Created || pos.Closed > current) {
		return governanceExportPosition{}, errors.Join(ErrProjectionCorrupt, err)
	}
	return pos, nil
}
func exportPage[T any](ctx context.Context, v *projectionExportView, o GovernanceListOptions, index, bucket string, validID func(string) bool, decode func(*bbolt.Tx, string, governanceExportPosition, projectionPosition, *int) (T, error)) (GovernanceListPage[T], error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.active {
		return GovernanceListPage[T]{}, ErrGovernanceExportClosed
	}
	if err := validateGovernanceList(o); err != nil {
		return GovernanceListPage[T]{}, err
	}
	progress := &v.units
	if index == "runs_list" {
		progress = &v.runs
	}
	if progress.done || o.After != progress.next {
		return GovernanceListPage[T]{}, ErrProjectionPageLimit
	}
	page := GovernanceListPage[T]{Items: make([]T, 0)}
	err := v.read(ctx, func(tx *bbolt.Tx, current projectionPosition) error {
		b := tx.Bucket([]byte(index))
		positions := tx.Bucket([]byte(bucket))
		if b == nil || positions == nil {
			return ErrProjectionCorrupt
		}
		if progress.seen == progress.expected {
			return nil
		}
		c := b.Cursor()
		k, value := c.Seek([]byte(o.After))
		if o.After != "" && bytes.Equal(k, []byte(o.After)) {
			k, value = c.Next()
		}
		last, sourceBytes := "", 0
		for ; k != nil; k, value = c.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			if page.Scanned == o.Limits.Rows {
				page.NextID = last
				return nil
			}
			if len(k) > domain.MaxGovernanceIDLength || !bytes.Equal(k, value) || !validID(string(k)) {
				return ErrProjectionCorrupt
			}
			id := string(k)
			raw := positions.Get(k)
			if len(raw) == 0 || len(raw) > 256 {
				return ErrProjectionCorrupt
			}
			if len(raw) > o.Limits.Bytes-sourceBytes {
				if page.Scanned == 0 {
					return ErrProjectionPageLimit
				}
				page.NextID = last
				return nil
			}
			// Commit source/scan progress only after a whole row fits. An excluded
			// future creation still counts against the physical page budget.
			before := sourceBytes
			sourceBytes += len(raw)
			pos, err := readExportPosition(tx, bucket, id, current.Watermark.Sequence)
			if err != nil {
				return err
			}
			if pos.Created <= v.head.Sequence {
				row, err := decode(tx, id, pos, current, &sourceBytes)
				if errors.Is(err, ErrProjectionPageLimit) && page.Scanned > 0 {
					sourceBytes = before
					page.NextID = last
					return nil
				}
				if err != nil {
					return err
				}
				page.Items = append(page.Items, row)
			}
			page.Scanned++
			last = id
			page.Bytes = sourceBytes
		}
		return nil
	})
	if err != nil {
		return GovernanceListPage[T]{}, err
	}
	seen := progress.seen + uint64(len(page.Items))
	if seen > progress.expected || page.NextID == "" && seen != progress.expected {
		v.failure = ErrProjectionCorrupt
		return GovernanceListPage[T]{}, v.failure
	}
	if seen == progress.expected {
		page.NextID = ""
	}
	progress.seen, progress.next, progress.done = seen, page.NextID, page.NextID == ""
	return page, nil
}
func exportSourceBytes(raw []byte, used *int, limit int) error {
	if len(raw) == 0 || len(raw) > projectionMaxValue {
		return ErrProjectionCorrupt
	}
	if len(raw) > limit-*used {
		return ErrProjectionPageLimit
	}
	*used += len(raw)
	return nil
}
func exportWorkUnit(tx *bbolt.Tx, id string, pos governanceExportPosition, cut, current uint64, used *int, limit int) (domain.WorkUnit, cohortVersion, error) {
	b := tx.Bucket([]byte("work_units"))
	if b == nil {
		return domain.WorkUnit{}, cohortVersion{}, ErrProjectionCorrupt
	}
	if err := exportSourceBytes(b.Get(projectionIDKey(id)), used, limit); err != nil {
		return domain.WorkUnit{}, cohortVersion{}, err
	}
	w, ok, err := projectionGet[domain.WorkUnit](tx, "work_units", projectionIDKey(id))
	if err != nil || !ok || w.ID != id || w.Validate() != nil || (w.Status == domain.WorkUnitClosed) != (pos.Closed != 0) {
		return domain.WorkUnit{}, cohortVersion{}, errors.Join(ErrProjectionCorrupt, err)
	}
	raw, seq, err := cohortVersionRaw(tx, id, cut)
	if err != nil || seq < pos.Created {
		return domain.WorkUnit{}, cohortVersion{}, errors.Join(ErrProjectionCorrupt, err)
	}
	if err := exportSourceBytes(raw, used, limit); err != nil {
		return domain.WorkUnit{}, cohortVersion{}, err
	}
	version, err := readCohortVersion(tx, id, seq)
	if err != nil {
		return domain.WorkUnit{}, cohortVersion{}, err
	}
	if pos.Closed == 0 || pos.Closed > cut {
		w.Status = domain.WorkUnitOpen
		w.ClosedAt = nil
	}
	if version.Status != w.Status {
		return domain.WorkUnit{}, cohortVersion{}, ErrProjectionCorrupt
	}
	return w, version, nil
}
func (v *projectionExportView) ReadWorkUnitPage(ctx context.Context, o GovernanceListOptions) (GovernanceListPage[GovernanceExportWorkUnit], error) {
	return exportPage(ctx, v, o, "work_units_list", "export_work_units", domain.ValidWorkUnitID, func(tx *bbolt.Tx, id string, pos governanceExportPosition, current projectionPosition, used *int) (GovernanceExportWorkUnit, error) {
		w, _, err := exportWorkUnit(tx, id, pos, v.head.Sequence, current.Watermark.Sequence, used, o.Limits.Bytes)
		if err != nil {
			return GovernanceExportWorkUnit{}, err
		}
		return GovernanceExportWorkUnit{w.ID, w.ProjectID, w.Status, w.CreatedAt, w.ClosedAt, w.OutcomeDefinitions}, nil
	})
}
func (v *projectionExportView) ReadRunPage(ctx context.Context, o GovernanceListOptions) (GovernanceListPage[GovernanceExportRun], error) {
	return exportPage(ctx, v, o, "runs_list", "export_runs", domain.ValidRunID, func(tx *bbolt.Tx, id string, pos governanceExportPosition, current projectionPosition, used *int) (GovernanceExportRun, error) {
		b := tx.Bucket([]byte("runs"))
		if b == nil {
			return GovernanceExportRun{}, ErrProjectionCorrupt
		}
		if err := exportSourceBytes(b.Get(projectionIDKey(id)), used, o.Limits.Bytes); err != nil {
			return GovernanceExportRun{}, err
		}
		row, ok, err := projectionGet[domain.Run](tx, "runs", projectionIDKey(id))
		if err != nil || !ok || row.ID != id || row.Validate() != nil || (row.Status == domain.RunClosed) != (pos.Closed != 0) {
			return GovernanceExportRun{}, errors.Join(ErrProjectionCorrupt, err)
		}
		if pos.Closed == 0 || pos.Closed > v.head.Sequence {
			row.Status = domain.RunActive
			row.ClosedAt = nil
		}
		return GovernanceExportRun{row.ID, row.ProjectID, row.WorkUnitID, row.BudgetMicrosUSD, row.Status, row.CreatedAt, row.ExpiresAt, row.ClosedAt}, nil
	})
}
func (v *projectionExportView) ReadMaturityPage(ctx context.Context, refs []WorkUnitReference) (WorkUnitMaturityPage, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.active {
		return WorkUnitMaturityPage{}, ErrGovernanceExportClosed
	}
	if v.failure != nil {
		return WorkUnitMaturityPage{}, v.failure
	}
	if err := validateMaturityReferences(refs); err != nil {
		// Invalid identities/counts cannot be retained as bounded retry state.
		v.failure = err
		return WorkUnitMaturityPage{}, err
	}
	if len(v.pendingMaturity) != 0 {
		for _, ref := range refs {
			if _, ok := v.pendingMaturity[ref]; !ok {
				v.failure = ErrGovernanceExportIncomplete
				return WorkUnitMaturityPage{}, v.failure
			}
		}
	}
	result := WorkUnitMaturityPage{Items: make([]WorkUnitMaturity, 0, len(refs)), Watermark: v.head}
	err := v.read(ctx, func(tx *bbolt.Tx, current projectionPosition) error {
		positions := tx.Bucket([]byte("export_work_units"))
		if positions == nil {
			return ErrProjectionCorrupt
		}
		used := 0
		cache := make(map[WorkUnitReference]WorkUnitMaturity, len(refs))
		for _, ref := range refs {
			if err := ctx.Err(); err != nil {
				return err
			}
			if row, ok := cache[ref]; ok {
				result.Items = append(result.Items, row)
				continue
			}
			row := WorkUnitMaturity{Reference: ref}
			raw := positions.Get([]byte(ref.WorkUnitID))
			if raw != nil {
				if len(raw) > 256 {
					return ErrProjectionCorrupt
				}
				if len(raw) > GovernanceListMaxBytes-used {
					return ErrProjectionPageLimit
				}
				used += len(raw)
				pos, err := readExportPosition(tx, "export_work_units", ref.WorkUnitID, current.Watermark.Sequence)
				if err != nil {
					return err
				}
				if pos.Created <= v.head.Sequence {
					w, version, err := exportWorkUnit(tx, ref.WorkUnitID, pos, v.head.Sequence, current.Watermark.Sequence, &used, GovernanceListMaxBytes)
					if err != nil {
						return err
					}
					if w.ProjectID == ref.ProjectID {
						row.Found, row.Status, row.Inflight = true, w.Status, version.ActiveRequests != 0 || version.PendingAttempts != 0
					}
				}
			} else {
				units := tx.Bucket([]byte("work_units"))
				if units == nil || units.Get(projectionIDKey(ref.WorkUnitID)) != nil {
					return ErrProjectionCorrupt
				}
			}
			cache[ref] = row
			result.Items = append(result.Items, row)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrProjectionPageLimit) && len(v.pendingMaturity) == 0 {
			v.pendingMaturity = make(map[WorkUnitReference]struct{}, len(refs))
			for _, ref := range refs {
				if _, ok := v.pendingMaturity[ref]; !ok {
					// Own only bounded identities, not a caller substring's backing
					// allocation after this failed call returns.
					owned := WorkUnitReference{strings.Clone(ref.ProjectID), strings.Clone(ref.WorkUnitID)}
					v.pendingMaturity[owned] = struct{}{}
				}
			}
		}
		return WorkUnitMaturityPage{}, err
	}
	for _, ref := range refs {
		delete(v.pendingMaturity, ref)
	}
	if len(v.pendingMaturity) == 0 {
		v.pendingMaturity = nil
	}
	return result, nil
}
