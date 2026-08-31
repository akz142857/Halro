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
	// One surface for all three MiniMax wire shapes, the same choice Bedrock
	// Mantle makes. An Access Surface names the API face one credential reaches,
	// not the wire format spoken on it: MiniMax serves Anthropic Messages,
	// OpenAI Chat Completions and OpenAI Responses from one host on one bearer
	// key, so splitting it into three would put the same credential in three
	// unrelated connection groups and make an operator create three connections
	// for one key.
	SurfaceMiniMax AccessSurface = "minimax-api"
)

const (
	ProfileOpenAIChatEmbeddings       ProviderProfileID = "openai.chat-embeddings.v1"
	ProfileOpenAIResponses            ProviderProfileID = "openai.responses.v1"
	ProfileAnthropicMessages          ProviderProfileID = "anthropic.messages.2023-06-01"
	ProfileAzureChatEmbeddings        ProviderProfileID = "azure-openai.chat-embeddings.v1"
	ProfileDeepSeekChat               ProviderProfileID = "deepseek.chat.v1"
	ProfileOpenAICompatible           ProviderProfileID = "openai-compatible.chat-embeddings.v1"
	ProfileGeminiText                 ProviderProfileID = "gemini.generate-content.text.v1beta"
	ProfileBedrockConverseText        ProviderProfileID = "bedrock.runtime.converse.text.v1"
	ProfileBedrockInvokeTitanEmbedV2  ProviderProfileID = "bedrock.runtime.invoke.titan-embed-text-v2.v1"
	ProfileOpenAIMediaResources       ProviderProfileID = "openai.media-resources.v1"
	ProfileBedrockInvokeTitanImageV2  ProviderProfileID = "bedrock.runtime.invoke.titan-image-v2.v1"
	ProfileBedrockAgentRerankCohere35 ProviderProfileID = "bedrock.agent-runtime.rerank.cohere-v3-5.v1"
	ProfileBedrockAsyncNovaReel       ProviderProfileID = "bedrock.runtime.async.nova-reel-v1.v1"
	// Bedrock Mantle serves one host through three routes, and a model reaches
	// exactly one of them. /v1 and /openai/v1 both speak the OpenAI wire shape,
	// so the wire shape cannot pick the route and the model list does not carry
	// it either — GET /v1/models/{id} returns id, status, owned_by and
	// data_retention, and no route. The route is therefore fixed by the profile,
	// one profile per (route, wire shape). Measured 2026-08-21 against a real
	// account, 50 models: 38 on /v1, 11 on /openai/v1, 1 on /anthropic/v1.
	ProfileBedrockMantleChat              ProviderProfileID = "bedrock.mantle.chat.v1"
	ProfileBedrockMantleResponses         ProviderProfileID = "bedrock.mantle.responses.v1"
	ProfileBedrockMantleOpenAIChat        ProviderProfileID = "bedrock.mantle.openai.chat.v1"
	ProfileBedrockMantleOpenAIResponses   ProviderProfileID = "bedrock.mantle.openai.responses.v1"
	ProfileBedrockMantleAnthropicMessages ProviderProfileID = "bedrock.mantle.anthropic.messages.v1"
	// MiniMax serves one host through three routes, one wire shape each:
	// /anthropic/v1/messages, /v1/chat/completions and /v1/responses. The route
	// is fixed by the profile rather than by the request, so a deployment says
	// which face it addresses instead of inferring it from a model identifier.
	ProfileMiniMaxAnthropicMessages ProviderProfileID = "minimax.anthropic.messages.v1"
	ProfileMiniMaxChat              ProviderProfileID = "minimax.chat.v1"
	ProfileMiniMaxResponses         ProviderProfileID = "minimax.responses.v1"
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

type CapabilityEvidenceSet map[string]CapabilityEvidence

type ProviderProfileDefaults struct {
	AccessSurface    AccessSurface
	ProfileID        ProviderProfileID
	CredentialScheme CredentialScheme
}

func DefaultProviderProfile(providerType ProviderType) (ProviderProfileDefaults, bool) {
	typeRow, ok := providerTypeIndex[providerType]
	if !ok {
		return ProviderProfileDefaults{}, false
	}
	row, ok := profileIndex[typeRow.DefaultProfile]
	if !ok {
		return ProviderProfileDefaults{}, false
	}
	return ProviderProfileDefaults{row.Surface, row.ID, row.Scheme}, true
}

func RegisteredProviderProfile(profile ProviderProfileID) (ProviderType, ProviderProfileDefaults, bool) {
	row, ok := profileIndex[profile]
	if !ok {
		return "", ProviderProfileDefaults{}, false
	}
	return row.Type, ProviderProfileDefaults{row.Surface, row.ID, row.Scheme}, true
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
	// Table order is the precedence: several profiles share one (type, surface,
	// scheme), and a stored credential must resolve to the same one every time.
	for _, row := range profileTable {
		if row.Type == providerType && row.Surface == surface && row.Scheme == scheme {
			return ProviderProfileDefaults{row.Surface, row.ID, row.Scheme}, true
		}
	}
	return ProviderProfileDefaults{}, false
}

func EvidenceForCapabilities(capabilities ProviderCapabilities, enabled CapabilityEvidence) CapabilityEvidenceSet {
	result := make(CapabilityEvidenceSet, len(capabilityFields))
	for _, field := range capabilityFields {
		if *field.Value(&capabilities) {
			result[field.Name] = enabled
		} else {
			result[field.Name] = EvidenceUnsupported
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
	value, _ := CapabilityValue(c, name)
	return value
}

// MaxBedrockProjectIDLength bounds the stored identifier. AWS project IDs are
// short (`proj_` plus a generated suffix); the bound exists so a pasted blob
// cannot become a request header.
const MaxBedrockProjectIDLength = 128

// ValidateBedrockProjectID accepts the identifiers AWS actually issues for a
// Bedrock Project and refuses everything else.
//
// A Bedrock project ID is `proj_` followed by alphanumerics. The literal
// `default` names the account's default project, which is what an empty value
// already means, so callers normalise it away rather than send a header that
// changes nothing.
//
// `wrkspc_` is refused by name. That prefix belongs to Claude Platform on AWS —
// a different service, on a different host — and pasting one product's
// identifier into the other's field is the most likely mistake here. The prefix
// is the whole of the distinction on the wire: both products carry this resource
// in an `anthropic-workspace-id` header, and only the identifier says which one
// the value is for. So this check is not a convenience, it is where the boundary
// between the two products is actually drawn, and the error says which product
// the value came from.
const (
	// MaxAnthropicBetaTokenLength bounds one token. Anthropic's are short
	// (`feature-name-YYYY-MM-DD`); the bound exists so a pasted blob cannot
	// become a request header.
	MaxAnthropicBetaTokenLength = 128
	MaxAnthropicBetaTokens      = 16
)

// ValidateAnthropicBetaTokens bounds the allowlist and constrains each entry to
// the shape Anthropic actually issues. The charset matters because these values
// are concatenated into an outbound header: anything that could carry a comma,
// newline, or whitespace would let one stored token smuggle in others.
func ValidateAnthropicBetaTokens(values []string) error {
	if len(values) > MaxAnthropicBetaTokens {
		return errors.New("too many anthropic beta tokens")
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" || len(value) > MaxAnthropicBetaTokenLength {
			return errors.New("anthropic beta token is empty or too long")
		}
		if _, duplicate := seen[value]; duplicate {
			return errors.New("anthropic beta tokens must be unique")
		}
		seen[value] = struct{}{}
		for _, char := range value {
			switch {
			case char >= 'a' && char <= 'z', char >= '0' && char <= '9', char == '-', char == '.', char == '_':
			default:
				return errors.New("anthropic beta token must be lowercase alphanumerics, dashes, dots, or underscores")
			}
		}
	}
	return nil
}

func ValidateBedrockProjectID(value string) error {
	if value == "" {
		return nil
	}
	if len(value) > MaxBedrockProjectIDLength {
		return errors.New("bedrock project id is too long")
	}
	if strings.HasPrefix(value, "wrkspc_") {
		return errors.New("bedrock project id looks like a Claude Platform on AWS workspace id, which belongs to a different service")
	}
	suffix, ok := strings.CutPrefix(value, "proj_")
	if !ok || suffix == "" {
		return errors.New("bedrock project id must be `proj_` followed by alphanumerics")
	}
	for _, char := range suffix {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
		default:
			return errors.New("bedrock project id must be `proj_` followed by alphanumerics")
		}
	}
	return nil
}

// NormalizeBedrockProjectID turns the ways of saying "the account default" into
// the one stored spelling: empty. `default` is the ID AWS lists for that
// project, and sending it as a header is indistinguishable from sending none.
func NormalizeBedrockProjectID(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "default" {
		return ""
	}
	return trimmed
}

// IsImmutableCapabilityProfile reports whether a profile's capability set is
// fixed by the build rather than declared by the operator. For these profiles
// DefaultProviderCapabilitiesForProfile is a ceiling, not a starting point: a
// binding may narrow it, never widen it.
//
// This is the single spelling of that list. It used to live in the Admin layer
// alone, which meant the ceiling was only enforced where the Admin API happened
// to look — the Bedrock Mantle profiles were missing from it, so an operator
// could declare Mantle capabilities beyond what the profile supports and have
// them reach capability detection and the data plane.
func IsImmutableCapabilityProfile(id ProviderProfileID) bool {
	return profileIndex[id].Immutable
}

// IsWithheldProfile reports whether this build offers the profile at all.
//
// A withheld profile is implemented and still walked by the invariant tests, but
// it is absent from the served matrix and refused on every write, so nothing new
// can be created on it. Reads are deliberately untouched: an install that
// already holds a withheld connection must still start, or the operator cannot
// delete it. See profileRow.
func IsWithheldProfile(id ProviderProfileID) bool {
	return profileIndex[id].Withheld
}

// IsBedrockMantleProfile reports whether a profile addresses the Bedrock Mantle
// surface. Mantle spans five profiles across three routes, and spelling that
// list out at each call site is how one of them gets left behind — which is the
// same mistake IsImmutableCapabilityProfile above exists to prevent.
func IsBedrockMantleProfile(id ProviderProfileID) bool {
	return profileIndex[id].Surface == SurfaceBedrockMantle
}

// ProviderCapabilitiesSubset is the single authoritative subset check used at
// API and storage boundaries.
func ProviderCapabilitiesSubset(candidate, available ProviderCapabilities) bool {
	for _, field := range capabilityFields {
		if *field.Value(&candidate) && !*field.Value(&available) {
			return false
		}
	}
	return capabilityLimitSubset(candidate.MaxContextTokens, available.MaxContextTokens) &&
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
	for _, field := range capabilityFields {
		enabled := *field.Value(&capabilities)
		if enabled && e[field.Name] == EvidenceUnsupported {
			return fmt.Errorf("enabled capability %s cannot be unsupported", field.Name)
		}
		if !enabled && e[field.Name] != EvidenceUnsupported {
			return fmt.Errorf("disabled capability %s must be unsupported", field.Name)
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

// ProfileSendsAnthropicBetas reports the profiles whose data path can actually
// emit an anthropic-beta header. Only the native Anthropic Messages surface
// forwards the caller's request bytes unchanged, which is the premise a beta
// token describes; the portable path rewrites the body, so a token claiming
// "this is the request I sent" would be false there.
func ProfileSendsAnthropicBetas(id ProviderProfileID) bool {
	switch id {
	case ProfileAnthropicMessages, ProfileBedrockMantleAnthropicMessages:
		return true
	default:
		return false
	}
}

// NormalizeAnthropicBetaTokens trims each entry and drops empties, so an
// operator pasting a comma-separated list with stray spaces stores the same set
// as one who typed them individually.
func NormalizeAnthropicBetaTokens(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}
