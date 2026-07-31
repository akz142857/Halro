package app

import (
	"net/http"
	"strconv"
	"time"

	"github.com/akz142857/Heimdall/internal/buildinfo"
	"github.com/akz142857/Heimdall/internal/usage"
	"github.com/go-chi/chi/v5"
)

func (r *Runtime) adminDashboard(writer http.ResponseWriter, request *http.Request) {
	if !r.syncUsageAdmin(writer, request) {
		return
	}
	dashboard := r.usage.Dashboard(time.Now(), r.usageLocation)
	writeJSON(writer, http.StatusOK, map[string]any{
		"usage":             dashboard,
		"accounting_status": r.status.Load(),
		"wal":               r.ledger.Stats(),
		"alerts":            r.alerts.Stats(),
	})
}

func (r *Runtime) adminUsage(writer http.ResponseWriter, request *http.Request) {
	if !r.syncUsageAdmin(writer, request) {
		return
	}
	allowed := map[string]struct{}{
		"cursor": {}, "limit": {}, "project_id": {}, "provider_id": {},
		"model": {}, "status": {}, "start": {}, "end": {},
	}
	for name := range request.URL.Query() {
		if _, exists := allowed[name]; !exists {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "unsupported usage filter"})
			return
		}
	}
	cursor, err := usage.DecodeCursor(request.URL.Query().Get("cursor"))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	limit := 50
	if raw := request.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid page limit"})
			return
		}
	}
	query := usage.AttemptQuery{
		BeforeSequence: cursor, Limit: limit,
		ProjectID:      request.URL.Query().Get("project_id"),
		ProviderID:     request.URL.Query().Get("provider_id"),
		RequestedModel: request.URL.Query().Get("model"),
		Status:         request.URL.Query().Get("status"),
	}
	if raw := request.URL.Query().Get("start"); raw != "" {
		query.Start, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid start timestamp"})
			return
		}
	}
	if raw := request.URL.Query().Get("end"); raw != "" {
		query.End, err = time.Parse(time.RFC3339, raw)
		if err != nil || !query.Start.IsZero() && !query.End.After(query.Start) {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid end timestamp"})
			return
		}
	}
	page, err := r.usage.QueryAttempts(query)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"items": page.Attempts, "next_cursor": page.NextCursor,
	})
}

func (r *Runtime) adminUsageRequest(writer http.ResponseWriter, request *http.Request) {
	if !r.syncUsageAdmin(writer, request) {
		return
	}
	requestID := chi.URLParam(request, "requestID")
	if requestID == "" || len(requestID) > 128 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid request ID"})
		return
	}
	detail, exists := r.usage.RequestDetail(requestID)
	if !exists {
		writeJSON(writer, http.StatusNotFound, map[string]string{"error": "usage request not found"})
		return
	}
	writeJSON(writer, http.StatusOK, detail)
}

func (r *Runtime) adminSystemStatus(writer http.ResponseWriter, request *http.Request) {
	if !r.syncUsageAdmin(writer, request) {
		return
	}
	auditSummary := r.audit.Summary()
	writeJSON(writer, http.StatusOK, map[string]any{
		"build":             buildinfo.Current(),
		"accounting_status": r.status.Load(),
		"draining":          r.draining.Load(),
		"wal":               r.ledger.Stats(),
		"audit":             auditSummary,
		"alerts":            r.alerts.Stats(),
		"usage_watermark":   r.usage.Watermark(),
	})
}

func (r *Runtime) syncUsageAdmin(writer http.ResponseWriter, request *http.Request) bool {
	if err := r.usageCollector.CatchUp(request.Context()); err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "usage analytics unavailable"})
		return false
	}
	return true
}
