package provider

import (
	"context"
	"errors"
	"fmt"

	openaiwire "github.com/akz142857/Halro/internal/compatibility/openai"
	"github.com/akz142857/Halro/internal/semantic"
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
	PrimitiveOpenAIResponses                      Primitive = "openai.responses"
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
	PrimitiveMiniMaxAnthropicMessages             Primitive = "minimax.anthropic.messages"
	PrimitiveMiniMaxAnthropicMessagesStream       Primitive = "minimax.anthropic.messages.stream"
	PrimitiveMiniMaxChat                          Primitive = "minimax.chat-completions"
	PrimitiveMiniMaxChatStream                    Primitive = "minimax.chat-completions.stream"
	// Five, not six. The Responses profile serves the unary operation only, so a
	// minimax.responses.stream constant would be a name nothing binds — and
	// ProfileManifest.Validate checks that every binding names a declared
	// operation, not that every constant is bound. See the Responses capability
	// set in internal/domain/provider_table.go for why streaming is absent.
	PrimitiveMiniMaxResponses Primitive = "minimax.responses"
	// Kimi's five, the same five shapes as MiniMax's and for the same reason: the
	// Responses profile serves the unary operation only, so there is no
	// kimi.responses.stream constant to bind.
	PrimitiveKimiChat                    Primitive = "kimi.chat-completions"
	PrimitiveKimiChatStream              Primitive = "kimi.chat-completions.stream"
	PrimitiveKimiAnthropicMessages       Primitive = "kimi.anthropic.messages"
	PrimitiveKimiAnthropicMessagesStream Primitive = "kimi.anthropic.messages.stream"
	PrimitiveKimiResponses               Primitive = "kimi.responses"

	// PrimitiveHalroLocalFiles is a file operation with no southbound call at
	// all: Halro stores the bytes and the upstream is never told they exist.
	//
	// It is a Primitive rather than a flag because a Primitive is exactly the
	// question "which provider API serves this operation", and "none, Halro
	// serves it" is an answer to that question. Saying it here means the profile
	// declares it, profileAllowsPrimitive checks it, and Validate refuses a
	// profile that binds it without meaning to — a claim that fails at load
	// rather than a behaviour inferred at request time.
	//
	// The alternative considered and rejected was inferring the mode from a
	// missing Go interface. Three independent reviewers found the same fatal
	// flaw: every adapter reaches the gateway wrapped in LegacyAdapterBridge,
	// which implements the file interface unconditionally, so the inference is
	// dead code in production while unit tests that register bare fakes see it
	// work. It also cannot tell "deliberately local" from "not implemented yet",
	// which turns a wiring defect into silent non-delivery.
	PrimitiveHalroLocalFiles Primitive = "halro.local-files"

	// PrimitiveAnthropicMessageBatches is Anthropic's own batch API. Its requests
	// are inline, which is why the file beside it is local: there is nothing to
	// upload them to.
	PrimitiveAnthropicMessageBatches Primitive = "anthropic.messages.batches"
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
		PrimitiveOpenAIResponses,
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

// NativeTokenCount resolves the count_tokens adapter. Eligibility is checked
// against OperationMessages because count_tokens takes a Messages request and is
// served by the same connection; what it does not share is billing, so the
// caller settles it at zero cost rather than through the generation path.
func (target Target) NativeTokenCount() (NativeTokenCountAdapter, error) {
	if _, ok := target.ResolveOperation(OperationMessages); !ok {
		return nil, errors.New("native Messages operation is unavailable")
	}
	adapter, ok := target.Adapter.(NativeTokenCountAdapter)
	if !ok {
		return nil, errors.New("provider adapter does not implement native count_tokens")
	}
	return adapter, nil
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

// SemanticGenerator is an adapter whose upstream wire can carry what the
// semantic request contains, so it takes the request as it is rather than as a
// Chat Completions request written from it.
//
// Every other generate adapter reaches the gateway through
// legacyGenerationPrimitive, which renders the semantic request into Chat wire
// on the way down and reads the Chat response on the way back. That is exact for
// an upstream whose own surface is Chat Completions, and it is a ceiling for
// everyone else: a member the Chat wire has no place for cannot cross it, which
// is why provider-executed tools were unreachable on OpenAI while the Responses
// endpoint that serves them was already implemented.
type SemanticGenerator interface {
	Adapter
	GenerateSemantic(context.Context, GenerateCall) (semantic.GenerateResult, error)
}

// semanticGenerationPrimitives names the primitives whose adapter must be a
// SemanticGenerator. It is a declaration, not an inference: the alternative —
// checking every adapter for the method and using it when present — cannot tell
// "this profile deliberately speaks semantic" from "this adapter happens to have
// grown a method", and it is invisible in the profile table where a reader looks
// for what a profile does.
var semanticGenerationPrimitives = map[Primitive]bool{
	PrimitiveOpenAIResponses: true,
	// MiniMax's Responses profile is served by the same adapter branch.
	//
	// What leaving it out does, stated after checking rather than before: the
	// request is not refused. Resolve falls back to the legacy Chat path, and
	// that branch translates through chatViaResponses and still addresses
	// /v1/responses. The cost is fidelity, not availability — the semantic
	// request would pass through the OpenAI Chat intermediate representation on
	// its way to the wire, taking that representation's losses, which this
	// profile's field rules do not declare because on the semantic path they do
	// not happen.
	//
	// That makes the omission invisible to every mechanical check there is. The
	// opposite mistake is caught: declaring a primitive semantic whose adapter
	// is not a SemanticGenerator fails
	// TestEveryReachableProfileReachesTheNetworkWhenCalled by name.
	PrimitiveMiniMaxResponses: true,
	// Kimi's Responses profile is served by the same adapter branch as the two
	// above, and is declared here for the same reason. This is one of the three
	// steps docs/contracts/adding-a-platform.md names as having no mechanical
	// guard: leaving it out is silent, and the cost is a lossier translation
	// that the profile's field rules do not declare.
	PrimitiveKimiResponses: true,
}

// IsSemanticGenerationPrimitive exposes the declaration above so a caller
// outside this package can dispatch the way Resolve does. It exists for the
// guard that drives one call per reachable profile: reading the declaration is
// the point, because guessing the pairing would test the guess.
func IsSemanticGenerationPrimitive(primitive Primitive) bool {
	return semanticGenerationPrimitives[primitive]
}

// unwrapSemanticGenerator finds the adapter under the profile wrapper.
//
// The wrapper embeds the Adapter interface, so the concrete adapter's own
// methods are not promoted through it — asserting on the wrapper would always
// fail and every request would be refused for a reason nothing explains.
func unwrapSemanticGenerator(adapter Adapter) (SemanticGenerator, bool) {
	for {
		if generator, ok := adapter.(SemanticGenerator); ok {
			return generator, true
		}
		unwrapper, ok := adapter.(interface{ UnwrapAdapter() Adapter })
		if !ok {
			return nil, false
		}
		adapter = unwrapper.UnwrapAdapter()
	}
}

type semanticGenerationPrimitive struct {
	adapter   SemanticGenerator
	primitive Primitive
	operation Operation
}

func (adapter semanticGenerationPrimitive) LegacyOperation() Operation { return adapter.operation }
func (adapter semanticGenerationPrimitive) SemanticOperation() semantic.Operation {
	return semantic.OperationGenerate
}
func (adapter semanticGenerationPrimitive) ProviderPrimitive() Primitive { return adapter.primitive }

func (adapter semanticGenerationPrimitive) Generate(ctx context.Context, call GenerateCall) (semantic.GenerateResult, error) {
	result, err := adapter.adapter.GenerateSemantic(ctx, call)
	if err != nil {
		return semantic.GenerateResult{}, err
	}
	result.Translation = translationForPrimitive(adapter.primitive)
	if err := result.Validate(); err != nil {
		return semantic.GenerateResult{}, &Error{Class: ErrorMalformed, Ambiguous: true, Message: "normalize provider generate result", Cause: err}
	}
	return result, nil
}

// GenerateStream is present because GenerationAdapter requires it and absent in
// effect because no profile binds this primitive to a streaming operation. A
// profile that wants streaming here has to bind a stream primitive, and until
// one does, a streaming request is refused by the target filter rather than by
// an adapter improvising a single chunk out of a unary answer.
func (adapter semanticGenerationPrimitive) GenerateStream(context.Context, GenerateCall, func(semantic.Event) error) (*semantic.Usage, error) {
	return nil, &Error{Class: ErrorBadRequest, Message: "this provider primitive does not stream"}
}
