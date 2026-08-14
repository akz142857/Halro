package compatibility

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/semantic"
)

func TestProviderFieldCompatibilityRejectsSilentLoss(t *testing.T) {
	seed := int64(7)
	candidates := 2
	parallel := false
	request := semantic.GenerateRequest{
		Seed:            &seed,
		Candidates:      &candidates,
		Tools:           []semantic.Tool{{Name: "lookup"}},
		ToolChoice:      &semantic.ToolChoice{Mode: semantic.ToolChoiceAuto},
		ParallelTools:   &parallel,
		OutputFormat:    &semantic.OutputFormat{Kind: semantic.OutputJSONObject},
		ReasoningEffort: "high",
		EndUserRef:      "customer",
	}
	if fields := UnsupportedGenerateFields(domain.ProfileOpenAIChatEmbeddings, request); len(fields) != 0 {
		t.Fatalf("OpenAI fields unexpectedly rejected: %v", fields)
	}
	if fields := UnsupportedGenerateFields(domain.ProfileGeminiText, request); len(fields) != 7 {
		t.Fatalf("Gemini loss was not fully declared: %v", fields)
	}
	if fields := UnsupportedGenerateFields(domain.ProfileBedrockConverseText, request); len(fields) != 8 {
		t.Fatalf("Bedrock loss was not fully declared: %v", fields)
	}
}

// Every profile either carries messages[].name to the wire or declares that it
// cannot. Listing both halves is the point: the Bedrock Mantle Responses branch
// used to do neither, and a table that only checked the profiles known to
// declare the field would never have noticed. A Responses message item has no
// place for a speaker's name, so a conversation with several participants
// routed there came back 200 with them made indistinguishable.
func TestProviderFieldCompatibilityAccountsForMessageNamesOnEveryProfile(t *testing.T) {
	request := semantic.GenerateRequest{Messages: []semantic.Message{{Role: semantic.RoleUser, Name: "customer", Content: []semantic.Content{{Kind: semantic.ContentText, Text: "hi"}}}}}
	for _, profileID := range []domain.ProviderProfileID{
		domain.ProfileGeminiText,
		domain.ProfileBedrockConverseText,
		domain.ProfileAnthropicMessages,
		domain.ProfileBedrockMantleAnthropicMessages,
		domain.ProfileBedrockMantleOpenAIResponses,
	} {
		if fields := UnsupportedGenerateFields(profileID, request); !slices.Contains(fields, "messages[].name") {
			t.Fatalf("%s drops messages[].name without declaring it: %v", profileID, fields)
		}
	}
	// These render the OpenAI wire form, whose message object has the field, so
	// declaring it would route requests away from a provider that serves them.
	for _, profileID := range []domain.ProviderProfileID{
		domain.ProfileOpenAIChatEmbeddings,
		domain.ProfileAzureChatEmbeddings,
		domain.ProfileDeepSeekChat,
		domain.ProfileOpenAICompatible,
		domain.ProfileBedrockMantleOpenAIChat,
	} {
		if fields := UnsupportedGenerateFields(profileID, request); slices.Contains(fields, "messages[].name") {
			t.Fatalf("%s declared a field it carries: %v", profileID, fields)
		}
	}
}

func TestBedrockMantleResponsesRejectsOnlyUnrepresentableChatFields(t *testing.T) {
	seed := int64(7)
	candidates := 2
	request := semantic.GenerateRequest{
		Stream: true, Candidates: &candidates, Stop: []string{"stop"}, Seed: &seed,
		Tools: []semantic.Tool{{Name: "lookup"}}, ReasoningEffort: "high", EndUserRef: "supported-user",
	}
	fields := UnsupportedGenerateFields(domain.ProfileBedrockMantleOpenAIResponses, request)
	for _, required := range []string{"n", "stop", "seed", "tools", "reasoning_effort"} {
		if !slices.Contains(fields, required) {
			t.Fatalf("Mantle Responses did not reject %s: %v", required, fields)
		}
	}
	if slices.Contains(fields, "user") {
		t.Fatalf("Mantle Responses rejected a represented field: %v", fields)
	}
	if fields := UnsupportedGenerateFields(domain.ProfileBedrockMantleOpenAIChat, request); len(fields) != 0 {
		t.Fatalf("Mantle Chat unexpectedly rejected OpenAI wire fields: %v", fields)
	}
}

func TestUnknownProviderProfileRejectsRichSemanticsAndDeclaresScalarConversion(t *testing.T) {
	temperature := 0.5
	request := semantic.GenerateRequest{
		Messages:    []semantic.Message{{Role: semantic.RoleDeveloper, Name: "named", Content: []semantic.Content{{Kind: semantic.ContentInputImage, URL: "https://example.test/image"}}}},
		Temperature: &temperature, Stop: []string{"stop"}, IncludeUsage: true,
	}
	fields := UnsupportedGenerateFields("", request)
	for _, required := range []string{"messages[].name", "messages[].role", "messages[].content", "stream_options"} {
		if !slices.Contains(fields, required) {
			t.Fatalf("unknown profile did not reject %s: %v", required, fields)
		}
	}
	dimensions := int64(256)
	embeddingFields := UnsupportedEmbeddingFields("", semantic.EmbeddingRequest{Encoding: "float", Dimensions: &dimensions, EndUserRef: "user"})
	if len(embeddingFields) != 1 || embeddingFields[0] != "user" {
		t.Fatalf("unknown embedding profile did not preserve scalar wire controls conservatively: %v", embeddingFields)
	}
}

func TestGeminiEmbeddingFieldCompatibility(t *testing.T) {
	dimensions := int64(768)
	request := semantic.EmbeddingRequest{Encoding: "base64", Dimensions: &dimensions, EndUserRef: "customer"}
	fields := UnsupportedEmbeddingFields(domain.ProfileGeminiText, request)
	if len(fields) != 2 || fields[0] != "encoding_format" || fields[1] != "user" {
		t.Fatalf("unexpected unsupported fields: %v", fields)
	}
}

func TestTitanEmbeddingProfileRejectsPartialOpenAISemantics(t *testing.T) {
	dimensions := int64(128)
	request := semantic.EmbeddingRequest{
		Operation: semantic.OperationEmbed, Source: semantic.Source{ProfileID: "openai.embeddings.v1", ProfileRevision: 1},
		Mode: semantic.ModePortable, RequestedModel: "embedding", Input: json.RawMessage(`["one","two"]`),
		Encoding: "base64", Dimensions: &dimensions, EndUserRef: "tenant",
	}
	request.Requirements = request.DeriveRequirements()
	fields := UnsupportedEmbeddingFields(domain.ProfileBedrockInvokeTitanEmbedV2, request)
	if !slices.Equal(fields, []string{"input", "encoding_format", "dimensions", "user"}) {
		t.Fatalf("fields=%v", fields)
	}
	validDimensions := int64(512)
	request.Input = json.RawMessage(`"one"`)
	request.Encoding = "float"
	request.Dimensions = &validDimensions
	request.EndUserRef = ""
	request.Requirements = request.DeriveRequirements()
	if fields := UnsupportedEmbeddingFields(domain.ProfileBedrockInvokeTitanEmbedV2, request); len(fields) != 0 {
		t.Fatalf("valid request rejected: %v", fields)
	}
}

func TestEndpointManifestCloneIsDeep(t *testing.T) {
	original := BuiltinEndpointManifests()[0]
	manifest := CloneEndpointManifest(original)
	coverageIndex := -1
	for index := range manifest.ProfileCoverage {
		if len(manifest.ProfileCoverage[index].UnsupportedRequestFields) > 0 {
			coverageIndex = index
			break
		}
	}
	if coverageIndex < 0 {
		t.Fatal("test manifest has no unsupported profile fields")
	}
	manifest.ProfileCoverage[coverageIndex].UnsupportedRequestFields[0] = "mutated"
	if original.ProfileCoverage[coverageIndex].UnsupportedRequestFields[0] == "mutated" {
		t.Fatal("profile coverage shares mutable storage")
	}
}

// The same both-halves table for a failed tool result. Only the Anthropic-wire
// profiles have somewhere to put is_error; every other profile has to say so, or
// the model is handed a failure that reads as a successful answer.
func TestFailedToolResultIsDeclaredByEveryProfileThatCannotCarryIt(t *testing.T) {
	request := semantic.GenerateRequest{Messages: []semantic.Message{
		{Role: semantic.RoleAssistant, Content: []semantic.Content{{Kind: semantic.ContentToolCall, CallID: "toolu_1", Name: "lookup", Arguments: "{}"}}},
		{Role: semantic.RoleTool, Content: []semantic.Content{{Kind: semantic.ContentToolResult, CallID: "toolu_1", Text: "boom", ToolError: true}}},
	}}
	for _, profileID := range []domain.ProviderProfileID{
		domain.ProfileGeminiText,
		domain.ProfileBedrockConverseText,
		domain.ProfileBedrockMantleOpenAIResponses,
		domain.ProfileOpenAIChatEmbeddings,
		domain.ProfileAzureChatEmbeddings,
		domain.ProfileDeepSeekChat,
		domain.ProfileOpenAICompatible,
		domain.ProfileBedrockMantleOpenAIChat,
		domain.ProviderProfileID("some-extension-adapter"),
	} {
		if fields := UnsupportedGenerateFields(profileID, request); !slices.Contains(fields, "messages[].content[].is_error") {
			t.Fatalf("%s drops is_error without declaring it: %v", profileID, fields)
		}
	}
	for _, profileID := range []domain.ProviderProfileID{
		domain.ProfileAnthropicMessages,
		domain.ProfileBedrockMantleAnthropicMessages,
	} {
		if fields := UnsupportedGenerateFields(profileID, request); slices.Contains(fields, "messages[].content[].is_error") {
			t.Fatalf("%s declared a field it carries: %v", profileID, fields)
		}
	}
	// A tool result that succeeded declares nothing: the field is only lost when
	// there is something in it to lose.
	request.Messages[1].Content[0].ToolError = false
	for _, profileID := range []domain.ProviderProfileID{domain.ProfileOpenAIChatEmbeddings, domain.ProfileGeminiText} {
		if fields := UnsupportedGenerateFields(profileID, request); slices.Contains(fields, "messages[].content[].is_error") {
			t.Fatalf("%s declared a loss for a request that carries no failure: %v", profileID, fields)
		}
	}
}
