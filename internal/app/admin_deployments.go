package app

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/id"
	"github.com/akz142857/Heimdall/internal/provider"
	"github.com/go-chi/chi/v5"
)

type deploymentInput struct {
	Name                   string                       `json:"name"`
	ProviderID             string                       `json:"provider_id"`
	ProviderModel          string                       `json:"provider_model"`
	Capabilities           *domain.ProviderCapabilities `json:"capabilities,omitempty"`
	InputMicrosPerMillion  int64                        `json:"input_micros_per_million"`
	OutputMicrosPerMillion int64                        `json:"output_micros_per_million"`
	MaxConcurrency         int64                        `json:"max_concurrency"`
	Priority               int                          `json:"priority"`
	Weight                 int                          `json:"weight"`
	Enabled                bool                         `json:"enabled"`
}

func (r *Runtime) createAdminDeployment(writer http.ResponseWriter, request *http.Request) {
	var input deploymentInput
	if err := decodeAdminJSON(request, &input); err != nil {
		adminBadRequest(writer, "invalid request")
		return
	}
	deploymentID, err := id.New("dep")
	if err != nil {
		adminStoreError(writer)
		return
	}
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	now := time.Now().UTC()
	deployment, err := r.deploymentFromInput(request, deploymentID, input, now, now)
	if err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	deployment, err = r.store.PutDeployment(request.Context(), deployment, 0)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if err := r.reloadProviderRegistry(request.Context()); err != nil {
		adminConfigurationError(writer, err)
		return
	}
	if err := r.auditAdminMutation(request, "deployment.create", "deployment", deployment.ID); err != nil {
		adminAuditError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(deployment.Revision))
	writeJSON(writer, http.StatusCreated, deployment)
}

func (r *Runtime) updateAdminDeployment(writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	var input deploymentInput
	if err := decodeAdminJSON(request, &input); err != nil {
		adminBadRequest(writer, "invalid request")
		return
	}
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	current, err := r.store.GetDeployment(request.Context(), chi.URLParam(request, "id"))
	if err != nil || current.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	if current.Revision != expected {
		adminPreconditionFailed(writer)
		return
	}
	deployment, err := r.deploymentFromInput(request, current.ID, input, current.CreatedAt, time.Now().UTC())
	if err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	deployment.DeletedAt = current.DeletedAt
	if err := r.validateDeploymentCanDeactivate(request, deployment.ID, deployment.Enabled); err != nil {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	deployment, err = r.store.PutDeployment(request.Context(), deployment, expected)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if err := r.reloadProviderRegistry(request.Context()); err != nil {
		adminConfigurationError(writer, err)
		return
	}
	if err := r.auditAdminMutation(request, "deployment.update", "deployment", deployment.ID); err != nil {
		adminAuditError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(deployment.Revision))
	writeJSON(writer, http.StatusOK, deployment)
}

func (r *Runtime) deleteAdminDeployment(writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	deployment, err := r.store.GetDeployment(request.Context(), chi.URLParam(request, "id"))
	if err != nil || deployment.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	if deployment.Revision != expected {
		adminPreconditionFailed(writer)
		return
	}
	if err := r.validateDeploymentCanDeactivate(request, deployment.ID, false); err != nil {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	now := time.Now().UTC()
	deployment.Enabled = false
	deployment.UpdatedAt = now
	deployment.DeletedAt = &now
	deployment, err = r.store.PutDeployment(request.Context(), deployment, expected)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if err := r.reloadProviderRegistry(request.Context()); err != nil {
		adminConfigurationError(writer, err)
		return
	}
	if err := r.auditAdminMutation(request, "deployment.delete", "deployment", deployment.ID); err != nil {
		adminAuditError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(deployment.Revision))
	writer.WriteHeader(http.StatusNoContent)
}

func (r *Runtime) testAdminDeployment(writer http.ResponseWriter, request *http.Request) {
	deployment, err := r.store.GetDeployment(request.Context(), chi.URLParam(request, "id"))
	if err != nil || deployment.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	if !deployment.Enabled {
		adminBadRequest(writer, "deployment is disabled")
		return
	}
	instance, err := r.store.GetProvider(request.Context(), deployment.ProviderID)
	if err != nil || !instance.Enabled || instance.DeletedAt != nil {
		adminBadRequest(writer, "deployment provider is unavailable")
		return
	}
	adapter, ok := r.providers.AdapterForProvider(deployment.ProviderID)
	if !ok {
		adminBadRequest(writer, "deployment provider adapter is unavailable")
		return
	}
	prober, ok := adapter.(provider.Prober)
	if !ok {
		adminBadRequest(writer, "provider does not support connection testing")
		return
	}
	r.runAdminProbe(writer, request, prober, deployment.ProviderModel, "deployment", deployment.ID, deployment.Capabilities)
}

func (r *Runtime) deploymentFromInput(request *http.Request, deploymentID string, input deploymentInput, createdAt, updatedAt time.Time) (domain.Deployment, error) {
	instance, err := r.store.GetProvider(request.Context(), input.ProviderID)
	if err != nil || instance.DeletedAt != nil || (input.Enabled && !instance.Enabled) {
		return domain.Deployment{}, errors.New("deployment provider is unavailable")
	}
	capabilities := normalizedProviderCapabilities(instance)
	if input.Capabilities != nil {
		capabilities = *input.Capabilities
		if !capabilitySubset(capabilities, normalizedProviderCapabilities(instance)) {
			return domain.Deployment{}, errors.New("deployment capabilities exceed provider capabilities")
		}
	}
	weight := input.Weight
	if weight == 0 {
		weight = 1
	}
	deployment := domain.Deployment{
		ID: deploymentID, Name: strings.TrimSpace(input.Name), ProviderID: input.ProviderID,
		ProviderModel: strings.TrimSpace(input.ProviderModel), Capabilities: capabilities,
		InputMicrosPerMillion:  input.InputMicrosPerMillion,
		OutputMicrosPerMillion: input.OutputMicrosPerMillion,
		MaxConcurrency:         input.MaxConcurrency, Priority: input.Priority, Weight: weight,
		Enabled: input.Enabled, CreatedAt: createdAt, UpdatedAt: updatedAt,
	}
	return deployment, deployment.Validate()
}

func capabilitySubset(candidate, available domain.ProviderCapabilities) bool {
	return (!candidate.Chat || available.Chat) &&
		(!candidate.Streaming || available.Streaming) &&
		(!candidate.Embeddings || available.Embeddings) &&
		(!candidate.Tools || available.Tools) &&
		(!candidate.Vision || available.Vision) &&
		(!candidate.JSONMode || available.JSONMode) &&
		(!candidate.DeveloperRole || available.DeveloperRole) &&
		(!candidate.Reasoning || available.Reasoning) &&
		(!candidate.StreamUsage || available.StreamUsage) &&
		capabilityLimitSubset(candidate.MaxContextTokens, available.MaxContextTokens) &&
		capabilityLimitSubset(candidate.MaxOutputTokens, available.MaxOutputTokens)
}

func capabilityLimitSubset(candidate, available int64) bool {
	if available == 0 {
		return candidate >= 0
	}
	return candidate > 0 && candidate <= available
}

func (r *Runtime) validateDeploymentCanDeactivate(request *http.Request, deploymentID string, willBeEnabled bool) error {
	if willBeEnabled {
		return nil
	}
	routes, err := r.store.ListRoutes(request.Context())
	if err != nil {
		return errors.New("routes are unavailable")
	}
	for _, route := range routes {
		if route.DeploymentID == deploymentID && route.Enabled && route.DeletedAt == nil {
			return errors.New("disable or delete the deployment's active routes first")
		}
	}
	return nil
}
