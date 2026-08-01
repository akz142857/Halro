package app

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/akz142857/Heimdall/internal/audit"
	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/usage"
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
	Revision         uint64                  `json:"revision"`
}

type gatewayKeyView struct {
	ID         string     `json:"id"`
	ProjectID  string     `json:"project_id"`
	Name       string     `json:"name"`
	Enabled    bool       `json:"enabled"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	Revision   uint64     `json:"revision"`
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
				LastUsedAt: key.LastUsedAt, Revision: key.Revision,
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
		views = append(views, credentialView{
			ID: item.ID, Name: item.Name, Type: item.Type,
			AccessSurface: item.AccessSurface, Scheme: item.Scheme,
			SecretConfigured: len(item.Ciphertext) > 0, KeyVersion: item.KeyVersion,
			Revision: item.Revision,
		})
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
	writeJSON(writer, http.StatusOK, credentialView{
		ID: item.ID, Name: item.Name, Type: item.Type,
		AccessSurface: item.AccessSurface, Scheme: item.Scheme,
		SecretConfigured: len(item.Ciphertext) > 0, KeyVersion: item.KeyVersion,
		Revision: item.Revision,
	})
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
	active := make([]domain.Deployment, 0, len(items))
	for _, item := range items {
		if item.DeletedAt == nil {
			active = append(active, item)
		}
	}
	writeResourcePage(writer, request, active, func(item domain.Deployment) string { return item.ID })
}

func (r *Runtime) getAdminDeployment(writer http.ResponseWriter, request *http.Request) {
	item, err := r.store.GetDeployment(request.Context(), chi.URLParam(request, "id"))
	if err != nil || item.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(item.Revision))
	writeJSON(writer, http.StatusOK, item)
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
	for _, item := range active {
		views = append(views, tokenGuardPolicyView(item))
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

func (r *Runtime) listAdminAudit(writer http.ResponseWriter, request *http.Request) {
	allowed, limit, cursor, ok := parseSequencePage(writer, request)
	if !ok || !allowed {
		return
	}
	records := make([]audit.Record, 0)
	if _, err := r.audit.Replay(func(record audit.Record) error {
		if cursor == 0 || record.Sequence < cursor {
			records = append(records, record)
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
	writeJSON(writer, http.StatusOK, map[string]any{
		"items": items[start:end], "next_cursor": nextCursor,
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

func adminNotFound(writer http.ResponseWriter) {
	writeJSON(writer, http.StatusNotFound, map[string]string{"error": "resource not found"})
}

func adminStoreError(writer http.ResponseWriter) {
	writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "metadata unavailable"})
}
