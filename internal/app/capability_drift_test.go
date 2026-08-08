package app

import (
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/modelcatalog"
)

func catalogueDeployment(t *testing.T) (domain.Deployment, domain.ProviderProfileBinding) {
	t.Helper()
	binding := bedrockBinding("b-embed", domain.ProfileBedrockInvokeTitanEmbedV2)
	entry, ok := modelcatalog.Builtin().Lookup(modelcatalog.Key{
		ProviderType: domain.ProviderBedrock, Profile: binding.ProfileID, Model: titanEmbedModel,
	})
	if !ok {
		t.Fatal("seed entry missing")
	}
	deployment := domain.Deployment{
		ID: "dep", ProviderModel: titanEmbedModel, ProfileID: binding.ProfileID, BindingID: binding.ID,
		Capabilities: entry.Capabilities,
		ModelCapabilitySnapshot: domain.ModelCapabilitySnapshot{
			ProviderModel: titanEmbedModel, ModelRevision: entry.Revision(),
			Source: string(entry.Source), Status: string(entry.Status),
			CapturedAt: time.Now().UTC(), Capabilities: entry.Capabilities,
		},
	}
	return deployment, binding
}

func TestUnchangedCatalogAndProfileStaysCurrent(t *testing.T) {
	deployment, binding := catalogueDeployment(t)
	if state := evaluateCapabilityReview(deployment, binding, domain.ProviderBedrock); state != domain.CapabilityReviewCurrent {
		t.Fatalf("state=%q", state)
	}
	if !capabilityReviewAdmitsTraffic(domain.CapabilityReviewCurrent) {
		t.Fatal("a current deployment was refused traffic")
	}
}

// The case no catalog refresh reports: the binary narrowed the profile under a
// deployment that was saved when it was wider.
func TestProfileNarrowingIsDriftAndFailsClosed(t *testing.T) {
	deployment, binding := catalogueDeployment(t)
	binding.Capabilities = domain.ProviderCapabilities{} // the profile lost embeddings
	state := evaluateCapabilityReview(deployment, binding, domain.ProviderBedrock)
	if state != domain.CapabilityReviewDrifted {
		t.Fatalf("state=%q", state)
	}
	if capabilityReviewAdmitsTraffic(state) {
		t.Fatal("a drifted deployment was still admitted")
	}
}

func TestSnapshotClaimingMoreThanTheCatalogIsDrift(t *testing.T) {
	deployment, binding := catalogueDeployment(t)
	// The snapshot claims chat as well; the catalog establishes embeddings only.
	deployment.ModelCapabilitySnapshot.Capabilities.Chat = true
	deployment.ModelCapabilitySnapshot.ModelRevision = "sha256:older"
	binding.Capabilities.Chat = true // the profile allows it, so only the catalog objects
	if state := evaluateCapabilityReview(deployment, binding, domain.ProviderBedrock); state != domain.CapabilityReviewDrifted {
		t.Fatalf("state=%q", state)
	}
}

// A declaration the catalog has since started covering is reviewable, not
// drifted: nothing it claims has been contradicted.
func TestDeclaredModelTheCatalogNowCoversIsOfferedForReview(t *testing.T) {
	deployment, binding := catalogueDeployment(t)
	deployment.ModelCapabilitySnapshot.Source = string(modelcatalog.SourceOperatorDeclared)
	deployment.ModelCapabilitySnapshot.ModelRevision = "sha256:when-nothing-was-known"
	state := evaluateCapabilityReview(deployment, binding, domain.ProviderBedrock)
	if state != domain.CapabilityReviewAvailable {
		t.Fatalf("state=%q", state)
	}
	// Being offered more does not change what the deployment already does, so it
	// keeps serving until an operator accepts and retests.
	if !capabilityReviewAdmitsTraffic(state) {
		t.Fatal("review_available stopped traffic")
	}
}

func TestCatalogEntryDisappearingUnderABuiltinSnapshotIsDrift(t *testing.T) {
	deployment, binding := catalogueDeployment(t)
	deployment.ProviderModel = "amazon.titan-embed-text-v9:0" // no such entry
	deployment.ModelCapabilitySnapshot.ProviderModel = deployment.ProviderModel
	if state := evaluateCapabilityReview(deployment, binding, domain.ProviderBedrock); state != domain.CapabilityReviewDrifted {
		t.Fatalf("state=%q", state)
	}
}

// Doctor is where an operator finds out why a deployment stopped routing,
// without having to read a start-up failure.
func TestDoctorReportsDriftAsFailAndReviewAsWarn(t *testing.T) {
	deployment, binding := catalogueDeployment(t)
	instance := domain.ProviderInstance{
		ID: "prov", Type: domain.ProviderBedrock, Enabled: true,
		Bindings: []domain.ProviderProfileBinding{binding},
	}
	deployment.ProviderID = instance.ID
	// A distinctive ID: the default "dep" is a substring of "deployment(s)" and
	// would make the check below pass on the message's own wording.
	deployment.ID = "dpl-7f21c9"

	collect := func(providers []domain.ProviderInstance, deployments []domain.Deployment) (string, string) {
		var status, detail string
		checkDoctorCapabilityDrift(providers, deployments, func(name, gotStatus, gotDetail string) {
			if name == "capability_drift" {
				status, detail = gotStatus, gotDetail
			}
		})
		return status, detail
	}

	if status, _ := collect([]domain.ProviderInstance{instance}, []domain.Deployment{deployment}); status != "pass" {
		t.Fatalf("unchanged deployment status=%q", status)
	}

	narrowed := instance
	narrowed.Bindings = []domain.ProviderProfileBinding{{
		ID: binding.ID, ProfileID: binding.ProfileID, Capabilities: domain.ProviderCapabilities{}, Enabled: true,
	}}
	status, detail := collect([]domain.ProviderInstance{narrowed}, []domain.Deployment{deployment})
	if status != "fail" {
		t.Fatalf("drift status=%q", status)
	}
	if strings.Contains(detail, deployment.ID) {
		t.Fatalf("detail names a specific deployment: %q", detail)
	}

	declared := deployment
	declared.ModelCapabilitySnapshot.Source = string(modelcatalog.SourceOperatorDeclared)
	declared.ModelCapabilitySnapshot.ModelRevision = "sha256:before-the-catalog-knew"
	if status, _ := collect([]domain.ProviderInstance{instance}, []domain.Deployment{declared}); status != "warn" {
		t.Fatalf("review status=%q", status)
	}
}
