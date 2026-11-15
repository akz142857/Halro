package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/usage"
)

func TestOnboardingProgressesFromConfigurationToFirstRequest(t *testing.T) {
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	fixture := readyOnboardingResources(now)
	var resources onboardingResources
	check := func(done int, detail, href string) {
		t.Helper()
		result := evaluateOnboardingReadiness(now, resources, usage.Metrics{}, usage.OnboardingEvidence{})
		goal := result.Goals[done]
		if result.CompletedGoals != done || goal.State != onboardingGoalCurrent || goal.DetailCode != detail || !strings.Contains(goal.ActionHref, href) {
			t.Fatalf("want %d %s (%s), got %#v", done, detail, href, result)
		}
		for _, blocked := range result.Goals[done+1:] {
			if blocked.State != onboardingGoalBlocked {
				t.Fatalf("prerequisite missing but goal isn't blocked: %#v", blocked)
			}
		}
		if done < 3 && result.Goals[3].DetailCode != "request_waiting_for_access" {
			t.Fatalf("request verification shown as ready before access: %#v", result.Goals[3])
		}
	}
	check(0, "credential_missing", "view=credentials&intent=create")
	resources.Credentials = fixture.Credentials
	check(0, "provider_missing", "/admin/providers?intent=create")
	resources.Providers = fixture.Providers
	resources.Providers[0].LastTestStatus = ""
	check(1, "deployment_missing", "/admin/deployments?intent=create")
	resources.Deployments = append([]domain.Deployment(nil), fixture.Deployments...)
	resources.Deployments[0].Enabled = false
	resources.Deployments[0].LastTestStatus = ""
	check(1, "deployment_price_missing", "/admin/deployments")
	resources.PriceReady = fixture.PriceReady
	check(1, "deployment_test_required", "/admin/deployments")
	resources.Deployments[0] = fixture.Deployments[0]
	resources.Deployments[0].Enabled = false
	check(1, "deployment_disabled", "/admin/deployments")
	resources.Deployments[0].Enabled = true
	check(1, "route_missing", "/admin/routes?intent=create")
	resources.Routes = append([]domain.Route(nil), fixture.Routes...)
	resources.Routes[0].Enabled = false
	check(1, "route_disabled", "/admin/routes")
	resources.Routes[0].Enabled = true
	resources.Routes[0].LastTestStatus = ""
	check(1, "route_test_required", "/admin/routes")
	resources.Routes[0] = fixture.Routes[0]
	check(2, "project_missing", "/admin/projects?onboarding=first-request&intent=create")
	resources.Projects = fixture.Projects
	check(2, "key_missing", "project_id=project_1")
	resources.Keys = fixture.Keys
	resources.Keys[0].Scopes = []domain.GatewayScope{domain.GatewayScopeGovernanceRead}
	check(2, "key_missing", "project_id=project_1")
	resources.Keys[0].Scopes = nil // Legacy keys default to inference.
	check(3, "request_ready", "/admin/developer?onboarding=first-request&project_id=project_1&model=halro-chat")
}

func TestOnboardingAPIWithMockProviderAndFinalizedGatewayRequest(t *testing.T) {
	runtime, bootstrap, _ := openBootstrappedRuntime(t)
	cookie, csrf := loginAdminForTest(t, runtime)
	instance, err := runtime.store.GetProvider(context.Background(), bootstrap.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &passingProbeAdapter{}
	manifest, ok := provider.BuiltinProfile(domain.ProfileOpenAIChatEmbeddings)
	if !ok {
		t.Fatal("OpenAI chat profile unavailable")
	}
	profiled, err := provider.NewLegacyAdapterBridge(adapter, manifest,
		domain.EvidenceForCapabilities(domain.ProviderCapabilities{Chat: true}, domain.EvidenceDeclared))
	if err != nil {
		t.Fatal(err)
	}
	installAdapter := func() {
		t.Helper()
		registry := provider.NewRegistry()
		for _, binding := range instance.EffectiveProfileBindings() {
			if err := registry.RegisterBindingAdapter(instance.ID, binding.ID, adapter); err != nil {
				t.Fatal(err)
			}
		}
		if err := registry.Register(provider.Target{
			ID: bootstrap.RouteID, DeploymentID: bootstrap.DeploymentID, ProviderID: bootstrap.ProviderID,
			PublicModel: "chat", ProviderModel: "gpt-test", Adapter: profiled, Strategy: "ordered",
			ProfileID: manifest.ID, AccessSurface: manifest.AccessSurface,
			Capabilities: provider.Capabilities{Chat: true},
		}); err != nil {
			t.Fatal(err)
		}
		for _, old := range runtime.providers.Replace(registry) {
			old.Close()
		}
	}
	check := func(done int, state onboardingState, detail string) {
		t.Helper()
		response := authenticatedAdminGet(t, runtime, cookie, "/admin/api/v1/onboarding/readiness")
		var readiness onboardingReadiness
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &readiness) != nil ||
			readiness.CompletedGoals != done || readiness.State != state || (done < 4 && readiness.Goals[done].DetailCode != detail) {
			t.Fatalf("want %d %s %s, got %d %s", done, state, detail, response.Code, response.Body)
		}
	}
	probe := func(path string) {
		t.Helper()
		installAdapter()
		response := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/"+path+"/test", "", nil)
		if response.Code != http.StatusOK {
			t.Fatalf("probe %s: status=%d body=%s", path, response.Code, response.Body)
		}
	}
	check(1, onboardingConfiguring, "deployment_test_required")
	probe("deployments/" + bootstrap.DeploymentID)
	check(1, onboardingConfiguring, "route_test_required")
	probe("routes/" + bootstrap.RouteID)
	check(3, onboardingReadyToVerify, "request_ready")
	installAdapter()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Authorization", "Bearer "+bootstrap.GatewayKey)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	runtime.gatewayRouter().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("gateway status=%d body=%s", response.Code, response.Body)
	}
	check(4, onboardingFirstValueReached, "request_succeeded")
}

func TestOnboardingRequiresOneUsableTopology(t *testing.T) {
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	resources := readyOnboardingResources(now)

	ready := evaluateOnboardingReadiness(now, resources, usage.Metrics{}, usage.OnboardingEvidence{})
	if ready.State != onboardingReadyToVerify || ready.CompletedGoals != 3 || ready.Goals[3].State != onboardingGoalCurrent {
		t.Fatalf("ready=%#v", ready)
	}

	// Every resource still exists, but the Project now grants a different public
	// model. Independent item counts would report 4/4; a topology check must not.
	resources.Projects[0].AllowedModels = []string{"another-model"}
	detached := evaluateOnboardingReadiness(now, resources, usage.Metrics{}, usage.OnboardingEvidence{})
	if detached.State != onboardingConfiguring || detached.CompletedGoals != 2 || detached.Goals[2].DetailCode != "project_route_unavailable" {
		t.Fatalf("detached=%#v", detached)
	}
}

func TestOnboardingRequiresAnAvailableRoute(t *testing.T) {
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	resources := readyOnboardingResources(now)
	resources.RouteWithholdings = map[string]routeWithholding{
		resources.Routes[0].ID: {Kind: routeWithheldCapabilityDrift},
	}

	result := evaluateOnboardingReadiness(now, resources, usage.Metrics{}, usage.OnboardingEvidence{})
	if result.CompletedGoals != 1 || result.Goals[1].DetailCode != "route_withheld" ||
		result.Goals[1].ActionHref != "/admin/routes?onboarding=first-request" {
		t.Fatalf("withheld route was counted as available: %#v", result)
	}
}

func TestOnboardingRejectsDeploymentThatIsNotCurrentlyUsable(t *testing.T) {
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*onboardingResources)
		detail string
	}{
		{name: "price missing", mutate: func(resources *onboardingResources) { resources.PriceReady["deployment_1"] = false }, detail: "deployment_price_missing"},
		{name: "test stale", mutate: func(resources *onboardingResources) {
			resources.Deployments[0].LastTestRevision = 1
			resources.Deployments[0].Revision = 2
		}, detail: "deployment_test_required"},
		{name: "disabled", mutate: func(resources *onboardingResources) { resources.Deployments[0].Enabled = false }, detail: "deployment_disabled"},
		{name: "quarantined", mutate: func(resources *onboardingResources) { resources.Deployments[0].PricingQuarantined = true }, detail: "deployment_price_missing"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resources := readyOnboardingResources(now)
			test.mutate(&resources)
			result := evaluateOnboardingReadiness(now, resources, usage.Metrics{}, usage.OnboardingEvidence{})
			if result.CompletedGoals != 1 || result.Goals[1].State != onboardingGoalCurrent || result.Goals[1].DetailCode != test.detail {
				t.Fatalf("result=%#v", result)
			}
		})
	}
}

// An operator who probed a deployment and a route but never opened the
// provider's own test was told the first goal was still ahead of them, and the
// panel read 0/4 for a chain that had already answered end to end. Both of
// those probes reach upstream through the provider, which is everything the
// provider's connection test proves and the model on top of it.
func TestOnboardingAcceptsADownstreamProbeAsProviderEvidence(t *testing.T) {
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	probed := now.Add(-time.Minute)
	for _, test := range []struct {
		name  string
		probe func(*onboardingResources)
	}{
		{name: "deployment probe", probe: func(resources *onboardingResources) {
			resources.Deployments[0].LastTestedAt = &probed
			resources.Routes[0].LastTestStatus = ""
			resources.Routes[0].LastTestRevision = 0
		}},
		{name: "route probe", probe: func(resources *onboardingResources) {
			resources.Routes[0].LastTestedAt = &probed
			resources.Deployments[0].LastTestStatus = ""
			resources.Deployments[0].LastTestRevision = 0
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			resources := readyOnboardingResources(now)
			resources.Providers[0].LastTestStatus = ""
			resources.Providers[0].LastTestRevision = 0
			resources.Providers[0].UpdatedAt = now.Add(-time.Hour)
			test.probe(&resources)

			result := evaluateOnboardingReadiness(now, resources, usage.Metrics{}, usage.OnboardingEvidence{})
			if result.Goals[0].State != onboardingGoalComplete || result.Goals[0].DetailCode != "provider_ready" {
				t.Fatalf("connect goal=%#v", result.Goals[0])
			}
		})
	}
}

func TestOnboardingRequiresCurrentRuntimeEvidenceForProxyProvider(t *testing.T) {
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	resources := readyOnboardingResources(now)
	resources.EgressRuntimeIDs = map[string]string{"corp-egress": "runtime_new"}
	resources.Providers[0].EgressProxyID = "corp-egress"
	resources.Providers[0].LastTestRuntimeID = "runtime_old"
	probed := now.Add(-time.Minute)
	resources.Deployments[0].LastTestedAt = &probed
	resources.Routes[0].LastTestedAt = &probed

	result := evaluateOnboardingReadiness(now, resources, usage.Metrics{}, usage.OnboardingEvidence{})
	if result.CompletedGoals != 1 || result.Goals[0].DetailCode != "provider_configured" || result.Goals[1].DetailCode != "deployment_test_required" {
		t.Fatalf("stale runtime proxy test passed readiness: %#v", result)
	}
	resources.Providers[0].LastTestRuntimeID = resources.EgressRuntimeIDs["corp-egress"]
	result = evaluateOnboardingReadiness(now, resources, usage.Metrics{}, usage.OnboardingEvidence{})
	if result.Goals[0].DetailCode != "provider_ready" || result.Goals[1].DetailCode != "deployment_test_required" {
		t.Fatalf("current provider test must not validate stale deployment and route probes: %#v", result)
	}
	resources.Deployments[0].LastTestRuntimeID = "runtime_new"
	result = evaluateOnboardingReadiness(now, resources, usage.Metrics{}, usage.OnboardingEvidence{})
	if result.Goals[1].DetailCode != "route_test_required" {
		t.Fatalf("current deployment test must not validate a stale route probe: %#v", result)
	}
	resources.Routes[0].LastTestRuntimeID = "runtime_new"
	resources.Providers[0].LastTestStatus = ""
	result = evaluateOnboardingReadiness(now, resources, usage.Metrics{}, usage.OnboardingEvidence{})
	if result.CompletedGoals != 3 || result.State != onboardingReadyToVerify {
		t.Fatalf("current downstream proxy probes should prove the chain: %#v", result)
	}
}

func TestOnboardingDoesNotCountInternalProxyAuthenticationAsProviderCredential(t *testing.T) {
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	result := evaluateOnboardingReadiness(now, onboardingResources{
		Credentials: []domain.Credential{{
			ID: "credential_proxy", Type: providerEgressCredentialType, Ciphertext: []byte("sealed"),
		}},
		PriceReady: map[string]bool{},
	}, usage.Metrics{}, usage.OnboardingEvidence{})
	if result.Goals[0].DetailCode != "credential_missing" {
		t.Fatalf("internal credential advanced onboarding: %#v", result.Goals[0])
	}
}

// The probe proves the provider as it stood when the probe ran. Editing the
// provider afterwards — a new base URL, a different credential — invalidates
// that evidence exactly the way it invalidates the provider's own test.
func TestOnboardingRejectsDownstreamProbeOlderThanTheProvider(t *testing.T) {
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	resources := readyOnboardingResources(now)
	resources.Providers[0].LastTestStatus = ""
	resources.Providers[0].LastTestRevision = 0
	resources.Providers[0].UpdatedAt = now
	probed := now.Add(-time.Minute)
	resources.Deployments[0].LastTestedAt = &probed
	resources.Routes[0].LastTestedAt = &probed

	result := evaluateOnboardingReadiness(now, resources, usage.Metrics{}, usage.OnboardingEvidence{})
	if result.CompletedGoals != 1 || result.Goals[0].DetailCode != "provider_configured" || result.Goals[1].DetailCode != "deployment_test_required" {
		t.Fatalf("result=%#v", result)
	}
}

// A probe that never recorded when it ran cannot be placed against the
// provider's last edit, so it is not evidence of anything current.
func TestOnboardingRejectsDownstreamProbeWithoutAnInstant(t *testing.T) {
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	resources := readyOnboardingResources(now)
	resources.Providers[0].LastTestStatus = ""
	resources.Providers[0].LastTestRevision = 0
	resources.Deployments[0].LastTestedAt = nil
	resources.Routes[0].LastTestedAt = nil

	result := evaluateOnboardingReadiness(now, resources, usage.Metrics{}, usage.OnboardingEvidence{})
	if result.CompletedGoals != 1 || result.Goals[0].DetailCode != "provider_configured" {
		t.Fatalf("result=%#v", result)
	}
}

func TestOnboardingKeepsFailedVerificationRecoverable(t *testing.T) {
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	resources := readyOnboardingResources(now)
	// The initial verification link selects the first usable project. If a
	// different project was used for the failed request, retry it there.
	resources.Projects = append([]domain.Project{{
		ID: "project_preferred", Enabled: true, AllowedModels: []string{"halro-chat"},
	}}, resources.Projects...)
	resources.Keys = append([]domain.GatewayKey{{
		ID: "key_preferred", ProjectID: "project_preferred", Enabled: true,
	}}, resources.Keys...)
	snapshot := usage.OnboardingEvidence{
		Latest: &usage.RequestSummary{
			RequestID: "request_failed", ProjectID: "project_1", KeyID: "key_1", RequestedModel: "halro-chat", Outcome: "provider_error", CompletedAt: now,
		},
		LatestAttempt: &usage.AttemptEvent{
			RequestID: "request_failed", Status: "provider_error", HTTPStatus: 502, ErrorClass: "upstream_timeout", CompletedAt: now,
		},
	}
	result := evaluateOnboardingReadiness(now, resources, usage.Metrics{RequestsError: 1}, snapshot)
	if result.State != onboardingVerifyFailed || result.CompletedGoals != 3 || result.Goals[3].State != onboardingGoalError || result.LastVerification == nil {
		t.Fatalf("result=%#v", result)
	}
	if result.Goals[3].ActionHref != "/admin/developer?onboarding=first-request&project_id=project_1&model=halro-chat" {
		t.Fatalf("retry targets the wrong project: %#v", result.Goals[3])
	}
	if result.LastVerification.RequestID != "request_failed" || result.LastVerification.HTTPStatus != 502 || result.LastVerification.ErrorClass != "upstream_timeout" {
		t.Fatalf("verification=%#v", result.LastVerification)
	}
	snapshot.Latest.RequestedModel = "unrelated-model"
	result = evaluateOnboardingReadiness(now, resources, usage.Metrics{RequestsError: 1}, snapshot)
	if result.State != onboardingReadyToVerify || result.LastVerification != nil {
		t.Fatalf("unrelated failure was shown as a verification attempt: %#v", result)
	}
}

func TestOnboardingCompletesOnlyAfterSuccessfulFinalization(t *testing.T) {
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	result := evaluateOnboardingReadiness(now, onboardingResources{}, usage.Metrics{RequestsSuccess: 1}, usage.OnboardingEvidence{})
	if result.State != onboardingFirstValueReached || result.CompletedGoals != 4 || result.Goals[3].State != onboardingGoalComplete {
		t.Fatalf("result=%#v", result)
	}
}

func TestOnboardingAcceptsSuccessfulRouteTrafficAsPublicationEvidence(t *testing.T) {
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	resources := readyOnboardingResources(now)
	resources.Providers[0].LastTestStatus = ""
	resources.Deployments[0].LastTestStatus = ""
	resources.Routes[0].LastTestStatus = ""
	resources.Providers[0].UpdatedAt = now.Add(-time.Hour)
	resources.Deployments[0].UpdatedAt = now.Add(-time.Hour)
	resources.Routes[0].UpdatedAt = now.Add(-time.Hour)
	snapshot := usage.OnboardingEvidence{
		Deployments: map[string]time.Time{"deployment_1": now},
		Routes:      map[string]time.Time{"route_1": now},
		Providers:   map[string]time.Time{"provider_1": now},
	}

	result := evaluateOnboardingReadiness(now, resources, usage.Metrics{}, snapshot)
	if result.State != onboardingReadyToVerify || result.CompletedGoals != 3 {
		t.Fatalf("successful route traffic was not accepted as readiness evidence: %#v", result)
	}
	for index, goal := range result.Goals[:3] {
		if goal.State != onboardingGoalComplete {
			t.Fatalf("goal %d=%#v", index, goal)
		}
	}
}

func TestOnboardingSuccessfulTrafficDoesNotSurviveALaterRouteEdit(t *testing.T) {
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	resources := readyOnboardingResources(now)
	resources.Routes[0].LastTestStatus = ""
	resources.Routes[0].UpdatedAt = now
	snapshot := usage.OnboardingEvidence{
		Deployments: map[string]time.Time{"deployment_1": now.Add(-time.Minute)},
		Routes:      map[string]time.Time{"route_1": now.Add(-time.Minute)},
		Providers:   map[string]time.Time{"provider_1": now.Add(-time.Minute)},
	}

	result := evaluateOnboardingReadiness(now, resources, usage.Metrics{}, snapshot)
	if result.Goals[1].DetailCode != "route_test_required" || result.Goals[1].State != onboardingGoalCurrent {
		t.Fatalf("old traffic survived a later route edit: %#v", result)
	}
}

func TestOnboardingRejectsExpiredKey(t *testing.T) {
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	resources := readyOnboardingResources(now)
	expired := now.Add(-time.Minute)
	resources.Keys[0].ExpiresAt = &expired
	result := evaluateOnboardingReadiness(now, resources, usage.Metrics{}, usage.OnboardingEvidence{})
	if result.CompletedGoals != 2 || result.Goals[2].DetailCode != "key_missing" {
		t.Fatalf("result=%#v", result)
	}
}

func readyOnboardingResources(now time.Time) onboardingResources {
	probed := now.Add(-time.Minute)
	return onboardingResources{
		Credentials: []domain.Credential{{
			ID: "credential_1", Type: domain.ProviderOpenAI, Audience: "https://api.openai.com:443:openai", Ciphertext: []byte("sealed"),
		}},
		Providers: []domain.ProviderInstance{{
			ID: "provider_1", Type: domain.ProviderOpenAI, BaseURL: "https://api.openai.com", CredentialID: "credential_1",
			Enabled: true, LastTestStatus: domain.DeploymentTestHealthy, LastTestRevision: 1, Revision: 1,
			ProfileID: domain.ProfileOpenAIChatEmbeddings, AccessSurface: domain.SurfaceOpenAI, CredentialScheme: domain.CredentialBearerStatic,
			Capabilities: domain.ProviderCapabilities{Chat: true},
		}},
		Deployments: []domain.Deployment{{
			ID: "deployment_1", ProviderID: "provider_1", Enabled: true,
			LastTestStatus: domain.DeploymentTestHealthy, LastTestRevision: 1, Revision: 1, LastTestedAt: &probed,
		}},
		Routes: []domain.Route{{
			ID: "route_1", PublicModel: "halro-chat", DeploymentID: "deployment_1", Enabled: true,
			LastTestStatus: domain.DeploymentTestHealthy, LastTestRevision: 1, Revision: 1,
		}},
		Projects: []domain.Project{{
			ID: "project_1", Enabled: true, AllowedModels: []string{"halro-chat"},
		}},
		Keys: []domain.GatewayKey{{
			ID: "key_1", ProjectID: "project_1", Enabled: true, CreatedAt: now,
		}},
		PriceReady: map[string]bool{"deployment_1": true},
	}
}
