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

// profileAllowsPrimitive answers from the one operation table. It used to hold
// a second copy of that mapping, written out per profile, whose only job was to
// be compared against the manifests — two hand-written lists agreeing was how
// they were checked, and the only way they could disagree.
func profileAllowsPrimitive(profileID domain.ProviderProfileID, operation Operation, primitive Primitive) bool {
	return profileAllowsPrimitiveDerived(profileID, operation, primitive)
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
			if semanticGenerationPrimitives[binding.Primitive] {
				generator, ok := unwrapSemanticGenerator(s.adapter)
				if !ok {
					return nil, false
				}
				return semanticGenerationPrimitive{adapter: generator, primitive: binding.Primitive, operation: binding.LegacyOperation}, true
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

// ProbeRequiresModel answers for the adapter behind the bridge, and answers
// false for one that never stated a requirement: an adapter that can probe
// without a model must not be refused for lack of one.
func (b *LegacyAdapterBridge) ProbeRequiresModel() bool {
	requirer, ok := b.Adapter.(ProbeModelRequirer)
	return ok && requirer.ProbeRequiresModel()
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

// BuiltinProfile assembles a profile's manifest from the operation table and
// the domain profile table, which owns the identity half.
//
// The manifest used to be written out per profile, repeating the type, surface
// and scheme that domain already holds — and that ValidateProviderProfile then
// checked them against, which is the shape of a value that carries no
// information. Slices are freshly built here, so callers keep getting copies.
func BuiltinProfile(id domain.ProviderProfileID) (ProfileManifest, bool) {
	return builtinProfileDerived(id)
}
