package app

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/id"
	"github.com/akz142857/Halro/internal/modelcatalog"
	"github.com/akz142857/Halro/internal/provider"
	bedrockprovider "github.com/akz142857/Halro/internal/provider/bedrock"
	"github.com/go-chi/chi/v5"
)

type deploymentInput struct {
	Name          string                       `json:"name"`
	ProviderID    string                       `json:"provider_id"`
	ProviderModel string                       `json:"provider_model"`
	TargetKind    domain.DeploymentTargetKind  `json:"target_kind,omitempty"`
	AccessSurface domain.AccessSurface         `json:"access_surface,omitempty"`
	ProfileID     domain.ProviderProfileID     `json:"profile_id,omitempty"`
	BindingID     string                       `json:"binding_id,omitempty"`
	Region        string                       `json:"region"`
	Capabilities  *domain.ProviderCapabilities `json:"capabilities,omitempty"`
	// ModelRevision is the per-model catalog revision the client read. An empty
	// value skips the check; a stale one is a conflict, never a silent accept.
	ModelRevision string `json:"model_revision,omitempty"`
	// Mode must be operator_declared for a model the catalog does not cover.
	// Requiring the word keeps "I know what this model does" an explicit act
	// rather than something inferred from a filled-in form.
	Mode           string `json:"mode,omitempty"`
	MaxConcurrency int64  `json:"max_concurrency"`
	Priority       int    `json:"priority"`
	Weight         int    `json:"weight"`
	Enabled        bool   `json:"enabled"`
}

const deploymentModeOperatorDeclared = "operator_declared"

// adminDeploymentInputError separates "you asked for something impossible" from
// "the catalog moved under you". The second is a conflict with a stable code:
// the console refreshes the model and the operator re-confirms, rather than the
// server quietly applying whatever the catalog says now.
func adminDeploymentInputError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errModelCapabilityChanged):
		writeJSON(writer, http.StatusConflict, map[string]string{
			"error": err.Error(), "code": "model_capability_changed",
		})
	case errors.Is(err, errModelCapabilitiesUnknown):
		writeJSON(writer, http.StatusBadRequest, map[string]string{
			"error": err.Error(), "code": "model_capabilities_unknown",
		})
	default:
		adminBadRequest(writer, err.Error())
	}
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
	deployment, err := r.deploymentFromInput(request, deploymentID, input, nil, nil, now, now)
	if err != nil {
		adminDeploymentInputError(writer, err)
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
	deployment, err := r.deploymentFromInput(request, current.ID, input, currentEvidence, &current.Capabilities, current.CreatedAt, time.Now().UTC())
	if err != nil {
		adminDeploymentInputError(writer, err)
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
	// Legacy fields remain readable for migration compatibility, but price writes
	// are exclusively handled by DeploymentPriceVersion endpoints.
	deployment.InputMicrosPerMillion = current.InputMicrosPerMillion
	deployment.OutputMicrosPerMillion = current.OutputMicrosPerMillion
	deployment.FixedRequestMicrosUSD = current.FixedRequestMicrosUSD
	if deployment.Enabled && !current.Enabled &&
		(current.LastTestStatus != domain.DeploymentTestHealthy || current.LastTestRevision != current.Revision) {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "deployment must pass a current validation test before enable"})
		return
	}
	if deployment.Enabled {
		if _, err := r.store.SelectDeploymentPriceVersion(request.Context(), deployment.ID, time.Now().UTC()); err != nil {
			writeJSON(writer, http.StatusConflict, map[string]string{
				"error": "deployment requires an effective versioned price before enable",
				"code":  "deployment_price_unavailable",
			})
			return
		}
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
	// Deleting is not undoable and a stolen session should not be enough to do
	// it. The revision precondition is checked first: it costs a header parse,
	// while the step-up costs an Argon2id verification, and a request that
	// cannot succeed anyway should not buy one.
	if !r.requireDestructiveStepUp(writer, request) {
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

func (r *Runtime) deploymentFromInput(request *http.Request, deploymentID string, input deploymentInput, currentEvidence domain.CapabilityEvidenceSet, priorCapabilities *domain.ProviderCapabilities, createdAt, updatedAt time.Time) (domain.Deployment, error) {
	instance, err := r.store.GetProvider(request.Context(), input.ProviderID)
	if err != nil || instance.DeletedAt != nil || (input.Enabled && !instance.Enabled) {
		return domain.Deployment{}, errors.New("deployment provider is unavailable")
	}
	model := strings.TrimSpace(input.ProviderModel)
	resolution, err := resolveDeploymentTarget(instance, input, model, deploymentRegion(instance, input), priorCapabilities)
	if err != nil {
		return domain.Deployment{}, err
	}
	binding, capabilities := resolution.binding, resolution.capabilities
	if !domain.ProviderCapabilitiesSubset(capabilities, binding.Capabilities) {
		return domain.Deployment{}, errors.New("deployment capabilities exceed provider capabilities")
	}
	if input.AccessSurface != "" && input.AccessSurface != binding.AccessSurface {
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
	region := deploymentRegion(instance, input)
	snapshot := domain.ModelCapabilitySnapshot{
		ProviderModel: model,
		ModelRevision: resolution.entry.Revision(),
		Source:        string(resolution.entry.Source),
		Status:        string(resolution.entry.Status),
		CapturedAt:    updatedAt,
		Capabilities:  resolution.capabilities,
	}
	if resolution.declared {
		// An operator declaration is its own source. Recording it as the catalog
		// would let a later refresh look like agreement that never happened.
		snapshot.Source = string(modelcatalog.SourceOperatorDeclared)
		snapshot.Status = string(modelcatalog.StatusPartial)
	} else {
		snapshot.CatalogRevision = modelcatalog.Builtin().Revision()
		snapshot.Capabilities = modelcatalog.Clamp(resolution.entry.Capabilities, resolution.binding.Capabilities)
	}
	deployment := domain.Deployment{
		ID: deploymentID, Name: strings.TrimSpace(input.Name), ProviderID: input.ProviderID,
		ProviderModel: model, TargetKind: targetKind, Capabilities: capabilities,
		ModelCapabilitySnapshot: snapshot, CapabilityReviewState: domain.CapabilityReviewCurrent,
		AccessSurface: binding.AccessSurface, ProfileID: binding.ProfileID, BindingID: binding.ID,
		Region:             region,
		CapabilityEvidence: evidence,
		MaxConcurrency:     input.MaxConcurrency, Priority: input.Priority, Weight: weight,
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

// deploymentRegion is the region the deployment runs in, taken from the request
// or derived from a regional provider's endpoint. The catalog is keyed on it
// because the same identifier can behave differently per region.
func deploymentRegion(instance domain.ProviderInstance, input deploymentInput) string {
	if region := strings.TrimSpace(input.Region); region != "" {
		return region
	}
	return providerRegion(instance)
}

// providerRegion derives the region a regional provider's endpoint points at.
// Model discovery and deployment creation must agree on it: they key the same
// catalog lookup, and a mismatch would make every create carrying a revision
// read from the listing conflict.
func providerRegion(instance domain.ProviderInstance) string {
	if !strings.HasPrefix(string(instance.AccessSurface), "bedrock-") {
		return ""
	}
	parsed, err := url.Parse(instance.BaseURL)
	if err != nil {
		return ""
	}
	parts := strings.Split(parsed.Hostname(), ".")
	if len(parts) > 1 {
		return parts[1]
	}
	return ""
}

// errModelCapabilityChanged is returned when the catalog moved under a client
// that was mid-edit. It carries a distinct status so the console can refresh
// and re-confirm rather than silently accepting whatever the catalog says now.
var errModelCapabilityChanged = errors.New("model capabilities changed since they were read; refresh and confirm")

// errModelCapabilitiesUnknown is returned when nothing establishes what a model
// does. It is not an outage: the operator declares the model explicitly.
var errModelCapabilitiesUnknown = errors.New("model capabilities are unknown; declare them explicitly with mode=operator_declared")

// deploymentResolution is what the server decided, from the catalog and the
// provider topology, rather than from what the client claimed.
type deploymentResolution struct {
	binding      domain.ProviderProfileBinding
	capabilities domain.ProviderCapabilities
	entry        modelcatalog.Entry
	declared     bool
}

// resolveDeploymentTarget picks the binding a model should run through and the
// capabilities it may keep.
//
// binding_id and profile_id from the client are treated as filters, never as
// authority: a client cannot name a binding to obtain capabilities the catalog
// does not support for that model.
func resolveDeploymentTarget(instance domain.ProviderInstance, input deploymentInput, model, region string, prior *domain.ProviderCapabilities) (deploymentResolution, error) {
	var candidates []domain.ProviderProfileBinding
	for _, binding := range instance.EffectiveProfileBindings() {
		if !binding.Enabled {
			continue
		}
		if input.BindingID != "" && binding.ID != input.BindingID {
			continue
		}
		if input.ProfileID != "" && binding.ProfileID != input.ProfileID {
			continue
		}
		candidates = append(candidates, binding)
	}
	if len(candidates) == 0 {
		return deploymentResolution{}, errors.New("deployment provider profile binding is unavailable")
	}
	slices.SortFunc(candidates, func(left, right domain.ProviderProfileBinding) int {
		return strings.Compare(string(left.ProfileID), string(right.ProfileID))
	})

	// Omitting capabilities on an edit means "leave them as they are". It used
	// to mean "inherit the profile ceiling", which is the default this whole
	// change exists to remove.
	retained := input.Capabilities
	if retained == nil {
		retained = prior
	}
	var known, unknown []deploymentResolution
	for _, binding := range candidates {
		key := modelcatalog.Key{ProviderType: instance.Type, Profile: binding.ProfileID, Model: model, Region: region}
		entry, found := modelcatalog.Builtin().Lookup(key)
		resolution := deploymentResolution{
			binding:      binding,
			capabilities: modelcatalog.Clamp(entry.Capabilities, binding.Capabilities),
			entry:        entry,
		}
		if found && entry.Status == modelcatalog.StatusKnown {
			known = append(known, resolution)
			continue
		}
		unknown = append(unknown, resolution)
	}

	// A known model narrows to what the catalog established for it.
	for _, resolution := range known {
		if retained == nil || domain.ProviderCapabilitiesSubset(*retained, resolution.capabilities) {
			if retained != nil {
				resolution.capabilities = *retained
			}
			return resolution, checkModelRevision(input.ModelRevision, resolution.entry)
		}
	}
	if len(known) > 0 {
		return deploymentResolution{}, errors.New("deployment capabilities exceed what the catalog establishes for this model")
	}

	// Nothing is established. The operator may still deploy the model, but the
	// declaration has to be explicit and stays Declared evidence.
	// Declaring is an act the operator performs once. An edit that stays inside
	// an existing declaration is not a new claim, and narrowing one is a
	// reduction — neither should demand the word again. Anything wider does.
	covered := prior != nil && retained != nil && domain.ProviderCapabilitiesSubset(*retained, *prior)
	if input.Mode != deploymentModeOperatorDeclared && !covered {
		return deploymentResolution{}, errModelCapabilitiesUnknown
	}
	if retained == nil || !retained.AnyOperation() {
		return deploymentResolution{}, errors.New("an operator-declared model must declare at least one core operation")
	}
	if err := modelcatalog.ValidateDependencies(*retained); err != nil {
		return deploymentResolution{}, err
	}
	for _, resolution := range unknown {
		if domain.ProviderCapabilitiesSubset(*retained, resolution.binding.Capabilities) {
			resolution.capabilities = *retained
			resolution.declared = true
			return resolution, checkModelRevision(input.ModelRevision, resolution.entry)
		}
	}
	return deploymentResolution{}, errors.New("no enabled provider binding supports the declared capabilities")
}

// checkModelRevision compares the revision the client read against the one that
// applies now. It is per-model on purpose: a catalog-wide digest would rotate
// whenever any unrelated model appeared, and operators would learn to retry
// through the conflict until it meant nothing.
func checkModelRevision(claimed string, entry modelcatalog.Entry) error {
	if claimed == "" || claimed == entry.Revision() {
		return nil
	}
	return errModelCapabilityChanged
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
