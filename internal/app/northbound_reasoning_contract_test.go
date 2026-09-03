package app

import (
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
// The router now refuses such a pairing before the reservation, reading
// compatibility.ReasoningAnswerSurvives. That is what makes these tests load
// bearing rather than documentary: the first proves the declared tables say what
// the real decoders and renderers do, and the second proves every target the
// catalogue marks is covered by them.
func TestReasoningReachabilityTablesMatchTheRealDecodersAndRenderers(t *testing.T) {
	// The endpoint half, derived by execution rather than restated: the real
	// northbound renderer is handed a result carrying a reasoning part and one
	// carrying only text, and the endpoint is intolerant when it refuses the
	// first and accepts the second.
	endpoints := 0
	for _, manifest := range compatibility.BuiltinEndpointManifests() {
		if manifest.SemanticOperation != semantic.OperationGenerate || !endpointReturnsContent(manifest.ID) {
			continue
		}
		northbound := compatibility.NorthboundProfileID(manifest.NorthboundProfile)
		renders := endpointRendersReasoning(t, manifest.ID)
		// Asked with a provider profile that loses nothing of its own, so the
		// endpoint half is what is under test.
		declared := compatibility.ReasoningAnswerSurvives(northbound, domain.ProfileOpenAIChatEmbeddings)
		if renders != declared {
			t.Errorf("%s renders a reasoning part = %v and ReasoningAnswerSurvives says %v: routing reads the table in internal/compatibility/reasoning_reachability.go, so the table is the half that is wrong",
				manifest.ID, renders, declared)
		}
		endpoints++
	}
	if endpoints == 0 {
		t.Fatal("no generate endpoint was checked, so this test asserts nothing")
	}

	// The provider half, the same way. Only profiles carrying a target the
	// catalogue marks need an entry, so the table stays as short as the problem
	// is; profileDecodesReasoning fails by name for one it does not know.
	for profileID := range unaskedReasoningEntries(t) {
		decodes := profileDecodesReasoning(t, profileID)
		// Asked on an endpoint that renders reasoning, so the provider half is
		// what is under test.
		declared := compatibility.ReasoningAnswerSurvives(compatibility.ProfileOpenAIChatCompletions, profileID)
		if decodes != declared {
			t.Errorf("%s decodes a reasoning part = %v and ReasoningAnswerSurvives says %v", profileID, decodes, declared)
		}
	}
}

// Every target the catalogue marks, against every endpoint that could serve it.
// A pairing that cannot carry the answer has to be declared unable, because that
// declaration is what the router acts on; one that can must not be, or a working
// deployment is routed away for nothing.
func TestEveryTargetThatReasonsUnaskedIsPairedWithEveryEndpoint(t *testing.T) {
	unasked := unaskedReasoningEntries(t)
	if len(unasked) == 0 {
		t.Fatal("no catalogue entry reasons unasked, so this guard asserts nothing — the two incidents it is written from were both this")
	}
	checked := 0
	for _, manifest := range compatibility.BuiltinEndpointManifests() {
		if manifest.SemanticOperation != semantic.OperationGenerate || !endpointReturnsContent(manifest.ID) {
			continue
		}
		northbound := compatibility.NorthboundProfileID(manifest.NorthboundProfile)
		renders := endpointRendersReasoning(t, manifest.ID)
		for _, coverage := range manifest.ProfileCoverage {
			models := unasked[coverage.ProfileID]
			if len(models) == 0 {
				continue
			}
			want := profileDecodesReasoning(t, coverage.ProfileID) && renders
			survives := compatibility.ReasoningAnswerSurvives(northbound, coverage.ProfileID)
			for _, model := range models {
				if survives != want {
					t.Errorf("%s/%s/%s: the answer survives = %v by execution and the routing tables say %v",
						manifest.ID, coverage.ProfileID, model, want, survives)
				}
				checked++
			}
		}
	}
	if checked == 0 {
		t.Fatal("no pairing was reached: every marked profile is absent from every endpoint's coverage, which cannot be right")
	}
}

// A withheld profile is skipped by the two walks above, and this is what keeps
// that from being a hole. kimi.responses.v1 is withheld because that face reasons
// on every model it serves and its own decoder refuses the answer; if it were
// offered again, every request through it would be routed away, and a connection
// that can never answer anything is not one an operator should be able to create.
func TestAWithheldProfileThatReasonsUnaskedStaysUnservable(t *testing.T) {
	found := false
	for _, entry := range modelcatalog.Builtin().Entries() {
		if !entry.ReasonsUnasked || !domain.IsWithheldProfile(entry.Key.Profile) {
			continue
		}
		found = true
		for _, northbound := range []compatibility.NorthboundProfileID{
			compatibility.ProfileOpenAIChatCompletions,
			compatibility.ProfileOpenAIResponses,
			compatibility.ProfileAnthropicMessages,
		} {
			if compatibility.ReasoningAnswerSurvives(northbound, entry.Key.Profile) {
				t.Errorf("%s/%s is withheld and would be servable on %s if offered again, so the withholding is carrying weight the tables should carry",
					entry.Key.Profile, entry.Key.Model, northbound)
			}
		}
	}
	if !found {
		t.Skip("no withheld profile carries a target that reasons unasked")
	}
}

func unaskedReasoningEntries(t *testing.T) map[domain.ProviderProfileID][]string {
	t.Helper()
	unasked := map[domain.ProviderProfileID][]string{}
	for _, entry := range modelcatalog.Builtin().Entries() {
		// A profile this build does not offer cannot be reached, so it cannot
		// fail this way.
		if entry.ReasonsUnasked && !domain.IsWithheldProfile(entry.Key.Profile) {
			unasked[entry.Key.Profile] = append(unasked[entry.Key.Profile], entry.Key.Model)
		}
	}
	return unasked
}

// profileDecodesReasoning asks whether this provider profile can turn an upstream
// reasoning part into semantic content at all, which is the question that comes
// before "can the endpoint render it".
//
// Keyed by profile and checked for completeness rather than defaulted, for the
// reason the reasoning-probe ladder is: a default here is a guess, and the
// guessing direction that hides an incident is "it decodes fine".
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

// endpointReturnsContent reports whether this endpoint's response carries model
// content at all.
//
// Token counting does not — it answers a number — so "which content kinds
// survive" does not arise for it, and it must not be allowed to answer the
// question either: it shares a northbound profile with /v1/messages, which does
// return content, and one profile can hold only one answer. Letting the counting
// endpoint reply "everything survives" would have overwritten the one that
// matters.
func endpointReturnsContent(endpointID string) bool {
	switch endpointID {
	case "anthropic.messages.count-tokens.2023-06-01":
		return false
	// The deferred lifecycle endpoints run no renderer. Retrieval replays bytes
	// that openai.responses.create.v1 rendered and sealed, so whether reasoning
	// survives was decided there; cancel and delete answer a status and carry no
	// model content at all. Asking them the question would be asking the create
	// endpoint the same question three more times under other names.
	case "openai.responses.get.v1", "openai.responses.cancel.v1", "openai.responses.delete.v1":
		return false
	}
	return true
}

// endpointRendersReasoning asks the endpoint's own renderer, rather than a table
// in this file.
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
	case "openai.responses.create.v1":
		_, err := openaiwire.RenderResponseResult(result, openaiapi.ResponseRequest{Model: "m"})
		return err
	case "anthropic.messages.2023-06-01":
		_, err := anthropicwire.RenderResult(result, "public")
		return err
	}
	t.Fatalf("endpoint %q renders generate results and this guard does not know how to ask it whether reasoning survives; add it to renderThroughEndpoint", endpointID)
	return nil
}

// TestNoEndpointIsServedByATargetThatReasonsUnasked is the guard four comments
// and three documents name as the thing that stops a target which reasons
// unasked from being served. It did not exist. Deleting a Withheld field was
// said to fail it; measured, that deletion left the two tests above at PASS and
// SKIP — the second skips precisely because the profile it guards is no longer
// withheld, so removing the withholding removes the check with it.
//
// What actually refused an un-withheld kimi.responses.v1 was the served-matrix
// golden and the write path's IsWithheldProfile. Both are real guards, and
// neither says anything about reasoning: they would have refused the change for
// being a matrix change, not for being unsafe. So the reason was carried by
// comments alone.
//
// This states it directly: a target the catalogue marks as reasoning unasked
// must not be reachable through an endpoint whose decoder cannot represent the
// reasoning it will return unasked. Reachable means the profile is offered —
// withholding is how such a target is kept out, and this is the assertion that
// makes withholding load-bearing rather than decorative.
func TestNoEndpointIsServedByATargetThatReasonsUnasked(t *testing.T) {
	marked := 0
	for _, entry := range modelcatalog.Builtin().Entries() {
		if !entry.ReasonsUnasked {
			continue
		}
		marked++
		if domain.IsWithheldProfile(entry.Key.Profile) {
			continue
		}
		if profileDecodesReasoning(t, entry.Key.Profile) {
			// The decoder can carry what this target returns unasked, so the
			// caller sees the reasoning rather than a 502 after a billed call.
			continue
		}
		t.Errorf(
			"%s/%s reasons unasked and its profile's decoder refuses a reasoning item, "+
				"yet the profile is offered: every request through it would be paid for upstream "+
				"and then fail. Withhold the profile, or teach the decoder to carry the item.",
			entry.Key.Profile, entry.Key.Model,
		)
	}
	if marked == 0 {
		t.Fatal("no catalogue entry is marked as reasoning unasked, so this guard asserts nothing")
	}
}
