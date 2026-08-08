package app

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/provider"
	"github.com/go-chi/chi/v5"
)

const providerModelCatalogTTL = 5 * time.Minute

type providerModelCatalogCache struct {
	Items     []provider.ModelDescriptor
	FetchedAt time.Time
	ExpiresAt time.Time
}

type providerModelCatalogResponse struct {
	Items     []provider.ModelDescriptor `json:"items"`
	FetchedAt time.Time                  `json:"fetched_at"`
	ExpiresAt time.Time                  `json:"expires_at"`
	Cached    bool                       `json:"cached"`
}

func (r *Runtime) listAdminProviderModels(writer http.ResponseWriter, request *http.Request) {
	providerID := chi.URLParam(request, "id")
	instance, err := r.store.GetProvider(request.Context(), providerID)
	if err != nil || instance.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	if !instance.Enabled {
		adminBadRequest(writer, "provider is disabled")
		return
	}
	bindingID := strings.TrimSpace(request.URL.Query().Get("binding_id"))
	selected, err := enabledProviderBinding(instance, bindingID)
	if err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	cacheKey := providerID + "\x00" + selected.ID
	refresh := request.URL.Query().Get("refresh") == "true" || request.URL.Query().Get("refresh") == "1"
	now := time.Now().UTC()
	if !refresh {
		r.providerModelsMu.Lock()
		cached, ok := r.providerModels[cacheKey]
		r.providerModelsMu.Unlock()
		if ok && now.Before(cached.ExpiresAt) {
			writeJSON(writer, http.StatusOK, providerModelCatalogResponse{
				Items: slices.Clone(cached.Items), FetchedAt: cached.FetchedAt, ExpiresAt: cached.ExpiresAt, Cached: true,
			})
			return
		}
	}
	adapter, ok := r.providers.AdapterForBinding(providerID, selected.ID)
	if !ok && len(instance.Bindings) == 0 {
		adapter, ok = r.providers.AdapterForProvider(providerID)
	}
	if !ok {
		adminBadRequest(writer, "provider binding adapter is unavailable")
		return
	}
	lister, ok := providerModelLister(adapter)
	if !ok {
		adminBadRequest(writer, "provider does not support model discovery; enter a model ID manually")
		return
	}
	timeout := r.config.Gateway.AttemptResponseHeaderTimeout.Value()
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(request.Context(), timeout)
	defer cancel()
	items, err := lister.ListModels(ctx)
	if err != nil {
		errorClass := provider.ErrorUnknown
		var classified *provider.Error
		if errors.As(err, &classified) {
			errorClass = classified.Class
		}
		writeJSON(writer, http.StatusBadGateway, map[string]any{
			"error": "provider model catalog is unavailable; enter a model ID manually", "error_class": errorClass,
		})
		return
	}
	items = normalizeProviderModels(items)
	fetchedAt := time.Now().UTC()
	entry := providerModelCatalogCache{Items: items, FetchedAt: fetchedAt, ExpiresAt: fetchedAt.Add(providerModelCatalogTTL)}
	r.providerModelsMu.Lock()
	if r.providerModels == nil {
		r.providerModels = make(map[string]providerModelCatalogCache)
	}
	r.providerModels[cacheKey] = entry
	r.providerModelsMu.Unlock()
	writeJSON(writer, http.StatusOK, providerModelCatalogResponse{
		Items: slices.Clone(entry.Items), FetchedAt: entry.FetchedAt, ExpiresAt: entry.ExpiresAt,
	})
}

func providerModelLister(adapter provider.Adapter) (provider.ModelLister, bool) {
	for adapter != nil {
		if lister, ok := adapter.(provider.ModelLister); ok {
			return lister, true
		}
		wrapper, ok := adapter.(provider.AdapterUnwrapper)
		if !ok {
			return nil, false
		}
		next := wrapper.UnwrapAdapter()
		if next == adapter {
			return nil, false
		}
		adapter = next
	}
	return nil, false
}

func normalizeProviderModels(items []provider.ModelDescriptor) []provider.ModelDescriptor {
	seen := make(map[string]struct{}, len(items))
	normalized := make([]provider.ModelDescriptor, 0, len(items))
	for _, item := range items {
		item.ID = strings.TrimSpace(item.ID)
		item.OwnedBy = strings.TrimSpace(item.OwnedBy)
		if item.ID == "" || len(item.ID) > 512 {
			continue
		}
		if _, exists := seen[item.ID]; exists {
			continue
		}
		seen[item.ID] = struct{}{}
		normalized = append(normalized, item)
		if len(normalized) == 10_000 {
			break
		}
	}
	slices.SortFunc(normalized, func(left, right provider.ModelDescriptor) int {
		return strings.Compare(strings.ToLower(left.ID), strings.ToLower(right.ID))
	})
	return normalized
}

func (r *Runtime) clearProviderModelCatalog(providerID string) {
	r.providerModelsMu.Lock()
	defer r.providerModelsMu.Unlock()
	for key := range r.providerModels {
		if strings.HasPrefix(key, providerID+"\x00") {
			delete(r.providerModels, key)
		}
	}
}
