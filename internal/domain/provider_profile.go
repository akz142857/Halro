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
	SurfaceOpenAI           AccessSurface = "openai-api"
	SurfaceAzureOpenAI      AccessSurface = "azure-openai"
	SurfaceDeepSeek         AccessSurface = "deepseek-api"
	SurfaceOpenAICompatible AccessSurface = "openai-compatible"
	SurfaceGemini           AccessSurface = "gemini-generate-content"
	SurfaceBedrockRuntime   AccessSurface = "bedrock-runtime"
)

const (
	ProfileOpenAIChatEmbeddings ProviderProfileID = "openai.chat-embeddings.v1"
	ProfileAzureChatEmbeddings  ProviderProfileID = "azure-openai.chat-embeddings.v1"
	ProfileDeepSeekChat         ProviderProfileID = "deepseek.chat.v1"
	ProfileOpenAICompatible     ProviderProfileID = "openai-compatible.chat-embeddings.v1"
	ProfileGeminiText           ProviderProfileID = "gemini.generate-content.text.v1beta"
	ProfileBedrockConverseText  ProviderProfileID = "bedrock.runtime.converse.text.v1"
)

const (
	CredentialBearerStatic     CredentialScheme = "bearer.static"
	CredentialAzureAPIKey      CredentialScheme = "azure.api-key"
	CredentialGoogleAPIKey     CredentialScheme = "google.api-key"
	CredentialAWSSigV4Explicit CredentialScheme = "aws.sigv4.explicit-session"
)

const (
	EvidenceVerified    CapabilityEvidence = "verified"
	EvidenceDeclared    CapabilityEvidence = "declared"
	EvidenceLegacy      CapabilityEvidence = "legacy"
	EvidenceUnsupported CapabilityEvidence = "unsupported"
)

var capabilityNames = []string{
	"chat", "streaming", "embeddings", "tools", "vision", "json_mode",
	"developer_role", "reasoning", "stream_usage",
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
	case ProfileOpenAIChatEmbeddings:
		return ProviderOpenAI, ProviderProfileDefaults{SurfaceOpenAI, profile, CredentialBearerStatic}, true
	case ProfileAzureChatEmbeddings:
		return ProviderAzureOpenAI, ProviderProfileDefaults{SurfaceAzureOpenAI, profile, CredentialAzureAPIKey}, true
	case ProfileDeepSeekChat:
		return ProviderDeepSeek, ProviderProfileDefaults{SurfaceDeepSeek, profile, CredentialBearerStatic}, true
	case ProfileOpenAICompatible:
		return ProviderOpenAICompatible, ProviderProfileDefaults{SurfaceOpenAICompatible, profile, CredentialBearerStatic}, true
	case ProfileGeminiText:
		return ProviderGemini, ProviderProfileDefaults{SurfaceGemini, profile, CredentialGoogleAPIKey}, true
	case ProfileBedrockConverseText:
		return ProviderBedrock, ProviderProfileDefaults{SurfaceBedrockRuntime, profile, CredentialAWSSigV4Explicit}, true
	default:
		return "", ProviderProfileDefaults{}, false
	}
}

func EvidenceForCapabilities(capabilities ProviderCapabilities, enabled CapabilityEvidence) CapabilityEvidenceSet {
	result := make(CapabilityEvidenceSet, len(capabilityNames))
	values := map[string]bool{
		"chat": capabilities.Chat, "streaming": capabilities.Streaming,
		"embeddings": capabilities.Embeddings, "tools": capabilities.Tools,
		"vision": capabilities.Vision, "json_mode": capabilities.JSONMode,
		"developer_role": capabilities.DeveloperRole, "reasoning": capabilities.Reasoning,
		"stream_usage": capabilities.StreamUsage,
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
		case EvidenceVerified, EvidenceDeclared, EvidenceLegacy, EvidenceUnsupported:
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

func evidenceRank(value CapabilityEvidence) int {
	switch value {
	case EvidenceLegacy:
		return 1
	case EvidenceDeclared:
		return 2
	case EvidenceVerified:
		return 3
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
