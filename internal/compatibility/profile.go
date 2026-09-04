package compatibility

import (
	"errors"
	"github.com/akz142857/Halro/internal/semantic"
	"slices"
)

type NorthboundProfileID string

const (
	ProfileOpenAIChatCompletions NorthboundProfileID = "openai.chat-completions.v1"
	ProfileOpenAIEmbeddings      NorthboundProfileID = "openai.embeddings.v1"
	// Not "openai.responses.v1": that string is already the identifier of the
	// OpenAI Responses *provider* profile (domain.ProfileOpenAIResponses), and
	// both names appear in one manifest document. "stateless" is what had to go
	// — the face now defers an answer to disk — and "deferrable" says which tier
	// it gained without claiming the request became stateful.
	ProfileOpenAIResponses         NorthboundProfileID = "openai.responses.deferrable.v1"
	ProfileAnthropicMessages       NorthboundProfileID = "anthropic.messages.2023-06-01"
	ProfileOpenAIMediaResources    NorthboundProfileID = "openai.media-resources.v1"
	ProfileHalroInferenceResources NorthboundProfileID = "halro.inference-resources.v1"
	ProfileHalroRunGovernance      NorthboundProfileID = "halro.run-governance.v1"
)

type NorthboundProfile struct {
	ID       NorthboundProfileID `json:"id"`
	Revision uint64              `json:"revision"`
	Protocol string              `json:"protocol"`
	Methods  []string            `json:"methods"`
}

func (profile NorthboundProfile) Validate() error {
	if profile.ID == "" || profile.Revision == 0 || profile.Protocol == "" || len(profile.Methods) == 0 {
		return errors.New("northbound profile is incomplete")
	}
	return nil
}

// builtinNorthboundProfiles is the table of API faces Halro serves, in the order
// they are served. It is a package-level list rather than a map built inside the
// lookup because a caller has to be able to walk it: the gateway route table is
// held to it, and a private list is one nothing can be told about — the same
// shape the provider profile table was fixed into.
var builtinNorthboundProfiles = []NorthboundProfile{
	{ID: ProfileOpenAIChatCompletions, Revision: 1, Protocol: "openai", Methods: []string{"POST /v1/chat/completions"}},
	{ID: ProfileOpenAIEmbeddings, Revision: 1, Protocol: "openai", Methods: []string{"POST /v1/embeddings"}},
	{ID: ProfileOpenAIResponses, Revision: 2, Protocol: "openai", Methods: []string{"POST /v1/responses", "GET /v1/responses/{id}", "POST /v1/responses/{id}/cancel", "DELETE /v1/responses/{id}"}},
	{ID: ProfileAnthropicMessages, Revision: 1, Protocol: "anthropic", Methods: []string{"POST /v1/messages", "POST /v1/messages/count_tokens"}},
	{ID: ProfileOpenAIMediaResources, Revision: 1, Protocol: "openai", Methods: []string{"POST /v1/moderations", "POST /v1/images/generations", "POST /v1/audio/transcriptions", "POST /v1/audio/speech", "POST /v1/files", "GET /v1/files/{id}", "GET /v1/files/{id}/content", "DELETE /v1/files/{id}", "POST /v1/batches", "GET /v1/batches/{id}", "POST /v1/batches/{id}/cancel"}},
	{ID: ProfileHalroInferenceResources, Revision: 1, Protocol: "halro", Methods: []string{"POST /v1/rerank", "POST /v1/async/invocations", "GET /v1/async/invocations/{id}", "POST /v1/async/invocations/{id}/cancel"}},
	{ID: ProfileHalroRunGovernance, Revision: 1, Protocol: "halro", Methods: []string{"POST /halro/v1/work-units", "GET /halro/v1/work-units/{id}", "POST /halro/v1/work-units/{id}/close", "POST /halro/v1/runs", "GET /halro/v1/runs/{id}", "POST /halro/v1/runs/{id}/close"}},
}

// SourceOf stamps a semantic operation with the northbound profile that
// produced it, at the revision the registry currently carries.
//
// The revision used to be written at each mapping as a literal, and one of them
// stayed at 1 through a bump to 2 — nothing failed, because no rule reads the
// revision yet. A rule that did would have branched on a number that was true
// of a different version of the endpoint. Reading it from the registry means
// the next bump cannot leave a mapping behind.
func SourceOf(id NorthboundProfileID) semantic.Source {
	source := semantic.Source{ProfileID: string(id), ProfileRevision: 1}
	if profile, found := BuiltinNorthboundProfile(id); found {
		source.ProfileRevision = profile.Revision
	}
	return source
}

func BuiltinNorthboundProfile(id NorthboundProfileID) (NorthboundProfile, bool) {
	for _, profile := range builtinNorthboundProfiles {
		if profile.ID == id {
			profile.Methods = slices.Clone(profile.Methods)
			return profile, true
		}
	}
	return NorthboundProfile{}, false
}

// AllNorthboundProfiles returns every API face this build serves, in table
// order. Callers that build something per endpoint — an invariant test, a route
// audit — walk this rather than keeping a list of their own.
func AllNorthboundProfiles() []NorthboundProfile {
	profiles := make([]NorthboundProfile, 0, len(builtinNorthboundProfiles))
	for _, profile := range builtinNorthboundProfiles {
		profile.Methods = slices.Clone(profile.Methods)
		profiles = append(profiles, profile)
	}
	return profiles
}
