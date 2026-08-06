package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/id"
	"github.com/akz142857/Heimdall/internal/provider"
	bedrockprovider "github.com/akz142857/Heimdall/internal/provider/bedrock"
	bedrockmantleprovider "github.com/akz142857/Heimdall/internal/provider/bedrockmantle"
	"github.com/akz142857/Heimdall/internal/safetransport"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
	"github.com/go-chi/chi/v5"
)

type credentialInput struct {
	Name          string                  `json:"name"`
	Type          domain.ProviderType     `json:"type"`
	BaseURL       string                  `json:"base_url"`
	AccessSurface domain.AccessSurface    `json:"access_surface,omitempty"`
	Scheme        domain.CredentialScheme `json:"scheme,omitempty"`
	Secret        *string                 `json:"secret,omitempty"`
}

type providerInput struct {
	Name             string                           `json:"name"`
	Type             domain.ProviderType              `json:"type"`
	BaseURL          string                           `json:"base_url"`
	APIVersion       string                           `json:"api_version,omitempty"`
	CredentialID     string                           `json:"credential_id"`
	AccessSurface    domain.AccessSurface             `json:"access_surface,omitempty"`
	ProfileID        domain.ProviderProfileID         `json:"profile_id,omitempty"`
	CredentialScheme domain.CredentialScheme          `json:"credential_scheme,omitempty"`
	Capabilities     *domain.ProviderCapabilities     `json:"capabilities,omitempty"`
	Bindings         *[]domain.ProviderProfileBinding `json:"bindings,omitempty"`
	MaxConcurrency   int64                            `json:"max_concurrency"`
	Enabled          bool                             `json:"enabled"`
}

type routeInput struct {
	PublicModel            string `json:"public_model"`
	DeploymentID           string `json:"deployment_id,omitempty"`
	ProviderID             string `json:"provider_id"`
	ProviderModel          string `json:"provider_model"`
	InputMicrosPerMillion  int64  `json:"input_micros_per_million"`
	OutputMicrosPerMillion int64  `json:"output_micros_per_million"`
	Priority               int    `json:"priority"`
	Strategy               string `json:"strategy"`
	Enabled                bool   `json:"enabled"`
}

func (r *Runtime) createAdminCredential(writer http.ResponseWriter, request *http.Request) {
	var input credentialInput
	if err := decodeAdminJSON(request, &input); err != nil || input.Secret == nil {
		adminBadRequest(writer, "invalid request")
		return
	}
	credentialID, err := id.New("cred")
	if err != nil {
		adminStoreError(writer)
		return
	}
	credential, err := r.credentialFromInput(credentialID, input, nil, time.Now().UTC())
	if err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	credential, err = r.store.PutCredential(request.Context(), credential, 0)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if err := r.auditAdminMutation(request, "credential.create", "credential", credential.ID); err != nil {
		adminAuditError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(credential.Revision))
	writeJSON(writer, http.StatusCreated, credentialViewFrom(credential))
}

func (r *Runtime) updateAdminCredential(writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	var input credentialInput
	if err := decodeAdminJSON(request, &input); err != nil {
		adminBadRequest(writer, "invalid request")
		return
	}
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	current, err := r.store.GetCredential(request.Context(), chi.URLParam(request, "id"))
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if current.Revision != expected {
		adminPreconditionFailed(writer)
		return
	}
	credential, err := r.credentialFromInput(current.ID, input, &current, current.CreatedAt)
	if err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	if err := r.validateCredentialReferences(request, credential); err != nil {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	credential, err = r.store.PutCredential(request.Context(), credential, expected)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if err := r.reloadProviderRegistry(request.Context()); err != nil {
		adminConfigurationError(writer, err)
		return
	}
	if err := r.auditAdminMutation(request, "credential.rotate", "credential", credential.ID); err != nil {
		adminAuditError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(credential.Revision))
	writeJSON(writer, http.StatusOK, credentialViewFrom(credential))
}

func (r *Runtime) deleteAdminCredential(writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	// Deleting is not undoable and a stolen session should not be enough to do
	// it. The revision precondition is checked first: it costs a header parse,
	// while the step-up costs an Argon2id verification, and a request that
	// cannot succeed anyway should not buy one.
	if !r.requireDestructiveStepUp(writer, request) {
		return
	}
	credentialID := chi.URLParam(request, "id")
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	if err := r.store.DeleteCredential(request.Context(), credentialID, expected); err != nil {
		if errors.Is(err, boltstore.ErrCredentialInUse) {
			writeJSON(writer, http.StatusConflict, map[string]string{"error": "credential is still referenced"})
			return
		}
		adminMutationError(writer, err)
		return
	}
	if err := r.auditAdminMutation(request, "credential.delete", "credential", credentialID); err != nil {
		adminAuditError(writer)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (r *Runtime) createAdminProvider(writer http.ResponseWriter, request *http.Request) {
	var input providerInput
	if err := decodeAdminJSON(request, &input); err != nil {
		adminBadRequest(writer, "invalid request")
		return
	}
	providerID, err := id.New("prv")
	if err != nil {
		adminStoreError(writer)
		return
	}
	now := time.Now().UTC()
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	instance, err := r.providerFromInput(request, providerID, input, "", nil, nil, now, now)
	if err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	instance, err = r.store.PutProvider(request.Context(), instance, 0)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if err := r.reloadProviderRegistry(request.Context()); err != nil {
		adminConfigurationError(writer, err)
		return
	}
	if err := r.auditAdminMutation(request, "provider.create", "provider", instance.ID); err != nil {
		adminAuditError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(instance.Revision))
	writeJSON(writer, http.StatusCreated, instance)
}

func (r *Runtime) updateAdminProvider(writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	var input providerInput
	if err := decodeAdminJSON(request, &input); err != nil {
		adminBadRequest(writer, "invalid request")
		return
	}
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	current, err := r.store.GetProvider(request.Context(), chi.URLParam(request, "id"))
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if current.Revision != expected {
		adminPreconditionFailed(writer)
		return
	}
	if len(current.EffectiveProfileBindings()) > 1 && input.Bindings == nil {
		adminBadRequest(writer, "bindings are required when updating a provider with multiple profile bindings")
		return
	}
	if input.Type != current.Type {
		deployments, listErr := r.store.ListDeployments(request.Context())
		if listErr != nil {
			adminStoreError(writer)
			return
		}
		for _, deployment := range deployments {
			if deployment.ProviderID == current.ID && deployment.DeletedAt == nil {
				adminBadRequest(writer, "provider type and profile cannot change while deployments reference it")
				return
			}
		}
	}
	currentEvidence := current.CapabilityEvidence
	if input.Type != current.Type || input.ProfileID != "" && input.ProfileID != current.ProfileID {
		currentEvidence = nil
	}
	instance, err := r.providerFromInput(request, current.ID, input, current.ProfileID, currentEvidence, current.Bindings, current.CreatedAt, time.Now().UTC())
	if err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	if err := r.validateProviderCanDeactivate(request, instance.ID, instance.Enabled); err != nil {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	instance.DeletedAt = current.DeletedAt
	instance.LastTestStatus = current.LastTestStatus
	instance.LastTestedAt = current.LastTestedAt
	instance.LastTestLatencyMillis = current.LastTestLatencyMillis
	instance.LastTestErrorClass = current.LastTestErrorClass
	instance.LastTestRevision = current.LastTestRevision
	instance.LastTestHealthyTargets = current.LastTestHealthyTargets
	instance.LastTestTotalTargets = current.LastTestTotalTargets
	instance, err = r.store.PutProvider(request.Context(), instance, expected)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if err := r.reloadProviderRegistry(request.Context()); err != nil {
		adminConfigurationError(writer, err)
		return
	}
	r.clearProviderModelCatalog(instance.ID)
	if err := r.auditAdminMutation(request, "provider.update", "provider", instance.ID); err != nil {
		adminAuditError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(instance.Revision))
	writeJSON(writer, http.StatusOK, instance)
}

func (r *Runtime) deleteAdminProvider(writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	// Deleting is not undoable and a stolen session should not be enough to do
	// it. The revision precondition is checked first: it costs a header parse,
	// while the step-up costs an Argon2id verification, and a request that
	// cannot succeed anyway should not buy one.
	if !r.requireDestructiveStepUp(writer, request) {
		return
	}
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	instance, err := r.store.GetProvider(request.Context(), chi.URLParam(request, "id"))
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if instance.Revision != expected {
		adminPreconditionFailed(writer)
		return
	}
	if err := r.validateProviderCanDeactivate(request, instance.ID, false); err != nil {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	now := time.Now().UTC()
	instance.Enabled = false
	instance.UpdatedAt = now
	instance.DeletedAt = &now
	instance, err = r.store.PutProvider(request.Context(), instance, expected)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if err := r.reloadProviderRegistry(request.Context()); err != nil {
		adminConfigurationError(writer, err)
		return
	}
	r.clearProviderModelCatalog(instance.ID)
	if err := r.auditAdminMutation(request, "provider.delete", "provider", instance.ID); err != nil {
		adminAuditError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(instance.Revision))
	writer.WriteHeader(http.StatusNoContent)
}

func (r *Runtime) testAdminProvider(writer http.ResponseWriter, request *http.Request) {
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
	bindings := make([]domain.ProviderProfileBinding, 0, len(instance.EffectiveProfileBindings()))
	if bindingID != "" {
		selected, selectionErr := enabledProviderBinding(instance, bindingID)
		if selectionErr != nil {
			adminBadRequest(writer, selectionErr.Error())
			return
		}
		bindings = append(bindings, selected)
	} else {
		for _, binding := range instance.EffectiveProfileBindings() {
			if binding.Enabled {
				bindings = append(bindings, binding)
			}
		}
	}
	if len(bindings) == 0 {
		adminBadRequest(writer, "provider binding is unavailable")
		return
	}
	deployments, err := r.store.ListDeployments(request.Context())
	if err != nil {
		adminStoreError(writer)
		return
	}
	routes, err := r.store.ListRoutes(request.Context())
	if err != nil {
		adminStoreError(writer)
		return
	}
	testedRevision := instance.Revision
	timeout := r.config.Gateway.AttemptResponseHeaderTimeout.Value()
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	testedAt := time.Now().UTC()
	maxLatencyMS := int64(0)
	healthyTargets := 0
	errorClass := provider.ErrorUnknown
	var probeErr error
	for _, binding := range bindings {
		adapter, ok := r.providers.AdapterForBinding(providerID, binding.ID)
		if !ok && len(instance.Bindings) == 0 {
			adapter, ok = r.providers.AdapterForProvider(providerID)
		}
		if !ok {
			adminBadRequest(writer, "provider binding adapter is unavailable")
			return
		}
		prober, ok := adapter.(provider.Prober)
		if !ok {
			adminBadRequest(writer, "provider does not support connection testing")
			return
		}
		providerModel := providerProbeModel(instance, providerID, binding.ID, deployments, routes)
		ctx, cancel := context.WithTimeout(request.Context(), timeout)
		started := time.Now()
		err := prober.Probe(ctx, providerModel)
		latencyMS := time.Since(started).Milliseconds()
		cancel()
		if latencyMS > maxLatencyMS {
			maxLatencyMS = latencyMS
		}
		if err == nil {
			healthyTargets++
			continue
		}
		if probeErr == nil {
			probeErr = err
			var classified *provider.Error
			if errors.As(err, &classified) {
				errorClass = classified.Class
			}
		}
	}
	status := domain.DeploymentTestHealthy
	if probeErr != nil {
		status = domain.DeploymentTestUnhealthy
	}
	testedAt = time.Now().UTC()
	r.adminTopologyMu.Lock()
	current, storeErr := r.store.GetProvider(request.Context(), providerID)
	if storeErr != nil || current.DeletedAt != nil {
		r.adminTopologyMu.Unlock()
		adminNotFound(writer)
		return
	}
	if current.Revision != testedRevision {
		r.adminTopologyMu.Unlock()
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "provider changed during validation; test the current revision again"})
		return
	}
	current.LastTestStatus = status
	current.LastTestedAt = &testedAt
	current.LastTestLatencyMillis = maxLatencyMS
	current.LastTestRevision = current.Revision + 1
	current.LastTestErrorClass = ""
	current.LastTestHealthyTargets = healthyTargets
	current.LastTestTotalTargets = len(bindings)
	if probeErr != nil {
		current.LastTestErrorClass = string(errorClass)
	}
	current.UpdatedAt = testedAt
	current, storeErr = r.store.PutProvider(request.Context(), current, testedRevision)
	r.adminTopologyMu.Unlock()
	if storeErr != nil {
		adminMutationError(writer, storeErr)
		return
	}
	action := "provider.test.success"
	if probeErr != nil {
		action = "provider.test.failure"
	}
	if err := r.auditAdminMutation(request, action, "provider", providerID); err != nil {
		adminAuditError(writer)
		return
	}
	result := map[string]any{
		"status": status, "latency_ms": maxLatencyMS, "tested_at": testedAt, "revision": current.Revision,
		"healthy_targets": healthyTargets, "total_targets": len(bindings),
	}
	writer.Header().Set("ETag", revisionETag(current.Revision))
	if probeErr != nil {
		result["error_class"] = errorClass
		writeJSON(writer, http.StatusBadGateway, result)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func providerProbeModel(instance domain.ProviderInstance, providerID, bindingID string, deployments []domain.Deployment, routes []domain.Route) string {
	for _, deployment := range deployments {
		deploymentBindingID := deployment.BindingID
		if deploymentBindingID == "" {
			deploymentBindingID = matchingBindingID(instance, deployment.ProfileID)
		}
		if deployment.ProviderID == providerID && deploymentBindingID == bindingID && deployment.Enabled && deployment.DeletedAt == nil {
			return deployment.ProviderModel
		}
	}
	for _, route := range routes {
		if route.ProviderID == providerID && route.Enabled && route.DeletedAt == nil {
			return route.ProviderModel
		}
	}
	return ""
}

func enabledProviderBinding(instance domain.ProviderInstance, bindingID string) (domain.ProviderProfileBinding, error) {
	var selected domain.ProviderProfileBinding
	for _, binding := range instance.EffectiveProfileBindings() {
		if !binding.Enabled || bindingID != "" && binding.ID != bindingID {
			continue
		}
		if selected.ID != "" {
			return domain.ProviderProfileBinding{}, errors.New("provider has multiple enabled bindings; binding_id is required")
		}
		selected = binding
	}
	if selected.ID == "" {
		return domain.ProviderProfileBinding{}, errors.New("provider binding is unavailable")
	}
	return selected, nil
}

func (r *Runtime) testAdminRoute(writer http.ResponseWriter, request *http.Request) {
	route, err := r.store.GetRoute(request.Context(), chi.URLParam(request, "id"))
	if err != nil || route.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	if !route.Enabled {
		adminBadRequest(writer, "route is disabled")
		return
	}
	providerID := route.ProviderID
	providerModel := route.ProviderModel
	var deployment domain.Deployment
	capabilities := domain.ProviderCapabilities{}
	if route.DeploymentID != "" {
		var deploymentErr error
		deployment, deploymentErr = r.store.GetDeployment(request.Context(), route.DeploymentID)
		if deploymentErr != nil || deployment.DeletedAt != nil || !deployment.Enabled {
			adminBadRequest(writer, "route deployment is unavailable")
			return
		}
		providerID = deployment.ProviderID
		providerModel = deployment.ProviderModel
		capabilities = deployment.Capabilities
	}
	instance, err := r.store.GetProvider(request.Context(), providerID)
	if err != nil || instance.DeletedAt != nil || !instance.Enabled {
		adminBadRequest(writer, "route provider is unavailable")
		return
	}
	if route.DeploymentID == "" {
		capabilities = normalizedProviderCapabilities(instance)
	}
	var adapter provider.Adapter
	var ok bool
	if route.DeploymentID != "" {
		adapter, ok = adapterForDeployment(r.providers, instance, deployment)
	} else {
		adapter, ok = r.providers.AdapterForProvider(providerID)
	}
	if !ok {
		adminBadRequest(writer, "route provider adapter is unavailable")
		return
	}
	prober, ok := adapter.(provider.Prober)
	if !ok {
		adminBadRequest(writer, "provider does not support connection testing")
		return
	}
	testedRevision := route.Revision
	timeout := r.config.Gateway.AttemptResponseHeaderTimeout.Value()
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(request.Context(), timeout)
	started := time.Now()
	probeErr := prober.Probe(ctx, providerModel)
	latencyMS := time.Since(started).Milliseconds()
	cancel()
	testedAt := time.Now().UTC()
	errorClass := provider.ErrorUnknown
	status := domain.DeploymentTestHealthy
	if probeErr != nil {
		status = domain.DeploymentTestUnhealthy
		var classified *provider.Error
		if errors.As(probeErr, &classified) {
			errorClass = classified.Class
		}
	}
	r.adminTopologyMu.Lock()
	current, storeErr := r.store.GetRoute(request.Context(), route.ID)
	if storeErr != nil || current.DeletedAt != nil {
		r.adminTopologyMu.Unlock()
		adminNotFound(writer)
		return
	}
	if current.Revision != testedRevision {
		r.adminTopologyMu.Unlock()
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "route changed during validation; test the current revision again"})
		return
	}
	current.LastTestStatus = status
	current.LastTestedAt = &testedAt
	current.LastTestLatencyMillis = latencyMS
	current.LastTestRevision = current.Revision + 1
	current.LastTestErrorClass = ""
	if probeErr != nil {
		current.LastTestErrorClass = string(errorClass)
	}
	current.UpdatedAt = testedAt
	current, storeErr = r.store.PutRoute(request.Context(), current, testedRevision)
	r.adminTopologyMu.Unlock()
	if storeErr != nil {
		adminMutationError(writer, storeErr)
		return
	}
	action := "route.test.success"
	if probeErr != nil {
		action = "route.test.failure"
	}
	if err := r.auditAdminMutation(request, action, "route", route.ID); err != nil {
		adminAuditError(writer)
		return
	}
	result := map[string]any{
		"status": status, "latency_ms": latencyMS, "tested_at": testedAt, "revision": current.Revision,
	}
	writer.Header().Set("ETag", revisionETag(current.Revision))
	if probeErr != nil {
		result["error_class"] = errorClass
		writeJSON(writer, http.StatusBadGateway, result)
		return
	}
	result["capabilities"] = capabilities
	writeJSON(writer, http.StatusOK, result)
}

func (r *Runtime) createAdminRoute(writer http.ResponseWriter, request *http.Request) {
	var input routeInput
	if err := decodeAdminJSON(request, &input); err != nil {
		adminBadRequest(writer, "invalid request")
		return
	}
	routeID, err := id.New("rte")
	if err != nil {
		adminStoreError(writer)
		return
	}
	now := time.Now().UTC()
	route := input.route(routeID, now, now)
	if err := route.Validate(); err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	if err := r.validateAdminRoute(request, route, ""); err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	route, err = r.store.PutRoute(request.Context(), route, 0)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if err := r.reloadProviderRegistry(request.Context()); err != nil {
		adminConfigurationError(writer, err)
		return
	}
	if err := r.auditAdminMutation(request, "route.create", "route", route.ID); err != nil {
		adminAuditError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(route.Revision))
	writeJSON(writer, http.StatusCreated, route)
}

func (r *Runtime) updateAdminRoute(writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	var input routeInput
	if err := decodeAdminJSON(request, &input); err != nil {
		adminBadRequest(writer, "invalid request")
		return
	}
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	current, err := r.store.GetRoute(request.Context(), chi.URLParam(request, "id"))
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if current.Revision != expected {
		adminPreconditionFailed(writer)
		return
	}
	route := input.route(current.ID, current.CreatedAt, time.Now().UTC())
	route.DeletedAt = current.DeletedAt
	route.LastTestStatus = current.LastTestStatus
	route.LastTestedAt = current.LastTestedAt
	route.LastTestLatencyMillis = current.LastTestLatencyMillis
	route.LastTestErrorClass = current.LastTestErrorClass
	route.LastTestRevision = current.LastTestRevision
	if err := route.Validate(); err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	if err := r.validateAdminRoute(request, route, current.ID); err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	route, err = r.store.PutRoute(request.Context(), route, expected)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if err := r.reloadProviderRegistry(request.Context()); err != nil {
		adminConfigurationError(writer, err)
		return
	}
	if err := r.auditAdminMutation(request, "route.update", "route", route.ID); err != nil {
		adminAuditError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(route.Revision))
	writeJSON(writer, http.StatusOK, route)
}

func (r *Runtime) deleteAdminRoute(writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	// Deleting is not undoable and a stolen session should not be enough to do
	// it. The revision precondition is checked first: it costs a header parse,
	// while the step-up costs an Argon2id verification, and a request that
	// cannot succeed anyway should not buy one.
	if !r.requireDestructiveStepUp(writer, request) {
		return
	}
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	route, err := r.store.GetRoute(request.Context(), chi.URLParam(request, "id"))
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if route.Revision != expected {
		adminPreconditionFailed(writer)
		return
	}
	now := time.Now().UTC()
	route.Enabled = false
	route.UpdatedAt = now
	route.DeletedAt = &now
	route, err = r.store.PutRoute(request.Context(), route, expected)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if err := r.reloadProviderRegistry(request.Context()); err != nil {
		adminConfigurationError(writer, err)
		return
	}
	if err := r.auditAdminMutation(request, "route.delete", "route", route.ID); err != nil {
		adminAuditError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(route.Revision))
	writer.WriteHeader(http.StatusNoContent)
}

func (r *Runtime) credentialFromInput(
	id string,
	input credentialInput,
	current *domain.Credential,
	createdAt time.Time,
) (domain.Credential, error) {
	if strings.TrimSpace(input.Name) == "" {
		return domain.Credential{}, errors.New("credential name is required")
	}
	if !implementedProviderType(input.Type) {
		return domain.Credential{}, errors.New("provider type is not implemented")
	}
	endpoint, err := safetransport.ValidateURL(input.BaseURL, safetransport.Policy{
		RequireHTTPS: true, AllowPrivate: r.config.Security.AllowPrivateProviderEndpoints,
	})
	if err != nil {
		return domain.Credential{}, err
	}
	audience, err := safetransport.Audience(input.BaseURL, string(input.Type))
	if err != nil {
		return domain.Credential{}, err
	}
	surface, scheme := input.AccessSurface, input.Scheme
	if current != nil && surface == "" && scheme == "" {
		surface, scheme = current.AccessSurface, current.Scheme
	}
	profile, ok := domain.ResolveCredentialProfile(input.Type, surface, scheme)
	if !ok {
		return domain.Credential{}, fmt.Errorf("credential access surface %q or scheme %q is incompatible with provider type %q", surface, scheme, input.Type)
	}
	if profile.AccessSurface == domain.SurfaceBedrockMantle {
		if err := bedrockmantleprovider.ValidateEndpoint(endpoint); err != nil {
			return domain.Credential{}, err
		}
	}
	var plaintext []byte
	if input.Secret != nil {
		plaintext = []byte(*input.Secret)
		if len(plaintext) == 0 || len(plaintext) > 16<<10 {
			clear(plaintext)
			return domain.Credential{}, errors.New("provider secret must contain 1 to 16384 bytes")
		}
	} else {
		if current == nil {
			return domain.Credential{}, errors.New("provider secret is required")
		}
		plaintext, err = r.vault.DecryptCredential(
			current.ID, string(current.Type), current.Audience, current.Ciphertext,
		)
		if err != nil {
			return domain.Credential{}, errors.New("stored credential could not be decrypted")
		}
	}
	defer clear(plaintext)
	ciphertext, err := r.vault.EncryptCredential(id, string(input.Type), audience, plaintext)
	if err != nil {
		return domain.Credential{}, err
	}
	now := time.Now().UTC()
	keyVersion := uint16(1)
	if current != nil {
		if current.KeyVersion == ^uint16(0) {
			return domain.Credential{}, errors.New("credential key version is exhausted")
		}
		keyVersion = current.KeyVersion + 1
	}
	credential := domain.Credential{
		ID: id, Name: input.Name, Type: input.Type, AccessSurface: profile.AccessSurface,
		Scheme: profile.CredentialScheme, Audience: audience,
		Ciphertext: ciphertext, KeyVersion: keyVersion, CreatedAt: createdAt, UpdatedAt: now,
	}
	return credential, credential.Validate()
}

func (r *Runtime) providerFromInput(
	request *http.Request,
	id string,
	input providerInput,
	currentProfile domain.ProviderProfileID,
	currentEvidence domain.CapabilityEvidenceSet,
	currentBindings []domain.ProviderProfileBinding,
	createdAt time.Time,
	updatedAt time.Time,
) (domain.ProviderInstance, error) {
	if !implementedProviderType(input.Type) {
		return domain.ProviderInstance{}, errors.New("provider type is not implemented")
	}
	endpoint, err := safetransport.ValidateURL(input.BaseURL, safetransport.Policy{
		RequireHTTPS: true, AllowPrivate: r.config.Security.AllowPrivateProviderEndpoints,
	})
	if err != nil {
		return domain.ProviderInstance{}, err
	}
	credential, err := r.store.GetCredential(request.Context(), input.CredentialID)
	if err != nil {
		return domain.ProviderInstance{}, errors.New("credential was not found")
	}
	audience, err := safetransport.Audience(input.BaseURL, string(input.Type))
	if err != nil {
		return domain.ProviderInstance{}, err
	}
	if credential.Type != input.Type || credential.Audience != audience {
		return domain.ProviderInstance{}, errors.New("credential type or audience does not match provider")
	}
	requestedProfile := input.ProfileID
	if requestedProfile == "" {
		requestedProfile = currentProfile
	}
	profile, ok := domain.ResolveProviderProfile(input.Type, requestedProfile)
	if !ok {
		return domain.ProviderInstance{}, errors.New("provider profile is not implemented")
	}
	if input.AccessSurface != "" && input.AccessSurface != profile.AccessSurface ||
		input.ProfileID != "" && input.ProfileID != profile.ProfileID ||
		input.CredentialScheme != "" && input.CredentialScheme != profile.CredentialScheme {
		return domain.ProviderInstance{}, errors.New("provider access surface, profile, or credential scheme is incompatible")
	}
	if credential.AccessSurface != profile.AccessSurface || credential.Scheme != profile.CredentialScheme {
		return domain.ProviderInstance{}, errors.New("credential access surface or scheme does not match provider")
	}
	if profile.AccessSurface == domain.SurfaceBedrockMantle {
		if err := bedrockmantleprovider.ValidateEndpoint(endpoint); err != nil {
			return domain.ProviderInstance{}, err
		}
	}
	instance := domain.ProviderInstance{
		ID: id, Name: input.Name, Type: input.Type, BaseURL: input.BaseURL,
		APIVersion:       strings.TrimSpace(input.APIVersion),
		CredentialID:     input.CredentialID,
		AccessSurface:    profile.AccessSurface,
		ProfileID:        profile.ProfileID,
		CredentialScheme: profile.CredentialScheme,
		AllowedHosts:     []string{strings.ToLower(endpoint.Hostname())},
		MaxConcurrency:   input.MaxConcurrency,
		Enabled:          input.Enabled, CreatedAt: createdAt, UpdatedAt: updatedAt,
	}
	if input.Capabilities == nil {
		instance.Capabilities = domain.DefaultProviderCapabilitiesForProfile(input.Type, profile.ProfileID)
	} else {
		instance.Capabilities = *input.Capabilities
		if !instance.Capabilities.AnyOperation() {
			return domain.ProviderInstance{}, errors.New("provider must declare at least one operation capability")
		}
		if isStrictOperationProfile(profile.ProfileID) &&
			!capabilitySubset(instance.Capabilities, domain.DefaultProviderCapabilitiesForProfile(input.Type, profile.ProfileID)) {
			return domain.ProviderInstance{}, errors.New("provider capabilities exceed the immutable operation profile")
		}
	}
	instance.CapabilityEvidence = preserveCapabilityEvidence(instance.Capabilities, currentEvidence)
	if input.Bindings != nil {
		if len(*input.Bindings) == 0 {
			return domain.ProviderInstance{}, errors.New("provider must contain at least one profile binding")
		}
		instance.Bindings = append([]domain.ProviderProfileBinding(nil), (*input.Bindings)...)
		for index := range instance.Bindings {
			binding := &instance.Bindings[index]
			binding.ProviderID = id
			binding.ID = domain.DefaultProviderProfileBindingID(id, binding.ProfileID)
			profile, ok := domain.ResolveProviderProfile(input.Type, binding.ProfileID)
			if !ok {
				return domain.ProviderInstance{}, errors.New("provider binding profile is not implemented")
			}
			binding.AccessSurface, binding.CredentialScheme = profile.AccessSurface, profile.CredentialScheme
			if binding.CredentialScheme != credential.Scheme || binding.AccessSurface != credential.AccessSurface {
				return domain.ProviderInstance{}, errors.New("provider binding credential profile does not match connection")
			}
			if isStrictOperationProfile(binding.ProfileID) &&
				!domain.ProviderCapabilitiesSubset(binding.Capabilities, domain.DefaultProviderCapabilitiesForProfile(input.Type, binding.ProfileID)) {
				return domain.ProviderInstance{}, errors.New("provider binding capabilities exceed the immutable operation profile")
			}
			var previous domain.CapabilityEvidenceSet
			for _, current := range currentBindings {
				if current.ID == binding.ID && current.ProfileID == binding.ProfileID {
					previous = current.CapabilityEvidence
					break
				}
			}
			binding.CapabilityEvidence = preserveCapabilityEvidence(binding.Capabilities, previous)
		}
		for index, binding := range instance.Bindings {
			if binding.Enabled {
				if index != 0 {
					instance.Bindings[0], instance.Bindings[index] = instance.Bindings[index], instance.Bindings[0]
				}
				break
			}
		}
		primary := instance.Bindings[0]
		instance.ProfileID, instance.AccessSurface, instance.CredentialScheme = primary.ProfileID, primary.AccessSurface, primary.CredentialScheme
		instance.Capabilities, instance.CapabilityEvidence = domain.BindingsCapabilitiesSummary(instance.Bindings)
	} else {
		instance.Bindings = []domain.ProviderProfileBinding{{
			ID: domain.DefaultProviderProfileBindingID(id, instance.ProfileID), ProviderID: id,
			ProfileID: instance.ProfileID, AccessSurface: instance.AccessSurface,
			CredentialScheme: instance.CredentialScheme, Capabilities: instance.Capabilities,
			CapabilityEvidence: instance.CapabilityEvidence.Clone(), Enabled: instance.Enabled,
		}}
		instance.Capabilities, instance.CapabilityEvidence = domain.BindingsCapabilitiesSummary(instance.Bindings)
	}
	return instance, instance.Validate()
}

func isStrictOperationProfile(id domain.ProviderProfileID) bool {
	switch id {
	case domain.ProfileOpenAIMediaResources, domain.ProfileBedrockInvokeTitanEmbedV2, domain.ProfileBedrockInvokeTitanImageV2, domain.ProfileBedrockAgentRerankCohere35, domain.ProfileBedrockAsyncNovaReel:
		return true
	default:
		return false
	}
}

func preserveCapabilityEvidence(capabilities domain.ProviderCapabilities, current domain.CapabilityEvidenceSet) domain.CapabilityEvidenceSet {
	evidence := domain.EvidenceForCapabilities(capabilities, domain.EvidenceDeclared)
	for name, value := range evidence {
		if value == domain.EvidenceUnsupported {
			continue
		}
		if previous := current[name]; previous != "" && previous != domain.EvidenceUnsupported {
			evidence[name] = previous
		}
	}
	return evidence
}

func (input routeInput) route(id string, createdAt, updatedAt time.Time) domain.Route {
	return domain.Route{
		ID: id, PublicModel: input.PublicModel, DeploymentID: input.DeploymentID, ProviderID: input.ProviderID,
		ProviderModel: input.ProviderModel, InputMicrosPerMillion: input.InputMicrosPerMillion,
		OutputMicrosPerMillion: input.OutputMicrosPerMillion, Priority: input.Priority,
		Strategy: input.Strategy, Enabled: input.Enabled, CreatedAt: createdAt, UpdatedAt: updatedAt,
	}
}

func (r *Runtime) validateAdminRoute(request *http.Request, candidate domain.Route, replacingID string) error {
	if candidate.DeploymentID != "" {
		deployment, err := r.store.GetDeployment(request.Context(), candidate.DeploymentID)
		if err != nil || deployment.DeletedAt != nil || (candidate.Enabled && !deployment.Enabled) {
			return errors.New("route deployment is unavailable")
		}
		instance, err := r.store.GetProvider(request.Context(), deployment.ProviderID)
		if err != nil || instance.DeletedAt != nil || (candidate.Enabled && !instance.Enabled) {
			return errors.New("route deployment provider is unavailable")
		}
		if err := bedrockprovider.ValidateProfileModel(instance.ProfileID, deployment.ProviderModel); err != nil {
			return err
		}
	} else {
		instance, err := r.store.GetProvider(request.Context(), candidate.ProviderID)
		if err != nil || instance.DeletedAt != nil || (candidate.Enabled && !instance.Enabled) {
			return errors.New("route provider is unavailable")
		}
		if len(instance.EffectiveProfileBindings()) != 1 {
			return errors.New("deployment_id is required for a provider with multiple profile bindings")
		}
		if err := bedrockprovider.ValidateProfileModel(instance.ProfileID, candidate.ProviderModel); err != nil {
			return err
		}
	}
	routes, err := r.store.ListRoutes(request.Context())
	if err != nil {
		return errors.New("routes are unavailable")
	}
	strategy := candidate.Strategy
	if strategy == "" {
		strategy = "ordered"
	}
	for _, route := range routes {
		if route.ID == replacingID || route.DeletedAt != nil || !route.Enabled ||
			!candidate.Enabled || route.PublicModel != candidate.PublicModel {
			continue
		}
		existingStrategy := route.Strategy
		if existingStrategy == "" {
			existingStrategy = "ordered"
		}
		if existingStrategy != strategy {
			return errors.New("all enabled routes for a public model must use the same strategy")
		}
	}
	return nil
}

func (r *Runtime) validateCredentialReferences(
	request *http.Request,
	credential domain.Credential,
) error {
	instances, err := r.store.ListProviders(request.Context())
	if err != nil {
		return errors.New("providers are unavailable")
	}
	for _, instance := range instances {
		if instance.CredentialID != credential.ID || instance.DeletedAt != nil {
			continue
		}
		audience, err := safetransport.Audience(instance.BaseURL, string(instance.Type))
		if err != nil || instance.Type != credential.Type || audience != credential.Audience {
			return errors.New("credential type or audience conflicts with an existing provider")
		}
	}
	return nil
}

func (r *Runtime) validateProviderCanDeactivate(
	request *http.Request,
	providerID string,
	willBeEnabled bool,
) error {
	if willBeEnabled {
		return nil
	}
	deployments, err := r.store.ListDeployments(request.Context())
	if err != nil {
		return errors.New("deployments are unavailable")
	}
	for _, deployment := range deployments {
		if deployment.ProviderID == providerID && deployment.Enabled && deployment.DeletedAt == nil {
			return errors.New("disable or delete the provider's active deployments first")
		}
	}
	routes, err := r.store.ListRoutes(request.Context())
	if err != nil {
		return errors.New("routes are unavailable")
	}
	for _, route := range routes {
		if route.DeploymentID == "" && route.ProviderID == providerID && route.Enabled && route.DeletedAt == nil {
			return errors.New("disable or delete the provider's active routes first")
		}
	}
	return nil
}

func implementedProviderType(value domain.ProviderType) bool {
	switch value {
	case domain.ProviderOpenAI, domain.ProviderAzureOpenAI,
		domain.ProviderAnthropic, domain.ProviderDeepSeek, domain.ProviderOpenAICompatible, domain.ProviderGemini,
		domain.ProviderBedrock:
		return true
	default:
		return false
	}
}

func credentialViewFrom(item domain.Credential) credentialView {
	return credentialView{
		ID: item.ID, Name: item.Name, Type: item.Type,
		AccessSurface: item.AccessSurface, Scheme: item.Scheme,
		BoundBaseURL:     strings.TrimSuffix(item.Audience, ":"+string(item.Type)),
		SecretConfigured: len(item.Ciphertext) > 0, KeyVersion: item.KeyVersion,
		Revision: item.Revision,
	}
}

func adminConfigurationError(writer http.ResponseWriter, err error) {
	writeJSON(writer, http.StatusConflict, map[string]string{
		"error": "configuration could not be activated",
	})
}
