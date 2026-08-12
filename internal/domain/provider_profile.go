package domain

import (
	"errors"
	"fmt"
	"strings"
)

type AccessSurface string
type ProviderProfileID string
type CredentialScheme string
type CapabilityEvidence string

const (
	SurfaceOpenAI              AccessSurface = "openai-api"
	SurfaceAnthropic           AccessSurface = "anthropic-api"
	SurfaceAzureOpenAI         AccessSurface = "azure-openai"
	SurfaceDeepSeek            AccessSurface = "deepseek-api"
	SurfaceOpenAICompatible    AccessSurface = "openai-compatible"
	SurfaceGemini              AccessSurface = "gemini-generate-content"
	SurfaceBedrockRuntime      AccessSurface = "bedrock-runtime"
	SurfaceBedrockMantle       AccessSurface = "bedrock-mantle"
	SurfaceBedrockAgentRuntime AccessSurface = "bedrock-agent-runtime"
)

const (
	ProfileOpenAIChatEmbeddings           ProviderProfileID = "openai.chat-embeddings.v1"
	ProfileAnthropicMessages              ProviderProfileID = "anthropic.messages.2023-06-01"
	ProfileAzureChatEmbeddings            ProviderProfileID = "azure-openai.chat-embeddings.v1"
	ProfileDeepSeekChat                   ProviderProfileID = "deepseek.chat.v1"
	ProfileOpenAICompatible               ProviderProfileID = "openai-compatible.chat-embeddings.v1"
	ProfileGeminiText                     ProviderProfileID = "gemini.generate-content.text.v1beta"
	ProfileBedrockConverseText            ProviderProfileID = "bedrock.runtime.converse.text.v1"
	ProfileBedrockInvokeTitanEmbedV2      ProviderProfileID = "bedrock.runtime.invoke.titan-embed-text-v2.v1"
	ProfileOpenAIMediaResources           ProviderProfileID = "openai.media-resources.v1"
	ProfileBedrockInvokeTitanImageV2      ProviderProfileID = "bedrock.runtime.invoke.titan-image-v2.v1"
	ProfileBedrockAgentRerankCohere35     ProviderProfileID = "bedrock.agent-runtime.rerank.cohere-v3-5.v1"
	ProfileBedrockAsyncNovaReel           ProviderProfileID = "bedrock.runtime.async.nova-reel-v1.v1"
	ProfileBedrockMantleOpenAIChat        ProviderProfileID = "bedrock.mantle.openai.chat.v1"
	ProfileBedrockMantleOpenAIResponses   ProviderProfileID = "bedrock.mantle.openai.responses.v1"
	ProfileBedrockMantleAnthropicMessages ProviderProfileID = "bedrock.mantle.anthropic.messages.v1"
)

const (
	CredentialBearerStatic     CredentialScheme = "bearer.static"
	CredentialAnthropicAPIKey  CredentialScheme = "anthropic.x-api-key"
	CredentialAzureAPIKey      CredentialScheme = "azure.api-key"
	CredentialGoogleAPIKey     CredentialScheme = "google.api-key"
	CredentialAWSSigV4Explicit CredentialScheme = "aws.sigv4.explicit-session"
	CredentialBedrockAPIKey    CredentialScheme = "aws.bedrock.api-key"
)

const (
	EvidenceVerified    CapabilityEvidence = "verified"
	EvidenceDeclared    CapabilityEvidence = "declared"
	EvidenceUnsupported CapabilityEvidence = "unsupported"
)

var capabilityNames = []string{
	"chat", "streaming", "embeddings", "tools", "vision", "json_mode",
	"developer_role", "reasoning", "stream_usage", "moderations", "images", "transcriptions", "speech", "files", "batches", "rerank", "async_generate",
}

type CapabilityEvidenceSet map[string]CapabilityEvidence

type ProviderProfileDefaults struct {
	AccessSurface    AccessSurface
	ProfileID        ProviderProfileID
	CredentialScheme CredentialScheme
}

func DefaultProviderProfile(providerType ProviderType) (ProviderProfileDefaults, bool) {
	switch providerType {
	case ProviderOpenAI:
		return ProviderProfileDefaults{SurfaceOpenAI, ProfileOpenAIChatEmbeddings, CredentialBearerStatic}, true
	case ProviderAnthropic:
		return ProviderProfileDefaults{SurfaceAnthropic, ProfileAnthropicMessages, CredentialAnthropicAPIKey}, true
	case ProviderAzureOpenAI:
		return ProviderProfileDefaults{SurfaceAzureOpenAI, ProfileAzureChatEmbeddings, CredentialAzureAPIKey}, true
	case ProviderDeepSeek:
		return ProviderProfileDefaults{SurfaceDeepSeek, ProfileDeepSeekChat, CredentialBearerStatic}, true
	case ProviderOpenAICompatible:
		return ProviderProfileDefaults{SurfaceOpenAICompatible, ProfileOpenAICompatible, CredentialBearerStatic}, true
	case ProviderGemini:
		return ProviderProfileDefaults{SurfaceGemini, ProfileGeminiText, CredentialGoogleAPIKey}, true
	case ProviderBedrock:
		return ProviderProfileDefaults{SurfaceBedrockRuntime, ProfileBedrockConverseText, CredentialAWSSigV4Explicit}, true
	default:
		return ProviderProfileDefaults{}, false
	}
}

func RegisteredProviderProfile(profile ProviderProfileID) (ProviderType, ProviderProfileDefaults, bool) {
	switch profile {
	case ProfileOpenAIChatEmbeddings, ProfileOpenAIMediaResources:
		return ProviderOpenAI, ProviderProfileDefaults{SurfaceOpenAI, profile, CredentialBearerStatic}, true
	case ProfileAnthropicMessages:
		return ProviderAnthropic, ProviderProfileDefaults{SurfaceAnthropic, profile, CredentialAnthropicAPIKey}, true
	case ProfileAzureChatEmbeddings:
		return ProviderAzureOpenAI, ProviderProfileDefaults{SurfaceAzureOpenAI, profile, CredentialAzureAPIKey}, true
	case ProfileDeepSeekChat:
		return ProviderDeepSeek, ProviderProfileDefaults{SurfaceDeepSeek, profile, CredentialBearerStatic}, true
	case ProfileOpenAICompatible:
		return ProviderOpenAICompatible, ProviderProfileDefaults{SurfaceOpenAICompatible, profile, CredentialBearerStatic}, true
	case ProfileGeminiText:
		return ProviderGemini, ProviderProfileDefaults{SurfaceGemini, profile, CredentialGoogleAPIKey}, true
	case ProfileBedrockConverseText, ProfileBedrockInvokeTitanEmbedV2, ProfileBedrockInvokeTitanImageV2, ProfileBedrockAsyncNovaReel:
		return ProviderBedrock, ProviderProfileDefaults{SurfaceBedrockRuntime, profile, CredentialAWSSigV4Explicit}, true
	case ProfileBedrockAgentRerankCohere35:
		return ProviderBedrock, ProviderProfileDefaults{SurfaceBedrockAgentRuntime, profile, CredentialAWSSigV4Explicit}, true
	case ProfileBedrockMantleOpenAIChat, ProfileBedrockMantleOpenAIResponses, ProfileBedrockMantleAnthropicMessages:
		return ProviderBedrock, ProviderProfileDefaults{SurfaceBedrockMantle, profile, CredentialBedrockAPIKey}, true
	default:
		return "", ProviderProfileDefaults{}, false
	}
}

func ResolveProviderProfile(providerType ProviderType, requested ProviderProfileID) (ProviderProfileDefaults, bool) {
	if requested == "" {
		return DefaultProviderProfile(providerType)
	}
	registeredType, profile, ok := RegisteredProviderProfile(requested)
	return profile, ok && registeredType == providerType
}

func ResolveCredentialProfile(providerType ProviderType, surface AccessSurface, scheme CredentialScheme) (ProviderProfileDefaults, bool) {
	if surface == "" && scheme == "" {
		return DefaultProviderProfile(providerType)
	}
	for _, profileID := range []ProviderProfileID{
		ProfileOpenAIChatEmbeddings, ProfileAnthropicMessages, ProfileAzureChatEmbeddings,
		ProfileDeepSeekChat, ProfileOpenAICompatible, ProfileGeminiText, ProfileBedrockConverseText,
		ProfileBedrockInvokeTitanEmbedV2,
		ProfileOpenAIMediaResources, ProfileBedrockInvokeTitanImageV2, ProfileBedrockAgentRerankCohere35, ProfileBedrockAsyncNovaReel,
		ProfileBedrockMantleOpenAIChat, ProfileBedrockMantleOpenAIResponses, ProfileBedrockMantleAnthropicMessages,
	} {
		registeredType, profile, ok := RegisteredProviderProfile(profileID)
		if ok && registeredType == providerType && profile.AccessSurface == surface && profile.CredentialScheme == scheme {
			return profile, true
		}
	}
	return ProviderProfileDefaults{}, false
}

func EvidenceForCapabilities(capabilities ProviderCapabilities, enabled CapabilityEvidence) CapabilityEvidenceSet {
	result := make(CapabilityEvidenceSet, len(capabilityNames))
	values := map[string]bool{
		"chat": capabilities.Chat, "streaming": capabilities.Streaming,
		"embeddings": capabilities.Embeddings, "tools": capabilities.Tools,
		"vision": capabilities.Vision, "json_mode": capabilities.JSONMode,
		"developer_role": capabilities.DeveloperRole, "reasoning": capabilities.Reasoning,
		"stream_usage": capabilities.StreamUsage,
		"moderations":  capabilities.Moderations, "images": capabilities.Images, "transcriptions": capabilities.Transcriptions, "speech": capabilities.Speech, "files": capabilities.Files, "batches": capabilities.Batches, "rerank": capabilities.Rerank, "async_generate": capabilities.AsyncGenerate,
	}
	for _, name := range capabilityNames {
		if values[name] {
			result[name] = enabled
		} else {
			result[name] = EvidenceUnsupported
		}
	}
	return result
}

func NormalizeCapabilityEvidence(capabilities ProviderCapabilities, existing CapabilityEvidenceSet, fallback CapabilityEvidence) CapabilityEvidenceSet {
	result := EvidenceForCapabilities(capabilities, fallback)
	for name, value := range existing {
		if _, known := result[name]; known {
			result[name] = value
		}
	}
	for name, value := range result {
		if value != EvidenceUnsupported && !capabilityEnabled(capabilities, name) {
			result[name] = EvidenceUnsupported
		}
	}
	return result
}
func capabilityEnabled(c ProviderCapabilities, name string) bool {
	values := map[string]bool{"chat": c.Chat, "streaming": c.Streaming, "embeddings": c.Embeddings, "tools": c.Tools, "vision": c.Vision, "json_mode": c.JSONMode, "developer_role": c.DeveloperRole, "reasoning": c.Reasoning, "stream_usage": c.StreamUsage, "moderations": c.Moderations, "images": c.Images, "transcriptions": c.Transcriptions, "speech": c.Speech, "files": c.Files, "batches": c.Batches, "rerank": c.Rerank, "async_generate": c.AsyncGenerate}
	return values[name]
}

// IsImmutableCapabilityProfile reports whether a profile's capability set is
// fixed by the build rather than declared by the operator. For these profiles
// DefaultProviderCapabilitiesForProfile is a ceiling, not a starting point: a
// binding may narrow it, never widen it.
//
// This is the single spelling of that list. It used to live in the Admin layer
// alone, which meant the ceiling was only enforced where the Admin API happened
// to look — the three Bedrock Mantle profiles were missing from it, so an
// operator could declare Mantle capabilities beyond what the profile supports
// and have them reach capability detection and the data plane.
func IsImmutableCapabilityProfile(id ProviderProfileID) bool {
	switch id {
	case ProfileOpenAIMediaResources,
		ProfileBedrockInvokeTitanEmbedV2,
		ProfileBedrockInvokeTitanImageV2,
		ProfileBedrockAgentRerankCohere35,
		ProfileBedrockAsyncNovaReel,
		ProfileBedrockMantleOpenAIChat,
		ProfileBedrockMantleOpenAIResponses,
		ProfileBedrockMantleAnthropicMessages:
		return true
	default:
		return false
	}
}

// ProviderCapabilitiesSubset is the single authoritative subset check used at
// API and storage boundaries.
func ProviderCapabilitiesSubset(candidate, available ProviderCapabilities) bool {
	return (!candidate.Chat || available.Chat) &&
		(!candidate.Streaming || available.Streaming) &&
		(!candidate.Embeddings || available.Embeddings) &&
		(!candidate.Moderations || available.Moderations) &&
		(!candidate.Images || available.Images) &&
		(!candidate.Transcriptions || available.Transcriptions) &&
		(!candidate.Speech || available.Speech) &&
		(!candidate.Files || available.Files) &&
		(!candidate.Batches || available.Batches) &&
		(!candidate.Rerank || available.Rerank) &&
		(!candidate.AsyncGenerate || available.AsyncGenerate) &&
		(!candidate.Tools || available.Tools) &&
		(!candidate.Vision || available.Vision) &&
		(!candidate.JSONMode || available.JSONMode) &&
		(!candidate.DeveloperRole || available.DeveloperRole) &&
		(!candidate.Reasoning || available.Reasoning) &&
		(!candidate.StreamUsage || available.StreamUsage) &&
		capabilityLimitSubset(candidate.MaxContextTokens, available.MaxContextTokens) &&
		capabilityLimitSubset(candidate.MaxOutputTokens, available.MaxOutputTokens)
}

func capabilityLimitSubset(candidate, available int64) bool {
	if available == 0 {
		return candidate >= 0
	}
	return candidate > 0 && candidate <= available
}

func (e CapabilityEvidenceSet) Clone() CapabilityEvidenceSet {
	result := make(CapabilityEvidenceSet, len(e))
	for name, value := range e {
		result[name] = value
	}
	return result
}

func (e CapabilityEvidenceSet) Validate(capabilities ProviderCapabilities) error {
	if len(e) == 0 {
		return errors.New("capability evidence is required")
	}
	known := make(map[string]struct{}, len(capabilityNames))
	for _, name := range capabilityNames {
		known[name] = struct{}{}
		if _, ok := e[name]; !ok {
			return fmt.Errorf("capability evidence for %s is required", name)
		}
	}
	for name, value := range e {
		if _, ok := known[name]; !ok {
			return fmt.Errorf("unknown capability evidence %q", name)
		}
		switch value {
		case EvidenceVerified, EvidenceDeclared, EvidenceUnsupported:
		default:
			return fmt.Errorf("invalid capability evidence %q for %s", value, name)
		}
	}
	values := map[string]bool{
		"chat": capabilities.Chat, "streaming": capabilities.Streaming,
		"embeddings": capabilities.Embeddings, "tools": capabilities.Tools,
		"vision": capabilities.Vision, "json_mode": capabilities.JSONMode,
		"developer_role": capabilities.DeveloperRole, "reasoning": capabilities.Reasoning,
		"stream_usage": capabilities.StreamUsage,
		"moderations":  capabilities.Moderations, "images": capabilities.Images, "transcriptions": capabilities.Transcriptions, "speech": capabilities.Speech, "files": capabilities.Files, "batches": capabilities.Batches, "rerank": capabilities.Rerank, "async_generate": capabilities.AsyncGenerate,
	}
	for name, enabled := range values {
		if enabled && e[name] == EvidenceUnsupported {
			return fmt.Errorf("enabled capability %s cannot be unsupported", name)
		}
		if !enabled && e[name] != EvidenceUnsupported {
			return fmt.Errorf("disabled capability %s must be unsupported", name)
		}
	}
	return nil
}

func (e CapabilityEvidenceSet) Satisfies(name string, minimum CapabilityEvidence) bool {
	actual, ok := e[name]
	if !ok || actual == EvidenceUnsupported {
		return false
	}
	if evidenceRank(minimum) == 0 {
		return false
	}
	return evidenceRank(actual) >= evidenceRank(minimum)
}

// evidenceRank orders the tiers. Unsupported and anything unrecognised are 0,
// which Satisfies treats as "does not meet any minimum" — the fail-closed
// reading, and the one an on-disk value from an older build now gets.
func evidenceRank(value CapabilityEvidence) int {
	switch value {
	case EvidenceDeclared:
		return 1
	case EvidenceVerified:
		return 2
	default:
		return 0
	}
}

func ValidateProviderProfile(providerType ProviderType, surface AccessSurface, profile ProviderProfileID, scheme CredentialScheme) error {
	registeredType, registered, ok := RegisteredProviderProfile(profile)
	if !ok {
		return errors.New("provider profile is not registered")
	}
	if strings.TrimSpace(string(surface)) == "" || strings.TrimSpace(string(profile)) == "" || strings.TrimSpace(string(scheme)) == "" {
		return errors.New("provider access surface, profile, and credential scheme are required")
	}
	// Defaults select new records. Validation resolves the supplied immutable ID
	// so multiple explicitly registered versions can coexist on one surface.
	if providerType != registeredType || surface != registered.AccessSurface || scheme != registered.CredentialScheme {
		return errors.New("provider access surface, profile, or credential scheme is incompatible")
	}
	return nil
}
