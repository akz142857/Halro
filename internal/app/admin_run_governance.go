package app

import (
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/go-chi/chi/v5"
)

func governanceSaturatingAdd(left, right int64) int64 {
	if right > 0 && left > math.MaxInt64-right {
		return math.MaxInt64
	}
	return left + right
}

type adminWorkUnitView struct {
	domain.WorkUnit
	RunCount           int   `json:"run_count"`
	CommittedMicrosUSD int64 `json:"committed_micros_usd"`
	ReservedMicrosUSD  int64 `json:"reserved_micros_usd"`
	UnknownAttempts    int64 `json:"unknown_attempts"`
}

func adminWorkUnitViewOf(item ledger.WorkUnitView) adminWorkUnitView {
	view := adminWorkUnitView{WorkUnit: item.WorkUnit}
	for _, run := range item.Runs {
		view.RunCount++
		view.CommittedMicrosUSD = governanceSaturatingAdd(view.CommittedMicrosUSD, run.CommittedMicrosUSD)
		view.ReservedMicrosUSD = governanceSaturatingAdd(view.ReservedMicrosUSD, run.ReservedMicrosUSD)
		view.UnknownAttempts = governanceSaturatingAdd(view.UnknownAttempts, run.UnknownAttempts)
	}
	return view
}

func (r *Runtime) listAdminWorkUnits(writer http.ResponseWriter, request *http.Request) {
	allowed := map[string]struct{}{"cursor": {}, "limit": {}, "project_id": {}, "status": {}}
	if !allowQueryKeys(writer, request, allowed) {
		return
	}
	projectID := strings.TrimSpace(request.URL.Query().Get("project_id"))
	status := domain.WorkUnitStatus(strings.TrimSpace(request.URL.Query().Get("status")))
	if status != "" && status != domain.WorkUnitOpen && status != domain.WorkUnitClosed {
		adminBadRequest(writer, "invalid Work Unit status")
		return
	}
	limit, cursor, ok := governanceAdminPageRequest(writer, request)
	if !ok {
		return
	}
	views := make([]adminWorkUnitView, 0, limit+1)
	err := r.state.VisitWorkUnits(ledger.ArchiveQuery{ProjectID: projectID, After: cursor}, func(item ledger.WorkUnitView) (bool, error) {
		if status == "" || item.WorkUnit.Status == status {
			views = append(views, adminWorkUnitViewOf(item))
		}
		return len(views) <= limit, request.Context().Err()
	})
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "accounting governance view is unavailable", "code": "run_governance_unavailable"})
		return
	}
	writeGovernanceAdminVisitedPage(writer, views, limit, func(item adminWorkUnitView) string { return item.ID })
}

func (r *Runtime) getAdminWorkUnit(writer http.ResponseWriter, request *http.Request) {
	if !allowQueryKeys(writer, request, map[string]struct{}{}) {
		return
	}
	item, ok, err := r.state.WorkUnitView(chi.URLParam(request, "workUnitID"))
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "accounting governance view is unavailable", "code": "run_governance_unavailable"})
		return
	}
	if !ok {
		adminNotFound(writer)
		return
	}
	writeJSON(writer, http.StatusOK, adminWorkUnitViewOf(item))
}

func (r *Runtime) listAdminRuns(writer http.ResponseWriter, request *http.Request) {
	allowed := map[string]struct{}{"cursor": {}, "limit": {}, "project_id": {}, "work_unit_id": {}, "status": {}}
	if !allowQueryKeys(writer, request, allowed) {
		return
	}
	projectID := strings.TrimSpace(request.URL.Query().Get("project_id"))
	workUnitID := strings.TrimSpace(request.URL.Query().Get("work_unit_id"))
	status := domain.RunStatus(strings.TrimSpace(request.URL.Query().Get("status")))
	if status != "" && status != domain.RunActive && status != domain.RunClosed && status != domain.RunExpired {
		adminBadRequest(writer, "invalid Run status")
		return
	}
	limit, cursor, ok := governanceAdminPageRequest(writer, request)
	if !ok {
		return
	}
	now := r.clockNow()
	items := make([]domain.Run, 0, limit+1)
	collect := func(run domain.Run) (bool, error) {
		if workUnitID != "" && run.WorkUnitID != workUnitID || projectID != "" && run.ProjectID != projectID {
			return true, nil
		}
		run = r.accounting.WithBudgetState(run)
		run.Status = domain.EffectiveRunStatus(run, now)
		if status == "" || run.Status == status {
			items = append(items, run)
		}
		return len(items) <= limit, request.Context().Err()
	}
	var err error
	if workUnitID != "" {
		// A Work Unit has at most domain.MaxRunsPerWorkUnit Runs, so they
		// are read with it rather than by walking the project's.
		var view ledger.WorkUnitView
		var found bool
		view, found, err = r.state.WorkUnitView(workUnitID)
		if err == nil && found {
			runs := slices.Clone(view.Runs)
			sort.Slice(runs, func(i, j int) bool { return runs[i].ID < runs[j].ID })
			for _, run := range runs {
				if run.ID <= cursor {
					continue
				}
				more, visitErr := collect(run)
				if err = visitErr; err != nil || !more {
					break
				}
			}
		}
	} else {
		err = r.state.VisitRuns(projectID, cursor, collect)
	}
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "accounting governance view is unavailable", "code": "run_governance_unavailable"})
		return
	}
	writeGovernanceAdminVisitedPage(writer, items, limit, func(item domain.Run) string { return item.ID })
}

func (r *Runtime) getAdminRun(writer http.ResponseWriter, request *http.Request) {
	if !allowQueryKeys(writer, request, map[string]struct{}{}) {
		return
	}
	item, ok, err := r.accounting.Run("", chi.URLParam(request, "runID"))
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "accounting governance view is unavailable", "code": "run_governance_unavailable"})
		return
	}
	if !ok {
		adminNotFound(writer)
		return
	}
	item.Status = domain.EffectiveRunStatus(item, r.clockNow())
	writeJSON(writer, http.StatusOK, item)
}

func allowQueryKeys(writer http.ResponseWriter, request *http.Request, allowed map[string]struct{}) bool {
	for key := range request.URL.Query() {
		if _, ok := allowed[key]; !ok {
			adminBadRequest(writer, "unsupported query parameter: "+key)
			return false
		}
	}
	return true
}

// governanceAdminPageRequest reads a page's limit and cursor.
func governanceAdminPageRequest(writer http.ResponseWriter, request *http.Request) (int, string, bool) {
	limit := 50
	if raw := request.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 200 {
			adminBadRequest(writer, "page limit must be between 1 and 200")
			return 0, "", false
		}
		limit = value
	}
	cursor, err := decodeResourceCursor(request.URL.Query().Get("cursor"))
	if err != nil {
		adminBadRequest(writer, "invalid cursor")
		return 0, "", false
	}
	return limit, cursor, true
}

// writeGovernanceAdminVisitedPage writes a page from items visited in ID
// order after the cursor, at most limit+1 of them: the extra one only says
// that another page follows.
func writeGovernanceAdminVisitedPage[T any](writer http.ResponseWriter, items []T, limit int, id func(T) string) {
	if items == nil {
		items = make([]T, 0)
	}
	nextCursor := ""
	if len(items) > limit {
		items = items[:limit]
		nextCursor = encodeResourceCursor(id(items[limit-1]))
	}
	writeJSON(writer, http.StatusOK, map[string]any{"items": items, "next_cursor": nextCursor})
}

// writeGovernanceAdminPage pages a list already held in full, sorted by ID.
func writeGovernanceAdminPage[T any](writer http.ResponseWriter, request *http.Request, items []T, id func(T) string) {
	limit, cursor, ok := governanceAdminPageRequest(writer, request)
	if !ok {
		return
	}
	start := 0
	for start < len(items) && cursor != "" && id(items[start]) <= cursor {
		start++
	}
	end := min(start+limit+1, len(items))
	writeGovernanceAdminVisitedPage(writer, items[start:end], limit, id)
}
