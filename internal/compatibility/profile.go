package compatibility

import (
	"errors"
	"slices"
)

type NorthboundProfileID string

const (
	ProfileOpenAIChatCompletions NorthboundProfileID = "openai.chat-completions.v1"
	ProfileOpenAIEmbeddings      NorthboundProfileID = "openai.embeddings.v1"
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
		ProfileOpenAIChatCompletions: {ID: ProfileOpenAIChatCompletions, Revision: 1, Protocol: "openai", Methods: []string{"POST /v1/chat/completions"}},
		ProfileOpenAIEmbeddings:      {ID: ProfileOpenAIEmbeddings, Revision: 1, Protocol: "openai", Methods: []string{"POST /v1/embeddings"}},
	}
	profile, ok := profiles[id]
	profile.Methods = slices.Clone(profile.Methods)
	return profile, ok
}
