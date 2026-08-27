package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/id"
	"github.com/akz142857/Halro/internal/modelcatalog"
	"github.com/akz142857/Halro/internal/provider"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/go-chi/chi/v5"
)

type modelCapabilityDetectionInput struct {
	ProviderModel     string                      `json:"provider_model"`
	TargetKind        domain.DeploymentTargetKind `json:"target_kind,omitempty"`
	Region            string                      `json:"region,omitempty"`
	BindingID         string                      `json:"binding_id,omitempty"`
	ProfileID         domain.ProviderProfileID    `json:"profile_id,omitempty"`
	RiskTier          string                      `json:"risk_tier"`
	SelectionRevision string                      `json:"selection_revision,omitempty"`
	ForceRefresh      bool                        `json:"force_refresh,omitempty"`
}

type capabilityDetectionRuntime struct {
	mu        sync.Mutex
	cancels   map[string]context.CancelFunc
	global    chan struct{}
	providers map[string]chan struct{}
	rateMu    sync.Mutex
	rate      adminRateState
}

var (
	errNoDetectableCapabilityBinding = errors.New("current capability interface does not support automatic detection")
	errAmbiguousCapabilityBinding    = errors.New("capability interface cannot be determined for this model; select one explicitly")
)

func (r *Runtime) createAdminModelCapabilityDetection(writer http.ResponseWriter, request *http.Request) {
	key := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 256 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "Idempotency-Key is required", "code": "idempotency_key_required"})
		return
	}
	var input struct {
		modelCapabilityDetectionInput
		stepUpMaterial
	}
	if err := decodeAdminJSON(request, &input); err != nil {
		adminBadRequest(writer, "invalid request")
		return
	}
	// Detection spends the operator's Provider credential on up to
	// MaxProviderCalls upstream calls and writes the capability evidence a
	// Deployment can later adopt. Both halves are outside project accounting,
	// so a stolen session is the only thing between an attacker and a bill;
	// see requireStepUpMaterial.
	//
	// Asked once per elevation window rather than once per detection. The
	// reasons above hold for every detection, so the guard stays; what changes
	// is that configuring six Deployments costs one proof instead of six, which
	// is the flow the per-action prompt was interrupting. See
	// requireDetectionStepUp for what the window is bound to and what it is not.
	if !r.requireDetectionStepUp(writer, request, input.stepUpMaterial) {
		return
	}
	input.ProviderModel, input.Region, input.BindingID = strings.TrimSpace(input.ProviderModel), strings.TrimSpace(input.Region), strings.TrimSpace(input.BindingID)
	if input.RiskTier == "" {
		input.RiskTier = "safe_automatic"
	}
	if input.ProviderModel == "" || len(input.ProviderModel) > 512 || len(input.SelectionRevision) > 256 || input.RiskTier != "safe_automatic" {
		adminBadRequest(writer, "invalid capability detection target")
		return
	}
	providerID := chi.URLParam(request, "id")
	instance, err := r.store.GetProvider(request.Context(), providerID)
	if err != nil {
		adminStoreError(writer)
		return
	}
	credential, err := r.store.GetCredential(request.Context(), instance.CredentialID)
	if err != nil {
		adminStoreError(writer)
		return
	}
	targetKind, err := deploymentTargetKind(instance.Type, instance.AccessSurface, instance.ProfileID, input.TargetKind)
	if err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	input.TargetKind = targetKind
	// A Mantle detection carries the same region rule a deployment does: the
	// probes run against the provider's own regional endpoint whatever label the
	// request carries, and a detection stamped with another region is evidence no
	// deployment can adopt — deploymentFromInput refuses that region outright.
	// Billable probes for a dead result is the worst version of this.
	if err := validateBedrockMantleRegion(instance, input.Region); err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	region := input.Region
	if region == "" {
		region = providerRegion(instance)
	}
	catalogCandidates, probeCandidates, err := r.capabilityDetectionCandidates(instance, input.modelCapabilityDetectionInput, region)
	if err == nil {
		err = capabilityCandidateError(catalogCandidates, probeCandidates)
	}
	if err != nil {
		code := "no_detectable_binding"
		if errors.Is(err, errAmbiguousCapabilityBinding) {
			code = "ambiguous_capability_binding"
		}
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": code})
		return
	}
	catalogKnown := len(catalogCandidates) == 1
	resolved := probeCandidates
	if catalogKnown {
		resolved = catalogCandidates
	}
	candidates := make([]domain.DetectionBindingCandidate, 0, len(resolved))
	candidateIDs := make([]string, 0, len(resolved))
	for _, item := range resolved {
		candidates = append(candidates, domain.DetectionBindingCandidate{BindingID: item.binding.ID, ProfileID: item.binding.ProfileID,
			AccessSurface: item.binding.AccessSurface, ModelRevision: item.entry.Revision(), Verifiable: item.verifiable, Status: domain.ProbeNotProbed})
		candidateIDs = append(candidateIDs, item.binding.ID)
	}
	now := r.now().UTC()
	requestShape := map[string]any{"provider_id": providerID, "provider_revision": instance.Revision, "credential_revision": credential.Revision,
		"credential_key_version": credential.KeyVersion, "provider_model": input.ProviderModel, "target_kind": targetKind,
		"region": region, "binding_candidates": candidateIDs, "risk_tier": input.RiskTier,
		"selection_revision": input.SelectionRevision, "force_refresh": input.ForceRefresh}
	requestHash := hashCanonical(requestShape)
	// Cooldown keys off what was asked for, so a repeated ask cools down even
	// while the interface is still unknown. The resolved-target fingerprint
	// deployment creation checks is written once identification has an answer.
	fingerprint := hashCanonical(map[string]any{"provider_id": providerID, "provider_revision": instance.Revision,
		"credential_revision": credential.Revision, "credential_key_version": credential.KeyVersion,
		"binding_candidates": candidateIDs, "provider_model": input.ProviderModel,
		"target_kind": targetKind, "canonical_target": input.ProviderModel, "region": region,
		"detector_version": provider.CapabilityDetectorContractVersion, "risk_tier": input.RiskTier})
	if input.ForceRefresh {
		items, listErr := r.store.ListModelCapabilityDetections(request.Context())
		if listErr != nil {
			adminStoreError(writer)
			return
		}
		cooldown := r.config.Admin.ModelCapabilityDetection.RefreshCooldown.Value()
		for _, item := range items {
			if item.SelectionFingerprint == fingerprint && now.Sub(item.UpdatedAt) < cooldown {
				writeJSON(writer, http.StatusTooManyRequests, map[string]string{"error": "capability detection refresh is cooling down", "code": "capability_detection_cooldown"})
				return
			}
		}
	}
	detectionID, err := id.New("mcd")
	if err != nil {
		adminStoreError(writer)
		return
	}
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	detection := domain.ModelCapabilityDetection{ID: detectionID, ProviderID: providerID, ProviderRevision: instance.Revision,
		CredentialRevision: credential.Revision, CredentialKeyVersion: credential.KeyVersion, ProviderModel: input.ProviderModel,
		ModelRevision: candidates[0].ModelRevision, Candidates: candidates,
		TargetKind: targetKind, CanonicalTarget: input.ProviderModel, Region: region, SelectionFingerprint: fingerprint,
		DetectorVersion: provider.CapabilityDetectorContractVersion, RiskTier: input.RiskTier, Status: domain.DetectionQueued,
		Source: string(modelcatalog.SourceVerifiedProbe), Results: map[string]domain.CapabilityProbeResult{},
		MaxProviderCalls: r.config.Admin.ModelCapabilityDetection.MaxProviderCalls, CreatedBy: admin.session.Username,
		IdempotencyKeyHash: hashCanonical(map[string]any{"actor": admin.session.Username, "key": key}), RequestHash: requestHash,
		SelectionRevision: input.SelectionRevision, ForceRefresh: input.ForceRefresh, CreatedAt: now, UpdatedAt: now}
	if catalogKnown {
		// The catalog already names the interface, so there is nothing to
		// identify and nothing to spend.
		binding, entry := resolved[0].binding, resolved[0].entry
		detection.BindingID, detection.ProfileID, detection.AccessSurface = binding.ID, binding.ProfileID, binding.AccessSurface
		detection.TargetFingerprint = detectionTargetFingerprint(providerID, instance.Revision, credential.Revision, credential.KeyVersion,
			binding.ID, binding.ProfileID, binding.AccessSurface, input.ProviderModel, targetKind, region, input.RiskTier)
		detection.Status, detection.Source = domain.DetectionCompleted, string(modelcatalog.SourceBuiltin)
		detection.Recommended = modelcatalog.Clamp(entry.Capabilities, binding.Capabilities)
		detection.MaxProviderCalls, detection.CompletedAt = 0, &now
	}
	detection, replayed, err := r.store.CreateModelCapabilityDetection(request.Context(), detection, now)
	if errors.Is(err, boltstore.ErrIdempotencyConflict) {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": err.Error(), "code": "idempotency_conflict"})
		return
	}
	if err != nil {
		adminStoreError(writer)
		return
	}
	if replayed || detection.Status == domain.DetectionCompleted {
		if replayed {
			r.capabilityMetrics.recordDetectionCache("hit")
		} else {
			r.capabilityMetrics.recordDetectionCache("catalog")
			r.capabilityMetrics.recordDetectionFinish(string(instance.Type), string(detection.Status), detection.Source, detection.Results, 0, 0)
		}
		action := "model_capability_detection.cache_reused"
		if !replayed {
			action = "model_capability_detection.completed"
		}
		if err := r.appendAdminAuditWithMetadata("admin_user", admin.session.Username, action, "model_capability_detection", detection.ID, "success", "", detectionAuditMetadata(detection)); err != nil {
			adminAuditError(writer)
			return
		}
		writer.Header().Set("ETag", revisionETag(detection.Revision))
		writeJSON(writer, http.StatusOK, publicCapabilityDetection(detection))
		return
	}
	detectors := make(map[string]provider.CapabilityDetector, len(resolved))
	for _, item := range resolved {
		if item.adapter == nil || item.detector == nil {
			adminStoreError(writer)
			return
		}
		detectors[item.binding.ID] = item.detector
	}
	allowed, _ := allowAdminRate(&r.capabilityDetections.rateMu, &r.capabilityDetections.rate,
		admin.session.Username, r.now().UTC(), r.config.Admin.ModelCapabilityDetection.CreateRPM)
	if !allowed {
		_ = r.store.DeleteModelCapabilityDetection(request.Context(), detection.ID)
		writer.Header().Set("Retry-After", "60")
		writeJSON(writer, http.StatusTooManyRequests, map[string]string{"error": "capability detection creation rate exceeded", "code": "capability_detection_rate_limited"})
		return
	}
	if err := r.appendAdminAuditWithMetadata("admin_user", admin.session.Username, "model_capability_detection.started", "model_capability_detection", detection.ID, "success", "", detectionAuditMetadata(detection)); err != nil {
		_ = r.store.DeleteModelCapabilityDetection(request.Context(), detection.ID)
		adminAuditError(writer)
		return
	}
	r.capabilityMetrics.recordDetectionCache("miss")
	r.capabilityMetrics.recordDetectionStart(string(instance.Type))
	r.startCapabilityDetection(detection.ID, detectors)
	writer.Header().Set("ETag", revisionETag(detection.Revision))
	writeJSON(writer, http.StatusAccepted, publicCapabilityDetection(detection))
}

// detectionCandidate is one invocation interface the model might run on, paired
// with the detector that can ask it.
type detectionCandidate struct {
	binding  domain.ProviderProfileBinding
	adapter  provider.Adapter
	detector provider.CapabilityDetector
	entry    modelcatalog.Entry
	// verifiable is what this interface's plan can establish by probing, which
	// is usually narrower than what the interface offers.
	verifiable []string
}

// resolveCapabilityDetector answers which interface a detection runs on when
// that is knowable without spending anything: a catalog hit, or a single
// interface that can be probed at all. When several interfaces could serve the
// model it returns them all rather than refusing — identification asks the
// provider which one answers, because that is the same question the operator
// would otherwise have to guess at before any evidence exists.
func (r *Runtime) resolveCapabilityDetector(instance domain.ProviderInstance, input modelCapabilityDetectionInput, region string) (domain.ProviderProfileBinding, provider.Adapter, provider.CapabilityDetector, modelcatalog.Entry, bool, error) {
	catalogCandidates, detectionCandidates, err := r.capabilityDetectionCandidates(instance, input, region)
	if err != nil {
		return domain.ProviderProfileBinding{}, nil, nil, modelcatalog.Entry{}, false, err
	}
	if len(catalogCandidates) == 1 {
		match := catalogCandidates[0]
		return match.binding, match.adapter, nil, match.entry, true, nil
	}
	if len(catalogCandidates) > 1 {
		return domain.ProviderProfileBinding{}, nil, nil, modelcatalog.Entry{}, false, errAmbiguousCapabilityBinding
	}
	if len(detectionCandidates) >= 1 {
		match := detectionCandidates[0]
		return match.binding, match.adapter, match.detector, match.entry, false, nil
	}
	return domain.ProviderProfileBinding{}, nil, nil, modelcatalog.Entry{}, false, errNoDetectableCapabilityBinding
}

func (r *Runtime) capabilityDetectionCandidates(instance domain.ProviderInstance, input modelCapabilityDetectionInput, region string) ([]detectionCandidate, []detectionCandidate, error) {
	type candidate = detectionCandidate
	var catalogCandidates, detectionCandidates []candidate
	bindings := instance.EffectiveProfileBindings()
	slices.SortFunc(bindings, func(a, b domain.ProviderProfileBinding) int { return strings.Compare(a.ID, b.ID) })
	for _, binding := range bindings {
		if !binding.Enabled || input.BindingID != "" && input.BindingID != binding.ID || input.ProfileID != "" && input.ProfileID != binding.ProfileID {
			continue
		}
		adapter, ok := r.providers.AdapterForBinding(instance.ID, binding.ID)
		if !ok {
			continue
		}
		entry, found := r.effectiveModelCatalog().Lookup(modelcatalog.Key{ProviderType: instance.Type, Profile: binding.ProfileID, TargetKind: input.TargetKind, Model: input.ProviderModel, Region: region})
		if !found {
			entry = modelcatalog.Unknown(modelcatalog.Key{ProviderType: instance.Type, Profile: binding.ProfileID, TargetKind: input.TargetKind, Model: input.ProviderModel, Region: region})
		}
		if found && (entry.Source == modelcatalog.SourceBuiltin || entry.Source == modelcatalog.SourceSignedCatalog) {
			catalogCandidates = append(catalogCandidates, candidate{binding: binding, adapter: adapter, entry: entry})
			continue
		}
		detector, ok := capabilityDetectorFor(adapter)
		if !ok {
			continue
		}
		plan, err := detector.CapabilityDetectionPlan(provider.ModelCapabilityDetectionTarget{ProviderModel: input.ProviderModel, BindingID: binding.ID, ProfileID: binding.ProfileID, RiskTier: input.RiskTier})
		if err == nil {
			verifiable := make([]string, 0, len(plan.Probes))
			for _, probe := range plan.Probes {
				verifiable = append(verifiable, probe.Capability)
			}
			detectionCandidates = append(detectionCandidates, candidate{binding: binding, adapter: adapter, detector: detector, entry: entry, verifiable: verifiable})
		}
	}
	return catalogCandidates, detectionCandidates, nil
}

func capabilityDetectorFor(adapter provider.Adapter) (provider.CapabilityDetector, bool) {
	for adapter != nil {
		if detector, ok := adapter.(provider.CapabilityDetector); ok {
			return detector, true
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

func hashCanonical(value any) string {
	encoded, _ := json.Marshal(value)
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func (r *Runtime) startCapabilityDetection(id string, detectors map[string]provider.CapabilityDetector) {
	r.backgroundWait.Add(1)
	go func() { defer r.backgroundWait.Done(); r.runCapabilityDetection(r.backgroundCtx, id, detectors) }()
}

// capabilityCandidateError reports the cases identification cannot rescue. A
// model seeded into the catalog under two interfaces is a seeding conflict, not
// a question to put to the operator, so it stays a refusal.
func capabilityCandidateError(catalogCandidates, probeCandidates []detectionCandidate) error {
	switch {
	case len(catalogCandidates) > 1:
		return errAmbiguousCapabilityBinding
	case len(catalogCandidates) == 1 || len(probeCandidates) > 0:
		return nil
	default:
		return errNoDetectableCapabilityBinding
	}
}

// detectionTargetFingerprint binds a detection to the interface it resolved to.
// Deployment creation recomputes it from its own reading of the provider, so
// both sides must build it from exactly these fields.
func detectionTargetFingerprint(providerID string, providerRevision, credentialRevision uint64, credentialKeyVersion uint16,
	bindingID string, profileID domain.ProviderProfileID, surface domain.AccessSurface, model string,
	targetKind domain.DeploymentTargetKind, region, riskTier string) string {
	return hashCanonical(map[string]any{"provider_id": providerID, "provider_revision": providerRevision,
		"credential_revision": credentialRevision, "credential_key_version": credentialKeyVersion, "binding_id": bindingID,
		"profile_id": profileID, "access_surface": surface, "provider_model": model,
		"target_kind": targetKind, "canonical_target": model, "region": region,
		"detector_version": provider.CapabilityDetectorContractVersion, "risk_tier": riskTier})
}

func (r *Runtime) capabilityDetectionProviderSemaphore(providerID string) chan struct{} {
	r.capabilityDetections.mu.Lock()
	defer r.capabilityDetections.mu.Unlock()
	sem := r.capabilityDetections.providers[providerID]
	if sem == nil {
		sem = make(chan struct{}, r.config.Admin.ModelCapabilityDetection.ProviderConcurrency)
		r.capabilityDetections.providers[providerID] = sem
	}
	return sem
}

func (r *Runtime) runCapabilityDetection(parent context.Context, detectionID string, detectors map[string]provider.CapabilityDetector) {
	d, err := r.store.GetModelCapabilityDetection(parent, detectionID)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, r.config.Admin.ModelCapabilityDetection.TotalTimeout.Value())
	r.capabilityDetections.mu.Lock()
	r.capabilityDetections.cancels[detectionID] = cancel
	r.capabilityDetections.mu.Unlock()
	defer func() {
		cancel()
		r.capabilityDetections.mu.Lock()
		delete(r.capabilityDetections.cancels, detectionID)
		r.capabilityDetections.mu.Unlock()
	}()
	providerSem := r.capabilityDetectionProviderSemaphore(d.ProviderID)
	select {
	case r.capabilityDetections.global <- struct{}{}:
		defer func() { <-r.capabilityDetections.global }()
	case <-ctx.Done():
		r.finishDetectionWithoutProbe(d, domain.DetectionInterrupted)
		return
	}
	select {
	case providerSem <- struct{}{}:
		defer func() { <-providerSem }()
	case <-ctx.Done():
		r.finishDetectionWithoutProbe(d, domain.DetectionInterrupted)
		return
	}
	now := r.now().UTC()
	expected := d.Revision
	d.Status, d.StartedAt, d.UpdatedAt = domain.DetectionRunning, &now, now
	d, err = r.store.PutModelCapabilityDetection(ctx, d, expected)
	if err != nil {
		return
	}
	d, resolved := r.identifyCapabilityBinding(ctx, d, detectors)
	if !resolved {
		return
	}
	detector := detectors[d.BindingID]
	target := provider.ModelCapabilityDetectionTarget{ProviderModel: d.ProviderModel, BindingID: d.BindingID, ProfileID: d.ProfileID, RiskTier: d.RiskTier}
	plan, err := detector.CapabilityDetectionPlan(target)
	if err != nil {
		r.finishDetectionWithoutProbe(d, domain.DetectionFailed)
		return
	}
	// A capability the profile serves and the call budget could not fit is
	// recorded before any probe runs, under its own probe kind. It is not the
	// same thing as a capability the plan deliberately never reaches — that one
	// is a policy, this one is a ceiling — and writing it now means it survives
	// a detection that is cancelled or fails partway.
	deferred := false
	for _, name := range plan.Deferred {
		if _, carried := d.Results[name]; !carried {
			d.Results[name] = domain.CapabilityProbeResult{Status: domain.ProbeNotProbed, BindingID: d.BindingID, ProbeKind: "probe_budget"}
			deferred = true
		}
	}
	// Written before the first probe runs, because the probe loop opens by
	// re-reading the detection from the store and assigning over d — an
	// in-memory entry would be replaced by the stored map on the first
	// iteration and never seen again. finalizeCapabilityDetection then filled
	// the missing name in as "risk_policy", which the console filters out of
	// both the unestablished-capability banner and the failed-probe list, so a
	// capability the budget cut disappeared from the page entirely. It is the
	// one kind an operator can act on: raise the ceiling, or declare it by hand.
	if deferred {
		stored, err := r.store.PutModelCapabilityDetection(ctx, d, d.Revision)
		if err != nil {
			return
		}
		d = stored
	}
	for probeIndex, probe := range plan.Probes {
		current, getErr := r.store.GetModelCapabilityDetection(ctx, d.ID)
		if getErr != nil {
			return
		}
		d = current
		if d.CancelRequestedAt != nil || ctx.Err() != nil {
			r.finalizeCanceledDetection(d)
			return
		}
		// Identification already paid for this capability on this interface.
		if _, carried := d.Results[probe.Capability]; carried {
			continue
		}
		// A capability the budget cannot reach stays unverified rather than
		// being reported as unsupported, which would be a claim nothing checked.
		if !probeDependenciesSupported(d.Results, probe.DependsOn) || d.ProviderCalls >= d.MaxProviderCalls {
			d.Results[probe.Capability] = domain.CapabilityProbeResult{Status: domain.ProbeNotProbed, BindingID: d.BindingID, ProbeKind: probe.Kind}
			d.UpdatedAt = r.now().UTC()
			d, _ = r.store.PutModelCapabilityDetection(ctx, d, d.Revision)
			continue
		}
		callTime := r.now().UTC()
		d.ProviderCalls++
		d.Calls = append(d.Calls, domain.DetectionProviderCall{Sequence: d.ProviderCalls, BindingID: d.BindingID, Capability: probe.Capability, ProbeKind: probe.Kind, Status: "reserved"})
		d.Results[probe.Capability] = domain.CapabilityProbeResult{Status: domain.ProbeInconclusive, BindingID: d.BindingID, ProbeKind: probe.Kind, StartedAt: &callTime}
		d.UpdatedAt = callTime
		d, err = r.store.PutModelCapabilityDetection(ctx, d, d.Revision)
		if err != nil {
			return
		}
		// Persist the transition to in-flight before the provider call. Startup
		// recovery treats either reserved or running as UNKNOWN and never replays
		// it, so a crash at either side of the network boundary cannot defeat the
		// call budget or pretend that a possibly billable request did not happen.
		d.Calls[len(d.Calls)-1].Status = "running"
		d.Calls[len(d.Calls)-1].StartedAt = &callTime
		d, err = r.store.PutModelCapabilityDetection(ctx, d, d.Revision)
		if err != nil {
			return
		}
		probeContext := ctx
		probeCancel := func() {}
		if deadline, ok := ctx.Deadline(); ok {
			// Share what is left among the probes that can actually run next,
			// not every probe the plan lists. In these plans everything is
			// gated on chat, so chat failing skips the rest without spending a
			// call — and dividing evenly gave the one probe that always runs,
			// and that all the others wait on, the smallest share: a seventh of
			// the budget on a plan of seven. A frontier reasoning model cannot
			// answer a non-streaming completion in that, so detection reported
			// a timeout for a model that works. A probe whose dependencies are
			// established is bounded by the attempt timeout instead, and one
			// still waiting on an unresolved dependency is not counted, because
			// it may never be asked at all.
			runnable := 0
			for _, pending := range plan.Probes[probeIndex:] {
				if probeDependenciesSupported(d.Results, pending.DependsOn) {
					runnable++
				}
			}
			fairShare := time.Until(deadline) / time.Duration(max(runnable, 1))
			probeTimeout := min(fairShare, r.config.Gateway.AttemptResponseHeaderTimeout.Value())
			if probeTimeout > 0 {
				probeContext, probeCancel = context.WithTimeout(ctx, probeTimeout)
			}
		}
		result := detector.DetectCapability(probeContext, target, probe)
		probeCancel()
		finished := r.now().UTC()
		current, getErr = r.store.GetModelCapabilityDetection(context.Background(), d.ID)
		if getErr != nil {
			return
		}
		d = current
		if len(d.Calls) > 0 {
			d.Calls[len(d.Calls)-1].FinishedAt = &finished
			if ctx.Err() != nil {
				d.Calls[len(d.Calls)-1].Status = "unknown"
			} else {
				d.Calls[len(d.Calls)-1].Status = "completed"
			}
		}
		if d.CancelRequestedAt != nil {
			r.finalizeCanceledDetection(d)
			return
		}
		result.StartedAt, result.CompletedAt = &callTime, &finished
		d.Results[probe.Capability] = result
		d.UpdatedAt = finished
		d, err = r.store.PutModelCapabilityDetection(context.Background(), d, d.Revision)
		if err != nil {
			return
		}
	}
	r.finalizeCapabilityDetection(d)
}

// identifyCapabilityBinding asks the provider which invocation interface the
// model actually answers on, instead of asking the operator to know it before
// anything has been verified. Each candidate is probed with the roots of its own
// plan — the probes that depend on nothing — because those are exactly the ones
// that establish whether the model exists on that Access Surface. A candidate
// the model does not serve fails its first root and costs one call that the
// provider rejects before doing any work.
//
// It returns the detection with a binding resolved, or writes a terminal state
// and reports false: nothing answered (failed), or several answered, which is a
// real choice and belongs to the operator — now with evidence attached.
func (r *Runtime) identifyCapabilityBinding(ctx context.Context, d domain.ModelCapabilityDetection,
	detectors map[string]provider.CapabilityDetector) (domain.ModelCapabilityDetection, bool) {
	carried := map[string]map[string]domain.CapabilityProbeResult{}
	for index := range d.Candidates {
		candidate := d.Candidates[index]
		detector := detectors[candidate.BindingID]
		if detector == nil {
			continue
		}
		target := provider.ModelCapabilityDetectionTarget{ProviderModel: d.ProviderModel, BindingID: candidate.BindingID,
			ProfileID: candidate.ProfileID, RiskTier: d.RiskTier}
		plan, err := detector.CapabilityDetectionPlan(target)
		if err != nil {
			continue
		}
		results := map[string]domain.CapabilityProbeResult{}
		for _, probe := range plan.Probes {
			if len(probe.DependsOn) > 0 {
				continue
			}
			// A single candidate has nothing to distinguish itself from, so it
			// is resolved without spending anything on identification.
			if len(d.Candidates) == 1 {
				break
			}
			if d.ProviderCalls >= d.MaxProviderCalls {
				break
			}
			updated, result, ok := r.spendDetectionProbe(ctx, d, candidate.BindingID, detector, target, probe, plan.Probes)
			d = updated
			if !ok {
				return d, false
			}
			results[probe.Capability] = result
			// Keep the outcome that tells the operator the most, not the last
			// one attempted. A credential rejected with 401 is a fact they can
			// act on; a later "could not tell" would otherwise bury it.
			if probeOutcomeRank(result.Status) > probeOutcomeRank(candidate.Status) {
				candidate.Capability, candidate.ProbeKind = probe.Capability, probe.Kind
				candidate.Status, candidate.Evidence, candidate.ErrorClass = result.Status, result.Evidence, result.ErrorClass
			}
			candidate.Answered = candidate.Status == domain.ProbeSupported
			if candidate.Answered {
				break
			}
		}
		carried[candidate.BindingID] = results
		d.Candidates[index] = candidate
		d.UpdatedAt = r.now().UTC()
		stored, err := r.store.PutModelCapabilityDetection(ctx, d, d.Revision)
		if err != nil {
			return d, false
		}
		d = stored
	}
	var answered []domain.DetectionBindingCandidate
	for _, candidate := range d.Candidates {
		if candidate.Answered {
			answered = append(answered, candidate)
		}
	}
	if len(d.Candidates) == 1 {
		answered = d.Candidates
	}
	switch len(answered) {
	case 0:
		r.finishDetectionWithoutProbe(d, domain.DetectionFailed)
		return d, false
	case 1:
	default:
		r.finishAmbiguousDetection(d)
		return d, false
	}
	return r.resolveDetectionBinding(ctx, d, answered[0], carried[answered[0].BindingID])
}

// resolveDetectionBinding writes the identified interface onto the detection,
// including the fingerprint deployment creation checks, and carries the
// identification probes into the results so the capability pass never pays for
// the same call twice.
func (r *Runtime) resolveDetectionBinding(ctx context.Context, d domain.ModelCapabilityDetection,
	candidate domain.DetectionBindingCandidate, carried map[string]domain.CapabilityProbeResult) (domain.ModelCapabilityDetection, bool) {
	instance, err := r.store.GetProvider(ctx, d.ProviderID)
	if err != nil {
		return d, false
	}
	binding, bound := instance.ProfileBinding(candidate.BindingID)
	if !bound || !binding.Enabled || binding.ProfileID != candidate.ProfileID || binding.AccessSurface != candidate.AccessSurface {
		r.finishDetectionWithoutProbe(d, domain.DetectionFailed)
		return d, false
	}
	d.BindingID, d.ProfileID, d.AccessSurface = binding.ID, binding.ProfileID, binding.AccessSurface
	d.ModelRevision = candidate.ModelRevision
	d.TargetFingerprint = detectionTargetFingerprint(d.ProviderID, d.ProviderRevision, d.CredentialRevision, d.CredentialKeyVersion,
		binding.ID, binding.ProfileID, binding.AccessSurface, d.ProviderModel, d.TargetKind, d.Region, d.RiskTier)
	d.Recommended.MaxContextTokens, d.Recommended.MaxOutputTokens = binding.Capabilities.MaxContextTokens, binding.Capabilities.MaxOutputTokens
	for capability, result := range carried {
		d.Results[capability] = result
	}
	d.UpdatedAt = r.now().UTC()
	stored, err := r.store.PutModelCapabilityDetection(ctx, d, d.Revision)
	if err != nil {
		return d, false
	}
	return stored, true
}

// spendDetectionProbe performs one durable, possibly billable control-plane
// call: reserved and then running are persisted before the request leaves the
// process, so a crash on either side of the network boundary is recovered as
// UNKNOWN rather than replayed.
func (r *Runtime) spendDetectionProbe(ctx context.Context, d domain.ModelCapabilityDetection, bindingID string,
	detector provider.CapabilityDetector, target provider.ModelCapabilityDetectionTarget, probe provider.CapabilityProbe,
	remaining []provider.CapabilityProbe) (domain.ModelCapabilityDetection, domain.CapabilityProbeResult, bool) {
	if d.CancelRequestedAt != nil || ctx.Err() != nil {
		r.finalizeCanceledDetection(d)
		return d, domain.CapabilityProbeResult{}, false
	}
	callTime := r.now().UTC()
	d.ProviderCalls++
	d.Calls = append(d.Calls, domain.DetectionProviderCall{Sequence: d.ProviderCalls, BindingID: bindingID,
		Capability: probe.Capability, ProbeKind: probe.Kind, Status: "reserved"})
	d.UpdatedAt = callTime
	stored, err := r.store.PutModelCapabilityDetection(ctx, d, d.Revision)
	if err != nil {
		return d, domain.CapabilityProbeResult{}, false
	}
	d = stored
	d.Calls[len(d.Calls)-1].Status = "running"
	d.Calls[len(d.Calls)-1].StartedAt = &callTime
	stored, err = r.store.PutModelCapabilityDetection(ctx, d, d.Revision)
	if err != nil {
		return d, domain.CapabilityProbeResult{}, false
	}
	d = stored
	probeContext, probeCancel := ctx, func() {}
	if deadline, ok := ctx.Deadline(); ok && len(remaining) > 0 {
		fairShare := time.Until(deadline) / time.Duration(len(remaining))
		if probeTimeout := min(fairShare, r.config.Gateway.AttemptResponseHeaderTimeout.Value()); probeTimeout > 0 {
			probeContext, probeCancel = context.WithTimeout(ctx, probeTimeout)
		}
	}
	result := detector.DetectCapability(probeContext, target, probe)
	probeCancel()
	finished := r.now().UTC()
	current, err := r.store.GetModelCapabilityDetection(context.Background(), d.ID)
	if err != nil {
		return d, domain.CapabilityProbeResult{}, false
	}
	d = current
	if len(d.Calls) > 0 {
		d.Calls[len(d.Calls)-1].FinishedAt = &finished
		d.Calls[len(d.Calls)-1].Status = "completed"
		if ctx.Err() != nil {
			d.Calls[len(d.Calls)-1].Status = "unknown"
		}
	}
	if d.CancelRequestedAt != nil {
		r.finalizeCanceledDetection(d)
		return d, domain.CapabilityProbeResult{}, false
	}
	result.StartedAt, result.CompletedAt = &callTime, &finished
	return d, result, true
}

// probeOutcomeRank orders probe outcomes by how much they tell the operator.
// A supported probe settles the interface. A rejected credential or an
// unreachable provider names something they can fix. An upstream that answered
// without showing the capability at least proves the interface answers, which
// is more than "could not tell" — and "was not asked" names nothing at all, so
// neither of those two ever displaces something an operator can act on.
//
// The numbers are compared against each other and nowhere else; they are spaced
// only so a new outcome can be placed without renumbering its neighbours.
func probeOutcomeRank(status domain.CapabilityProbeStatus) int {
	switch status {
	case domain.ProbeSupported:
		return 6
	case domain.ProbeUnauthorized:
		return 5
	case domain.ProbeUnavailable:
		return 4
	case domain.ProbeUnsupported:
		return 3
	case domain.ProbeAssertionFailed:
		return 2
	case domain.ProbeInconclusive:
		return 1
	default:
		return 0
	}
}

func probeDependenciesSupported(results map[string]domain.CapabilityProbeResult, dependencies []string) bool {
	for _, dependency := range dependencies {
		if results[dependency].Status != domain.ProbeSupported {
			return false
		}
	}
	return true
}

func (r *Runtime) finalizeCapabilityDetection(d domain.ModelCapabilityDetection) {
	now := r.now().UTC()
	for _, name := range modelcatalog.CapabilityNames {
		if name == "max_context_tokens" || name == "max_output_tokens" {
			continue
		}
		if _, ok := d.Results[name]; !ok {
			d.Results[name] = domain.CapabilityProbeResult{Status: domain.ProbeNotProbed, BindingID: d.BindingID, ProbeKind: "risk_policy"}
		}
	}
	recommended := capabilitiesFromProbeResults(d.Results)
	recommended.MaxContextTokens, recommended.MaxOutputTokens = d.Recommended.MaxContextTokens, d.Recommended.MaxOutputTokens
	d.Recommended = recommended
	if d.Recommended.AnyOperation() {
		expires := now.Add(r.config.Admin.ModelCapabilityDetection.FreshTTL.Value())
		d.Status, d.ExpiresAt = domain.DetectionCompleted, &expires
	} else {
		d.Status = domain.DetectionFailed
	}
	d.CompletedAt, d.UpdatedAt = &now, now
	stored, err := r.store.PutModelCapabilityDetection(context.Background(), d, d.Revision)
	if err != nil {
		return
	}
	r.recordCapabilityDetectionTerminal(stored)
	action := "model_capability_detection.completed"
	if stored.Status == domain.DetectionFailed {
		action = "model_capability_detection.failed"
	}
	_ = r.appendAdminAuditWithMetadata("admin_user", stored.CreatedBy, action, "model_capability_detection", stored.ID, "success", "", detectionAuditMetadata(stored))
}

func (r *Runtime) finalizeCanceledDetection(d domain.ModelCapabilityDetection) {
	now := r.now().UTC()
	d.Status, d.CompletedAt, d.UpdatedAt = domain.DetectionCanceled, &now, now
	for capability, result := range d.Results {
		if result.CompletedAt == nil {
			result.Status, result.CompletedAt = domain.ProbeCanceled, &now
			d.Results[capability] = result
		}
	}
	if stored, err := r.store.PutModelCapabilityDetection(context.Background(), d, d.Revision); err == nil {
		r.recordCapabilityDetectionTerminal(stored)
	}
}

func (r *Runtime) finishDetectionWithoutProbe(d domain.ModelCapabilityDetection, status domain.ModelCapabilityDetectionStatus) {
	now := r.now().UTC()
	current, err := r.store.GetModelCapabilityDetection(context.Background(), d.ID)
	if err == nil {
		d = current
	}
	d.Status, d.CompletedAt, d.UpdatedAt = status, &now, now
	if stored, err := r.store.PutModelCapabilityDetection(context.Background(), d, d.Revision); err == nil {
		r.recordCapabilityDetectionTerminal(stored)
		_ = r.appendAdminAuditWithMetadata("admin_user", stored.CreatedBy, "model_capability_detection.failed", "model_capability_detection", stored.ID, "success", "", detectionAuditMetadata(stored))
	}
}

// finishAmbiguousDetection ends a detection that found the model answering on
// more than one interface. A deployment runs on exactly one, so the choice is
// the operator's — but it is now made against what each interface answered
// rather than asked before anything was known.
func (r *Runtime) finishAmbiguousDetection(d domain.ModelCapabilityDetection) {
	now := r.now().UTC()
	current, err := r.store.GetModelCapabilityDetection(context.Background(), d.ID)
	if err == nil {
		d = current
	}
	d.Status, d.CompletedAt, d.UpdatedAt = domain.DetectionAmbiguous, &now, now
	if stored, err := r.store.PutModelCapabilityDetection(context.Background(), d, d.Revision); err == nil {
		r.recordCapabilityDetectionTerminal(stored)
		_ = r.appendAdminAuditWithMetadata("admin_user", stored.CreatedBy, "model_capability_detection.ambiguous",
			"model_capability_detection", stored.ID, "success", "", detectionAuditMetadata(stored))
	}
}

func (r *Runtime) recordCapabilityDetectionTerminal(d domain.ModelCapabilityDetection) {
	instance, err := r.store.GetProvider(context.Background(), d.ProviderID)
	if err != nil {
		return
	}
	started := d.CreatedAt
	if d.StartedAt != nil {
		started = *d.StartedAt
	}
	r.capabilityMetrics.recordDetectionFinish(string(instance.Type), string(d.Status), d.Source, d.Results, d.ProviderCalls, d.UpdatedAt.Sub(started))
}

func capabilitiesFromProbeResults(results map[string]domain.CapabilityProbeResult) domain.ProviderCapabilities {
	c := domain.ProviderCapabilities{}
	enabled := func(name string) bool { return results[name].Status == domain.ProbeSupported }
	c.Chat, c.Streaming, c.Embeddings, c.Moderations = enabled("chat"), enabled("streaming"), enabled("embeddings"), enabled("moderations")
	c.Images, c.Transcriptions, c.Speech, c.Files = enabled("images"), enabled("transcriptions"), enabled("speech"), enabled("files")
	c.Batches, c.Rerank, c.AsyncGenerate, c.Tools = enabled("batches"), enabled("rerank"), enabled("async_generate"), enabled("tools")
	c.Vision, c.JSONMode, c.DeveloperRole, c.Reasoning, c.StreamUsage = enabled("vision"), enabled("json_mode"), enabled("developer_role"), enabled("reasoning"), enabled("stream_usage")
	return c
}

func detectionAuditMetadata(d domain.ModelCapabilityDetection) map[string]any {
	counts := map[string]int{}
	for _, result := range d.Results {
		counts[string(result.Status)]++
	}
	answered := 0
	for _, candidate := range d.Candidates {
		if candidate.Answered {
			answered++
		}
	}
	return map[string]any{"provider_id": d.ProviderID, "binding_id": d.BindingID, "binding_candidates": len(d.Candidates),
		"answering_bindings": answered, "model_sha256": hashCanonical(d.ProviderModel), "status": d.Status,
		"status_counts": counts, "provider_calls": d.ProviderCalls, "risk_tier": d.RiskTier, "revision": d.Revision}
}

func publicCapabilityDetection(d domain.ModelCapabilityDetection) map[string]any {
	encoded, _ := json.Marshal(d)
	view := map[string]any{}
	_ = json.Unmarshal(encoded, &view)
	delete(view, "target_fingerprint")
	delete(view, "credential_revision")
	delete(view, "credential_key_version")
	delete(view, "created_by")
	delete(view, "idempotency_key_hash")
	delete(view, "request_hash")
	delete(view, "provider_call_records")
	delete(view, "expiry_recorded_at")
	return view
}

func (r *Runtime) runCapabilityDetectionMaintenance(ctx context.Context) {
	r.maintainCapabilityDetections(ctx)
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.maintainCapabilityDetections(ctx)
		}
	}
}

func (r *Runtime) maintainCapabilityDetections(ctx context.Context) {
	items, err := r.store.ListModelCapabilityDetections(ctx)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	retention := r.config.Admin.ModelCapabilityDetection.Retention.Value()
	for _, d := range items {
		if d.Status == domain.DetectionCompleted && d.ExpiresAt != nil && !now.Before(*d.ExpiresAt) && d.ExpiryRecordedAt == nil {
			if r.appendAdminAuditWithMetadata("system", "halro", "model_capability_detection.expired", "model_capability_detection", d.ID, "success", "", detectionAuditMetadata(d)) == nil {
				d.ExpiryRecordedAt, d.UpdatedAt = &now, now
				_, _ = r.store.PutModelCapabilityDetection(ctx, d, d.Revision)
			}
		}
		if d.Status.Terminal() && now.Sub(d.CreatedAt) >= retention {
			_ = r.store.DeleteModelCapabilityDetection(ctx, d.ID)
		}
	}
}

func (r *Runtime) getAdminModelCapabilityDetection(writer http.ResponseWriter, request *http.Request) {
	d, err := r.store.GetModelCapabilityDetection(request.Context(), chi.URLParam(request, "id"))
	if err != nil {
		adminStoreError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(d.Revision))
	writeJSON(writer, http.StatusOK, publicCapabilityDetection(d))
}

func (r *Runtime) cancelAdminModelCapabilityDetection(writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	d, err := r.store.GetModelCapabilityDetection(request.Context(), chi.URLParam(request, "id"))
	if err != nil {
		adminStoreError(writer)
		return
	}
	if d.Revision != expected {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "capability detection changed", "code": "capability_detection_changed"})
		return
	}
	if d.Status.Terminal() {
		writer.Header().Set("ETag", revisionETag(d.Revision))
		writeJSON(writer, http.StatusOK, publicCapabilityDetection(d))
		return
	}
	now := r.now().UTC()
	d.CancelRequestedAt, d.UpdatedAt = &now, now
	d, err = r.store.PutModelCapabilityDetection(request.Context(), d, expected)
	if err != nil {
		adminStoreError(writer)
		return
	}
	r.capabilityDetections.mu.Lock()
	cancel := r.capabilityDetections.cancels[d.ID]
	r.capabilityDetections.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	_ = r.appendAdminAuditWithMetadata("admin_user", admin.session.Username, "model_capability_detection.cancel_requested", "model_capability_detection", d.ID, "success", "", detectionAuditMetadata(d))
	writer.Header().Set("ETag", revisionETag(d.Revision))
	writeJSON(writer, http.StatusOK, publicCapabilityDetection(d))
}
