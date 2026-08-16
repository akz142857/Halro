package app

import (
	"net/http"

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

type providerCapabilityView struct {
	Chat                  bool  `json:"chat"`
	Streaming             bool  `json:"streaming"`
	Embeddings            bool  `json:"embeddings"`
	Tools                 bool  `json:"tools"`
	Vision                bool  `json:"vision"`
	JSONMode              bool  `json:"json_mode"`
	DeveloperRole         bool  `json:"developer_role"`
	Reasoning             bool  `json:"reasoning"`
	StreamUsage           bool  `json:"stream_usage"`
	ProviderExecutedTools bool  `json:"provider_executed_tools"`
	Moderations           bool  `json:"moderations"`
	Images                bool  `json:"images"`
	Transcriptions        bool  `json:"transcriptions"`
	Speech                bool  `json:"speech"`
	Files                 bool  `json:"files"`
	Batches               bool  `json:"batches"`
	Rerank                bool  `json:"rerank"`
	AsyncGenerate         bool  `json:"async_generate"`
	MaxContextTokens      int64 `json:"max_context_tokens"`
	MaxOutputTokens       int64 `json:"max_output_tokens"`
}

func providerCapabilityViewOf(c domain.ProviderCapabilities) providerCapabilityView {
	return providerCapabilityView{
		Chat: c.Chat, Streaming: c.Streaming, Embeddings: c.Embeddings, Tools: c.Tools,
		Vision: c.Vision, JSONMode: c.JSONMode, DeveloperRole: c.DeveloperRole,
		Reasoning: c.Reasoning, StreamUsage: c.StreamUsage,
		ProviderExecutedTools: c.ProviderExecutedTools,
		Moderations:           c.Moderations, Images: c.Images, Transcriptions: c.Transcriptions,
		Speech: c.Speech, Files: c.Files, Batches: c.Batches, Rerank: c.Rerank,
		AsyncGenerate:    c.AsyncGenerate,
		MaxContextTokens: c.MaxContextTokens, MaxOutputTokens: c.MaxOutputTokens,
	}
}

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
	// CapabilityRequiresChat is the dependency the server enforces. Sending it
	// means a form can present the rule instead of discovering it on refusal.
	CapabilityRequiresChat []string           `json:"capability_requires_chat"`
	ProviderTypes          []providerTypeView `json:"provider_types"`
}

// The view is assembled per request rather than memoised. It is fifteen rows of
// compile-time data plus one config value on an endpoint a console reads once a
// session, and process-wide memoisation would tie every runtime in a test binary
// to whichever config happened to build it first.
func buildProviderProfilesView(region string) providerProfilesView {
	profilesByType := make(map[domain.ProviderType][]providerProfileView)
	for _, profile := range domain.AllProviderProfiles() {
		profilesByType[profile.Type] = append(profilesByType[profile.Type], providerProfileView{
			ID:               profile.ID,
			AccessSurface:    profile.AccessSurface,
			CredentialScheme: profile.CredentialScheme,
			DefaultBaseURL:   domain.ResolveBaseURL(profile.ID, region),
			Immutable:        profile.Immutable,
			Defaults:         providerCapabilityViewOf(profile.Defaults),
			Ceiling:          providerCapabilityViewOf(profile.Ceiling),
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
		CapabilityNames:        domain.CapabilityNames(),
		CapabilityRequiresChat: domain.CapabilityRequiresChat(),
		ProviderTypes:          types,
	}
}

func (r *Runtime) getAdminProviderProfiles(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, buildProviderProfilesView(r.config.Providers.Bedrock.Region))
}
