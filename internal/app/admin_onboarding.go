package app

import (
	"net/http"
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
	Credentials []domain.Credential
	Providers   []domain.ProviderInstance
	Deployments []domain.Deployment
	Routes      []domain.Route
	Projects    []domain.Project
	Keys        []domain.GatewayKey
	PriceReady  map[string]bool
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
	resources := onboardingResources{EndpointPolicy: providerEndpointPolicy(r.config)}
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

	now := time.Now().UTC()
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

	writeJSON(writer, http.StatusOK, evaluateOnboardingReadiness(
		now, resources, r.usage.Metrics(), r.usage.Snapshot(),
	))
}

func evaluateOnboardingReadiness(now time.Time, resources onboardingResources, metrics usage.Metrics, snapshot usage.Snapshot) onboardingReadiness {
	credentialReady := make(map[string]domain.Credential)
	for _, credential := range resources.Credentials {
		if len(credential.Ciphertext) > 0 && string(credential.Type) != webhookCredentialType {
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
	deploymentProvider := make(map[string]string, len(resources.Deployments))
	downstreamProbe := make(map[string]time.Time)
	successfulDeployment := make(map[string]time.Time)
	successfulRoute := make(map[string]time.Time)
	recordProbe := func(providerID string, at *time.Time) {
		if providerID == "" || at == nil {
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
			recordProbe(deployment.ProviderID, deployment.LastTestedAt)
		}
	}
	for _, route := range resources.Routes {
		if route.DeletedAt != nil {
			continue
		}
		if route.LastTestStatus == domain.DeploymentTestHealthy && route.LastTestRevision == route.Revision {
			recordProbe(deploymentProvider[route.DeploymentID], route.LastTestedAt)
		}
	}
	// A finalized successful request is stronger evidence than an explicit
	// control-plane probe: it proves the selected Route, Deployment, Provider,
	// credential, Project grant, and Gateway Key worked together. Keep it tied
	// to the exact attempt IDs and revision timestamps so a later edit still
	// invalidates the evidence.
	successfulRequests := make(map[string]struct{})
	for _, request := range snapshot.Requests {
		if request.Outcome == "success" {
			successfulRequests[request.RequestID] = struct{}{}
		}
	}
	for _, attempt := range snapshot.Attempts {
		if attempt.Status != "success" {
			continue
		}
		if _, ok := successfulRequests[attempt.RequestID]; !ok {
			continue
		}
		if newest := successfulDeployment[attempt.DeploymentID]; attempt.DeploymentID != "" && attempt.CompletedAt.After(newest) {
			successfulDeployment[attempt.DeploymentID] = attempt.CompletedAt
		}
		if newest := successfulRoute[attempt.RouteID]; attempt.RouteID != "" && attempt.CompletedAt.After(newest) {
			successfulRoute[attempt.RouteID] = attempt.CompletedAt
		}
		at := attempt.CompletedAt
		recordProbe(attempt.ProviderID, &at)
	}

	hasProvider := false
	hasProviderCredential := false
	hasEnabledProvider := false
	hasProviderBinding := false
	readyProviders := make(map[string]domain.ProviderInstance)
	for _, provider := range resources.Providers {
		if provider.DeletedAt != nil {
			continue
		}
		hasProvider = true
		credential, ok := credentialReady[provider.CredentialID]
		if !ok || credential.Type != provider.Type {
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
		if provider.LastTestStatus == domain.DeploymentTestHealthy && provider.LastTestRevision == provider.Revision {
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
	case len(readyProviders) == 0:
		connectDetail = "provider_test_required"
	}
	connectReady := len(readyProviders) > 0

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
		provider, ok := readyProviders[deployment.ProviderID]
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
		provenAt, provenByTraffic := successfulDeployment[deployment.ID]
		if (deployment.LastTestStatus != domain.DeploymentTestHealthy || deployment.LastTestRevision != deployment.Revision) &&
			(!provenByTraffic || provenAt.Before(deployment.UpdatedAt)) {
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
	readyRoutes := make(map[string]domain.Route)
	for _, route := range resources.Routes {
		if route.DeletedAt != nil {
			continue
		}
		hasRoute = true
		if _, ok := readyDeployments[route.DeploymentID]; !ok {
			continue
		}
		hasLinkedRoute = true
		if !route.Enabled {
			continue
		}
		hasEnabledRoute = true
		provenAt, provenByTraffic := successfulRoute[route.ID]
		if route.LastTestStatus == domain.DeploymentTestHealthy && route.LastTestRevision == route.Revision ||
			provenByTraffic && !provenAt.Before(route.UpdatedAt) {
			readyRoutes[route.PublicModel] = route
		}
	}

	publishDetail := "model_ready"
	switch {
	case !connectReady:
		// The earlier step is what blocks this one, so say which of its two
		// shapes it is. "A provider exists but is unverified" and "there is no
		// provider yet" need different actions, and describing the second as
		// the first sends the operator looking for an object that is not there.
		publishDetail = connectDetail
		if connectDetail == "provider_test_required" {
			publishDetail = "provider_not_verified"
		}
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
	case len(readyRoutes) == 0:
		publishDetail = "route_test_required"
	}
	publishReady := len(readyRoutes) > 0

	hasProject := false
	hasEnabledProject := false
	hasAllowedRoute := false
	hasActiveKey := false
	readyProjectIDs := make(map[string]struct{})
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
		for _, alias := range project.AllowedRoutes {
			if _, ok := readyRoutes[alias]; ok {
				matchesRoute = true
				break
			}
		}
		if !matchesRoute {
			continue
		}
		hasAllowedRoute = true
		readyProjectIDs[project.ID] = struct{}{}
	}
	for _, key := range resources.Keys {
		if key.DeletedAt != nil || !key.Enabled || key.ExpiresAt != nil && !key.ExpiresAt.After(now) {
			continue
		}
		if _, ok := readyProjectIDs[key.ProjectID]; ok {
			hasActiveKey = true
			break
		}
	}

	accessDetail := "access_ready"
	switch {
	case !publishReady:
		// Same rule as above: propagate what the publish step actually found
		// unless it is the "exists but unverified" case, which is the only one
		// this step can describe in its own words.
		accessDetail = publishDetail
		if publishDetail == "deployment_test_required" || publishDetail == "route_test_required" {
			accessDetail = "model_not_verified"
		}
	case !hasProject:
		accessDetail = "project_missing"
	case !hasEnabledProject:
		accessDetail = "project_disabled"
	case !hasAllowedRoute:
		accessDetail = "project_route_unavailable"
	case !hasActiveKey:
		accessDetail = "key_missing"
	}
	accessReady := hasActiveKey

	result := onboardingReadiness{
		Version: 1, TotalGoals: 4, EvaluatedAt: now,
		Goals: []onboardingGoal{
			{Key: "connect_provider", DetailCode: connectDetail, ActionHref: onboardingConnectHref(connectDetail)},
			{Key: "publish_model", DetailCode: publishDetail, ActionHref: onboardingPublishHref(publishDetail)},
			{Key: "grant_access", DetailCode: accessDetail, ActionHref: onboardingAccessHref(accessDetail)},
			{Key: "verify_request", DetailCode: "request_ready", ActionHref: "/admin/developer?onboarding=first-request"},
		},
	}
	completed := []bool{connectReady, publishReady, accessReady, false}

	if metrics.RequestsSuccess > 0 {
		result.State = onboardingFirstValueReached
		completed[3] = true
		result.Goals[3].DetailCode = "request_succeeded"
	} else if accessReady {
		result.State = onboardingReadyToVerify
		if verification := latestOnboardingVerification(snapshot); verification != nil && verification.Outcome != "success" {
			result.State = onboardingVerifyFailed
			result.LastVerification = verification
			result.Goals[3].DetailCode = "request_failed"
		}
	} else {
		result.State = onboardingConfiguring
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

func latestOnboardingVerification(snapshot usage.Snapshot) *onboardingVerification {
	if len(snapshot.Requests) == 0 {
		return nil
	}
	request := snapshot.Requests[len(snapshot.Requests)-1]
	verification := &onboardingVerification{
		Outcome: request.Outcome, RequestID: request.RequestID, RequestedModel: request.RequestedModel,
		CompletedAt: request.CompletedAt,
	}
	for index := len(snapshot.Attempts) - 1; index >= 0; index-- {
		attempt := snapshot.Attempts[index]
		if attempt.RequestID == request.RequestID {
			verification.HTTPStatus = attempt.HTTPStatus
			verification.ErrorClass = attempt.ErrorClass
			break
		}
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
	return "/admin/providers?onboarding=first-request"
}

func onboardingPublishHref(detail string) string {
	if detail == "route_missing" {
		return "/admin/routes?intent=create&onboarding=first-request"
	}
	if detail == "route_disabled" || detail == "route_test_required" || detail == "route_deployment_unavailable" {
		return "/admin/routes?onboarding=first-request"
	}
	if detail == "deployment_missing" {
		return "/admin/deployments?intent=create&onboarding=first-request"
	}
	return "/admin/deployments?onboarding=first-request"
}

func onboardingAccessHref(detail string) string {
	href := "/admin/projects?onboarding=first-request"
	if detail == "project_missing" {
		href += "&intent=create"
	}
	return href
}
