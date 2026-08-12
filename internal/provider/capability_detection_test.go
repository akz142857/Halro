package provider

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

type capabilityDetectorAdapter struct {
	errorFor map[string]error
	requests []openaiapi.ChatCompletionRequest
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
