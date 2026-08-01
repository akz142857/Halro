package provider

import (
	"context"
	"errors"
	"slices"

	"github.com/akz142857/Heimdall/internal/domain"
)

type ProfileManifest struct {
	ID               domain.ProviderProfileID
	Revision         uint64
	ProviderType     domain.ProviderType
	AccessSurface    domain.AccessSurface
	CredentialScheme domain.CredentialScheme
	Operations       []Operation
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
		case OperationChat, OperationChatStream, OperationEmbeddings:
		default:
			return errors.New("provider profile manifest contains an unknown operation")
		}
		if _, exists := seen[operation]; exists {
			return errors.New("provider profile manifest contains a duplicate operation")
		}
		seen[operation] = struct{}{}
	}
	return nil
}

type OperationRegistry interface {
	Supports(Operation) bool
	List() []Operation
}

type operationSet struct{ operations []Operation }

func (s operationSet) Supports(operation Operation) bool {
	return slices.Contains(s.operations, operation)
}
func (s operationSet) List() []Operation { return slices.Clone(s.operations) }

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
	return &LegacyAdapterBridge{Adapter: adapter, manifest: manifest, evidence: evidence.Clone()}, nil
}

func (b *LegacyAdapterBridge) Profile() ProfileManifest {
	manifest := b.manifest
	manifest.Operations = slices.Clone(b.manifest.Operations)
	return manifest
}
func (b *LegacyAdapterBridge) Operations() OperationRegistry {
	return operationSet{operations: slices.Clone(b.manifest.Operations)}
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

func BuiltinProfile(id domain.ProviderProfileID) (ProfileManifest, bool) {
	manifests := map[domain.ProviderProfileID]ProfileManifest{
		domain.ProfileOpenAIChatEmbeddings: {
			ID: domain.ProfileOpenAIChatEmbeddings, Revision: 1, ProviderType: domain.ProviderOpenAI,
			AccessSurface: domain.SurfaceOpenAI, CredentialScheme: domain.CredentialBearerStatic,
			Operations: []Operation{OperationChat, OperationChatStream, OperationEmbeddings},
		},
		domain.ProfileAzureChatEmbeddings: {
			ID: domain.ProfileAzureChatEmbeddings, Revision: 1, ProviderType: domain.ProviderAzureOpenAI,
			AccessSurface: domain.SurfaceAzureOpenAI, CredentialScheme: domain.CredentialAzureAPIKey,
			Operations: []Operation{OperationChat, OperationChatStream, OperationEmbeddings},
		},
		domain.ProfileDeepSeekChat: {
			ID: domain.ProfileDeepSeekChat, Revision: 1, ProviderType: domain.ProviderDeepSeek,
			AccessSurface: domain.SurfaceDeepSeek, CredentialScheme: domain.CredentialBearerStatic,
			Operations: []Operation{OperationChat, OperationChatStream},
		},
		domain.ProfileOpenAICompatible: {
			ID: domain.ProfileOpenAICompatible, Revision: 1, ProviderType: domain.ProviderOpenAICompatible,
			AccessSurface: domain.SurfaceOpenAICompatible, CredentialScheme: domain.CredentialBearerStatic,
			Operations: []Operation{OperationChat, OperationChatStream, OperationEmbeddings},
		},
		domain.ProfileGeminiText: {
			ID: domain.ProfileGeminiText, Revision: 1, ProviderType: domain.ProviderGemini,
			AccessSurface: domain.SurfaceGemini, CredentialScheme: domain.CredentialGoogleAPIKey,
			Operations: []Operation{OperationChat, OperationChatStream, OperationEmbeddings},
		},
		domain.ProfileBedrockConverseText: {
			ID: domain.ProfileBedrockConverseText, Revision: 1, ProviderType: domain.ProviderBedrock,
			AccessSurface: domain.SurfaceBedrockRuntime, CredentialScheme: domain.CredentialAWSSigV4Explicit,
			Operations: []Operation{OperationChat, OperationChatStream},
		},
	}
	manifest, ok := manifests[id]
	if !ok {
		return ProfileManifest{}, false
	}
	manifest.Operations = slices.Clone(manifest.Operations)
	return manifest, true
}
