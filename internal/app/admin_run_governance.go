package app

import (
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/akz142857/Halro/internal/domain"
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

func (r *Runtime) adminWorkUnitView(item domain.WorkUnit) adminWorkUnitView {
	view := adminWorkUnitView{WorkUnit: item}
	for _, run := range r.accounting.Runs(item.ProjectID, item.ID) {
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
	items := r.accounting.WorkUnits(projectID)
	filtered := make([]domain.WorkUnit, 0, len(items))
	for _, item := range items {
		if status == "" || item.Status == status {
			filtered = append(filtered, item)
		}
	}
	views := make([]adminWorkUnitView, 0, len(filtered))
	for _, item := range filtered {
		views = append(views, r.adminWorkUnitView(item))
	}
	writeGovernanceAdminPage(writer, request, views, func(item adminWorkUnitView) string { return item.ID })
}

func (r *Runtime) getAdminWorkUnit(writer http.ResponseWriter, request *http.Request) {
	if !allowQueryKeys(writer, request, map[string]struct{}{}) {
		return
	}
	item, ok := r.accounting.WorkUnit("", chi.URLParam(request, "workUnitID"))
	if !ok {
		adminNotFound(writer)
		return
	}
	writeJSON(writer, http.StatusOK, r.adminWorkUnitView(item))
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
	items := r.accounting.Runs(projectID, workUnitID)
	filtered := make([]domain.Run, 0, len(items))
	for _, item := range items {
		item.Status = domain.EffectiveRunStatus(item, r.clockNow())
		if status == "" || item.Status == status {
			filtered = append(filtered, item)
		}
	}
	writeGovernanceAdminPage(writer, request, filtered, func(item domain.Run) string { return item.ID })
}

func (r *Runtime) getAdminRun(writer http.ResponseWriter, request *http.Request) {
	if !allowQueryKeys(writer, request, map[string]struct{}{}) {
		return
	}
	item, ok := r.accounting.Run("", chi.URLParam(request, "runID"))
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

func writeGovernanceAdminPage[T any](writer http.ResponseWriter, request *http.Request, items []T, id func(T) string) {
	limit := 50
	if raw := request.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 200 {
			adminBadRequest(writer, "page limit must be between 1 and 200")
			return
		}
		limit = value
	}
	cursor, err := decodeResourceCursor(request.URL.Query().Get("cursor"))
	if err != nil {
		adminBadRequest(writer, "invalid cursor")
		return
	}
	start := 0
	for start < len(items) && cursor != "" && id(items[start]) <= cursor {
		start++
	}
	end := start + limit
	if end > len(items) {
		end = len(items)
	}
	nextCursor := ""
	if end < len(items) && end > start {
		nextCursor = encodeResourceCursor(id(items[end-1]))
	}
	page := items[start:end]
	if page == nil {
		page = make([]T, 0)
	}
	writeJSON(writer, http.StatusOK, map[string]any{"items": page, "next_cursor": nextCursor})
}
