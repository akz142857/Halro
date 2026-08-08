package app

import (
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/usage"
)

func TestOnboardingRequiresOneUsableTopology(t *testing.T) {
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	resources := readyOnboardingResources(now)

	ready := evaluateOnboardingReadiness(now, resources, usage.Metrics{}, usage.Snapshot{})
	if ready.State != onboardingReadyToVerify || ready.CompletedGoals != 3 || ready.Goals[3].State != onboardingGoalCurrent {
		t.Fatalf("ready=%#v", ready)
	}

	// Every resource still exists, but the Project now grants a different public
	// model. Independent item counts would report 4/4; a topology check must not.
	resources.Projects[0].AllowedRoutes = []string{"another-model"}
	detached := evaluateOnboardingReadiness(now, resources, usage.Metrics{}, usage.Snapshot{})
	if detached.State != onboardingConfiguring || detached.CompletedGoals != 2 || detached.Goals[2].DetailCode != "project_route_unavailable" {
		t.Fatalf("detached=%#v", detached)
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
			result := evaluateOnboardingReadiness(now, resources, usage.Metrics{}, usage.Snapshot{})
			if result.CompletedGoals != 1 || result.Goals[1].State != onboardingGoalCurrent || result.Goals[1].DetailCode != test.detail {
				t.Fatalf("result=%#v", result)
			}
		})
	}
}

func TestOnboardingKeepsFailedVerificationRecoverable(t *testing.T) {
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	snapshot := usage.Snapshot{
		Requests: []usage.RequestSummary{{
			RequestID: "request_failed", RequestedModel: "halro-chat", Outcome: "provider_error", CompletedAt: now,
		}},
		Attempts: []usage.AttemptEvent{{
			RequestID: "request_failed", Status: "provider_error", HTTPStatus: 502, ErrorClass: "upstream_timeout", CompletedAt: now,
		}},
	}
	result := evaluateOnboardingReadiness(now, readyOnboardingResources(now), usage.Metrics{RequestsError: 1}, snapshot)
	if result.State != onboardingVerifyFailed || result.CompletedGoals != 3 || result.Goals[3].State != onboardingGoalError || result.LastVerification == nil {
		t.Fatalf("result=%#v", result)
	}
	if result.LastVerification.RequestID != "request_failed" || result.LastVerification.HTTPStatus != 502 || result.LastVerification.ErrorClass != "upstream_timeout" {
		t.Fatalf("verification=%#v", result.LastVerification)
	}
}

func TestOnboardingCompletesOnlyAfterSuccessfulFinalization(t *testing.T) {
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	result := evaluateOnboardingReadiness(now, readyOnboardingResources(now), usage.Metrics{RequestsSuccess: 1}, usage.Snapshot{})
	if result.State != onboardingFirstValueReached || result.CompletedGoals != 4 || result.Goals[3].State != onboardingGoalComplete {
		t.Fatalf("result=%#v", result)
	}
}

func TestOnboardingRejectsExpiredKey(t *testing.T) {
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	resources := readyOnboardingResources(now)
	expired := now.Add(-time.Minute)
	resources.Keys[0].ExpiresAt = &expired
	result := evaluateOnboardingReadiness(now, resources, usage.Metrics{}, usage.Snapshot{})
	if result.CompletedGoals != 2 || result.Goals[2].DetailCode != "key_missing" {
		t.Fatalf("result=%#v", result)
	}
}

func readyOnboardingResources(now time.Time) onboardingResources {
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
			LastTestStatus: domain.DeploymentTestHealthy, LastTestRevision: 1, Revision: 1,
		}},
		Routes: []domain.Route{{
			ID: "route_1", PublicModel: "halro-chat", DeploymentID: "deployment_1", Enabled: true,
			LastTestStatus: domain.DeploymentTestHealthy, LastTestRevision: 1, Revision: 1,
		}},
		Projects: []domain.Project{{
			ID: "project_1", Enabled: true, AllowedRoutes: []string{"halro-chat"},
		}},
		Keys: []domain.GatewayKey{{
			ID: "key_1", ProjectID: "project_1", Enabled: true, CreatedAt: now,
		}},
		PriceReady: map[string]bool{"deployment_1": true},
	}
}
