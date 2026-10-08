package app

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
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

func (r *Runtime) adminWorkUnitView(ctx context.Context, item domain.WorkUnit) (adminWorkUnitView, error) {
	view := adminWorkUnitView{WorkUnit: item}
	query := ledger.RunListQuery{ProjectID: item.ProjectID, WorkUnitID: item.ID, At: r.clockNow(), GovernanceListOptions: ledger.GovernanceListOptions{Limits: ledger.ProjectionPageLimits{Rows: 32, Bytes: ledger.GovernanceListMaxBytes}}}
	for {
		page, err := r.accounting.ReadRunPage(ctx, query)
		if err != nil {
			return adminWorkUnitView{}, err
		}
		for _, run := range page.Items {
			view.RunCount++
			view.CommittedMicrosUSD = governanceSaturatingAdd(view.CommittedMicrosUSD, run.CommittedMicrosUSD)
			view.ReservedMicrosUSD = governanceSaturatingAdd(view.ReservedMicrosUSD, run.ReservedMicrosUSD)
			view.UnknownAttempts = governanceSaturatingAdd(view.UnknownAttempts, run.UnknownAttempts)
		}
		if page.NextID == "" {
			return view, nil
		}
		query.After = page.NextID
	}
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
	options, ok := readGovernanceListOptions(writer, request)
	if !ok {
		return
	}
	page, err := r.accounting.ReadWorkUnitPage(request.Context(), ledger.WorkUnitListQuery{ProjectID: projectID, Status: status, GovernanceListOptions: options})
	if err != nil {
		writeGovernanceOperationError(writer, err, "work unit")
		return
	}
	views := make([]adminWorkUnitView, 0, len(page.Items))
	size := 0
	for _, item := range page.Items {
		view, err := r.adminWorkUnitView(request.Context(), item)
		if err != nil {
			writeGovernanceOperationError(writer, err, "work unit")
			return
		}
		raw, err := json.Marshal(view)
		if err != nil {
			writeGovernanceOperationError(writer, err, "work unit")
			return
		}
		if len(raw) > options.Limits.Bytes-size {
			if len(views) == 0 {
				writeGovernanceOperationError(writer, ledger.ErrProjectionPageLimit, "work unit")
				return
			}
			page.NextID = views[len(views)-1].ID
			break
		}
		size += len(raw)
		views = append(views, view)
	}
	writeJSON(writer, http.StatusOK, map[string]any{"items": views, "next_cursor": encodeGovernanceNext(page.NextID)})
}

func (r *Runtime) getAdminWorkUnit(writer http.ResponseWriter, request *http.Request) {
	if !allowQueryKeys(writer, request, map[string]struct{}{}) {
		return
	}
	item, ok, err := r.accounting.ReadWorkUnit(request.Context(), "", chi.URLParam(request, "workUnitID"))
	if err != nil {
		writeGovernanceOperationError(writer, err, "work unit")
		return
	}
	if !ok {
		adminNotFound(writer)
		return
	}
	view, err := r.adminWorkUnitView(request.Context(), item)
	if err != nil {
		writeGovernanceOperationError(writer, err, "work unit")
		return
	}
	writeJSON(writer, http.StatusOK, view)
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
	options, ok := readGovernanceListOptions(writer, request)
	if !ok {
		return
	}
	at := r.clockNow()
	page, err := r.accounting.ReadRunPage(request.Context(), ledger.RunListQuery{ProjectID: projectID, WorkUnitID: workUnitID, Status: status, At: at, GovernanceListOptions: options})
	if err != nil {
		writeGovernanceOperationError(writer, err, "run")
		return
	}
	for i := range page.Items {
		page.Items[i].Status = domain.EffectiveRunStatus(page.Items[i], at)
	}
	writeJSON(writer, http.StatusOK, map[string]any{"items": page.Items, "next_cursor": encodeGovernanceNext(page.NextID)})
}

func (r *Runtime) getAdminRun(writer http.ResponseWriter, request *http.Request) {
	if !allowQueryKeys(writer, request, map[string]struct{}{}) {
		return
	}
	item, ok, err := r.accounting.ReadRun(request.Context(), "", chi.URLParam(request, "runID"))
	if err != nil {
		writeGovernanceOperationError(writer, err, "run")
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

func encodeGovernanceNext(id string) string {
	if id == "" {
		return ""
	}
	return encodeResourceCursor(id)
}
func readGovernanceListOptions(writer http.ResponseWriter, request *http.Request) (ledger.GovernanceListOptions, bool) {
	options := ledger.GovernanceListOptions{Limits: ledger.ProjectionPageLimits{Rows: 50, Bytes: ledger.GovernanceListMaxBytes}}
	if raw := request.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > ledger.GovernanceListMaxRows {
			adminBadRequest(writer, "page limit must be between 1 and 200")
			return ledger.GovernanceListOptions{}, false
		}
		options.Limits.Rows = n
	}
	cursor, err := decodeResourceCursor(request.URL.Query().Get("cursor"))
	if err != nil {
		adminBadRequest(writer, "invalid cursor")
		return ledger.GovernanceListOptions{}, false
	}
	options.After = cursor
	return options, true
}
