package provider

import (
	"context"
	"errors"
	"fmt"

	openaiwire "github.com/akz142857/Heimdall/internal/compatibility/openai"
	"github.com/akz142857/Heimdall/internal/semantic"
)

// Primitive identifies the concrete provider API used southbound. It is
// intentionally distinct from both a northbound endpoint and a semantic
// operation.
type Primitive string

const (
	PrimitiveOpenAIChatCompletions                Primitive = "openai.chat-completions"
	PrimitiveAnthropicMessages                    Primitive = "anthropic.messages"
	PrimitiveAnthropicMessagesStream              Primitive = "anthropic.messages.stream"
	PrimitiveOpenAIChatStream                     Primitive = "openai.chat-completions.stream"
	PrimitiveOpenAIEmbeddings                     Primitive = "openai.embeddings"
	PrimitiveAzureChatCompletions                 Primitive = "azure-openai.chat-completions"
	PrimitiveAzureChatStream                      Primitive = "azure-openai.chat-completions.stream"
	PrimitiveAzureEmbeddings                      Primitive = "azure-openai.embeddings"
	PrimitiveDeepSeekChat                         Primitive = "deepseek.chat-completions"
	PrimitiveDeepSeekChatStream                   Primitive = "deepseek.chat-completions.stream"
	PrimitiveCompatibleChat                       Primitive = "openai-compatible.chat-completions"
	PrimitiveCompatibleChatStream                 Primitive = "openai-compatible.chat-completions.stream"
	PrimitiveCompatibleEmbeddings                 Primitive = "openai-compatible.embeddings"
	PrimitiveGeminiGenerateContent                Primitive = "gemini.generate-content"
	PrimitiveGeminiStreamGenerateContent          Primitive = "gemini.stream-generate-content"
	PrimitiveGeminiEmbedContent                   Primitive = "gemini.embed-content"
	PrimitiveBedrockConverse                      Primitive = "bedrock.converse"
	PrimitiveBedrockConverseStream                Primitive = "bedrock.converse-stream"
	PrimitiveBedrockInvokeTitanEmbedV2            Primitive = "bedrock.invoke-model.titan-embed-text-v2"
	PrimitiveOpenAIModerations                    Primitive = "openai.moderations"
	PrimitiveOpenAIImages                         Primitive = "openai.images"
	PrimitiveOpenAIAudioTranscriptions            Primitive = "openai.audio.transcriptions"
	PrimitiveOpenAIAudioSpeech                    Primitive = "openai.audio.speech"
	PrimitiveOpenAIFiles                          Primitive = "openai.files"
	PrimitiveOpenAIBatches                        Primitive = "openai.batches"
	PrimitiveBedrockTitanImageV2                  Primitive = "bedrock.invoke-model.titan-image-v2"
	PrimitiveBedrockAgentRerankCohere35           Primitive = "bedrock-agent-runtime.rerank.cohere-v3-5"
	PrimitiveBedrockAsyncNovaReel                 Primitive = "bedrock.start-async-invoke.nova-reel-v1"
	PrimitiveBedrockMantleOpenAIChat              Primitive = "bedrock.mantle.openai.chat"
	PrimitiveBedrockMantleOpenAIChatStream        Primitive = "bedrock.mantle.openai.chat.stream"
	PrimitiveBedrockMantleOpenAIResponses         Primitive = "bedrock.mantle.openai.responses"
	PrimitiveBedrockMantleOpenAIResponsesStream   Primitive = "bedrock.mantle.openai.responses.stream"
	PrimitiveBedrockMantleAnthropicMessages       Primitive = "bedrock.mantle.anthropic.messages"
	PrimitiveBedrockMantleAnthropicMessagesStream Primitive = "bedrock.mantle.anthropic.messages.stream"
)

type PrimitiveBinding struct {
	LegacyOperation   Operation          `json:"legacy_operation"`
	SemanticOperation semantic.Operation `json:"semantic_operation"`
	Primitive         Primitive          `json:"provider_primitive"`
}

func semanticOperationFor(operation Operation) semantic.Operation {
	switch operation {
	case OperationChat, OperationChatStream, OperationMessages, OperationMessagesStream:
		return semantic.OperationGenerate
	case OperationEmbeddings:
		return semantic.OperationEmbed
	case OperationModerations:
		return semantic.OperationModerate
	case OperationImages:
		return semantic.OperationImage
	case OperationTranscriptions:
		return semantic.OperationTranscribe
	case OperationSpeech:
		return semantic.OperationSynthesize
	case OperationFiles:
		return semantic.OperationFile
	case OperationBatches:
		return semantic.OperationBatch
	case OperationRerank:
		return semantic.OperationRerank
	case OperationAsyncInvoke:
		return semantic.OperationAsyncGenerate
	default:
		return ""
	}
}

type OperationAdapter interface {
	LegacyOperation() Operation
	SemanticOperation() semantic.Operation
	ProviderPrimitive() Primitive
}

type GenerateCall struct {
	RequestID     string
	ProviderModel string
	Request       semantic.GenerateRequest
}

type EmbedCall struct {
	RequestID     string
	ProviderModel string
	Request       semantic.EmbeddingRequest
}

type GenerationAdapter interface {
	OperationAdapter
	Generate(context.Context, GenerateCall) (semantic.GenerateResult, error)
	GenerateStream(context.Context, GenerateCall, func(semantic.Event) error) (*semantic.Usage, error)
}

type EmbeddingAdapter interface {
	OperationAdapter
	EmbedSemantic(context.Context, EmbedCall) (semantic.EmbeddingResult, error)
}

type legacyGenerationPrimitive struct {
	adapter   Adapter
	primitive Primitive
	operation Operation
}

func (adapter legacyGenerationPrimitive) LegacyOperation() Operation { return adapter.operation }
func (adapter legacyGenerationPrimitive) SemanticOperation() semantic.Operation {
	return semantic.OperationGenerate
}
func (adapter legacyGenerationPrimitive) ProviderPrimitive() Primitive { return adapter.primitive }
func (adapter legacyGenerationPrimitive) Generate(ctx context.Context, call GenerateCall) (semantic.GenerateResult, error) {
	wireRequest, err := openaiwire.RenderGenerateRequest(call.Request, call.ProviderModel)
	if err != nil {
		return semantic.GenerateResult{}, &Error{Class: ErrorBadRequest, Message: "render canonical generate request", Cause: err}
	}
	wireResult, err := adapter.adapter.Chat(ctx, ChatCall{RequestID: call.RequestID, ProviderModel: call.ProviderModel, Request: wireRequest})
	if err != nil {
		return semantic.GenerateResult{}, err
	}
	result, err := openaiwire.DecodeGenerateResult(wireResult)
	if err != nil {
		return semantic.GenerateResult{}, &Error{Class: ErrorMalformed, Ambiguous: true, Message: "normalize provider generate result", Cause: err}
	}
	result.Translation = translationForPrimitive(adapter.primitive)
	return result, nil
}
func (adapter legacyGenerationPrimitive) GenerateStream(ctx context.Context, call GenerateCall, emit func(semantic.Event) error) (*semantic.Usage, error) {
	wireRequest, err := openaiwire.RenderGenerateRequest(call.Request, call.ProviderModel)
	if err != nil {
		return nil, &Error{Class: ErrorBadRequest, Message: "render canonical stream request", Cause: err}
	}
	var last semantic.Event
	usageEmitted := false
	var downstreamErr error
	var observedUsage *semantic.Usage
	usage, err := adapter.adapter.ChatStream(ctx, ChatCall{RequestID: call.RequestID, ProviderModel: call.ProviderModel, Request: wireRequest}, func(event semantic.Event) error {
		event.MappingRevision = openaiwire.MappingRevision
		event.Translation = translationForPrimitive(adapter.primitive)
		last = event
		if event.Usage != nil {
			copy := *event.Usage
			observedUsage = &copy
		}
		// Provider adapters may attach cumulative usage to an output delta. The
		// canonical contract emits usage exactly once, after every output ends.
		if event.Kind == semantic.EventUsage {
			return nil
		}
		event.Usage = nil
		downstreamErr = emit(event)
		return downstreamErr
	})
	semanticUsage := openaiwire.DecodeUsage(usage)
	if semanticUsage == nil {
		semanticUsage = observedUsage
	}
	if err != nil && downstreamErr != nil {
		return semanticUsage, &Error{Class: ErrorMalformed, Ambiguous: true, Message: "consume canonical provider stream", Cause: err}
	}
	if err == nil && call.Request.IncludeUsage && semanticUsage != nil && !usageEmitted && last.ID != "" {
		event := semantic.Event{Kind: semantic.EventUsage, ID: last.ID, Created: last.Created, Model: last.Model, Usage: semanticUsage, MappingRevision: openaiwire.MappingRevision, Translation: translationForPrimitive(adapter.primitive)}
		if emitErr := emit(event); emitErr != nil {
			return semanticUsage, emitErr
		}
		usageEmitted = true
	}
	return semanticUsage, err
}

type legacyEmbeddingPrimitive struct {
	adapter   Adapter
	primitive Primitive
	operation Operation
}

type nativeOperationPrimitive struct {
	adapter   Adapter
	primitive Primitive
	operation Operation
}

func (adapter nativeOperationPrimitive) LegacyOperation() Operation { return adapter.operation }
func (adapter nativeOperationPrimitive) SemanticOperation() semantic.Operation {
	return semantic.OperationGenerate
}
func (adapter nativeOperationPrimitive) ProviderPrimitive() Primitive { return adapter.primitive }

func (adapter legacyEmbeddingPrimitive) LegacyOperation() Operation { return adapter.operation }
func (adapter legacyEmbeddingPrimitive) SemanticOperation() semantic.Operation {
	return semantic.OperationEmbed
}
func (adapter legacyEmbeddingPrimitive) ProviderPrimitive() Primitive { return adapter.primitive }
func (adapter legacyEmbeddingPrimitive) EmbedSemantic(ctx context.Context, call EmbedCall) (semantic.EmbeddingResult, error) {
	wireRequest, err := openaiwire.RenderEmbeddingRequest(call.Request, call.ProviderModel)
	if err != nil {
		return semantic.EmbeddingResult{}, &Error{Class: ErrorBadRequest, Message: "render canonical embedding request", Cause: err}
	}
	wireResult, err := adapter.adapter.Embed(ctx, EmbeddingCall{RequestID: call.RequestID, ProviderModel: call.ProviderModel, Request: wireRequest})
	if err != nil {
		return semantic.EmbeddingResult{}, err
	}
	result, err := openaiwire.DecodeEmbeddingResult(wireResult)
	if err != nil {
		return semantic.EmbeddingResult{}, &Error{Class: ErrorMalformed, Ambiguous: true, Message: "normalize provider embedding result", Cause: err}
	}
	result.Translation = translationForPrimitive(adapter.primitive)
	return result, nil
}

func translationForPrimitive(primitive Primitive) semantic.TranslationLoss {
	switch primitive {
	case PrimitiveOpenAIChatCompletions, PrimitiveOpenAIChatStream, PrimitiveOpenAIEmbeddings,
		PrimitiveAnthropicMessages, PrimitiveAnthropicMessagesStream,
		PrimitiveBedrockMantleOpenAIChat, PrimitiveBedrockMantleOpenAIChatStream,
		PrimitiveBedrockMantleOpenAIResponses, PrimitiveBedrockMantleOpenAIResponsesStream,
		PrimitiveBedrockMantleAnthropicMessages, PrimitiveBedrockMantleAnthropicMessagesStream,
		PrimitiveAzureChatCompletions, PrimitiveAzureChatStream, PrimitiveAzureEmbeddings,
		PrimitiveDeepSeekChat, PrimitiveDeepSeekChatStream,
		PrimitiveCompatibleChat, PrimitiveCompatibleChatStream, PrimitiveCompatibleEmbeddings:
		return semantic.TranslationNone
	case PrimitiveGeminiGenerateContent, PrimitiveGeminiStreamGenerateContent, PrimitiveGeminiEmbedContent,
		PrimitiveBedrockConverse, PrimitiveBedrockConverseStream, PrimitiveBedrockInvokeTitanEmbedV2:
		return semantic.TranslationDeclared
	default:
		// Unknown and legacy primitives have no versioned mapping contract that
		// can prove a lossless conversion.
		return semantic.TranslationDeclared
	}
}

func legacyPrimitive(adapter Adapter, operation Operation) OperationAdapter {
	name := Primitive(fmt.Sprintf("legacy.%s.%s", adapter.Type(), operation))
	if semanticOperationFor(operation) == semantic.OperationGenerate {
		return legacyGenerationPrimitive{adapter: adapter, primitive: name, operation: operation}
	}
	return legacyEmbeddingPrimitive{adapter: adapter, primitive: name, operation: operation}
}

func (target Target) ResolveOperation(operation Operation) (OperationAdapter, bool) {
	if target.operations != nil {
		return target.operations.Resolve(operation)
	}
	if target.Adapter == nil {
		return nil, false
	}
	switch operation {
	case OperationChat:
		return legacyPrimitive(target.Adapter, operation), target.Capabilities.Chat
	case OperationChatStream:
		return legacyPrimitive(target.Adapter, operation), target.Capabilities.Chat && target.Capabilities.Streaming
	case OperationEmbeddings:
		return legacyPrimitive(target.Adapter, operation), target.Capabilities.Embeddings
	default:
		return nil, false
	}
}

func (target Target) Generation(operation Operation) (GenerationAdapter, error) {
	if operation != OperationChat && operation != OperationChatStream {
		return nil, errors.New("requested operation is not generation")
	}
	adapter, ok := target.ResolveOperation(operation)
	if !ok {
		return nil, errors.New("generation operation is unavailable")
	}
	generation, ok := adapter.(GenerationAdapter)
	if !ok {
		return nil, errors.New("generation operation has an invalid adapter")
	}
	return generation, nil
}

func (target Target) Embedding() (EmbeddingAdapter, error) {
	adapter, ok := target.ResolveOperation(OperationEmbeddings)
	if !ok {
		return nil, errors.New("embedding operation is unavailable")
	}
	embedding, ok := adapter.(EmbeddingAdapter)
	if !ok {
		return nil, errors.New("embedding operation has an invalid adapter")
	}
	return embedding, nil
}

func (target Target) NativeMessages(stream bool) (NativeMessagesAdapter, error) {
	operation := OperationMessages
	if stream {
		operation = OperationMessagesStream
	}
	if _, ok := target.ResolveOperation(operation); !ok {
		return nil, errors.New("native Messages operation is unavailable")
	}
	adapter, ok := target.Adapter.(NativeMessagesAdapter)
	if !ok {
		return nil, errors.New("provider adapter does not implement native Messages")
	}
	return adapter, nil
}
