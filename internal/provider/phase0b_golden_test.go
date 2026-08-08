package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	openaiwire "github.com/akz142857/Halro/internal/compatibility/openai"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

type profileMatrixLegacy struct {
	providerType  string
	lastChat      openaiapi.ChatCompletionRequest
	lastEmbedding openaiapi.EmbeddingRequest
}

func (adapter *profileMatrixLegacy) Type() string { return adapter.providerType }
func (adapter *profileMatrixLegacy) Close()       {}
func (adapter *profileMatrixLegacy) Chat(_ context.Context, call ChatCall) (openaiapi.ChatCompletionResponse, error) {
	adapter.lastChat = call.Request
	finish := "stop"
	return openaiapi.ChatCompletionResponse{ID: "result_1", Object: "chat.completion", Created: 7, Model: call.ProviderModel, Choices: []openaiapi.Choice{{Index: 0, Message: &openaiapi.Message{Role: "assistant", Content: openaiapi.TextContent("hello")}, FinishReason: &finish}}, Usage: &openaiapi.Usage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3}}, nil
}
func (adapter *profileMatrixLegacy) ChatStream(_ context.Context, call ChatCall, emit func(semantic.Event) error) (*openaiapi.Usage, error) {
	adapter.lastChat = call.Request
	event, err := openaiwire.DecodeEvent(openaiapi.ChatCompletionResponse{ID: "event_1", Object: "chat.completion.chunk", Model: call.ProviderModel, Choices: []openaiapi.Choice{{Index: 0, Delta: &openaiapi.Message{Role: "assistant", Content: openaiapi.TextContent("hello")}}}})
	if err != nil {
		return nil, err
	}
	if err = emit(event); err != nil {
		return nil, err
	}
	return &openaiapi.Usage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3}, nil
}
func (adapter *profileMatrixLegacy) Embed(_ context.Context, call EmbeddingCall) (openaiapi.EmbeddingResponse, error) {
	adapter.lastEmbedding = call.Request
	return openaiapi.EmbeddingResponse{Object: "list", Model: call.ProviderModel, Data: []openaiapi.EmbeddingData{{Object: "embedding", Index: 0, Embedding: json.RawMessage(`[0.25]`)}}, Usage: &openaiapi.Usage{PromptTokens: 1, TotalTokens: 1}}, nil
}

// This fixture verifies that profile identity selects distinct immutable
// primitives while the legacy bridge preserves the canonical shape. It is not
// a provider-wire compatibility matrix; real wire behavior belongs to each
// adapter's transport fixtures.
func TestLegacyBridgeGenerateProfileBindings(t *testing.T) {
	request, err := openaiwire.DecodeGenerate(openaiapi.ChatCompletionRequest{Model: "public", Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}}})
	if err != nil {
		t.Fatal(err)
	}
	requestJSON, _ := json.Marshal(request)
	assertNoProviderLeak(t, requestJSON)
	var golden string
	for _, profileID := range []domain.ProviderProfileID{domain.ProfileOpenAIChatEmbeddings, domain.ProfileAzureChatEmbeddings, domain.ProfileDeepSeekChat, domain.ProfileOpenAICompatible, domain.ProfileGeminiText, domain.ProfileBedrockConverseText} {
		t.Run(string(profileID), func(t *testing.T) {
			manifest, _ := BuiltinProfile(profileID)
			legacy := &profileMatrixLegacy{providerType: string(manifest.ProviderType)}
			bridge, err := NewLegacyAdapterBridge(legacy, manifest, nil)
			if err != nil {
				t.Fatal(err)
			}
			resolved, ok := bridge.Operations().Resolve(OperationChat)
			if !ok {
				t.Fatal("generate primitive missing")
			}
			generation := resolved.(GenerationAdapter)
			result, err := generation.Generate(context.Background(), GenerateCall{RequestID: "request_1", ProviderModel: "provider-model", Request: request})
			if err != nil {
				t.Fatal(err)
			}
			rendered, err := openaiwire.RenderGenerateResult(result)
			if err != nil {
				t.Fatal(err)
			}
			if legacy.lastChat.Model != "provider-model" || rendered.Choices[0].Message == nil {
				t.Fatal("canonical bridge did not traverse provider wire")
			}
			encoded, _ := json.Marshal(result)
			assertNoProviderLeak(t, encoded)
			expectedTranslation := semantic.TranslationNone
			if profileID == domain.ProfileGeminiText || profileID == domain.ProfileBedrockConverseText {
				expectedTranslation = semantic.TranslationDeclared
			}
			if result.Translation != expectedTranslation {
				t.Fatalf("translation=%s want %s", result.Translation, expectedTranslation)
			}
			result.Translation = semantic.TranslationNone
			encoded, _ = json.Marshal(result)
			if golden == "" {
				golden = string(encoded)
			} else if string(encoded) != golden {
				t.Fatalf("canonical result differs across profiles\nwant %s\n got %s", golden, encoded)
			}
			streamResolved, ok := bridge.Operations().Resolve(OperationChatStream)
			if !ok {
				t.Fatal("stream primitive missing")
			}
			streamGeneration := streamResolved.(GenerationAdapter)
			if streamGeneration.ProviderPrimitive() == generation.ProviderPrimitive() {
				t.Fatal("streaming primitive collapsed into request primitive")
			}
			var events []semantic.Event
			usage, err := streamGeneration.GenerateStream(context.Background(), GenerateCall{RequestID: "request_1", ProviderModel: "provider-model", Request: withStream(request)}, func(event semantic.Event) error { events = append(events, event); return nil })
			if err != nil || len(events) != 1 || usage.TotalTokens != 3 {
				t.Fatalf("stream contract failed: events=%d usage=%#v err=%v", len(events), usage, err)
			}
			if events[0].MappingRevision != openaiwire.MappingRevision || events[0].Translation != expectedTranslation {
				t.Fatalf("stream audit metadata=%#v", events[0])
			}
		})
	}
}

func TestLegacyBridgeEmbeddingProfileBindings(t *testing.T) {
	request, err := openaiwire.DecodeEmbedding(openaiapi.EmbeddingRequest{Model: "public", Input: json.RawMessage(`"hi"`)})
	if err != nil {
		t.Fatal(err)
	}
	var golden string
	for _, profileID := range []domain.ProviderProfileID{domain.ProfileOpenAIChatEmbeddings, domain.ProfileAzureChatEmbeddings, domain.ProfileOpenAICompatible, domain.ProfileGeminiText, domain.ProfileBedrockInvokeTitanEmbedV2} {
		manifest, _ := BuiltinProfile(profileID)
		legacy := &profileMatrixLegacy{providerType: string(manifest.ProviderType)}
		bridge, err := NewLegacyAdapterBridge(legacy, manifest, nil)
		if err != nil {
			t.Fatal(err)
		}
		resolved, ok := bridge.Operations().Resolve(OperationEmbeddings)
		if !ok {
			t.Fatalf("%s embed primitive missing", profileID)
		}
		result, err := resolved.(EmbeddingAdapter).EmbedSemantic(context.Background(), EmbedCall{RequestID: "request_1", ProviderModel: "provider-model", Request: request})
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(result)
		assertNoProviderLeak(t, encoded)
		if profileID == domain.ProfileGeminiText || profileID == domain.ProfileBedrockInvokeTitanEmbedV2 {
			if result.Translation != semantic.TranslationDeclared {
				t.Fatalf("%s embedding transform was not declared", profileID)
			}
			result.Translation = semantic.TranslationNone
			encoded, _ = json.Marshal(result)
		}
		if golden == "" {
			golden = string(encoded)
		} else if string(encoded) != golden {
			t.Fatalf("canonical embedding differs for %s", profileID)
		}
	}
}

func TestProfileAxesRemainOrthogonal(t *testing.T) {
	manifest, _ := BuiltinProfile(domain.ProfileGeminiText)
	bridge, err := NewLegacyAdapterBridge(&profileMatrixLegacy{providerType: string(manifest.ProviderType)}, manifest, nil)
	if err != nil {
		t.Fatal(err)
	}
	generate, _ := bridge.Operations().Resolve(OperationChat)
	embed, _ := bridge.Operations().Resolve(OperationEmbeddings)
	if generate.SemanticOperation() != semantic.OperationGenerate || embed.SemanticOperation() != semantic.OperationEmbed || generate.ProviderPrimitive() == embed.ProviderPrimitive() {
		t.Fatalf("axes collapsed: generate=%s embed=%s", generate.ProviderPrimitive(), embed.ProviderPrimitive())
	}
}

func withStream(request semantic.GenerateRequest) semantic.GenerateRequest {
	request.Stream = true
	request.Requirements.Streaming = true
	return request
}
func assertNoProviderLeak(t *testing.T, encoded []byte) {
	t.Helper()
	lower := strings.ToLower(string(encoded))
	for _, forbidden := range []string{"previous_response_id", "cache_control", "function_calling_config", "anthropic_version", "bedrock_model_id"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("provider field %q leaked into canonical JSON: %s", forbidden, encoded)
		}
	}
}
