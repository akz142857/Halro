package compatibility

import (
	"encoding/json"
	"slices"

	"github.com/akz142857/Halro/internal/anthropicapi"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

// portableEffortLevels is what a portable request can actually ask an Anthropic
// target for: the values Anthropic accepts, minus the ones that cannot survive
// the OpenAI intermediate representation every portable request passes through.
// Anthropic's `max` is the whole difference, and it is why this is an
// intersection rather than either list on its own.
var portableEffortLevels = intersectSorted(anthropicapi.EffortLevels, openaiapi.ReasoningEffortLevels)

func intersectSorted(left, right []string) []string {
	result := make([]string, 0, len(left))
	for _, value := range left {
		if slices.Contains(right, value) {
			result = append(result, value)
		}
	}
	return result
}

// UnsupportedGenerateFields returns northbound fields that the selected
// provider profile cannot represent without silent semantic loss.
func UnsupportedGenerateFields(profileID domain.ProviderProfileID, request semantic.GenerateRequest) []string {
	var unsupported []string
	seen := map[string]struct{}{}
	add := func(condition bool, field string) {
		if _, exists := seen[field]; condition && !exists {
			unsupported = append(unsupported, field)
			seen[field] = struct{}{}
		}
	}
	switch profileID {
	case domain.ProfileGeminiText:
		add(hasNamedMessage(request), "messages[].name")
		add(request.Seed != nil, "seed")
		add(len(request.Tools) > 0, "tools")
		add(request.ToolChoice != nil, "tool_choice")
		add(request.ParallelTools != nil, "parallel_tool_calls")
		add(request.OutputFormat != nil, "response_format")
		add(request.ReasoningEffort != "", "reasoning_effort")
		add(request.EndUserRef != "", "user")
	case domain.ProfileBedrockConverseText:
		add(hasNamedMessage(request), "messages[].name")
		add(request.Candidates != nil && *request.Candidates > 1, "n")
		add(request.Seed != nil, "seed")
		add(len(request.Tools) > 0, "tools")
		add(request.ToolChoice != nil, "tool_choice")
		add(request.ParallelTools != nil, "parallel_tool_calls")
		add(request.OutputFormat != nil, "response_format")
		add(request.ReasoningEffort != "", "reasoning_effort")
		add(request.EndUserRef != "", "user")
	case domain.ProfileAnthropicMessages:
		add(hasNamedMessage(request), "messages[].name")
		add(hasDeveloperMessage(request), "messages[].role=developer")
		add(request.Candidates != nil && *request.Candidates > 1, "n")
		add(request.Seed != nil, "seed")
		// Structured output is supported, but only as a schema: Anthropic has no
		// counterpart to the schema-less json_object mode, and it has no relaxed
		// mode either — a schema it is given is enforced. Declaring the gaps
		// precisely lets routing pick another provider rather than failing the
		// request at render time.
		add(request.OutputFormat != nil && request.OutputFormat.Kind == semantic.OutputJSONObject, "response_format")
		add(request.OutputFormat != nil && request.OutputFormat.Kind == semantic.OutputJSONSchema && !request.OutputFormat.Strict, "response_format")
		// The ladder is bounded by the portable representation, not by Anthropic.
		// Every portable request reaches this profile through the OpenAI wire
		// form, whose reasoning_effort stops at xhigh, so declaring Anthropic's
		// own `max` as routable produced a request that passed capability
		// filtering and then failed while being rendered — after the budget
		// reservation, naming a field the caller never sent.
		add(request.ReasoningEffort != "" && !slices.Contains(portableEffortLevels, request.ReasoningEffort), "reasoning_effort")
		add(request.EndUserRef != "", "user")
	case domain.ProfileBedrockMantleAnthropicMessages:
		// The Mantle Beta profile shares this wire representation and could carry
		// output_config, but its capability ceiling is fixed by the build and
		// widening it is a separate contract review. Until that happens the
		// declared surface stays where it was.
		add(hasNamedMessage(request), "messages[].name")
		add(hasDeveloperMessage(request), "messages[].role=developer")
		add(request.Candidates != nil && *request.Candidates > 1, "n")
		add(request.Seed != nil, "seed")
		add(request.OutputFormat != nil, "response_format")
		add(request.ReasoningEffort != "", "reasoning_effort")
		add(request.EndUserRef != "", "user")
	case domain.ProfileBedrockMantleOpenAIResponses:
		// A Responses message item has no author name to put one in — the Name
		// field on the item carries a function's name, not a speaker's — so the
		// renderer drops it. Every other profile that cannot carry it says so;
		// this one did not, which made it the one branch that neither carried the
		// field nor declared the loss, and a multi-speaker conversation routed
		// here came back 200 with the speakers made indistinguishable.
		add(hasNamedMessage(request), "messages[].name")
		add(request.Candidates != nil && *request.Candidates > 1, "n")
		add(len(request.Stop) > 0, "stop")
		add(request.Seed != nil, "seed")
		add(request.Stream && len(request.Tools) > 0, "tools")
		add(request.ReasoningEffort != "", "reasoning_effort")
	case domain.ProfileOpenAIChatEmbeddings, domain.ProfileAzureChatEmbeddings, domain.ProfileDeepSeekChat, domain.ProfileOpenAICompatible, domain.ProfileBedrockMantleOpenAIChat:
		// These profiles use the OpenAI-compatible wire representation directly.
	default:
		// Legacy or extension adapters do not have profile-level proof for optional
		// fields. Permit only the portable text/model core and fail closed for
		// everything whose semantics an adapter could otherwise silently discard.
		add(hasNamedMessage(request), "messages[].name")
		add(hasDeveloperMessage(request), "messages[].role")
		add(hasNonTextContent(request), "messages[].content")
		// Scalar Chat Completions controls remain part of the legacy OpenAI-wire
		// adapter contract. Their conversion is declared (never lossless), while
		// richer optional semantics stay fail-closed until a profile is supplied.
		add(request.Seed != nil, "seed")
		add(len(request.Tools) > 0, "tools")
		add(request.ToolChoice != nil, "tool_choice")
		add(request.ParallelTools != nil, "parallel_tool_calls")
		add(request.OutputFormat != nil, "response_format")
		add(request.ReasoningEffort != "", "reasoning_effort")
		add(request.EndUserRef != "", "user")
		add(request.IncludeUsage, "stream_options")
	}
	return unsupported
}

func hasNamedMessage(request semantic.GenerateRequest) bool {
	for _, message := range request.Messages {
		if message.Name != "" {
			return true
		}
	}
	return false
}

func hasDeveloperMessage(request semantic.GenerateRequest) bool {
	for _, message := range request.Messages {
		if message.Role == semantic.RoleDeveloper {
			return true
		}
	}
	return false
}

func hasNonTextContent(request semantic.GenerateRequest) bool {
	for _, message := range request.Messages {
		for _, part := range message.Content {
			if part.Kind != semantic.ContentText {
				return true
			}
		}
	}
	return false
}

// UnsupportedEmbeddingFields returns northbound embedding fields which would
// otherwise be ignored by the selected provider profile.
func UnsupportedEmbeddingFields(profileID domain.ProviderProfileID, request semantic.EmbeddingRequest) []string {
	switch profileID {
	case domain.ProfileOpenAIChatEmbeddings, domain.ProfileAzureChatEmbeddings, domain.ProfileOpenAICompatible:
		return nil
	case domain.ProfileGeminiText:
		var unsupported []string
		if request.Encoding != "" && request.Encoding != "float" {
			unsupported = append(unsupported, "encoding_format")
		}
		if request.EndUserRef != "" {
			unsupported = append(unsupported, "user")
		}
		return unsupported
	case domain.ProfileBedrockInvokeTitanEmbedV2:
		var unsupported []string
		var input string
		if json.Unmarshal(request.Input, &input) != nil {
			unsupported = append(unsupported, "input")
		}
		if request.Encoding != "" && request.Encoding != "float" {
			unsupported = append(unsupported, "encoding_format")
		}
		if request.Dimensions != nil && *request.Dimensions != 256 && *request.Dimensions != 512 && *request.Dimensions != 1024 {
			unsupported = append(unsupported, "dimensions")
		}
		if request.EndUserRef != "" {
			unsupported = append(unsupported, "user")
		}
		return unsupported
	default:
		var unsupported []string
		// Encoding and dimensions are part of the legacy embedding wire contract;
		// the unknown conversion is declared by the legacy primitive.
		if request.EndUserRef != "" {
			unsupported = append(unsupported, "user")
		}
		return unsupported
	}
}
