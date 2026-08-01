package compatibility

import (
	"slices"
	"testing"

	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/semantic"
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

func TestEndpointManifestCloneIsDeep(t *testing.T) {
	original := BuiltinEndpointManifests()[0]
	manifest := CloneEndpointManifest(original)
	manifest.ProfileCoverage[4].UnsupportedRequestFields[0] = "mutated"
	if original.ProfileCoverage[4].UnsupportedRequestFields[0] == "mutated" {
		t.Fatal("profile coverage shares mutable storage")
	}
}
