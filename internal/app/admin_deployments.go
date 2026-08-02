package app

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/id"
	"github.com/akz142857/Heimdall/internal/provider"
	bedrockprovider "github.com/akz142857/Heimdall/internal/provider/bedrock"
	"github.com/go-chi/chi/v5"
)

type deploymentInput struct {
	Name                   string                       `json:"name"`
	ProviderID             string                       `json:"provider_id"`
	ProviderModel          string                       `json:"provider_model"`
	TargetKind             domain.DeploymentTargetKind  `json:"target_kind,omitempty"`
	AccessSurface          domain.AccessSurface         `json:"access_surface,omitempty"`
	ProfileID              domain.ProviderProfileID     `json:"profile_id,omitempty"`
	BindingID              string                       `json:"binding_id,omitempty"`
	Region                 string                       `json:"region"`
	Capabilities           *domain.ProviderCapabilities `json:"capabilities,omitempty"`
	InputMicrosPerMillion  int64                        `json:"input_micros_per_million"`
	OutputMicrosPerMillion int64                        `json:"output_micros_per_million"`
	FixedRequestMicrosUSD  int64                        `json:"fixed_request_micros_usd"`
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
	if input.Enabled {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "new deployments must be saved disabled and pass validation before enable"})
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
	deployment, err := r.deploymentFromInput(request, deploymentID, input, nil, now, now)
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
	currentEvidence := domain.CapabilityEvidenceSet(nil)
	if input.ProviderID == current.ProviderID && strings.TrimSpace(input.ProviderModel) == current.ProviderModel &&
		(input.BindingID == "" || input.BindingID == current.BindingID) &&
		(input.ProfileID == "" || input.ProfileID == current.ProfileID) {
		currentEvidence = current.CapabilityEvidence
	}
	deployment, err := r.deploymentFromInput(request, current.ID, input, currentEvidence, current.CreatedAt, time.Now().UTC())
	if err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	targetChanged := deployment.ProviderID != current.ProviderID ||
		deployment.ProviderModel != current.ProviderModel ||
		deployment.TargetKind != current.TargetKind && current.TargetKind != "" ||
		deployment.BindingID != current.BindingID ||
		deployment.ProfileID != current.ProfileID ||
		deployment.AccessSurface != current.AccessSurface
	if targetChanged {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "deployment target identity is immutable; create and validate a replacement deployment"})
		return
	}
	deployment.DeletedAt = current.DeletedAt
	deployment.LastTestStatus = current.LastTestStatus
	deployment.LastTestedAt = current.LastTestedAt
	deployment.LastTestLatencyMillis = current.LastTestLatencyMillis
	deployment.LastTestErrorClass = current.LastTestErrorClass
	deployment.LastTestRevision = current.LastTestRevision
	if deployment.Enabled && !current.Enabled &&
		(current.LastTestStatus != domain.DeploymentTestHealthy || current.LastTestRevision != current.Revision) {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "deployment must pass a current validation test before enable"})
		return
	}
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
	instance, err := r.store.GetProvider(request.Context(), deployment.ProviderID)
	if err != nil || !instance.Enabled || instance.DeletedAt != nil {
		adminBadRequest(writer, "deployment provider is unavailable")
		return
	}
	adapter, ok := adapterForDeployment(r.providers, instance, deployment)
	if !ok {
		adminBadRequest(writer, "deployment provider adapter is unavailable")
		return
	}
	prober, ok := adapter.(provider.Prober)
	if !ok {
		adminBadRequest(writer, "provider does not support connection testing")
		return
	}
	testedRevision := deployment.Revision
	timeout := r.config.Gateway.AttemptResponseHeaderTimeout.Value()
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(request.Context(), timeout)
	defer cancel()
	started := time.Now()
	probeErr := prober.Probe(ctx, deployment.ProviderModel)
	testedAt := time.Now().UTC()
	latencyMS := time.Since(started).Milliseconds()
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
	current, storeErr := r.store.GetDeployment(request.Context(), deployment.ID)
	if storeErr != nil || current.DeletedAt != nil {
		r.adminTopologyMu.Unlock()
		adminNotFound(writer)
		return
	}
	if current.Revision != testedRevision {
		r.adminTopologyMu.Unlock()
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "deployment changed during validation; test the current revision again"})
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
	current, storeErr = r.store.PutDeployment(request.Context(), current, testedRevision)
	r.adminTopologyMu.Unlock()
	if storeErr != nil {
		adminMutationError(writer, storeErr)
		return
	}
	action := "deployment.test.success"
	if probeErr != nil {
		action = "deployment.test.failure"
	}
	if err := r.auditAdminMutation(request, action, "deployment", deployment.ID); err != nil {
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
	result["capabilities"] = current.Capabilities
	writeJSON(writer, http.StatusOK, result)
}

func (r *Runtime) deploymentFromInput(request *http.Request, deploymentID string, input deploymentInput, currentEvidence domain.CapabilityEvidenceSet, createdAt, updatedAt time.Time) (domain.Deployment, error) {
	instance, err := r.store.GetProvider(request.Context(), input.ProviderID)
	if err != nil || instance.DeletedAt != nil || (input.Enabled && !instance.Enabled) {
		return domain.Deployment{}, errors.New("deployment provider is unavailable")
	}
	binding, err := resolveDeploymentBinding(instance, input.BindingID, input.ProfileID)
	if err != nil {
		return domain.Deployment{}, err
	}
	capabilities := binding.Capabilities
	if input.Capabilities != nil {
		capabilities = *input.Capabilities
		if !domain.ProviderCapabilitiesSubset(capabilities, binding.Capabilities) {
			return domain.Deployment{}, errors.New("deployment capabilities exceed provider capabilities")
		}
	}
	if input.AccessSurface != "" && input.AccessSurface != binding.AccessSurface ||
		input.ProfileID != "" && input.ProfileID != binding.ProfileID {
		return domain.Deployment{}, errors.New("deployment access surface or profile does not match provider")
	}
	if err := bedrockprovider.ValidateProfileModel(binding.ProfileID, input.ProviderModel); err != nil {
		return domain.Deployment{}, err
	}
	targetKind, err := deploymentTargetKind(instance.Type, binding.AccessSurface, binding.ProfileID, input.TargetKind)
	if err != nil {
		return domain.Deployment{}, err
	}
	evidence := deploymentCapabilityEvidence(capabilities, binding.CapabilityEvidence, currentEvidence)
	for name, value := range evidence {
		if !capabilitiesEnabledByName(capabilities, name) && value != domain.EvidenceUnsupported {
			return domain.Deployment{}, errors.New("deployment capability evidence exceeds enabled capabilities")
		}
		if providerValue, ok := binding.CapabilityEvidence[name]; ok && evidenceRank(value) > evidenceRank(providerValue) {
			return domain.Deployment{}, errors.New("deployment capability evidence exceeds provider evidence")
		}
	}
	weight := input.Weight
	if weight == 0 {
		weight = 1
	}
	region := strings.TrimSpace(input.Region)
	if region == "" && strings.HasPrefix(string(instance.AccessSurface), "bedrock-") {
		if parsed, parseErr := url.Parse(instance.BaseURL); parseErr == nil {
			parts := strings.Split(parsed.Hostname(), ".")
			if len(parts) > 1 {
				region = parts[1]
			}
		}
	}
	deployment := domain.Deployment{
		ID: deploymentID, Name: strings.TrimSpace(input.Name), ProviderID: input.ProviderID,
		ProviderModel: strings.TrimSpace(input.ProviderModel), TargetKind: targetKind, Capabilities: capabilities,
		AccessSurface: binding.AccessSurface, ProfileID: binding.ProfileID, BindingID: binding.ID,
		Region:                 region,
		CapabilityEvidence:     evidence,
		InputMicrosPerMillion:  input.InputMicrosPerMillion,
		OutputMicrosPerMillion: input.OutputMicrosPerMillion,
		FixedRequestMicrosUSD:  input.FixedRequestMicrosUSD,
		MaxConcurrency:         input.MaxConcurrency, Priority: input.Priority, Weight: weight,
		Enabled: input.Enabled, CreatedAt: createdAt, UpdatedAt: updatedAt,
	}
	return deployment, deployment.Validate()
}

func deploymentTargetKind(providerType domain.ProviderType, surface domain.AccessSurface, profileID domain.ProviderProfileID, requested domain.DeploymentTargetKind) (domain.DeploymentTargetKind, error) {
	if requested == "" {
		switch {
		case providerType == domain.ProviderAzureOpenAI:
			return domain.TargetAzureDeployment, nil
		case providerType == domain.ProviderOpenAICompatible:
			return domain.TargetCustomEndpointModel, nil
		case providerType == domain.ProviderBedrock && surface != domain.SurfaceBedrockMantle:
			return domain.TargetBedrockFoundationModel, nil
		default:
			return domain.TargetModelID, nil
		}
	}
	switch {
	case providerType == domain.ProviderAzureOpenAI && requested == domain.TargetAzureDeployment:
		return requested, nil
	case providerType == domain.ProviderOpenAICompatible && (requested == domain.TargetCustomEndpointModel || requested == domain.TargetModelID):
		return requested, nil
	case providerType == domain.ProviderBedrock && surface == domain.SurfaceBedrockMantle && requested == domain.TargetModelID:
		return requested, nil
	case providerType == domain.ProviderBedrock && surface != domain.SurfaceBedrockMantle && profileID != domain.ProfileBedrockConverseText && requested == domain.TargetBedrockFoundationModel:
		return requested, nil
	case providerType == domain.ProviderBedrock && surface != domain.SurfaceBedrockMantle && profileID == domain.ProfileBedrockConverseText &&
		(requested == domain.TargetBedrockFoundationModel || requested == domain.TargetBedrockInferenceProfile || requested == domain.TargetBedrockProvisionedThroughput):
		return requested, nil
	case providerType != domain.ProviderAzureOpenAI && providerType != domain.ProviderBedrock && providerType != domain.ProviderOpenAICompatible && requested == domain.TargetModelID:
		return requested, nil
	default:
		return "", errors.New("deployment target kind is incompatible with provider or access surface")
	}
}

func resolveDeploymentBinding(instance domain.ProviderInstance, bindingID string, profileID domain.ProviderProfileID) (domain.ProviderProfileBinding, error) {
	bindings := instance.EffectiveProfileBindings()
	if bindingID != "" {
		binding, ok := instance.ProfileBinding(bindingID)
		if !ok || !binding.Enabled {
			return domain.ProviderProfileBinding{}, errors.New("deployment provider profile binding is unavailable")
		}
		return binding, nil
	}
	var matches []domain.ProviderProfileBinding
	for _, binding := range bindings {
		if binding.Enabled && (profileID == "" || binding.ProfileID == profileID) {
			matches = append(matches, binding)
		}
	}
	if len(matches) != 1 {
		return domain.ProviderProfileBinding{}, errors.New("binding_id is required when provider profile binding is ambiguous")
	}
	return matches[0], nil
}

func deploymentCapabilityEvidence(capabilities domain.ProviderCapabilities, providerEvidence, currentEvidence domain.CapabilityEvidenceSet) domain.CapabilityEvidenceSet {
	evidence := providerEvidence.Clone()
	for name, value := range evidence {
		if !capabilitiesEnabledByName(capabilities, name) {
			evidence[name] = domain.EvidenceUnsupported
			continue
		}
		providerValue := value
		if value == domain.EvidenceVerified {
			evidence[name] = domain.EvidenceDeclared
		}
		if previous := currentEvidence[name]; previous != "" && previous != domain.EvidenceUnsupported &&
			evidenceRank(previous) <= evidenceRank(providerValue) {
			evidence[name] = previous
		}
	}
	return evidence
}

func capabilitiesEnabledByName(value domain.ProviderCapabilities, name string) bool {
	switch name {
	case "chat":
		return value.Chat
	case "streaming":
		return value.Streaming
	case "embeddings":
		return value.Embeddings
	case "tools":
		return value.Tools
	case "vision":
		return value.Vision
	case "json_mode":
		return value.JSONMode
	case "developer_role":
		return value.DeveloperRole
	case "reasoning":
		return value.Reasoning
	case "stream_usage":
		return value.StreamUsage
	case "moderations":
		return value.Moderations
	case "images":
		return value.Images
	case "transcriptions":
		return value.Transcriptions
	case "speech":
		return value.Speech
	case "files":
		return value.Files
	case "batches":
		return value.Batches
	case "rerank":
		return value.Rerank
	case "async_generate":
		return value.AsyncGenerate
	default:
		return false
	}
}

func evidenceRank(value domain.CapabilityEvidence) int {
	switch value {
	case domain.EvidenceVerified:
		return 3
	case domain.EvidenceDeclared:
		return 2
	case domain.EvidenceLegacy:
		return 1
	default:
		return 0
	}
}

func capabilitySubset(candidate, available domain.ProviderCapabilities) bool {
	return domain.ProviderCapabilitiesSubset(candidate, available)
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
