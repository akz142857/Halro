package app

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/usage"
	"github.com/go-chi/chi/v5"
)

type credentialView struct {
	ID               string                  `json:"id"`
	Name             string                  `json:"name"`
	Type             domain.ProviderType     `json:"type"`
	AccessSurface    domain.AccessSurface    `json:"access_surface"`
	Scheme           domain.CredentialScheme `json:"scheme"`
	BoundBaseURL     string                  `json:"bound_base_url"`
	SecretConfigured bool                    `json:"secret_configured"`
	KeyVersion       uint16                  `json:"key_version"`
	ExpiresAt        *time.Time              `json:"expires_at,omitempty"`
	Revision         uint64                  `json:"revision"`
}

type gatewayKeyView struct {
	ID        string     `json:"id"`
	ProjectID string     `json:"project_id"`
	Name      string     `json:"name"`
	Enabled   bool       `json:"enabled"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	Revision  uint64     `json:"revision"`
}

type alertWebhookView struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	URL              string `json:"url"`
	HeaderName       string `json:"header_name,omitempty"`
	SecretConfigured bool   `json:"secret_configured"`
	Enabled          bool   `json:"enabled"`
	Revision         uint64 `json:"revision"`
}

func (r *Runtime) listAdminProjects(writer http.ResponseWriter, request *http.Request) {
	items, err := r.store.ListProjects(request.Context())
	if err != nil {
		adminStoreError(writer)
		return
	}
	active := make([]domain.Project, 0, len(items))
	for _, item := range items {
		if item.DeletedAt == nil {
			active = append(active, item)
		}
	}
	writeResourcePage(writer, request, active, func(item domain.Project) string { return item.ID })
}

func (r *Runtime) getAdminProject(writer http.ResponseWriter, request *http.Request) {
	item, err := r.store.GetProject(request.Context(), chi.URLParam(request, "id"))
	if err != nil || item.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(item.Revision))
	writeJSON(writer, http.StatusOK, item)
}

func (r *Runtime) listAdminProjectKeys(writer http.ResponseWriter, request *http.Request) {
	projectID := chi.URLParam(request, "id")
	keys, err := r.store.ListGatewayKeys(request.Context())
	if err != nil {
		adminStoreError(writer)
		return
	}
	views := make([]gatewayKeyView, 0)
	for _, key := range keys {
		if key.ProjectID == projectID && key.DeletedAt == nil {
			views = append(views, gatewayKeyView{
				ID: key.ID, ProjectID: key.ProjectID, Name: key.Name,
				Enabled: key.Enabled, ExpiresAt: key.ExpiresAt, CreatedAt: key.CreatedAt,
				Revision: key.Revision,
			})
		}
	}
	writeResourcePage(writer, request, views, func(item gatewayKeyView) string { return item.ID })
}

func (r *Runtime) listAdminCredentials(writer http.ResponseWriter, request *http.Request) {
	items, err := r.store.ListCredentials(request.Context())
	if err != nil {
		adminStoreError(writer)
		return
	}
	views := make([]credentialView, 0, len(items))
	for _, item := range items {
		if string(item.Type) == webhookCredentialType {
			continue
		}
		views = append(views, credentialViewFrom(item))
	}
	writeResourcePage(writer, request, views, func(item credentialView) string { return item.ID })
}

func (r *Runtime) getAdminCredential(writer http.ResponseWriter, request *http.Request) {
	item, err := r.store.GetCredential(request.Context(), chi.URLParam(request, "id"))
	if err != nil {
		adminNotFound(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(item.Revision))
	writeJSON(writer, http.StatusOK, credentialViewFrom(item))
}

func (r *Runtime) listAdminProviders(writer http.ResponseWriter, request *http.Request) {
	items, err := r.store.ListProviders(request.Context())
	if err != nil {
		adminStoreError(writer)
		return
	}
	active := make([]domain.ProviderInstance, 0, len(items))
	for _, item := range items {
		if item.DeletedAt == nil {
			active = append(active, item)
		}
	}
	writeResourcePage(writer, request, active, func(item domain.ProviderInstance) string { return item.ID })
}

func (r *Runtime) getAdminProvider(writer http.ResponseWriter, request *http.Request) {
	item, err := r.store.GetProvider(request.Context(), chi.URLParam(request, "id"))
	if err != nil || item.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(item.Revision))
	writeJSON(writer, http.StatusOK, item)
}

func (r *Runtime) listAdminDeployments(writer http.ResponseWriter, request *http.Request) {
	items, err := r.store.ListDeployments(request.Context())
	if err != nil {
		adminStoreError(writer)
		return
	}
	instances, err := r.providerInstancesByID(request.Context())
	if err != nil {
		adminStoreError(writer)
		return
	}
	probes := r.providers.DeploymentProbes()
	active := make([]adminDeploymentView, 0, len(items))
	for _, item := range items {
		if item.DeletedAt == nil {
			quarantined, reason, quarantineErr := r.store.DeploymentPricingQuarantine(request.Context(), item.ID)
			if quarantineErr != nil {
				adminStoreError(writer)
				return
			}
			item.PricingQuarantined, item.PricingQuarantineReason = quarantined, reason
			active = append(active, adminDeploymentView{
				Deployment: item, CapabilityReview: reviewForDeploymentWithCatalogState(instances, item, r.effectiveModelCatalog(), r.modelCatalogUnavailable()),
				Probe: probeView(probes, item.ID),
			})
		}
	}
	writeResourcePage(writer, request, active, func(item adminDeploymentView) string { return item.ID })
}

func (r *Runtime) getAdminDeployment(writer http.ResponseWriter, request *http.Request) {
	item, err := r.store.GetDeployment(request.Context(), chi.URLParam(request, "id"))
	if err != nil || item.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	item.PricingQuarantined, item.PricingQuarantineReason, err = r.store.DeploymentPricingQuarantine(request.Context(), item.ID)
	if err != nil {
		adminStoreError(writer)
		return
	}
	instances, err := r.providerInstancesByID(request.Context())
	if err != nil {
		adminStoreError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(item.Revision))
	writeJSON(writer, http.StatusOK, adminDeploymentView{
		Deployment: item, CapabilityReview: reviewForDeploymentWithCatalogState(instances, item, r.effectiveModelCatalog(), r.modelCatalogUnavailable()),
		Probe: probeView(r.providers.DeploymentProbes(), item.ID),
	})
}

func (r *Runtime) listAdminRoutes(writer http.ResponseWriter, request *http.Request) {
	items, err := r.store.ListRoutes(request.Context())
	if err != nil {
		adminStoreError(writer)
		return
	}
	active := make([]domain.Route, 0, len(items))
	for _, item := range items {
		if item.DeletedAt == nil {
			active = append(active, item)
		}
	}
	writeResourcePage(writer, request, active, func(item domain.Route) string { return item.ID })
}

func (r *Runtime) getAdminRoute(writer http.ResponseWriter, request *http.Request) {
	item, err := r.store.GetRoute(request.Context(), chi.URLParam(request, "id"))
	if err != nil || item.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(item.Revision))
	writeJSON(writer, http.StatusOK, item)
}

func (r *Runtime) listAdminTokenGuardPolicies(writer http.ResponseWriter, request *http.Request) {
	items, err := r.store.ListTokenGuardPolicies(request.Context())
	if err != nil {
		adminStoreError(writer)
		return
	}
	active := make([]domain.TokenGuardPolicy, 0, len(items))
	for _, item := range items {
		if item.DeletedAt == nil {
			active = append(active, item)
		}
	}
	views := make([]tokenGuardView, 0, len(active))
	bindings := make(map[string]int)
	projects, err := r.store.ListProjects(request.Context())
	if err != nil {
		adminStoreError(writer)
		return
	}
	for _, project := range projects {
		if project.DeletedAt == nil && project.TokenGuardPolicyID != "" {
			bindings[project.TokenGuardPolicyID]++
		}
	}
	for _, item := range active {
		view := tokenGuardPolicyView(item)
		view.BoundProjects = bindings[item.ID]
		views = append(views, view)
	}
	writeResourcePage(writer, request, views, func(item tokenGuardView) string { return item.ID })
}

func (r *Runtime) listAdminAlerts(writer http.ResponseWriter, request *http.Request) {
	items, err := r.store.ListAlertWebhooks(request.Context())
	if err != nil {
		adminStoreError(writer)
		return
	}
	views := make([]alertWebhookView, 0, len(items))
	for _, item := range items {
		if item.DeletedAt == nil {
			views = append(views, alertWebhookSafeView(item))
		}
	}
	writeResourcePage(writer, request, views, func(item alertWebhookView) string { return item.ID })
}

// auditRecordView flattens audit.Record for the console. The stored record nests every
// field under `event`, and serialising it directly leaves the timeline showing sequence
// numbers beside blank rows. The per-frame hash stays out: it is verified server-side on
// every replay and means nothing to a reader who cannot recompute the chain.
type auditRecordView struct {
	Sequence      uint64    `json:"sequence"`
	EventID       string    `json:"event_id"`
	OccurredAt    time.Time `json:"occurred_at"`
	ActorType     string    `json:"actor_type"`
	ActorID       string    `json:"actor_id"`
	Action        string    `json:"action"`
	TargetType    string    `json:"target_type"`
	TargetID      string    `json:"target_id"`
	Outcome       string    `json:"outcome"`
	ReasonCode    string    `json:"reason_code,omitempty"`
	CorrelationID string    `json:"correlation_id,omitempty"`
	// Evidence some actions attach — pricing request digests, developer HTTP status.
	Metadata map[string]any `json:"metadata,omitempty"`
}

func auditRecordFlatView(record audit.Record) auditRecordView {
	return auditRecordView{
		Sequence: record.Sequence, EventID: record.Event.EventID,
		OccurredAt: record.Event.OccurredAt, ActorType: record.Event.ActorType,
		ActorID: record.Event.ActorID, Action: record.Event.Action,
		TargetType: record.Event.TargetType, TargetID: record.Event.TargetID,
		Outcome: record.Event.Outcome, ReasonCode: record.Event.ReasonCode,
		CorrelationID: record.Event.CorrelationID, Metadata: record.Event.Metadata,
	}
}

func (r *Runtime) listAdminAudit(writer http.ResponseWriter, request *http.Request) {
	allowed, limit, cursor, ok := parseSequencePage(writer, request)
	if !ok || !allowed {
		return
	}
	records := make([]auditRecordView, 0)
	if _, err := r.audit.Replay(func(record audit.Record) error {
		if cursor == 0 || record.Sequence < cursor {
			records = append(records, auditRecordFlatView(record))
		}
		return nil
	}); err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "audit unavailable"})
		return
	}
	for left, right := 0, len(records)-1; left < right; left, right = left+1, right-1 {
		records[left], records[right] = records[right], records[left]
	}
	nextCursor := ""
	if len(records) > limit {
		records = records[:limit]
		nextCursor = usage.EncodeCursor(records[len(records)-1].Sequence)
	}
	writeJSON(writer, http.StatusOK, map[string]any{"items": records, "next_cursor": nextCursor})
}

func writeResourcePage[T any](
	writer http.ResponseWriter,
	request *http.Request,
	items []T,
	id func(T) string,
) {
	allowed := map[string]struct{}{"cursor": {}, "limit": {}}
	for name := range request.URL.Query() {
		if _, exists := allowed[name]; !exists {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "unsupported list filter"})
			return
		}
	}
	limit := 50
	if raw := request.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "page limit must be between 1 and 100"})
			return
		}
		limit = value
	}
	cursor, err := decodeResourceCursor(request.URL.Query().Get("cursor"))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid cursor"})
		return
	}
	start := 0
	if cursor != "" {
		for start < len(items) && id(items[start]) <= cursor {
			start++
		}
	}
	end := start + limit
	if end > len(items) {
		end = len(items)
	}
	nextCursor := ""
	if end < len(items) && end > start {
		nextCursor = encodeResourceCursor(id(items[end-1]))
	}
	pageItems := items[start:end]
	if pageItems == nil {
		pageItems = make([]T, 0)
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"items": pageItems, "next_cursor": nextCursor,
	})
}

func parseSequencePage(
	writer http.ResponseWriter,
	request *http.Request,
) (bool, int, uint64, bool) {
	for name := range request.URL.Query() {
		if name != "cursor" && name != "limit" {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "unsupported audit filter"})
			return false, 0, 0, false
		}
	}
	limit := 50
	if raw := request.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "page limit must be between 1 and 100"})
			return false, 0, 0, false
		}
		limit = value
	}
	cursor, err := usage.DecodeCursor(request.URL.Query().Get("cursor"))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid cursor"})
		return false, 0, 0, false
	}
	return true, limit, cursor, true
}

func encodeResourceCursor(id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id))
}

func decodeResourceCursor(cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(raw) == 0 || len(raw) > 256 || strings.ContainsRune(string(raw), 0) {
		return "", errors.New("invalid cursor")
	}
	return string(raw), nil
}

func revisionETag(revision uint64) string {
	return `"` + strconv.FormatUint(revision, 10) + `"`
}
