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

// ProfileRequestConstraint is what one profile has declared it cannot carry on
// one endpoint, in the endpoint's own spelling of the field.
type ProfileRequestConstraint struct {
	EndpointID               string   `json:"endpoint_id"`
	Path                     string   `json:"path"`
	UnsupportedRequestFields []string `json:"unsupported_request_fields"`
	DeclaredTransforms       []string `json:"declared_transforms,omitempty"`
}

// ProfileRequestConstraints collects, for one provider profile, every endpoint
// where it declares a member it cannot serve.
//
// The data is the same coverage the published manifest carries. What this adds
// is an answer keyed by profile rather than by endpoint, because that is the
// question an operator asks: they are looking at one connection and deciding
// what to turn on, not reading a compatibility table endpoint by endpoint.
//
// It exists because routing already refuses on this and nothing showed it. The
// console can offer a capability tick and the Gateway can then refuse the
// request that tick made possible — the ceiling says vision, the manifest says
// this profile cannot fetch the image — and until the second half is served,
// the operator has no surface on which that rule is visible at all.
//
// Endpoints where the profile declares nothing are omitted rather than returned
// empty: a list of "no constraints" rows is a list an operator has to read to
// learn nothing.
func ProfileRequestConstraints(profileID domain.ProviderProfileID) []ProfileRequestConstraint {
	constraints := make([]ProfileRequestConstraint, 0)
	for _, manifest := range BuiltinEndpointManifests() {
		for _, coverage := range manifest.ProfileCoverage {
			if coverage.ProfileID != profileID || len(coverage.UnsupportedRequestFields) == 0 {
				continue
			}
			constraints = append(constraints, ProfileRequestConstraint{
				EndpointID:               manifest.ID,
				Path:                     manifest.Path,
				UnsupportedRequestFields: slices.Clone(coverage.UnsupportedRequestFields),
				DeclaredTransforms:       slices.Clone(coverage.DeclaredTransforms),
			})
		}
	}
	return constraints
}

// AnthropicPortableOnlyFields are the Messages members no portable request can
// carry, whichever profile serves it.
//
// They are refused one layer above the field rules: DecodePortable in
// compatibility/anthropic rejects the request outright rather than projecting it,
// because these members describe the request as written and the portable path
// re-authors it. That makes them a property of the endpoint's portable path
// rather than of any profile — which is why they were identical in the thirteen
// coverage rows that carry them.
//
// Native mode carries them, so a natively served pairing declares none of this.
// TestPortableOnlyFieldsAreTheOnesTheDecoderRejects holds the list to the decoder
// that enforces it.
var AnthropicPortableOnlyFields = []string{"top_k", "thinking", "metadata", "service_tier"}

// withAnthropicPortableLosses adds the endpoint-level losses to one profile's
// own, so a row states what is true of that profile and nothing else.
func withAnthropicPortableLosses(profileOwn ...string) []string {
	combined := make([]string, 0, len(profileOwn)+len(AnthropicPortableOnlyFields))
	combined = append(combined, profileOwn...)
	combined = append(combined, AnthropicPortableOnlyFields...)
	slices.Sort(combined)
	return combined
}

func BuiltinEndpointManifests() []EndpointCompatibilityManifest {
	chatProfiles := []domain.ProviderProfileID{domain.ProfileOpenAIChatEmbeddings, domain.ProfileOpenAIResponses, domain.ProfileAnthropicMessages, domain.ProfileAzureChatEmbeddings, domain.ProfileDeepSeekChat, domain.ProfileOpenAICompatible, domain.ProfileGeminiText, domain.ProfileBedrockConverseText, domain.ProfileBedrockMantleChat, domain.ProfileBedrockMantleOpenAIChat, domain.ProfileBedrockMantleResponses, domain.ProfileBedrockMantleOpenAIResponses, domain.ProfileBedrockMantleAnthropicMessages, domain.ProfileMiniMaxAnthropicMessages, domain.ProfileMiniMaxChat, domain.ProfileMiniMaxResponses, domain.ProfileKimiChat, domain.ProfileKimiAnthropicMessages, domain.ProfileKimiResponses}
	// MiniMax is absent from embedProfiles on purpose. It serves POST
	// /v1/embeddings, but in its own shape — `texts` and `type` in, a top-level
	// `vectors` array out — so no MiniMax profile declares the embeddings
	// capability and none of them may appear on this endpoint. Kimi is absent for
	// a blunter reason: it publishes no embedding endpoint at all.
	embedProfiles := []domain.ProviderProfileID{domain.ProfileOpenAIChatEmbeddings, domain.ProfileAzureChatEmbeddings, domain.ProfileOpenAICompatible, domain.ProfileGeminiText, domain.ProfileBedrockInvokeTitanEmbedV2}
	chatCoverage := []ProfileCoverage{
		{ProfileID: domain.ProfileOpenAIChatEmbeddings},
		// A Chat Completions request reaching the Responses profile is served on
		// the Responses endpoint. What it cannot ask for there is streaming, which
		// this profile binds no primitive for, and the members the stateless
		// Responses form has no place for.
		{ProfileID: domain.ProfileOpenAIResponses, UnsupportedRequestFields: []string{"messages[].name", "n", "stop", "seed", "reasoning_effort"}, DeclaredTransforms: []string{"Chat messages are mapped to stateless Responses input items", "store=false is always sent upstream", "this profile binds no stream primitive, so a streaming request is routed away rather than refused by field"}},
		{ProfileID: domain.ProfileAnthropicMessages, UnsupportedRequestFields: []string{"messages[].name", "n", "seed", "response_format", "reasoning_effort", "user", "messages[].content[].detail"}, DeclaredTransforms: []string{"portable Chat content is mapped to Anthropic Messages blocks", "an Anthropic image source is base64, url, or file and has no member for the OpenAI fidelity hint, so detail is rejected before provider I/O at any value other than auto rather than dropped from a request that paid for it", "response_format and reasoning_effort are declared unsupported at field granularity because support is value-dependent: json_schema maps to output_config.format and the low/medium/high/xhigh/max ladder maps to output_config.effort, while json_object and any effort outside that ladder have no Anthropic representation and are routed away before provider I/O"}},
		{ProfileID: domain.ProfileAzureChatEmbeddings},
		{ProfileID: domain.ProfileDeepSeekChat, UnsupportedRequestFields: []string{"n", "seed", "max_completion_tokens", "parallel_tool_calls", "response_format", "reasoning_effort"}, DeclaredTransforms: []string{"DeepSeek speaks this wire format but accepts a smaller member list, so the fields it has no place for are rejected before provider I/O rather than sent and ignored", "user is carried as DeepSeek's user_id", "reasoning_effort and response_format are declared unsupported at field granularity because support is value-dependent: none maps to thinking.type=disabled and the low and high rungs map to thinking.reasoning_effort with thinking enabled, while minimal, medium and xhigh have no DeepSeek rung; json_object maps to response_format and json_schema has no DeepSeek counterpart", "max_completion_tokens is value-dependent too: it counts reasoning tokens and DeepSeek's max_tokens does not, so it is carried as max_tokens on a request with thinking off and rejected before provider I/O on one with thinking on, or on one that already carries max_tokens", "n and parallel_tool_calls are value-dependent in the same way: n=1 and parallel_tool_calls=true are what omitting the member already means, and only n>1 and a request to run tools one at a time are rejected"}},
		{ProfileID: domain.ProfileOpenAICompatible},
		{ProfileID: domain.ProfileGeminiText, UnsupportedRequestFields: []string{"messages[].name", "seed", "tools", "tool_choice", "parallel_tool_calls", "response_format", "reasoning_effort", "user"}, DeclaredTransforms: []string{"developer messages are merged into Gemini system_instruction"}},
		{ProfileID: domain.ProfileBedrockConverseText, UnsupportedRequestFields: []string{"messages[].name", "n", "seed", "tools", "tool_choice", "parallel_tool_calls", "response_format", "reasoning_effort", "user"}, DeclaredTransforms: []string{"Bedrock stop reasons are normalized to OpenAI finish reasons"}},
		// The two chat profiles and the two responses profiles differ only in the
		// Mantle route they address, never in what the wire form carries, so each
		// pair states the same coverage.
		{ProfileID: domain.ProfileBedrockMantleChat},
		{ProfileID: domain.ProfileBedrockMantleOpenAIChat},
		{ProfileID: domain.ProfileBedrockMantleResponses, UnsupportedRequestFields: []string{"messages[].name", "n", "stop", "seed", "tools", "reasoning_effort"}, DeclaredTransforms: []string{"Chat messages are mapped to stateless Responses input items", "store=false is always sent upstream", "streaming requests with tools are rejected before provider I/O"}},
		{ProfileID: domain.ProfileBedrockMantleOpenAIResponses, UnsupportedRequestFields: []string{"messages[].name", "n", "stop", "seed", "tools", "reasoning_effort"}, DeclaredTransforms: []string{"Chat messages are mapped to stateless Responses input items", "store=false is always sent upstream", "streaming requests with tools are rejected before provider I/O"}},
		{ProfileID: domain.ProfileBedrockMantleAnthropicMessages, UnsupportedRequestFields: []string{"messages[].name", "n", "seed", "response_format", "reasoning_effort", "user", "messages[].content[].detail"}, DeclaredTransforms: []string{"portable Chat content is mapped to Bedrock Mantle Anthropic Messages blocks", "an Anthropic image source is base64, url, or file and has no member for the OpenAI fidelity hint, so detail is rejected before provider I/O at any value other than auto rather than dropped from a request that paid for it", "Anthropic states that only base64-encoded image sources are available on Amazon Bedrock, so an image_url naming an address to fetch is rejected before provider I/O while a data URL is carried as a base64 source"}},
		{ProfileID: domain.ProfileMiniMaxAnthropicMessages, UnsupportedRequestFields: []string{"messages[].name", "n", "seed", "stop", "response_format", "reasoning_effort", "user", "messages[].content[].detail"}, DeclaredTransforms: []string{"portable Chat content is mapped to MiniMax's Anthropic Messages blocks", "an Anthropic image source is base64, url, or file and has no member for the OpenAI fidelity hint, so detail is rejected before provider I/O at any value other than auto rather than dropped from a request that paid for it", "reasoning_effort is unsupported at any value because MiniMax's reasoning switch is the thinking member and it accepts no output_config; a request that asks for no depth still reaches MiniMax with thinking disabled, because the portable mapper emits that member on its own and MiniMax spells it the same way", "stop is rejected before provider I/O rather than sent: MiniMax documents stop_sequences as ignored, so sending it would return 200 for a completion that ran past a boundary the caller set and paid for", "response_format has no counterpart on this face at all: MiniMax accepts no output_config, and its Chat face was later measured serving json_object while this one was not"}},
		{ProfileID: domain.ProfileMiniMaxChat, UnsupportedRequestFields: []string{"n", "seed", "stop", "response_format", "parallel_tool_calls", "user", "max_tokens"}, DeclaredTransforms: []string{"MiniMax speaks this wire format but accepts a smaller member list, so the fields it has no place for are rejected before provider I/O rather than sent and ignored — this upstream documents its unsupported parameters as ignored, not refused", "reasoning_effort is carried as MiniMax's thinking member: none reaches thinking.type=disabled and every other rung reaches thinking.type=adaptive, because MiniMax's switch has one on state and no depth ladder, so depth is not preserved", "a request that asks for no reasoning is sent with thinking explicitly disabled rather than left to MiniMax's own default, which is on", "reasoning_split is sent alongside an enabled switch so reasoning returns in its own member instead of inline in the answer", "max_completion_tokens is carried as itself and max_tokens is carried into it only while thinking is off, because MiniMax's single output bound counts reasoning; max_tokens is value-dependent for that reason — a request that also thinks, or that carries both members, is routed away by field before provider I/O rather than refused after the budget is reserved", "n and parallel_tool_calls are value-dependent: n=1 and parallel_tool_calls=true are what omitting the member already means", "response_format is value-dependent: json_object was measured against a real account on 2026-08-31 and is carried, text is dropped because it means what omitting the member already means, and the schema mode has never been sent and is rejected before provider I/O"}},
		{ProfileID: domain.ProfileMiniMaxResponses, UnsupportedRequestFields: []string{"messages[].name", "n", "seed", "stop", "response_format", "parallel_tool_calls", "reasoning_effort", "user"}, DeclaredTransforms: []string{"Chat messages are mapped to stateless Responses input items", "store=false is always sent upstream", "this profile binds no stream primitive, so a streaming request is routed away rather than refused by field — MiniMax itself documents stream on /v1/responses, so this is a Halro scope decision and not an upstream limit", "reasoning is unsupported here and supported on the MiniMax Chat profile: the canonical response mapper cannot preserve the reasoning items this endpoint returns"}},
		{ProfileID: domain.ProfileKimiChat, UnsupportedRequestFields: []string{"temperature", "top_p", "n", "seed", "parallel_tool_calls", "user", "max_tokens", "reasoning_effort", "stop", "tool_choice"}, DeclaredTransforms: []string{"Kimi speaks this wire format but does not model the sampling parameters at all: temperature, top_p and n are absent from its request schema, and its parameter reference pins each to one value per model and answers any other with an error, so they are rejected before provider I/O rather than sent or silently replaced with the pinned value", "temperature and top_p are rejected at any value rather than only at the wrong one, because the pinned value varies by model and a field rule is keyed by profile with no target in hand; carrying the pinned value is a capability-model change rather than a platform registration", "max_tokens is value-dependent: Kimi has one output bound and it counts reasoning, so an answer-only bound is the same tokens exactly while nothing is thinking — which is the ordinary case, because a request that names no depth is sent with reasoning switched off. A request that does ask for depth, or that carries both output members, is routed away before provider I/O", "reasoning_effort is value-dependent: none, low and high are carried, and minimal, medium, xhigh and Kimi's own max are routed away because the portable ladder and Kimi's do not share those rungs", "the reasoning member itself depends on the model named: kimi-k3 reads a top-level reasoning_effort while the K2.x line reads thinking, and the renderer follows Kimi's own OpenAPI discriminator over exact model identifiers rather than inferring a family from a name", "K2.x has one on state and no depth ladder, so an effort routed to those models is honoured as on rather than preserved", "a request that names no depth is sent with reasoning explicitly switched off rather than left to Kimi's default, which is on: measured 2026-09-01, kimi-k3 and kimi-k2.6 both accept the off switch, and leaving it out returned a 64-token budget spent entirely on reasoning with an empty answer. kimi-k2.7-code has no off state and keeps reasoning, which is a per-model fact this profile-scoped declaration cannot express", "stop is value-dependent: Kimi documents it as honoured and it is carried, but only within its published bounds of five sequences of at most 32 bytes each, and a request outside them is routed away before provider I/O rather than refused while the body is encoded", "tool_choice is value-dependent, and the two ways of forcing a tool call are not the same value: a named function together with a depth is refused by every Kimi model measured and is routed away before provider I/O, while required together with a depth is refused by the K2.x line and accepted by kimi-k3, which answers with a tool call and a reasoning span in one response. That second half is a per-model limit this profile-scoped declaration cannot express, so it is refused after the reservation rather than routed away with the kimi-k3 request that works"}},
		{ProfileID: domain.ProfileKimiAnthropicMessages, UnsupportedRequestFields: []string{"temperature", "top_p", "messages[].name", "n", "seed", "response_format", "reasoning_effort", "user", "messages[].content[].detail"}, DeclaredTransforms: []string{"portable Chat content is mapped to Kimi's Anthropic Messages blocks", "an Anthropic image source is base64, url, or file and has no member for the OpenAI fidelity hint, so detail is rejected before provider I/O at any value other than auto rather than dropped from a request that paid for it", "reasoning_effort is unsupported at any value on this face, and that is what makes the face usable at all: a request naming no depth reaches Kimi with thinking disabled and comes back with no thinking block, which the portable decoder can read, while a request naming a depth comes back with one, which it refuses outright after the upstream has already been paid", "temperature and top_p are rejected because Kimi pins each per model and answers any other value with an error", "response_format is value-dependent: output_config.format takes a json_schema and enforces it, and there is no schema-less mode", "stop is carried: measured honoured on this face, unlike MiniMax's, which documents the member as ignored"}},
		{ProfileID: domain.ProfileKimiResponses, UnsupportedRequestFields: []string{"temperature", "top_p", "messages[].name", "n", "seed", "stop", "max_tokens", "parallel_tool_calls", "reasoning_effort", "user"}, DeclaredTransforms: []string{"Chat messages are mapped to stateless Responses input items", "Kimi does not model the sampling parameters on this face either, so temperature and top_p are rejected before provider I/O", "this profile binds no stream primitive, so a streaming request is routed away rather than refused by field — Kimi itself documents stream on /v1/responses, so this is a Halro scope decision and not an upstream limit", "the schema-less json_object mode is unavailable here and available on the Kimi Chat profile: this endpoint models structured output as text.format and accepts the json_schema type alone, so a json_object request is routed away by the absent json_object capability rather than by field", "reasoning is unsupported here and supported on the Kimi Chat profile: the canonical response mapper cannot preserve the reasoning items this endpoint returns", "this endpoint serves kimi-k3 alone, which the model catalogue records per model rather than this profile-scoped declaration"}},
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
		// The one profile that reaches the Responses endpoint as a Responses
		// request, which is why it is the only one that can carry a tool the
		// upstream runs itself and return the citations that come back with it.
		{ProfileID: domain.ProfileOpenAIResponses, DeclaredTransforms: []string{"stateless Responses are sent directly with store=false", "tools[].type=web_search requires the selected connection to declare provider_executed_tools", "url_citation annotations are carried back on the output text"}},
		{ProfileID: domain.ProfileAnthropicMessages, UnsupportedRequestFields: []string{"text.format", "user", "input[].content[].detail"}, DeclaredTransforms: []string{"Responses items are mapped through the Anthropic Messages ProviderPrimitive"}},
		{ProfileID: domain.ProfileAzureChatEmbeddings, DeclaredTransforms: []string{"Responses items are mapped through the Azure Chat Completions ProviderPrimitive"}},
		{ProfileID: domain.ProfileDeepSeekChat, UnsupportedRequestFields: []string{"parallel_tool_calls", "text.format"}, DeclaredTransforms: []string{"Responses items are mapped through the DeepSeek Chat ProviderPrimitive", "max_output_tokens is carried as DeepSeek's max_tokens: it is a completion budget that counts reasoning, and this endpoint rejects the reasoning request field outright, so nothing served here thinks and the two bound the same tokens", "parallel_tool_calls is value-dependent: true is what omitting the member already means, and only a request to run tools one at a time is rejected", "text.format is value-dependent: DeepSeek has json_object and no schema mode, so a schema is rejected before provider I/O", "user is carried as DeepSeek's user_id"}},
		{ProfileID: domain.ProfileOpenAICompatible, DeclaredTransforms: []string{"Responses items are mapped through the compatible Chat Completions ProviderPrimitive"}},
		{ProfileID: domain.ProfileGeminiText, UnsupportedRequestFields: []string{"tools", "tool_choice", "parallel_tool_calls", "text.format", "user"}, DeclaredTransforms: []string{"instructions are mapped to a developer message and merged into Gemini system_instruction"}},
		{ProfileID: domain.ProfileBedrockConverseText, UnsupportedRequestFields: []string{"tools", "tool_choice", "parallel_tool_calls", "text.format", "user"}, DeclaredTransforms: []string{"instructions are mapped to a developer message", "Bedrock stop reasons are normalized to Responses status"}},
		{ProfileID: domain.ProfileBedrockMantleChat, DeclaredTransforms: []string{"Responses items are mapped through Bedrock Mantle Chat Completions"}},
		{ProfileID: domain.ProfileBedrockMantleOpenAIChat, DeclaredTransforms: []string{"Responses items are mapped through Bedrock Mantle Chat Completions"}},
		{ProfileID: domain.ProfileBedrockMantleResponses, UnsupportedRequestFields: []string{"tools"}, DeclaredTransforms: []string{"stateless Responses are sent directly with store=false"}},
		{ProfileID: domain.ProfileBedrockMantleOpenAIResponses, UnsupportedRequestFields: []string{"tools"}, DeclaredTransforms: []string{"stateless Responses are sent directly with store=false"}},
		{ProfileID: domain.ProfileBedrockMantleAnthropicMessages, UnsupportedRequestFields: []string{"text.format", "user", "input[].content[].detail"}, DeclaredTransforms: []string{"an Anthropic image source is base64, url, or file and has no member for the OpenAI fidelity hint, so detail is rejected before provider I/O at any value other than auto rather than dropped from a request that paid for it", "Responses items are mapped through Bedrock Mantle Anthropic Messages"}},
		{ProfileID: domain.ProfileMiniMaxAnthropicMessages, UnsupportedRequestFields: []string{"text.format", "user", "input[].content[].detail"}, DeclaredTransforms: []string{"Responses items are mapped through MiniMax's Anthropic Messages ProviderPrimitive", "an Anthropic image source is base64, url, or file and has no member for the OpenAI fidelity hint, so detail is rejected before provider I/O at any value other than auto rather than dropped from a request that paid for it"}},
		{ProfileID: domain.ProfileMiniMaxChat, UnsupportedRequestFields: []string{"parallel_tool_calls", "text.format", "user"}, DeclaredTransforms: []string{"Responses items are mapped through the MiniMax Chat ProviderPrimitive", "max_output_tokens is carried as MiniMax's max_completion_tokens, which is the same quantity: this endpoint rejects the reasoning request field outright, so nothing served here thinks", "parallel_tool_calls is value-dependent: true is what omitting the member already means"}},
		{ProfileID: domain.ProfileMiniMaxResponses, UnsupportedRequestFields: []string{"parallel_tool_calls", "text.format", "user"}, DeclaredTransforms: []string{"stateless Responses are sent directly with store=false", "reasoning is rejected before provider I/O because the canonical response mapper cannot preserve the reasoning items MiniMax returns"}},
		{ProfileID: domain.ProfileKimiChat, UnsupportedRequestFields: []string{"temperature", "top_p", "parallel_tool_calls", "user", "tool_choice"}, DeclaredTransforms: []string{"Responses items are mapped through the Kimi Chat ProviderPrimitive", "tool_choice is value-dependent: a named function together with a reasoning depth is refused by every Kimi model and is routed away before provider I/O", "Kimi does not model temperature or top_p as request members and pins each to one value per model, so they are rejected before provider I/O rather than sent or silently replaced", "max_output_tokens is carried as Kimi's max_completion_tokens, which Kimi documents as the same bound", "parallel_tool_calls is value-dependent: true is what omitting the member already means", "text.format is carried: the Kimi Chat face documents the json_schema response format alongside json_object"}},
		{ProfileID: domain.ProfileKimiAnthropicMessages, UnsupportedRequestFields: []string{"temperature", "top_p", "text.format", "user", "input[].content[].detail"}, DeclaredTransforms: []string{"Responses items are mapped through Kimi's Anthropic Messages ProviderPrimitive", "an Anthropic image source is base64, url, or file and has no member for the OpenAI fidelity hint, so detail is rejected before provider I/O at any value other than auto rather than dropped from a request that paid for it", "temperature and top_p are rejected because Kimi pins each per model", "text.format is value-dependent: this face takes a schema through output_config.format and enforces it, so a schema-less request is routed away"}},
		{ProfileID: domain.ProfileKimiResponses, UnsupportedRequestFields: []string{"temperature", "top_p", "parallel_tool_calls", "user"}, DeclaredTransforms: []string{"stateless Responses are sent directly", "Kimi does not model temperature or top_p as request members on this face either", "reasoning is rejected before provider I/O because the canonical response mapper cannot preserve the reasoning items Kimi returns", "text.format carries a schema; the schema-less json_object mode is routed away by the absent json_object capability rather than by field, because Kimi's Responses face accepts the json_schema type alone", "this endpoint serves kimi-k3 alone, which the model catalogue records per model"}},
	}
	manifests := []EndpointCompatibilityManifest{
		{ID: "openai.chat-completions.v1", NorthboundProfile: ProfileOpenAIChatCompletions, ProfileRevision: 1, Protocol: "openai", Method: "POST", Path: "/v1/chat/completions", SemanticOperation: semantic.OperationGenerate, RequestFields: []string{"model", "messages", "messages[].name", "messages[].content[].image_url", "messages[].content[].detail", "stream", "stream_options", "temperature", "top_p", "max_tokens", "max_completion_tokens", "n", "stop", "seed", "tools", "tool_choice", "parallel_tool_calls", "response_format", "reasoning_effort", "user"}, RequestHeaders: []string{"Authorization", "Content-Type"}, ResponseFields: []string{"id", "object", "created", "model", "choices", "usage"}, StreamEvents: []string{"chat.completion.chunk", "[DONE]", "error"}, StateSemantics: "stateless", SDKMatrix: []string{"openai-go", "openai-node", "openai-python"}, Evidence: []EvidenceKind{EvidenceGatewayContract, EvidenceProviderTransportFixture, EvidenceSDKBlackBox}, Status: StatusCompatible, DocumentedDeviations: []string{"gateway routes model names; provider-owned chat state is not exposed", "unknown request fields are rejected before provider I/O", "provider-specific unsupported fields are rejected before provider I/O"}, ProviderProfiles: chatProfiles, ProfileCoverage: chatCoverage},
		{ID: "openai.embeddings.v1", NorthboundProfile: ProfileOpenAIEmbeddings, ProfileRevision: 1, Protocol: "openai", Method: "POST", Path: "/v1/embeddings", SemanticOperation: semantic.OperationEmbed, RequestFields: []string{"model", "input", "encoding_format", "dimensions", "user"}, RequestHeaders: []string{"Authorization", "Content-Type"}, ResponseFields: []string{"object", "data", "model", "usage"}, StateSemantics: "stateless", SDKMatrix: []string{"openai-go", "openai-node", "openai-python"}, Evidence: []EvidenceKind{EvidenceGatewayContract, EvidenceProviderTransportFixture, EvidenceSDKBlackBox}, Status: StatusCompatible, DocumentedDeviations: []string{"gateway routes model names", "unknown request fields are rejected before provider I/O", "provider-specific unsupported fields are rejected before provider I/O"}, ProviderProfiles: embedProfiles, ProfileCoverage: embedCoverage},
		{ID: "openai.responses.create.v1", NorthboundProfile: ProfileOpenAIResponses, ProfileRevision: 2, Protocol: "openai", Method: "POST", Path: "/v1/responses", SemanticOperation: semantic.OperationGenerate,
			RequestFields:         []string{"model", "input", "input[].type", "input[].role", "input[].content", "input[].call_id", "input[].name", "input[].arguments", "input[].output", "input[].content[].type", "input[].content[].text", "input[].content[].detail", "instructions", "reasoning", "stream", "store", "temperature", "top_p", "max_output_tokens", "tools", "tools[].type", "tools[].name", "tools[].description", "tools[].parameters", "tools[].strict", "tool_choice", "parallel_tool_calls", "text.format", "text.format.type", "text.format.name", "text.format.description", "text.format.schema", "text.format.strict", "user", "background"},
			RejectedRequestFields: []string{"store=true", "previous_response_id", "conversation", "background=true with stream=true", "prompt", "metadata", "include", "context_management", "service_tier", "truncation", "max_tool_calls", "prompt_cache_key", "prompt_cache_options", "prompt_cache_retention", "safety_identifier", "stream_options", "top_logprobs", "reasoning", "input[].id", "input[].type=unsupported", "tools[].type!=function", "tools[].strict=true", "stream=true with tools"},
			RequestHeaders:        []string{"Authorization", "Content-Type", "Idempotency-Key when submitting with background=true"}, ResponseFields: []string{"id", "object", "created_at", "completed_at", "status", "background", "error", "incomplete_details", "instructions", "max_output_tokens", "model", "output", "output[].id", "output[].type", "output[].status", "output[].role", "output[].content", "output[].call_id", "output[].name", "output[].arguments", "output[].content[].type", "output[].content[].text", "output[].content[].refusal", "output[].content[].annotations", "output[].content[].logprobs", "parallel_tool_calls", "previous_response_id", "reasoning", "store", "temperature", "text", "tool_choice", "tools", "top_p", "truncation", "usage", "usage.input_tokens", "usage.output_tokens", "usage.total_tokens"}, StreamEvents: []string{"response.created", "response.in_progress", "response.output_item.added", "response.content_part.added", "response.output_text.delta", "response.output_text.done", "response.content_part.done", "response.output_item.done", "response.completed", "response.incomplete", "error"}, StateSemantics: "stateless by default; background=true defers one generation to a project-owned record with a 24 hour TTL and a 15 minute cool-off after first retrieval; the remaining stateful fields are rejected before provider I/O", SDKMatrix: []string{"openai-go", "openai-node", "openai-python"}, Evidence: []EvidenceKind{EvidenceGatewayContract, EvidenceProviderTransportFixture, EvidenceSDKBlackBox}, Status: StatusCompatible, DocumentedDeviations: []string{"input_items, Conversations, and webhooks are unavailable; retrieval, cancellation and deletion serve deferred submissions alone", "store defaults to false and store=true is rejected even alongside background=true, because store asserts that the provider retains the Response and Halro's upstream retains nothing (ADR 0021)", "background=true is enabled per Project and refused with unsupported_feature where the Project has not enabled it, because a deferred answer is written to disk and that is a decision about what the data directory holds", "a background submission carries at most 256 KiB of request body, which is below the gateway's own request ceiling: the same body sent synchronously is accepted and sent with background=true is refused, because the deferred body is stored rather than forwarded", "a background submission does not survive a gateway restart: a deferred response has no upstream handle, so a request in flight when the process dies is failed and settled conservatively", "a deferred answer larger than 1 MiB is failed rather than stored, and the request was still billed: the ceiling is on what may be kept, and nothing can measure an answer before the upstream produces it", "a background submission whose Project is at its TPM or concurrency ceiling waits in the queue rather than being refused; only the queue depth and the 24 hour TTL bound that wait", "one deferred attempt is bounded by the same route_total_timeout a synchronous attempt runs under; exceeding it fails the request, which was still billed", "strict function tools, reasoning output, and streaming tools are rejected", "tools[].type=web_search is the one hosted tool accepted; it is routed against the provider_executed_tools capability and served only by the OpenAI Responses profile, and code_interpreter and file_search stay rejected because both are provider-side state", "request instructions, tool definitions, tool choice, and structured schema bodies are returned as conservative null, empty, or default response metadata because the original Responses object has not passed through outbound redaction", "portable requests are translated through the selected profile's existing generation primitive", "unknown fields and unsupported item types are rejected before provider I/O"}, ProviderProfiles: responseProfiles, ProfileCoverage: responseCoverage},
		{ID: "anthropic.messages.2023-06-01", NorthboundProfile: ProfileAnthropicMessages, ProfileRevision: 1, Protocol: "anthropic", Method: "POST", Path: "/v1/messages", SemanticOperation: semantic.OperationGenerate,
			RequestFields: []string{"model", "max_tokens", "messages", "messages[].role", "messages[].content", "messages[].content[].type=document", "messages[].content[].type=search_result", "system", "stream", "stop_sequences", "temperature", "top_p", "top_k", "tools", "tools[].type=custom", "tools[].type=bash_*", "tools[].type=text_editor_*", "tools[].type=memory_*", "tools[].type=computer_*", "tool_choice", "thinking", "metadata", "service_tier", "output_config", "output_config.effort", "output_config.format"}, RejectedRequestFields: []string{"provider-executed tools (web_search_*, web_fetch_*, code_execution_*, advisor_*, tool_search_*) unless the selected connection declares provider_executed_tools", "mcp_servers", "container", "fallbacks", "Anthropic-defined tools in portable mode", "strict tools in portable mode", "signed thinking in portable mode", "unknown top-level fields", "unknown members of tools[] and output_config in portable mode"}, RequestHeaders: []string{"x-api-key or Authorization", "anthropic-version", "anthropic-beta", "Halro-Route-Mode", "Content-Type"}, ResponseFields: []string{"id", "type", "role", "content", "model", "stop_reason", "stop_sequence", "usage"}, StreamEvents: []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop", "ping", "error"}, StateSemantics: "stateless; portable is default; native pins one exact Anthropic-wire provider profile and disables cross-provider fallback", SDKMatrix: []string{"anthropic-go", "anthropic-typescript", "anthropic-python"}, Evidence: []EvidenceKind{EvidenceGatewayContract, EvidenceProviderTransportFixture, EvidenceSDKBlackBox}, Status: StatusCompatible, DocumentedDeviations: []string{"only anthropic-version 2023-06-01 is accepted", "anthropic-beta is forwarded only in native mode and only for tokens the selected connection has been configured to accept; portable mode rejects it because the request is re-authored through the canonical model and a beta token describes the request as written", "tools are classified by execution site: Anthropic-defined client-executed tools are accepted at any dated version suffix, while provider-executed tools require the selected connection to declare provider_executed_tools, because the upstream would make network calls outside SafeTransport's host allowlist", "family matching is anchored to <family>_<YYYYMMDD>, so a longer name that merely begins with a known family (bash_code_execution_*) is classified on its own terms rather than as the client-executed tool it resembles", "portable mode rejects members of tools[], output_config, and message content blocks that the canonical model cannot carry, because that path re-authors the body and would otherwise drop them silently; native mode forwards them verbatim", "mcp_servers, container, and fallbacks are rejected as deliberate boundaries rather than unmodelled fields: the first two delegate egress or code execution to the upstream, and the third moves model selection outside Halro's routing and cost attribution", "in native mode tool and content-block bodies are forwarded verbatim, so cache_control and per-tool configuration reach the provider unchanged; portable mode refuses them rather than re-authoring the body without them", "Gateway Keys are accepted through x-api-key for official SDK compatibility and are never forwarded upstream", "native mode is selected with Halro-Route-Mode and requires either the direct Anthropic or Bedrock Mantle Anthropic profile"}, ProviderProfiles: chatProfiles, ProfileCoverage: []ProfileCoverage{
				{ProfileID: domain.ProfileOpenAIChatEmbeddings, UnsupportedRequestFields: withAnthropicPortableLosses(), DeclaredTransforms: []string{"portable Messages content is mapped through OpenAI Chat Completions"}},
				{ProfileID: domain.ProfileOpenAIResponses, UnsupportedRequestFields: withAnthropicPortableLosses("stop_sequences", "output_config.effort"), DeclaredTransforms: []string{"portable Messages content is mapped through the OpenAI Responses endpoint", "this profile binds no stream primitive, so a streaming Messages request is routed away rather than refused by field"}},
				{ProfileID: domain.ProfileAnthropicMessages, DeclaredTransforms: []string{"native mode preserves validated Anthropic content blocks and events"}},
				{ProfileID: domain.ProfileAzureChatEmbeddings, UnsupportedRequestFields: withAnthropicPortableLosses(), DeclaredTransforms: []string{"portable Messages content is mapped through Azure Chat Completions"}},
				{ProfileID: domain.ProfileDeepSeekChat, UnsupportedRequestFields: withAnthropicPortableLosses("output_config.effort", "output_config.format"), DeclaredTransforms: []string{"portable Messages content is mapped through DeepSeek Chat Completions", "output_config.effort and output_config.format are declared unsupported at field granularity because support is value-dependent: none, low and high reach DeepSeek's thinking switch while minimal, medium and xhigh have no rung, and DeepSeek has json_object but no schema mode", "thinking stays unsupported for the same reason it is on every other portable profile — it is the Anthropic-native block config, which only native mode forwards; DeepSeek's own thinking switch is reached through output_config.effort"}},
				{ProfileID: domain.ProfileOpenAICompatible, UnsupportedRequestFields: withAnthropicPortableLosses(), DeclaredTransforms: []string{"portable Messages content is mapped through an OpenAI-compatible primitive"}},
				{ProfileID: domain.ProfileGeminiText, UnsupportedRequestFields: withAnthropicPortableLosses("tools", "tool_choice", "output_config.effort", "output_config.format"), DeclaredTransforms: []string{"portable text Messages content is mapped through Gemini generateContent", "output_config.effort and output_config.format are this endpoint's spelling of reasoning_effort and response_format, which this profile declares unsupported at every value, so a request carrying either is routed away before provider I/O"}},
				{ProfileID: domain.ProfileBedrockConverseText, UnsupportedRequestFields: withAnthropicPortableLosses("tools", "tool_choice", "output_config.effort", "output_config.format"), DeclaredTransforms: []string{"portable text Messages content is mapped through Bedrock Converse", "output_config.effort and output_config.format are this endpoint's spelling of reasoning_effort and response_format, which this profile declares unsupported at every value, so a request carrying either is routed away before provider I/O"}},
				{ProfileID: domain.ProfileBedrockMantleChat, UnsupportedRequestFields: withAnthropicPortableLosses(), DeclaredTransforms: []string{"portable Messages content is mapped through Bedrock Mantle Chat Completions"}},
				{ProfileID: domain.ProfileBedrockMantleOpenAIChat, UnsupportedRequestFields: withAnthropicPortableLosses(), DeclaredTransforms: []string{"portable Messages content is mapped through Bedrock Mantle Chat Completions"}},
				{ProfileID: domain.ProfileBedrockMantleResponses, UnsupportedRequestFields: withAnthropicPortableLosses("stop_sequences", "tools", "output_config.effort"), DeclaredTransforms: []string{"portable Messages content is mapped through stateless Bedrock Mantle Responses", "streaming requests with tools are rejected before provider I/O", "output_config.effort is this endpoint's spelling of reasoning_effort, which the stateless Responses primitive does not carry, so a request carrying it is routed away before provider I/O"}},
				{ProfileID: domain.ProfileBedrockMantleOpenAIResponses, UnsupportedRequestFields: withAnthropicPortableLosses("stop_sequences", "tools", "output_config.effort"), DeclaredTransforms: []string{"portable Messages content is mapped through stateless Bedrock Mantle Responses", "streaming requests with tools are rejected before provider I/O", "output_config.effort is this endpoint's spelling of reasoning_effort, which the stateless Responses primitive does not carry, so a request carrying it is routed away before provider I/O"}},
				{ProfileID: domain.ProfileBedrockMantleAnthropicMessages, UnsupportedRequestFields: []string{"output_config.effort", "output_config.format"}, DeclaredTransforms: []string{"native mode preserves validated Anthropic content blocks, thinking signatures, and events", "output_config is unsupported in portable mode only: this profile shares the Anthropic wire form and could carry the member, but its Mantle Beta capability ceiling is fixed by the build and widening it is a separate contract review, so a portable request carrying effort or format is routed away before provider I/O while native mode forwards the member verbatim"}},
				// MiniMax's Anthropic face is the one profile here that refuses
				// members native mode would otherwise forward. Everywhere else the
				// portable path is the narrower one; here top_k, stop_sequences and
				// cache_control are legal Anthropic members that MiniMax accepts and
				// then ignores, so forwarding them verbatim returns 200 for a
				// request that did not happen as written. The gateway refuses them
				// on both paths instead.
				{ProfileID: domain.ProfileMiniMaxAnthropicMessages, UnsupportedRequestFields: []string{"top_k", "stop_sequences", "output_config.effort", "output_config.format"}, DeclaredTransforms: []string{"native mode preserves validated Anthropic content blocks, thinking signatures, and events", "top_k, stop_sequences and cache_control are rejected on the native path as well as the portable one, because MiniMax documents them as ignored rather than refused and a silently dropped boundary is billed as if it had been honoured", "prompt caching is unavailable, so a cache_control marker would claim a discount that does not exist", "anthropic-beta is never forwarded: MiniMax does not accept beta headers", "output_config is unsupported because MiniMax's reasoning switch is the older thinking member; a portable request that asks for no depth still arrives with thinking disabled, which is MiniMax's exact spelling", "video and mid_conv_system content blocks are refused by the Messages decoder, which accepts only Anthropic's own block types — MiniMax extends the wire form and Halro does not follow it there"}},
				{ProfileID: domain.ProfileMiniMaxChat, UnsupportedRequestFields: withAnthropicPortableLosses("stop_sequences", "output_config.format", "max_tokens"), DeclaredTransforms: []string{"portable Messages content is mapped through MiniMax Chat Completions", "output_config.effort reaches MiniMax's thinking switch, which has one on state and no depth ladder, so a depth is honoured as on rather than preserved — but only on a request that does not also bound the answer, and this endpoint requires max_tokens, so an effort-bearing Messages request is routed away from this profile by field rather than translated", "max_tokens is why: MiniMax has one output bound and it counts reasoning, so the answer-only bound this endpoint requires is the same quantity only while nothing is thinking"}},
				{ProfileID: domain.ProfileMiniMaxResponses, UnsupportedRequestFields: withAnthropicPortableLosses("stop_sequences", "output_config.effort", "output_config.format"), DeclaredTransforms: []string{"portable Messages content is mapped through stateless MiniMax Responses", "this profile binds no stream primitive, so a streaming Messages request is routed away rather than refused by field", "output_config.effort is this endpoint's spelling of reasoning_effort, which the stateless Responses primitive does not carry"}},
				// Kimi's Anthropic face refuses two members native mode would
				// otherwise forward, and for one reason each. top_k is accepted and
				// answers 200, but Kimi's own schema has no such member, so what it
				// does with it is unestablished and forwarding it would let a
				// caller believe a sampling constraint was applied. cache_control
				// claims a discount on a cache Kimi manages automatically and
				// exposes no handle for.
				{ProfileID: domain.ProfileKimiAnthropicMessages, UnsupportedRequestFields: []string{"metadata", "output_config.effort", "service_tier", "temperature", "thinking", "top_k", "top_p"}, DeclaredTransforms: []string{"native mode preserves validated Anthropic content blocks, thinking signatures, and events", "output_config.effort is unsupported in portable mode only: a portable request naming a depth comes back carrying a thinking block, which the portable decoder refuses after the upstream has been paid, while native mode forwards the member and reads the block back verbatim", "output_config.format is carried: this endpoint always names a schema, and Kimi takes a json_schema through the same member and enforces it", "temperature and top_p are rejected on both paths because Kimi pins each per model and answers any other value with an error", "top_k and cache_control are rejected on the native path as well as the portable one: Kimi's Messages schema has neither member, top_k answers 200 without anything establishing what it did, and prompt caching is automatic with no handle to mark", "anthropic-beta is never forwarded: Kimi documents no beta headers", "stop_sequences is carried and was measured honoured, so it is not refused the way MiniMax's is"}},
				{ProfileID: domain.ProfileKimiChat, UnsupportedRequestFields: withAnthropicPortableLosses("temperature", "top_p", "output_config.effort", "max_tokens", "stop_sequences", "tool_choice"), DeclaredTransforms: []string{"portable Messages content is mapped through Kimi Chat Completions", "temperature and top_p are this endpoint's own members and Kimi models neither: they are absent from its request schema and pinned per model, so a Messages request naming either is routed away before provider I/O", "output_config.effort is this endpoint's spelling of reasoning_effort and is value-dependent: low and high reach Kimi's switch, while medium, xhigh, Anthropic's max and a request for no reasoning at all have no Kimi rung on this profile", "output_config.format is carried: it is always a schema on this endpoint, and the Kimi Chat face documents the json_schema response format", "max_tokens is carried as max_completion_tokens, which Kimi documents as the same bound under a newer name — but the two are the same quantity only while nothing is thinking, because Kimi's single output bound counts reasoning, measured 2026-09-01. This endpoint requires max_tokens and means the answer alone, so a request that also asks for a depth is routed away rather than having this layer decide which of the two quantities the caller meant", "stop_sequences is carried and was measured honoured, within Kimi's published bounds of five sequences of at most 32 bytes each; a request outside them is routed away before provider I/O", "tool_choice is value-dependent: a named function together with a depth is refused by every Kimi model and routed away before provider I/O, while required together with a depth is refused by the K2.x line alone — a per-model limit this declaration cannot express, refused after the reservation rather than costing kimi-k3 the request it serves"}},
				{ProfileID: domain.ProfileKimiResponses, UnsupportedRequestFields: withAnthropicPortableLosses("temperature", "top_p", "stop_sequences", "max_tokens", "output_config.effort"), DeclaredTransforms: []string{"portable Messages content is mapped through stateless Kimi Responses", "this profile binds no stream primitive, so a streaming Messages request is routed away rather than refused by field", "output_config.effort is this endpoint's spelling of reasoning_effort, which the stateless Responses primitive does not carry", "temperature and top_p are rejected for the same reason as on the Kimi Chat profile: Kimi models neither member", "output_config.format is carried: it is always a schema on this endpoint, and a schema is exactly what Kimi's Responses face accepts"}},
			}},
		{ID: "anthropic.messages.count-tokens.2023-06-01", NorthboundProfile: ProfileAnthropicMessages, ProfileRevision: 1, Protocol: "anthropic", Method: "POST", Path: "/v1/messages/count_tokens", SemanticOperation: semantic.OperationGenerate,
			RequestFields:         []string{"model", "messages", "messages[].role", "messages[].content", "system", "tools", "tool_choice", "thinking", "output_config"},
			RejectedRequestFields: []string{"stream", "Halro-Route-Mode: portable", "unknown top-level fields"},
			RequestHeaders:        []string{"x-api-key or Authorization", "anthropic-version", "anthropic-beta", "Content-Type"}, ResponseFields: []string{"input_tokens"}, StateSemantics: "stateless; served natively against one direct Anthropic connection", SDKMatrix: []string{"anthropic-go", "anthropic-typescript", "anthropic-python"}, Evidence: []EvidenceKind{EvidenceGatewayContract, EvidenceProviderTransportFixture, EvidenceSDKBlackBox}, Status: StatusCompatible, DocumentedDeviations: []string{"only the direct Anthropic Messages provider profile serves this endpoint; Bedrock Mantle shares the Messages wire format but its count_tokens surface is not established", "the request body reaches the provider verbatim with only the upstream model identifier substituted, so there is no portable execution mode to select", "Anthropic does not bill count_tokens, and Halro settles it at zero cost — but it is a real provider call on the operator's credential, so it takes a ledger attempt and appears in the audit trail like any other", "the reported count is Anthropic's, for the upstream model behind the alias; it is not re-derived by Halro"}, ProviderProfiles: []domain.ProviderProfileID{domain.ProfileAnthropicMessages}, ProfileCoverage: []ProfileCoverage{
				{ProfileID: domain.ProfileAnthropicMessages, DeclaredTransforms: []string{"only the model identifier is substituted; every other member is forwarded unchanged"}},
			}},
	}
	// The three deferred lifecycle endpoints carry an identifier and nothing
	// else, so no provider profile can be short a request field on them. Their
	// coverage rows say which profiles can own such a record; they enumerate no
	// losses because there are none to enumerate.
	deferredCoverage := make([]ProfileCoverage, len(responseProfiles))
	for index, profileID := range responseProfiles {
		deferredCoverage[index] = ProfileCoverage{ProfileID: profileID}
	}
	manifests = append(manifests,
		EndpointCompatibilityManifest{ID: "openai.responses.get.v1", NorthboundProfile: ProfileOpenAIResponses, ProfileRevision: 2, Protocol: "openai", Method: "GET", Path: "/v1/responses/{id}", SemanticOperation: semantic.OperationGenerate,
			RequestFields: []string{"id"}, RequestHeaders: []string{"Authorization"},
			ResponseFields: []string{"id", "object", "created_at", "completed_at", "status", "background", "error", "model", "output", "usage"},
			StateSemantics: "project-owned deferred record; polling is authoritative and writes no accounting events",
			Evidence:       []EvidenceKind{EvidenceGatewayContract}, Status: StatusExperimental,
			DocumentedDeviations: []string{"only a submission made with background=true is retrievable; a synchronous response is never stored", "the polling cadence is carried in the Retry-After header rather than as a non-standard member of the Response object", "retrieval makes no upstream call and writes no ledger events", "the record is reaped 15 minutes after its first successful retrieval, or at its 24 hour TTL, whichever comes first"},
			ProviderProfiles:     responseProfiles, ProfileCoverage: deferredCoverage},
		EndpointCompatibilityManifest{ID: "openai.responses.cancel.v1", NorthboundProfile: ProfileOpenAIResponses, ProfileRevision: 2, Protocol: "openai", Method: "POST", Path: "/v1/responses/{id}/cancel", SemanticOperation: semantic.OperationGenerate,
			RequestFields: []string{"id"}, RequestHeaders: []string{"Authorization"},
			ResponseFields: []string{"id", "object", "created_at", "completed_at", "status", "background", "error", "model", "output", "usage"},
			StateSemantics: "project-owned deferred record",
			Evidence:       []EvidenceKind{EvidenceGatewayContract}, Status: StatusExperimental,
			DocumentedDeviations: []string{"cancelling a queued submission is determinate; cancelling one already running is best-effort and is settled conservatively, so the record says plainly that it may have been billed upstream", "a request that has already finished answers 409 rather than pretending to cancel"},
			ProviderProfiles:     responseProfiles, ProfileCoverage: deferredCoverage},
		EndpointCompatibilityManifest{ID: "openai.responses.delete.v1", NorthboundProfile: ProfileOpenAIResponses, ProfileRevision: 2, Protocol: "openai", Method: "DELETE", Path: "/v1/responses/{id}", SemanticOperation: semantic.OperationGenerate,
			RequestFields: []string{"id"}, RequestHeaders: []string{"Authorization"},
			ResponseFields: []string{"id", "object", "deleted"},
			StateSemantics: "project-owned deferred record",
			Evidence:       []EvidenceKind{EvidenceGatewayContract}, Status: StatusExperimental,
			DocumentedDeviations: []string{"deletion removes the record and both of its sealed objects; it does not undo accounting for work that already happened", "a submission still owed an answer answers 409: cancel it first"},
			ProviderProfiles:     responseProfiles, ProfileCoverage: deferredCoverage},
	)
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
