package provider

import (
	"context"
	"errors"
	"slices"

	"github.com/akz142857/Halro/internal/anthropicapi"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/semantic"
)

type ProfileManifest struct {
	ID                domain.ProviderProfileID
	Revision          uint64
	ProviderType      domain.ProviderType
	AccessSurface     domain.AccessSurface
	CredentialScheme  domain.CredentialScheme
	Operations        []Operation
	PrimitiveBindings []PrimitiveBinding
}

func (m ProfileManifest) Validate() error {
	if m.ID == "" || m.Revision == 0 || m.ProviderType == "" || m.AccessSurface == "" || m.CredentialScheme == "" {
		return errors.New("provider profile manifest identity is incomplete")
	}
	if err := domain.ValidateProviderProfile(m.ProviderType, m.AccessSurface, m.ID, m.CredentialScheme); err != nil {
		return err
	}
	if len(m.Operations) == 0 {
		return errors.New("provider profile manifest operations are required")
	}
	seen := make(map[Operation]struct{}, len(m.Operations))
	for _, operation := range m.Operations {
		switch operation {
		case OperationChat, OperationChatStream, OperationEmbeddings, OperationMessages, OperationMessagesStream,
			OperationModerations, OperationImages, OperationTranscriptions, OperationSpeech,
			OperationFiles, OperationBatches, OperationRerank, OperationAsyncInvoke:
		default:
			return errors.New("provider profile manifest contains an unknown operation")
		}
		if _, exists := seen[operation]; exists {
			return errors.New("provider profile manifest contains a duplicate operation")
		}
		seen[operation] = struct{}{}
	}
	if len(m.PrimitiveBindings) != len(m.Operations) {
		return errors.New("provider profile primitive bindings do not match operations")
	}
	bound := make(map[Operation]struct{}, len(m.PrimitiveBindings))
	for _, binding := range m.PrimitiveBindings {
		if binding.Primitive == "" || binding.SemanticOperation.Validate() != nil {
			return errors.New("provider profile contains an invalid primitive binding")
		}
		if binding.SemanticOperation != semanticOperationFor(binding.LegacyOperation) || !profileAllowsPrimitive(m.ID, binding.LegacyOperation, binding.Primitive) {
			return errors.New("provider profile contains a semantically invalid primitive binding")
		}
		if _, exists := seen[binding.LegacyOperation]; !exists {
			return errors.New("provider profile binds an undeclared operation")
		}
		if _, duplicate := bound[binding.LegacyOperation]; duplicate {
			return errors.New("provider profile contains a duplicate primitive binding")
		}
		bound[binding.LegacyOperation] = struct{}{}
	}
	return nil
}

func profileAllowsPrimitive(profileID domain.ProviderProfileID, operation Operation, primitive Primitive) bool {
	expected := map[domain.ProviderProfileID]map[Operation]Primitive{
		domain.ProfileOpenAIChatEmbeddings:           {OperationChat: PrimitiveOpenAIChatCompletions, OperationChatStream: PrimitiveOpenAIChatStream, OperationEmbeddings: PrimitiveOpenAIEmbeddings},
		domain.ProfileAnthropicMessages:              {OperationChat: PrimitiveAnthropicMessages, OperationChatStream: PrimitiveAnthropicMessagesStream, OperationMessages: PrimitiveAnthropicMessages, OperationMessagesStream: PrimitiveAnthropicMessagesStream, OperationFiles: PrimitiveHalroLocalFiles, OperationBatches: PrimitiveAnthropicMessageBatches},
		domain.ProfileAzureChatEmbeddings:            {OperationChat: PrimitiveAzureChatCompletions, OperationChatStream: PrimitiveAzureChatStream, OperationEmbeddings: PrimitiveAzureEmbeddings},
		domain.ProfileDeepSeekChat:                   {OperationChat: PrimitiveDeepSeekChat, OperationChatStream: PrimitiveDeepSeekChatStream},
		domain.ProfileOpenAICompatible:               {OperationChat: PrimitiveCompatibleChat, OperationChatStream: PrimitiveCompatibleChatStream, OperationEmbeddings: PrimitiveCompatibleEmbeddings},
		domain.ProfileGeminiText:                     {OperationChat: PrimitiveGeminiGenerateContent, OperationChatStream: PrimitiveGeminiStreamGenerateContent, OperationEmbeddings: PrimitiveGeminiEmbedContent},
		domain.ProfileBedrockConverseText:            {OperationChat: PrimitiveBedrockConverse, OperationChatStream: PrimitiveBedrockConverseStream},
		domain.ProfileBedrockInvokeTitanEmbedV2:      {OperationEmbeddings: PrimitiveBedrockInvokeTitanEmbedV2},
		domain.ProfileOpenAIMediaResources:           {OperationModerations: PrimitiveOpenAIModerations, OperationImages: PrimitiveOpenAIImages, OperationTranscriptions: PrimitiveOpenAIAudioTranscriptions, OperationSpeech: PrimitiveOpenAIAudioSpeech, OperationFiles: PrimitiveOpenAIFiles, OperationBatches: PrimitiveOpenAIBatches},
		domain.ProfileBedrockInvokeTitanImageV2:      {OperationImages: PrimitiveBedrockTitanImageV2},
		domain.ProfileBedrockAgentRerankCohere35:     {OperationRerank: PrimitiveBedrockAgentRerankCohere35},
		domain.ProfileBedrockAsyncNovaReel:           {OperationAsyncInvoke: PrimitiveBedrockAsyncNovaReel},
		domain.ProfileBedrockMantleOpenAIChat:        {OperationChat: PrimitiveBedrockMantleOpenAIChat, OperationChatStream: PrimitiveBedrockMantleOpenAIChatStream},
		domain.ProfileBedrockMantleOpenAIResponses:   {OperationChat: PrimitiveBedrockMantleOpenAIResponses, OperationChatStream: PrimitiveBedrockMantleOpenAIResponsesStream},
		domain.ProfileBedrockMantleAnthropicMessages: {OperationChat: PrimitiveBedrockMantleAnthropicMessages, OperationChatStream: PrimitiveBedrockMantleAnthropicMessagesStream, OperationMessages: PrimitiveBedrockMantleAnthropicMessages, OperationMessagesStream: PrimitiveBedrockMantleAnthropicMessagesStream},
	}
	operations, ok := expected[profileID]
	if !ok {
		return false
	}
	return operations[operation] == primitive
}

type OperationRegistry interface {
	Supports(Operation) bool
	List() []Operation
	Resolve(Operation) (OperationAdapter, bool)
	Bindings() []PrimitiveBinding
}

type operationSet struct {
	operations []Operation
	bindings   []PrimitiveBinding
	adapter    Adapter
}

func (s operationSet) Supports(operation Operation) bool {
	return slices.Contains(s.operations, operation)
}
func (s operationSet) List() []Operation            { return slices.Clone(s.operations) }
func (s operationSet) Bindings() []PrimitiveBinding { return slices.Clone(s.bindings) }
func (s operationSet) Resolve(operation Operation) (OperationAdapter, bool) {
	for _, binding := range s.bindings {
		if binding.LegacyOperation != operation {
			continue
		}
		switch binding.SemanticOperation {
		case semantic.OperationGenerate:
			if binding.LegacyOperation == OperationMessages || binding.LegacyOperation == OperationMessagesStream {
				return nativeOperationPrimitive{adapter: s.adapter, primitive: binding.Primitive, operation: binding.LegacyOperation}, true
			}
			return legacyGenerationPrimitive{adapter: s.adapter, primitive: binding.Primitive, operation: binding.LegacyOperation}, true
		case semantic.OperationEmbed:
			return legacyEmbeddingPrimitive{adapter: s.adapter, primitive: binding.Primitive, operation: binding.LegacyOperation}, true
		default:
			return inferenceResourcesOperationPrimitive{operation: binding.LegacyOperation, semantic: binding.SemanticOperation, primitive: binding.Primitive}, true
		}
	}
	return nil, false
}

type inferenceResourcesOperationPrimitive struct {
	operation Operation
	semantic  semantic.Operation
	primitive Primitive
}

func (p inferenceResourcesOperationPrimitive) LegacyOperation() Operation { return p.operation }
func (p inferenceResourcesOperationPrimitive) SemanticOperation() semantic.Operation {
	return p.semantic
}
func (p inferenceResourcesOperationPrimitive) ProviderPrimitive() Primitive { return p.primitive }

type ProfiledAdapter interface {
	Adapter
	Profile() ProfileManifest
	Operations() OperationRegistry
	CapabilityEvidence() domain.CapabilityEvidenceSet
}

type LegacyAdapterBridge struct {
	Adapter
	manifest ProfileManifest
	evidence domain.CapabilityEvidenceSet
}

func NewLegacyAdapterBridge(adapter Adapter, manifest ProfileManifest, evidence domain.CapabilityEvidenceSet) (*LegacyAdapterBridge, error) {
	if adapter == nil {
		return nil, errors.New("legacy adapter is required")
	}
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	if adapter.Type() != string(manifest.ProviderType) {
		return nil, errors.New("legacy adapter type does not match profile")
	}
	manifest.Operations = slices.Clone(manifest.Operations)
	manifest.PrimitiveBindings = slices.Clone(manifest.PrimitiveBindings)
	return &LegacyAdapterBridge{Adapter: adapter, manifest: manifest, evidence: evidence.Clone()}, nil
}

func (b *LegacyAdapterBridge) Profile() ProfileManifest {
	manifest := b.manifest
	manifest.Operations = slices.Clone(b.manifest.Operations)
	manifest.PrimitiveBindings = slices.Clone(b.manifest.PrimitiveBindings)
	return manifest
}

func (b *LegacyAdapterBridge) UnwrapAdapter() Adapter { return b.Adapter }
func (b *LegacyAdapterBridge) Operations() OperationRegistry {
	return operationSet{operations: slices.Clone(b.manifest.Operations), bindings: slices.Clone(b.manifest.PrimitiveBindings), adapter: b}
}
func (b *LegacyAdapterBridge) CapabilityEvidence() domain.CapabilityEvidenceSet {
	return b.evidence.Clone()
}
func (b *LegacyAdapterBridge) Capabilities() Capabilities {
	if reporter, ok := b.Adapter.(CapabilityReporter); ok {
		return reporter.Capabilities()
	}
	capabilities := Capabilities{}
	for _, operation := range b.manifest.Operations {
		switch operation {
		case OperationChat, OperationMessages:
			capabilities.Chat = true
		case OperationChatStream, OperationMessagesStream:
			capabilities.Chat, capabilities.Streaming = true, true
		case OperationEmbeddings:
			capabilities.Embeddings = true
		case OperationModerations:
			capabilities.Moderations = true
		case OperationImages:
			capabilities.Images = true
		case OperationTranscriptions:
			capabilities.Transcriptions = true
		case OperationSpeech:
			capabilities.Speech = true
		case OperationFiles:
			capabilities.Files = true
		case OperationBatches:
			capabilities.Batches = true
		case OperationRerank:
			capabilities.Rerank = true
		case OperationAsyncInvoke:
			capabilities.AsyncGenerate = true
		}
	}
	return capabilities
}

func (b *LegacyAdapterBridge) Moderate(ctx context.Context, call ModerationCall) (ModerationResult, error) {
	a, ok := b.Adapter.(StatelessInferenceResourcesAdapter)
	if !ok {
		return ModerationResult{}, errors.New("moderation is unavailable")
	}
	return a.Moderate(ctx, call)
}
func (b *LegacyAdapterBridge) GenerateImage(ctx context.Context, call ImageCall) (ImageResult, error) {
	a, ok := b.Adapter.(StatelessInferenceResourcesAdapter)
	if !ok {
		return ImageResult{}, errors.New("image generation is unavailable")
	}
	return a.GenerateImage(ctx, call)
}
func (b *LegacyAdapterBridge) Transcribe(ctx context.Context, call TranscriptionCall) (TranscriptionResult, error) {
	a, ok := b.Adapter.(StatelessInferenceResourcesAdapter)
	if !ok {
		return TranscriptionResult{}, errors.New("transcription is unavailable")
	}
	return a.Transcribe(ctx, call)
}
func (b *LegacyAdapterBridge) Synthesize(ctx context.Context, call SpeechCall) (SpeechResult, error) {
	a, ok := b.Adapter.(StatelessInferenceResourcesAdapter)
	if !ok {
		return SpeechResult{}, errors.New("speech is unavailable")
	}
	return a.Synthesize(ctx, call)
}
func (b *LegacyAdapterBridge) CreateFile(ctx context.Context, call FileCreateCall) (FileObject, error) {
	a, ok := b.Adapter.(ResourceFilesAdapter)
	if !ok {
		return FileObject{}, errors.New("files are unavailable")
	}
	return a.CreateFile(ctx, call)
}
func (b *LegacyAdapterBridge) GetFile(ctx context.Context, requestID, id string) (FileObject, error) {
	a, ok := b.Adapter.(ResourceFilesAdapter)
	if !ok {
		return FileObject{}, errors.New("files are unavailable")
	}
	return a.GetFile(ctx, requestID, id)
}
func (b *LegacyAdapterBridge) DownloadFile(ctx context.Context, requestID, id string) (FileContent, error) {
	a, ok := b.Adapter.(ResourceFilesAdapter)
	if !ok {
		return FileContent{}, errors.New("files are unavailable")
	}
	return a.DownloadFile(ctx, requestID, id)
}
func (b *LegacyAdapterBridge) DeleteFile(ctx context.Context, requestID, id string) (FileDeleteResult, error) {
	a, ok := b.Adapter.(ResourceFilesAdapter)
	if !ok {
		return FileDeleteResult{}, errors.New("files are unavailable")
	}
	return a.DeleteFile(ctx, requestID, id)
}

// FetchBatchResults forwards to an adapter that can collect a finished batch's
// results. It exists on the bridge because everything reaches the gateway
// wrapped in one: an optional interface asserted against the wrapper is an
// assertion against the wrapper, which is how a criterion that could never fire
// was nearly shipped once already.
func (b *LegacyAdapterBridge) FetchBatchResults(ctx context.Context, requestID, batchID, resultsURL string) ([]byte, error) {
	a, ok := b.Adapter.(BatchResultsAdapter)
	if !ok {
		return nil, errors.New("batch result collection is unavailable")
	}
	return a.FetchBatchResults(ctx, requestID, batchID, resultsURL)
}

func (b *LegacyAdapterBridge) CreateBatch(ctx context.Context, call BatchCreateCall) (BatchObject, error) {
	a, ok := b.Adapter.(ResourceBatchesAdapter)
	if !ok {
		return BatchObject{}, errors.New("batches are unavailable")
	}
	return a.CreateBatch(ctx, call)
}
func (b *LegacyAdapterBridge) GetBatch(ctx context.Context, requestID, id string) (BatchObject, error) {
	a, ok := b.Adapter.(ResourceBatchesAdapter)
	if !ok {
		return BatchObject{}, errors.New("batches are unavailable")
	}
	return a.GetBatch(ctx, requestID, id)
}
func (b *LegacyAdapterBridge) CancelBatch(ctx context.Context, requestID, id string) (BatchObject, error) {
	a, ok := b.Adapter.(ResourceBatchesAdapter)
	if !ok {
		return BatchObject{}, errors.New("batches are unavailable")
	}
	return a.CancelBatch(ctx, requestID, id)
}
func (b *LegacyAdapterBridge) Rerank(ctx context.Context, call RerankCall) (RerankResult, error) {
	a, ok := b.Adapter.(BedrockInferenceResourcesAdapter)
	if !ok {
		return RerankResult{}, errors.New("rerank is unavailable")
	}
	return a.Rerank(ctx, call)
}
func (b *LegacyAdapterBridge) StartAsyncInvoke(ctx context.Context, call AsyncInvokeCall) (AsyncInvokeObject, error) {
	a, ok := b.Adapter.(BedrockInferenceResourcesAdapter)
	if !ok {
		return AsyncInvokeObject{}, errors.New("async invoke is unavailable")
	}
	return a.StartAsyncInvoke(ctx, call)
}
func (b *LegacyAdapterBridge) GetAsyncInvoke(ctx context.Context, requestID, id string) (AsyncInvokeObject, error) {
	a, ok := b.Adapter.(BedrockInferenceResourcesAdapter)
	if !ok {
		return AsyncInvokeObject{}, errors.New("async invoke is unavailable")
	}
	return a.GetAsyncInvoke(ctx, requestID, id)
}
func (b *LegacyAdapterBridge) GenerateBedrockImage(ctx context.Context, call ImageCall) (ImageResult, error) {
	a, ok := b.Adapter.(BedrockInferenceResourcesAdapter)
	if !ok {
		return ImageResult{}, errors.New("Bedrock image generation is unavailable")
	}
	return a.GenerateBedrockImage(ctx, call)
}
func (b *LegacyAdapterBridge) Probe(ctx context.Context, model string) error {
	prober, ok := b.Adapter.(Prober)
	if !ok {
		return errors.New("provider profile does not support connection testing")
	}
	return prober.Probe(ctx, model)
}

func (b *LegacyAdapterBridge) MessagesNative(ctx context.Context, call NativeMessageCall) (NativeMessageResult, error) {
	adapter, ok := b.Adapter.(NativeMessagesAdapter)
	if !ok {
		return NativeMessageResult{}, errors.New("native Messages is unavailable")
	}
	return adapter.MessagesNative(ctx, call)
}

func (b *LegacyAdapterBridge) MessagesNativeStream(ctx context.Context, call NativeMessageCall, emit func(anthropicapi.RawStreamEvent) error) (*anthropicapi.Usage, error) {
	adapter, ok := b.Adapter.(NativeMessagesAdapter)
	if !ok {
		return nil, errors.New("native Messages streaming is unavailable")
	}
	return adapter.MessagesNativeStream(ctx, call, emit)
}

func (b *LegacyAdapterBridge) CountTokensNative(ctx context.Context, call NativeMessageCall) (NativeMessageResult, error) {
	adapter, ok := b.Adapter.(NativeTokenCountAdapter)
	if !ok {
		return NativeMessageResult{}, errors.New("native count_tokens is unavailable")
	}
	return adapter.CountTokensNative(ctx, call)
}

func BuiltinProfile(id domain.ProviderProfileID) (ProfileManifest, bool) {
	manifests := map[domain.ProviderProfileID]ProfileManifest{
		domain.ProfileOpenAIChatEmbeddings: {
			ID: domain.ProfileOpenAIChatEmbeddings, Revision: 1, ProviderType: domain.ProviderOpenAI,
			AccessSurface: domain.SurfaceOpenAI, CredentialScheme: domain.CredentialBearerStatic,
			Operations:        []Operation{OperationChat, OperationChatStream, OperationEmbeddings},
			PrimitiveBindings: []PrimitiveBinding{{OperationChat, semantic.OperationGenerate, PrimitiveOpenAIChatCompletions}, {OperationChatStream, semantic.OperationGenerate, PrimitiveOpenAIChatStream}, {OperationEmbeddings, semantic.OperationEmbed, PrimitiveOpenAIEmbeddings}},
		},
		domain.ProfileAnthropicMessages: {
			ID: domain.ProfileAnthropicMessages, Revision: 2, ProviderType: domain.ProviderAnthropic,
			AccessSurface: domain.SurfaceAnthropic, CredentialScheme: domain.CredentialAnthropicAPIKey,
			// Files are declared and served locally. Anthropic batches carry their
			// requests inline and refer to no file, so the upload exists to give
			// the caller something to name in the OpenAI-shaped batch request —
			// Halro keeps the bytes and Anthropic is never told about them
			// (ADR 0021). Declaring the operation is what makes the file
			// addressable at all: CreateFile routes by operation, so without it
			// there is no Anthropic deployment to upload to.
			Operations:        []Operation{OperationChat, OperationChatStream, OperationMessages, OperationMessagesStream, OperationFiles, OperationBatches},
			PrimitiveBindings: []PrimitiveBinding{{OperationChat, semantic.OperationGenerate, PrimitiveAnthropicMessages}, {OperationChatStream, semantic.OperationGenerate, PrimitiveAnthropicMessagesStream}, {OperationMessages, semantic.OperationGenerate, PrimitiveAnthropicMessages}, {OperationMessagesStream, semantic.OperationGenerate, PrimitiveAnthropicMessagesStream}, {OperationFiles, semantic.OperationFile, PrimitiveHalroLocalFiles}, {OperationBatches, semantic.OperationBatch, PrimitiveAnthropicMessageBatches}},
		},
		domain.ProfileAzureChatEmbeddings: {
			ID: domain.ProfileAzureChatEmbeddings, Revision: 1, ProviderType: domain.ProviderAzureOpenAI,
			AccessSurface: domain.SurfaceAzureOpenAI, CredentialScheme: domain.CredentialAzureAPIKey,
			Operations:        []Operation{OperationChat, OperationChatStream, OperationEmbeddings},
			PrimitiveBindings: []PrimitiveBinding{{OperationChat, semantic.OperationGenerate, PrimitiveAzureChatCompletions}, {OperationChatStream, semantic.OperationGenerate, PrimitiveAzureChatStream}, {OperationEmbeddings, semantic.OperationEmbed, PrimitiveAzureEmbeddings}},
		},
		domain.ProfileDeepSeekChat: {
			ID: domain.ProfileDeepSeekChat, Revision: 1, ProviderType: domain.ProviderDeepSeek,
			AccessSurface: domain.SurfaceDeepSeek, CredentialScheme: domain.CredentialBearerStatic,
			Operations:        []Operation{OperationChat, OperationChatStream},
			PrimitiveBindings: []PrimitiveBinding{{OperationChat, semantic.OperationGenerate, PrimitiveDeepSeekChat}, {OperationChatStream, semantic.OperationGenerate, PrimitiveDeepSeekChatStream}},
		},
		domain.ProfileOpenAICompatible: {
			ID: domain.ProfileOpenAICompatible, Revision: 1, ProviderType: domain.ProviderOpenAICompatible,
			AccessSurface: domain.SurfaceOpenAICompatible, CredentialScheme: domain.CredentialBearerStatic,
			Operations:        []Operation{OperationChat, OperationChatStream, OperationEmbeddings},
			PrimitiveBindings: []PrimitiveBinding{{OperationChat, semantic.OperationGenerate, PrimitiveCompatibleChat}, {OperationChatStream, semantic.OperationGenerate, PrimitiveCompatibleChatStream}, {OperationEmbeddings, semantic.OperationEmbed, PrimitiveCompatibleEmbeddings}},
		},
		domain.ProfileGeminiText: {
			ID: domain.ProfileGeminiText, Revision: 1, ProviderType: domain.ProviderGemini,
			AccessSurface: domain.SurfaceGemini, CredentialScheme: domain.CredentialGoogleAPIKey,
			Operations:        []Operation{OperationChat, OperationChatStream, OperationEmbeddings},
			PrimitiveBindings: []PrimitiveBinding{{OperationChat, semantic.OperationGenerate, PrimitiveGeminiGenerateContent}, {OperationChatStream, semantic.OperationGenerate, PrimitiveGeminiStreamGenerateContent}, {OperationEmbeddings, semantic.OperationEmbed, PrimitiveGeminiEmbedContent}},
		},
		domain.ProfileBedrockConverseText: {
			ID: domain.ProfileBedrockConverseText, Revision: 1, ProviderType: domain.ProviderBedrock,
			AccessSurface: domain.SurfaceBedrockRuntime, CredentialScheme: domain.CredentialAWSSigV4Explicit,
			Operations:        []Operation{OperationChat, OperationChatStream},
			PrimitiveBindings: []PrimitiveBinding{{OperationChat, semantic.OperationGenerate, PrimitiveBedrockConverse}, {OperationChatStream, semantic.OperationGenerate, PrimitiveBedrockConverseStream}},
		},
		domain.ProfileBedrockInvokeTitanEmbedV2: {
			ID: domain.ProfileBedrockInvokeTitanEmbedV2, Revision: 1, ProviderType: domain.ProviderBedrock,
			AccessSurface: domain.SurfaceBedrockRuntime, CredentialScheme: domain.CredentialAWSSigV4Explicit,
			Operations:        []Operation{OperationEmbeddings},
			PrimitiveBindings: []PrimitiveBinding{{OperationEmbeddings, semantic.OperationEmbed, PrimitiveBedrockInvokeTitanEmbedV2}},
		},
		domain.ProfileOpenAIMediaResources: {
			ID: domain.ProfileOpenAIMediaResources, Revision: 1, ProviderType: domain.ProviderOpenAI, AccessSurface: domain.SurfaceOpenAI, CredentialScheme: domain.CredentialBearerStatic,
			Operations:        []Operation{OperationModerations, OperationImages, OperationTranscriptions, OperationSpeech, OperationFiles, OperationBatches},
			PrimitiveBindings: []PrimitiveBinding{{OperationModerations, semantic.OperationModerate, PrimitiveOpenAIModerations}, {OperationImages, semantic.OperationImage, PrimitiveOpenAIImages}, {OperationTranscriptions, semantic.OperationTranscribe, PrimitiveOpenAIAudioTranscriptions}, {OperationSpeech, semantic.OperationSynthesize, PrimitiveOpenAIAudioSpeech}, {OperationFiles, semantic.OperationFile, PrimitiveOpenAIFiles}, {OperationBatches, semantic.OperationBatch, PrimitiveOpenAIBatches}},
		},
		domain.ProfileBedrockInvokeTitanImageV2:  {ID: domain.ProfileBedrockInvokeTitanImageV2, Revision: 1, ProviderType: domain.ProviderBedrock, AccessSurface: domain.SurfaceBedrockRuntime, CredentialScheme: domain.CredentialAWSSigV4Explicit, Operations: []Operation{OperationImages}, PrimitiveBindings: []PrimitiveBinding{{OperationImages, semantic.OperationImage, PrimitiveBedrockTitanImageV2}}},
		domain.ProfileBedrockAgentRerankCohere35: {ID: domain.ProfileBedrockAgentRerankCohere35, Revision: 1, ProviderType: domain.ProviderBedrock, AccessSurface: domain.SurfaceBedrockAgentRuntime, CredentialScheme: domain.CredentialAWSSigV4Explicit, Operations: []Operation{OperationRerank}, PrimitiveBindings: []PrimitiveBinding{{OperationRerank, semantic.OperationRerank, PrimitiveBedrockAgentRerankCohere35}}},
		domain.ProfileBedrockAsyncNovaReel:       {ID: domain.ProfileBedrockAsyncNovaReel, Revision: 1, ProviderType: domain.ProviderBedrock, AccessSurface: domain.SurfaceBedrockRuntime, CredentialScheme: domain.CredentialAWSSigV4Explicit, Operations: []Operation{OperationAsyncInvoke}, PrimitiveBindings: []PrimitiveBinding{{OperationAsyncInvoke, semantic.OperationAsyncGenerate, PrimitiveBedrockAsyncNovaReel}}},
		domain.ProfileBedrockMantleOpenAIChat: {
			ID: domain.ProfileBedrockMantleOpenAIChat, Revision: 1, ProviderType: domain.ProviderBedrock,
			AccessSurface: domain.SurfaceBedrockMantle, CredentialScheme: domain.CredentialBedrockAPIKey,
			Operations:        []Operation{OperationChat, OperationChatStream},
			PrimitiveBindings: []PrimitiveBinding{{OperationChat, semantic.OperationGenerate, PrimitiveBedrockMantleOpenAIChat}, {OperationChatStream, semantic.OperationGenerate, PrimitiveBedrockMantleOpenAIChatStream}},
		},
		domain.ProfileBedrockMantleOpenAIResponses: {
			ID: domain.ProfileBedrockMantleOpenAIResponses, Revision: 1, ProviderType: domain.ProviderBedrock,
			AccessSurface: domain.SurfaceBedrockMantle, CredentialScheme: domain.CredentialBedrockAPIKey,
			Operations:        []Operation{OperationChat, OperationChatStream},
			PrimitiveBindings: []PrimitiveBinding{{OperationChat, semantic.OperationGenerate, PrimitiveBedrockMantleOpenAIResponses}, {OperationChatStream, semantic.OperationGenerate, PrimitiveBedrockMantleOpenAIResponsesStream}},
		},
		domain.ProfileBedrockMantleAnthropicMessages: {
			ID: domain.ProfileBedrockMantleAnthropicMessages, Revision: 1, ProviderType: domain.ProviderBedrock,
			AccessSurface: domain.SurfaceBedrockMantle, CredentialScheme: domain.CredentialBedrockAPIKey,
			Operations:        []Operation{OperationChat, OperationChatStream, OperationMessages, OperationMessagesStream},
			PrimitiveBindings: []PrimitiveBinding{{OperationChat, semantic.OperationGenerate, PrimitiveBedrockMantleAnthropicMessages}, {OperationChatStream, semantic.OperationGenerate, PrimitiveBedrockMantleAnthropicMessagesStream}, {OperationMessages, semantic.OperationGenerate, PrimitiveBedrockMantleAnthropicMessages}, {OperationMessagesStream, semantic.OperationGenerate, PrimitiveBedrockMantleAnthropicMessagesStream}},
		},
	}
	manifest, ok := manifests[id]
	if !ok {
		return ProfileManifest{}, false
	}
	manifest.Operations = slices.Clone(manifest.Operations)
	manifest.PrimitiveBindings = slices.Clone(manifest.PrimitiveBindings)
	return manifest, true
}
