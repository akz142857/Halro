package app

import (
	"fmt"
	"testing"

	"github.com/akz142857/Halro/internal/compatibility"
	anthropicwire "github.com/akz142857/Halro/internal/compatibility/anthropic"
	openaiwire "github.com/akz142857/Halro/internal/compatibility/openai"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/modelcatalog"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

// An endpoint that cannot render a content kind must not be served by a target
// that produces it unasked.
//
// This is the third step docs/contracts/adding-a-northbound-endpoint.md names as
// having no mechanical guard, and it is the one both of this repository's
// production incidents came from:
//
//   - DeepSeek, 2026-08-18. /v1/responses rejects the reasoning request field, so
//     a caller cannot ask for reasoning there. DeepSeek reasoned anyway. The
//     renderer could not represent the answer and the attempt ended
//     malformed_response, ambiguous, paid for.
//   - Kimi, 2026-09-01. The same chain on /v1/responses and /v1/messages, then
//     503 once enough failures marked the deployment unhealthy.
//
// Neither half is wrong on its own and nothing in the request says so, which is
// why both were found in production. The pairing is what this walks.
//
// Two things make it more than a restatement of a list. The endpoint's side is
// derived by execution — the actual northbound renderer is handed a result
// carrying a reasoning part, and whether it refuses is the answer — so an
// endpoint that gains or loses the ability is reclassified without anyone
// editing this file. And the provider's side is the model catalogue, which is
// keyed by profile and model together, the only granularity at which the fact is
// true.
func TestNoEndpointIsServedByATargetThatReasonsUnasked(t *testing.T) {
	// The residue, and the reason it is a list rather than a skip: each entry is
	// a request that reaches an upstream, is billed, and comes back as a 502.
	// Routing it away needs ReasonsUnasked to reach provider.Target, which means
	// threading it through the deployment capability snapshot — durable state, and
	// a separate piece of work. Until then this names exactly what is uncovered,
	// and the assertion is that the set does not grow.
	residue := map[string]string{
		"openai.responses.stateless.v1/kimi.chat.v1/kimi-k2.7-code":           "kimi-k2.7-code has no off switch at all: `invalid thinking: only type=enabled is allowed for this model`",
		"openai.responses.stateless.v1/kimi.chat.v1/kimi-k2.7-code-highspeed": "the same line, same measurement",
		"anthropic.messages.2023-06-01/kimi.chat.v1/kimi-k2.7-code":           "kimi-k2.7-code has no off switch at all",
		"anthropic.messages.2023-06-01/kimi.chat.v1/kimi-k2.7-code-highspeed": "the same line, same measurement",
	}

	unasked := map[domain.ProviderProfileID][]string{}
	for _, entry := range modelcatalog.Builtin().Entries() {
		if entry.ReasonsUnasked {
			unasked[entry.Key.Profile] = append(unasked[entry.Key.Profile], entry.Key.Model)
		}
	}
	if len(unasked) == 0 {
		t.Fatal("no catalogue entry reasons unasked, so this guard asserts nothing — the two incidents it is written from were both this")
	}

	// The provider side is asked first, and it is a separate question from the
	// endpoint's. A profile whose own result decoder refuses a reasoning part
	// fails before any northbound renderer is reached, so every endpoint is
	// affected rather than only the intolerant ones.
	//
	// The first version of this guard did not model that half, and a measurement
	// found the hole: MiniMax-M2.1 on minimax.responses.v1 returns a reasoning
	// output item that compatibility/openai.DecodeProviderResponse refuses, and
	// this walk would have reported /v1/chat/completions as fine because that
	// endpoint can render reasoning. It never gets the chance.
	for profileID := range unasked {
		if domain.IsWithheldProfile(profileID) || profileDecodesReasoning(t, profileID) {
			continue
		}
		for _, model := range unasked[profileID] {
			t.Errorf("%s/%s: this target reasons whatever the request says and the profile's own result decoder refuses a reasoning part, so every northbound endpoint answers 502 with the upstream already paid. A catalogue entry for it offers a deployment that fails every call.",
				profileID, model)
		}
	}

	seen := map[string]struct{}{}
	for _, manifest := range compatibility.BuiltinEndpointManifests() {
		if manifest.SemanticOperation != semantic.OperationGenerate {
			continue
		}
		renders := endpointRendersReasoning(t, manifest.ID)
		for _, coverage := range manifest.ProfileCoverage {
			// A profile this build does not offer cannot be reached, so it cannot
			// fail this way. Skipping it is also what ties a withholding to its
			// reason: offering kimi.responses.v1 again without finding an off
			// switch on that face brings its entries back into this walk and fails
			// here, rather than reaching an operator.
			if domain.IsWithheldProfile(coverage.ProfileID) {
				continue
			}
			for _, model := range unasked[coverage.ProfileID] {
				pair := fmt.Sprintf("%s/%s/%s", manifest.ID, coverage.ProfileID, model)
				seen[pair] = struct{}{}
				_, tolerated := residue[pair]
				switch {
				case renders && tolerated:
					t.Errorf("%s is listed as residue and this endpoint renders reasoning: remove it from the list", pair)
				case !renders && !tolerated:
					t.Errorf("%s: %s reasons whatever the request says and %s cannot render a reasoning part, so every call is billed upstream and answered 502. Either the target must be able to stop reasoning, or this endpoint must not be served by it.",
						pair, model, manifest.ID)
				}
			}
		}
	}
	for pair := range residue {
		if _, ok := seen[pair]; !ok {
			t.Errorf("%q is tolerated as residue and no longer exists: delete the entry so the list stays the truth about what is uncovered", pair)
		}
	}
}

// profileDecodesReasoning asks whether this provider profile can turn an
// upstream reasoning part into semantic content at all, which is the question
// that comes before "can the endpoint render it".
//
// Keyed by profile and checked for completeness rather than defaulted, for the
// reason the reasoning-probe ladder is: a default here is a guess, and the
// guessing direction that hides an incident is "it decodes fine". Only profiles
// carrying a target that reasons unasked need an entry, so the table stays as
// short as the problem is.
func profileDecodesReasoning(t *testing.T, profileID domain.ProviderProfileID) bool {
	t.Helper()
	switch profileID {
	case domain.ProfileKimiChat, domain.ProfileMiniMaxChat, domain.ProfileDeepSeekChat,
		domain.ProfileOpenAIChatEmbeddings, domain.ProfileAzureChatEmbeddings:
		// The Chat wire carries reasoning in its own member, and the decoder maps
		// it to a reasoning content part.
		finish := "stop"
		_, err := openaiwire.DecodeGenerateResult(openaiapi.ChatCompletionResponse{
			ID: "chatcmpl_1", Object: "chat.completion", Created: 1, Model: "m",
			Choices: []openaiapi.Choice{{
				Index: 0, FinishReason: &finish,
				Message: &openaiapi.Message{
					Role: "assistant", Content: openaiapi.TextContent("the answer"),
					ReasoningContent: "thinking out loud",
				},
			}},
		})
		return err == nil
	case domain.ProfileMiniMaxResponses, domain.ProfileKimiResponses,
		domain.ProfileOpenAIResponses, domain.ProfileBedrockMantleResponses,
		domain.ProfileBedrockMantleOpenAIResponses:
		// The Responses wire carries reasoning as an output item, and the
		// canonical mapper has no case for one. It refuses; it does not drop.
		_, err := openaiwire.DecodeProviderResponse(openaiapi.Response{
			ID: "resp_1", Model: "m", Status: "completed",
			Output: []openaiapi.ResponseOutputItem{
				{ID: "rs_1", Type: "reasoning", Status: "completed"},
				{ID: "msg_1", Type: "message", Status: "completed", Role: "assistant",
					Content: []openaiapi.ResponseOutputContent{{Type: "output_text", Text: "the answer"}}},
			},
		})
		return err == nil
	}
	t.Fatalf("a catalogue entry on %s reasons unasked and this guard does not know whether that profile's decoder can carry a reasoning part; add it to profileDecodesReasoning", profileID)
	return false
}

// endpointRendersReasoning asks the endpoint's own renderer, rather than a table
// in this file. A result carrying one reasoning part and one text part goes
// through the same function the gateway calls; refusing it is the endpoint
// saying it cannot carry the kind.
func endpointRendersReasoning(t *testing.T, endpointID string) bool {
	t.Helper()
	// Rendered twice, with the reasoning part and without it. Without the
	// control, "the renderer returned an error" also covers "the fixture in this
	// file is not a valid result", and the two are the difference between a guard
	// and a red light that is always on. The first draft of this had exactly that
	// bug: it reported the Chat face as unable to carry reasoning, which is the
	// one face that carries it.
	withReasoning := renderThroughEndpoint(t, endpointID, []semantic.Content{
		{Kind: semantic.ContentReasoning, Text: "thinking out loud"},
		{Kind: semantic.ContentText, Text: "the answer"},
	})
	control := renderThroughEndpoint(t, endpointID, []semantic.Content{
		{Kind: semantic.ContentText, Text: "the answer"},
	})
	if control != nil {
		t.Fatalf("%s refused a result carrying nothing but text, so this guard is measuring its own fixture rather than the endpoint: %v", endpointID, control)
	}
	return withReasoning == nil
}

func renderThroughEndpoint(t *testing.T, endpointID string, content []semantic.Content) error {
	t.Helper()
	result := semantic.GenerateResult{
		ID: "res_1", Model: "m", Created: 1,
		Translation: semantic.TranslationNone, MappingRevision: 1,
		Choices: []semantic.GenerateChoice{{
			Index: 0, Termination: "complete",
			Message: semantic.Message{Role: semantic.RoleAssistant, Content: content},
		}},
	}
	switch endpointID {
	case "openai.chat-completions.v1":
		_, err := openaiwire.RenderGenerateResult(result)
		return err
	case "openai.responses.stateless.v1":
		_, err := openaiwire.RenderResponseResult(result, openaiapi.ResponseRequest{Model: "m"})
		return err
	case "anthropic.messages.2023-06-01":
		_, err := anthropicwire.RenderResult(result, "public")
		return err
	case "anthropic.messages.count-tokens.2023-06-01":
		// Counting returns a token total and renders no content at all, so the
		// question does not arise. Named rather than defaulted: a generate-shaped
		// endpoint this function does not know must fail loudly, because guessing
		// "it renders" is the direction that hides the incident.
		return nil
	}
	t.Fatalf("endpoint %q renders generate results and this guard does not know how to ask it whether reasoning survives; add it to renderThroughEndpoint", endpointID)
	return nil
}
