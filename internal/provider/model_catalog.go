package provider

import (
	"encoding/json"
	"strings"

	"github.com/akz142857/Halro/internal/domain"
)

// OpenAIShapedModelCatalog is the `GET /v1/models` body OpenAI defined and every
// OpenAI-compatible upstream copies: an envelope naming a list, and entries
// carrying an identifier and an owner.
//
// It lives here rather than in provider/openai because two adapter packages read
// it. MiniMax serves this shape beside an Anthropic-wire generation route, so
// provider/anthropic reads it too — one wire format, one reader, because two
// would drift and only one of them would be corrected.
//
// Note what an entry does not carry: no context window, no output ceiling, no
// capability flags. A list of this shape answers who exists on the account and
// nothing about what they do, which is why a target built from it takes
// MetadataSourceNone and leaves capabilities to the model catalog.
type OpenAIShapedModelCatalog struct {
	Object string `json:"object"`
	Data   []struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	} `json:"data"`
}

// DecodeOpenAIShapedModelCatalog reads the body and reports whether it carried a
// list at all. An absent `data` is not an empty account: it means the reply did
// not come from a models endpoint, and callers use that to keep a proxy login
// page served over HTTPS from reading as a healthy provider. An empty `data` is
// a real answer — an account can be entitled to nothing.
func DecodeOpenAIShapedModelCatalog(payload []byte) (OpenAIShapedModelCatalog, bool, error) {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return OpenAIShapedModelCatalog{}, false, err
	}
	var catalog OpenAIShapedModelCatalog
	if err := json.Unmarshal(payload, &catalog); err != nil {
		return OpenAIShapedModelCatalog{}, false, err
	}
	return catalog, len(envelope.Data) > 0, nil
}

// InvocationTargets projects the list into descriptors.
//
// Availability is `available` because the account's own list is exactly the
// evidence that claim wants. Everything else stays deliberately empty:
// lifecycle is unknown because the list does not say what is retired, and the
// metadata source is none because there is no metadata — capability evidence
// comes from Halro's model catalog or from an operator, never from the fact that
// an identifier appeared in a list.
func (c OpenAIShapedModelCatalog) InvocationTargets(kind domain.DeploymentTargetKind, canonicalModelRef func(string) string) []domain.InvocationTargetDescriptor {
	targets := make([]domain.InvocationTargetDescriptor, 0, len(c.Data))
	for _, entry := range c.Data {
		id := strings.TrimSpace(entry.ID)
		if id == "" || len(id) > 512 {
			continue
		}
		targets = append(targets, domain.InvocationTargetDescriptor{
			TargetID: id, TargetKind: kind, DisplayName: id, OwnedBy: strings.TrimSpace(entry.OwnedBy),
			CanonicalModelRef: canonicalModelRef(id), Lifecycle: domain.TargetLifecycleUnknown,
			MetadataSource: domain.MetadataSourceNone, Availability: domain.AvailabilityAvailable,
		})
	}
	return targets
}
