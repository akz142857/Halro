package app

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/modelcatalog"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/go-chi/chi/v5"
)

const (
	providerModelCatalogTTL = 5 * time.Minute
	// Bindings are fetched concurrently so a provider with several enabled
	// bindings does not multiply the console's wait by the number of upstreams.
	providerModelFetchConcurrency = 4
)

type providerModelCatalogCache struct {
	Items     []provider.ModelDescriptor
	FetchedAt time.Time
	ExpiresAt time.Time
}

// adminProviderModel is the Admin projection of one discovered model. Upstream
// contributes the identifier; every capability field comes from the catalog.
type adminProviderModel struct {
	ID                 string                       `json:"id"`
	OwnedBy            string                       `json:"owned_by,omitempty"`
	Status             modelcatalog.Status          `json:"status"`
	Capabilities       domain.ProviderCapabilities  `json:"capabilities"`
	CapabilityEvidence domain.CapabilityEvidenceSet `json:"capability_evidence"`
	CapabilitySource   modelcatalog.Source          `json:"capability_source"`
	// Preselect tells the console whether these capabilities may arrive checked.
	Preselect bool `json:"preselect"`
	// ModelRevision is what a create request echoes back and what conflict
	// detection compares. The catalog-wide revision is diagnostic only.
	ModelRevision     string                  `json:"model_revision"`
	ProfileCandidates []modelProfileCandidate `json:"profile_candidates"`
}

type modelProfileCandidate struct {
	BindingID string                   `json:"binding_id"`
	ProfileID domain.ProviderProfileID `json:"profile_id"`
	Status    modelcatalog.Status      `json:"status"`
	Selected  bool                     `json:"selected"`
	// Capabilities is what this model can do through this profile: the catalog
	// entry clamped to the profile ceiling.
	Capabilities domain.ProviderCapabilities `json:"capabilities"`
	// ProfileCapabilities is the ceiling itself, kept separate so the console
	// never presents "the protocol can carry it" as "this model does it".
	ProfileCapabilities domain.ProviderCapabilities `json:"profile_capabilities"`
}

// degradedBinding records a binding whose catalog could not be read. Its models
// are missing from the response; that absence is never evidence that a
// capability is unsupported.
type degradedBinding struct {
	BindingID  string                   `json:"binding_id"`
	ProfileID  domain.ProviderProfileID `json:"profile_id"`
	ErrorClass provider.ErrorClass      `json:"error_class"`
}

type providerModelCatalogResponse struct {
	Items            []adminProviderModel `json:"items"`
	CatalogRevision  string               `json:"catalog_revision"`
	DegradedBindings []degradedBinding    `json:"degraded_bindings,omitempty"`
	FetchedAt        time.Time            `json:"fetched_at"`
	ExpiresAt        time.Time            `json:"expires_at"`
	Cached           bool                 `json:"cached"`
}

type bindingCatalog struct {
	binding    domain.ProviderProfileBinding
	items      []provider.ModelDescriptor
	fetchedAt  time.Time
	expiresAt  time.Time
	cached     bool
	failed     bool
	errorClass provider.ErrorClass
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
	// binding_id stays available as a diagnostic filter. The ordinary flow no
	// longer asks an operator to pick one.
	bindings, err := catalogProviderBindings(instance, strings.TrimSpace(request.URL.Query().Get("binding_id")))
	if err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	refresh := request.URL.Query().Get("refresh") == "true" || request.URL.Query().Get("refresh") == "1"

	results := r.fetchProviderCatalogs(request.Context(), instance, bindings, refresh)
	var degraded []degradedBinding
	var reached int
	for _, result := range results {
		if result.failed {
			degraded = append(degraded, degradedBinding{
				BindingID: result.binding.ID, ProfileID: result.binding.ProfileID, ErrorClass: result.errorClass,
			})
			continue
		}
		reached++
	}
	if reached == 0 {
		errorClass := provider.ErrorUnknown
		if len(degraded) > 0 {
			errorClass = degraded[0].ErrorClass
		}
		writeJSON(writer, http.StatusBadGateway, map[string]any{
			"error": "provider model catalog is unavailable; enter a model ID manually", "error_class": errorClass,
		})
		return
	}
	writeJSON(writer, http.StatusOK, aggregateProviderModels(instance, results, degraded))
}

// catalogProviderBindings returns the bindings whose catalogs make up the
// aggregate. Unlike deployment creation, listing does not require the operator
// to disambiguate: every enabled binding contributes.
func catalogProviderBindings(instance domain.ProviderInstance, bindingID string) ([]domain.ProviderProfileBinding, error) {
	var bindings []domain.ProviderProfileBinding
	for _, binding := range instance.EffectiveProfileBindings() {
		if !binding.Enabled || bindingID != "" && binding.ID != bindingID {
			continue
		}
		bindings = append(bindings, binding)
	}
	if len(bindings) == 0 {
		return nil, errors.New("provider binding is unavailable")
	}
	return bindings, nil
}

func (r *Runtime) fetchProviderCatalogs(ctx context.Context, instance domain.ProviderInstance, bindings []domain.ProviderProfileBinding, refresh bool) []bindingCatalog {
	results := make([]bindingCatalog, len(bindings))
	slots := make(chan struct{}, providerModelFetchConcurrency)
	var wait sync.WaitGroup
	for index, binding := range bindings {
		wait.Add(1)
		go func() {
			defer wait.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			results[index] = r.fetchProviderCatalog(ctx, instance, binding, refresh)
		}()
	}
	wait.Wait()
	return results
}

func (r *Runtime) fetchProviderCatalog(ctx context.Context, instance domain.ProviderInstance, binding domain.ProviderProfileBinding, refresh bool) bindingCatalog {
	result := bindingCatalog{binding: binding}
	cacheKey := instance.ID + "\x00" + binding.ID
	now := time.Now().UTC()
	if !refresh {
		r.providerModelsMu.Lock()
		cached, ok := r.providerModels[cacheKey]
		r.providerModelsMu.Unlock()
		if ok && now.Before(cached.ExpiresAt) {
			result.items = slices.Clone(cached.Items)
			result.fetchedAt, result.expiresAt, result.cached = cached.FetchedAt, cached.ExpiresAt, true
			return result
		}
	}
	adapter, ok := r.providers.AdapterForBinding(instance.ID, binding.ID)
	if !ok && len(instance.Bindings) == 0 {
		adapter, ok = r.providers.AdapterForProvider(instance.ID)
	}
	if !ok {
		result.failed, result.errorClass = true, provider.ErrorUnknown
		return result
	}
	lister, ok := providerModelLister(adapter)
	if !ok {
		// The profile has no discovery endpoint. That is a property of the
		// binding, not an outage, but it degrades the same way: the operator
		// falls back to entering the model ID.
		result.failed, result.errorClass = true, provider.ErrorBadRequest
		return result
	}
	// The timeout is per binding, not shared across the fan-out: one slow
	// upstream must not consume the budget the others need.
	timeout := r.config.Gateway.AttemptResponseHeaderTimeout.Value()
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	fetchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	items, err := lister.ListModels(fetchCtx)
	if err != nil {
		result.failed, result.errorClass = true, provider.ErrorUnknown
		var classified *provider.Error
		if errors.As(err, &classified) {
			result.errorClass = classified.Class
		}
		return result
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
	result.items = slices.Clone(entry.Items)
	result.fetchedAt, result.expiresAt = entry.FetchedAt, entry.ExpiresAt
	return result
}

// aggregateProviderModels folds every reachable binding's catalog into one list
// keyed by model ID, resolving capabilities through the model catalog.
func aggregateProviderModels(instance domain.ProviderInstance, results []bindingCatalog, degraded []degradedBinding) providerModelCatalogResponse {
	models := map[string]*adminProviderModel{}
	var order []string
	response := providerModelCatalogResponse{
		CatalogRevision: modelcatalog.Builtin().Revision(), DegradedBindings: degraded, Cached: true,
	}
	for _, result := range results {
		if result.failed {
			continue
		}
		if response.FetchedAt.IsZero() || result.fetchedAt.Before(response.FetchedAt) {
			response.FetchedAt = result.fetchedAt
		}
		if response.ExpiresAt.IsZero() || result.expiresAt.Before(response.ExpiresAt) {
			response.ExpiresAt = result.expiresAt
		}
		if !result.cached {
			response.Cached = false
		}
		for _, descriptor := range result.items {
			model, exists := models[descriptor.ID]
			if !exists {
				model = &adminProviderModel{ID: descriptor.ID, OwnedBy: descriptor.OwnedBy}
				models[descriptor.ID] = model
				order = append(order, descriptor.ID)
			}
			if model.OwnedBy == "" {
				model.OwnedBy = descriptor.OwnedBy
			}
			model.ProfileCandidates = append(model.ProfileCandidates, resolveModelCandidate(instance, result.binding, descriptor.ID))
		}
	}
	slices.Sort(order)
	response.Items = make([]adminProviderModel, 0, len(order))
	for _, id := range order {
		model := models[id]
		applySelectedCandidate(instance, model)
		response.Items = append(response.Items, *model)
	}
	return response
}

func resolveModelCandidate(instance domain.ProviderInstance, binding domain.ProviderProfileBinding, model string) modelProfileCandidate {
	entry, _ := modelcatalog.Builtin().Lookup(modelCatalogKey(instance, binding, model))
	return modelProfileCandidate{
		BindingID:           binding.ID,
		ProfileID:           binding.ProfileID,
		Status:              entry.Status,
		Capabilities:        modelcatalog.Clamp(entry.Capabilities, binding.Capabilities),
		ProfileCapabilities: binding.Capabilities,
	}
}

// applySelectedCandidate picks the binding a deployment for this model would
// use. A model reachable through several profiles is not presented as one
// multi-capability deployment: that needs per-operation bindings, which the
// runtime does not have yet.
func applySelectedCandidate(instance domain.ProviderInstance, model *adminProviderModel) {
	slices.SortFunc(model.ProfileCandidates, func(left, right modelProfileCandidate) int {
		if rank := candidateRank(right) - candidateRank(left); rank != 0 {
			return rank
		}
		if count := capabilityCount(right.Capabilities) - capabilityCount(left.Capabilities); count != 0 {
			return count
		}
		return strings.Compare(string(left.ProfileID), string(right.ProfileID))
	})
	if len(model.ProfileCandidates) == 0 {
		return
	}
	model.ProfileCandidates[0].Selected = true
	selected := model.ProfileCandidates[0]
	binding := domain.ProviderProfileBinding{ID: selected.BindingID, ProfileID: selected.ProfileID, Capabilities: selected.ProfileCapabilities}
	entry, _ := modelcatalog.Builtin().Lookup(modelCatalogKey(instance, binding, model.ID))
	model.Status = selected.Status
	model.Capabilities = selected.Capabilities
	model.CapabilityEvidence = entry.Evidence()
	model.CapabilitySource = entry.Source
	model.Preselect = entry.Source.PreselectsCapabilities()
	model.ModelRevision = entry.Revision()
}

func modelCatalogKey(instance domain.ProviderInstance, binding domain.ProviderProfileBinding, model string) modelcatalog.Key {
	return modelcatalog.Key{ProviderType: instance.Type, Profile: binding.ProfileID, Model: model}
}

func candidateRank(candidate modelProfileCandidate) int {
	switch candidate.Status {
	case modelcatalog.StatusKnown:
		return 3
	case modelcatalog.StatusPartial:
		return 2
	case modelcatalog.StatusConflicting:
		return 1
	default:
		return 0
	}
}

func capabilityCount(capabilities domain.ProviderCapabilities) int {
	count := 0
	for _, enabled := range []bool{
		capabilities.Chat, capabilities.Embeddings, capabilities.Moderations, capabilities.Images,
		capabilities.Transcriptions, capabilities.Speech, capabilities.Files, capabilities.Batches,
		capabilities.Rerank, capabilities.AsyncGenerate, capabilities.Streaming, capabilities.Tools,
		capabilities.Vision, capabilities.JSONMode, capabilities.DeveloperRole, capabilities.Reasoning,
		capabilities.StreamUsage,
	} {
		if enabled {
			count++
		}
	}
	return count
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
