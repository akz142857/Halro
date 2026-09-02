package app

import (
	"net/http"

	"github.com/akz142857/Halro/internal/compatibility"
	"github.com/akz142857/Halro/internal/domain"
)

// The provider matrix, served to whoever builds a connection form.
//
// The console used to carry its own copy of this — which capabilities exist,
// what each provider starts with, what an operator may turn on, which endpoint
// to offer — kept in step with the server by a comment asking the next editor to
// remember. It drifted, and the two shapes of drift are both bad: a console
// wider than the server offers capabilities whose save is refused without saying
// which one, and a console narrower than the server hides capabilities that
// work.
//
// Serving it removes the second copy rather than synchronising it. What is sent
// is the domain table itself, walked through AllProviderProfiles so a profile
// added there cannot be missing here.

// The capability sets are the domain type itself. This endpoint's job is to
// publish the dictionary, so a projection with its own copy of every member
// was a second place to add a capability and a second place to forget one —
// and its json tags were already identical, which is what a copy that exists
// only to be kept identical looks like. The golden fixture is the review gate:
// a domain change that alters this answer shows up in its diff.
type providerCapabilityView = domain.ProviderCapabilities

type providerProfileView struct {
	ID               domain.ProviderProfileID `json:"id"`
	AccessSurface    domain.AccessSurface     `json:"access_surface"`
	CredentialScheme domain.CredentialScheme  `json:"credential_scheme"`
	// DefaultBaseURL is already resolved for this deployment: the region is
	// substituted, so a caller fills a form field with it and never learns that a
	// template existed.
	DefaultBaseURL string                 `json:"default_base_url"`
	Immutable      bool                   `json:"immutable"`
	Defaults       providerCapabilityView `json:"defaults"`
	Ceiling        providerCapabilityView `json:"ceiling"`
	// What a whole connection anchored on this profile may turn on, and what it
	// starts with. One connection can span several profiles — an OpenAI key
	// serves the chat endpoints and the media ones — and the rule for which
	// profile serves what lives in the domain table, so the answer is computed
	// there and sent, not recomputed by every caller from the per-profile sets
	// above. A capability several of the connection's profiles could serve is
	// absent from ConnectionCeiling on purpose: the save would be refused as
	// ambiguous, and a form must not offer a tick that cannot be saved.
	ConnectionCeiling  providerCapabilityView `json:"connection_ceiling"`
	ConnectionDefaults providerCapabilityView `json:"connection_defaults"`
	// CombinesWith names the other profiles a connection anchored here can carry,
	// which is what makes the two sets above readable: it says where a capability
	// the anchor does not serve would go.
	CombinesWith []domain.ProviderProfileID `json:"combines_with"`
	// RequestConstraints is the half of the capability model that routing applies
	// and nothing ever showed. A capability tick says what the profile can do; a
	// constraint says which member of a request it still cannot carry — Bedrock
	// reads an image and does not fetch one — and the Gateway refuses on it before
	// any provider call. Sending it means the form can state the rule where the
	// tick is made, instead of the operator meeting it as a refused request.
	RequestConstraints []compatibility.ProfileRequestConstraint `json:"request_constraints"`
}

type providerTypeView struct {
	Type             domain.ProviderType      `json:"type"`
	DefaultProfileID domain.ProviderProfileID `json:"default_profile_id"`
	Profiles         []providerProfileView    `json:"profiles"`
}

type providerProfilesView struct {
	// CapabilityNames is the key set, not a display order. How they are arranged
	// and what they are called in a given language stay with whoever renders
	// them.
	CapabilityNames []string `json:"capability_names"`
	// CapabilityDependencies is what each capability needs alongside it, as the
	// server enforces it. Sending it means a form can present the rule instead
	// of discovering it on refusal. Direct dependencies only: stream_usage names
	// streaming, and streaming names chat.
	CapabilityDependencies map[string][]string `json:"capability_dependencies"`
	// CapabilityOptInWarnings names the capabilities a form must say something
	// about before it is ticked, rather than rendering as one more checkbox. The
	// wording is the renderer's; the list is the server's, so a capability that
	// gains this property does not depend on the console being edited too.
	CapabilityOptInWarnings []string `json:"capability_opt_in_warnings"`
	// CapabilityModalities and NonModalCapabilities render Halro's capability
	// vocabulary as the input/output view a model catalogue uses. The mapping is
	// the server's for the same reason the dependencies above are: the console
	// is not the only thing that will ever want it, and a second copy in the
	// browser is a second thing to keep true.
	CapabilityModalities []domain.CapabilityModality `json:"capability_modalities"`
	NonModalCapabilities []string                    `json:"non_modal_capabilities"`
	ProviderTypes        []providerTypeView          `json:"provider_types"`
}

// The view is assembled per request rather than memoised. It is fifteen rows of
// compile-time data plus one config value on an endpoint a console reads once a
// session, and process-wide memoisation would tie every runtime in a test binary
// to whichever config happened to build it first.
func buildProviderProfilesView(region string) providerProfilesView {
	profilesByType := make(map[domain.ProviderType][]providerProfileView)
	for _, profile := range domain.AllProviderProfiles() {
		// A withheld profile is refused on every write, so offering it here would
		// be the first shape of drift this endpoint exists to prevent: a form
		// wider than the server, whose save is rejected without saying which
		// field caused it.
		if profile.Withheld {
			continue
		}
		// A withheld peer is dropped for the same reason the withheld profile
		// itself is: this list tells an operator which other implementations one
		// credential opens, and the write path refuses to bind a withheld one. A
		// group whose members are all offered is unaffected; kimi.responses.v1 is
		// the first profile withheld out of the middle of an offered group, and
		// listing it here would have promised a Kimi credential coverage the save
		// then rejects.
		combines := make([]domain.ProviderProfileID, 0)
		for _, peer := range domain.ConnectionProfiles(profile.Type, profile.ID)[1:] {
			if peer.Withheld {
				continue
			}
			combines = append(combines, peer.ID)
		}
		profilesByType[profile.Type] = append(profilesByType[profile.Type], providerProfileView{
			ID:                 profile.ID,
			AccessSurface:      profile.AccessSurface,
			CredentialScheme:   profile.CredentialScheme,
			DefaultBaseURL:     domain.ResolveBaseURL(profile.ID, region),
			Immutable:          profile.Immutable,
			Defaults:           profile.Defaults,
			Ceiling:            profile.Ceiling,
			ConnectionCeiling:  domain.ConnectionCeiling(profile.Type, profile.ID),
			ConnectionDefaults: domain.ConnectionDefaults(profile.Type, profile.ID),
			CombinesWith:       combines,
			RequestConstraints: compatibility.ProfileRequestConstraints(profile.ID),
		})
	}
	types := make([]providerTypeView, 0, len(domain.AllProviderTypes()))
	for _, providerType := range domain.AllProviderTypes() {
		defaultProfile, _ := domain.DefaultProviderProfile(providerType)
		types = append(types, providerTypeView{
			Type:             providerType,
			DefaultProfileID: defaultProfile.ProfileID,
			Profiles:         profilesByType[providerType],
		})
	}
	return providerProfilesView{
		CapabilityNames:         domain.CapabilityNames(),
		CapabilityDependencies:  domain.CapabilityDependencies(),
		CapabilityOptInWarnings: domain.CapabilityOptInWarnings(),
		CapabilityModalities:    domain.CapabilityModalities(),
		NonModalCapabilities:    domain.NonModalCapabilities(),
		ProviderTypes:           types,
	}
}

func (r *Runtime) getAdminProviderProfiles(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, buildProviderProfilesView(r.config.Providers.Bedrock.Region))
}
