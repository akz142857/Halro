package app

import (
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/modelcatalog"
)

// evaluateCapabilityReview compares a deployment's stored snapshot against what
// the catalog and the running profile say now.
//
// Two things can move underneath a saved deployment. The catalog can change,
// which is the case the per-model revision exists to detect. The binary can also
// narrow a profile, which no catalog refresh will tell us about — and if that is
// only noticed on the request path, an operator learns about it as production
// traffic failing rather than as a state they can see. Both are resolved here so
// they can be resolved at load time.
//
// The result never enables anything. Widening is offered for review; anything
// the snapshot claims beyond what is supported now is drift, and drift is
// fail-closed.
func evaluateCapabilityReview(deployment domain.Deployment, binding domain.ProviderProfileBinding, providerType domain.ProviderType) domain.CapabilityReviewState {
	snapshot := deployment.ModelCapabilitySnapshot

	// The profile ceiling is the harder constraint: a snapshot that exceeds it
	// describes something this build can no longer do, whatever the catalog says.
	if !domain.ProviderCapabilitiesSubset(snapshot.Capabilities, binding.Capabilities) ||
		!domain.ProviderCapabilitiesSubset(deployment.Capabilities, binding.Capabilities) {
		return domain.CapabilityReviewDrifted
	}

	entry, found := modelcatalog.Builtin().Lookup(modelcatalog.Key{
		ProviderType: providerType, Profile: binding.ProfileID,
		Model: deployment.ProviderModel, Region: deployment.Region,
	})
	if entry.Revision() == snapshot.ModelRevision {
		return domain.CapabilityReviewCurrent
	}

	// The catalog moved. An operator declaration that the catalog has since
	// started covering is not drift — it is a claim that can now be checked
	// against evidence, which is a review.
	if !found {
		// The model left the catalog, or was never in it and the "nothing
		// established" digest changed shape. Nothing here establishes the
		// snapshot's claims any more.
		if snapshot.Source == string(modelcatalog.SourceBuiltin) {
			return domain.CapabilityReviewDrifted
		}
		return domain.CapabilityReviewAvailable
	}
	if !domain.ProviderCapabilitiesSubset(snapshot.Capabilities, entry.Capabilities) {
		// The catalog now establishes less than the snapshot claims.
		return domain.CapabilityReviewDrifted
	}
	return domain.CapabilityReviewAvailable
}

// capabilityReviewAdmitsTraffic reports whether a deployment in this state may
// serve. review_available is deliberately routable: the catalog offering more
// than a deployment uses changes nothing about what it already does.
func capabilityReviewAdmitsTraffic(state domain.CapabilityReviewState) bool {
	return state != domain.CapabilityReviewDrifted
}
