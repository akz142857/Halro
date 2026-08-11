package app

import (
	"bytes"
	"context"
	"encoding/json"
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
	Name          string `json:"name"`
	ProviderID    string `json:"provider_id"`
	ProviderModel string `json:"provider_model"`
	// CapabilityModel is the reviewed model behind an operator-named Azure
	// Deployment or custom endpoint target. It never replaces ProviderModel in
	// an upstream request; it only bounds the capability declaration.
	CapabilityModel string                       `json:"capability_model,omitempty"`
	TargetKind      domain.DeploymentTargetKind  `json:"target_kind,omitempty"`
	AccessSurface   domain.AccessSurface         `json:"access_surface,omitempty"`
	ProfileID       domain.ProviderProfileID     `json:"profile_id,omitempty"`
	BindingID       string                       `json:"binding_id,omitempty"`
	Region          string                       `json:"region"`
	Capabilities    *domain.ProviderCapabilities `json:"capabilities,omitempty"`
	// ModelRevision is the per-model catalog revision the client read. An empty
	// value skips the check; a stale one is a conflict, never a silent accept.
	ModelRevision string `json:"model_revision,omitempty"`
	// ResolutionRevision binds this write to the exact target and single
	// Deployment Variant the operator reviewed.
	ResolutionRevision          string `json:"resolution_revision,omitempty"`
	CapabilityDetectionID       string `json:"capability_detection_id,omitempty"`
	CapabilityDetectionRevision uint64 `json:"capability_detection_revision,omitempty"`
	// OperationBindings is accepted by the decoder only so it can be refused by
	// name. It is the shape a deployment would take if it mapped each operation
	// to its own internal binding, and it is not coming: a deployment carries one
	// model's own capabilities, and composing several into one outward-facing
	// model is what routes are for. Refusing it here rather than through the
	// decoder's unknown-field rule is what lets the refusal say that.
	OperationBindings json.RawMessage `json:"operation_bindings,omitempty"`
	// Mode must be operator_declared for a model the catalog does not cover.
	// Requiring the word keeps "I know what this model does" an explicit act
	// rather than something inferred from a filled-in form.
	Mode           string `json:"mode,omitempty"`
	MaxConcurrency int64  `json:"max_concurrency"`
	Enabled        bool   `json:"enabled"`
}

const deploymentModeOperatorDeclared = "operator_declared"

// errOperationBindingsUnavailable answers a request that tries to give one
// deployment several internal bindings.
//
// The design that proposed it has been withdrawn rather than deferred, so this
// is a permanent refusal and the message says what to do instead. Composition
// already works at the route layer: several routes may share one public model,
// and the router picks a candidate per core operation, so a model whose
// capabilities span two profiles is two deployments behind one public model.
var errOperationBindingsUnavailable = errors.New(
	"a deployment carries one model's own capabilities through one internal binding; " +
		"to serve several capabilities under one public model, create one deployment per internal binding " +
		"and point a route at each from the same public model")

// refuseOperationBindings reports whether the request tried to set them. A JSON
// null is not an attempt: it says nothing, the same as omitting the field.
func refuseOperationBindings(input deploymentInput) error {
	encoded := bytes.TrimSpace(input.OperationBindings)
	if len(encoded) == 0 || string(encoded) == "null" {
		return nil
	}
	return errOperationBindingsUnavailable
}

// adminDeploymentInputError separates "you asked for something impossible" from
// "the catalog moved under you". The second is a conflict with a stable code:
// the console refreshes the model and the operator re-confirms, rather than the
// server quietly applying whatever the catalog says now.
func (r *Runtime) adminDeploymentInputError(writer http.ResponseWriter, request *http.Request, err error) {
	var changed *resolutionChangedError
	switch {
	case errors.As(err, &changed):
		r.capabilityMetrics.recordRevisionConflict()
		admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
		if auditErr := r.appendAdminAuditWithMetadata("admin_user", admin.session.Username, "deployment.resolution_changed", "provider", changed.ProviderID, "refused", "resolution_changed", map[string]any{
			"mismatches": changed.Mismatches, "requested_revision": changed.RequestedRevision, "current_revision": changed.Resolution.ResolutionRevision,
		}); auditErr != nil {
			adminAuditError(writer)
			return
		}
		writeJSON(writer, http.StatusConflict, map[string]any{
			"error": changed.Error(), "code": "resolution_changed", "mismatches": changed.Mismatches,
			"resolution": changed.Resolution,
		})
	case errors.Is(err, errModelCapabilityChanged):
		// Counted here rather than at each caller: this is the single funnel
		// every per-model revision conflict passes through.
		r.capabilityMetrics.recordRevisionConflict()
		writeJSON(writer, http.StatusConflict, map[string]string{
			"error": err.Error(), "code": "model_capability_changed",
		})
	case errors.Is(err, errModelCapabilitiesUnknown):
		writeJSON(writer, http.StatusBadRequest, map[string]string{
			"error": err.Error(), "code": "model_capabilities_unknown",
		})
	case errors.Is(err, errOperationBindingsUnavailable):
		writeJSON(writer, http.StatusBadRequest, map[string]string{
			"error": err.Error(), "code": "operation_bindings_unavailable",
		})
	case errors.Is(err, errModelCapabilitiesExceedCatalog):
		writeJSON(writer, http.StatusBadRequest, map[string]string{
			"error": err.Error(), "code": "model_capabilities_exceed_catalog",
		})
	case errors.Is(err, errCapabilityDetectionStale):
		writeJSON(writer, http.StatusConflict, map[string]string{"error": err.Error(), "code": "capability_detection_stale"})
	case errors.Is(err, errCapabilityDetectionChanged):
		writeJSON(writer, http.StatusConflict, map[string]string{"error": err.Error(), "code": "capability_detection_changed"})
	case errors.Is(err, errCapabilitiesExceedDetection):
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "capabilities_exceed_detection"})
	case errors.Is(err, errCapabilityDetectionTargetMismatch):
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "capability_detection_target_mismatch"})
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
	if err := refuseOperationBindings(input); err != nil {
		r.adminDeploymentInputError(writer, request, err)
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
		r.adminDeploymentInputError(writer, request, err)
		return
	}
	deployment, err = r.store.PutDeployment(request.Context(), deployment, 0)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if err := r.activateTopology(); err != nil {
		adminConfigurationError(writer, err)
		return
	}
	if err := r.auditAdminMutation(request, "deployment.create", "deployment", deployment.ID); err != nil {
		adminAuditError(writer)
		return
	}
	if err := r.auditCapabilitySnapshot(request, auditCapabilitySnapshotCreated, deployment,
		capabilitySnapshotMetadata(deployment)); err != nil {
		adminAuditError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(deployment.Revision))
	writeJSON(writer, http.StatusCreated, deployment)
}

// auditCapabilitySnapshot writes the capability-specific record, and the
// declaration record alongside it when the operator is the source. An operator
// declaration is its own event because §8.3 wants the declarer, the claim and
// the catalog state recorded together, which deployment.create does not carry.
func (r *Runtime) auditCapabilitySnapshot(request *http.Request, action string,
	deployment domain.Deployment, metadata map[string]any) error {
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	if err := r.appendAdminAuditWithMetadata("admin_user", admin.session.Username, action,
		"deployment", deployment.ID, "success", "", metadata); err != nil {
		return err
	}
	if deployment.ModelCapabilitySnapshot.Source != string(modelcatalog.SourceOperatorDeclared) {
		return nil
	}
	return r.appendAdminAuditWithMetadata("admin_user", admin.session.Username,
		auditOperatorCapabilitiesDeclared, "deployment", deployment.ID, "success", "", metadata)
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
	if err := refuseOperationBindings(input); err != nil {
		r.adminDeploymentInputError(writer, request, err)
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
	// Everything an update may carry forward comes from the record it replaces,
	// and only while the request still points at the same model through the same
	// interface. Aimed somewhere else, nothing earned here applies.
	prior := &priorDeployment{Capabilities: current.Capabilities, Snapshot: current.ModelCapabilitySnapshot}
	if input.ProviderID == current.ProviderID && strings.TrimSpace(input.ProviderModel) == current.ProviderModel &&
		(input.BindingID == "" || input.BindingID == current.BindingID) &&
		(input.ProfileID == "" || input.ProfileID == current.ProfileID) {
		prior.Evidence = current.CapabilityEvidence
		prior.OperatorDisabled = current.OperatorDisabled
	}
	deployment, err := r.deploymentFromInput(request, current.ID, input, prior, current.CreatedAt, time.Now().UTC())
	if err != nil {
		r.adminDeploymentInputError(writer, request, err)
		return
	}
	// Region is part of the target's identity, not a label on it: it selects the
	// endpoint, enters the model catalog key and the capability claim revision,
	// and binds the inference resources a deployment may use. A probe that
	// succeeded in one region is evidence about that region only.
	targetChanged := deployment.ProviderID != current.ProviderID ||
		deployment.ProviderModel != current.ProviderModel ||
		deployment.TargetKind != current.TargetKind && current.TargetKind != "" ||
		deployment.BindingID != current.BindingID ||
		deployment.ProfileID != current.ProfileID ||
		deployment.Region != current.Region ||
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
	// §7.2 treats the two edit directions differently, and the difference is not
	// stylistic. Turning a capability off leaves the deployment doing strictly
	// less than it was validated for, so it keeps serving. Turning one on makes
	// it claim something no test has ever exercised against this provider —
	// routing traffic on that claim is the fail-open this design exists to
	// prevent. So a widening advances the revision (making the test stale) and
	// drops the deployment to disabled, requiring an explicit re-enable, which
	// the enable path already gates on a current healthy test.
	//
	// Token limits are deliberately not treated as a widening: they bound which
	// requests fit, not what the deployment claims it can do.
	widened := modelcatalog.GainedCapabilities(current.Capabilities, deployment.Capabilities)
	if len(widened) != 0 {
		// The deployment has to leave the candidate set to be re-enabled
		// explicitly, and an enabled route may not point at a disabled
		// deployment. Saying so is better than letting the operator hit the
		// generic deactivation error and guess which edit caused it.
		if err := r.validateDeploymentCanDeactivate(request, deployment.ID, false); err != nil {
			writeJSON(writer, http.StatusConflict, map[string]string{
				"error": "enabling additional capabilities requires revalidation, so the deployment must leave routing first; disable its active routes, or narrow capabilities instead",
				"code":  "capability_expansion_requires_revalidation",
			})
			return
		}
		deployment.Enabled = false
	}
	if deployment.Enabled && !current.Enabled &&
		(current.LastTestStatus != domain.DeploymentTestHealthy || current.LastTestRevision != current.Revision) {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "deployment must pass a current validation test before enable"})
		return
	}
	// PutDeployment advances the optimistic-lock revision for every update, so a
	// probe recorded against the current revision would read as stale after an
	// edit that changed nothing the probe was evidence about. Carrying it forward
	// is what keeps a rename or a concurrency change from silently demanding a
	// retest, and it is narrow on purpose:
	//
	//   - the target itself cannot move here at all; targetChanged rejected that
	//     above, region included;
	//   - a change to the capability claim or its snapshot is a change to what
	//     the deployment says it can do, whether or not it widened, so the probe
	//     no longer covers the claim being made;
	//   - taking a deployment out of service invalidates the probe. Re-enabling
	//     is where an operator asserts it is healthy again, and that assertion
	//     has to be paid for with a fresh test rather than with the one that was
	//     current when it was switched off.
	//
	// The enable direction does carry forward: it only reaches this line by
	// presenting a current healthy test to the gate above, so the result is still
	// evidence about the revision being stored.
	if current.LastTestStatus != "" && current.LastTestRevision == current.Revision &&
		!capabilityChanged(current, deployment) && !(current.Enabled && !deployment.Enabled) {
		deployment.LastTestRevision = current.Revision + 1
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
	if err := r.activateTopology(); err != nil {
		adminConfigurationError(writer, err)
		return
	}
	if err := r.auditAdminMutation(request, "deployment.update", "deployment", deployment.ID); err != nil {
		adminAuditError(writer)
		return
	}
	// A review is a change to what the deployment claims about its model. An
	// edit to its name or concurrency is not one, and recording it as such
	// would bury the events an operator actually reviews.
	if capabilityChanged(current, deployment) {
		if err := r.auditCapabilitySnapshot(request, auditCapabilitySnapshotReviewed, deployment,
			capabilityChangeMetadata(current, deployment)); err != nil {
			adminAuditError(writer)
			return
		}
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
	if err := r.activateTopology(); err != nil {
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
	r.capabilityMetrics.recordDeploymentTest(probeErr == nil)
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

// priorDeployment is what an update may carry forward from the record it
// replaces: nil for a create, and with Evidence and OperatorDisabled empty when
// the request aims at a different model or interface.
type priorDeployment struct {
	Capabilities     domain.ProviderCapabilities
	Snapshot         domain.ModelCapabilitySnapshot
	Evidence         domain.CapabilityEvidenceSet
	OperatorDisabled []string
}

// Declaration is what has already been established for this deployment, which
// is wider than what it uses whenever the operator has switched something off.
// It is the right side of "is this edit a new claim": re-enabling a capability
// that was declared and then switched off is not a new claim, and demanding the
// word again for it would make switching one off effectively irreversible.
func (p *priorDeployment) Declaration() domain.ProviderCapabilities {
	if p == nil {
		return domain.ProviderCapabilities{}
	}
	if p.Snapshot.Capabilities.AnyOperation() {
		return p.Snapshot.Capabilities
	}
	return p.Capabilities
}

func (r *Runtime) deploymentFromInput(request *http.Request, deploymentID string, input deploymentInput, prior *priorDeployment, createdAt, updatedAt time.Time) (domain.Deployment, error) {
	instance, err := r.store.GetProvider(request.Context(), input.ProviderID)
	if err != nil || instance.DeletedAt != nil || (input.Enabled && !instance.Enabled) {
		return domain.Deployment{}, errors.New("deployment provider is unavailable")
	}
	model := strings.TrimSpace(input.ProviderModel)
	region := deploymentRegion(instance, input)
	var detected *domain.ModelCapabilityDetection
	var resolution deploymentResolution
	if input.CapabilityDetectionID != "" {
		resolution, detected, err = r.resolveDeploymentDetection(request.Context(), instance, input, model, region)
	} else if input.ResolutionRevision != "" {
		resolution, err = r.resolveDeploymentVariant(request.Context(), instance, input, model, region)
	} else {
		resolution, err = resolveDeploymentTargetWithCatalog(instance, input, model, region, prior, r.effectiveModelCatalog())
	}
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
	// The model/profile pin was settled while choosing the binding. The target
	// kind is checked here rather than as a candidate filter: every binding on a
	// provider shares its access surface, so the only profile-dependent kinds
	// are Bedrock converse's, and capability matching already separates converse
	// from the profiles that carry no chat.
	targetKind, err := deploymentTargetKind(instance.Type, binding.AccessSurface, binding.ProfileID, input.TargetKind)
	if err != nil {
		return domain.Deployment{}, err
	}
	// The snapshot is built first because its source decides how strong the
	// deployment's own evidence is allowed to be.
	snapshot := domain.ModelCapabilitySnapshot{
		ProviderModel:      model,
		CanonicalModelRef:  resolution.canonicalModelRef,
		ModelRevision:      resolution.entry.Revision(),
		ResolutionRevision: resolution.resolutionRevision,
		ProviderRevision:   instance.Revision,
		Source:             string(resolution.entry.Source),
		Status:             string(resolution.entry.Status),
		CapturedAt:         updatedAt,
		Capabilities:       resolution.capabilities,
	}
	if resolution.variant != nil {
		snapshot.ClaimRevisions = make([]string, 0, len(resolution.variant.CapabilityClaims))
		for _, claim := range resolution.variant.CapabilityClaims {
			snapshot.ClaimRevisions = append(snapshot.ClaimRevisions, claim.Revision)
		}
		snapshot.TargetFingerprint = provider.CapabilityClaimRevision(instance.ID, model, string(resolution.variant.Target.TargetKind), resolution.binding.ID, region)
	}
	if detected != nil {
		snapshot = domain.DetectionCapabilitySnapshot(*detected, detected.Recommended, updatedAt)
		snapshot.Capabilities = detected.Recommended
	} else if resolution.declared {
		// An operator declaration is its own source. Recording it as the catalog
		// would let a later refresh look like agreement that never happened.
		snapshot.Source = string(modelcatalog.SourceOperatorDeclared)
		snapshot.Status = string(modelcatalog.StatusPartial)
		if resolution.mapped {
			// Keep the reviewed model's full, interface-clamped declaration in
			// the snapshot even when the operator switches one capability off.
			// The live deployment remains narrowed, and OperatorDisabled records
			// the difference.
			snapshot.Capabilities = modelcatalog.Clamp(resolution.entry.Capabilities, resolution.binding.Capabilities)
		}
		// An edit that stays inside an existing declaration is not a new claim,
		// so what was declared still stands and the narrowing is a switch-off
		// rather than a withdrawal. Collapsing the snapshot onto the narrowed set
		// would erase the very difference OperatorDisabled is read from, and the
		// capability would be offered again as though it had never been declined.
		if !resolution.mapped && input.Mode != deploymentModeOperatorDeclared &&
			domain.ProviderCapabilitiesSubset(capabilities, prior.Declaration()) {
			snapshot.Capabilities = prior.Declaration()
		}
	} else {
		snapshot.CatalogRevision = r.effectiveModelCatalog().Revision()
		snapshot.Capabilities = modelcatalog.Clamp(resolution.entry.Capabilities, resolution.binding.Capabilities)
	}
	snapshot.Evidence = domain.SnapshotEvidence(snapshot)

	var priorDisabled []string
	if prior != nil {
		priorDisabled = prior.OperatorDisabled
	}
	var priorEvidence domain.CapabilityEvidenceSet
	if prior != nil {
		priorEvidence = prior.Evidence
	}
	evidence := deploymentCapabilityEvidence(capabilities, binding.CapabilityEvidence, priorEvidence, snapshot.Source)
	for name, value := range evidence {
		if !capabilitiesEnabledByName(capabilities, name) && value != domain.EvidenceUnsupported {
			return domain.Deployment{}, errors.New("deployment capability evidence exceeds enabled capabilities")
		}
		if providerValue, ok := binding.CapabilityEvidence[name]; ok && evidenceRank(value) > evidenceRank(providerValue) {
			return domain.Deployment{}, errors.New("deployment capability evidence exceeds provider evidence")
		}
	}
	deployment := domain.Deployment{
		ID: deploymentID, Name: strings.TrimSpace(input.Name), ProviderID: input.ProviderID,
		ProviderModel: model, TargetKind: targetKind, Capabilities: capabilities,
		ModelCapabilitySnapshot: snapshot,
		OperatorDisabled:        domain.OperatorDisabledCapabilities(snapshot, capabilities, priorDisabled),
		AccessSurface:           binding.AccessSurface, ProfileID: binding.ProfileID, BindingID: binding.ID,
		Region:             region,
		CapabilityEvidence: evidence,
		MaxConcurrency:     input.MaxConcurrency,
		Enabled:            input.Enabled, CreatedAt: createdAt, UpdatedAt: updatedAt,
	}
	return deployment, deployment.Validate()
}

var (
	errCapabilityDetectionStale          = errors.New("capability detection is incomplete or expired")
	errCapabilityDetectionChanged        = errors.New("capability detection changed since it was read")
	errCapabilitiesExceedDetection       = errors.New("deployment capabilities exceed the verified detection result")
	errCapabilityDetectionTargetMismatch = errors.New("capability detection does not match the current target")
)

func (r *Runtime) resolveDeploymentDetection(ctx context.Context, instance domain.ProviderInstance, input deploymentInput, model, region string) (deploymentResolution, *domain.ModelCapabilityDetection, error) {
	if input.Mode != "" || input.CapabilityDetectionRevision == 0 || input.Capabilities == nil {
		return deploymentResolution{}, nil, errCapabilityDetectionTargetMismatch
	}
	detection, err := r.store.GetModelCapabilityDetection(ctx, input.CapabilityDetectionID)
	if err != nil {
		return deploymentResolution{}, nil, errCapabilityDetectionTargetMismatch
	}
	if detection.Revision != input.CapabilityDetectionRevision {
		return deploymentResolution{}, nil, errCapabilityDetectionChanged
	}
	if !detection.Fresh(r.now().UTC()) {
		return deploymentResolution{}, nil, errCapabilityDetectionStale
	}
	targetKind, err := deploymentTargetKind(instance.Type, detection.AccessSurface, detection.ProfileID, input.TargetKind)
	if err != nil {
		return deploymentResolution{}, nil, errCapabilityDetectionTargetMismatch
	}
	credential, err := r.store.GetCredential(ctx, instance.CredentialID)
	if err != nil {
		return deploymentResolution{}, nil, errCapabilityDetectionTargetMismatch
	}
	if detection.ProviderID != instance.ID || detection.ProviderModel != model || detection.Region != region || detection.TargetKind != targetKind ||
		(input.BindingID != "" && input.BindingID != detection.BindingID) ||
		(input.ProfileID != "" && input.ProfileID != detection.ProfileID) {
		return deploymentResolution{}, nil, errCapabilityDetectionTargetMismatch
	}
	if detection.ProviderRevision != instance.Revision || detection.CredentialRevision != credential.Revision || detection.CredentialKeyVersion != credential.KeyVersion {
		return deploymentResolution{}, nil, errCapabilityDetectionStale
	}
	expectedFingerprint := detectionTargetFingerprint(instance.ID, instance.Revision, credential.Revision, credential.KeyVersion,
		detection.BindingID, detection.ProfileID, detection.AccessSurface, model, targetKind, region, detection.RiskTier)
	if detection.TargetFingerprint != expectedFingerprint {
		return deploymentResolution{}, nil, errCapabilityDetectionStale
	}
	binding, ok := instance.ProfileBinding(detection.BindingID)
	if !ok || !binding.Enabled || binding.ProfileID != detection.ProfileID || binding.AccessSurface != detection.AccessSurface {
		return deploymentResolution{}, nil, errCapabilityDetectionTargetMismatch
	}
	retained := *input.Capabilities
	if retained.MaxContextTokens == 0 {
		retained.MaxContextTokens = detection.Recommended.MaxContextTokens
	}
	if retained.MaxOutputTokens == 0 {
		retained.MaxOutputTokens = detection.Recommended.MaxOutputTokens
	}
	if !domain.ProviderCapabilitiesSubset(retained, detection.Recommended) || !domain.ProviderCapabilitiesSubset(retained, binding.Capabilities) {
		return deploymentResolution{}, nil, errCapabilitiesExceedDetection
	}
	if err := modelcatalog.ValidateDependencies(retained); err != nil {
		return deploymentResolution{}, nil, err
	}
	input.Capabilities.MaxContextTokens, input.Capabilities.MaxOutputTokens = retained.MaxContextTokens, retained.MaxOutputTokens
	entry := modelcatalog.Unknown(modelcatalog.Key{ProviderType: instance.Type, Profile: binding.ProfileID, TargetKind: input.TargetKind, Model: model, Region: region})
	return deploymentResolution{binding: binding, capabilities: retained, entry: entry}, &detection, nil
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
	for index, part := range parts {
		if strings.HasPrefix(part, "bedrock") && index+1 < len(parts) {
			candidate := parts[index+1]
			if candidate != "amazonaws" && candidate != "vpce" {
				return candidate
			}
		}
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

// errModelCapabilitiesExceedCatalog is its sibling for a model the catalog does
// cover. It is a separate code because the operator's next move is different:
// the usual answer is that the request is wrong, and the catalog is right. When
// it is not, the same explicit declaration overrides it, and the deployment
// then records the operator as the source rather than the catalog.
var errModelCapabilitiesExceedCatalog = errors.New("deployment capabilities exceed what the catalog establishes for this model; declare them explicitly with mode=operator_declared to override")

// A mapped capability model is optional. Once supplied, however, it is the
// bound the operator chose for this alias; widening past it while keeping the
// mapping would produce a snapshot narrower than the deployment. Clear the
// mapping and use the ordinary explicit declaration path instead.
var errCapabilitiesExceedMappedModel = errors.New("deployment capabilities exceed the selected capability_model; clear capability_model to declare them manually")

// deploymentResolution is what the server decided, from the catalog and the
// provider topology, rather than from what the client claimed.
type deploymentResolution struct {
	binding            domain.ProviderProfileBinding
	capabilities       domain.ProviderCapabilities
	entry              modelcatalog.Entry
	variant            *domain.DeploymentVariant
	canonicalModelRef  string
	resolutionRevision string
	declared           bool
	mapped             bool
}

type resolutionChangedError struct {
	Mismatches        []string
	Resolution        adminInvocationTarget
	ProviderID        string
	RequestedRevision string
}

func (e *resolutionChangedError) Error() string {
	return "invocation target resolution changed; review the updated capability summary before saving"
}

func (r *Runtime) resolveDeploymentVariant(ctx context.Context, instance domain.ProviderInstance, input deploymentInput, model, region string) (deploymentResolution, error) {
	bindings, err := invocationTargetBindings(instance, strings.TrimSpace(input.BindingID))
	if err != nil {
		return deploymentResolution{}, err
	}
	targetKind, err := deploymentTargetKind(instance.Type, bindings[0].AccessSurface, bindings[0].ProfileID, input.TargetKind)
	if err != nil {
		return deploymentResolution{}, err
	}
	target := domain.InvocationTargetDescriptor{
		TargetID: model, TargetKind: targetKind, DisplayName: model, CanonicalModelRef: strings.TrimSpace(input.CapabilityModel),
		Region: region, Lifecycle: domain.TargetLifecycleUnknown, MetadataSource: domain.MetadataSourceNone,
		Availability: domain.AvailabilityUnverified, FetchedAt: r.now().UTC(),
	}
	mappers := make(map[string]provider.ProviderMetadataMapper)
	bindingTargets := make(map[string]domain.InvocationTargetDescriptor)
	credentialRevision, credentialErr := r.providerCredentialRevision(ctx, instance)
	if credentialErr != nil {
		return deploymentResolution{}, errors.New("deployment provider credential is unavailable")
	}
	results := r.fetchInvocationTargetCatalogs(ctx, instance, bindings, true)
	for index, binding := range bindings {
		result := results[index]
		var bindingTarget domain.InvocationTargetDescriptor
		found := false
		for _, candidate := range result.items {
			if candidate.TargetID == model && candidate.TargetKind == targetKind && (candidate.Region == "" || region == "" || candidate.Region == region) {
				bindingTarget, found = candidate, true
				break
			}
		}
		adapter, ok := r.providers.AdapterForBinding(instance.ID, binding.ID)
		if !ok && len(instance.Bindings) == 0 {
			adapter, ok = r.providers.AdapterForProvider(instance.ID)
		}
		if ok {
			if mapper, mapped := providerMetadataMapper(adapter); mapped {
				mappers[binding.ID] = mapper
			}
			if result.discovery.CanDescribe {
				if describer, canDescribe := invocationTargetDescriber(adapter); canDescribe {
					describeTarget := target
					if found {
						describeTarget = bindingTarget
					}
					if described, describedOK := r.describeInvocationTarget(ctx, instance, binding, describer, describeTarget); describedOK {
						bindingTarget, found = described, true
					}
				}
			}
		}
		if found {
			if input.CapabilityModel != "" {
				bindingTarget.CanonicalModelRef = strings.TrimSpace(input.CapabilityModel)
			}
			bindingTargets[binding.ID] = bindingTarget
			if len(bindingTargets) == 1 {
				target = bindingTarget
			}
		}
	}
	if input.CapabilityModel != "" {
		target.CanonicalModelRef = strings.TrimSpace(input.CapabilityModel)
	}
	current := resolveInvocationTargetWithCatalog(instance, target, bindingTargets, bindings, mappers, credentialRevision, r.clockNow().UTC(), r.effectiveModelCatalog())
	var selected *domain.DeploymentVariant
	for index := range current.Variants {
		if current.Variants[index].BindingID == input.BindingID {
			selected = &current.Variants[index]
			break
		}
	}
	var mismatches []string
	if selected == nil {
		mismatches = append(mismatches, "binding_revision")
	} else if input.ResolutionRevision != selected.Revision {
		mismatches = append(mismatches, "resolution_revision")
	}
	if len(mismatches) > 0 {
		return deploymentResolution{}, &resolutionChangedError{Mismatches: mismatches, Resolution: current, ProviderID: instance.ID, RequestedRevision: input.ResolutionRevision}
	}
	retained := selected.Capabilities
	if input.Capabilities != nil {
		if !domain.ProviderCapabilitiesSubset(*input.Capabilities, selected.Capabilities) {
			return deploymentResolution{}, errors.New("deployment capabilities exceed the selected deployment variant")
		}
		retained = *input.Capabilities
	}
	source := modelcatalog.SourceProviderMetadata
	status := modelcatalog.StatusPartial
	for _, claim := range selected.CapabilityClaims {
		if claim.Source == domain.ClaimSourceBuiltinCatalog {
			source, status = modelcatalog.SourceBuiltin, modelcatalog.StatusKnown
			break
		}
		if claim.Source == domain.ClaimSourceSignedCatalog {
			source, status = modelcatalog.SourceSignedCatalog, modelcatalog.StatusPartial
		}
	}
	catalogModel := target.CanonicalModelRef
	if catalogModel == "" {
		catalogModel = model
	}
	bindingIndex := slices.IndexFunc(bindings, func(binding domain.ProviderProfileBinding) bool { return binding.ID == selected.BindingID })
	selectedBinding := bindings[bindingIndex]
	_, catalogEntry := invocationTargetCatalogEntryWithCatalog(instance, selectedBinding, target, r.effectiveModelCatalog())
	entry := modelcatalog.Entry{
		Key:    modelcatalog.Key{ProviderType: instance.Type, Profile: selected.ProfileID, TargetKind: target.TargetKind, Model: catalogModel, Region: region},
		Status: status, Source: source, Capabilities: selected.Capabilities,
	}
	if source == modelcatalog.SourceBuiltin || source == modelcatalog.SourceSignedCatalog {
		entry = catalogEntry
	}
	mapped := target.CanonicalModelRef != "" && target.CanonicalModelRef != model
	return deploymentResolution{
		binding:      selectedBinding,
		capabilities: retained, entry: entry, variant: selected, canonicalModelRef: target.CanonicalModelRef,
		resolutionRevision: selected.Revision, declared: mapped, mapped: mapped,
	}, nil
}

// resolveDeploymentTarget picks the binding a model should run through and the
// capabilities it may keep.
//
// binding_id and profile_id from the client are treated as filters, never as
// authority: a client cannot name a binding to obtain capabilities the catalog
// does not support for that model.
func resolveDeploymentTarget(instance domain.ProviderInstance, input deploymentInput, model, region string, prior *priorDeployment) (deploymentResolution, error) {
	return resolveDeploymentTargetWithCatalog(instance, input, model, region, prior, modelcatalog.Builtin())
}

func resolveDeploymentTargetWithCatalog(instance domain.ProviderInstance, input deploymentInput, model, region string, prior *priorDeployment, catalog *modelcatalog.Catalog) (deploymentResolution, error) {
	mappedEntry, mapped, err := resolveCapabilityModelEntryWithCatalog(instance, input, catalog)
	if err != nil {
		return deploymentResolution{}, err
	}
	var candidates []domain.ProviderProfileBinding
	// Why a binding can be excluded before capabilities are even considered:
	// some profiles accept exactly one model. That pin used to be checked after
	// the binding had been chosen, which is too late — automatic selection could
	// settle on a binding the very next check rejected, and the operator got a
	// refusal naming a profile they never picked. A binding that cannot serve
	// this model is not a candidate for it.
	var excluded error
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
		if err := bedrockprovider.ValidateProfileModel(binding.ProfileID, model); err != nil {
			if excluded == nil {
				excluded = err
			}
			continue
		}
		candidates = append(candidates, binding)
	}
	if len(candidates) == 0 {
		// When a filter emptied the list, report why that filter rejected rather
		// than the generic absence: "this profile requires model X" tells an
		// operator what to change, "binding is unavailable" does not.
		if excluded != nil {
			return deploymentResolution{}, excluded
		}
		return deploymentResolution{}, errors.New("deployment provider profile binding is unavailable")
	}
	slices.SortFunc(candidates, func(left, right domain.ProviderProfileBinding) int {
		return strings.Compare(string(left.ProfileID), string(right.ProfileID))
	})

	// Omitting capabilities on an edit means "leave them as they are". It used
	// to mean "inherit the profile ceiling", which is the default this whole
	// change exists to remove.
	retained := input.Capabilities
	if retained == nil && prior != nil {
		// Omitting capabilities means "leave them as they are", not "adopt
		// everything that was ever declared" — so this is what the deployment
		// uses, not its declaration.
		retained = &prior.Capabilities
	}
	var known, unknown []deploymentResolution
	for _, binding := range candidates {
		targetKind, kindErr := deploymentTargetKind(instance.Type, binding.AccessSurface, binding.ProfileID, input.TargetKind)
		if kindErr != nil {
			return deploymentResolution{}, kindErr
		}
		key := modelcatalog.Key{ProviderType: instance.Type, Profile: binding.ProfileID, TargetKind: targetKind, Model: model, Region: region}
		entry, found := catalog.Lookup(key)
		if mapped {
			entry, found = mappedEntry, true
		}
		resolution := deploymentResolution{
			binding:      binding,
			capabilities: modelcatalog.Clamp(entry.Capabilities, binding.Capabilities),
			entry:        entry,
			mapped:       mapped,
		}
		if found && (entry.Source == modelcatalog.SourceBuiltin || entry.Source == modelcatalog.SourceSignedCatalog) {
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
			// Mapping an operator-owned target name to a catalog model is itself
			// an operator declaration. The catalog bounds the checkboxes, but it
			// is not evidence that the alias really points at that model.
			resolution.declared = resolution.mapped
			return resolution, checkModelRevision(input.ModelRevision, resolution.entry)
		}
	}
	if mapped {
		return deploymentResolution{}, errCapabilitiesExceedMappedModel
	}
	// Either nothing is established, or the catalog establishes less than was
	// asked for. Both leave the operator as the only source, and both need the
	// word said out loud.
	//
	// Letting an explicit declaration exceed a catalog entry is deliberate. The
	// catalog is reviewed claims about models, not measurements, and an entry
	// that under-claims would otherwise be a wall: the deployment cannot be
	// created at all, and no amount of operator knowledge gets past it. The
	// declaration is recorded as the operator's claim rather than the catalog's,
	// so nothing here turns a mistaken entry into apparent agreement. Silence
	// still loses — without the word, the catalog stands.
	//
	// Declaring is an act the operator performs once. An edit that stays inside
	// an existing declaration is not a new claim, and narrowing one is a
	// reduction — neither should demand the word again. Anything wider does.
	declaration := prior.Declaration()
	covered := prior != nil && retained != nil && domain.ProviderCapabilitiesSubset(*retained, declaration)
	if input.Mode != deploymentModeOperatorDeclared && !covered {
		if len(known) > 0 {
			return deploymentResolution{}, errModelCapabilitiesExceedCatalog
		}
		return deploymentResolution{}, errModelCapabilitiesUnknown
	}
	if retained == nil || !retained.AnyOperation() {
		return deploymentResolution{}, errors.New("an operator-declared model must declare at least one core operation")
	}
	if err := modelcatalog.ValidateDependencies(*retained); err != nil {
		return deploymentResolution{}, err
	}
	// Known bindings come first: when the catalog covers this model on one of
	// them, that is still the binding it belongs on, even though the operator is
	// claiming more than the catalog does.
	for _, resolution := range append(known, unknown...) {
		if domain.ProviderCapabilitiesSubset(*retained, resolution.binding.Capabilities) {
			resolution.capabilities = *retained
			resolution.declared = true
			return resolution, checkModelRevision(input.ModelRevision, resolution.entry)
		}
	}
	return deploymentResolution{}, errors.New("no enabled provider binding supports the declared capabilities")
}

func resolveCapabilityModelEntry(instance domain.ProviderInstance, input deploymentInput) (modelcatalog.Entry, bool, error) {
	return resolveCapabilityModelEntryWithCatalog(instance, input, modelcatalog.Builtin())
}

func resolveCapabilityModelEntryWithCatalog(instance domain.ProviderInstance, input deploymentInput, catalog *modelcatalog.Catalog) (modelcatalog.Entry, bool, error) {
	model := strings.TrimSpace(input.CapabilityModel)
	if model == "" {
		return modelcatalog.Entry{}, false, nil
	}
	if instance.Type == domain.ProviderAzureOpenAI {
		if input.TargetKind != "" && input.TargetKind != domain.TargetAzureDeployment {
			return modelcatalog.Entry{}, false, errors.New("capability_model is only valid for an Azure Deployment target")
		}
	} else if instance.Type == domain.ProviderOpenAICompatible {
		if input.TargetKind != "" && input.TargetKind != domain.TargetCustomEndpointModel {
			return modelcatalog.Entry{}, false, errors.New("capability_model is only valid for a custom endpoint target")
		}
	} else {
		return modelcatalog.Entry{}, false, errors.New("capability_model is only valid for Azure Deployment and custom endpoint targets")
	}
	providerType, profileID, _ := capabilityModelCatalogScope(instance.Type)
	targetKind := domain.TargetModelID
	if providerType == domain.ProviderOpenAICompatible {
		targetKind = domain.TargetCustomEndpointModel
	}
	entry, found := catalog.Lookup(modelcatalog.Key{ProviderType: providerType, Profile: profileID, TargetKind: targetKind, Model: model})
	if !found || entry.Source != modelcatalog.SourceBuiltin && entry.Source != modelcatalog.SourceSignedCatalog {
		return modelcatalog.Entry{}, false, errors.New("capability_model is not covered by a reviewed catalog")
	}
	if err := checkModelRevision(input.ModelRevision, entry); err != nil {
		return modelcatalog.Entry{}, false, err
	}
	return entry, true, nil
}

// checkModelRevision compares the revision the client read against the one that
// applies now. It is per-model on purpose: a catalog-wide digest would rotate
// whenever any unrelated model appeared, and operators would learn to retry
// through the conflict until it meant nothing.
//
// An absent revision is deliberately not an error, and this is the reasoning so
// it does not get "hardened" later. The builtin catalog is compiled in behind
// sync.OnceValues, so it cannot move while the process runs: a request that
// names no revision is resolved against the same catalog it would have read,
// and there is no stale read to catch. The one real staleness — a client that
// read the catalog from an older binary and writes to a newer one — sends the
// *old* revision, which the comparison below already refuses with a 409.
// Requiring the field would fail honest minimal requests without closing that.
func checkModelRevision(claimed string, entry modelcatalog.Entry) error {
	if claimed == "" || claimed == entry.Revision() {
		return nil
	}
	return errModelCapabilityChanged
}

// deploymentCapabilityEvidence is the deployment's own evidence: what the
// binding establishes, capped by what the capability source may claim, with a
// previously earned level carried forward.
//
// The cap used to be an unconditional verified→declared downgrade. It produced
// the right answer for every source that exists today and the wrong reason for
// it — a probe result would have been demoted too. Asking the source what it
// may claim is the rule §4.1 actually states.
func deploymentCapabilityEvidence(capabilities domain.ProviderCapabilities,
	providerEvidence, currentEvidence domain.CapabilityEvidenceSet, source string) domain.CapabilityEvidenceSet {
	maximum := domain.MaxEvidenceForCapabilitySource(source)
	evidence := providerEvidence.Clone()
	for name, value := range evidence {
		if !capabilitiesEnabledByName(capabilities, name) {
			evidence[name] = domain.EvidenceUnsupported
			continue
		}
		providerValue := value
		if evidenceRank(value) > evidenceRank(maximum) {
			evidence[name] = maximum
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
