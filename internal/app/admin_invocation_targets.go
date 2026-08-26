package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/modelcatalog"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/go-chi/chi/v5"
)

const (
	invocationTargetCatalogTTL       = 5 * time.Minute
	invocationTargetFetchConcurrency = 4
)

type invocationTargetCatalogCache struct {
	Items              []domain.InvocationTargetDescriptor
	FetchedAt          time.Time
	ExpiresAt          time.Time
	ProviderRevision   uint64
	CredentialRevision uint64
	FetchSequence      uint64
}

var invocationTargetFetchSequence atomic.Uint64

type adminInvocationTarget struct {
	domain.InvocationTargetDescriptor
	Variants            []domain.DeploymentVariant `json:"variants"`
	ConflictingBindings []string                   `json:"conflicting_bindings,omitempty"`
	ResolutionState     domain.ResolutionState     `json:"resolution_state"`
	// CoveredByProfiles names the interfaces the catalog lists this model under
	// when this provider has bound none of them. Present only with
	// ResolutionCoveredElsewhere, and the reason that state is worth having:
	// without the list the console can say "not here" but not "there".
	CoveredByProfiles  []domain.ProviderProfileID `json:"covered_by_profiles,omitempty"`
	ResolutionRevision string                     `json:"resolution_revision"`
}

type degradedBinding struct {
	BindingID  string                   `json:"binding_id"`
	ProfileID  domain.ProviderProfileID `json:"profile_id"`
	ErrorClass provider.ErrorClass      `json:"error_class"`
}

type invocationTargetCatalogResponse struct {
	Items            []adminInvocationTarget                      `json:"items"`
	CanonicalModels  []adminInvocationTarget                      `json:"canonical_models,omitempty"`
	Discovery        domain.InvocationTargetDiscoveryCapabilities `json:"discovery"`
	CatalogRevision  string                                       `json:"catalog_revision"`
	ProviderRevision uint64                                       `json:"provider_revision"`
	DegradedBindings []degradedBinding                            `json:"degraded_bindings,omitempty"`
	// NotCached says the catalogue was never fetched, or the copy expired —
	// not that fetching it failed. The console reads it as "press refresh",
	// which is the control that is allowed to spend the credential.
	NotCached bool      `json:"not_cached,omitempty"`
	FetchedAt time.Time `json:"fetched_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Cached    bool      `json:"cached"`
}

type bindingTargetCatalog struct {
	binding   domain.ProviderProfileBinding
	items     []domain.InvocationTargetDescriptor
	fetchedAt time.Time
	expiresAt time.Time
	cached    bool
	// notCached is a read that stopped rather than a binding that failed: the
	// cache had nothing live and the read is not allowed to dial. It is not an
	// error class, because no provider was reached to produce one.
	notCached          bool
	failed             bool
	errorClass         provider.ErrorClass
	discovery          domain.InvocationTargetDiscoveryCapabilities
	mapper             provider.ProviderMetadataMapper
	credentialRevision uint64
}

// Reading the cached catalog is a read: any authenticated role may ask for it,
// and it never reaches a Provider — a miss answers not_cached rather than
// dialling. That was once only a comment: the miss fell through to the
// upstream, so an endpoint guarded by requireAdmin, which checks a session and
// not a role, spent the operator's provider credential once per binding
// whenever a read-only admin opened the create form. Spending it is the POST
// next door, behind requireAdminMutation, where the console says so first.
func (r *Runtime) listAdminInvocationTargets(writer http.ResponseWriter, request *http.Request) {
	r.writeInvocationTargetCatalog(writer, request, catalogCachedOnly)
}

// Bypassing the cache is not a read: it spends the operator's provider
// credential on an upstream call, against their quota and their bill, as often
// as it is asked for. So it is a POST behind requireAdminMutation — role, CSRF
// token and Origin together — rather than a GET carrying refresh=true.
//
// It must not be a GET with an Origin check bolted on: browsers do not send
// Origin on a same-origin GET, and this router sets Referrer-Policy:
// no-referrer, so such a check rejects the console itself while leaving a
// cross-site attacker free to use a form-shaped POST. The method is the thing
// that carries the CSRF defence here.
func (r *Runtime) refreshAdminInvocationTargets(writer http.ResponseWriter, request *http.Request) {
	r.writeInvocationTargetCatalog(writer, request, catalogAlwaysDial)
}

func (r *Runtime) writeInvocationTargetCatalog(writer http.ResponseWriter, request *http.Request, listAccess catalogAccess) {
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
	bindings, err := invocationTargetBindings(instance, strings.TrimSpace(request.URL.Query().Get("binding_id")))
	if err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	results := r.fetchInvocationTargetCatalogs(request.Context(), instance, bindings, listAccess)
	catalog := r.effectiveModelCatalog()
	evaluatedAt := r.clockNow().UTC()
	response := aggregateInvocationTargetsWithCatalog(instance, results, evaluatedAt, catalog)
	// The console names this ceiling before the operator presses a billable
	// button, so it has to be the configured value, not a bundled constant.
	response.Discovery.MaxVerificationCalls = r.config.Admin.ModelCapabilityDetection.MaxProviderCalls
	credentialRevision, err := r.providerCredentialRevision(request.Context(), instance)
	if err == nil {
		response.CanonicalModels = canonicalModelTemplatesWithCatalog(instance, bindings, credentialRevision, evaluatedAt, catalog)
	} else {
		response.CanonicalModels = make([]adminInvocationTarget, 0)
	}
	for _, item := range response.Items {
		r.capabilityMetrics.recordResolution(instance.Type, item.TargetKind, item.ResolutionState, resolutionSource(item))
	}
	if response.FetchedAt.IsZero() {
		response.FetchedAt = evaluatedAt
		response.ExpiresAt = response.FetchedAt.Add(invocationTargetCatalogTTL)
	}
	writeJSON(writer, http.StatusOK, response)
}

func (r *Runtime) resolveAdminInvocationTarget(writer http.ResponseWriter, request *http.Request) {
	// Resolution may enumerate a cold catalog and may call the Provider's
	// Describe interface when the target is absent, so the whole endpoint is
	// treated as credential-spending rather than trying to predict which cache
	// branch will run. It is registered as a POST behind requireAdminMutation
	// for the reason spelled out on refreshAdminInvocationTargets.
	providerID := chi.URLParam(request, "id")
	instance, err := r.store.GetProvider(request.Context(), providerID)
	if err != nil || instance.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	raw := strings.TrimSuffix(chi.URLParam(request, "*"), "/resolution")
	targetID, err := url.PathUnescape(strings.Trim(raw, "/"))
	if err != nil || strings.TrimSpace(targetID) == "" || len(targetID) > 2048 {
		adminBadRequest(writer, "invocation target is invalid")
		return
	}
	bindings, err := invocationTargetBindings(instance, strings.TrimSpace(request.URL.Query().Get("binding_id")))
	if err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	targetKind := domain.DeploymentTargetKind(strings.TrimSpace(request.URL.Query().Get("target_kind")))
	canonicalRef := strings.TrimSpace(request.URL.Query().Get("canonical_model_ref"))
	results := r.fetchInvocationTargetCatalogs(request.Context(), instance, bindings, catalogDialOnMiss)
	evaluatedAt := r.clockNow().UTC()
	if canonicalRef != "" {
		for resultIndex := range results {
			for itemIndex := range results[resultIndex].items {
				item := &results[resultIndex].items[itemIndex]
				if item.TargetID == targetID && (targetKind == "" || item.TargetKind == targetKind) {
					item.CanonicalModelRef = canonicalRef
				}
			}
		}
	}
	var resolved adminInvocationTarget
	matched := false
	for _, item := range aggregateInvocationTargetsWithCatalog(instance, results, evaluatedAt, r.effectiveModelCatalog()).Items {
		if item.TargetID == targetID && (targetKind == "" || item.TargetKind == targetKind) {
			resolved, matched = item, true
			break
		}
	}
	if !matched && targetKind == "" {
		targetKind, err = deploymentTargetKind(instance.Type, instance.AccessSurface, instance.ProfileID, "")
		if err != nil {
			adminBadRequest(writer, err.Error())
			return
		}
	}
	target := domain.InvocationTargetDescriptor{
		TargetID: targetID, TargetKind: targetKind, DisplayName: targetID, CanonicalModelRef: canonicalRef,
		Region: strings.TrimSpace(request.URL.Query().Get("region")), Lifecycle: domain.TargetLifecycleUnknown,
		MetadataSource: domain.MetadataSourceNone, Availability: domain.AvailabilityUnverified, FetchedAt: evaluatedAt,
	}
	if !matched {
		bindingTargets := make(map[string]domain.InvocationTargetDescriptor)
		mappers := make(map[string]provider.ProviderMetadataMapper)
		for _, binding := range bindings {
			adapter, ok := r.providers.AdapterForBinding(instance.ID, binding.ID)
			if !ok && len(instance.Bindings) == 0 {
				adapter, ok = r.providers.AdapterForProvider(instance.ID)
			}
			if !ok {
				continue
			}
			if mapper, mapped := providerMetadataMapper(adapter); mapped {
				mappers[binding.ID] = mapper
			}
			reporter, reportOK := invocationTargetDiscoveryReporter(adapter)
			describer, describeOK := invocationTargetDescriber(adapter)
			if !reportOK || !describeOK || !reporter.InvocationTargetDiscovery().CanDescribe {
				continue
			}
			described, describedOK := r.describeInvocationTarget(request.Context(), instance, binding, describer, target)
			if describedOK {
				if canonicalRef != "" {
					described.CanonicalModelRef = canonicalRef
				}
				bindingTargets[binding.ID] = described
				if len(bindingTargets) == 1 {
					target = described
				}
			}
		}
		credentialRevision, credentialErr := r.providerCredentialRevision(request.Context(), instance)
		if credentialErr != nil {
			adminBadRequest(writer, "provider credential is unavailable")
			return
		}
		resolved = resolveInvocationTargetWithCatalog(instance, target, bindingTargets, bindings, mappers, credentialRevision, evaluatedAt, r.effectiveModelCatalog())
	}
	r.capabilityMetrics.recordResolution(instance.Type, resolved.TargetKind, resolved.ResolutionState, resolutionSource(resolved))
	if resolved.ResolutionState == domain.ResolutionConflicting || resolved.ResolutionState == domain.ResolutionNoVariant ||
		resolved.ResolutionState == domain.ResolutionCoveredElsewhere || len(resolved.ConflictingBindings) > 0 {
		admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
		actionState := string(resolved.ResolutionState)
		if len(resolved.ConflictingBindings) > 0 && resolved.ResolutionState == domain.ResolutionResolved {
			actionState = "partial_conflict"
		}
		if err := r.appendAdminAuditWithMetadata("admin_user", admin.session.Username, "invocation_target.resolution."+actionState, "provider", instance.ID, "success", "", map[string]any{
			"provider_revision": instance.Revision, "resolution_revision": resolved.ResolutionRevision, "target_kind": string(resolved.TargetKind), "variant_count": len(resolved.Variants), "conflicting_binding_count": len(resolved.ConflictingBindings),
		}); err != nil {
			adminAuditError(writer)
			return
		}
	}
	writeJSON(writer, http.StatusOK, resolved)
}

func (r *Runtime) describeInvocationTarget(ctx context.Context, instance domain.ProviderInstance, binding domain.ProviderProfileBinding, describer provider.InvocationTargetDescriber, target domain.InvocationTargetDescriptor) (domain.InvocationTargetDescriptor, bool) {
	credential, err := r.store.GetCredential(ctx, instance.CredentialID)
	if err != nil {
		return domain.InvocationTargetDescriptor{}, false
	}
	timeout := r.config.Gateway.AttemptResponseHeaderTimeout.Value()
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	describeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	described, err := describer.DescribeInvocationTarget(describeCtx, target)
	if err != nil {
		return domain.InvocationTargetDescriptor{}, false
	}
	items, ok := normalizeInvocationTargets([]domain.InvocationTargetDescriptor{described})
	if !ok || len(items) != 1 || items[0].TargetID != target.TargetID || items[0].TargetKind != target.TargetKind {
		return domain.InvocationTargetDescriptor{}, false
	}
	currentProvider, providerErr := r.store.GetProvider(ctx, instance.ID)
	currentCredential, credentialErr := r.store.GetCredential(ctx, instance.CredentialID)
	if providerErr != nil || credentialErr != nil || currentProvider.Revision != instance.Revision || currentCredential.Revision != credential.Revision {
		return domain.InvocationTargetDescriptor{}, false
	}
	described = items[0]
	return described, true
}

func invocationTargetBindings(instance domain.ProviderInstance, bindingID string) ([]domain.ProviderProfileBinding, error) {
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

// catalogAccess says what a caller of the invocation-target catalogue is allowed
// to spend.
//
// It replaced a single `refresh bool`, which could not tell the two reading
// endpoints apart. The GET is behind a session and not a role, so a cache miss
// there must end the read rather than dial — a read-only admin opening the
// create form would otherwise spend the operator's credential once per binding.
// Resolution is a POST behind requireAdminMutation and its own comment says it
// "may enumerate a cold catalog", but it shared the read's flag and so never
// dialled: on a cold cache every Bedrock model the seed does not carry resolved
// to unknown, and the console told the operator that refreshing would not help,
// which was the opposite of true.
type catalogAccess int

const (
	// catalogCachedOnly answers from the cache or not at all.
	catalogCachedOnly catalogAccess = iota
	// catalogDialOnMiss answers from the cache when it is warm and dials when it
	// is not. The caller has already been charged for being a mutation.
	catalogDialOnMiss
	// catalogAlwaysDial ignores a warm cache. This is what refreshing means.
	catalogAlwaysDial
)

func (r *Runtime) fetchInvocationTargetCatalogs(ctx context.Context, instance domain.ProviderInstance, bindings []domain.ProviderProfileBinding, access catalogAccess) []bindingTargetCatalog {
	results := make([]bindingTargetCatalog, len(bindings))
	slots := make(chan struct{}, invocationTargetFetchConcurrency)
	var wait sync.WaitGroup
	for index, binding := range bindings {
		wait.Add(1)
		go func() {
			defer wait.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			results[index] = r.fetchInvocationTargetCatalog(ctx, instance, binding, access)
		}()
	}
	wait.Wait()
	return results
}

func (r *Runtime) fetchInvocationTargetCatalog(ctx context.Context, instance domain.ProviderInstance, binding domain.ProviderProfileBinding, access catalogAccess) bindingTargetCatalog {
	result := bindingTargetCatalog{binding: binding}
	credentialRevision := uint64(0)
	if r.store != nil {
		credential, err := r.store.GetCredential(ctx, instance.CredentialID)
		if err != nil {
			result.failed, result.errorClass = true, provider.ErrorAuthentication
			return result
		}
		credentialRevision = credential.Revision
	}
	result.credentialRevision = credentialRevision
	adapter, ok := r.providers.AdapterForBinding(instance.ID, binding.ID)
	if !ok && len(instance.Bindings) == 0 {
		adapter, ok = r.providers.AdapterForProvider(instance.ID)
	}
	if !ok {
		result.failed, result.errorClass = true, provider.ErrorUnknown
		return result
	}
	reporter, ok := invocationTargetDiscoveryReporter(adapter)
	if !ok {
		result.failed, result.errorClass = true, provider.ErrorBadRequest
		return result
	}
	result.discovery = reporter.InvocationTargetDiscovery()
	result.mapper, _ = providerMetadataMapper(adapter)
	if !result.discovery.CanEnumerate {
		return result
	}
	lister, ok := invocationTargetLister(adapter)
	if !ok {
		result.failed, result.errorClass = true, provider.ErrorBadRequest
		return result
	}
	cacheKey := instance.ID + "\x00" + binding.ID
	now := time.Now().UTC()
	if access != catalogAlwaysDial {
		r.capabilityResolution.mu.Lock()
		cached, found := r.capabilityResolution.invocationTargets[cacheKey]
		r.capabilityResolution.mu.Unlock()
		if found && now.Before(cached.ExpiresAt) && cached.ProviderRevision == instance.Revision && cached.CredentialRevision == credentialRevision {
			result.items = slices.Clone(cached.Items)
			result.fetchedAt, result.expiresAt, result.cached = cached.FetchedAt, cached.ExpiresAt, true
			return result
		}
		if access == catalogCachedOnly {
			// A miss ends the read here. Falling through would put a billable
			// upstream call behind an endpoint that checks a session and not a
			// role: a read-only admin opening the create form would spend the
			// operator's provider credential once per binding, without a control
			// on the screen that said so. Dialling is for the two callers that
			// are behind requireAdminMutation and say what they cost.
			result.notCached = true
			return result
		}
	}
	timeout := r.config.Gateway.AttemptResponseHeaderTimeout.Value()
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	fetchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	fetchSequence := r.beginInvocationTargetFetch(cacheKey)
	items, err := lister.ListInvocationTargets(fetchCtx, domain.TargetQuery{Region: providerRegion(instance)})
	if err != nil {
		r.capabilityMetrics.recordCatalogRefresh(instance.Type, binding.ProfileID, false)
		result.failed, result.errorClass = true, provider.ErrorUnknown
		var classified *provider.Error
		if errors.As(err, &classified) {
			result.errorClass = classified.Class
		}
		return result
	}
	// Counted after the listing is judged, not after it arrives: a catalogue
	// too large to serve is a refresh that did not succeed.
	items, withinBounds := normalizeInvocationTargets(items)
	r.capabilityMetrics.recordCatalogRefresh(instance.Type, binding.ProfileID, withinBounds)
	if !withinBounds {
		result.failed, result.errorClass = true, provider.ErrorMalformed
		return result
	}
	if r.store != nil {
		currentProvider, providerErr := r.store.GetProvider(ctx, instance.ID)
		currentCredential, credentialErr := r.store.GetCredential(ctx, instance.CredentialID)
		if providerErr != nil || credentialErr != nil || currentProvider.Revision != instance.Revision || currentCredential.Revision != credentialRevision {
			result.failed, result.errorClass = true, provider.ErrorUnknown
			return result
		}
	}
	fetchedAt := time.Now().UTC()
	entry := invocationTargetCatalogCache{Items: items, FetchedAt: fetchedAt, ExpiresAt: fetchedAt.Add(invocationTargetCatalogTTL), ProviderRevision: instance.Revision, CredentialRevision: credentialRevision, FetchSequence: fetchSequence}
	r.capabilityResolution.mu.Lock()
	if r.capabilityResolution.invocationTargets == nil {
		r.capabilityResolution.invocationTargets = make(map[string]invocationTargetCatalogCache)
	}
	winner, winnerFound := r.capabilityResolution.invocationTargets[cacheKey]
	if !winnerFound || winner.FetchSequence != fetchSequence {
		r.capabilityResolution.mu.Unlock()
		if winnerFound && winner.ProviderRevision == instance.Revision && winner.CredentialRevision == credentialRevision && fetchedAt.Before(winner.ExpiresAt) {
			result.items = slices.Clone(winner.Items)
			result.fetchedAt, result.expiresAt, result.cached = winner.FetchedAt, winner.ExpiresAt, true
			return result
		}
		result.failed, result.errorClass = true, provider.ErrorUnknown
		return result
	}
	r.capabilityResolution.invocationTargets[cacheKey] = entry
	r.capabilityResolution.mu.Unlock()
	result.items = slices.Clone(items)
	result.fetchedAt, result.expiresAt = entry.FetchedAt, entry.ExpiresAt
	return result
}

func aggregateInvocationTargets(instance domain.ProviderInstance, results []bindingTargetCatalog, at time.Time) invocationTargetCatalogResponse {
	return aggregateInvocationTargetsWithCatalog(instance, results, at, modelcatalog.Builtin())
}

// at is the instant the resolution is evaluated at, and it decides which
// capability claims are still live. It arrives from the caller rather than being
// read from the global clock here: resolution has to be reproducible for a given
// catalog, and a resolver that reads the wall clock cannot be asserted on except
// by a test that races it.
func aggregateInvocationTargetsWithCatalog(instance domain.ProviderInstance, results []bindingTargetCatalog, at time.Time, catalog *modelcatalog.Catalog) invocationTargetCatalogResponse {
	response := invocationTargetCatalogResponse{
		Items:           make([]adminInvocationTarget, 0),
		Discovery:       domain.InvocationTargetDiscoveryCapabilities{TargetKinds: make([]domain.DeploymentTargetKind, 0)},
		CatalogRevision: catalog.Revision(), ProviderRevision: instance.Revision, Cached: true,
	}
	type aggregate struct {
		target             domain.InvocationTargetDescriptor
		bindingTargets     map[string]domain.InvocationTargetDescriptor
		bindings           []domain.ProviderProfileBinding
		mappers            map[string]provider.ProviderMetadataMapper
		credentialRevision uint64
	}
	byKey := map[string]*aggregate{}
	var keys []string
	for _, result := range results {
		response.Discovery = mergeDiscovery(response.Discovery, result.discovery)
		if result.notCached {
			response.NotCached = true
			continue
		}
		if result.failed {
			response.DegradedBindings = append(response.DegradedBindings, degradedBinding{BindingID: result.binding.ID, ProfileID: result.binding.ProfileID, ErrorClass: result.errorClass})
			continue
		}
		if response.FetchedAt.IsZero() || !result.fetchedAt.IsZero() && result.fetchedAt.Before(response.FetchedAt) {
			response.FetchedAt = result.fetchedAt
		}
		if response.ExpiresAt.IsZero() || !result.expiresAt.IsZero() && result.expiresAt.Before(response.ExpiresAt) {
			response.ExpiresAt = result.expiresAt
		}
		if result.discovery.CanEnumerate && !result.cached {
			response.Cached = false
		}
		for _, target := range result.items {
			key := string(target.TargetKind) + "\x00" + target.TargetID + "\x00" + target.Region
			item := byKey[key]
			if item == nil {
				item = &aggregate{target: target, bindingTargets: map[string]domain.InvocationTargetDescriptor{}, mappers: map[string]provider.ProviderMetadataMapper{}}
				byKey[key] = item
				keys = append(keys, key)
			}
			item.bindings = append(item.bindings, result.binding)
			item.credentialRevision = result.credentialRevision
			item.bindingTargets[result.binding.ID] = target
			if result.mapper != nil {
				item.mappers[result.binding.ID] = result.mapper
			}
		}
	}
	slices.Sort(keys)
	for _, key := range keys {
		item := byKey[key]
		response.Items = append(response.Items, resolveInvocationTargetWithCatalog(instance, item.target, item.bindingTargets, item.bindings, item.mappers, item.credentialRevision, at, catalog))
	}
	return response
}

func resolveInvocationTargetWithCatalog(instance domain.ProviderInstance, target domain.InvocationTargetDescriptor, bindingTargets map[string]domain.InvocationTargetDescriptor, bindings []domain.ProviderProfileBinding, mappers map[string]provider.ProviderMetadataMapper, credentialRevision uint64, at time.Time, catalog *modelcatalog.Catalog) adminInvocationTarget {
	resolved := adminInvocationTarget{InvocationTargetDescriptor: target, Variants: make([]domain.DeploymentVariant, 0), ResolutionState: domain.ResolutionUnknown}
	var allRevisions []string
	claimsExist := false
	hadConflict := false
	for _, binding := range bindings {
		bindingTarget := target
		if scoped, ok := bindingTargets[binding.ID]; ok {
			bindingTarget = scoped
		}
		scope := domain.InvocationTargetScopeKey{
			ProviderID: instance.ID, TargetKind: bindingTarget.TargetKind, TargetID: bindingTarget.TargetID,
			BindingID: binding.ID, ProfileID: binding.ProfileID, Location: bindingTarget.Region,
		}
		catalogModel, entry := invocationTargetCatalogEntryWithCatalog(instance, binding, bindingTarget, catalog)
		var claims []domain.CapabilityClaim
		var mergeClaims []modelcatalog.Claim
		bindingConflict := false
		if entry.Status != modelcatalog.StatusUnknown {
			claims = append(claims, claimsFromCatalogEntry(entry, scope, target.FetchedAt)...)
			unsupported := append(slices.Clone(entry.Unsupported), entry.Conflicts...)
			mergeClaims = append(mergeClaims, modelcatalog.Claim{Source: entry.Source, Supported: entry.Capabilities, Unsupported: unsupported})
			claimsExist = true
		}
		if mapper := mappers[binding.ID]; mapper != nil {
			metadataClaims := mapper.MapCapabilityClaims(bindingTarget, scope, bindingTarget.FetchedAt)
			expiresAt := bindingTarget.FetchedAt.Add(invocationTargetCatalogTTL)
			active := metadataClaims[:0]
			for index := range metadataClaims {
				if metadataClaims[index].ExpiresAt == nil && !bindingTarget.FetchedAt.IsZero() {
					metadataClaims[index].ExpiresAt = &expiresAt
				}
				if err := metadataClaims[index].Validate(); err != nil || metadataClaims[index].Scope != scope {
					bindingConflict = true
					continue
				}
				if metadataClaims[index].ActiveAt(at) {
					active = append(active, metadataClaims[index])
				}
			}
			metadataClaims = active
			claims = append(claims, metadataClaims...)
			if len(metadataClaims) > 0 {
				mergeClaims = append(mergeClaims, catalogClaimFromCapabilities(modelcatalog.SourceProviderMetadata, metadataClaims, bindingTarget.Metadata))
				claimsExist = true
			}
		}
		mergeKey := modelcatalog.Key{ProviderType: instance.Type, Profile: binding.ProfileID, TargetKind: bindingTarget.TargetKind, Model: catalogModel, Region: bindingTarget.Region}
		merged := modelcatalog.Merge(mergeKey, mergeClaims...)
		capabilities := modelcatalog.Clamp(dependencyClosure(merged.Capabilities), binding.Capabilities)
		if len(merged.Conflicts) > 0 || entry.Status == modelcatalog.StatusConflicting {
			bindingConflict = true
			for index := range claims {
				if slices.Contains(merged.Conflicts, claims[index].CapabilityID) {
					claims[index].Status = domain.ClaimConflicting
				}
			}
		}
		if bindingConflict {
			hadConflict = true
			claimsExist = true
			resolved.ConflictingBindings = append(resolved.ConflictingBindings, binding.ID)
			continue
		}
		if !capabilities.AnyOperation() {
			continue
		}
		revisionParts := []string{instance.ID, strconv.FormatUint(instance.Revision, 10), strconv.FormatUint(credentialRevision, 10), bindingTarget.TargetID, string(bindingTarget.TargetKind), binding.ID, string(binding.ProfileID), entry.Revision(), normalizedResolutionFingerprint(bindingTarget, capabilities, claims)}
		for _, claim := range claims {
			revisionParts = append(revisionParts, claim.Revision, string(claim.Status))
		}
		revision := provider.CapabilityClaimRevision(revisionParts...)
		variant := domain.DeploymentVariant{
			ID: binding.ID, BindingID: binding.ID, ProfileID: binding.ProfileID, Target: bindingTarget,
			Capabilities: capabilities, CapabilityClaims: claims, ResolutionState: domain.ResolutionResolved, Revision: revision,
		}
		if err := variant.ValidateForProvider(instance.ID, binding); err != nil {
			hadConflict = true
			resolved.ConflictingBindings = append(resolved.ConflictingBindings, binding.ID)
			continue
		}
		resolved.Variants = append(resolved.Variants, variant)
		allRevisions = append(allRevisions, revision)
	}
	if len(resolved.Variants) > 0 {
		resolved.ResolutionState = domain.ResolutionResolved
	} else if hadConflict {
		resolved.ResolutionState = domain.ResolutionConflicting
	} else if claimsExist {
		resolved.ResolutionState = domain.ResolutionNoVariant
	} else if profiles := catalog.ProfilesCovering(instance.Type, target.TargetKind, coverageModel(instance, target)); len(profiles) > 0 {
		// Only from unknown. A conflict needs a person to read it and a
		// no_variant already knows which binding fell short; rewriting either
		// into "it lives on another interface" would replace an answer the
		// operator can act on with a different one that is not true of it.
		resolved.ResolutionState = domain.ResolutionCoveredElsewhere
		resolved.CoveredByProfiles = profiles
	}
	if len(allRevisions) == 0 {
		allRevisions = []string{instance.ID, strconv.FormatUint(instance.Revision, 10), target.TargetID, string(target.TargetKind), string(resolved.ResolutionState), catalog.Revision()}
	}
	resolved.ResolutionRevision = provider.CapabilityClaimRevision(allRevisions...)
	return resolved
}

func normalizedResolutionFingerprint(target domain.InvocationTargetDescriptor, capabilities domain.ProviderCapabilities, claims []domain.CapabilityClaim) string {
	metadata := target.Metadata
	metadata.InputModalities = sortedStrings(metadata.InputModalities)
	metadata.OutputModalities = sortedStrings(metadata.OutputModalities)
	metadata.SupportedOperations = sortedStrings(metadata.SupportedOperations)
	metadata.InferenceTypes = sortedStrings(metadata.InferenceTypes)
	type claimSemantic struct {
		CapabilityID string
		Status       domain.ClaimStatus
		Source       domain.ClaimSource
		Evidence     domain.CapabilityEvidence
		Revision     string
	}
	semantics := make([]claimSemantic, 0, len(claims))
	for _, claim := range claims {
		semantics = append(semantics, claimSemantic{claim.CapabilityID, claim.Status, claim.Source, claim.Evidence, claim.Revision})
	}
	slices.SortFunc(semantics, func(left, right claimSemantic) int {
		return strings.Compare(left.CapabilityID+"\x00"+string(left.Source)+"\x00"+left.Revision, right.CapabilityID+"\x00"+string(right.Source)+"\x00"+right.Revision)
	})
	payload, _ := json.Marshal(struct {
		TargetID, CanonicalModelRef, Region string
		TargetKind                          domain.DeploymentTargetKind
		Availability                        domain.AvailabilityState
		Lifecycle                           domain.TargetLifecycle
		Metadata                            domain.NormalizedModelMetadata
		Capabilities                        domain.ProviderCapabilities
		Claims                              []claimSemantic
	}{target.TargetID, target.CanonicalModelRef, target.Region, target.TargetKind, target.Availability, target.Lifecycle, metadata, capabilities, semantics})
	return provider.CapabilityClaimRevision(string(payload))
}

func sortedStrings(values []string) []string {
	result := slices.Clone(values)
	slices.Sort(result)
	return result
}

func resolutionSource(resolved adminInvocationTarget) domain.ClaimSource {
	for _, variant := range resolved.Variants {
		for _, claim := range variant.CapabilityClaims {
			if claim.Status == domain.ClaimSupported {
				return claim.Source
			}
		}
	}
	return ""
}

func invocationTargetCatalogEntry(instance domain.ProviderInstance, binding domain.ProviderProfileBinding, target domain.InvocationTargetDescriptor) (string, modelcatalog.Entry) {
	return invocationTargetCatalogEntryWithCatalog(instance, binding, target, modelcatalog.Builtin())
}

// coverageModel is the model name the catalog would list this target under,
// independently of any binding. The per-binding derivation cannot answer that:
// it is asked which interface serves the model, and the question here is
// whether any interface does.
//
// An empty result means the question must not be asked. A self-hosted
// OpenAI-compatible endpoint may serve something it calls `gpt-5`, and that name
// is not a claim about OpenAI's gpt-5 — the per-binding lookup refuses to read
// the catalog by name there for exactly that reason, and a coverage lookup that
// did would reintroduce the inheritance by another route, telling the operator
// their own endpoint's model "lives on another interface".
func coverageModel(instance domain.ProviderInstance, target domain.InvocationTargetDescriptor) string {
	if model := strings.TrimSpace(target.CanonicalModelRef); model != "" {
		return model
	}
	if instance.Type == domain.ProviderOpenAICompatible && target.TargetKind == domain.TargetCustomEndpointModel {
		return ""
	}
	return strings.TrimSpace(target.TargetID)
}

func invocationTargetCatalogEntryWithCatalog(instance domain.ProviderInstance, binding domain.ProviderProfileBinding, target domain.InvocationTargetDescriptor, catalog *modelcatalog.Catalog) (string, modelcatalog.Entry) {
	model := strings.TrimSpace(target.CanonicalModelRef)
	if model == "" && instance.Type == domain.ProviderOpenAICompatible && target.TargetKind == domain.TargetCustomEndpointModel {
		key := modelcatalog.Key{ProviderType: instance.Type, Profile: binding.ProfileID, TargetKind: target.TargetKind, Model: target.TargetID, Region: target.Region}
		return target.TargetID, modelcatalog.Unknown(key)
	}
	if model == "" {
		model = target.TargetID
	}
	if target.CanonicalModelRef != "" {
		if providerType, profileID, ok := capabilityModelCatalogScope(instance.Type); ok {
			canonicalKind := domain.TargetModelID
			if providerType == domain.ProviderOpenAICompatible {
				canonicalKind = domain.TargetCustomEndpointModel
			}
			key := modelcatalog.Key{ProviderType: providerType, Profile: profileID, TargetKind: canonicalKind, Model: model}
			entry, found := catalog.Lookup(key)
			if !found {
				entry = modelcatalog.Unknown(key)
			}
			return model, entry
		}
	}
	key := modelcatalog.Key{ProviderType: instance.Type, Profile: binding.ProfileID, TargetKind: target.TargetKind, Model: model, Region: target.Region}
	entry, found := catalog.Lookup(key)
	if !found {
		entry = modelcatalog.Unknown(key)
	}
	return model, entry
}

func claimsFromCatalogEntry(entry modelcatalog.Entry, scope domain.InvocationTargetScopeKey, observedAt time.Time) []domain.CapabilityClaim {
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	var claims []domain.CapabilityClaim
	for _, name := range domain.CapabilityNames() {
		if !modelcatalog.HasCapability(entry.Capabilities, name) && !slices.Contains(entry.Conflicts, name) && !slices.Contains(entry.Unsupported, name) {
			continue
		}
		status := domain.ClaimSupported
		if slices.Contains(entry.Conflicts, name) {
			status = domain.ClaimConflicting
		} else if slices.Contains(entry.Unsupported, name) {
			status = domain.ClaimUnsupported
		}
		claims = append(claims, domain.CapabilityClaim{
			CapabilityID: name, Status: status, Evidence: entry.Source.MaxEvidence(),
			Source: domain.ClaimSource(entry.Source), Scope: scope, ObservedAt: observedAt,
			Revision: provider.CapabilityClaimRevision(string(entry.Source), entry.Revision(), name, string(status)),
		})
	}
	return claims
}

func catalogClaimFromCapabilities(source modelcatalog.Source, claims []domain.CapabilityClaim, metadata domain.NormalizedModelMetadata) modelcatalog.Claim {
	capabilities := domain.ProviderCapabilities{MaxContextTokens: metadata.MaxContextTokens, MaxOutputTokens: metadata.MaxOutputTokens}
	var unsupported []string
	for _, claim := range claims {
		switch claim.Status {
		case domain.ClaimSupported:
			setCapability(&capabilities, claim.CapabilityID)
		case domain.ClaimUnsupported:
			unsupported = append(unsupported, claim.CapabilityID)
		}
	}
	return modelcatalog.Claim{Source: source, Supported: capabilities, Unsupported: unsupported}
}

func dependencyClosure(value domain.ProviderCapabilities) domain.ProviderCapabilities {
	if !value.Chat {
		value.Streaming, value.Tools, value.Vision, value.JSONMode = false, false, false, false
		value.DeveloperRole, value.Reasoning, value.StreamUsage = false, false, false
	}
	if !value.Streaming {
		value.StreamUsage = false
	}
	return value
}

func setCapability(value *domain.ProviderCapabilities, name string) {
	domain.SetCapability(value, name, true)
}

// maxInvocationTargetEntries bounds a catalogue Halro did not write, at the one
// point every adapter's listing passes through. One adapter already bounded its
// own, which left the other four holding nothing back but the 16 MiB response
// ceiling — and a body that size holds several hundred thousand
// `{"id":…,"owned_by":…}` entries, all of which would reach a cache and then a
// console list. An account an operator picks a model from does not have this
// many, so exceeding it is an upstream misbehaving rather than a large account,
// and the refresh fails instead of quietly keeping a prefix.
const maxInvocationTargetEntries = 2000

// maxInvocationTargetLabelLength bounds the two strings the upstream writes that
// are shown rather than matched on. Unlike the identifier, which is dropped when
// it is implausible, an oversized label costs only itself: the name falls back
// to the identifier and the owner is cleared, so the model stays selectable.
const maxInvocationTargetLabelLength = 256

func normalizeInvocationTargets(items []domain.InvocationTargetDescriptor) ([]domain.InvocationTargetDescriptor, bool) {
	if len(items) > maxInvocationTargetEntries {
		return nil, false
	}
	seen := make(map[string]struct{}, len(items))
	normalized := make([]domain.InvocationTargetDescriptor, 0, len(items))
	for _, item := range items {
		item.TargetID = strings.TrimSpace(item.TargetID)
		if item.TargetID == "" || len(item.TargetID) > 2048 || item.TargetKind == "" {
			continue
		}
		key := string(item.TargetKind) + "\x00" + item.TargetID + "\x00" + item.Region
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		item.DisplayName = strings.TrimSpace(item.DisplayName)
		if item.DisplayName == "" || len(item.DisplayName) > maxInvocationTargetLabelLength {
			item.DisplayName = item.TargetID
		}
		if len(strings.TrimSpace(item.OwnedBy)) > maxInvocationTargetLabelLength {
			item.OwnedBy = ""
		}
		if item.Availability == "" {
			item.Availability = domain.AvailabilityAvailable
		}
		if item.Lifecycle == "" {
			item.Lifecycle = domain.TargetLifecycleUnknown
		}
		normalized = append(normalized, item)
	}
	slices.SortFunc(normalized, func(left, right domain.InvocationTargetDescriptor) int {
		return strings.Compare(left.DisplayName+"\x00"+left.TargetID, right.DisplayName+"\x00"+right.TargetID)
	})
	return normalized, true
}

func mergeDiscovery(left, right domain.InvocationTargetDiscoveryCapabilities) domain.InvocationTargetDiscoveryCapabilities {
	for _, kind := range right.TargetKinds {
		if !slices.Contains(left.TargetKinds, kind) {
			left.TargetKinds = append(left.TargetKinds, kind)
		}
	}
	slices.Sort(left.TargetKinds)
	left.CanEnumerate = left.CanEnumerate || right.CanEnumerate
	left.CanDescribe = left.CanDescribe || right.CanDescribe
	left.CanVerify = left.CanVerify || right.CanVerify
	left.RequiresManagementIdentity = left.RequiresManagementIdentity || right.RequiresManagementIdentity
	left.RequiresCanonicalModelMapping = left.RequiresCanonicalModelMapping || right.RequiresCanonicalModelMapping
	return left
}

func canonicalModelTemplates(instance domain.ProviderInstance, bindings []domain.ProviderProfileBinding, credentialRevision uint64, at time.Time) []adminInvocationTarget {
	return canonicalModelTemplatesWithCatalog(instance, bindings, credentialRevision, at, modelcatalog.Builtin())
}

func canonicalModelTemplatesWithCatalog(instance domain.ProviderInstance, bindings []domain.ProviderProfileBinding, credentialRevision uint64, at time.Time, catalog *modelcatalog.Catalog) []adminInvocationTarget {
	providerType, profileID, ok := capabilityModelCatalogScope(instance.Type)
	if !ok {
		return nil
	}
	var templates []adminInvocationTarget
	for _, entry := range catalog.Entries() {
		if entry.Key.ProviderType != providerType || entry.Key.Profile != profileID || entry.Key.Region != "" || !entry.Capabilities.AnyOperation() {
			continue
		}
		target := domain.InvocationTargetDescriptor{
			TargetID: entry.Key.Model, TargetKind: domain.TargetModelID, DisplayName: entry.Key.Model,
			CanonicalModelRef: entry.Key.Model, Lifecycle: domain.TargetLifecycleUnknown,
			MetadataSource: domain.MetadataSourceNone, Availability: domain.AvailabilityUnverified, FetchedAt: at,
		}
		templates = append(templates, resolveInvocationTargetWithCatalog(instance, target, nil, bindings, nil, credentialRevision, at, catalog))
	}
	slices.SortFunc(templates, func(left, right adminInvocationTarget) int {
		return strings.Compare(left.DisplayName, right.DisplayName)
	})
	return templates
}

func capabilityModelCatalogScope(providerType domain.ProviderType) (domain.ProviderType, domain.ProviderProfileID, bool) {
	switch providerType {
	case domain.ProviderAzureOpenAI:
		return domain.ProviderOpenAI, domain.ProfileOpenAIChatEmbeddings, true
	case domain.ProviderOpenAICompatible:
		return domain.ProviderOpenAICompatible, domain.ProfileOpenAICompatible, true
	default:
		return "", "", false
	}
}

func unwrapOptional[T any](adapter provider.Adapter) (T, bool) {
	var zero T
	for adapter != nil {
		if value, ok := any(adapter).(T); ok {
			return value, true
		}
		wrapper, ok := adapter.(provider.AdapterUnwrapper)
		if !ok {
			return zero, false
		}
		next := wrapper.UnwrapAdapter()
		if next == adapter {
			return zero, false
		}
		adapter = next
	}
	return zero, false
}

func invocationTargetLister(adapter provider.Adapter) (provider.InvocationTargetLister, bool) {
	return unwrapOptional[provider.InvocationTargetLister](adapter)
}

func invocationTargetDiscoveryReporter(adapter provider.Adapter) (provider.InvocationTargetDiscoveryReporter, bool) {
	return unwrapOptional[provider.InvocationTargetDiscoveryReporter](adapter)
}

func invocationTargetDescriber(adapter provider.Adapter) (provider.InvocationTargetDescriber, bool) {
	return unwrapOptional[provider.InvocationTargetDescriber](adapter)
}

func providerMetadataMapper(adapter provider.Adapter) (provider.ProviderMetadataMapper, bool) {
	return unwrapOptional[provider.ProviderMetadataMapper](adapter)
}

func (r *Runtime) beginInvocationTargetFetch(cacheKey string) uint64 {
	sequence := invocationTargetFetchSequence.Add(1)
	r.capabilityResolution.mu.Lock()
	defer r.capabilityResolution.mu.Unlock()
	if r.capabilityResolution.invocationTargets == nil {
		r.capabilityResolution.invocationTargets = make(map[string]invocationTargetCatalogCache)
	}
	entry := r.capabilityResolution.invocationTargets[cacheKey]
	entry.FetchSequence = sequence
	r.capabilityResolution.invocationTargets[cacheKey] = entry
	return sequence
}

func (r *Runtime) clearInvocationTargetCatalog(providerID string) {
	r.capabilityResolution.mu.Lock()
	defer r.capabilityResolution.mu.Unlock()
	for key := range r.capabilityResolution.invocationTargets {
		if strings.HasPrefix(key, providerID+"\x00") {
			delete(r.capabilityResolution.invocationTargets, key)
		}
	}
}

func (r *Runtime) clearAllInvocationTargetCatalogs() {
	r.capabilityResolution.mu.Lock()
	defer r.capabilityResolution.mu.Unlock()
	clear(r.capabilityResolution.invocationTargets)
}

func (r *Runtime) providerCredentialRevision(ctx context.Context, instance domain.ProviderInstance) (uint64, error) {
	if r.store == nil {
		return 0, nil
	}
	credential, err := r.store.GetCredential(ctx, instance.CredentialID)
	if err != nil {
		return 0, err
	}
	return credential.Revision, nil
}

func (r *Runtime) effectiveModelCatalog() *modelcatalog.Catalog {
	if r.capabilityResolution.catalog != nil {
		return r.capabilityResolution.catalog.Current()
	}
	return modelcatalog.Builtin()
}

func (r *Runtime) modelCatalogUnavailable() bool {
	if r.capabilityResolution.catalog == nil {
		return true
	}
	return r.capabilityResolution.catalog.Status().State != modelcatalog.CatalogStateCurrent
}
