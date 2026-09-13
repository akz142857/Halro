package provider

import (
	"context"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

// This is deliberately an independent oracle, not a value generated from
// profileOperationTable. The table is production's declaration; this contract
// is reviewable test evidence for the adapter entry point each profile is meant
// to invoke. If two same-shaped primitives are swapped in production, their
// constants remain referenced and the generated manifest remains valid, but
// this test must fail.
func TestBuiltinProfilePrimitivesMatchIndependentAdapterContract(t *testing.T) {
	expected := map[domain.ProviderProfileID]map[Operation]Primitive{
		domain.ProfileOpenAIChatEmbeddings: bindings(chatPairContract(PrimitiveOpenAIChatCompletions, PrimitiveOpenAIChatStream), pair{OperationEmbeddings, PrimitiveOpenAIEmbeddings}),
		domain.ProfileOpenAIResponses:      bindings([]pair{{OperationChat, PrimitiveOpenAIResponses}}),
		domain.ProfileAnthropicMessages: bindings(
			anthropicContract(PrimitiveAnthropicMessages, PrimitiveAnthropicMessagesStream),
			pair{OperationFiles, PrimitiveHalroLocalFiles}, pair{OperationBatches, PrimitiveAnthropicMessageBatches}),
		domain.ProfileAzureChatEmbeddings:       bindings(chatPairContract(PrimitiveAzureChatCompletions, PrimitiveAzureChatStream), pair{OperationEmbeddings, PrimitiveAzureEmbeddings}),
		domain.ProfileDeepSeekChat:              bindings(chatPairContract(PrimitiveDeepSeekChat, PrimitiveDeepSeekChatStream)),
		domain.ProfileBigModelCNChatEmbeddings:  bindings(chatPairContract(PrimitiveBigModelChat, PrimitiveBigModelChatStream), pair{OperationEmbeddings, PrimitiveBigModelEmbeddings}),
		domain.ProfileBigModelGlobalChat:        bindings(chatPairContract(PrimitiveBigModelChat, PrimitiveBigModelChatStream)),
		domain.ProfileBigModelCNCodingChat:      bindings(chatPairContract(PrimitiveBigModelChat, PrimitiveBigModelChatStream)),
		domain.ProfileBigModelGlobalCodingChat:  bindings(chatPairContract(PrimitiveBigModelChat, PrimitiveBigModelChatStream)),
		domain.ProfileOpenAICompatible:          bindings(chatPairContract(PrimitiveCompatibleChat, PrimitiveCompatibleChatStream), pair{OperationEmbeddings, PrimitiveCompatibleEmbeddings}),
		domain.ProfileGeminiText:                bindings(chatPairContract(PrimitiveGeminiGenerateContent, PrimitiveGeminiStreamGenerateContent), pair{OperationEmbeddings, PrimitiveGeminiEmbedContent}),
		domain.ProfileBedrockConverseText:       bindings(chatPairContract(PrimitiveBedrockConverse, PrimitiveBedrockConverseStream)),
		domain.ProfileBedrockInvokeTitanEmbedV2: bindings([]pair{{OperationEmbeddings, PrimitiveBedrockInvokeTitanEmbedV2}}),
		domain.ProfileOpenAIMediaResources: bindings([]pair{
			{OperationModerations, PrimitiveOpenAIModerations}, {OperationImages, PrimitiveOpenAIImages},
			{OperationTranscriptions, PrimitiveOpenAIAudioTranscriptions}, {OperationSpeech, PrimitiveOpenAIAudioSpeech},
			{OperationFiles, PrimitiveOpenAIFiles}, {OperationBatches, PrimitiveOpenAIBatches},
		}),
		domain.ProfileBedrockInvokeTitanImageV2:                  bindings([]pair{{OperationImages, PrimitiveBedrockTitanImageV2}}),
		domain.ProfileBedrockAgentRerankCohere35:                 bindings([]pair{{OperationRerank, PrimitiveBedrockAgentRerankCohere35}}),
		domain.ProfileBedrockAsyncNovaReel:                       bindings([]pair{{OperationAsyncInvoke, PrimitiveBedrockAsyncNovaReel}}),
		domain.ProfileBedrockMantleChat:                          bindings(chatPairContract(PrimitiveBedrockMantleOpenAIChat, PrimitiveBedrockMantleOpenAIChatStream)),
		domain.ProfileBedrockMantleOpenAIChat:                    bindings(chatPairContract(PrimitiveBedrockMantleOpenAIChat, PrimitiveBedrockMantleOpenAIChatStream)),
		domain.ProfileBedrockMantleResponses:                     bindings(chatPairContract(PrimitiveBedrockMantleOpenAIResponses, PrimitiveBedrockMantleOpenAIResponsesStream)),
		domain.ProfileBedrockMantleOpenAIResponses:               bindings(chatPairContract(PrimitiveBedrockMantleOpenAIResponses, PrimitiveBedrockMantleOpenAIResponsesStream)),
		domain.ProfileBedrockMantleAnthropicMessages:             bindings(anthropicContract(PrimitiveBedrockMantleAnthropicMessages, PrimitiveBedrockMantleAnthropicMessagesStream)),
		domain.ProfileMiniMaxAnthropicMessages:                   bindings(anthropicContract(PrimitiveMiniMaxAnthropicMessages, PrimitiveMiniMaxAnthropicMessagesStream)),
		domain.ProfileMiniMaxChat:                                bindings(chatPairContract(PrimitiveMiniMaxChat, PrimitiveMiniMaxChatStream)),
		domain.ProfileMiniMaxResponses:                           bindings([]pair{{OperationChat, PrimitiveMiniMaxResponses}}),
		domain.ProfileKimiChat:                                   bindings(chatPairContract(PrimitiveKimiChat, PrimitiveKimiChatStream)),
		domain.ProfileKimiAnthropicMessages:                      bindings(anthropicContract(PrimitiveKimiAnthropicMessages, PrimitiveKimiAnthropicMessagesStream)),
		domain.ProfileKimiResponses:                              bindings([]pair{{OperationChat, PrimitiveKimiResponses}}),
		domain.ProfileKimiCodeOpenAIChat:                         bindings(chatPairContract(PrimitiveKimiChat, PrimitiveKimiChatStream)),
		domain.ProfileKimiCodeAnthropicMessages:                  bindings(anthropicContract(PrimitiveKimiAnthropicMessages, PrimitiveKimiAnthropicMessagesStream)),
		domain.ProfileMiniMaxCNSubscriptionOpenAIChat:            bindings(chatPairContract(PrimitiveMiniMaxChat, PrimitiveMiniMaxChatStream)),
		domain.ProfileMiniMaxCNSubscriptionAnthropicMessages:     bindings(anthropicContract(PrimitiveMiniMaxAnthropicMessages, PrimitiveMiniMaxAnthropicMessagesStream)),
		domain.ProfileMiniMaxGlobalSubscriptionOpenAIChat:        bindings(chatPairContract(PrimitiveMiniMaxChat, PrimitiveMiniMaxChatStream)),
		domain.ProfileMiniMaxGlobalSubscriptionAnthropicMessages: bindings(anthropicContract(PrimitiveMiniMaxAnthropicMessages, PrimitiveMiniMaxAnthropicMessagesStream)),
	}

	if len(expected) != len(profileOperationTable) {
		t.Fatalf("independent contract covers %d profiles, production declares %d", len(expected), len(profileOperationTable))
	}
	for profileID := range profileOperationTable {
		if _, ok := expected[profileID]; !ok {
			t.Errorf("production profile %s has no independent primitive contract", profileID)
		}
	}
	for profileID, want := range expected {
		t.Run(string(profileID), func(t *testing.T) {
			manifest, ok := BuiltinProfile(profileID)
			if !ok {
				t.Fatal("profile is absent from production table")
			}
			if len(manifest.PrimitiveBindings) != len(want) {
				t.Fatalf("binding count=%d want=%d", len(manifest.PrimitiveBindings), len(want))
			}
			adapter := &primitiveContractAdapter{providerType: string(manifest.ProviderType)}
			bridge, err := NewLegacyAdapterBridge(adapter, manifest, nil)
			if err != nil {
				t.Fatal(err)
			}
			for operation, primitive := range want {
				resolved, ok := bridge.Operations().Resolve(operation)
				if !ok {
					t.Errorf("operation %s did not resolve", operation)
					continue
				}
				if resolved.ProviderPrimitive() != primitive {
					t.Errorf("operation %s resolved primitive %s, want %s", operation, resolved.ProviderPrimitive(), primitive)
				}
			}
		})
	}
}

type pair struct {
	operation Operation
	primitive Primitive
}

func chatPairContract(unary, stream Primitive) []pair {
	return []pair{{OperationChat, unary}, {OperationChatStream, stream}}
}

func anthropicContract(unary, stream Primitive) []pair {
	return []pair{
		{OperationChat, unary}, {OperationChatStream, stream},
		{OperationMessages, unary}, {OperationMessagesStream, stream},
	}
}

func bindings(groups ...any) map[Operation]Primitive {
	result := map[Operation]Primitive{}
	for _, group := range groups {
		switch value := group.(type) {
		case pair:
			result[value.operation] = value.primitive
		case []pair:
			for _, item := range value {
				result[item.operation] = item.primitive
			}
		default:
			panic("unsupported primitive contract group")
		}
	}
	return result
}

type primitiveContractAdapter struct {
	providerType string
}

func (a *primitiveContractAdapter) Type() string { return a.providerType }
func (*primitiveContractAdapter) Close()         {}
func (*primitiveContractAdapter) Chat(context.Context, ChatCall) (openaiapi.ChatCompletionResponse, error) {
	return openaiapi.ChatCompletionResponse{}, nil
}
func (*primitiveContractAdapter) ChatStream(context.Context, ChatCall, func(semantic.Event) error) (*openaiapi.Usage, error) {
	return nil, nil
}
func (*primitiveContractAdapter) Embed(context.Context, EmbeddingCall) (openaiapi.EmbeddingResponse, error) {
	return openaiapi.EmbeddingResponse{}, nil
}
func (*primitiveContractAdapter) GenerateSemantic(context.Context, GenerateCall) (semantic.GenerateResult, error) {
	return semantic.GenerateResult{}, nil
}
