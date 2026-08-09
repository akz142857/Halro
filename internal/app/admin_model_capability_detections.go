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

func (r *Runtime) createAdminModelCapabilityDetection(writer http.ResponseWriter, request *http.Request) {
	key := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 256 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "Idempotency-Key is required", "code": "idempotency_key_required"})
		return
	}
	var input modelCapabilityDetectionInput
	if err := decodeAdminJSON(request, &input); err != nil {
		adminBadRequest(writer, "invalid request")
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
	region := input.Region
	if region == "" {
		region = providerRegion(instance)
	}
	binding, adapter, detector, entry, catalogKnown, err := r.resolveCapabilityDetector(instance, input, region)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "no_detectable_binding"})
		return
	}
	now := r.now().UTC()
	requestShape := map[string]any{"provider_id": providerID, "provider_revision": instance.Revision, "credential_revision": credential.Revision,
		"credential_key_version": credential.KeyVersion, "provider_model": input.ProviderModel, "target_kind": targetKind,
		"region": region, "binding_id": binding.ID, "profile_id": binding.ProfileID, "risk_tier": input.RiskTier,
		"selection_revision": input.SelectionRevision, "force_refresh": input.ForceRefresh}
	requestHash := hashCanonical(requestShape)
	fingerprint := hashCanonical(map[string]any{"provider_id": providerID, "provider_revision": instance.Revision,
		"credential_revision": credential.Revision, "credential_key_version": credential.KeyVersion, "binding_id": binding.ID,
		"profile_id": binding.ProfileID, "access_surface": binding.AccessSurface, "provider_model": input.ProviderModel,
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
			if item.TargetFingerprint == fingerprint && now.Sub(item.UpdatedAt) < cooldown {
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
		ModelRevision: entry.Revision(), BindingID: binding.ID, ProfileID: binding.ProfileID, AccessSurface: binding.AccessSurface,
		TargetKind: targetKind, CanonicalTarget: input.ProviderModel, Region: region, TargetFingerprint: fingerprint,
		DetectorVersion: provider.CapabilityDetectorContractVersion, RiskTier: input.RiskTier, Status: domain.DetectionQueued,
		Source: string(modelcatalog.SourceVerifiedProbe), Results: map[string]domain.CapabilityProbeResult{},
		Recommended:      domain.ProviderCapabilities{MaxContextTokens: binding.Capabilities.MaxContextTokens, MaxOutputTokens: binding.Capabilities.MaxOutputTokens},
		MaxProviderCalls: r.config.Admin.ModelCapabilityDetection.MaxProviderCalls, CreatedBy: admin.session.Username,
		IdempotencyKeyHash: hashCanonical(map[string]any{"actor": admin.session.Username, "key": key}), RequestHash: requestHash,
		SelectionRevision: input.SelectionRevision, ForceRefresh: input.ForceRefresh, CreatedAt: now, UpdatedAt: now}
	if catalogKnown {
		detection.Status, detection.Source = domain.DetectionCompleted, string(modelcatalog.SourceBuiltin)
		detection.Recommended = modelcatalog.Clamp(entry.Capabilities, binding.Capabilities)
		detection.MaxProviderCalls, detection.CompletedAt = 0, &now
	}
	detection, replayed, err := r.store.CreateModelCapabilityDetection(request.Context(), detection)
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
	if adapter == nil || detector == nil {
		adminStoreError(writer)
		return
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
	r.startCapabilityDetection(detection.ID, detector)
	writer.Header().Set("ETag", revisionETag(detection.Revision))
	writeJSON(writer, http.StatusAccepted, publicCapabilityDetection(detection))
}

func (r *Runtime) resolveCapabilityDetector(instance domain.ProviderInstance, input modelCapabilityDetectionInput, region string) (domain.ProviderProfileBinding, provider.Adapter, provider.CapabilityDetector, modelcatalog.Entry, bool, error) {
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
		entry, found := modelcatalog.Builtin().Lookup(modelcatalog.Key{ProviderType: instance.Type, Profile: binding.ProfileID, Model: input.ProviderModel, Region: region})
		if !found {
			entry = modelcatalog.Unknown(modelcatalog.Key{ProviderType: instance.Type, Profile: binding.ProfileID, Model: input.ProviderModel, Region: region})
		}
		if found && entry.Status == modelcatalog.StatusKnown {
			return binding, adapter, nil, entry, true, nil
		}
		detector, ok := capabilityDetectorFor(adapter)
		if !ok {
			continue
		}
		if _, err := detector.CapabilityDetectionPlan(provider.ModelCapabilityDetectionTarget{ProviderModel: input.ProviderModel, BindingID: binding.ID, ProfileID: binding.ProfileID, RiskTier: input.RiskTier}); err == nil {
			return binding, adapter, detector, entry, false, nil
		}
	}
	return domain.ProviderProfileBinding{}, nil, nil, modelcatalog.Entry{}, false, errors.New("current capability interface does not support automatic detection")
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

func (r *Runtime) startCapabilityDetection(id string, detector provider.CapabilityDetector) {
	r.backgroundWait.Add(1)
	go func() { defer r.backgroundWait.Done(); r.runCapabilityDetection(r.backgroundCtx, id, detector) }()
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

func (r *Runtime) runCapabilityDetection(parent context.Context, detectionID string, detector provider.CapabilityDetector) {
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
	target := provider.ModelCapabilityDetectionTarget{ProviderModel: d.ProviderModel, BindingID: d.BindingID, ProfileID: d.ProfileID, RiskTier: d.RiskTier}
	plan, err := detector.CapabilityDetectionPlan(target)
	if err != nil || plan.MaxCalls > d.MaxProviderCalls || len(plan.Probes) > d.MaxProviderCalls {
		r.finishDetectionWithoutProbe(d, domain.DetectionFailed)
		return
	}
	now := r.now().UTC()
	expected := d.Revision
	d.Status, d.StartedAt, d.UpdatedAt = domain.DetectionRunning, &now, now
	d, err = r.store.PutModelCapabilityDetection(ctx, d, expected)
	if err != nil {
		return
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
		if !probeDependenciesSupported(d.Results, probe.DependsOn) {
			d.Results[probe.Capability] = domain.CapabilityProbeResult{Status: domain.ProbeNotProbed, BindingID: d.BindingID, ProbeKind: probe.Kind}
			d.UpdatedAt = r.now().UTC()
			d, _ = r.store.PutModelCapabilityDetection(ctx, d, d.Revision)
			continue
		}
		callTime := r.now().UTC()
		d.ProviderCalls++
		d.Calls = append(d.Calls, domain.DetectionProviderCall{Sequence: d.ProviderCalls, Capability: probe.Capability, ProbeKind: probe.Kind, Status: "reserved"})
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
			remainingProbes := len(plan.Probes) - probeIndex
			fairShare := time.Until(deadline) / time.Duration(remainingProbes)
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
	return map[string]any{"provider_id": d.ProviderID, "binding_id": d.BindingID, "model_sha256": hashCanonical(d.ProviderModel), "status": d.Status, "status_counts": counts, "provider_calls": d.ProviderCalls, "risk_tier": d.RiskTier, "revision": d.Revision}
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
