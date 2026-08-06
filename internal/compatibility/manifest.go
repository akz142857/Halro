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
	Status                   CompatibilityStatus      `json:"status"`
	UnsupportedRequestFields []string                 `json:"unsupported_request_fields,omitempty"`
	DeclaredTransforms       []string                 `json:"declared_transforms,omitempty"`
}

type EndpointCompatibilityManifest struct {
	ID                    string                     `json:"id"`
	NorthboundProfile     NorthboundProfileID        `json:"northbound_profile"`
	ProfileRevision       uint64                     `json:"profile_revision"`
	Protocol              string                     `json:"protocol"`
	Method                string                     `json:"method"`
	Path                  string                     `json:"path"`
	SemanticOperation     semantic.Operation         `json:"semantic_operation"`
	RequestFields         []string                   `json:"request_fields"`
	RejectedRequestFields []string                   `json:"rejected_request_fields,omitempty"`
	RequestHeaders        []string                   `json:"request_headers"`
	ResponseFields        []string                   `json:"response_fields"`
	StreamEvents          []string                   `json:"stream_events,omitempty"`
	StateSemantics        string                     `json:"state_semantics"`
	SDKMatrix             []string                   `json:"sdk_matrix"`
	Status                CompatibilityStatus        `json:"status"`
	DocumentedDeviations  []string                   `json:"documented_deviations,omitempty"`
	ProviderProfiles      []domain.ProviderProfileID `json:"provider_profiles"`
	ProfileCoverage       []ProfileCoverage          `json:"profile_coverage"`
}

func (manifest EndpointCompatibilityManifest) Validate() error {
	if manifest.ID == "" || manifest.NorthboundProfile == "" || manifest.ProfileRevision == 0 || manifest.Protocol == "" || manifest.Method == "" || !strings.HasPrefix(manifest.Path, "/") || manifest.SemanticOperation.Validate() != nil || manifest.StateSemantics == "" || len(manifest.RequestFields) == 0 || len(manifest.ResponseFields) == 0 || len(manifest.ProviderProfiles) == 0 {
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
	if manifest.Status == StatusCompatible && len(manifest.SDKMatrix) == 0 {
		return errors.New("compatible endpoint must have a validated SDK matrix")
	}
	northbound, ok := BuiltinNorthboundProfile(manifest.NorthboundProfile)
	if !ok {
		return errors.New("endpoint compatibility manifest references an unknown northbound profile")
	}
	if northbound.Protocol != manifest.Protocol || northbound.Revision != manifest.ProfileRevision {
		return errors.New("endpoint compatibility manifest does not match its northbound profile")
	}
	if !slices.Contains(northbound.Methods, manifest.Method+" "+manifest.Path) {
		return errors.New("endpoint compatibility method is not registered by its northbound profile")
	}
	for _, list := range [][]string{manifest.RequestFields, manifest.RejectedRequestFields, manifest.RequestHeaders, manifest.ResponseFields, manifest.StreamEvents, manifest.SDKMatrix, manifest.DocumentedDeviations} {
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
		switch coverage.Status {
		case StatusUnsupported, StatusExperimental, StatusCompatible, StatusNativePassThrough:
		default:
			return errors.New("endpoint compatibility profile coverage status is invalid")
		}
		if isInferenceResourcesProviderProfile(coverage.ProfileID) && coverage.Status != StatusExperimental {
			return errors.New("phase 2 provider profile must remain experimental until its release gates pass")
		}
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
	chatProfiles := []domain.ProviderProfileID{domain.ProfileOpenAIChatEmbeddings, domain.ProfileAnthropicMessages, domain.ProfileAzureChatEmbeddings, domain.ProfileDeepSeekChat, domain.ProfileOpenAICompatible, domain.ProfileGeminiText, domain.ProfileBedrockConverseText, domain.ProfileBedrockMantleOpenAIChat, domain.ProfileBedrockMantleOpenAIResponses, domain.ProfileBedrockMantleAnthropicMessages}
	embedProfiles := []domain.ProviderProfileID{domain.ProfileOpenAIChatEmbeddings, domain.ProfileAzureChatEmbeddings, domain.ProfileOpenAICompatible, domain.ProfileGeminiText, domain.ProfileBedrockInvokeTitanEmbedV2}
	chatCoverage := []ProfileCoverage{
		{ProfileID: domain.ProfileOpenAIChatEmbeddings},
		{ProfileID: domain.ProfileAnthropicMessages, UnsupportedRequestFields: []string{"messages[].name", "n", "seed", "response_format", "reasoning_effort", "user"}, DeclaredTransforms: []string{"portable Chat content is mapped to Anthropic Messages blocks"}},
		{ProfileID: domain.ProfileAzureChatEmbeddings},
		{ProfileID: domain.ProfileDeepSeekChat},
		{ProfileID: domain.ProfileOpenAICompatible},
		{ProfileID: domain.ProfileGeminiText, UnsupportedRequestFields: []string{"messages[].name", "seed", "tools", "tool_choice", "parallel_tool_calls", "response_format", "reasoning_effort", "user"}, DeclaredTransforms: []string{"developer messages are merged into Gemini system_instruction"}},
		{ProfileID: domain.ProfileBedrockConverseText, UnsupportedRequestFields: []string{"messages[].name", "n", "seed", "tools", "tool_choice", "parallel_tool_calls", "response_format", "reasoning_effort", "user"}, DeclaredTransforms: []string{"Bedrock stop reasons are normalized to OpenAI finish reasons"}},
		{ProfileID: domain.ProfileBedrockMantleOpenAIChat},
		{ProfileID: domain.ProfileBedrockMantleOpenAIResponses, UnsupportedRequestFields: []string{"n", "stop", "seed", "reasoning_effort"}, DeclaredTransforms: []string{"Chat messages are mapped to stateless Responses input items", "store=false is always sent upstream", "streaming requests with tools are rejected before provider I/O"}},
		{ProfileID: domain.ProfileBedrockMantleAnthropicMessages, UnsupportedRequestFields: []string{"messages[].name", "n", "seed", "response_format", "reasoning_effort", "user"}, DeclaredTransforms: []string{"portable Chat content is mapped to Bedrock Mantle Anthropic Messages blocks"}},
	}
	embedCoverage := []ProfileCoverage{
		{ProfileID: domain.ProfileOpenAIChatEmbeddings},
		{ProfileID: domain.ProfileAzureChatEmbeddings},
		{ProfileID: domain.ProfileOpenAICompatible},
		{ProfileID: domain.ProfileGeminiText, UnsupportedRequestFields: []string{"encoding_format", "user"}, DeclaredTransforms: []string{"token usage is locally estimated when Gemini omits usage"}},
		{ProfileID: domain.ProfileBedrockInvokeTitanEmbedV2, UnsupportedRequestFields: []string{"input", "encoding_format", "dimensions", "user"}, DeclaredTransforms: []string{"only one string input is accepted", "dimensions are limited to 256, 512, or 1024", "native requests force normalized float embeddings", "Bedrock inputTextTokenCount is mapped to OpenAI usage"}},
	}
	responseProfiles := slices.Clone(chatProfiles)
	responseCoverage := []ProfileCoverage{
		{ProfileID: domain.ProfileOpenAIChatEmbeddings, DeclaredTransforms: []string{"Responses items are mapped through the OpenAI Chat Completions ProviderPrimitive"}},
		{ProfileID: domain.ProfileAnthropicMessages, UnsupportedRequestFields: []string{"text.format", "user"}, DeclaredTransforms: []string{"Responses items are mapped through the Anthropic Messages ProviderPrimitive"}},
		{ProfileID: domain.ProfileAzureChatEmbeddings, DeclaredTransforms: []string{"Responses items are mapped through the Azure Chat Completions ProviderPrimitive"}},
		{ProfileID: domain.ProfileDeepSeekChat, DeclaredTransforms: []string{"Responses items are mapped through the DeepSeek Chat ProviderPrimitive"}},
		{ProfileID: domain.ProfileOpenAICompatible, DeclaredTransforms: []string{"Responses items are mapped through the compatible Chat Completions ProviderPrimitive"}},
		{ProfileID: domain.ProfileGeminiText, UnsupportedRequestFields: []string{"tools", "tool_choice", "parallel_tool_calls", "text.format", "user"}, DeclaredTransforms: []string{"instructions are mapped to a developer message and merged into Gemini system_instruction"}},
		{ProfileID: domain.ProfileBedrockConverseText, UnsupportedRequestFields: []string{"tools", "tool_choice", "parallel_tool_calls", "text.format", "user"}, DeclaredTransforms: []string{"instructions are mapped to a developer message", "Bedrock stop reasons are normalized to Responses status"}},
		{ProfileID: domain.ProfileBedrockMantleOpenAIChat, DeclaredTransforms: []string{"Responses items are mapped through Bedrock Mantle Chat Completions"}},
		{ProfileID: domain.ProfileBedrockMantleOpenAIResponses, DeclaredTransforms: []string{"stateless Responses are sent directly with store=false"}},
		{ProfileID: domain.ProfileBedrockMantleAnthropicMessages, UnsupportedRequestFields: []string{"text.format", "user"}, DeclaredTransforms: []string{"Responses items are mapped through Bedrock Mantle Anthropic Messages"}},
	}
	manifests := []EndpointCompatibilityManifest{
		{ID: "openai.chat-completions.v1", NorthboundProfile: ProfileOpenAIChatCompletions, ProfileRevision: 1, Protocol: "openai", Method: "POST", Path: "/v1/chat/completions", SemanticOperation: semantic.OperationGenerate, RequestFields: []string{"model", "messages", "messages[].name", "stream", "stream_options", "temperature", "top_p", "max_tokens", "max_completion_tokens", "n", "stop", "seed", "tools", "tool_choice", "parallel_tool_calls", "response_format", "reasoning_effort", "user"}, RequestHeaders: []string{"Authorization", "Content-Type"}, ResponseFields: []string{"id", "object", "created", "model", "choices", "usage"}, StreamEvents: []string{"chat.completion.chunk", "[DONE]", "error"}, StateSemantics: "stateless", SDKMatrix: []string{"openai-go", "openai-node", "openai-python"}, Status: StatusCompatible, DocumentedDeviations: []string{"gateway routes model names; provider-owned chat state is not exposed", "provider-specific unsupported fields are rejected before provider I/O"}, ProviderProfiles: chatProfiles, ProfileCoverage: chatCoverage},
		{ID: "openai.embeddings.v1", NorthboundProfile: ProfileOpenAIEmbeddings, ProfileRevision: 1, Protocol: "openai", Method: "POST", Path: "/v1/embeddings", SemanticOperation: semantic.OperationEmbed, RequestFields: []string{"model", "input", "encoding_format", "dimensions", "user"}, RequestHeaders: []string{"Authorization", "Content-Type"}, ResponseFields: []string{"object", "data", "model", "usage"}, StateSemantics: "stateless", SDKMatrix: []string{"openai-go", "openai-node", "openai-python"}, Status: StatusCompatible, DocumentedDeviations: []string{"gateway routes model names", "provider-specific unsupported fields are rejected before provider I/O"}, ProviderProfiles: embedProfiles, ProfileCoverage: embedCoverage},
		{ID: "openai.responses.stateless.v1", NorthboundProfile: ProfileOpenAIResponses, ProfileRevision: 1, Protocol: "openai", Method: "POST", Path: "/v1/responses", SemanticOperation: semantic.OperationGenerate,
			RequestFields:         []string{"model", "input", "input[].type", "input[].role", "input[].content", "input[].call_id", "input[].name", "input[].arguments", "input[].output", "input[].content[].type", "input[].content[].text", "input[].content[].image_url", "input[].content[].detail", "instructions", "stream", "store", "temperature", "top_p", "max_output_tokens", "tools", "tools[].type", "tools[].name", "tools[].description", "tools[].parameters", "tools[].strict", "tool_choice", "parallel_tool_calls", "text.format", "text.format.type", "text.format.name", "text.format.description", "text.format.schema", "text.format.strict", "user"},
			RejectedRequestFields: []string{"store=true", "previous_response_id", "conversation", "background", "prompt", "metadata", "include", "context_management", "service_tier", "truncation", "max_tool_calls", "prompt_cache_key", "prompt_cache_options", "prompt_cache_retention", "safety_identifier", "stream_options", "top_logprobs", "reasoning", "input[].id", "input[].type=unsupported", "tools[].type!=function", "tools[].strict=true", "stream=true with tools"},
			RequestHeaders:        []string{"Authorization", "Content-Type"}, ResponseFields: []string{"id", "object", "created_at", "completed_at", "status", "background", "error", "incomplete_details", "instructions", "max_output_tokens", "model", "output", "output[].id", "output[].type", "output[].status", "output[].role", "output[].content", "output[].call_id", "output[].name", "output[].arguments", "output[].content[].type", "output[].content[].text", "output[].content[].refusal", "output[].content[].annotations", "output[].content[].logprobs", "parallel_tool_calls", "previous_response_id", "reasoning", "store", "temperature", "text", "tool_choice", "tools", "top_p", "truncation", "usage", "usage.input_tokens", "usage.output_tokens", "usage.total_tokens"}, StreamEvents: []string{"response.created", "response.in_progress", "response.output_item.added", "response.content_part.added", "response.output_text.delta", "response.output_text.done", "response.content_part.done", "response.output_item.done", "response.completed", "response.incomplete", "error"}, StateSemantics: "stateless; omitted store is treated as false; stateful fields are rejected before provider I/O", SDKMatrix: []string{"openai-go", "openai-node", "openai-python"}, Status: StatusCompatible, DocumentedDeviations: []string{"only POST create is available; retrieval, deletion, cancellation, input_items, Conversations, background mode, and webhooks are unavailable", "store defaults to false and store=true is rejected", "hosted tools, strict function tools, reasoning output, and streaming function calls are rejected", "request instructions, tool definitions, tool choice, and structured schema bodies are returned as conservative null, empty, or default response metadata because the original Responses object has not passed through outbound redaction", "portable requests are translated through the selected profile's existing generation primitive", "unknown fields and unsupported item types are rejected before provider I/O"}, ProviderProfiles: responseProfiles, ProfileCoverage: responseCoverage},
		{ID: "anthropic.messages.2023-06-01", NorthboundProfile: ProfileAnthropicMessages, ProfileRevision: 1, Protocol: "anthropic", Method: "POST", Path: "/v1/messages", SemanticOperation: semantic.OperationGenerate,
			RequestFields: []string{"model", "max_tokens", "messages", "messages[].role", "messages[].content", "system", "stream", "stop_sequences", "temperature", "top_p", "top_k", "tools", "tool_choice", "thinking", "metadata", "service_tier"}, RejectedRequestFields: []string{"anthropic-beta", "hosted tools", "strict tools in portable mode", "signed thinking in portable mode", "unknown fields"}, RequestHeaders: []string{"x-api-key or Authorization", "anthropic-version", "Heimdall-Route-Mode", "Content-Type"}, ResponseFields: []string{"id", "type", "role", "content", "model", "stop_reason", "stop_sequence", "usage"}, StreamEvents: []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop", "ping", "error"}, StateSemantics: "stateless; portable is default; native pins one exact Anthropic-wire provider profile and disables cross-provider fallback", SDKMatrix: []string{"anthropic-go", "anthropic-typescript", "anthropic-python"}, Status: StatusCompatible, DocumentedDeviations: []string{"only anthropic-version 2023-06-01 is accepted", "anthropic-beta and hosted tools are unavailable", "Gateway Keys are accepted through x-api-key for official SDK compatibility and are never forwarded upstream", "native mode is selected with Heimdall-Route-Mode and requires either the direct Anthropic or Bedrock Mantle Anthropic profile"}, ProviderProfiles: chatProfiles, ProfileCoverage: []ProfileCoverage{
				{ProfileID: domain.ProfileOpenAIChatEmbeddings, UnsupportedRequestFields: []string{"top_k", "thinking", "metadata", "service_tier"}, DeclaredTransforms: []string{"portable Messages content is mapped through OpenAI Chat Completions"}},
				{ProfileID: domain.ProfileAnthropicMessages, DeclaredTransforms: []string{"native mode preserves validated Anthropic content blocks and events"}},
				{ProfileID: domain.ProfileAzureChatEmbeddings, UnsupportedRequestFields: []string{"top_k", "thinking", "metadata", "service_tier"}, DeclaredTransforms: []string{"portable Messages content is mapped through Azure Chat Completions"}},
				{ProfileID: domain.ProfileDeepSeekChat, UnsupportedRequestFields: []string{"top_k", "thinking", "metadata", "service_tier"}, DeclaredTransforms: []string{"portable Messages content is mapped through DeepSeek Chat Completions"}},
				{ProfileID: domain.ProfileOpenAICompatible, UnsupportedRequestFields: []string{"top_k", "thinking", "metadata", "service_tier"}, DeclaredTransforms: []string{"portable Messages content is mapped through an OpenAI-compatible primitive"}},
				{ProfileID: domain.ProfileGeminiText, UnsupportedRequestFields: []string{"top_k", "tools", "tool_choice", "thinking", "metadata", "service_tier"}, DeclaredTransforms: []string{"portable text Messages content is mapped through Gemini generateContent"}},
				{ProfileID: domain.ProfileBedrockConverseText, UnsupportedRequestFields: []string{"top_k", "tools", "tool_choice", "thinking", "metadata", "service_tier"}, DeclaredTransforms: []string{"portable text Messages content is mapped through Bedrock Converse"}},
				{ProfileID: domain.ProfileBedrockMantleOpenAIChat, UnsupportedRequestFields: []string{"top_k", "thinking", "metadata", "service_tier"}, DeclaredTransforms: []string{"portable Messages content is mapped through Bedrock Mantle Chat Completions"}},
				{ProfileID: domain.ProfileBedrockMantleOpenAIResponses, UnsupportedRequestFields: []string{"stop_sequences", "top_k", "thinking", "metadata", "service_tier"}, DeclaredTransforms: []string{"portable Messages content is mapped through stateless Bedrock Mantle Responses", "streaming requests with tools are rejected before provider I/O"}},
				{ProfileID: domain.ProfileBedrockMantleAnthropicMessages, DeclaredTransforms: []string{"native mode preserves validated Anthropic content blocks, thinking signatures, and events"}},
			}},
	}
	setProfileCompatibilityStatuses(manifests)
	return append(manifests, inferenceResourcesEndpointManifests()...)
}

func inferenceResourcesEndpointManifests() []EndpointCompatibilityManifest {
	openAI := domain.ProfileOpenAIMediaResources
	imageProfiles := []domain.ProviderProfileID{openAI, domain.ProfileBedrockInvokeTitanImageV2}
	makeManifest := func(id, method, path string, operation semantic.Operation, requestFields, responseFields []string, profiles []domain.ProviderProfileID, state string) EndpointCompatibilityManifest {
		coverage := make([]ProfileCoverage, len(profiles))
		for index, profileID := range profiles {
			coverage[index] = ProfileCoverage{ProfileID: profileID}
		}
		return EndpointCompatibilityManifest{ID: id, NorthboundProfile: ProfileOpenAIMediaResources, ProfileRevision: 1, Protocol: "openai", Method: method, Path: path, SemanticOperation: operation, RequestFields: requestFields, RequestHeaders: []string{"Authorization", "Content-Type", "Idempotency-Key when creating a resource"}, ResponseFields: responseFields, StateSemantics: state, Status: StatusExperimental, DocumentedDeviations: []string{"official SDK black-box matrix is not yet validated; current coverage is limited to gateway contracts and provider transport fixtures", "unknown fields and unsupported profile fields are rejected before provider I/O", "resource identifiers are opaque Heimdall identifiers scoped to one project"}, ProviderProfiles: profiles, ProfileCoverage: coverage}
	}
	manifests := []EndpointCompatibilityManifest{
		makeManifest("openai.moderations.v1", "POST", "/v1/moderations", semantic.OperationModerate, []string{"model", "input"}, []string{"id", "model", "results"}, []domain.ProviderProfileID{openAI}, "stateless"),
		makeManifest("openai.images.generations.v1", "POST", "/v1/images/generations", semantic.OperationImage, []string{"model", "prompt", "n", "quality", "response_format", "size", "style"}, []string{"created", "data", "data[].url", "data[].b64_json", "data[].revised_prompt"}, imageProfiles, "stateless"),
		makeManifest("openai.audio.transcriptions.v1", "POST", "/v1/audio/transcriptions", semantic.OperationTranscribe, []string{"file", "model", "language", "prompt", "response_format", "temperature"}, []string{"text"}, []domain.ProviderProfileID{openAI}, "stateless"),
		makeManifest("openai.audio.speech.v1", "POST", "/v1/audio/speech", semantic.OperationSynthesize, []string{"model", "input", "voice", "response_format", "speed"}, []string{"binary audio"}, []domain.ProviderProfileID{openAI}, "stateless"),
		makeManifest("openai.files.create.v1", "POST", "/v1/files", semantic.OperationFile, []string{"file", "purpose", "Heimdall-Route"}, []string{"id", "object", "bytes", "created_at", "filename", "purpose", "status", "status_details"}, []domain.ProviderProfileID{openAI}, "project-owned resource with 30 day TTL"),
		makeManifest("openai.files.get.v1", "GET", "/v1/files/{id}", semantic.OperationFile, []string{"id"}, []string{"id", "object", "bytes", "created_at", "filename", "purpose", "status", "status_details"}, []domain.ProviderProfileID{openAI}, "project-owned resource"),
		makeManifest("openai.files.content.v1", "GET", "/v1/files/{id}/content", semantic.OperationFile, []string{"id"}, []string{"binary content"}, []domain.ProviderProfileID{openAI}, "content served from the private local object directory"),
		makeManifest("openai.files.delete.v1", "DELETE", "/v1/files/{id}", semantic.OperationFile, []string{"id"}, []string{"id", "object", "deleted"}, []domain.ProviderProfileID{openAI}, "deletes upstream, metadata, and local content"),
		makeManifest("openai.batches.create.v1", "POST", "/v1/batches", semantic.OperationBatch, []string{"input_file_id", "endpoint", "completion_window", "metadata"}, batchResponseFields(), []domain.ProviderProfileID{openAI}, "project-owned resource with 7 day TTL"),
		makeManifest("openai.batches.get.v1", "GET", "/v1/batches/{id}", semantic.OperationBatch, []string{"id"}, batchResponseFields(), []domain.ProviderProfileID{openAI}, "project-owned resource"),
		makeManifest("openai.batches.cancel.v1", "POST", "/v1/batches/{id}/cancel", semantic.OperationBatch, []string{"id"}, batchResponseFields(), []domain.ProviderProfileID{openAI}, "project-owned cancellable resource"),
		makeManifest("heimdall.rerank.v1", "POST", "/v1/rerank", semantic.OperationRerank, []string{"model", "query", "documents", "top_n"}, []string{"results"}, []domain.ProviderProfileID{domain.ProfileBedrockAgentRerankCohere35}, "stateless Heimdall extension"),
		makeManifest("heimdall.async.create.v1", "POST", "/v1/async/invocations", semantic.OperationAsyncGenerate, []string{"model", "prompt", "s3_output_uri", "duration_seconds", "dimension", "fps", "seed"}, asyncResponseFields(), []domain.ProviderProfileID{domain.ProfileBedrockAsyncNovaReel}, "project-owned resource with 7 day TTL"),
		makeManifest("heimdall.async.get.v1", "GET", "/v1/async/invocations/{id}", semantic.OperationAsyncGenerate, []string{"id"}, asyncResponseFields(), []domain.ProviderProfileID{domain.ProfileBedrockAsyncNovaReel}, "project-owned resource"),
		makeManifest("heimdall.async.cancel.v1", "POST", "/v1/async/invocations/{id}/cancel", semantic.OperationAsyncGenerate, []string{"id"}, []string{"error"}, []domain.ProviderProfileID{domain.ProfileBedrockAsyncNovaReel}, "always fails closed because Bedrock has no cancellation operation"),
	}
	for index := range manifests {
		if manifests[index].ID == "openai.images.generations.v1" {
			manifests[index].RejectedRequestFields = []string{"user"}
			manifests[index].DocumentedDeviations = append(manifests[index].DocumentedDeviations, "the OpenAI user field is not accepted by this experimental tier")
		}
		if strings.HasPrefix(manifests[index].ID, "heimdall.") {
			manifests[index].Protocol = "heimdall"
			manifests[index].NorthboundProfile = ProfileHeimdallInferenceResources
			manifests[index].DocumentedDeviations = append(manifests[index].DocumentedDeviations, "this is a Heimdall extension and has no OpenAI official SDK surface")
		}
	}
	setProfileCompatibilityStatuses(manifests)
	return manifests
}

func setProfileCompatibilityStatuses(manifests []EndpointCompatibilityManifest) {
	for manifestIndex := range manifests {
		for coverageIndex := range manifests[manifestIndex].ProfileCoverage {
			coverage := &manifests[manifestIndex].ProfileCoverage[coverageIndex]
			coverage.Status = providerProfileCompatibilityStatus(coverage.ProfileID, manifests[manifestIndex].Status)
		}
	}
}

func providerProfileCompatibilityStatus(profileID domain.ProviderProfileID, endpointStatus CompatibilityStatus) CompatibilityStatus {
	if isInferenceResourcesProviderProfile(profileID) {
		return StatusExperimental
	}
	return endpointStatus
}

func isInferenceResourcesProviderProfile(profileID domain.ProviderProfileID) bool {
	switch profileID {
	case domain.ProfileOpenAIMediaResources,
		domain.ProfileBedrockInvokeTitanEmbedV2,
		domain.ProfileBedrockInvokeTitanImageV2,
		domain.ProfileBedrockAgentRerankCohere35,
		domain.ProfileBedrockAsyncNovaReel:
		return true
	default:
		return false
	}
}

func batchResponseFields() []string {
	return []string{"id", "object", "endpoint", "input_file_id", "completion_window", "status", "output_file_id", "error_file_id", "created_at", "expires_at", "completed_at", "failed_at", "cancelling_at", "cancelled_at", "metadata", "errors"}
}

func asyncResponseFields() []string {
	return []string{"invocation_arn", "status", "s3_output_uri", "failure_message", "submitted_at", "last_modified_at"}
}

func CloneEndpointManifest(manifest EndpointCompatibilityManifest) EndpointCompatibilityManifest {
	manifest.RequestFields = slices.Clone(manifest.RequestFields)
	manifest.RejectedRequestFields = slices.Clone(manifest.RejectedRequestFields)
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
