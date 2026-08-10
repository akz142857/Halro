package app

import (
	"slices"
	"strings"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/modelcatalog"
)

// capabilityReview is the full answer to "does this deployment's saved snapshot
// still describe something that is supported now" — the state, the two sides
// being compared, and which capability names moved.
//
// Nothing here is persisted. Both sides of the comparison can change without the
// deployment record being rewritten, so a stored answer would only ever record
// what was true at the last write.
type capabilityReview struct {
	State domain.CapabilityReviewState `json:"state"`

	// What the deployment saved.
	Source        string `json:"source"`
	Status        string `json:"status"`
	ModelRevision string `json:"model_revision"`

	// What the catalog says now. Empty when the catalog no longer covers the
	// model at all, which CatalogCovered distinguishes from "covers it and
	// establishes nothing".
	CatalogCovered       bool   `json:"catalog_covered"`
	CatalogSource        string `json:"catalog_source,omitempty"`
	CatalogStatus        string `json:"catalog_status,omitempty"`
	CatalogModelRevision string `json:"catalog_model_revision,omitempty"`

	// AvailableForReview names capabilities established now that the deployment
	// does not use. They are offered, never enabled — adopting one is an
	// operator action that advances the revision and makes the test stale.
	//
	// Anything the operator switched off is excluded. Re-offering it on every
	// catalog change would be asking the same question until the answer changed,
	// which is how an operator learns to click past the notice.
	AvailableForReview []string `json:"available_for_review,omitempty"`
	// OperatorDisabled names what is established now and switched off on
	// purpose. It is reported rather than hidden so the decision stays visible
	// and reversible, but it is never presented as something new to adopt.
	OperatorDisabled []string `json:"operator_disabled,omitempty"`
	// NoLongerSupported names capabilities the snapshot claims that neither the
	// running profile nor the catalog establishes any more.
	NoLongerSupported []string `json:"no_longer_supported,omitempty"`

	// Reason is a stable machine-readable cause for a non-current state, so the
	// console can explain the state without re-deriving it.
	Reason string `json:"reason,omitempty"`
}

const (
	reviewReasonProfileNarrowed    = "profile_narrowed"
	reviewReasonCatalogDropped     = "catalog_no_longer_covers_model"
	reviewReasonCatalogNarrowed    = "catalog_establishes_less"
	reviewReasonCatalogAdvanced    = "catalog_revision_advanced"
	reviewReasonCatalogNowCovers   = "catalog_now_covers_model"
	reviewReasonCatalogDisagrees   = "catalog_disagrees_with_declaration"
	reviewReasonCatalogUnavailable = "catalog_unavailable"
)

// reviewCapabilities compares a deployment's stored snapshot against what the
// catalog and the running profile say now.
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
func reviewCapabilities(deployment domain.Deployment, binding domain.ProviderProfileBinding, providerType domain.ProviderType) capabilityReview {
	return reviewCapabilitiesWithCatalog(deployment, binding, providerType, modelcatalog.Builtin())
}

func reviewCapabilitiesWithCatalog(deployment domain.Deployment, binding domain.ProviderProfileBinding, providerType domain.ProviderType, catalog *modelcatalog.Catalog) capabilityReview {
	return reviewCapabilitiesWithCatalogState(deployment, binding, providerType, catalog, false)
}

func reviewCapabilitiesWithCatalogState(deployment domain.Deployment, binding domain.ProviderProfileBinding, providerType domain.ProviderType, catalog *modelcatalog.Catalog, catalogUnavailable bool) capabilityReview {
	snapshot := deployment.ModelCapabilitySnapshot
	review := capabilityReview{
		State:         domain.CapabilityReviewCurrent,
		Source:        snapshot.Source,
		Status:        snapshot.Status,
		ModelRevision: snapshot.ModelRevision,
	}

	// The profile ceiling is the harder constraint: a snapshot that exceeds it
	// describes something this build can no longer do, whatever the catalog says.
	if !domain.ProviderCapabilitiesSubset(snapshot.Capabilities, binding.Capabilities) ||
		!domain.ProviderCapabilitiesSubset(deployment.Capabilities, binding.Capabilities) {
		review.State = domain.CapabilityReviewDrifted
		review.Reason = reviewReasonProfileNarrowed
		review.NoLongerSupported = modelcatalog.LostCapabilities(
			unionCapabilities(snapshot.Capabilities, deployment.Capabilities), binding.Capabilities)
		return review
	}
	if snapshot.Source == string(modelcatalog.SourceSignedCatalog) && catalogUnavailable {
		review.State = domain.CapabilityReviewCatalogUnavailable
		review.Reason = reviewReasonCatalogUnavailable
		return review
	}

	entry, found := catalog.Lookup(capabilityReviewCatalogKey(deployment, binding, providerType))
	if found {
		review.CatalogCovered = true
		review.CatalogSource = string(entry.Source)
		review.CatalogStatus = string(entry.Status)
	}
	review.CatalogModelRevision = entry.Revision()
	if entry.Revision() == snapshot.ModelRevision {
		return review
	}

	// The catalog moved. An operator declaration that the catalog has since
	// started covering is not drift — it is a claim that can now be checked
	// against evidence, which is a review.
	if !found {
		// The model left the catalog, or was never in it and the "nothing
		// established" digest changed shape. Nothing here establishes the
		// snapshot's claims any more.
		if snapshot.Source == string(modelcatalog.SourceBuiltin) || snapshot.Source == string(modelcatalog.SourceSignedCatalog) {
			review.State = domain.CapabilityReviewDrifted
			review.Reason = reviewReasonCatalogDropped
			review.NoLongerSupported = modelcatalog.LostCapabilities(snapshot.Capabilities, entry.Capabilities)
			return review
		}
		review.State = domain.CapabilityReviewAvailable
		review.Reason = reviewReasonCatalogAdvanced
		return review
	}
	if !domain.ProviderCapabilitiesSubset(snapshot.Capabilities, entry.Capabilities) {
		// The catalog now establishes less than the snapshot claims. Whether that
		// is drift depends on where the snapshot came from.
		//
		// A snapshot the catalog produced has lost its basis, so it is drift and
		// fails closed. A snapshot the operator declared never rested on the
		// catalog: the catalog growing to cover the model is a second opinion
		// arriving, not the operator's claim being withdrawn. Treating that as
		// drift would mean shipping a catalog entry silently stops traffic on
		// every deployment that declared more than the entry — and it would
		// contradict the create path, which lets an explicit declaration exceed
		// the catalog. A deployment that can be created must not be withheld by
		// the next restart.
		if snapshot.Source != string(modelcatalog.SourceOperatorDeclared) && snapshot.Source != string(modelcatalog.SourceVerifiedProbe) {
			review.State = domain.CapabilityReviewDrifted
			review.Reason = reviewReasonCatalogNarrowed
			review.NoLongerSupported = modelcatalog.LostCapabilities(snapshot.Capabilities, entry.Capabilities)
			return review
		}
		review.State = domain.CapabilityReviewAvailable
		review.Reason = reviewReasonCatalogDisagrees
		// What the operator claims and the catalog does not. It is named so the
		// disagreement is visible and reviewable, not so anything is turned off.
		review.NoLongerSupported = modelcatalog.LostCapabilities(deployment.Capabilities, entry.Capabilities)
		review.setOffered(deployment, modelcatalog.Clamp(entry.Capabilities, binding.Capabilities))
		return review
	}
	review.State = domain.CapabilityReviewAvailable
	review.Reason = reviewReasonCatalogAdvanced
	if snapshot.Source == string(modelcatalog.SourceOperatorDeclared) || snapshot.Source == string(modelcatalog.SourceVerifiedProbe) {
		review.Reason = reviewReasonCatalogNowCovers
	}
	review.setOffered(deployment, modelcatalog.Clamp(entry.Capabilities, binding.Capabilities))
	return review
}

// capabilityReviewCatalogKey reproduces the catalog scope used when the
// immutable snapshot was saved. Azure deployments and compatible endpoint
// aliases keep their invocation identity in ProviderModel while their reviewed
// capability identity lives in CanonicalModelRef; looking up the alias here
// would report review_available immediately even though no evidence changed.
func capabilityReviewCatalogKey(deployment domain.Deployment, binding domain.ProviderProfileBinding, providerType domain.ProviderType) modelcatalog.Key {
	key := modelcatalog.Key{
		ProviderType: providerType, Profile: binding.ProfileID,
		TargetKind: deployment.TargetKind, Model: deployment.ProviderModel, Region: deployment.Region,
	}
	canonical := strings.TrimSpace(deployment.ModelCapabilitySnapshot.CanonicalModelRef)
	if canonical == "" || canonical == deployment.ProviderModel {
		return key
	}
	canonicalProvider, canonicalProfile, ok := capabilityModelCatalogScope(providerType)
	if !ok {
		return key
	}
	key.ProviderType, key.Profile, key.Model, key.Region = canonicalProvider, canonicalProfile, canonical, ""
	key.TargetKind = domain.TargetModelID
	if canonicalProvider == domain.ProviderOpenAICompatible {
		key.TargetKind = domain.TargetCustomEndpointModel
	}
	return key
}

// setOffered splits what is established now and unused into the part worth
// offering and the part the operator already declined.
//
// The comparison is against what the deployment uses, not against its snapshot:
// the snapshot can hold capabilities that were never switched on, and those are
// equally available. What separates them from a genuine offer is the operator
// having said no, which is why that answer is stored rather than inferred from
// the difference between the two.
func (r *capabilityReview) setOffered(deployment domain.Deployment, established domain.ProviderCapabilities) {
	for _, name := range modelcatalog.GainedCapabilities(deployment.Capabilities, established) {
		if slices.Contains(deployment.OperatorDisabled, name) {
			r.OperatorDisabled = append(r.OperatorDisabled, name)
			continue
		}
		r.AvailableForReview = append(r.AvailableForReview, name)
	}
}

// evaluateCapabilityReview is the state alone, for callers that only need to
// decide whether a deployment may serve.
func evaluateCapabilityReview(deployment domain.Deployment, binding domain.ProviderProfileBinding, providerType domain.ProviderType) domain.CapabilityReviewState {
	return reviewCapabilities(deployment, binding, providerType).State
}

// capabilityReviewAdmitsTraffic reports whether a deployment in this state may
// serve. review_available is deliberately routable: the catalog offering more
// than a deployment uses changes nothing about what it already does.
//
// Written as an allowlist rather than `!= drifted`. Both spellings agree on the
// three states that exist, and they disagree about a fourth: a state added
// later would inherit "may serve" from the denylist without anyone choosing
// that. Admitting traffic is the decision that should have to be made
// explicitly, so a new state arrives non-routable and the compiler's silence
// is not mistaken for a judgement.
func capabilityReviewAdmitsTraffic(state domain.CapabilityReviewState) bool {
	switch state {
	case domain.CapabilityReviewCurrent, domain.CapabilityReviewAvailable, domain.CapabilityReviewCatalogUnavailable:
		return true
	default:
		return false
	}
}

// unionCapabilities is used only to report which names exceed a narrowed
// profile, where the snapshot and the deployment can each be the side that
// exceeds it.
func unionCapabilities(first, second domain.ProviderCapabilities) domain.ProviderCapabilities {
	return modelcatalog.Union(first, second)
}
