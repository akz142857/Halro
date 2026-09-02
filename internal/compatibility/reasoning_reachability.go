package compatibility

import "github.com/akz142857/Halro/internal/domain"

// Whether a reasoning part an upstream produced can reach a caller.
//
// This exists because two production incidents came from a pairing nothing
// modelled: an endpoint that cannot represent a content kind, served by a target
// that produces it whether or not anyone asked. DeepSeek on 2026-08-18 and Kimi
// on 2026-09-01 were both that, and both surfaced as a 502 on a request the
// upstream had already been paid for. See
// docs/contracts/adding-a-northbound-endpoint.md.
//
// There are two places a reasoning part can be lost, and they are not
// interchangeable — the difference is how much is affected:
//
//   - The provider profile's own result decoder refuses it. Nothing reaches a
//     northbound renderer, so every endpoint fails.
//   - The northbound endpoint's renderer cannot represent it. Only that endpoint
//     fails, and the same target serves the others.
//
// Both lists name what is lost rather than what is carried. A profile added
// without an entry therefore reads as "carries reasoning", which is the wrong
// direction to guess in — so it is not left to a guess:
// TestNoEndpointIsServedByATargetThatReasonsUnasked drives the real decoder and
// the real renderer and requires both lists to match what they do.

// providerDecodeLosesReasoning names the provider profiles whose result decoder
// refuses a reasoning part outright.
//
// All of them are Responses-shaped, and they share one cause:
// compatibility/openai.DecodeProviderResponse has no case for a `reasoning`
// output item and returns an error for one. It does not drop it — three separate
// comments in this repository said "drops" and the whole argument for serving
// Kimi's Responses face rested on that wrong verb.
var providerDecodeLosesReasoning = map[domain.ProviderProfileID]bool{
	domain.ProfileOpenAIResponses:              true,
	domain.ProfileBedrockMantleResponses:       true,
	domain.ProfileBedrockMantleOpenAIResponses: true,
	domain.ProfileMiniMaxResponses:             true,
	domain.ProfileKimiResponses:                true,
}

// northboundCannotRenderReasoning names the API faces whose result renderer has
// no shape for a reasoning part.
//
// The Chat face is absent because it does carry one, in reasoning_content. That
// is why Kimi's k2.7-code line is served there and not on the other two.
var northboundCannotRenderReasoning = map[NorthboundProfileID]bool{
	ProfileOpenAIResponses:   true,
	ProfileAnthropicMessages: true,
}

// ReasoningAnswerSurvives reports whether a reasoning part produced by a target
// on this provider profile can be returned to a caller on this northbound
// endpoint.
//
// A northbound profile this build does not know is treated as unable to carry
// one. That is the fail-closed direction and it costs nothing today: every
// endpoint that serves generation is listed, and an unknown one is a caller
// reaching a face that does not exist.
//
// Native mode is not this function's question. There the caller's own bytes are
// forwarded and read back verbatim, so a thinking block survives a face that the
// portable mapper could not carry it through — which is why the Anthropic
// Messages profiles declare reasoning at all.
func ReasoningAnswerSurvives(northbound NorthboundProfileID, profile domain.ProviderProfileID) bool {
	if providerDecodeLosesReasoning[profile] {
		return false
	}
	if _, known := BuiltinNorthboundProfile(northbound); !known {
		return false
	}
	return !northboundCannotRenderReasoning[northbound]
}
