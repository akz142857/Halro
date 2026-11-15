package app

import (
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/safetransport"
	"github.com/akz142857/Halro/internal/usage"
)

type onboardingState string

const (
	onboardingConfiguring       onboardingState = "configuring"
	onboardingReadyToVerify     onboardingState = "ready_to_verify"
	onboardingVerifyFailed      onboardingState = "verify_failed"
	onboardingFirstValueReached onboardingState = "first_value_reached"
)

type onboardingGoalState string

const (
	onboardingGoalComplete onboardingGoalState = "complete"
	onboardingGoalCurrent  onboardingGoalState = "current"
	onboardingGoalBlocked  onboardingGoalState = "blocked"
	onboardingGoalError    onboardingGoalState = "error"
)

type onboardingGoal struct {
	Key        string              `json:"key"`
	State      onboardingGoalState `json:"state"`
	DetailCode string              `json:"detail_code"`
	ActionHref string              `json:"action_href"`
}

type onboardingVerification struct {
	Outcome        string    `json:"outcome"`
	RequestID      string    `json:"request_id"`
	HTTPStatus     int       `json:"http_status,omitempty"`
	ErrorClass     string    `json:"error_class,omitempty"`
	RequestedModel string    `json:"requested_model,omitempty"`
	CompletedAt    time.Time `json:"completed_at"`
}

type onboardingReadiness struct {
	Version          int                     `json:"version"`
	State            onboardingState         `json:"state"`
	CompletedGoals   int                     `json:"completed_goals"`
	TotalGoals       int                     `json:"total_goals"`
	Goals            []onboardingGoal        `json:"goals"`
	LastVerification *onboardingVerification `json:"last_verification,omitempty"`
	EvaluatedAt      time.Time               `json:"evaluated_at"`
}

type onboardingResources struct {
	Credentials       []domain.Credential
	Providers         []domain.ProviderInstance
	Deployments       []domain.Deployment
	Routes            []domain.Route
	Projects          []domain.Project
	Keys              []domain.GatewayKey
	PriceReady        map[string]bool
	EgressRuntimeIDs  map[string]string
	RouteWithholdings map[string]routeWithholding
	// EndpointPolicy is what "is this provider's endpoint usable" means for this
	// instance. Readiness compares a derived audience against the stored one, and
	// deriving it under a stricter policy than the one the provider was created
	// under reported a correctly configured provider as not connected.
	EndpointPolicy safetransport.Policy
}

func (r *Runtime) getAdminOnboardingReadiness(writer http.ResponseWriter, request *http.Request) {
	if !r.syncUsageAdmin(writer, request) {
		return
	}
	now := time.Now().UTC()
	metrics := r.usage.Metrics()
	if metrics.RequestsSuccess > 0 {
		writeJSON(writer, http.StatusOK, completedOnboarding(now))
		return
	}
	runtimeIDs := map[string]string{}
	if registry := r.providerEgress.Current(); registry != nil {
		for _, proxy := range registry.descriptions() {
			runtimeIDs[proxy.ID] = proxy.Fingerprint
		}
	}
	resources := onboardingResources{
		EndpointPolicy: providerEndpointPolicy(r.config), EgressRuntimeIDs: runtimeIDs,
		RouteWithholdings: r.routeWithholdings(),
	}
	var err error
	if resources.Credentials, err = r.store.ListCredentials(request.Context()); err != nil {
		adminStoreError(writer)
		return
	}
	if resources.Providers, err = r.store.ListProviders(request.Context()); err != nil {
		adminStoreError(writer)
		return
	}
	if resources.Deployments, err = r.store.ListDeployments(request.Context()); err != nil {
		adminStoreError(writer)
		return
	}
	if resources.Routes, err = r.store.ListRoutes(request.Context()); err != nil {
		adminStoreError(writer)
		return
	}
	if resources.Projects, err = r.store.ListProjects(request.Context()); err != nil {
		adminStoreError(writer)
		return
	}
	if resources.Keys, err = r.store.ListGatewayKeys(request.Context()); err != nil {
		adminStoreError(writer)
		return
	}

	resources.PriceReady = make(map[string]bool, len(resources.Deployments))
	for _, deployment := range resources.Deployments {
		if deployment.DeletedAt != nil {
			continue
		}
		quarantined, _, quarantineErr := r.store.DeploymentPricingQuarantine(request.Context(), deployment.ID)
		if quarantineErr != nil {
			adminStoreError(writer)
			return
		}
		if quarantined {
			continue
		}
		_, priceErr := r.store.SelectDeploymentPriceVersion(request.Context(), deployment.ID, now)
		resources.PriceReady[deployment.ID] = priceErr == nil
	}

	evidence, err := r.usage.OnboardingEvidence()
	if err != nil {
		r.logger.Error("usage history read failed", "error", err)
		adminStoreError(writer)
		return
	}
	writeJSON(writer, http.StatusOK, evaluateOnboardingReadiness(
		now, resources, r.usage.Metrics(), evidence,
	))
}

func evaluateOnboardingReadiness(now time.Time, resources onboardingResources, metrics usage.Metrics, evidence usage.OnboardingEvidence) onboardingReadiness {
	if metrics.RequestsSuccess > 0 {
		return completedOnboarding(now)
	}

	credentialReady := make(map[string]domain.Credential)
	for _, credential := range resources.Credentials {
		if len(credential.Ciphertext) > 0 && !internalCredentialType(credential.Type) {
			credentialReady[credential.ID] = credential
		}
	}

	// A deployment or route probe reaches upstream through the provider it
	// names, so a healthy one proves everything the provider's own connection
	// test proves and the model on top of it. Requiring the provider test
	// anyway told operators who had already probed the whole chain that they
	// had not started. Evidence collected before the provider was last edited
	// says nothing about the provider as it now stands, which is why the
	// instant is kept and compared rather than just the fact of a probe.
	providers := make(map[string]domain.ProviderInstance, len(resources.Providers))
	for _, provider := range resources.Providers {
		providers[provider.ID] = provider
	}
	deploymentProvider := make(map[string]string, len(resources.Deployments))
	downstreamProbe := make(map[string]time.Time)
	recordProbe := func(providerID, runtimeID string, at *time.Time) {
		provider, exists := providers[providerID]
		if !exists || at == nil || !onboardingProbeRuntimeCurrent(provider, resources.EgressRuntimeIDs[provider.EgressProxyID], runtimeID) {
			return
		}
		if newest, seen := downstreamProbe[providerID]; !seen || at.After(newest) {
			downstreamProbe[providerID] = *at
		}
	}
	for _, deployment := range resources.Deployments {
		if deployment.DeletedAt != nil {
			continue
		}
		deploymentProvider[deployment.ID] = deployment.ProviderID
		if deployment.LastTestStatus == domain.DeploymentTestHealthy && deployment.LastTestRevision == deployment.Revision {
			recordProbe(deployment.ProviderID, deployment.LastTestRuntimeID, deployment.LastTestedAt)
		}
	}
	for _, route := range resources.Routes {
		if route.DeletedAt != nil {
			continue
		}
		if route.LastTestStatus == domain.DeploymentTestHealthy && route.LastTestRevision == route.Revision {
			recordProbe(deploymentProvider[route.DeploymentID], route.LastTestRuntimeID, route.LastTestedAt)
		}
	}
	// A finalized successful request is stronger evidence than an explicit
	// control-plane probe: it proves the selected Route, Deployment, Provider,
	// credential, Project grant, and Gateway Key worked together. Keep it tied
	// to the exact attempt IDs and revision timestamps so a later edit still
	// invalidates the evidence.
	for providerID, at := range evidence.Providers {
		recordProbe(providerID, "", &at)
	}

	hasProvider := false
	hasProviderCredential := false
	hasEnabledProvider := false
	hasProviderBinding := false
	configuredProviders := make(map[string]domain.ProviderInstance)
	readyProviders := make(map[string]domain.ProviderInstance)
	for _, provider := range resources.Providers {
		if provider.DeletedAt != nil {
			continue
		}
		hasProvider = true
		credential, ok := credentialReady[provider.CredentialID]
		credentialProfile, profileOK := domain.ResolveCredentialProfile(credential.Type, credential.AccessSurface, credential.Scheme)
		if !ok || credential.Type != provider.Type || !profileOK || provider.AccessSurface != credentialProfile.AccessSurface ||
			provider.CredentialScheme != credentialProfile.CredentialScheme {
			continue
		}
		audience, audienceErr := safetransport.AudienceWithPolicy(provider.BaseURL, string(provider.Type), resources.EndpointPolicy)
		if audienceErr != nil || audience != credential.Audience {
			continue
		}
		hasProviderCredential = true
		if !provider.Enabled {
			continue
		}
		hasEnabledProvider = true
		bindingEnabled := false
		for _, binding := range provider.EffectiveProfileBindings() {
			if binding.Enabled {
				bindingEnabled = true
				break
			}
		}
		if !bindingEnabled {
			continue
		}
		hasProviderBinding = true
		if provider.EgressProxyID != "" && resources.EgressRuntimeIDs[provider.EgressProxyID] == "" {
			continue
		}
		configuredProviders[provider.ID] = provider
		if providerTestIsCurrent(provider, resources.EgressRuntimeIDs[provider.EgressProxyID]) && provider.LastTestStatus == domain.DeploymentTestHealthy {
			readyProviders[provider.ID] = provider
			continue
		}
		if probedAt, proven := downstreamProbe[provider.ID]; proven && !probedAt.Before(provider.UpdatedAt) {
			readyProviders[provider.ID] = provider
		}
	}

	connectDetail := "provider_ready"
	switch {
	case len(credentialReady) == 0:
		connectDetail = "credential_missing"
	case !hasProvider:
		connectDetail = "provider_missing"
	case !hasProviderCredential:
		connectDetail = "provider_credential_unavailable"
	case !hasEnabledProvider:
		connectDetail = "provider_disabled"
	case !hasProviderBinding:
		connectDetail = "provider_binding_disabled"
	case len(configuredProviders) == 0:
		connectDetail = "provider_egress_unavailable"
	case len(readyProviders) == 0:
		connectDetail = "provider_configured"
	}
	connectReady := len(configuredProviders) > 0

	hasDeployment := false
	hasLinkedDeployment := false
	hasDeploymentPrice := false
	hasTestedDeployment := false
	hasEnabledDeployment := false
	readyDeployments := make(map[string]domain.Deployment)
	for _, deployment := range resources.Deployments {
		if deployment.DeletedAt != nil {
			continue
		}
		hasDeployment = true
		provider, ok := configuredProviders[deployment.ProviderID]
		if !ok {
			continue
		}
		if deployment.BindingID != "" {
			binding, exists := provider.ProfileBinding(deployment.BindingID)
			if !exists || !binding.Enabled {
				continue
			}
		}
		hasLinkedDeployment = true
		if !resources.PriceReady[deployment.ID] || deployment.PricingQuarantined {
			continue
		}
		hasDeploymentPrice = true
		if _, verified := readyProviders[provider.ID]; !verified {
			continue
		}
		runtimeID := resources.EgressRuntimeIDs[provider.EgressProxyID]
		provenAt, provenByTraffic := evidence.Deployments[deployment.ID]
		healthyTest := deployment.LastTestStatus == domain.DeploymentTestHealthy && deployment.LastTestRevision == deployment.Revision &&
			onboardingProbeRuntimeCurrent(provider, runtimeID, deployment.LastTestRuntimeID)
		currentTraffic := provenByTraffic && provider.EgressProxyID == "" && !provenAt.Before(deployment.UpdatedAt) && !provenAt.Before(provider.UpdatedAt)
		if !healthyTest && !currentTraffic {
			continue
		}
		hasTestedDeployment = true
		if !deployment.Enabled {
			continue
		}
		hasEnabledDeployment = true
		readyDeployments[deployment.ID] = deployment
	}

	hasRoute := false
	hasLinkedRoute := false
	hasEnabledRoute := false
	hasServingRoute := false
	readyRoutes := make(map[string]domain.Route)
	for _, route := range resources.Routes {
		if route.DeletedAt != nil {
			continue
		}
		hasRoute = true
		deployment, ok := readyDeployments[route.DeploymentID]
		if !ok {
			continue
		}
		hasLinkedRoute = true
		if !route.Enabled {
			continue
		}
		hasEnabledRoute = true
		if _, withheld := resources.RouteWithholdings[route.ID]; withheld {
			continue
		}
		hasServingRoute = true
		provider := configuredProviders[deployment.ProviderID]
		provenAt, provenByTraffic := evidence.Routes[route.ID]
		healthyTest := route.LastTestStatus == domain.DeploymentTestHealthy && route.LastTestRevision == route.Revision &&
			onboardingProbeRuntimeCurrent(provider, resources.EgressRuntimeIDs[provider.EgressProxyID], route.LastTestRuntimeID)
		currentTraffic := provenByTraffic && provider.EgressProxyID == "" && !provenAt.Before(route.UpdatedAt) && !provenAt.Before(provider.UpdatedAt)
		if healthyTest || currentTraffic {
			readyRoutes[route.PublicModel] = route
		}
	}

	publishDetail := "model_ready"
	switch {
	case !connectReady:
		publishDetail = "publish_waiting_for_provider"
	case !hasDeployment:
		publishDetail = "deployment_missing"
	case !hasLinkedDeployment:
		publishDetail = "deployment_provider_unavailable"
	case !hasDeploymentPrice:
		publishDetail = "deployment_price_missing"
	case !hasTestedDeployment:
		publishDetail = "deployment_test_required"
	case !hasEnabledDeployment:
		publishDetail = "deployment_disabled"
	case !hasRoute:
		publishDetail = "route_missing"
	case !hasLinkedRoute:
		publishDetail = "route_deployment_unavailable"
	case !hasEnabledRoute:
		publishDetail = "route_disabled"
	case !hasServingRoute:
		publishDetail = "route_withheld"
	case len(readyRoutes) == 0:
		publishDetail = "route_test_required"
	}
	publishReady := len(readyRoutes) > 0

	hasProject := false
	hasEnabledProject := false
	hasAllowedRoute := false
	readyProjects := make(map[string]domain.Project)
	var keyProjectID string
	for _, project := range resources.Projects {
		if project.DeletedAt != nil {
			continue
		}
		hasProject = true
		if !project.Enabled {
			continue
		}
		hasEnabledProject = true
		matchesRoute := false
		for _, alias := range project.AllowedModels {
			if _, ok := readyRoutes[alias]; ok {
				matchesRoute = true
				break
			}
		}
		if !matchesRoute {
			continue
		}
		hasAllowedRoute = true
		readyProjects[project.ID] = project
		if keyProjectID == "" {
			keyProjectID = project.ID
		}
	}
	activeKeys := make(map[string]domain.GatewayKey)
	var verifyProjectID, verifyModel string
	for _, key := range resources.Keys {
		if key.DeletedAt != nil || !key.Enabled || key.ExpiresAt != nil && !key.ExpiresAt.After(now) ||
			!domain.HasGatewayScope(key.Scopes, domain.GatewayScopeInference) {
			continue
		}
		project, ok := readyProjects[key.ProjectID]
		if !ok {
			continue
		}
		activeKeys[key.ID] = key
		if verifyProjectID == "" {
			for _, model := range project.AllowedModels {
				if _, ready := readyRoutes[model]; ready {
					verifyProjectID, verifyModel = project.ID, model
					break
				}
			}
		}
	}
	accessReady := len(activeKeys) > 0

	accessDetail := "access_ready"
	switch {
	case !publishReady:
		accessDetail = "access_waiting_for_model"
	case !hasProject:
		accessDetail = "project_missing"
	case !hasEnabledProject:
		accessDetail = "project_disabled"
	case !hasAllowedRoute:
		accessDetail = "project_route_unavailable"
	case !accessReady:
		accessDetail = "key_missing"
	}
	requestDetail := "request_waiting_for_access"
	if accessReady {
		requestDetail = "request_ready"
	}

	result := onboardingReadiness{
		Version: 1, State: onboardingConfiguring, TotalGoals: 4, EvaluatedAt: now,
		Goals: []onboardingGoal{
			{Key: "connect_provider", DetailCode: connectDetail, ActionHref: onboardingConnectHref(connectDetail)},
			{Key: "publish_model", DetailCode: publishDetail, ActionHref: onboardingPublishHref(publishDetail)},
			{Key: "grant_access", DetailCode: accessDetail, ActionHref: onboardingAccessHref(accessDetail, keyProjectID)},
			{Key: "verify_request", DetailCode: requestDetail, ActionHref: onboardingRequestHref(verifyProjectID, verifyModel)},
		},
	}
	completed := []bool{connectReady, publishReady, accessReady, false}

	if accessReady {
		result.State = onboardingReadyToVerify
		if latest := evidence.Latest; latest != nil && latest.Outcome != "success" {
			key, active := activeKeys[latest.KeyID]
			project := readyProjects[latest.ProjectID]
			if _, routed := readyRoutes[latest.RequestedModel]; active && key.ProjectID == latest.ProjectID && routed &&
				slices.Contains(project.AllowedModels, latest.RequestedModel) {
				result.State = onboardingVerifyFailed
				result.LastVerification = latestOnboardingVerification(evidence)
				result.Goals[3].DetailCode = "request_failed"
				result.Goals[3].ActionHref = onboardingRequestHref(latest.ProjectID, latest.RequestedModel)
			}
		}
	}

	currentAssigned := false
	for index := range result.Goals {
		if completed[index] {
			result.Goals[index].State = onboardingGoalComplete
			result.CompletedGoals++
			continue
		}
		if !currentAssigned {
			if index == 3 && result.State == onboardingVerifyFailed {
				result.Goals[index].State = onboardingGoalError
			} else {
				result.Goals[index].State = onboardingGoalCurrent
			}
			currentAssigned = true
		} else {
			result.Goals[index].State = onboardingGoalBlocked
		}
	}
	return result
}

func completedOnboarding(now time.Time) onboardingReadiness {
	// A successful Gateway request proves the historical milestone even after
	// the resources used in that request are edited or removed.
	return onboardingReadiness{
		Version: 1, State: onboardingFirstValueReached, CompletedGoals: 4, TotalGoals: 4, EvaluatedAt: now,
		Goals: []onboardingGoal{
			{Key: "connect_provider", State: onboardingGoalComplete, DetailCode: "request_proven", ActionHref: "/admin/providers"},
			{Key: "publish_model", State: onboardingGoalComplete, DetailCode: "request_proven", ActionHref: "/admin/routes"},
			{Key: "grant_access", State: onboardingGoalComplete, DetailCode: "request_proven", ActionHref: "/admin/projects"},
			{Key: "verify_request", State: onboardingGoalComplete, DetailCode: "request_succeeded", ActionHref: "/admin/developer"},
		},
	}
}

func providerTestIsCurrent(instance domain.ProviderInstance, runtimeID string) bool {
	if instance.LastTestRevision != instance.Revision {
		return false
	}
	return instance.EgressProxyID == "" || runtimeID != "" && instance.LastTestRuntimeID == runtimeID
}

func onboardingProbeRuntimeCurrent(provider domain.ProviderInstance, runtimeID, testedRuntimeID string) bool {
	if provider.EgressProxyID == "" {
		return testedRuntimeID == "" || testedRuntimeID == "direct"
	}
	return runtimeID != "" && testedRuntimeID == runtimeID
}

func latestOnboardingVerification(evidence usage.OnboardingEvidence) *onboardingVerification {
	request := evidence.Latest
	verification := &onboardingVerification{
		Outcome: request.Outcome, RequestID: request.RequestID, RequestedModel: request.RequestedModel,
		CompletedAt: request.CompletedAt,
	}
	if attempt := evidence.LatestAttempt; attempt != nil {
		verification.HTTPStatus = attempt.HTTPStatus
		verification.ErrorClass = attempt.ErrorClass
	}
	return verification
}

func onboardingConnectHref(detail string) string {
	if detail == "credential_missing" {
		return "/admin/providers?view=credentials&intent=create&onboarding=first-request"
	}
	if detail == "provider_missing" {
		return "/admin/providers?intent=create&onboarding=first-request"
	}
	if detail == "provider_egress_unavailable" {
		return "/admin/providers?view=proxies&onboarding=first-request"
	}
	return "/admin/providers?onboarding=first-request"
}

func onboardingPublishHref(detail string) string {
	if detail == "route_missing" {
		return "/admin/routes?intent=create&onboarding=first-request"
	}
	if detail == "route_disabled" || detail == "route_withheld" || detail == "route_test_required" || detail == "route_deployment_unavailable" {
		return "/admin/routes?onboarding=first-request"
	}
	if detail == "deployment_missing" {
		return "/admin/deployments?intent=create&onboarding=first-request"
	}
	return "/admin/deployments?onboarding=first-request"
}

func onboardingAccessHref(detail, keyProjectID string) string {
	href := "/admin/projects?onboarding=first-request"
	if detail == "project_missing" {
		href += "&intent=create"
	}
	if detail == "key_missing" && keyProjectID != "" {
		href += "&project_id=" + url.QueryEscape(keyProjectID)
	}
	return href
}

func onboardingRequestHref(projectID, model string) string {
	href := "/admin/developer?onboarding=first-request"
	if projectID != "" {
		href += "&project_id=" + url.QueryEscape(projectID) + "&model=" + url.QueryEscape(model)
	}
	return href
}
