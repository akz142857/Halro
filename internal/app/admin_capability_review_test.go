package app

import (
	"slices"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/modelcatalog"
)

func chatDeployment(id string, capabilities domain.ProviderCapabilities) domain.Deployment {
	return domain.Deployment{ID: id, ProviderID: "prov", Enabled: true, Capabilities: capabilities}
}

func enabledRoute(id, publicModel, deploymentID string) domain.Route {
	return domain.Route{ID: id, PublicModel: publicModel, DeploymentID: deploymentID, Enabled: true}
}

// The case the preflight exists for: the deployment is the only thing serving
// gpt-4o that can do tools, so turning tools off makes every tool request to
// gpt-4o start failing with unsupported_feature.
func TestPreflightReportsARouteLosingItsOnlyCandidate(t *testing.T) {
	target := chatDeployment("dpl-a", domain.ProviderCapabilities{Chat: true, Tools: true, Vision: true})
	proposed := domain.ProviderCapabilities{Chat: true, Vision: true}
	routes := []domain.Route{enabledRoute("rt-1", "gpt-4o", "dpl-a")}

	result := capabilityPreflight(target, proposed, routes, []domain.Deployment{target})

	if !slices.Equal(result.Removed, []string{"tools"}) {
		t.Fatalf("removed=%v", result.Removed)
	}
	if len(result.Added) != 0 {
		t.Fatalf("added=%v", result.Added)
	}
	if len(result.Routes) != 1 {
		t.Fatalf("routes=%v", result.Routes)
	}
	if !result.Routes[0].SoleCandidate || !result.Blocking {
		t.Fatalf("expected a blocking sole-candidate impact, got %+v", result.Routes[0])
	}
	if result.Routes[0].PublicModel != "gpt-4o" || result.Routes[0].Capability != "tools" {
		t.Fatalf("impact=%+v", result.Routes[0])
	}
}

// A second deployment on the same public model that still has the capability
// means the public model keeps answering, so the change is reported but not
// blocking.
func TestPreflightIsNotBlockingWhenAnotherCandidateStillHasTheCapability(t *testing.T) {
	target := chatDeployment("dpl-a", domain.ProviderCapabilities{Chat: true, Tools: true})
	sibling := chatDeployment("dpl-b", domain.ProviderCapabilities{Chat: true, Tools: true})
	routes := []domain.Route{
		enabledRoute("rt-1", "gpt-4o", "dpl-a"),
		enabledRoute("rt-2", "gpt-4o", "dpl-b"),
	}

	result := capabilityPreflight(target, domain.ProviderCapabilities{Chat: true}, routes,
		[]domain.Deployment{target, sibling})

	if result.Blocking {
		t.Fatal("a public model with a surviving candidate was reported as blocking")
	}
	if len(result.Routes) != 1 || result.Routes[0].SoleCandidate {
		t.Fatalf("routes=%+v", result.Routes)
	}
}

// A second route onto the same deployment is not an alternative candidate: the
// narrowing removes the capability from both at once.
func TestPreflightDoesNotCountTheSameDeploymentAsItsOwnAlternative(t *testing.T) {
	target := chatDeployment("dpl-a", domain.ProviderCapabilities{Chat: true, Tools: true})
	routes := []domain.Route{
		enabledRoute("rt-1", "gpt-4o", "dpl-a"),
		enabledRoute("rt-2", "gpt-4o", "dpl-a"),
	}

	result := capabilityPreflight(target, domain.ProviderCapabilities{Chat: true}, routes,
		[]domain.Deployment{target})

	if !result.Blocking {
		t.Fatal("two routes onto one deployment were treated as two candidates")
	}
	if len(result.Routes) != 2 {
		t.Fatalf("routes=%+v", result.Routes)
	}
	for _, impact := range result.Routes {
		if !impact.SoleCandidate {
			t.Fatalf("impact=%+v", impact)
		}
	}
}

// Disabled routes and disabled sibling deployments are not candidates, so
// neither may make a narrowing look safe.
func TestPreflightIgnoresDisabledRoutesAndDeployments(t *testing.T) {
	target := chatDeployment("dpl-a", domain.ProviderCapabilities{Chat: true, Tools: true})
	disabled := chatDeployment("dpl-b", domain.ProviderCapabilities{Chat: true, Tools: true})
	disabled.Enabled = false
	disabledRoute := enabledRoute("rt-3", "gpt-4o", "dpl-c")
	disabledRoute.Enabled = false
	routes := []domain.Route{
		enabledRoute("rt-1", "gpt-4o", "dpl-a"),
		enabledRoute("rt-2", "gpt-4o", "dpl-b"),
		disabledRoute,
	}

	result := capabilityPreflight(target, domain.ProviderCapabilities{Chat: true}, routes,
		[]domain.Deployment{target, disabled})

	if !result.Blocking {
		t.Fatal("a disabled candidate was counted as an alternative")
	}
}

// Widening is not a preflight concern: no candidate goes away, so there is
// nothing to confirm.
func TestPreflightOnWideningReportsNoAffectedRoutes(t *testing.T) {
	target := chatDeployment("dpl-a", domain.ProviderCapabilities{Chat: true})
	routes := []domain.Route{enabledRoute("rt-1", "gpt-4o", "dpl-a")}

	result := capabilityPreflight(target, domain.ProviderCapabilities{Chat: true, Tools: true}, routes,
		[]domain.Deployment{target})

	if result.Blocking || len(result.Routes) != 0 {
		t.Fatalf("result=%+v", result)
	}
	if !slices.Equal(result.Added, []string{"tools"}) {
		t.Fatalf("added=%v", result.Added)
	}
}

// Routes on a different public model are unaffected — a narrowing on gpt-4o's
// deployment says nothing about claude-sonnet's candidates.
func TestPreflightScopesAlternativesToTheSamePublicModel(t *testing.T) {
	target := chatDeployment("dpl-a", domain.ProviderCapabilities{Chat: true, Tools: true})
	other := chatDeployment("dpl-b", domain.ProviderCapabilities{Chat: true, Tools: true})
	routes := []domain.Route{
		enabledRoute("rt-1", "gpt-4o", "dpl-a"),
		enabledRoute("rt-2", "claude-sonnet", "dpl-b"),
	}

	result := capabilityPreflight(target, domain.ProviderCapabilities{Chat: true}, routes,
		[]domain.Deployment{target, other})

	if !result.Blocking {
		t.Fatal("a candidate on a different public model was treated as an alternative")
	}
	if len(result.Routes) != 1 || result.Routes[0].PublicModel != "gpt-4o" {
		t.Fatalf("routes=%+v", result.Routes)
	}
}

// The review the console reads must name what moved, not just that something
// did — an operator deciding whether to adopt needs the capability names.
func TestReviewNamesTheCapabilitiesThatMoved(t *testing.T) {
	deployment, binding := catalogueDeployment(t)
	binding.Capabilities = domain.ProviderCapabilities{} // the profile lost embeddings

	review := reviewCapabilities(deployment, binding, domain.ProviderBedrock)

	if review.State != domain.CapabilityReviewDrifted {
		t.Fatalf("state=%q", review.State)
	}
	if review.Reason != reviewReasonProfileNarrowed {
		t.Fatalf("reason=%q", review.Reason)
	}
	if !slices.Contains(review.NoLongerSupported, "embeddings") {
		t.Fatalf("no_longer_supported=%v", review.NoLongerSupported)
	}
}

// A declaration the catalog has since started covering should tell the operator
// which capabilities are now on offer.
func TestReviewOffersTheCapabilitiesTheCatalogNowEstablishes(t *testing.T) {
	deployment, binding := catalogueDeployment(t)
	deployment.ModelCapabilitySnapshot.Source = string(modelcatalog.SourceOperatorDeclared)
	deployment.ModelCapabilitySnapshot.ModelRevision = "sha256:when-nothing-was-known"
	// The operator declared no operation; the catalog establishes embeddings. The
	// token limits stay as declared — dropping those too would read as exceeding
	// the profile's bounded limits, which is a different state entirely.
	noOperation := domain.ProviderCapabilities{
		MaxContextTokens: deployment.Capabilities.MaxContextTokens,
		MaxOutputTokens:  deployment.Capabilities.MaxOutputTokens,
	}
	deployment.Capabilities = noOperation
	deployment.ModelCapabilitySnapshot.Capabilities = noOperation

	review := reviewCapabilities(deployment, binding, domain.ProviderBedrock)

	if review.State != domain.CapabilityReviewAvailable {
		t.Fatalf("state=%q", review.State)
	}
	if review.Reason != reviewReasonCatalogNowCovers {
		t.Fatalf("reason=%q", review.Reason)
	}
	if !slices.Contains(review.AvailableForReview, "embeddings") {
		t.Fatalf("available_for_review=%v", review.AvailableForReview)
	}
	// Offered, never taken: the review must not have changed what runs.
	if deployment.Capabilities.Embeddings {
		t.Fatal("the review enabled a capability")
	}
}

// A deployment whose provider is gone has nothing establishing its claims, and
// the fail-closed reading is the one that does not invent evidence.
func TestReviewOfADeploymentWithNoProviderIsDrifted(t *testing.T) {
	deployment, _ := catalogueDeployment(t)
	deployment.ProviderID = "prov-gone"

	review := reviewForDeployment(map[string]domain.ProviderInstance{}, deployment)

	if review.State != domain.CapabilityReviewDrifted {
		t.Fatalf("state=%q", review.State)
	}
}

// The console's state and the state that admits traffic have to come from the
// same comparison, or an operator sees "current" for a deployment the registry
// refuses to load.
func TestReadProjectionAgreesWithLoadTimeResolution(t *testing.T) {
	deployment, binding := catalogueDeployment(t)
	binding.Capabilities = domain.ProviderCapabilities{}
	instance := domain.ProviderInstance{
		ID: "prov", Type: domain.ProviderBedrock, Enabled: true,
		Bindings: []domain.ProviderProfileBinding{binding},
	}
	deployment.ProviderID = instance.ID

	projected := reviewForDeployment(map[string]domain.ProviderInstance{instance.ID: instance}, deployment)
	loadTime := evaluateCapabilityReview(deployment, deploymentBinding(instance, deployment), instance.Type)

	if projected.State != loadTime {
		t.Fatalf("console shows %q, load time resolves %q", projected.State, loadTime)
	}
}
