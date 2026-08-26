package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

type capabilityDetectorAdapter struct {
	errorFor map[string]error
	requests []openaiapi.ChatCompletionRequest
	// toolsAnswerWithoutCall makes the upstream answer a tool probe normally and
	// simply not call the tool — the shape the probe's own assertion rejects,
	// with no error anywhere.
	toolsAnswerWithoutCall bool
}

type resourceCapabilityDetectorAdapter struct {
	capabilityDetectorAdapter
	capabilities Capabilities
}

func (a *resourceCapabilityDetectorAdapter) Capabilities() Capabilities { return a.capabilities }

func (*resourceCapabilityDetectorAdapter) Moderate(context.Context, ModerationCall) (ModerationResult, error) {
	return ModerationResult{Results: json.RawMessage(`[{"flagged":false}]`)}, nil
}
func (*resourceCapabilityDetectorAdapter) GenerateImage(context.Context, ImageCall) (ImageResult, error) {
	return ImageResult{}, nil
}
func (*resourceCapabilityDetectorAdapter) Transcribe(context.Context, TranscriptionCall) (TranscriptionResult, error) {
	return TranscriptionResult{}, nil
}
func (*resourceCapabilityDetectorAdapter) Synthesize(context.Context, SpeechCall) (SpeechResult, error) {
	return SpeechResult{}, nil
}
func (*resourceCapabilityDetectorAdapter) Rerank(context.Context, RerankCall) (RerankResult, error) {
	return RerankResult{Results: []RerankItem{{Index: 0, RelevanceScore: 0.9}}}, nil
}
func (*resourceCapabilityDetectorAdapter) StartAsyncInvoke(context.Context, AsyncInvokeCall) (AsyncInvokeObject, error) {
	return AsyncInvokeObject{}, nil
}
func (*resourceCapabilityDetectorAdapter) GetAsyncInvoke(context.Context, string, string) (AsyncInvokeObject, error) {
	return AsyncInvokeObject{}, nil
}
func (*resourceCapabilityDetectorAdapter) GenerateBedrockImage(context.Context, ImageCall) (ImageResult, error) {
	return ImageResult{}, nil
}

// reasoningDetectorAdapter answers every request normally and reports exactly
// what the test tells it to report about reasoning.
type reasoningDetectorAdapter struct {
	capabilityDetectorAdapter
	reasoningTokens  int64
	reasoningContent string
	lastEffort       string
}

func (a *reasoningDetectorAdapter) Chat(_ context.Context, call ChatCall) (openaiapi.ChatCompletionResponse, error) {
	a.lastEffort = call.Request.ReasoningEffort
	usage := &openaiapi.Usage{TotalTokens: 3}
	usage.SetReasoningTokens(a.reasoningTokens)
	return openaiapi.ChatCompletionResponse{
		Choices: []openaiapi.Choice{{Message: &openaiapi.Message{
			Content: openaiapi.TextContent("ok"), ReasoningContent: a.reasoningContent}}},
		Usage: usage,
	}, nil
}

func (a *capabilityDetectorAdapter) Type() string { return string(domain.ProviderOpenAI) }
func (a *capabilityDetectorAdapter) Close()       {}
func (a *capabilityDetectorAdapter) Capabilities() Capabilities {
	return Capabilities{Chat: true, Streaming: true, Embeddings: true, Tools: true, Vision: true, JSONMode: true, DeveloperRole: true, StreamUsage: true}
}
func (a *capabilityDetectorAdapter) Chat(_ context.Context, call ChatCall) (openaiapi.ChatCompletionResponse, error) {
	a.requests = append(a.requests, call.Request)
	if len(call.Request.Tools) > 0 {
		if err := a.errorFor["tools"]; err != nil {
			return openaiapi.ChatCompletionResponse{}, err
		}
		if a.toolsAnswerWithoutCall {
			return openaiapi.ChatCompletionResponse{Choices: []openaiapi.Choice{{Message: &openaiapi.Message{Content: openaiapi.TextContent("I cannot call tools.")}}}}, nil
		}
		return openaiapi.ChatCompletionResponse{Choices: []openaiapi.Choice{{Message: &openaiapi.Message{ToolCalls: []openaiapi.ToolCall{{ID: "call", Type: "function"}}}}}}, nil
	}
	if len(call.Request.ResponseFormat) > 0 {
		return openaiapi.ChatCompletionResponse{Choices: []openaiapi.Choice{{Message: &openaiapi.Message{Content: openaiapi.TextContent(`{"ok":true}`)}}}}, nil
	}
	return openaiapi.ChatCompletionResponse{Choices: []openaiapi.Choice{{Message: &openaiapi.Message{Content: openaiapi.TextContent("ok")}}}}, nil
}
func (a *capabilityDetectorAdapter) ChatStream(_ context.Context, _ ChatCall, emit func(semantic.Event) error) (*openaiapi.Usage, error) {
	_ = emit(semantic.Event{Outputs: []semantic.OutputDelta{{Termination: "stop"}}})
	return &openaiapi.Usage{TotalTokens: 2}, nil
}
func (a *capabilityDetectorAdapter) Embed(_ context.Context, _ EmbeddingCall) (openaiapi.EmbeddingResponse, error) {
	return openaiapi.EmbeddingResponse{Data: []openaiapi.EmbeddingData{{Embedding: json.RawMessage(`[0.1,0.2]`)}}}, nil
}

func TestLegacyProfileCapabilityDetectorHasBoundedSideEffectFreePlan(t *testing.T) {
	manifest, ok := BuiltinProfile(domain.ProfileOpenAIChatEmbeddings)
	if !ok {
		t.Fatal("profile missing")
	}
	adapter := &capabilityDetectorAdapter{}
	bridge, err := NewLegacyAdapterBridge(adapter, manifest, domain.EvidenceForCapabilities(domain.DefaultProviderCapabilities(domain.ProviderOpenAI), domain.EvidenceDeclared))
	if err != nil {
		t.Fatal(err)
	}
	target := ModelCapabilityDetectionTarget{ProviderModel: "unknown", BindingID: "binding", ProfileID: manifest.ID, RiskTier: "safe_automatic"}
	plan, err := bridge.CapabilityDetectionPlan(target)
	if err != nil {
		t.Fatal(err)
	}
	if plan.MaxCalls > 8 || len(plan.Probes) > 8 {
		t.Fatalf("plan=%#v", plan)
	}
	for _, probe := range plan.Probes {
		if probe.PersistentEffect || !probe.MayBill || probe.MaxInputBytes > 2048 || probe.MaxOutputTokens > 16 {
			t.Fatalf("unsafe probe=%#v", probe)
		}
		result := bridge.DetectCapability(context.Background(), target, probe)
		if result.Status != domain.ProbeSupported || result.Evidence != domain.EvidenceVerified {
			t.Fatalf("probe=%s result=%#v", probe.Capability, result)
		}
	}
}

func TestCapabilityDetectorDoesNotTreatFreeFormBadRequestAsUnsupported(t *testing.T) {
	manifest, _ := BuiltinProfile(domain.ProfileOpenAIChatEmbeddings)
	adapter := &capabilityDetectorAdapter{errorFor: map[string]error{"tools": &Error{Class: ErrorBadRequest, StatusCode: 400, Message: "arbitrary upstream text"}}}
	bridge, err := NewLegacyAdapterBridge(adapter, manifest, domain.EvidenceForCapabilities(domain.DefaultProviderCapabilities(domain.ProviderOpenAI), domain.EvidenceDeclared))
	if err != nil {
		t.Fatal(err)
	}
	target := ModelCapabilityDetectionTarget{ProviderModel: "unknown", BindingID: "binding", ProfileID: manifest.ID, RiskTier: "safe_automatic"}
	result := bridge.DetectCapability(context.Background(), target, CapabilityProbe{Capability: "tools", Kind: "tool_call"})
	if result.Status != domain.ProbeInconclusive {
		t.Fatalf("result=%#v", result)
	}
	adapter.errorFor["tools"] = &Error{Class: ErrorAuthentication, StatusCode: 401}
	if result = bridge.DetectCapability(context.Background(), target, CapabilityProbe{Capability: "tools", Kind: "tool_call"}); result.Status != domain.ProbeUnauthorized {
		t.Fatalf("result=%#v", result)
	}
}

// The refused parameter travels inside the same identifier field as the code
// that names the refusal, because that field is the only one the error contract
// has and the parameter is the half an operator acts on. The verdict is the
// code's, so it has to survive the parameter being appended.
//
// This is driven through the bridge rather than the classifier alone: the bug
// this pins was a classifier that read a whole string an adapter had already
// joined, and a test that asserted only the joined string passed happily while
// the verdict it decided never changed.
func TestCapabilityDetectorReadsTheCodeHalfOfAJoinedRefusal(t *testing.T) {
	manifest, _ := BuiltinProfile(domain.ProfileOpenAIChatEmbeddings)
	adapter := &capabilityDetectorAdapter{errorFor: map[string]error{}}
	bridge, err := NewLegacyAdapterBridge(adapter, manifest, domain.EvidenceForCapabilities(domain.DefaultProviderCapabilities(domain.ProviderOpenAI), domain.EvidenceDeclared))
	if err != nil {
		t.Fatal(err)
	}
	target := ModelCapabilityDetectionTarget{ProviderModel: "unknown", BindingID: "binding", ProfileID: manifest.ID, RiskTier: "safe_automatic"}
	for _, test := range []struct {
		code string
		want domain.CapabilityProbeStatus
	}{
		{"unsupported_parameter", domain.ProbeUnsupported},
		{"unsupported_parameter:image_url", domain.ProbeUnsupported},
		{"unsupported_value:input_image", domain.ProbeUnsupported},
		{"UNSUPPORTED_PARAMETER:image_url", domain.ProbeUnsupported},
		// A code Halro has not reviewed stays inconclusive whether or not it
		// names a parameter. Splitting must not widen what counts as a verdict.
		{"invalid_request_error:image_url", domain.ProbeInconclusive},
		{"model_not_found", domain.ProbeInconclusive},
		{"", domain.ProbeInconclusive},
	} {
		adapter.errorFor["tools"] = &Error{Class: ErrorBadRequest, StatusCode: 400, ProviderCode: test.code, Message: "arbitrary upstream text"}
		result := bridge.DetectCapability(context.Background(), target, CapabilityProbe{Capability: "tools", Kind: "tool_call"})
		if result.Status != test.want {
			t.Fatalf("code %q classified as %s, want %s", test.code, result.Status, test.want)
		}
	}
}

// A probe that failed used to record only Halro's own classification, which
// says what kind of failure without saying which field — and for a probe, no
// log line is written at all, so "could not tell" was the end of the trail.
// The upstream's status and identifier are recorded; its sentence is not.
func TestCapabilityProbeRecordsTheUpstreamIdentifiersAndNotItsSentence(t *testing.T) {
	manifest, _ := BuiltinProfile(domain.ProfileOpenAIChatEmbeddings)
	adapter := &capabilityDetectorAdapter{errorFor: map[string]error{}}
	bridge, err := NewLegacyAdapterBridge(adapter, manifest, domain.EvidenceForCapabilities(domain.DefaultProviderCapabilities(domain.ProviderOpenAI), domain.EvidenceDeclared))
	if err != nil {
		t.Fatal(err)
	}
	target := ModelCapabilityDetectionTarget{ProviderModel: "unknown", BindingID: "binding", ProfileID: manifest.ID, RiskTier: "safe_automatic"}
	probe := CapabilityProbe{Capability: "tools", Kind: "tool_call"}

	adapter.errorFor["tools"] = &Error{Class: ErrorBadRequest, StatusCode: 400,
		ProviderCode: "invalid_request_error:tool_choice", Message: "Your tool_choice is not valid for this model."}
	result := bridge.DetectCapability(context.Background(), target, probe)
	if result.ProviderStatus != 400 || result.ProviderCode != "invalid_request_error:tool_choice" {
		t.Fatalf("upstream identifiers lost: %#v", result)
	}
	if strings.Contains(result.ProviderCode, "not valid for this model") {
		t.Fatalf("provider sentence reached the record: %#v", result)
	}

	// An indexed JSON path is the shape the parameter half actually arrives in,
	// and it is the half an operator needs: "unsupported_parameter" alone names
	// a category, not a field. It survives narrowing intact.
	adapter.errorFor["tools"] = &Error{Class: ErrorBadRequest, StatusCode: 400,
		ProviderCode: "unsupported_parameter:messages[0].content"}
	if result = bridge.DetectCapability(context.Background(), target, probe); result.ProviderCode != "unsupported_parameter:messages[0].content" {
		t.Fatalf("the refused parameter was lost: %q", result.ProviderCode)
	}
	if result.Status != domain.ProbeUnsupported {
		t.Fatalf("verdict changed with the parameter: %s", result.Status)
	}

	// Narrowed at capture, so nothing downstream has to re-derive the rule. A
	// parameter genuinely outside the set is dropped on its own — losing it must
	// not take the code half with it, because the code is what decides the
	// verdict and the parameter only annotates it.
	adapter.errorFor["tools"] = &Error{Class: ErrorBadRequest, StatusCode: 400,
		ProviderCode: `unsupported_parameter:the "messages" field you sent`}
	if result = bridge.DetectCapability(context.Background(), target, probe); result.ProviderCode != "unsupported_parameter" {
		t.Fatalf("code half not salvaged: %q", result.ProviderCode)
	}
	if result.Status != domain.ProbeUnsupported {
		t.Fatalf("verdict changed with the parameter: %s", result.Status)
	}

	// A status outside the HTTP range is not a status. Transport failures carry
	// none at all, and a zero must stay a zero so the field stays omitted.
	adapter.errorFor["tools"] = &Error{Class: ErrorConnect, ProviderCode: strings.Repeat("x", MaxProviderIdentifierLength+1)}
	if result = bridge.DetectCapability(context.Background(), target, probe); result.ProviderStatus != 0 || result.ProviderCode != "" {
		t.Fatalf("unbounded or statusless value stored: %#v", result)
	}
}

// "The upstream refused and Halro could not read why" and "the upstream
// answered and the answer proved nothing" are opposite situations that used to
// share one word. Only the first is worth identifying again — the second sends
// the identical request and gets the identical answer.
func TestCapabilityProbeSeparatesAFailedAssertionFromAnUnreadableRefusal(t *testing.T) {
	manifest, _ := BuiltinProfile(domain.ProfileOpenAIChatEmbeddings)
	// No error and no tool call in the reply: the probe's own assertion is what
	// fails, and nothing about the request produced it.
	adapter := &capabilityDetectorAdapter{errorFor: map[string]error{}, toolsAnswerWithoutCall: true}
	bridge, err := NewLegacyAdapterBridge(adapter, manifest, domain.EvidenceForCapabilities(domain.DefaultProviderCapabilities(domain.ProviderOpenAI), domain.EvidenceDeclared))
	if err != nil {
		t.Fatal(err)
	}
	target := ModelCapabilityDetectionTarget{ProviderModel: "unknown", BindingID: "binding", ProfileID: manifest.ID, RiskTier: "safe_automatic"}
	probe := CapabilityProbe{Capability: "tools", Kind: "tool_call"}

	result := bridge.DetectCapability(context.Background(), target, probe)
	if result.Status != domain.ProbeAssertionFailed {
		t.Fatalf("a bare answer was not recorded as a failed assertion: %#v", result)
	}
	if result.Evidence == domain.EvidenceVerified {
		t.Fatalf("a failed assertion carried verified evidence: %#v", result)
	}

	// A refusal Halro could not read stays inconclusive: the same request may
	// parse next time, so re-running is a real next step there.
	adapter.errorFor["tools"] = &Error{Class: ErrorBadRequest, StatusCode: 400, ProviderCode: "some_unreviewed_code"}
	if result = bridge.DetectCapability(context.Background(), target, probe); result.Status != domain.ProbeInconclusive {
		t.Fatalf("an unreadable refusal was reclassified: %#v", result)
	}

	// A probe kind the plan does not implement is neither: it was never asked.
	delete(adapter.errorFor, "tools")
	if result = bridge.DetectCapability(context.Background(), target, CapabilityProbe{Capability: "tools", Kind: "no_such_kind"}); result.Status != domain.ProbeNotProbed {
		t.Fatalf("an unimplemented probe kind was reclassified: %#v", result)
	}
	if !domain.ProbeAssertionFailed.Valid() {
		t.Fatal("the new status is not accepted by the domain validator, so no record carrying it can be stored")
	}
}

// Reasoning is the one probe whose criterion cannot be "the upstream did not
// complain": a model that ignores reasoning_effort answers identically to one
// that has no reasoning to report. Only something the model produced counts —
// a billed reasoning span, or reasoning content beside the answer.
func TestReasoningProbeAcceptsOnlyProducedEvidence(t *testing.T) {
	manifest, _ := BuiltinProfile(domain.ProfileOpenAIChatEmbeddings)
	adapter := &reasoningDetectorAdapter{}
	bridge, err := NewLegacyAdapterBridge(adapter, manifest, domain.EvidenceForCapabilities(domain.DefaultProviderCapabilities(domain.ProviderOpenAI), domain.EvidenceDeclared))
	if err != nil {
		t.Fatal(err)
	}
	target := ModelCapabilityDetectionTarget{ProviderModel: "unknown", BindingID: "binding", ProfileID: manifest.ID, RiskTier: "safe_automatic"}
	probe := CapabilityProbe{Capability: "reasoning", Kind: "reasoning_effort"}

	// Answers the request, reports nothing. Not a refusal, not evidence.
	if result := bridge.DetectCapability(context.Background(), target, probe); result.Status != domain.ProbeAssertionFailed {
		t.Fatalf("silence was read as reasoning: %#v", result)
	}
	adapter.reasoningTokens = 12
	if result := bridge.DetectCapability(context.Background(), target, probe); result.Status != domain.ProbeSupported || result.Evidence != domain.EvidenceVerified {
		t.Fatalf("a billed reasoning span was not accepted: %#v", result)
	}
	adapter.reasoningTokens, adapter.reasoningContent = 0, "thinking"
	if result := bridge.DetectCapability(context.Background(), target, probe); result.Status != domain.ProbeSupported {
		t.Fatalf("returned reasoning content was not accepted: %#v", result)
	}
	// The probe asks whether reasoning happens at all, so it buys the cheapest
	// level the ladder offers above "none".
	if adapter.lastEffort != "minimal" {
		t.Fatalf("probe asked for effort %q", adapter.lastEffort)
	}
}

// A budget that cannot fit every capability the profile serves says which ones
// it dropped. Returning early instead made a ceiling look like a policy.
func TestCapabilityDetectionPlanNamesWhatTheBudgetDropped(t *testing.T) {
	// This profile serves eight probeable capabilities plus reasoning, which is
	// one more than the budget. Reasoning is added last precisely so it is the
	// one deferred.
	manifest, _ := BuiltinProfile(domain.ProfileOpenAIChatEmbeddings)
	adapter := &resourceCapabilityDetectorAdapter{capabilities: Capabilities{
		Chat: true, Streaming: true, StreamUsage: true, Tools: true, JSONMode: true,
		DeveloperRole: true, Vision: true, Embeddings: true, Reasoning: true,
	}}
	bridge, err := NewLegacyAdapterBridge(adapter, manifest, domain.EvidenceForCapabilities(domain.DefaultProviderCapabilities(domain.ProviderOpenAI), domain.EvidenceDeclared))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := bridge.CapabilityDetectionPlan(ModelCapabilityDetectionTarget{
		ProviderModel: "unknown", BindingID: "binding", ProfileID: manifest.ID, RiskTier: "safe_automatic"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Probes) != maxDetectionProbes {
		t.Fatalf("plan spends %d of %d calls", len(plan.Probes), maxDetectionProbes)
	}
	if len(plan.Deferred) != 1 || plan.Deferred[0] != "reasoning" {
		t.Fatalf("budget deferral=%v, want exactly [reasoning]", plan.Deferred)
	}
	for _, name := range plan.Deferred {
		for _, probe := range plan.Probes {
			if probe.Capability == name {
				t.Fatalf("%q is both planned and deferred", name)
			}
		}
	}
}

func TestCapabilityDetectorUsesStructuredModerationAndRerankResults(t *testing.T) {
	adapter := &resourceCapabilityDetectorAdapter{}
	for _, test := range []struct {
		profile    domain.ProviderProfileID
		capability string
		kind       string
	}{
		{domain.ProfileOpenAIMediaResources, "moderations", "moderation"},
		{domain.ProfileBedrockAgentRerankCohere35, "rerank", "rerank"},
	} {
		t.Run(test.capability, func(t *testing.T) {
			manifest, ok := BuiltinProfile(test.profile)
			if !ok {
				t.Fatal("profile missing")
			}
			adapter.capabilities = Capabilities{Moderations: test.capability == "moderations", Rerank: test.capability == "rerank"}
			bridge := &LegacyAdapterBridge{Adapter: adapter, manifest: manifest}
			target := ModelCapabilityDetectionTarget{ProviderModel: "unknown", BindingID: "binding", ProfileID: manifest.ID, RiskTier: "safe_automatic"}
			plan, err := bridge.CapabilityDetectionPlan(target)
			if err != nil || len(plan.Probes) != 1 || plan.Probes[0].Capability != test.capability {
				t.Fatalf("plan=%#v err=%v", plan, err)
			}
			result := bridge.DetectCapability(context.Background(), target, CapabilityProbe{Capability: test.capability, Kind: test.kind})
			if result.Status != domain.ProbeSupported || result.Evidence != domain.EvidenceVerified {
				t.Fatalf("result=%#v", result)
			}
		})
	}
}

// The detection plan is derived from the adapter's capabilities, and it can
// bill. For a profile whose ceiling the build fixes, that makes the ceiling the
// bound on what a detection run may spend probes on — a plan may never contain
// a probe for something the profile does not declare.
//
// Verified by inverting it: an adapter that declares embeddings — the shape of
// the pre-Phase-0 bug, where a widened binding reached the adapter — makes this
// fail on all three profiles, because the planner does add an embeddings probe.
// (Reasoning has no probe of its own, so the Responses profile's missing
// reasoning ceiling is not what this test can demonstrate.)
func TestCapabilityDetectionPlanStaysInsideTheProfileCeiling(t *testing.T) {
	for _, profileID := range []domain.ProviderProfileID{
		domain.ProfileBedrockMantleOpenAIChat,
		domain.ProfileBedrockMantleOpenAIResponses,
		domain.ProfileBedrockMantleAnthropicMessages,
	} {
		manifest, ok := BuiltinProfile(profileID)
		if !ok {
			t.Fatalf("%s is not a registered profile", profileID)
		}
		ceiling := domain.DefaultProviderCapabilitiesForProfile(domain.ProviderBedrock, profileID)
		bridge, err := NewLegacyAdapterBridge(
			&bedrockCapabilityDetectorAdapter{resourceCapabilityDetectorAdapter{capabilities: capabilitiesFromDomain(ceiling)}},
			manifest, nil,
		)
		if err != nil {
			t.Fatalf("%s: %v", profileID, err)
		}
		plan, err := bridge.CapabilityDetectionPlan(ModelCapabilityDetectionTarget{
			RiskTier: "safe_automatic", ProviderModel: "model", BindingID: "binding_1", ProfileID: profileID,
		})
		if err != nil {
			t.Fatalf("%s: %v", profileID, err)
		}
		declared := map[string]bool{
			"chat": ceiling.Chat, "streaming": ceiling.Streaming, "stream_usage": ceiling.StreamUsage,
			"tools": ceiling.Tools, "json_mode": ceiling.JSONMode, "developer_role": ceiling.DeveloperRole,
			"vision": ceiling.Vision, "embeddings": ceiling.Embeddings, "reasoning": ceiling.Reasoning,
			"moderations": ceiling.Moderations, "rerank": ceiling.Rerank,
		}
		for _, probe := range plan.Probes {
			if !declared[probe.Capability] {
				t.Fatalf("%s planned a billable probe for %q, which its ceiling does not declare", profileID, probe.Capability)
			}
		}
	}
}

// The Mantle profiles are Bedrock profiles, and the bridge refuses an adapter
// whose provider type disagrees with the manifest.
type bedrockCapabilityDetectorAdapter struct {
	resourceCapabilityDetectorAdapter
}

func (*bedrockCapabilityDetectorAdapter) Type() string { return string(domain.ProviderBedrock) }

func capabilitiesFromDomain(declared domain.ProviderCapabilities) Capabilities {
	return Capabilities{
		Chat: declared.Chat, Streaming: declared.Streaming, Embeddings: declared.Embeddings,
		Tools: declared.Tools, Vision: declared.Vision, JSONMode: declared.JSONMode,
		DeveloperRole: declared.DeveloperRole, Reasoning: declared.Reasoning,
		StreamUsage: declared.StreamUsage, Moderations: declared.Moderations,
		Images: declared.Images, Transcriptions: declared.Transcriptions, Speech: declared.Speech,
		Files: declared.Files, Batches: declared.Batches, Rerank: declared.Rerank,
		AsyncGenerate:    declared.AsyncGenerate,
		MaxContextTokens: declared.MaxContextTokens, MaxOutputTokens: declared.MaxOutputTokens,
	}
}
