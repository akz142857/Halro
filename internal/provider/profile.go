package provider

import (
	"context"
	"errors"
	"slices"

	"github.com/akz142857/Heimdall/internal/anthropicapi"
	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/semantic"
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
		case OperationChat, OperationChatStream, OperationEmbeddings, OperationMessages, OperationMessagesStream:
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
		domain.ProfileOpenAIChatEmbeddings: {OperationChat: PrimitiveOpenAIChatCompletions, OperationChatStream: PrimitiveOpenAIChatStream, OperationEmbeddings: PrimitiveOpenAIEmbeddings},
		domain.ProfileAnthropicMessages:    {OperationChat: PrimitiveAnthropicMessages, OperationChatStream: PrimitiveAnthropicMessagesStream, OperationMessages: PrimitiveAnthropicMessages, OperationMessagesStream: PrimitiveAnthropicMessagesStream},
		domain.ProfileAzureChatEmbeddings:  {OperationChat: PrimitiveAzureChatCompletions, OperationChatStream: PrimitiveAzureChatStream, OperationEmbeddings: PrimitiveAzureEmbeddings},
		domain.ProfileDeepSeekChat:         {OperationChat: PrimitiveDeepSeekChat, OperationChatStream: PrimitiveDeepSeekChatStream},
		domain.ProfileOpenAICompatible:     {OperationChat: PrimitiveCompatibleChat, OperationChatStream: PrimitiveCompatibleChatStream, OperationEmbeddings: PrimitiveCompatibleEmbeddings},
		domain.ProfileGeminiText:           {OperationChat: PrimitiveGeminiGenerateContent, OperationChatStream: PrimitiveGeminiStreamGenerateContent, OperationEmbeddings: PrimitiveGeminiEmbedContent},
		domain.ProfileBedrockConverseText:  {OperationChat: PrimitiveBedrockConverse, OperationChatStream: PrimitiveBedrockConverseStream},
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
		}
	}
	return nil, false
}

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
	return Capabilities{Chat: true, Streaming: true, Embeddings: true}
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

func BuiltinProfile(id domain.ProviderProfileID) (ProfileManifest, bool) {
	manifests := map[domain.ProviderProfileID]ProfileManifest{
		domain.ProfileOpenAIChatEmbeddings: {
			ID: domain.ProfileOpenAIChatEmbeddings, Revision: 1, ProviderType: domain.ProviderOpenAI,
			AccessSurface: domain.SurfaceOpenAI, CredentialScheme: domain.CredentialBearerStatic,
			Operations:        []Operation{OperationChat, OperationChatStream, OperationEmbeddings},
			PrimitiveBindings: []PrimitiveBinding{{OperationChat, semantic.OperationGenerate, PrimitiveOpenAIChatCompletions}, {OperationChatStream, semantic.OperationGenerate, PrimitiveOpenAIChatStream}, {OperationEmbeddings, semantic.OperationEmbed, PrimitiveOpenAIEmbeddings}},
		},
		domain.ProfileAnthropicMessages: {
			ID: domain.ProfileAnthropicMessages, Revision: 1, ProviderType: domain.ProviderAnthropic,
			AccessSurface: domain.SurfaceAnthropic, CredentialScheme: domain.CredentialAnthropicAPIKey,
			Operations:        []Operation{OperationChat, OperationChatStream, OperationMessages, OperationMessagesStream},
			PrimitiveBindings: []PrimitiveBinding{{OperationChat, semantic.OperationGenerate, PrimitiveAnthropicMessages}, {OperationChatStream, semantic.OperationGenerate, PrimitiveAnthropicMessagesStream}, {OperationMessages, semantic.OperationGenerate, PrimitiveAnthropicMessages}, {OperationMessagesStream, semantic.OperationGenerate, PrimitiveAnthropicMessagesStream}},
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
	}
	manifest, ok := manifests[id]
	if !ok {
		return ProfileManifest{}, false
	}
	manifest.Operations = slices.Clone(manifest.Operations)
	manifest.PrimitiveBindings = slices.Clone(manifest.PrimitiveBindings)
	return manifest, true
}
