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
		add(hasFailedToolResult(request), "messages[].content[].is_error")
		add(request.Seed != nil, "seed")
		add(len(request.Tools) > 0, "tools")
		add(request.ToolChoice != nil, "tool_choice")
		add(request.ParallelTools != nil, "parallel_tool_calls")
		add(request.OutputFormat != nil, "response_format")
		add(request.ReasoningEffort != "", "reasoning_effort")
		add(request.EndUserRef != "", "user")
	case domain.ProfileBedrockConverseText:
		add(hasNamedMessage(request), "messages[].name")
		add(hasFailedToolResult(request), "messages[].content[].is_error")
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
		add(hasFailedToolResult(request), "messages[].content[].is_error")
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
	case domain.ProfileDeepSeekChat:
		// DeepSeek speaks this wire format but accepts a smaller set of members
		// than OpenAI does; deepseek.go holds the list and the renderer that has
		// to agree with it. Sharing OpenAI's branch declared none of the gap, so
		// n, seed and parallel_tool_calls were sent to a surface that ignores
		// them and the caller got a 200 for a request that never happened as
		// written.
		add(hasFailedToolResult(request), "messages[].content[].is_error")
		add(request.Candidates != nil && *request.Candidates > 1, "n")
		add(request.Seed != nil, "seed")
		// Only the disable is a loss, and the guard is not cosmetic. Every
		// portable Messages request that names a tool_choice comes out of
		// DecodeToolChoice with this flag set — it always returns one, defaulting
		// to parallel-allowed — so declaring on presence routed DeepSeek away from
		// requests that expressed no preference at all. Parallel-allowed is what
		// omitting the member already gets, the way n=1 is; asking for tools to be
		// run one at a time is the thing DeepSeek has no member for.
		add(request.ParallelTools != nil && !*request.ParallelTools, "parallel_tool_calls")
		// Value-dependent, and the value is another field: a completion budget
		// counts reasoning tokens and DeepSeek's max_tokens does not, so the two
		// are the same bound exactly while thinking is off. Declaring it
		// unconditionally routed away every /v1/responses request that budgeted
		// its output — that endpoint rejects the `reasoning` field outright, so
		// its requests never think, and the limit it decodes is this one.
		// Carrying it when thinking is on would be the opposite mistake: a bound
		// the caller set over answer-plus-reasoning would silently become a bound
		// over the answer alone.
		//
		// Two output limits in one request stay declared, because DeepSeek has
		// one member for them and choosing between them is not this layer's call.
		add(request.CompletionTokenLimit != nil &&
			(deepSeekThinkingIsOn(request.ReasoningEffort) || request.VisibleOutputTokenLimit != nil),
			"max_completion_tokens")
		// DeepSeek has json_object and no schema mode, so support is value-
		// dependent the way it is on the Anthropic profile: a schema request is
		// routed away rather than sent to a surface that would refuse it after
		// the budget was already reserved.
		add(request.OutputFormat != nil && request.OutputFormat.Kind == semantic.OutputJSONSchema, "response_format")
		// Value-dependent too. `none` reaches thinking.type=disabled and low/high
		// reach thinking.reasoning_effort; minimal, medium and xhigh have no
		// DeepSeek rung and are not rounded to a neighbouring one.
		add(request.ReasoningEffort != "" && !slices.Contains(deepSeekPortableEfforts, request.ReasoningEffort), "reasoning_effort")
		// `user` is absent on purpose. DeepSeek carries the same concept as
		// user_id, so it is renamed by the renderer rather than declared lost.
	case domain.ProfileOpenAIChatEmbeddings, domain.ProfileAzureChatEmbeddings, domain.ProfileOpenAICompatible, domain.ProfileBedrockMantleOpenAIChat:
		// These profiles use the OpenAI-compatible wire representation directly.
		// The one thing that representation has no place for is a tool result the
		// caller marked as failed: an OpenAI tool message is its text and nothing
		// else, so is_error would be dropped and the model would read a failure as
		// a successful answer.
		add(hasFailedToolResult(request), "messages[].content[].is_error")
	default:
		// Legacy or extension adapters do not have profile-level proof for optional
		// fields. Permit only the portable text/model core and fail closed for
		// everything whose semantics an adapter could otherwise silently discard.
		add(hasNamedMessage(request), "messages[].name")
		add(hasDeveloperMessage(request), "messages[].role")
		add(hasNonTextContent(request), "messages[].content")
		add(hasFailedToolResult(request), "messages[].content[].is_error")
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

// hasFailedToolResult reports a tool result the caller marked as failed. Only
// the Anthropic-wire profiles can carry it; everywhere else the flag would be
// dropped and the model would be told a failed tool call succeeded.
func hasFailedToolResult(request semantic.GenerateRequest) bool {
	for _, message := range request.Messages {
		for _, part := range message.Content {
			if part.Kind == semantic.ContentToolResult && part.ToolError {
				return true
			}
		}
	}
	return false
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
