package compatibility

import (
	"errors"
	"slices"
)

type NorthboundProfileID string

const (
	ProfileOpenAIChatCompletions   NorthboundProfileID = "openai.chat-completions.v1"
	ProfileOpenAIEmbeddings        NorthboundProfileID = "openai.embeddings.v1"
	ProfileOpenAIResponses         NorthboundProfileID = "openai.responses.stateless.v1"
	ProfileAnthropicMessages       NorthboundProfileID = "anthropic.messages.2023-06-01"
	ProfileOpenAIMediaResources    NorthboundProfileID = "openai.media-resources.v1"
	ProfileHalroInferenceResources NorthboundProfileID = "halro.inference-resources.v1"
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

func BuiltinNorthboundProfile(id NorthboundProfileID) (NorthboundProfile, bool) {
	profiles := map[NorthboundProfileID]NorthboundProfile{
		ProfileOpenAIChatCompletions:   {ID: ProfileOpenAIChatCompletions, Revision: 1, Protocol: "openai", Methods: []string{"POST /v1/chat/completions"}},
		ProfileOpenAIEmbeddings:        {ID: ProfileOpenAIEmbeddings, Revision: 1, Protocol: "openai", Methods: []string{"POST /v1/embeddings"}},
		ProfileOpenAIResponses:         {ID: ProfileOpenAIResponses, Revision: 1, Protocol: "openai", Methods: []string{"POST /v1/responses"}},
		ProfileAnthropicMessages:       {ID: ProfileAnthropicMessages, Revision: 1, Protocol: "anthropic", Methods: []string{"POST /v1/messages"}},
		ProfileOpenAIMediaResources:    {ID: ProfileOpenAIMediaResources, Revision: 1, Protocol: "openai", Methods: []string{"POST /v1/moderations", "POST /v1/images/generations", "POST /v1/audio/transcriptions", "POST /v1/audio/speech", "POST /v1/files", "GET /v1/files/{id}", "GET /v1/files/{id}/content", "DELETE /v1/files/{id}", "POST /v1/batches", "GET /v1/batches/{id}", "POST /v1/batches/{id}/cancel"}},
		ProfileHalroInferenceResources: {ID: ProfileHalroInferenceResources, Revision: 1, Protocol: "halro", Methods: []string{"POST /v1/rerank", "POST /v1/async/invocations", "GET /v1/async/invocations/{id}", "POST /v1/async/invocations/{id}/cancel"}},
	}
	profile, ok := profiles[id]
	profile.Methods = slices.Clone(profile.Methods)
	return profile, ok
}
