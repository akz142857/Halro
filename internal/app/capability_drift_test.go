package app

import (
	"context"
	"net/http"
	"slices"
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

// Admitting traffic is an allowlist, so a state nobody has taught this function
// about does not serve. The three states that exist are decided above; this
// pins the default for the fourth, which is the one a later change introduces
// without remembering to come back here.
func TestAnUnrecognisedReviewStateIsNotAdmitted(t *testing.T) {
	if capabilityReviewAdmitsTraffic(domain.CapabilityReviewState("invented-later")) {
		t.Fatal("a review state this build does not recognise was admitted to routing")
	}
	if capabilityReviewAdmitsTraffic("") {
		t.Fatal("an empty review state was admitted to routing")
	}
}

// A deployment whose provider is gone is drifted, not current. The registry
// load used to skip the check when the provider was missing and admit the
// deployment, while the Admin and doctor surfaces called the same condition
// drifted — the route died later on a nil adapter, so nothing leaked, but by
// accident and through a different check.
func TestADeploymentWhoseProviderIsMissingIsDrifted(t *testing.T) {
	deployment, _ := catalogueDeployment(t)
	deployment.ProviderID = "provider_that_is_gone"

	review := reviewForDeployment(map[string]domain.ProviderInstance{}, deployment)

	if review.State != domain.CapabilityReviewDrifted {
		t.Fatalf("state=%q, want drifted", review.State)
	}
	if capabilityReviewAdmitsTraffic(review.State) {
		t.Fatal("a deployment with no provider was admitted to routing")
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

// declaredOpenAIDeployment is a model an operator declared before the catalog
// covered it. The OpenAI profile declares no token limits, so a declaration
// that leaves them open sits comfortably inside the profile — and stops being a
// subset of the catalog the moment an entry fills them in.
func declaredOpenAIDeployment() (domain.Deployment, domain.ProviderProfileBinding) {
	binding := openAIInstance().Bindings[0]
	declared := domain.ProviderCapabilities{Chat: true, Streaming: true, StreamUsage: true}
	return domain.Deployment{
		ID: "dep", ProviderModel: "gpt-4o", ProfileID: binding.ProfileID, BindingID: binding.ID,
		Capabilities: declared,
		ModelCapabilitySnapshot: domain.ModelCapabilitySnapshot{
			ProviderModel: "gpt-4o", ModelRevision: "sha256:before-the-catalog-covered-it",
			Source: string(modelcatalog.SourceOperatorDeclared), Status: string(modelcatalog.StatusPartial),
			CapturedAt: time.Now().UTC(), Capabilities: declared,
		},
	}, binding
}

// Growing the catalog must not silently stop traffic. An operator who declared
// a model before the catalog covered it stops being a subset of the entry on
// any point where the two disagree — token limits especially, since a
// declaration that left them open is not inside an entry that fills them in.
// That is a disagreement between two claims, not a capability that stopped
// being supported, and the create path lets exactly this declaration through:
// a deployment that can be created must not be withheld by the next restart.
func TestCatalogGrowingUnderADeclarationIsReviewableNotDrift(t *testing.T) {
	deployment, binding := declaredOpenAIDeployment()
	if domain.ProviderCapabilitiesSubset(deployment.ModelCapabilitySnapshot.Capabilities, catalogueEntryFor(t, deployment, binding).Capabilities) {
		t.Fatal("this test needs a snapshot the catalog entry does not cover")
	}

	review := reviewCapabilities(deployment, binding, domain.ProviderOpenAI)
	if review.State != domain.CapabilityReviewAvailable || review.Reason != reviewReasonCatalogDisagrees {
		t.Fatalf("state=%q reason=%q", review.State, review.Reason)
	}
	if !capabilityReviewAdmitsTraffic(review.State) {
		t.Fatal("a catalog entry arriving stopped traffic on a working deployment")
	}
}

// The same disagreement under a snapshot the catalog itself produced is drift:
// that snapshot rested on the catalog, and the basis is gone.
func TestCatalogNarrowingUnderItsOwnSnapshotIsStillDrift(t *testing.T) {
	deployment, binding := declaredOpenAIDeployment()
	deployment.ModelCapabilitySnapshot.Source = string(modelcatalog.SourceBuiltin)
	deployment.ModelCapabilitySnapshot.Status = string(modelcatalog.StatusKnown)

	review := reviewCapabilities(deployment, binding, domain.ProviderOpenAI)
	if review.State != domain.CapabilityReviewDrifted || review.Reason != reviewReasonCatalogNarrowed {
		t.Fatalf("state=%q reason=%q", review.State, review.Reason)
	}
	if capabilityReviewAdmitsTraffic(review.State) {
		t.Fatal("a drifted deployment was still admitted")
	}
}

func catalogueEntryFor(t *testing.T, deployment domain.Deployment, binding domain.ProviderProfileBinding) modelcatalog.Entry {
	t.Helper()
	entry, ok := modelcatalog.Builtin().Lookup(modelcatalog.Key{
		ProviderType: domain.ProviderOpenAI, Profile: binding.ProfileID, Model: deployment.ProviderModel,
	})
	if !ok {
		t.Fatalf("the catalog does not cover %q, so this test proves nothing", deployment.ProviderModel)
	}
	return entry
}

// Storing only the retained set makes "the operator turned this off" and
// "nothing ever established it" the same absence, and they call for opposite
// treatment. A capability that was switched off must stop being offered, or the
// console asks the same question after every catalog change until the answer
// changes — which is how an operator learns to click past the notice.
func TestACapabilityTheOperatorSwitchedOffIsNotOfferedAgain(t *testing.T) {
	deployment, binding := catalogueDeployment(t)
	// The catalog has moved on and now establishes more than this deployment
	// uses, which is what puts anything on offer at all.
	deployment.ModelCapabilitySnapshot.ModelRevision = "sha256:before-the-catalog-grew"
	deployment.Capabilities.Embeddings = false
	deployment.ModelCapabilitySnapshot.Capabilities.Embeddings = false

	offered := reviewCapabilities(deployment, binding, domain.ProviderBedrock)
	if !slices.Contains(offered.AvailableForReview, "embeddings") {
		t.Fatalf("nothing was offered, so this test cannot show it being withheld: %#v", offered)
	}

	// The same deployment, with the operator having answered.
	deployment.OperatorDisabled = []string{"embeddings"}
	declined := reviewCapabilities(deployment, binding, domain.ProviderBedrock)
	if slices.Contains(declined.AvailableForReview, "embeddings") {
		t.Fatalf("a capability the operator switched off was offered again: %#v", declined.AvailableForReview)
	}
	// Reported, not hidden: the decision stays visible and reversible.
	if !slices.Contains(declined.OperatorDisabled, "embeddings") {
		t.Fatalf("the operator's decision is invisible: %#v", declined)
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

// The end of the wire: switching a capability off through the Admin API has to
// leave a record saying so, or the review side has nothing to read.
func TestTurningACapabilityOffRecordsTheOperatorsDecision(t *testing.T) {
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	cookie, csrf := loginAdminForTest(t, runtime)

	current, err := runtime.store.GetDeployment(context.Background(), bootstrap.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	if !current.Capabilities.Streaming {
		t.Fatal("the bootstrap deployment has nothing to switch off")
	}
	if len(current.OperatorDisabled) != 0 {
		t.Fatalf("nothing was switched off yet: %#v", current.OperatorDisabled)
	}

	narrowed := current.Capabilities
	narrowed.Streaming = false
	narrowed.StreamUsage = false
	response := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
		"/admin/api/v1/deployments/"+current.ID, revisionETag(current.Revision), map[string]any{
			"name": current.Name, "provider_id": current.ProviderID, "provider_model": current.ProviderModel,
			"capabilities": narrowed, "max_concurrency": current.MaxConcurrency,
			"enabled": current.Enabled,
		})
	if response.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", response.Code, response.Body.String())
	}

	updated, err := runtime.store.GetDeployment(context.Background(), current.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(updated.OperatorDisabled, "streaming") {
		t.Fatalf("switching streaming off left no record: %#v", updated.OperatorDisabled)
	}
	if slices.Contains(updated.OperatorDisabled, "chat") {
		t.Fatalf("a capability still in use was recorded as switched off: %#v", updated.OperatorDisabled)
	}
	// And the snapshot carries evidence bounded by its own source.
	evidence := updated.ModelCapabilitySnapshot.Evidence
	if len(evidence) == 0 {
		t.Fatal("the snapshot carries no evidence")
	}
	maximum := domain.MaxEvidenceForCapabilitySource(updated.ModelCapabilitySnapshot.Source)
	for name, value := range evidence {
		if value != domain.EvidenceUnsupported && value != maximum {
			t.Fatalf("evidence for %q is %q, which source %q may not claim",
				name, value, updated.ModelCapabilitySnapshot.Source)
		}
	}
}
