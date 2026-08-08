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

func TestProviderFieldCompatibilityRejectsUnsupportedMessageNames(t *testing.T) {
	request := semantic.GenerateRequest{Messages: []semantic.Message{{Role: semantic.RoleUser, Name: "customer", Content: []semantic.Content{{Kind: semantic.ContentText, Text: "hi"}}}}}
	for _, profileID := range []domain.ProviderProfileID{domain.ProfileGeminiText, domain.ProfileBedrockConverseText} {
		if fields := UnsupportedGenerateFields(profileID, request); !slices.Contains(fields, "messages[].name") {
			t.Fatalf("%s did not reject messages[].name: %v", profileID, fields)
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
