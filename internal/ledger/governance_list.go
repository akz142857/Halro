package ledger

import (
	"container/heap"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

const GovernanceListMaxRows = 200
const GovernanceListMaxBytes = 8 << 20

type GovernanceListOptions struct {
	After  string
	Limits ProjectionPageLimits
}
type WorkUnitListQuery struct {
	GovernanceListOptions
	ProjectID string
	Status    domain.WorkUnitStatus
}
type RunListQuery struct {
	GovernanceListOptions
	ProjectID, WorkUnitID string
	Status                domain.RunStatus
	At                    time.Time
}

// These are live ID-ordered lists, not frozen accounting/recovery snapshots.
// NextID may name a scanned, filtered row; an empty page with NextID is not EOF.
type GovernanceListPage[T any] struct {
	Items   []T
	NextID  string
	Bytes   int
	Scanned int
}

type AccountingListReader interface {
	ReadWorkUnitPage(context.Context, WorkUnitListQuery) (GovernanceListPage[domain.WorkUnit], error)
	ReadRunPage(context.Context, RunListQuery) (GovernanceListPage[domain.Run], error)
}

var _ AccountingListReader = (*State)(nil)
var _ AccountingListReader = (*LedgerProjection)(nil)

func validateGovernanceList(options GovernanceListOptions) error {
	if options.Limits.Rows < 1 || options.Limits.Rows > GovernanceListMaxRows || options.Limits.Bytes < 1 || options.Limits.Bytes > GovernanceListMaxBytes || len(options.After) > 256 || strings.ContainsRune(options.After, 0) {
		return ErrProjectionPageLimit
	}
	return nil
}
func validateWorkUnitList(q WorkUnitListQuery) error {
	if q.Status != "" && q.Status != domain.WorkUnitOpen && q.Status != domain.WorkUnitClosed {
		return ErrProjectionPageLimit
	}
	return validateGovernanceList(q.GovernanceListOptions)
}
func validateRunList(q RunListQuery) error {
	if q.Status != "" && q.Status != domain.RunActive && q.Status != domain.RunClosed && q.Status != domain.RunExpired {
		return ErrProjectionPageLimit
	}
	return validateGovernanceList(q.GovernanceListOptions)
}

// Only the earliest limit+1 matching values are retained. Replacing the heap
// root does not box/allocate one object per historical row.
type governanceListHeap[T any] struct {
	values []T
	id     func(T) string
}

func (h governanceListHeap[T]) Len() int           { return len(h.values) }
func (h governanceListHeap[T]) Less(i, j int) bool { return h.id(h.values[i]) > h.id(h.values[j]) }
func (h governanceListHeap[T]) Swap(i, j int)      { h.values[i], h.values[j] = h.values[j], h.values[i] }
func (h *governanceListHeap[T]) Push(v any)        { h.values = append(h.values, v.(T)) }
func (h *governanceListHeap[T]) Pop() any {
	i := len(h.values) - 1
	v := h.values[i]
	var zero T
	h.values[i] = zero
	h.values = h.values[:i]
	return v
}

func stateGovernancePage[T any](ctx context.Context, s *State, options GovernanceListOptions, rows map[string]T, id func(T) string, matches func(T) bool, own func(T) T) (GovernanceListPage[T], error) {
	page := GovernanceListPage[T]{Items: make([]T, 0)}
	if err := ctx.Err(); err != nil {
		return GovernanceListPage[T]{}, err
	}
	if s == nil {
		return GovernanceListPage[T]{}, ErrProjectionUnavailable
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := &governanceListHeap[T]{id: id, values: make([]T, 0, options.Limits.Rows+1)}
	for key, row := range rows {
		if err := ctx.Err(); err != nil {
			return GovernanceListPage[T]{}, err
		}
		page.Scanned++
		if key <= options.After || !matches(row) {
			continue
		}
		if h.Len() < options.Limits.Rows+1 {
			heap.Push(h, row)
		} else if key < id(h.values[0]) {
			h.values[0] = row
			heap.Fix(h, 0)
		}
	}
	slices.SortFunc(h.values, func(a, b T) int { return strings.Compare(id(a), id(b)) })
	for i, row := range h.values {
		if err := ctx.Err(); err != nil {
			return GovernanceListPage[T]{}, err
		}
		if i == options.Limits.Rows {
			page.NextID = id(page.Items[len(page.Items)-1])
			break
		}
		raw, err := json.Marshal(row)
		if err != nil {
			return GovernanceListPage[T]{}, err
		}
		if len(raw) > options.Limits.Bytes-page.Bytes {
			if len(page.Items) == 0 {
				return GovernanceListPage[T]{}, ErrProjectionPageLimit
			}
			page.NextID = id(page.Items[len(page.Items)-1])
			break
		}
		page.Items = append(page.Items, own(row))
		page.Bytes += len(raw)
	}
	if err := ctx.Err(); err != nil {
		return GovernanceListPage[T]{}, err
	}
	return page, nil
}

func ownWorkUnit(v domain.WorkUnit) domain.WorkUnit {
	v.OutcomeDefinitions = slices.Clone(v.OutcomeDefinitions)
	if v.ClosedAt != nil {
		c := *v.ClosedAt
		v.ClosedAt = &c
	}
	return v
}
func ownRun(v domain.Run) domain.Run {
	if v.ClosedAt != nil {
		c := *v.ClosedAt
		v.ClosedAt = &c
	}
	return v
}

func (s *State) ReadWorkUnitPage(ctx context.Context, q WorkUnitListQuery) (GovernanceListPage[domain.WorkUnit], error) {
	if err := validateWorkUnitList(q); err != nil {
		return GovernanceListPage[domain.WorkUnit]{}, err
	}
	if s == nil {
		return GovernanceListPage[domain.WorkUnit]{}, ErrProjectionUnavailable
	}
	return stateGovernancePage(ctx, s, q.GovernanceListOptions, s.workUnits, func(v domain.WorkUnit) string { return v.ID }, func(v domain.WorkUnit) bool {
		return (q.ProjectID == "" || v.ProjectID == q.ProjectID) && (q.Status == "" || v.Status == q.Status)
	}, ownWorkUnit)
}
func (s *State) ReadRunPage(ctx context.Context, q RunListQuery) (GovernanceListPage[domain.Run], error) {
	if err := validateRunList(q); err != nil {
		return GovernanceListPage[domain.Run]{}, err
	}
	if s == nil {
		return GovernanceListPage[domain.Run]{}, ErrProjectionUnavailable
	}
	return stateGovernancePage(ctx, s, q.GovernanceListOptions, s.runs, func(v domain.Run) string { return v.ID }, func(v domain.Run) bool {
		return (q.ProjectID == "" || v.ProjectID == q.ProjectID) && (q.WorkUnitID == "" || v.WorkUnitID == q.WorkUnitID) && (q.Status == "" || domain.EffectiveRunStatus(v, q.At) == q.Status)
	}, ownRun)
}
