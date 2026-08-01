package compatibility

import (
	"errors"
	"slices"
	"strings"

	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/semantic"
)

type CompatibilityStatus string

const (
	StatusUnsupported       CompatibilityStatus = "unsupported"
	StatusExperimental      CompatibilityStatus = "experimental"
	StatusCompatible        CompatibilityStatus = "compatible"
	StatusNativePassThrough CompatibilityStatus = "native-pass-through"
)

// ProfileCoverage makes provider-specific loss explicit. An empty unsupported
// list is an affirmative claim that all documented northbound fields can be
// represented by that provider profile.
type ProfileCoverage struct {
	ProfileID                domain.ProviderProfileID `json:"profile_id"`
	UnsupportedRequestFields []string                 `json:"unsupported_request_fields,omitempty"`
	DeclaredTransforms       []string                 `json:"declared_transforms,omitempty"`
}

type EndpointCompatibilityManifest struct {
	ID                   string                     `json:"id"`
	NorthboundProfile    NorthboundProfileID        `json:"northbound_profile"`
	ProfileRevision      uint64                     `json:"profile_revision"`
	Protocol             string                     `json:"protocol"`
	Method               string                     `json:"method"`
	Path                 string                     `json:"path"`
	SemanticOperation    semantic.Operation         `json:"semantic_operation"`
	RequestFields        []string                   `json:"request_fields"`
	RequestHeaders       []string                   `json:"request_headers"`
	ResponseFields       []string                   `json:"response_fields"`
	StreamEvents         []string                   `json:"stream_events,omitempty"`
	StateSemantics       string                     `json:"state_semantics"`
	SDKMatrix            []string                   `json:"sdk_matrix"`
	Status               CompatibilityStatus        `json:"status"`
	DocumentedDeviations []string                   `json:"documented_deviations,omitempty"`
	ProviderProfiles     []domain.ProviderProfileID `json:"provider_profiles"`
	ProfileCoverage      []ProfileCoverage          `json:"profile_coverage"`
}

func (manifest EndpointCompatibilityManifest) Validate() error {
	if manifest.ID == "" || manifest.NorthboundProfile == "" || manifest.ProfileRevision == 0 || manifest.Protocol == "" || manifest.Method == "" || !strings.HasPrefix(manifest.Path, "/") || manifest.SemanticOperation.Validate() != nil || manifest.StateSemantics == "" || len(manifest.RequestFields) == 0 || len(manifest.ResponseFields) == 0 || len(manifest.SDKMatrix) == 0 || len(manifest.ProviderProfiles) == 0 {
		return errors.New("endpoint compatibility manifest is incomplete")
	}
	switch manifest.Status {
	case StatusUnsupported, StatusExperimental, StatusCompatible, StatusNativePassThrough:
	default:
		return errors.New("endpoint compatibility status is invalid")
	}
	if manifest.Status == StatusCompatible && len(manifest.DocumentedDeviations) == 0 {
		return errors.New("compatible endpoint must explicitly document deviations or state none")
	}
	for _, list := range [][]string{manifest.RequestFields, manifest.RequestHeaders, manifest.ResponseFields, manifest.StreamEvents, manifest.SDKMatrix, manifest.DocumentedDeviations} {
		if hasEmptyOrDuplicate(list) {
			return errors.New("endpoint compatibility manifest contains empty or duplicate values")
		}
	}
	profiles := make(map[domain.ProviderProfileID]struct{}, len(manifest.ProviderProfiles))
	for _, profileID := range manifest.ProviderProfiles {
		if _, _, ok := domain.RegisteredProviderProfile(profileID); !ok {
			return errors.New("endpoint compatibility manifest references an unknown provider profile")
		}
		if _, duplicate := profiles[profileID]; duplicate {
			return errors.New("endpoint compatibility manifest contains duplicate provider profiles")
		}
		profiles[profileID] = struct{}{}
	}
	if len(manifest.ProfileCoverage) != len(profiles) {
		return errors.New("endpoint compatibility manifest must declare coverage for every provider profile")
	}
	covered := make(map[domain.ProviderProfileID]struct{}, len(manifest.ProfileCoverage))
	requestFields := make(map[string]struct{}, len(manifest.RequestFields))
	for _, field := range manifest.RequestFields {
		requestFields[field] = struct{}{}
	}
	for _, coverage := range manifest.ProfileCoverage {
		if _, ok := profiles[coverage.ProfileID]; !ok {
			return errors.New("endpoint compatibility coverage references an undeclared provider profile")
		}
		if _, duplicate := covered[coverage.ProfileID]; duplicate {
			return errors.New("endpoint compatibility manifest contains duplicate profile coverage")
		}
		covered[coverage.ProfileID] = struct{}{}
		if hasEmptyOrDuplicate(coverage.UnsupportedRequestFields) || hasEmptyOrDuplicate(coverage.DeclaredTransforms) {
			return errors.New("endpoint compatibility profile coverage contains empty or duplicate values")
		}
		for _, field := range coverage.UnsupportedRequestFields {
			if _, ok := requestFields[field]; !ok {
				return errors.New("endpoint compatibility coverage references an unknown request field")
			}
		}
	}
	return nil
}

func BuiltinEndpointManifests() []EndpointCompatibilityManifest {
	chatProfiles := []domain.ProviderProfileID{domain.ProfileOpenAIChatEmbeddings, domain.ProfileAzureChatEmbeddings, domain.ProfileDeepSeekChat, domain.ProfileOpenAICompatible, domain.ProfileGeminiText, domain.ProfileBedrockConverseText}
	embedProfiles := []domain.ProviderProfileID{domain.ProfileOpenAIChatEmbeddings, domain.ProfileAzureChatEmbeddings, domain.ProfileOpenAICompatible, domain.ProfileGeminiText}
	chatCoverage := []ProfileCoverage{
		{ProfileID: domain.ProfileOpenAIChatEmbeddings},
		{ProfileID: domain.ProfileAzureChatEmbeddings},
		{ProfileID: domain.ProfileDeepSeekChat},
		{ProfileID: domain.ProfileOpenAICompatible},
		{ProfileID: domain.ProfileGeminiText, UnsupportedRequestFields: []string{"messages[].name", "seed", "tools", "tool_choice", "parallel_tool_calls", "response_format", "reasoning_effort", "user"}, DeclaredTransforms: []string{"developer messages are merged into Gemini system_instruction"}},
		{ProfileID: domain.ProfileBedrockConverseText, UnsupportedRequestFields: []string{"messages[].name", "n", "seed", "tools", "tool_choice", "parallel_tool_calls", "response_format", "reasoning_effort", "user"}, DeclaredTransforms: []string{"Bedrock stop reasons are normalized to OpenAI finish reasons"}},
	}
	embedCoverage := []ProfileCoverage{
		{ProfileID: domain.ProfileOpenAIChatEmbeddings},
		{ProfileID: domain.ProfileAzureChatEmbeddings},
		{ProfileID: domain.ProfileOpenAICompatible},
		{ProfileID: domain.ProfileGeminiText, UnsupportedRequestFields: []string{"encoding_format", "user"}, DeclaredTransforms: []string{"token usage is locally estimated when Gemini omits usage"}},
	}
	return []EndpointCompatibilityManifest{
		{ID: "openai.chat-completions.v1", NorthboundProfile: ProfileOpenAIChatCompletions, ProfileRevision: 1, Protocol: "openai", Method: "POST", Path: "/v1/chat/completions", SemanticOperation: semantic.OperationGenerate, RequestFields: []string{"model", "messages", "messages[].name", "stream", "stream_options", "temperature", "top_p", "max_tokens", "max_completion_tokens", "n", "stop", "seed", "tools", "tool_choice", "parallel_tool_calls", "response_format", "reasoning_effort", "user"}, RequestHeaders: []string{"Authorization", "Content-Type"}, ResponseFields: []string{"id", "object", "created", "model", "choices", "usage"}, StreamEvents: []string{"chat.completion.chunk", "[DONE]", "error"}, StateSemantics: "stateless", SDKMatrix: []string{"openai-go", "openai-node", "openai-python"}, Status: StatusCompatible, DocumentedDeviations: []string{"gateway routes model names; provider-owned chat state is not exposed", "provider-specific unsupported fields are rejected before provider I/O"}, ProviderProfiles: chatProfiles, ProfileCoverage: chatCoverage},
		{ID: "openai.embeddings.v1", NorthboundProfile: ProfileOpenAIEmbeddings, ProfileRevision: 1, Protocol: "openai", Method: "POST", Path: "/v1/embeddings", SemanticOperation: semantic.OperationEmbed, RequestFields: []string{"model", "input", "encoding_format", "dimensions", "user"}, RequestHeaders: []string{"Authorization", "Content-Type"}, ResponseFields: []string{"object", "data", "model", "usage"}, StateSemantics: "stateless", SDKMatrix: []string{"openai-go", "openai-node", "openai-python"}, Status: StatusCompatible, DocumentedDeviations: []string{"gateway routes model names", "provider-specific unsupported fields are rejected before provider I/O"}, ProviderProfiles: embedProfiles, ProfileCoverage: embedCoverage},
	}
}

func CloneEndpointManifest(manifest EndpointCompatibilityManifest) EndpointCompatibilityManifest {
	manifest.RequestFields = slices.Clone(manifest.RequestFields)
	manifest.RequestHeaders = slices.Clone(manifest.RequestHeaders)
	manifest.ResponseFields = slices.Clone(manifest.ResponseFields)
	manifest.StreamEvents = slices.Clone(manifest.StreamEvents)
	manifest.SDKMatrix = slices.Clone(manifest.SDKMatrix)
	manifest.DocumentedDeviations = slices.Clone(manifest.DocumentedDeviations)
	manifest.ProviderProfiles = slices.Clone(manifest.ProviderProfiles)
	manifest.ProfileCoverage = slices.Clone(manifest.ProfileCoverage)
	for index := range manifest.ProfileCoverage {
		manifest.ProfileCoverage[index].UnsupportedRequestFields = slices.Clone(manifest.ProfileCoverage[index].UnsupportedRequestFields)
		manifest.ProfileCoverage[index].DeclaredTransforms = slices.Clone(manifest.ProfileCoverage[index].DeclaredTransforms)
	}
	return manifest
}
func hasEmptyOrDuplicate(values []string) bool {
	seen := map[string]struct{}{}
	for _, value := range values {
		if value == "" {
			return true
		}
		if _, ok := seen[value]; ok {
			return true
		}
		seen[value] = struct{}{}
	}
	return false
}
