package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/id"
	"github.com/akz142857/Halro/internal/provider"
	bedrockprovider "github.com/akz142857/Halro/internal/provider/bedrock"
	bedrockmantleprovider "github.com/akz142857/Halro/internal/provider/bedrockmantle"
	"github.com/akz142857/Halro/internal/safelog"
	"github.com/akz142857/Halro/internal/safetransport"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/go-chi/chi/v5"
)

type credentialInput struct {
	Name          string                  `json:"name"`
	Type          domain.ProviderType     `json:"type"`
	BaseURL       string                  `json:"base_url"`
	AccessSurface domain.AccessSurface    `json:"access_surface,omitempty"`
	Scheme        domain.CredentialScheme `json:"scheme,omitempty"`
	Secret        *string                 `json:"secret,omitempty"`
	// Absolute, like the Gateway Key's own expiry: what the request says is what
	// is stored, and an omitted or null value means the secret has no declared
	// end. A rotation that means to keep an expiry sends it again.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type providerInput struct {
	Name                  string                   `json:"name"`
	Type                  domain.ProviderType      `json:"type"`
	BaseURL               string                   `json:"base_url"`
	APIVersion            string                   `json:"api_version,omitempty"`
	CredentialID          string                   `json:"credential_id"`
	AccessSurface         domain.AccessSurface     `json:"access_surface,omitempty"`
	ProfileID             domain.ProviderProfileID `json:"profile_id,omitempty"`
	CredentialScheme      domain.CredentialScheme  `json:"credential_scheme,omitempty"`
	BedrockProjectID      string                   `json:"bedrock_project_id,omitempty"`
	AllowedAnthropicBetas []string                 `json:"allowed_anthropic_betas,omitempty"`
	// One flat set for the whole connection. Which profile ends up serving each
	// capability is the server's answer (domain.AssignConnectionCapabilities),
	// not the caller's: a connection can span profiles — an OpenAI key serves the
	// chat endpoints and the media ones — and when the caller supplied the split
	// itself, the rule for it lived in the console and nowhere else. There is
	// deliberately no bindings field to send alongside this; decodeAdminJSON
	// refuses unknown fields, so a caller still sending one is told rather than
	// having it silently overridden.
	Capabilities   *domain.ProviderCapabilities `json:"capabilities,omitempty"`
	MaxConcurrency int64                        `json:"max_concurrency"`
	Enabled        bool                         `json:"enabled"`
}

type routeInput struct {
	PublicModel string `json:"public_model"`
	// Required. A route names a deployment; provider, model and price all come
	// from it. decodeAdminJSON rejects unknown fields, so a client still
	// sending the old provider_id/provider_model shape gets a 400 rather than a
	// route that silently drops them.
	DeploymentID string `json:"deployment_id"`
	Priority     int    `json:"priority"`
	Strategy     string `json:"strategy"`
	Enabled      bool   `json:"enabled"`
}

func (r *Runtime) createAdminCredential(writer http.ResponseWriter, request *http.Request) {
	var input credentialInput
	if err := decodeAdminJSON(request, &input); err != nil || input.Secret == nil {
		adminBadRequestCode(writer, "invalid_request", "invalid request")
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
	intent, intentErr := r.newAdminAuditIntent(request, "credential.create", "credential", credential.ID)
	if intentErr != nil {
		adminStoreError(writer)
		return
	}
	credential, err = r.store.PutCredential(request.Context(), credential, 0, intent)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	r.completeAdminMutation(writer, request, *intent)
	writer.Header().Set("ETag", revisionETag(credential.Revision))
	writeJSON(writer, http.StatusCreated, credentialViewFrom(credential))
}

func (r *Runtime) updateAdminCredential(writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	var input struct {
		credentialInput
		stepUpMaterial
	}
	if err := decodeAdminJSON(request, &input); err != nil {
		adminBadRequestCode(writer, "invalid_request", "invalid request")
		return
	}
	// Replacing credential material is the same trust-boundary change as
	// deleting it and creating a new one, which already asks; see
	// requireStepUpMaterial.
	if !r.requireStepUpMaterial(writer, request, input.stepUpMaterial) {
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
	credential, err := r.credentialFromInput(current.ID, input.credentialInput, &current, current.CreatedAt)
	if err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	if err := r.validateCredentialReferences(request, credential); err != nil {
		// A generic 409 reads as "someone else edited this, refresh" in the
		// console, which is the one thing that will not help here: the rotation
		// would move the credential away from an endpoint a provider still uses.
		var conflict credentialMatchError
		if errors.As(err, &conflict) {
			writeJSON(writer, http.StatusConflict, codedErrorBody(conflict.code, err.Error(), conflict.fields))
			return
		}
		writeJSON(writer, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	intent, intentErr := r.newAdminAuditIntent(request, "credential.rotate", "credential", credential.ID)
	if intentErr != nil {
		adminStoreError(writer)
		return
	}
	credential, err = r.store.PutCredential(request.Context(), credential, expected, intent)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	r.activateTopologyAfterCommit()
	r.clearAllInvocationTargetCatalogs()
	r.completeAdminMutation(writer, request, *intent)
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
	intent, intentErr := r.newAdminAuditIntent(request, "credential.delete", "credential", credentialID)
	if intentErr != nil {
		adminStoreError(writer)
		return
	}
	if err := r.store.DeleteCredential(request.Context(), credentialID, expected, intent); err != nil {
		if errors.Is(err, boltstore.ErrCredentialInUse) {
			writeJSON(writer, http.StatusConflict, map[string]string{"error": "credential is still referenced"})
			return
		}
		adminMutationError(writer, err)
		return
	}
	r.completeAdminMutation(writer, request, *intent)
	writer.WriteHeader(http.StatusNoContent)
}

func (r *Runtime) createAdminProvider(writer http.ResponseWriter, request *http.Request) {
	var input providerInput
	if err := decodeAdminJSON(request, &input); err != nil {
		adminBadRequestCode(writer, "invalid_request", "invalid request")
		return
	}
	idempotencyKey, ok := adminCreateIdempotencyKey(writer, request)
	if !ok {
		return
	}
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	providerID := adminCreateID("prv", "provider", admin.session.Username, idempotencyKey)
	now := time.Now().UTC()
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	instance, err := r.providerFromInput(request, providerID, input, "", nil, nil, now, now)
	if err != nil {
		adminProviderInputError(writer, err)
		return
	}
	intent, intentErr := r.newAdminAuditIntent(request, "provider.create", "provider", instance.ID)
	if intentErr != nil {
		adminStoreError(writer)
		return
	}
	instance, err = r.store.PutProvider(request.Context(), instance, 0, intent)
	if err != nil {
		if writeAdminCreateReplay(writer, err, "provider", providerID) {
			return
		}
		adminMutationError(writer, err)
		return
	}
	r.activateTopologyAfterCommit()
	r.completeAdminMutation(writer, request, *intent)
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
		adminBadRequestCode(writer, "invalid_request", "invalid request")
		return
	}
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	current, err := r.store.GetProvider(request.Context(), chi.URLParam(request, "id"))
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	// A tombstone is not editable. Deployment and Gateway Key updates already
	// answer 404 here; without this, a deleted provider stayed writable and the
	// audit trail recorded successful updates against a removed object.
	if current.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	if current.Revision != expected {
		adminPreconditionFailed(writer)
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
				adminBadRequestCode(writer, "provider_type_locked_by_deployments", "provider type and profile cannot change while deployments reference it")
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
		adminProviderInputError(writer, err)
		return
	}
	if err := r.validateProviderCanDeactivate(request, instance.ID, instance.Enabled); err != nil {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	if err := r.validateBindingsCanDeactivate(request, instance); err != nil {
		writeJSON(writer, http.StatusConflict, map[string]string{
			"error": err.Error(), "code": "binding_referenced_by_deployment",
		})
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
	intent, intentErr := r.newAdminAuditIntent(request, "provider.update", "provider", instance.ID)
	if intentErr != nil {
		adminStoreError(writer)
		return
	}
	instance, err = r.store.PutProvider(request.Context(), instance, expected, intent)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	r.activateTopologyAfterCommit()
	r.clearInvocationTargetCatalog(instance.ID)
	r.completeAdminMutation(writer, request, *intent)
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
	// Deleting a tombstone again would advance its revision and record a second
	// delete event for an object that is already gone.
	if instance.DeletedAt != nil {
		adminNotFound(writer)
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
	intent, intentErr := r.newAdminAuditIntent(request, "provider.delete", "provider", instance.ID)
	if intentErr != nil {
		adminStoreError(writer)
		return
	}
	instance, err = r.store.PutProvider(request.Context(), instance, expected, intent)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	r.activateTopologyAfterCommit()
	r.clearInvocationTargetCatalog(instance.ID)
	r.completeAdminMutation(writer, request, *intent)
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
		adminBadRequestCode(writer, "provider_disabled", "provider is disabled")
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
		adminBadRequestCode(writer, "provider_binding_unavailable", "provider binding is unavailable")
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
	var failure probeFailure
	for _, binding := range bindings {
		adapter, ok := r.providers.AdapterForBinding(providerID, binding.ID)
		if !ok && len(instance.Bindings) == 0 {
			adapter, ok = r.providers.AdapterForProvider(providerID)
		}
		if !ok {
			// The symptom is always the same — no adapter — but the causes are
			// not, and they are fixed in different places: a credential, a
			// capability set, an endpoint policy. The load already decided which
			// one it was, so the refusal carries that class rather than making
			// the operator go read the log for it.
			reason := r.providers.UnavailableReason(providerID, binding.ID)
			if reason == "" {
				reason = excludedBindingAdapterMissing
			}
			r.logProbeRefusal(providerID, binding.ID, "provider binding adapter is unavailable")
			adminBadRequestCode(writer, reason, "provider binding adapter is unavailable")
			return
		}
		prober, ok := adapter.(provider.Prober)
		if !ok {
			r.logProbeRefusal(providerID, binding.ID, "provider does not support connection testing")
			adminBadRequestCode(writer, "provider_test_unsupported", "provider does not support connection testing")
			return
		}
		providerModel := providerProbeModel(instance, providerID, binding.ID, deployments, routes)
		// A probe that addresses a model has nothing to address until a
		// deployment names one. The adapters used to report that as a malformed
		// model — "invalid Bedrock model id" for an id that was never supplied —
		// which points the operator at a value they never chose instead of at
		// the deployment they have not created.
		if requirer, needs := adapter.(provider.ProbeModelRequirer); providerModel == "" && needs && requirer.ProbeRequiresModel() {
			r.logProbeRefusal(providerID, binding.ID, "connection test has no deployment to probe with")
			adminBadRequestCode(writer, probeRequiresDeployment, "connection test requires an enabled deployment on this binding")
			return
		}
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
		// Every failing binding is logged, not only the one the response
		// carries: a provider with three bindings that fails on one of them is
		// a different problem from one that fails on all three, and the console
		// only ever shows the first.
		r.logProbeFailure("provider", providerID, binding.ID, describeProbeFailure(err), latencyMS)
		if probeErr == nil {
			probeErr = err
			failure = describeProbeFailure(err)
			errorClass = failure.Class
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
		current.LastTestErrorClass = persistedProbeClass(failure)
	}
	current.UpdatedAt = testedAt
	action := "provider.test.success"
	if probeErr != nil {
		action = "provider.test.failure"
	}
	intent, intentErr := r.newAdminAuditIntent(request, action, "provider", providerID)
	if intentErr != nil {
		r.adminTopologyMu.Unlock()
		adminStoreError(writer)
		return
	}
	current, storeErr = r.store.PutProvider(request.Context(), current, testedRevision, intent)
	r.adminTopologyMu.Unlock()
	if storeErr != nil {
		r.logProbeResultWriteFailure("provider", providerID, storeErr)
		adminMutationError(writer, storeErr)
		return
	}
	r.completeAdminMutation(writer, request, *intent)
	result := map[string]any{
		"status": status, "latency_ms": maxLatencyMS, "tested_at": testedAt, "revision": current.Revision,
		"healthy_targets": healthyTargets, "total_targets": len(bindings),
	}
	writer.Header().Set("ETag", revisionETag(current.Revision))
	if probeErr != nil {
		result["error_class"] = errorClass
		failure.addTo(result)
		writeJSON(writer, http.StatusBadGateway, result)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

// probeFailure is what a failed connection test can say about itself. The
// stored record keeps only the class, which answers "what kind of failure" and
// nothing about which upstream refusal produced it — so the operator was left
// with a red "failed" and no way to tell an expired key from a wrong region.
// These fields travel in the response and the log, and are not persisted.
type probeFailure struct {
	Class     provider.ErrorClass
	Status    int
	Code      string
	RequestID string
	Reason    string
	// Type names the Go type of a failure that arrived unclassified, and is
	// empty for every classified one. It exists because the absence of an
	// upstream status means two different things on those two paths, and
	// probeFailureAttributes has to tell them apart.
	Type string
}

// maxProbeReasonLength bounds the upstream sentence. Provider error bodies are
// already read under a limit, but the reason reaches a table cell that does not
// want four kilobytes of it.
const maxProbeReasonLength = 300

// persistedProbeClass is the class a failed connection test stores.
//
// The record keeps a class and nothing else — no upstream status, no sentence —
// so the class is the whole account of the failure once the page is reloaded.
// That made Halro's own refusals unreadable: a probe the gateway rejected
// before sending anything carries class bad_request, which is the same value an
// upstream 4xx carries, and the console then reported "the upstream rejected
// this probe" for a request the upstream never saw.
//
// A bad_request with no upstream status is a refusal Halro made itself: the
// classes for a request that did leave (connect, timeout) say so on their own,
// and an upstream refusal always brings a status back with it.
func persistedProbeClass(failure probeFailure) string {
	if failure.Class == provider.ErrorBadRequest && failure.Status == 0 {
		return localProbeRefusalClass
	}
	return string(failure.Class)
}

// localProbeRefusalClass is stored, never produced by a provider. The console
// already had wording for it — it derives the same distinction from a live
// response, where the upstream status is still there to read — so a stored
// value reads back as the same sentence rather than needing a second key.
const localProbeRefusalClass = "bad_request_local"

func describeProbeFailure(err error) probeFailure {
	failure := probeFailure{Class: provider.ErrorUnknown}
	if err == nil {
		return failure
	}
	// The upstream sentence is forwarded to the console, and an operator who
	// pasted a key into a base URL or a project field gets it echoed back inside
	// the provider's own error. Redacting here covers the response; the log gets
	// this text only when Halro wrote it (see logProbeFailure).
	failure.Reason = truncateProbeReason(safelog.Redact(probeReason(err)))
	var classified *provider.Error
	if errors.As(err, &classified) {
		failure.Class = classified.Class
		failure.Status = classified.StatusCode
		failure.Code = provider.SafeProviderIdentifier(classified.ProviderCode)
		failure.RequestID = provider.SafeProviderIdentifier(classified.ProviderRequestID)
		return failure
	}
	// Nothing classified this, so nothing established which side of the
	// boundary wrote the sentence it carries. The type is recorded instead, and
	// the log reads it in place of the text.
	failure.Type = fmt.Sprintf("%T", err)
	return failure
}

// probeIdentifier bounds and narrows an identifier the upstream chose. Anything
// outside the shape real provider codes and request IDs take is dropped rather
// than trimmed, because a value that is not one of those is not an identifier
// and has no business in a log attribute.

// probeReason unwraps the cause a provider error carries. provider.Error.Error
// returns its own headline and stops there, so a transport refusal arrived as
// the bare sentence "provider probe failed" — which is the one case where the
// operator most needs the layer underneath, because it names the address or the
// allowlist entry that refused the dial.
func probeReason(err error) string {
	var classified *provider.Error
	if !errors.As(err, &classified) || classified.Cause == nil {
		return err.Error()
	}
	cause := classified.Cause.Error()
	if classified.Message == "" || strings.Contains(cause, classified.Message) {
		return cause
	}
	return classified.Message + ": " + cause
}

func truncateProbeReason(reason string) string {
	trimmed := strings.TrimSpace(reason)
	if len(trimmed) <= maxProbeReasonLength {
		return trimmed
	}
	return strings.ToValidUTF8(trimmed[:maxProbeReasonLength], "") + "…"
}

// addTo carries the failure into a test response. Absent fields are omitted
// rather than sent empty, so the console can tell "the provider said nothing"
// from "the provider said this".
func (f probeFailure) addTo(result map[string]any) {
	if f.Status > 0 {
		result["provider_status"] = f.Status
	}
	if f.Code != "" {
		result["provider_code"] = f.Code
	}
	if f.RequestID != "" {
		result["provider_request_id"] = f.RequestID
	}
	if f.Reason != "" {
		result["error_detail"] = f.Reason
	}
}

// logProbeRefusal records a connection test Halro turned down before probing
// anything. These paths answer 400 and return, so they used to leave no trace at
// all on the server: the operator saw a red result in the console, went to the
// log for the reason, and found the log silent — which reads as "the test never
// ran" rather than "the test was refused, here is why".
func (r *Runtime) logProbeRefusal(providerID, bindingID, reason string) {
	r.logger.Warn("provider connection test refused", "provider_id", providerID, "binding_id", bindingID, "reason", reason)
}

// logProbeResultWriteFailure records a connection test that ran and then could
// not be recorded. This is the one failure in the test path that says nothing
// about the upstream at all — the probe already answered — and it used to leave
// no server-side trace whatsoever: the console showed a refusal, the log showed
// a successful test or nothing, and the two disagreed with no way to tell which
// was describing what. A stale record the store now refuses is the case this was
// added for, and reading the store's own sentence is the whole diagnosis.
//
// The error comes from Halro's validation and storage layer, never from a
// provider response, so unlike a probe failure it carries no upstream body.
func (r *Runtime) logProbeResultWriteFailure(kind, id string, err error) {
	r.logger.Warn(kind+" connection test result could not be recorded",
		kind+"_id", id, "reason", truncateProbeReason(safelog.Redact(err.Error())))
}

// logProbeFailure records a connection test the operator ran and the upstream
// refused. Without it the only trace of a failed test is an audit action name,
// which says a test failed and never why. What may be said about the failure
// itself is probeFailureAttributes' decision, not this function's.
func (r *Runtime) logProbeFailure(kind, id, bindingID string, failure probeFailure, latencyMS int64) {
	attributes := []any{kind + "_id", id, "latency_ms", latencyMS}
	if bindingID != "" {
		attributes = append(attributes, "binding_id", bindingID)
	}
	r.logger.Warn(kind+" connection test failed", append(attributes, probeFailureAttributes(failure)...)...)
}

// probeFailureAttributes is everything about a failed probe that may be written
// to the process log, and it is deliberately the only place that decides so —
// every probe, manual or periodic, logs through here.
//
// What it leaves out is the upstream's own sentence. That text is a provider
// response body, which this repo does not persist anywhere outside its one-time
// response — the operator who ran a test still reads it in the reply, and the
// console reads it redacted through probeFailure.Reason, both of which live for
// one request rather than on disk. A pattern denylist is not a basis for writing
// an upstream body to a log file: it knows the credential formats it was told
// about, and the one thing an upstream is most likely to echo is the key it just
// refused.
//
// Reason is admitted only when no response came back at all. With no status
// there was no upstream body to quote, so the sentence is one Halro wrote — a
// dial failure, a refusal to probe — and withholding it would leave the line
// with nothing but a class.
//
// That inference needs a classified error to stand on. An unclassified one
// never went through the classification that decides what may be said about it,
// so it is either Halro's own error or an adapter that did not honour the
// contract, and from here the two are indistinguishable — one of them can be
// holding a response body. Its Go type goes in and its text stays out: a type
// name is produced by the code rather than by an upstream, and it answers what
// this line is actually asked, which is what produced a failure nothing
// classified.
func probeFailureAttributes(failure probeFailure) []any {
	attributes := []any{"error_class", string(failure.Class)}
	if failure.Status > 0 {
		attributes = append(attributes, "provider_status", failure.Status)
	}
	if failure.Code != "" {
		attributes = append(attributes, "provider_code", failure.Code)
	}
	if failure.RequestID != "" {
		attributes = append(attributes, "provider_request_id", failure.RequestID)
	}
	if failure.Type != "" {
		return append(attributes, "error_type", failure.Type)
	}
	if failure.Reason != "" && failure.Status == 0 {
		attributes = append(attributes, "reason", failure.Reason)
	}
	return attributes
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
		adminBadRequestCode(writer, "route_disabled", "route is disabled")
		return
	}
	deployment, deploymentErr := r.store.GetDeployment(request.Context(), route.DeploymentID)
	if deploymentErr != nil || deployment.DeletedAt != nil || !deployment.Enabled {
		adminBadRequestCode(writer, "route_deployment_unavailable", "route deployment is unavailable")
		return
	}
	providerID := deployment.ProviderID
	providerModel := deployment.ProviderModel
	capabilities := deployment.Capabilities
	instance, err := r.store.GetProvider(request.Context(), providerID)
	if err != nil || instance.DeletedAt != nil || !instance.Enabled {
		adminBadRequestCode(writer, "route_provider_unavailable", "route provider is unavailable")
		return
	}
	adapter, ok := adapterForDeployment(r.providers, instance, deployment)
	if !ok {
		adminBadRequestCode(writer, "route_provider_adapter_unavailable", "route provider adapter is unavailable")
		return
	}
	prober, ok := adapter.(provider.Prober)
	if !ok {
		adminBadRequestCode(writer, "provider_test_unsupported", "provider does not support connection testing")
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
	var failure probeFailure
	status := domain.DeploymentTestHealthy
	if probeErr != nil {
		status = domain.DeploymentTestUnhealthy
		failure = describeProbeFailure(probeErr)
		errorClass = failure.Class
		r.logProbeFailure("route", route.ID, "", failure, latencyMS)
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
		current.LastTestErrorClass = persistedProbeClass(failure)
	}
	current.UpdatedAt = testedAt
	action := "route.test.success"
	if probeErr != nil {
		action = "route.test.failure"
	}
	intent, intentErr := r.newAdminAuditIntent(request, action, "route", route.ID)
	if intentErr != nil {
		r.adminTopologyMu.Unlock()
		adminStoreError(writer)
		return
	}
	current, storeErr = r.store.PutRoute(request.Context(), current, testedRevision, intent)
	r.adminTopologyMu.Unlock()
	if storeErr != nil {
		r.logProbeResultWriteFailure("route", route.ID, storeErr)
		adminMutationError(writer, storeErr)
		return
	}
	r.completeAdminMutation(writer, request, *intent)
	result := map[string]any{
		"status": status, "latency_ms": latencyMS, "tested_at": testedAt, "revision": current.Revision,
	}
	writer.Header().Set("ETag", revisionETag(current.Revision))
	if probeErr != nil {
		result["error_class"] = errorClass
		failure.addTo(result)
		writeJSON(writer, http.StatusBadGateway, result)
		return
	}
	result["capabilities"] = capabilities
	writeJSON(writer, http.StatusOK, result)
}

func (r *Runtime) createAdminRoute(writer http.ResponseWriter, request *http.Request) {
	var input routeInput
	if err := decodeAdminJSON(request, &input); err != nil {
		adminBadRequestCode(writer, "invalid_request", "invalid request")
		return
	}
	idempotencyKey, ok := adminCreateIdempotencyKey(writer, request)
	if !ok {
		return
	}
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	routeID := adminCreateID("rte", "route", admin.session.Username, idempotencyKey)
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
	intent, intentErr := r.newAdminAuditIntent(request, "route.create", "route", route.ID)
	if intentErr != nil {
		adminStoreError(writer)
		return
	}
	route, err := r.store.PutRoute(request.Context(), route, 0, intent)
	if err != nil {
		if writeAdminCreateReplay(writer, err, "route", routeID) {
			return
		}
		adminMutationError(writer, err)
		return
	}
	r.activateTopologyAfterCommit()
	r.completeAdminMutation(writer, request, *intent)
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
		adminBadRequestCode(writer, "invalid_request", "invalid request")
		return
	}
	// Topology before projects, everywhere both are held, so the two coordinators
	// cannot deadlock. Projects are held because renaming a route's alias orphans
	// the old one exactly as deleting the route would.
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	r.adminProjectMu.Lock()
	defer r.adminProjectMu.Unlock()
	current, err := r.store.GetRoute(request.Context(), chi.URLParam(request, "id"))
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	// A tombstone is not editable; see the provider update handler.
	if current.DeletedAt != nil {
		adminNotFound(writer)
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
	if err := r.validateAliasKeepsServingProjects(request, current, route.PublicModel); err != nil {
		writeJSON(writer, http.StatusConflict, map[string]string{
			"error": err.Error(), "code": "route_referenced_by_project",
		})
		return
	}
	intent, intentErr := r.newAdminAuditIntent(request, "route.update", "route", route.ID)
	if intentErr != nil {
		adminStoreError(writer)
		return
	}
	route, err = r.store.PutRoute(request.Context(), route, expected, intent)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	r.activateTopologyAfterCommit()
	r.completeAdminMutation(writer, request, *intent)
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
	// Topology before projects, the one order both coordinators are taken in.
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	r.adminProjectMu.Lock()
	defer r.adminProjectMu.Unlock()
	route, err := r.store.GetRoute(request.Context(), chi.URLParam(request, "id"))
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	// Deleting a tombstone again would advance its revision and record a second
	// delete event for an object that is already gone.
	if route.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	if route.Revision != expected {
		adminPreconditionFailed(writer)
		return
	}
	if err := r.validateAliasKeepsServingProjects(request, route, ""); err != nil {
		writeJSON(writer, http.StatusConflict, map[string]string{
			"error": err.Error(), "code": "route_referenced_by_project",
		})
		return
	}
	now := time.Now().UTC()
	route.Enabled = false
	route.UpdatedAt = now
	route.DeletedAt = &now
	intent, intentErr := r.newAdminAuditIntent(request, "route.delete", "route", route.ID)
	if intentErr != nil {
		adminStoreError(writer)
		return
	}
	route, err = r.store.PutRoute(request.Context(), route, expected, intent)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	r.activateTopologyAfterCommit()
	r.completeAdminMutation(writer, request, *intent)
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
	policy := providerEndpointPolicy(r.config)
	endpoint, err := safetransport.ValidateURL(input.BaseURL, policy)
	if err != nil {
		return domain.Credential{}, err
	}
	audience, err := safetransport.AudienceWithPolicy(input.BaseURL, string(input.Type), policy)
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
	// Refused here rather than only hidden from the served matrix: the console is
	// one caller of this API, and a surface this build does not offer must not be
	// reachable by the others either. A rotation of a credential stored before the
	// profile was withheld lands here too, which is intended — it can be deleted,
	// not carried forward.
	if domain.IsWithheldProfile(profile.ProfileID) {
		return domain.Credential{}, fmt.Errorf("credential access surface %q is not supported by this build", profile.AccessSurface)
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
	if err := validateCredentialMaterial(profile.CredentialScheme, endpoint, plaintext); err != nil {
		return domain.Credential{}, err
	}
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
	var expiresAt *time.Time
	if input.ExpiresAt != nil {
		if input.ExpiresAt.IsZero() {
			return domain.Credential{}, errors.New("credential expiry must be a real instant or absent")
		}
		normalized := input.ExpiresAt.UTC()
		expiresAt = &normalized
	}
	credential := domain.Credential{
		ID: id, Name: input.Name, Type: input.Type, AccessSurface: profile.AccessSurface,
		Scheme: profile.CredentialScheme, Audience: audience,
		Ciphertext: ciphertext, KeyVersion: keyVersion, ExpiresAt: expiresAt,
		CreatedAt: createdAt, UpdatedAt: now,
	}
	return credential, credential.Validate()
}

// validateCredentialMaterial runs the credential through the same constructor
// the registry load will run, at the one moment the operator can still fix the
// value.
//
// Only the AWS SigV4 scheme has material with a shape: the static-header schemes
// carry an opaque token whose only requirement — non-empty — the caller has
// already checked. An AWS document, by contrast, declares its own region, and
// the signer pins the host to that region; a credential bound to us-east-2 while
// its JSON says us-east-1 used to save cleanly and then be dropped at load, so
// the console reported "provider binding adapter is unavailable" on a record
// that looked fine and named nothing to change.
//
// The errors are safe to return to the admin: they name the field or the
// disagreement, never the key material and never the host.
func validateCredentialMaterial(scheme domain.CredentialScheme, endpoint *url.URL, plaintext []byte) error {
	if scheme != domain.CredentialAWSSigV4Explicit {
		return nil
	}
	authorizer, err := bedrockprovider.NewAuthorizer(endpoint, plaintext, nil)
	if err != nil {
		return err
	}
	authorizer.Close()
	return nil
}

// bedrockProjectIDError marks the one provider refusal an operator is most
// likely to hit and least able to act on from a generic message: a value that
// is not a Bedrock Project ID. Typed rather than matched on message text, so
// the console can say what to do about it in the reader's own language.
type bedrockProjectIDError struct{ err error }

func (e bedrockProjectIDError) Error() string { return e.err.Error() }
func (e bedrockProjectIDError) Unwrap() error { return e.err }

// credentialMatchError marks the refusals that follow from which credential the
// operator picked rather than from a malformed field. A credential is sealed
// against one provider type and one base URL, so "does not match" has several
// distinct causes with different fixes, and a single message covering all of
// them tells the reader nothing about which value to change. Each cause carries
// its own code and the two values that disagreed.
type credentialMatchError struct {
	code   string
	fields map[string]string
	err    error
}

func (e credentialMatchError) Error() string { return e.err.Error() }
func (e credentialMatchError) Unwrap() error { return e.err }

// capabilityAssignmentError marks a capability set no connection of this shape
// can carry. It names the capabilities rather than only the fact, because the
// operator's next action is to untick one of them — and it carries them as keys
// so the console can print them in the reader's language instead of echoing
// `provider_executed_tools` at them.
type capabilityAssignmentError struct {
	code         string
	capabilities []string
	err          error
}

func (e capabilityAssignmentError) Error() string { return e.err.Error() }
func (e capabilityAssignmentError) Unwrap() error { return e.err }

// adminProviderInputError answers a rejected provider payload. Most refusals
// are self-explanatory in context and stay code-less; the ones that are not
// carry a stable code so the console can localise them.
func adminProviderInputError(writer http.ResponseWriter, err error) {
	var projectID bedrockProjectIDError
	if errors.As(err, &projectID) {
		adminBadRequestCode(writer, "bedrock_project_id_invalid", err.Error())
		return
	}
	var credentialMatch credentialMatchError
	if errors.As(err, &credentialMatch) {
		adminBadRequestFields(writer, credentialMatch.code, err.Error(), credentialMatch.fields)
		return
	}
	var capabilities capabilityAssignmentError
	if errors.As(err, &capabilities) {
		adminBadRequestFields(writer, capabilities.code, err.Error(), map[string]string{
			"capabilities": strings.Join(capabilities.capabilities, ","),
		})
		return
	}
	adminBadRequest(writer, err.Error())
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
	policy := providerEndpointPolicy(r.config)
	endpoint, err := safetransport.ValidateURL(input.BaseURL, policy)
	if err != nil {
		return domain.ProviderInstance{}, err
	}
	credential, err := r.store.GetCredential(request.Context(), input.CredentialID)
	if err != nil {
		return domain.ProviderInstance{}, errors.New("credential was not found")
	}
	audience, err := safetransport.AudienceWithPolicy(input.BaseURL, string(input.Type), policy)
	if err != nil {
		return domain.ProviderInstance{}, err
	}
	if credential.Type != input.Type {
		return domain.ProviderInstance{}, credentialMatchError{
			code: "credential_type_mismatch",
			fields: map[string]string{
				"credential_provider_type": string(credential.Type),
				"provider_type":            string(input.Type),
			},
			err: fmt.Errorf(
				"credential is for provider type %s, this provider is %s",
				credential.Type, input.Type,
			),
		}
	}
	// The audience seals the credential to the base URL it was encrypted for, so
	// a base URL edit after the credential was saved lands here rather than at
	// the upstream. Both origins go back as data: which one to change is the
	// operator's call, and they can only make it if they can see both.
	if credential.Audience != audience {
		return domain.ProviderInstance{}, credentialMatchError{
			code: "credential_base_url_mismatch",
			fields: map[string]string{
				"credential_base_url": credentialOrigin(credential.Audience, credential.Type),
				"provider_base_url":   credentialOrigin(audience, input.Type),
			},
			err: fmt.Errorf(
				"credential is bound to %s, this provider's base URL is %s",
				credentialOrigin(credential.Audience, credential.Type),
				credentialOrigin(audience, input.Type),
			),
		}
	}
	requestedProfile := input.ProfileID
	if requestedProfile == "" {
		requestedProfile = currentProfile
	}
	profile, ok := domain.ResolveProviderProfile(input.Type, requestedProfile)
	if !ok {
		return domain.ProviderInstance{}, errors.New("provider profile is not implemented")
	}
	if domain.IsWithheldProfile(profile.ProfileID) {
		return domain.ProviderInstance{}, errors.New("the selected capability implementation is not supported by this build")
	}
	if input.AccessSurface != "" && input.AccessSurface != profile.AccessSurface ||
		input.ProfileID != "" && input.ProfileID != profile.ProfileID ||
		input.CredentialScheme != "" && input.CredentialScheme != profile.CredentialScheme {
		return domain.ProviderInstance{}, errors.New("provider access surface, profile, or credential scheme is incompatible")
	}
	if credential.AccessSurface != profile.AccessSurface || credential.Scheme != profile.CredentialScheme {
		return domain.ProviderInstance{}, credentialMatchError{
			code: "credential_surface_mismatch",
			fields: map[string]string{
				"credential_access_surface": string(credential.AccessSurface),
				"provider_access_surface":   string(profile.AccessSurface),
			},
			err: fmt.Errorf(
				"credential is for access surface %s, this provider uses %s",
				credential.AccessSurface, profile.AccessSurface,
			),
		}
	}
	if profile.AccessSurface == domain.SurfaceBedrockMantle {
		if err := bedrockmantleprovider.ValidateEndpoint(endpoint); err != nil {
			return domain.ProviderInstance{}, err
		}
	}
	// `default` is the ID AWS lists for the account default project, and sending
	// it as a header is indistinguishable from sending none; normalising here
	// means one stored spelling for "the default" rather than two.
	bedrockProjectID := domain.NormalizeBedrockProjectID(input.BedrockProjectID)
	if err := domain.ValidateBedrockProjectID(bedrockProjectID); err != nil {
		return domain.ProviderInstance{}, bedrockProjectIDError{err}
	}
	if bedrockProjectID != "" && profile.AccessSurface != domain.SurfaceBedrockMantle {
		return domain.ProviderInstance{}, bedrockProjectIDError{
			errors.New("bedrock project id is only valid on the Bedrock Mantle access surface"),
		}
	}
	allowedBetas := domain.NormalizeAnthropicBetaTokens(input.AllowedAnthropicBetas)
	if err := domain.ValidateAnthropicBetaTokens(allowedBetas); err != nil {
		return domain.ProviderInstance{}, err
	}
	if len(allowedBetas) > 0 && profile.AccessSurface != domain.SurfaceAnthropic && profile.AccessSurface != domain.SurfaceBedrockMantle {
		return domain.ProviderInstance{}, errors.New("anthropic beta tokens are only valid on an Anthropic-wire access surface")
	}
	instance := domain.ProviderInstance{
		ID: id, Name: input.Name, Type: input.Type, BaseURL: input.BaseURL,
		APIVersion:            strings.TrimSpace(input.APIVersion),
		CredentialID:          input.CredentialID,
		AccessSurface:         profile.AccessSurface,
		ProfileID:             profile.ProfileID,
		CredentialScheme:      profile.CredentialScheme,
		BedrockProjectID:      bedrockProjectID,
		AllowedAnthropicBetas: allowedBetas,
		AllowedHosts:          []string{strings.ToLower(endpoint.Hostname())},
		MaxConcurrency:        input.MaxConcurrency,
		Enabled:               input.Enabled, CreatedAt: createdAt, UpdatedAt: updatedAt,
	}
	// One flat set in, one binding per profile that will serve part of it out.
	// The caller says what the connection should do; which profile does it is
	// read from the matrix, so the console, a script, and a restore all get the
	// same answer to a question none of them has to know how to ask.
	//
	// Omitting the field means "leave this alone", never "reset it". On an update
	// it resolves to what the connection already serves, so a rename-only PUT
	// keeps every binding and every narrowing the operator applied; resolving to
	// the profile's defaults instead dropped an OpenAI connection's media binding
	// and re-widened its chat capabilities, with a 200 and nothing said. Only a
	// create, which has nothing to preserve, starts from the defaults of the
	// profile the request named.
	requested := domain.DefaultProviderCapabilitiesForProfile(input.Type, profile.ProfileID)
	if len(currentBindings) > 0 {
		requested, _ = domain.BindingsCapabilitiesSummary(currentBindings)
	}
	if input.Capabilities != nil {
		requested = *input.Capabilities
	}
	assignment := domain.AssignConnectionCapabilities(input.Type, profile.ProfileID, requested)
	// Refused, not filtered. Intersecting the request with what the connection
	// can serve is the natural implementation and the wrong one: it stores a
	// connection that does less than was asked for, and nothing tells the caller
	// which capability went missing.
	if len(assignment.Unservable) > 0 {
		return domain.ProviderInstance{}, capabilityAssignmentError{
			code:         "capabilities_unservable",
			capabilities: assignment.Unservable,
			err: fmt.Errorf("this connection cannot serve %s",
				strings.Join(assignment.Unservable, ", ")),
		}
	}
	// Several profiles on this connection could serve it and the operator did not
	// say which. Choosing by table order would bind the connection to a protocol
	// nobody picked, so the request is refused and names the capability.
	if len(assignment.Ambiguous) > 0 {
		return domain.ProviderInstance{}, capabilityAssignmentError{
			code:         "capabilities_ambiguous",
			capabilities: assignment.Ambiguous,
			err: fmt.Errorf("more than one capability implementation on this connection can serve %s",
				strings.Join(assignment.Ambiguous, ", ")),
		}
	}
	// A token limit only exists where a profile declares one. Asking this
	// connection to hold one it has nowhere to put is refused rather than
	// dropped: the alternative stores a connection the caller believes is
	// bounded and nothing on it is. Model token specifications belong to the
	// Deployment, which declares them per model.
	if len(assignment.Unboundable) > 0 {
		return domain.ProviderInstance{}, capabilityAssignmentError{
			code:         "capabilities_limit_unavailable",
			capabilities: assignment.Unboundable,
			err: fmt.Errorf("no capability implementation on this connection bounds %s",
				strings.Join(assignment.Unboundable, ", ")),
		}
	}
	// Asked for a larger bound than the profile that would hold it allows. Its
	// own refusal, because the fix is a smaller number rather than a dropped
	// field, and "cannot serve maximum context tokens" describes neither.
	if len(assignment.Exceeded) > 0 {
		return domain.ProviderInstance{}, capabilityAssignmentError{
			code:         "capabilities_limit_too_large",
			capabilities: assignment.Exceeded,
			err: fmt.Errorf("this connection cannot bound %s that high",
				strings.Join(assignment.Exceeded, ", ")),
		}
	}
	if len(assignment.Assignments) == 0 {
		return domain.ProviderInstance{}, errors.New("provider must declare at least one operation capability")
	}
	// The caller named an implementation and none of the enabled capabilities
	// land on it. Refused rather than quietly re-pointed: the connection's
	// profile projection is taken from the first binding below, so accepting it
	// would answer 201 with a profile_id the caller did not ask for — the same
	// silent substitution the ambiguity check above exists to prevent.
	if input.ProfileID != "" && !slices.ContainsFunc(assignment.Assignments,
		func(a domain.ProfileCapabilityAssignment) bool { return a.ProfileID == input.ProfileID }) {
		return domain.ProviderInstance{}, errors.New("the selected capability implementation serves none of the enabled capabilities")
	}
	instance.Bindings = make([]domain.ProviderProfileBinding, 0, len(assignment.Assignments))
	for _, assigned := range assignment.Assignments {
		bound, ok := domain.ResolveProviderProfile(input.Type, assigned.ProfileID)
		if !ok {
			return domain.ProviderInstance{}, errors.New("provider binding profile is not implemented")
		}
		if domain.IsWithheldProfile(assigned.ProfileID) {
			return domain.ProviderInstance{}, errors.New("a selected capability implementation is not supported by this build")
		}
		binding := domain.ProviderProfileBinding{
			ID:               domain.DefaultProviderProfileBindingID(id, assigned.ProfileID),
			ProviderID:       id,
			ProfileID:        assigned.ProfileID,
			AccessSurface:    bound.AccessSurface,
			CredentialScheme: bound.CredentialScheme,
			Capabilities:     assigned.Capabilities,
			// A binding exists because it carries capabilities, so it is enabled
			// even when the connection is not. Tying it to the connection's own flag
			// would empty the stored capability summary the moment an operator
			// disables a connection — and the form reads that summary back, so
			// re-enabling would come up with nothing ticked.
			Enabled: true,
		}
		if binding.CredentialScheme != credential.Scheme || binding.AccessSurface != credential.AccessSurface {
			return domain.ProviderInstance{}, errors.New("provider binding credential profile does not match connection")
		}
		// A bound the operator already narrowed survives an edit that says nothing
		// about it. The console sends zero for both limits — it has no field for
		// them — so without this, disabling and re-enabling a connection widened a
		// Titan Embed binding from a chosen 4096 back to the profile's full 8192,
		// removing a routing guard nobody asked to remove.
		binding.Capabilities.MaxContextTokens = retainedLimit(
			requested.MaxContextTokens, binding.Capabilities.MaxContextTokens,
			storedBindingLimits(currentBindings, binding.ID, assigned.ProfileID).MaxContextTokens)
		binding.Capabilities.MaxOutputTokens = retainedLimit(
			requested.MaxOutputTokens, binding.Capabilities.MaxOutputTokens,
			storedBindingLimits(currentBindings, binding.ID, assigned.ProfileID).MaxOutputTokens)
		binding.CapabilityEvidence = preserveCapabilityEvidence(
			binding.Capabilities, previousBindingEvidence(currentBindings, binding.ID, assigned.ProfileID, currentEvidence),
		)
		instance.Bindings = append(instance.Bindings, binding)
	}
	primary := instance.Bindings[0]
	instance.ProfileID, instance.AccessSurface, instance.CredentialScheme = primary.ProfileID, primary.AccessSurface, primary.CredentialScheme
	instance.Capabilities, instance.CapabilityEvidence = domain.BindingsCapabilitiesSummary(instance.Bindings)
	return instance, instance.Validate()
}

// retainedLimit keeps a narrower bound the connection already carried when the
// request does not speak about it. A request that names a value wins, and a
// binding that did not exist before keeps what the assignment gave it.
func retainedLimit(requested, assigned, stored int64) int64 {
	if requested > 0 || stored <= 0 {
		return assigned
	}
	if assigned > 0 && stored > assigned {
		return assigned
	}
	return stored
}

// storedBindingLimits is what this binding carried before the edit, if it
// existed. A binding being added now has nothing to retain.
func storedBindingLimits(
	current []domain.ProviderProfileBinding,
	bindingID string,
	profileID domain.ProviderProfileID,
) domain.ProviderCapabilities {
	for _, binding := range current {
		if binding.ID == bindingID && binding.ProfileID == profileID {
			return binding.Capabilities
		}
	}
	return domain.ProviderCapabilities{}
}

// previousBindingEvidence finds what was already known about this binding's
// capabilities, so a detection result survives an unrelated edit.
//
// The fallback to the connection's own evidence covers the record written before
// bindings existed, whose evidence lives on the instance: without it, editing
// such a connection would demote every verified capability back to declared.
func previousBindingEvidence(
	current []domain.ProviderProfileBinding,
	bindingID string,
	profileID domain.ProviderProfileID,
	instanceEvidence domain.CapabilityEvidenceSet,
) domain.CapabilityEvidenceSet {
	for _, binding := range current {
		if binding.ID == bindingID && binding.ProfileID == profileID {
			return binding.CapabilityEvidence
		}
	}
	if len(current) == 0 {
		return instanceEvidence
	}
	return nil
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
		ID: id, PublicModel: input.PublicModel, DeploymentID: input.DeploymentID,
		Priority: input.Priority,
		Strategy: input.Strategy, Enabled: input.Enabled, CreatedAt: createdAt, UpdatedAt: updatedAt,
	}
}

func (r *Runtime) validateAdminRoute(request *http.Request, candidate domain.Route, replacingID string) error {
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
		// Two enabled routes on one alias pointing at the same deployment read
		// as two targets and are not two: they share the deployment's price,
		// probe, capability snapshot and concurrency limit. The circuit breaker
		// keys on the route ID, so it does not merge them either — the second
		// one is tried immediately after the first failed, against the thing
		// that just failed. Refused here rather than explained in the console,
		// because a console warning does not reach the Admin API.
		//
		// This dedupes the deployment, which is not the same as deduping the
		// failure domain: nothing stops two deployment records naming one
		// binding and one upstream model, and two routes onto those are still
		// one credential and one quota. Closing that would mean uniqueness on
		// (provider, binding, provider model), which is a rule about what a
		// deployment is, not about what a route may point at — it belongs to
		// the deployment write path and to a decision that has not been made.
		if route.DeploymentID == candidate.DeploymentID {
			return errors.New("another enabled route already points this public model at the same deployment")
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
		audience, err := safetransport.AudienceWithPolicy(instance.BaseURL, string(instance.Type), providerEndpointPolicy(r.config))
		// A rotation can move the credential along two axes, and naming the wrong
		// one is worse than naming neither: a type change reports two identical
		// endpoints and sends the operator to correct a base URL that is already
		// right.
		if instance.Type != credential.Type {
			return credentialMatchError{
				code: "credential_type_in_use",
				fields: map[string]string{
					"credential_provider_type": string(credential.Type),
					"provider_provider_type":   string(instance.Type),
					"provider_name":            instance.Name,
				},
				err: fmt.Errorf(
					"provider %q uses this credential as provider type %s, which this rotation would change to %s",
					instance.Name, instance.Type, credential.Type,
				),
			}
		}
		if err != nil || audience != credential.Audience {
			return credentialMatchError{
				code: "credential_endpoint_in_use",
				fields: map[string]string{
					"credential_base_url": credentialOrigin(credential.Audience, credential.Type),
					"provider_base_url":   credentialOrigin(audience, instance.Type),
					"provider_name":       instance.Name,
				},
				err: fmt.Errorf(
					"provider %q uses this credential at %s, which this rotation would move to %s",
					instance.Name, credentialOrigin(audience, instance.Type),
					credentialOrigin(credential.Audience, credential.Type),
				),
			}
		}
		// The third axis. A credential is sealed against one access surface and
		// one scheme as much as against a type and an endpoint, and the registry
		// refuses to load at all when a binding disagrees with the credential it
		// was issued for — not by withholding that provider, but by failing the
		// whole load. Checking only type and audience here let a rotation move a
		// credential onto a surface no binding uses and take the data plane down
		// with it, so the surface is compared against every binding that would
		// have to accept the rotated credential.
		for _, binding := range providerCredentialProfiles(instance) {
			if binding.AccessSurface == credential.AccessSurface && binding.CredentialScheme == credential.Scheme {
				continue
			}
			return credentialMatchError{
				code: "credential_surface_in_use",
				fields: map[string]string{
					"credential_access_surface": string(credential.AccessSurface),
					"provider_access_surface":   string(binding.AccessSurface),
					"provider_name":             instance.Name,
				},
				err: fmt.Errorf(
					"provider %q uses this credential on access surface %s, which this rotation would change to %s",
					instance.Name, binding.AccessSurface, credential.AccessSurface,
				),
			}
		}
	}
	return nil
}

// providerCredentialProfiles answers the (surface, scheme) pairs a connection
// would present the credential with. Bindings are the authority — the
// instance's own fields are a projection of the first one — but a record
// written before bindings existed has none, and its projection is then the only
// declaration there is.
func providerCredentialProfiles(instance domain.ProviderInstance) []domain.ProviderProfileBinding {
	if len(instance.Bindings) > 0 {
		return instance.Bindings
	}
	return []domain.ProviderProfileBinding{{
		AccessSurface:    instance.AccessSurface,
		CredentialScheme: instance.CredentialScheme,
	}}
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
	return nil
}

// validateAliasKeepsServingProjects refuses a route mutation that would leave a
// live Project allowing a public model alias nothing serves.
//
// Project writes already reject an unknown alias, on the stated grounds that an
// alias with no route only fails at request time and does so silently. The
// reverse direction had no such check, so referential integrity held in one
// direction only: deleting the last route for an alias — or renaming it, which
// removes the old alias just as effectively — left the Project authorizing
// something that could only ever answer 404. Worse, the Project then could not be
// saved at all, because its own validator rejects the alias it already holds, so
// an unrelated edit to its budget or CIDRs was blocked until someone noticed why.
//
// replacementAlias is what the route will serve after the mutation, empty for a
// delete. The rule reads the same way as validateProjectReferences: a route
// exists if it is not a tombstone, disabled or not, because a disabled route is
// a deliberate maintenance state the console shows as unavailable.
//
// The caller must hold adminProjectMu as well as adminTopologyMu. Checking
// without it would only narrow the race the review described: Project validation
// sees the route, the route is deleted concurrently, the Project commits.
func (r *Runtime) validateAliasKeepsServingProjects(
	request *http.Request,
	route domain.Route,
	replacementAlias string,
) error {
	if route.PublicModel == replacementAlias {
		return nil
	}
	routes, err := r.store.ListRoutes(request.Context())
	if err != nil {
		return errors.New("routes are unavailable")
	}
	for _, other := range routes {
		if other.ID == route.ID || other.DeletedAt != nil {
			continue
		}
		if other.PublicModel == route.PublicModel {
			// The alias keeps a route, so this is a capacity change.
			return nil
		}
	}
	projects, err := r.store.ListProjects(request.Context())
	if err != nil {
		return errors.New("projects are unavailable")
	}
	for _, project := range projects {
		if project.DeletedAt != nil {
			continue
		}
		if slices.Contains(project.AllowedModels, route.PublicModel) {
			return fmt.Errorf(
				"this is the last route for model alias %q; remove it from project %q's allowed models first",
				route.PublicModel, project.Name,
			)
		}
	}
	return nil
}

// validateBindingsCanDeactivate is the same rule one level down, and it was the
// level that had no rule at all.
//
// Provider and Deployment both refuse to leave service while something
// downstream still names them. A Profile Binding did not: because bindings are
// replaced wholesale on update and each one's Enabled comes straight from the
// request, switching off — or simply omitting — a binding that an enabled
// deployment is bound to was accepted. The Provider record stayed valid, since
// domain validation only requires *some* enabled binding, so the write
// committed and left a deployment pointing at a binding that produces no
// adapter.
//
// The registry no longer treats that as fatal, so it can no longer brick the
// data directory. This is still the right answer for a deliberate edit: the
// operator gets told which deployment is in the way instead of watching an
// alias quietly stop answering.
func (r *Runtime) validateBindingsCanDeactivate(
	request *http.Request,
	instance domain.ProviderInstance,
) error {
	deployments, err := r.store.ListDeployments(request.Context())
	if err != nil {
		return errors.New("deployments are unavailable")
	}
	enabled := make(map[string]bool)
	for _, binding := range instance.EffectiveProfileBindings() {
		if binding.Enabled {
			enabled[binding.ID] = true
		}
	}
	for _, deployment := range deployments {
		if deployment.ProviderID != instance.ID || !deployment.Enabled || deployment.DeletedAt != nil {
			continue
		}
		// Resolved exactly the way the registry resolves it, so the check and the
		// thing it is protecting cannot disagree about which binding a deployment
		// runs on.
		bindingID := deployment.BindingID
		if bindingID == "" {
			bindingID = matchingBindingID(instance, deployment.ProfileID)
		}
		if !enabled[bindingID] {
			return fmt.Errorf(
				"model deployment %q runs on this capability interface; disable or delete it before switching the interface off",
				deployment.Name,
			)
		}
	}
	return nil
}

// credentialOrigin recovers the base URL an audience seals a credential to. The
// audience is that origin with the provider type appended, and the type is what
// the reader already knows; the origin is the part they can act on.
func credentialOrigin(audience string, providerType domain.ProviderType) string {
	return strings.TrimSuffix(audience, ":"+string(providerType))
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
		BoundBaseURL:     credentialOrigin(item.Audience, item.Type),
		SecretConfigured: len(item.Ciphertext) > 0, KeyVersion: item.KeyVersion,
		ExpiresAt: item.ExpiresAt,
		Revision:  item.Revision,
	}
}

func adminConfigurationError(writer http.ResponseWriter, err error) {
	writeJSON(writer, http.StatusConflict, map[string]string{
		"error": "configuration could not be activated",
	})
}
