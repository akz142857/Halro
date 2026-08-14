package compatibility

import (
	"errors"
	"slices"
	"strings"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/semantic"
)

type CompatibilityStatus string

const (
	StatusUnsupported       CompatibilityStatus = "unsupported"
	StatusExperimental      CompatibilityStatus = "experimental"
	StatusCompatible        CompatibilityStatus = "compatible"
	StatusNativePassThrough CompatibilityStatus = "native-pass-through"
)

// EvidenceKind names one kind of verification an endpoint has behind it.
//
// It exists because "experimental" was answering two different questions with
// one word. Fifteen endpoints carried the same sentence — "official SDK
// black-box matrix is not yet validated; current coverage is limited to gateway
// contracts and provider transport fixtures" — which told a reader what is
// missing but never what is present, and made an endpoint with contract tests
// and transport fixtures indistinguishable from one with neither. A status is a
// verdict; this is the evidence the verdict was reached on, and a reader who
// disagrees with the verdict can now see why.
//
// These are endpoint-level kinds on purpose. A real-account smoke exercises an
// adapter, not a northbound endpoint, so it is not one of these — claiming it
// here would attribute southbound evidence to a surface it never touched. That
// dimension lives in docs/verification/provider-real-matrix.md, which is
// organised by provider profile.
type EvidenceKind string

const (
	// EvidenceGatewayContract is a test that drives this endpoint through the
	// gateway and asserts its contract.
	EvidenceGatewayContract EvidenceKind = "gateway_contract"
	// EvidenceProviderTransportFixture is a fake upstream that asserts what
	// leaves Halro and what it makes of the reply.
	EvidenceProviderTransportFixture EvidenceKind = "provider_transport_fixture"
	// EvidenceSDKBlackBox is the official-SDK suite calling this endpoint as an
	// application would, with no knowledge of Halro's internals.
	EvidenceSDKBlackBox EvidenceKind = "sdk_blackbox"
)

func (kind EvidenceKind) Valid() bool {
	switch kind {
	case EvidenceGatewayContract, EvidenceProviderTransportFixture, EvidenceSDKBlackBox:
		return true
	}
	return false
}

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
	ID                    string              `json:"id"`
	NorthboundProfile     NorthboundProfileID `json:"northbound_profile"`
	ProfileRevision       uint64              `json:"profile_revision"`
	Protocol              string              `json:"protocol"`
	Method                string              `json:"method"`
	Path                  string              `json:"path"`
	SemanticOperation     semantic.Operation  `json:"semantic_operation"`
	RequestFields         []string            `json:"request_fields"`
	RejectedRequestFields []string            `json:"rejected_request_fields,omitempty"`
	RequestHeaders        []string            `json:"request_headers"`
	ResponseFields        []string            `json:"response_fields"`
	StreamEvents          []string            `json:"stream_events,omitempty"`
	StateSemantics        string              `json:"state_semantics"`
	SDKMatrix             []string            `json:"sdk_matrix"`
	// Evidence is what has actually been verified about this endpoint, as data
	// rather than as a sentence. Status is the verdict; this is its basis.
	Evidence             []EvidenceKind             `json:"evidence"`
	Status               CompatibilityStatus        `json:"status"`
	DocumentedDeviations []string                   `json:"documented_deviations,omitempty"`
	ProviderProfiles     []domain.ProviderProfileID `json:"provider_profiles"`
	ProfileCoverage      []ProfileCoverage          `json:"profile_coverage"`
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
	if len(manifest.Evidence) == 0 {
		return errors.New("endpoint compatibility manifest must declare the evidence behind its status")
	}
	seenEvidence := make(map[EvidenceKind]struct{}, len(manifest.Evidence))
	sdkBlackBox := false
	for _, kind := range manifest.Evidence {
		if !kind.Valid() {
			return errors.New("endpoint compatibility evidence kind is invalid")
		}
		if _, duplicate := seenEvidence[kind]; duplicate {
			return errors.New("endpoint compatibility manifest declares an evidence kind twice")
		}
		seenEvidence[kind] = struct{}{}
		sdkBlackBox = sdkBlackBox || kind == EvidenceSDKBlackBox
	}
	// The two ways of saying the same thing must not drift: an endpoint claiming
	// SDK evidence has to name the SDKs, and one that names them has to claim it.
	if sdkBlackBox != (len(manifest.SDKMatrix) > 0) {
		return errors.New("endpoint compatibility SDK evidence and SDK matrix disagree")
	}
	if manifest.Status == StatusCompatible && !sdkBlackBox {
		return errors.New("compatible endpoint must rest on SDK black-box evidence")
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
		{ProfileID: domain.ProfileAnthropicMessages, UnsupportedRequestFields: []string{"messages[].name", "n", "seed", "response_format", "reasoning_effort", "user"}, DeclaredTransforms: []string{"portable Chat content is mapped to Anthropic Messages blocks", "response_format and reasoning_effort are declared unsupported at field granularity because support is value-dependent: json_schema maps to output_config.format and the low/medium/high/xhigh/max ladder maps to output_config.effort, while json_object and any effort outside that ladder have no Anthropic representation and are routed away before provider I/O"}},
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
		{ID: "openai.chat-completions.v1", NorthboundProfile: ProfileOpenAIChatCompletions, ProfileRevision: 1, Protocol: "openai", Method: "POST", Path: "/v1/chat/completions", SemanticOperation: semantic.OperationGenerate, RequestFields: []string{"model", "messages", "messages[].name", "stream", "stream_options", "temperature", "top_p", "max_tokens", "max_completion_tokens", "n", "stop", "seed", "tools", "tool_choice", "parallel_tool_calls", "response_format", "reasoning_effort", "user"}, RequestHeaders: []string{"Authorization", "Content-Type"}, ResponseFields: []string{"id", "object", "created", "model", "choices", "usage"}, StreamEvents: []string{"chat.completion.chunk", "[DONE]", "error"}, StateSemantics: "stateless", SDKMatrix: []string{"openai-go", "openai-node", "openai-python"}, Evidence: []EvidenceKind{EvidenceGatewayContract, EvidenceProviderTransportFixture, EvidenceSDKBlackBox}, Status: StatusCompatible, DocumentedDeviations: []string{"gateway routes model names; provider-owned chat state is not exposed", "unknown request fields are rejected before provider I/O", "provider-specific unsupported fields are rejected before provider I/O"}, ProviderProfiles: chatProfiles, ProfileCoverage: chatCoverage},
		{ID: "openai.embeddings.v1", NorthboundProfile: ProfileOpenAIEmbeddings, ProfileRevision: 1, Protocol: "openai", Method: "POST", Path: "/v1/embeddings", SemanticOperation: semantic.OperationEmbed, RequestFields: []string{"model", "input", "encoding_format", "dimensions", "user"}, RequestHeaders: []string{"Authorization", "Content-Type"}, ResponseFields: []string{"object", "data", "model", "usage"}, StateSemantics: "stateless", SDKMatrix: []string{"openai-go", "openai-node", "openai-python"}, Evidence: []EvidenceKind{EvidenceGatewayContract, EvidenceProviderTransportFixture, EvidenceSDKBlackBox}, Status: StatusCompatible, DocumentedDeviations: []string{"gateway routes model names", "unknown request fields are rejected before provider I/O", "provider-specific unsupported fields are rejected before provider I/O"}, ProviderProfiles: embedProfiles, ProfileCoverage: embedCoverage},
		{ID: "openai.responses.stateless.v1", NorthboundProfile: ProfileOpenAIResponses, ProfileRevision: 1, Protocol: "openai", Method: "POST", Path: "/v1/responses", SemanticOperation: semantic.OperationGenerate,
			RequestFields:         []string{"model", "input", "input[].type", "input[].role", "input[].content", "input[].call_id", "input[].name", "input[].arguments", "input[].output", "input[].content[].type", "input[].content[].text", "input[].content[].image_url", "input[].content[].detail", "instructions", "stream", "store", "temperature", "top_p", "max_output_tokens", "tools", "tools[].type", "tools[].name", "tools[].description", "tools[].parameters", "tools[].strict", "tool_choice", "parallel_tool_calls", "text.format", "text.format.type", "text.format.name", "text.format.description", "text.format.schema", "text.format.strict", "user"},
			RejectedRequestFields: []string{"store=true", "previous_response_id", "conversation", "background", "prompt", "metadata", "include", "context_management", "service_tier", "truncation", "max_tool_calls", "prompt_cache_key", "prompt_cache_options", "prompt_cache_retention", "safety_identifier", "stream_options", "top_logprobs", "reasoning", "input[].id", "input[].type=unsupported", "tools[].type!=function", "tools[].strict=true", "stream=true with tools"},
			RequestHeaders:        []string{"Authorization", "Content-Type"}, ResponseFields: []string{"id", "object", "created_at", "completed_at", "status", "background", "error", "incomplete_details", "instructions", "max_output_tokens", "model", "output", "output[].id", "output[].type", "output[].status", "output[].role", "output[].content", "output[].call_id", "output[].name", "output[].arguments", "output[].content[].type", "output[].content[].text", "output[].content[].refusal", "output[].content[].annotations", "output[].content[].logprobs", "parallel_tool_calls", "previous_response_id", "reasoning", "store", "temperature", "text", "tool_choice", "tools", "top_p", "truncation", "usage", "usage.input_tokens", "usage.output_tokens", "usage.total_tokens"}, StreamEvents: []string{"response.created", "response.in_progress", "response.output_item.added", "response.content_part.added", "response.output_text.delta", "response.output_text.done", "response.content_part.done", "response.output_item.done", "response.completed", "response.incomplete", "error"}, StateSemantics: "stateless; omitted store is treated as false; stateful fields are rejected before provider I/O", SDKMatrix: []string{"openai-go", "openai-node", "openai-python"}, Evidence: []EvidenceKind{EvidenceGatewayContract, EvidenceProviderTransportFixture, EvidenceSDKBlackBox}, Status: StatusCompatible, DocumentedDeviations: []string{"only POST create is available; retrieval, deletion, cancellation, input_items, Conversations, background mode, and webhooks are unavailable", "store defaults to false and store=true is rejected", "hosted tools, strict function tools, reasoning output, and streaming function calls are rejected", "request instructions, tool definitions, tool choice, and structured schema bodies are returned as conservative null, empty, or default response metadata because the original Responses object has not passed through outbound redaction", "portable requests are translated through the selected profile's existing generation primitive", "unknown fields and unsupported item types are rejected before provider I/O"}, ProviderProfiles: responseProfiles, ProfileCoverage: responseCoverage},
		{ID: "anthropic.messages.2023-06-01", NorthboundProfile: ProfileAnthropicMessages, ProfileRevision: 1, Protocol: "anthropic", Method: "POST", Path: "/v1/messages", SemanticOperation: semantic.OperationGenerate,
			RequestFields: []string{"model", "max_tokens", "messages", "messages[].role", "messages[].content", "messages[].content[].type=document", "messages[].content[].type=search_result", "system", "stream", "stop_sequences", "temperature", "top_p", "top_k", "tools", "tools[].type=custom", "tools[].type=bash_*", "tools[].type=text_editor_*", "tools[].type=memory_*", "tools[].type=computer_*", "tool_choice", "thinking", "metadata", "service_tier", "output_config", "output_config.effort", "output_config.format"}, RejectedRequestFields: []string{"provider-executed tools (web_search_*, web_fetch_*, code_execution_*, advisor_*, tool_search_*) unless the selected connection declares provider_executed_tools", "mcp_servers", "container", "fallbacks", "Anthropic-defined tools in portable mode", "strict tools in portable mode", "signed thinking in portable mode", "unknown top-level fields", "unknown members of tools[] and output_config in portable mode"}, RequestHeaders: []string{"x-api-key or Authorization", "anthropic-version", "anthropic-beta", "Halro-Route-Mode", "Content-Type"}, ResponseFields: []string{"id", "type", "role", "content", "model", "stop_reason", "stop_sequence", "usage"}, StreamEvents: []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop", "ping", "error"}, StateSemantics: "stateless; portable is default; native pins one exact Anthropic-wire provider profile and disables cross-provider fallback", SDKMatrix: []string{"anthropic-go", "anthropic-typescript", "anthropic-python"}, Evidence: []EvidenceKind{EvidenceGatewayContract, EvidenceProviderTransportFixture, EvidenceSDKBlackBox}, Status: StatusCompatible, DocumentedDeviations: []string{"only anthropic-version 2023-06-01 is accepted", "anthropic-beta is forwarded only in native mode and only for tokens the selected connection has been configured to accept; portable mode rejects it because the request is re-authored through the canonical model and a beta token describes the request as written", "tools are classified by execution site: Anthropic-defined client-executed tools are accepted at any dated version suffix, while provider-executed tools require the selected connection to declare provider_executed_tools, because the upstream would make network calls outside SafeTransport's host allowlist", "family matching is anchored to <family>_<YYYYMMDD>, so a longer name that merely begins with a known family (bash_code_execution_*) is classified on its own terms rather than as the client-executed tool it resembles", "portable mode rejects members of tools[], output_config, and message content blocks that the canonical model cannot carry, because that path re-authors the body and would otherwise drop them silently; native mode forwards them verbatim", "mcp_servers, container, and fallbacks are rejected as deliberate boundaries rather than unmodelled fields: the first two delegate egress or code execution to the upstream, and the third moves model selection outside Halro's routing and cost attribution", "in native mode tool and content-block bodies are forwarded verbatim, so cache_control and per-tool configuration reach the provider unchanged; portable mode refuses them rather than re-authoring the body without them", "Gateway Keys are accepted through x-api-key for official SDK compatibility and are never forwarded upstream", "native mode is selected with Halro-Route-Mode and requires either the direct Anthropic or Bedrock Mantle Anthropic profile"}, ProviderProfiles: chatProfiles, ProfileCoverage: []ProfileCoverage{
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
		{ID: "anthropic.messages.count-tokens.2023-06-01", NorthboundProfile: ProfileAnthropicMessages, ProfileRevision: 1, Protocol: "anthropic", Method: "POST", Path: "/v1/messages/count_tokens", SemanticOperation: semantic.OperationGenerate,
			RequestFields:         []string{"model", "messages", "messages[].role", "messages[].content", "system", "tools", "tool_choice", "thinking", "output_config"},
			RejectedRequestFields: []string{"stream", "Halro-Route-Mode: portable", "unknown top-level fields"},
			RequestHeaders:        []string{"x-api-key or Authorization", "anthropic-version", "anthropic-beta", "Content-Type"}, ResponseFields: []string{"input_tokens"}, StateSemantics: "stateless; served natively against one direct Anthropic connection", SDKMatrix: []string{"anthropic-go", "anthropic-typescript", "anthropic-python"}, Evidence: []EvidenceKind{EvidenceGatewayContract, EvidenceProviderTransportFixture, EvidenceSDKBlackBox}, Status: StatusCompatible, DocumentedDeviations: []string{"only the direct Anthropic Messages provider profile serves this endpoint; Bedrock Mantle shares the Messages wire format but its count_tokens surface is not established", "the request body reaches the provider verbatim with only the upstream model identifier substituted, so there is no portable execution mode to select", "Anthropic does not bill count_tokens, and Halro settles it at zero cost — but it is a real provider call on the operator's credential, so it takes a ledger attempt and appears in the audit trail like any other", "the reported count is Anthropic's, for the upstream model behind the alias; it is not re-derived by Halro"}, ProviderProfiles: []domain.ProviderProfileID{domain.ProfileAnthropicMessages}, ProfileCoverage: []ProfileCoverage{
				{ProfileID: domain.ProfileAnthropicMessages, DeclaredTransforms: []string{"only the model identifier is substituted; every other member is forwarded unchanged"}},
			}},
	}
	setProfileCompatibilityStatuses(manifests)
	return append(manifests, inferenceResourcesEndpointManifests()...)
}

func inferenceResourcesEndpointManifests() []EndpointCompatibilityManifest {
	openAI := domain.ProfileOpenAIMediaResources
	imageProfiles := []domain.ProviderProfileID{openAI, domain.ProfileBedrockInvokeTitanImageV2}
	// Batching is a modality, not a provider's feature, so the endpoint lists
	// every profile that can serve it and routing turns away the rest (ADR 0021).
	// The two differ in where the work is described: OpenAI reads the requests
	// from an uploaded file, Anthropic is handed them inline, and the caller
	// sees neither.
	batchProfiles := []domain.ProviderProfileID{openAI, domain.ProfileAnthropicMessages}
	// The Anthropic profile serves files so a batch destined for it has an input
	// to name. Halro keeps those bytes and the upstream never receives them.
	fileProfiles := batchProfiles
	makeManifest := func(id, method, path string, operation semantic.Operation, requestFields, responseFields []string, profiles []domain.ProviderProfileID, state string) EndpointCompatibilityManifest {
		coverage := make([]ProfileCoverage, len(profiles))
		for index, profileID := range profiles {
			coverage[index] = ProfileCoverage{ProfileID: profileID}
		}
		return EndpointCompatibilityManifest{ID: id, NorthboundProfile: ProfileOpenAIMediaResources, ProfileRevision: 1, Protocol: "openai", Method: method, Path: path, SemanticOperation: operation, RequestFields: requestFields, RequestHeaders: []string{"Authorization", "Content-Type", "Idempotency-Key when creating a resource"}, ResponseFields: responseFields, StateSemantics: state, Evidence: []EvidenceKind{EvidenceGatewayContract, EvidenceProviderTransportFixture}, Status: StatusExperimental, DocumentedDeviations: []string{"unknown fields and unsupported profile fields are rejected before provider I/O", "resource identifiers are opaque Halro identifiers scoped to one project"}, ProviderProfiles: profiles, ProfileCoverage: coverage}
	}
	manifests := []EndpointCompatibilityManifest{
		makeManifest("openai.moderations.v1", "POST", "/v1/moderations", semantic.OperationModerate, []string{"model", "input"}, []string{"id", "model", "results"}, []domain.ProviderProfileID{openAI}, "stateless"),
		makeManifest("openai.images.generations.v1", "POST", "/v1/images/generations", semantic.OperationImage, []string{"model", "prompt", "n", "quality", "response_format", "size", "style"}, []string{"created", "data", "data[].url", "data[].b64_json", "data[].revised_prompt"}, imageProfiles, "stateless"),
		makeManifest("openai.audio.transcriptions.v1", "POST", "/v1/audio/transcriptions", semantic.OperationTranscribe, []string{"file", "model", "language", "prompt", "response_format", "temperature"}, []string{"text"}, []domain.ProviderProfileID{openAI}, "stateless"),
		makeManifest("openai.audio.speech.v1", "POST", "/v1/audio/speech", semantic.OperationSynthesize, []string{"model", "input", "voice", "response_format", "speed"}, []string{"binary audio"}, []domain.ProviderProfileID{openAI}, "stateless"),
		makeManifest("openai.files.create.v1", "POST", "/v1/files", semantic.OperationFile, []string{"file", "purpose", "Halro-Route"}, []string{"id", "object", "bytes", "created_at", "filename", "purpose", "status", "status_details"}, fileProfiles, "project-owned resource with 30 day TTL"),
		makeManifest("openai.files.get.v1", "GET", "/v1/files/{id}", semantic.OperationFile, []string{"id"}, []string{"id", "object", "bytes", "created_at", "filename", "purpose", "status", "status_details"}, fileProfiles, "project-owned resource"),
		makeManifest("openai.files.content.v1", "GET", "/v1/files/{id}/content", semantic.OperationFile, []string{"id"}, []string{"binary content"}, fileProfiles, "content served from the private local object directory"),
		makeManifest("openai.files.delete.v1", "DELETE", "/v1/files/{id}", semantic.OperationFile, []string{"id"}, []string{"id", "object", "deleted"}, fileProfiles, "deletes upstream, metadata, and local content"),
		makeManifest("openai.batches.create.v1", "POST", "/v1/batches", semantic.OperationBatch, []string{"input_file_id", "endpoint", "completion_window", "metadata"}, batchResponseFields(), batchProfiles, "project-owned resource with 7 day TTL"),
		makeManifest("openai.batches.get.v1", "GET", "/v1/batches/{id}", semantic.OperationBatch, []string{"id"}, batchResponseFields(), batchProfiles, "project-owned resource"),
		makeManifest("openai.batches.cancel.v1", "POST", "/v1/batches/{id}/cancel", semantic.OperationBatch, []string{"id"}, batchResponseFields(), batchProfiles, "project-owned cancellable resource"),
		makeManifest("halro.rerank.v1", "POST", "/v1/rerank", semantic.OperationRerank, []string{"model", "query", "documents", "top_n"}, []string{"results"}, []domain.ProviderProfileID{domain.ProfileBedrockAgentRerankCohere35}, "stateless Halro extension"),
		makeManifest("halro.async.create.v1", "POST", "/v1/async/invocations", semantic.OperationAsyncGenerate, []string{"model", "prompt", "s3_output_uri", "duration_seconds", "dimension", "fps", "seed"}, asyncResponseFields(), []domain.ProviderProfileID{domain.ProfileBedrockAsyncNovaReel}, "project-owned resource with 7 day TTL"),
		makeManifest("halro.async.get.v1", "GET", "/v1/async/invocations/{id}", semantic.OperationAsyncGenerate, []string{"id"}, asyncResponseFields(), []domain.ProviderProfileID{domain.ProfileBedrockAsyncNovaReel}, "project-owned resource"),
		makeManifest("halro.async.cancel.v1", "POST", "/v1/async/invocations/{id}/cancel", semantic.OperationAsyncGenerate, []string{"id"}, []string{"error"}, []domain.ProviderProfileID{domain.ProfileBedrockAsyncNovaReel}, "always fails closed because Bedrock has no cancellation operation"),
	}
	for index := range manifests {
		if strings.HasPrefix(manifests[index].ID, "openai.batches.") {
			manifests[index].DocumentedDeviations = append(manifests[index].DocumentedDeviations,
				"a batch served by the Anthropic profile has its requests carried inline: the input file is stored by Halro and never uploaded, so its identifier names a Halro object rather than one the upstream also holds",
				"results collected from a provider that does not return them as a file are bounded by the gateway's response ceiling; a batch whose results exceed it is reported as undeliverable rather than truncated",
				"completion_window is honoured only where the provider has one; the Anthropic profile expires a batch 24 hours after creation and accepts no other value")
		}
		if manifests[index].ID == "openai.batches.create.v1" {
			manifests[index].DocumentedDeviations = append(manifests[index].DocumentedDeviations,
				"every line of the input is checked against the selected profile before the batch is created, because a batch is routed once for many requests; a line the profile cannot carry fails the batch and names itself")
		}
		if manifests[index].ID == "openai.images.generations.v1" {
			manifests[index].RejectedRequestFields = []string{"user"}
			manifests[index].DocumentedDeviations = append(manifests[index].DocumentedDeviations, "the OpenAI user field is not accepted by this experimental tier")
		}
		if strings.HasPrefix(manifests[index].ID, "halro.") {
			manifests[index].Protocol = "halro"
			manifests[index].NorthboundProfile = ProfileHalroInferenceResources
			manifests[index].DocumentedDeviations = append(manifests[index].DocumentedDeviations, "this is a Halro extension and has no OpenAI official SDK surface")
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
