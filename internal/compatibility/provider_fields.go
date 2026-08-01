package compatibility

import (
	"encoding/json"

	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/semantic"
)

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
	case domain.ProfileAnthropicMessages, domain.ProfileBedrockMantleAnthropicMessages:
		add(hasNamedMessage(request), "messages[].name")
		add(hasDeveloperMessage(request), "messages[].role=developer")
		add(request.Candidates != nil && *request.Candidates > 1, "n")
		add(request.Seed != nil, "seed")
		add(request.OutputFormat != nil, "response_format")
		add(request.ReasoningEffort != "", "reasoning_effort")
		add(request.EndUserRef != "", "user")
	case domain.ProfileBedrockMantleOpenAIResponses:
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
