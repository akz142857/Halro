package provider

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

const CapabilityDetectorContractVersion = "capability-detector-v1"

type ModelCapabilityDetectionTarget struct {
	ProviderModel string
	BindingID     string
	ProfileID     domain.ProviderProfileID
	RiskTier      string
}

type CapabilityProbe struct {
	Capability       string   `json:"capability"`
	Kind             string   `json:"kind"`
	DependsOn        []string `json:"depends_on,omitempty"`
	MaxInputBytes    int64    `json:"max_input_bytes"`
	MaxOutputTokens  int64    `json:"max_output_tokens"`
	MayBill          bool     `json:"may_bill"`
	PersistentEffect bool     `json:"persistent_effect"`
}

type CapabilityDetectionPlan struct {
	ContractVersion string            `json:"contract_version"`
	Probes          []CapabilityProbe `json:"probes"`
	MaxCalls        int               `json:"max_calls"`
}

type CapabilityDetector interface {
	CapabilityDetectionPlan(ModelCapabilityDetectionTarget) (CapabilityDetectionPlan, error)
	DetectCapability(context.Context, ModelCapabilityDetectionTarget, CapabilityProbe) domain.CapabilityProbeResult
}

// CapabilityDetectionPlan is deliberately derived from the immutable profile
// wrapper. It can only offer operations the registered adapter/profile ceiling
// already permits, and excludes every persistent or high-cost primitive.
func (b *LegacyAdapterBridge) CapabilityDetectionPlan(target ModelCapabilityDetectionTarget) (CapabilityDetectionPlan, error) {
	if target.RiskTier != "safe_automatic" || target.ProviderModel == "" || target.BindingID == "" || target.ProfileID != b.manifest.ID {
		return CapabilityDetectionPlan{}, errors.New("capability detection target does not match adapter profile")
	}
	c := b.Capabilities()
	probes := make([]CapabilityProbe, 0, 8)
	add := func(capability, kind string, dependencies ...string) {
		if len(probes) == 8 {
			return
		}
		probes = append(probes, CapabilityProbe{Capability: capability, Kind: kind, DependsOn: dependencies,
			MaxInputBytes: 2048, MaxOutputTokens: 16, MayBill: true})
	}
	if c.Chat {
		add("chat", "minimal_chat")
	}
	if c.Streaming {
		add("streaming", "minimal_stream", "chat")
	}
	if c.StreamUsage {
		add("stream_usage", "stream_usage", "streaming")
	}
	if c.Tools {
		add("tools", "tool_call", "chat")
	}
	if c.JSONMode {
		add("json_mode", "json_object", "chat")
	}
	if c.DeveloperRole {
		add("developer_role", "developer_message", "chat")
	}
	if c.Vision {
		add("vision", "inline_image", "chat")
	}
	if c.Embeddings {
		add("embeddings", "embedding")
	}
	if c.Moderations {
		add("moderations", "moderation")
	}
	if c.Rerank {
		add("rerank", "rerank")
	}
	if len(probes) == 0 {
		return CapabilityDetectionPlan{}, errors.New("adapter profile has no safe automatic capability probes")
	}
	return CapabilityDetectionPlan{ContractVersion: CapabilityDetectorContractVersion, Probes: probes, MaxCalls: len(probes)}, nil
}

func (b *LegacyAdapterBridge) DetectCapability(ctx context.Context, target ModelCapabilityDetectionTarget, probe CapabilityProbe) domain.CapabilityProbeResult {
	result := domain.CapabilityProbeResult{Status: domain.ProbeInconclusive, BindingID: target.BindingID, ProbeKind: probe.Kind}
	maxTokens := probe.MaxOutputTokens
	request := openaiapi.ChatCompletionRequest{Model: target.ProviderModel, MaxCompletionTokens: &maxTokens,
		Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("Reply briefly.")}}}
	var err error
	switch probe.Kind {
	case "minimal_chat":
		var response openaiapi.ChatCompletionResponse
		response, err = b.Chat(ctx, ChatCall{RequestID: "capability-detection", ProviderModel: target.ProviderModel, Request: request})
		if err == nil && len(response.Choices) > 0 && response.Choices[0].Message != nil {
			result.Status, result.Evidence = domain.ProbeSupported, domain.EvidenceVerified
		}
	case "minimal_stream", "stream_usage":
		terminated := false
		var usage *openaiapi.Usage
		usage, err = b.ChatStream(ctx, ChatCall{RequestID: "capability-detection", ProviderModel: target.ProviderModel, Request: request}, func(event semantic.Event) error {
			for _, output := range event.Outputs {
				terminated = terminated || output.Termination != ""
			}
			return nil
		})
		if err == nil && terminated && (probe.Kind != "stream_usage" || usage != nil) {
			result.Status, result.Evidence = domain.ProbeSupported, domain.EvidenceVerified
		}
	case "tool_call":
		request.Tools = []openaiapi.Tool{{Type: "function", Function: openaiapi.ToolFunction{Name: "halro_probe", Description: "Return the fixed value", Parameters: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)}}}
		request.ToolChoice = json.RawMessage(`{"type":"function","function":{"name":"halro_probe"}}`)
		var response openaiapi.ChatCompletionResponse
		response, err = b.Chat(ctx, ChatCall{RequestID: "capability-detection", ProviderModel: target.ProviderModel, Request: request})
		if err == nil && len(response.Choices) > 0 && response.Choices[0].Message != nil && len(response.Choices[0].Message.ToolCalls) > 0 {
			result.Status, result.Evidence = domain.ProbeSupported, domain.EvidenceVerified
		}
	case "json_object":
		request.Messages[0].Content = openaiapi.TextContent("Return a JSON object with ok=true.")
		request.ResponseFormat = json.RawMessage(`{"type":"json_object"}`)
		var response openaiapi.ChatCompletionResponse
		response, err = b.Chat(ctx, ChatCall{RequestID: "capability-detection", ProviderModel: target.ProviderModel, Request: request})
		if err == nil && len(response.Choices) > 0 && response.Choices[0].Message != nil {
			if content, ok := openaiapi.DecodeTextContent(response.Choices[0].Message.Content); ok {
				var object map[string]any
				if json.Unmarshal([]byte(content), &object) == nil {
					result.Status, result.Evidence = domain.ProbeSupported, domain.EvidenceVerified
				}
			}
		}
	case "developer_message":
		request.Messages = append([]openaiapi.Message{{Role: "developer", Content: openaiapi.TextContent("Be brief.")}}, request.Messages...)
		var response openaiapi.ChatCompletionResponse
		response, err = b.Chat(ctx, ChatCall{RequestID: "capability-detection", ProviderModel: target.ProviderModel, Request: request})
		if err == nil && len(response.Choices) > 0 {
			result.Status, result.Evidence = domain.ProbeSupported, domain.EvidenceVerified
		}
	case "inline_image":
		// A 1x1 transparent PNG is fixed, public-domain data. No administrator
		// input or external URL is ever incorporated into the probe.
		request.Messages[0].Content = json.RawMessage(`[{"type":"text","text":"Describe briefly."},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M/wHwAF/gL+XwV0WQAAAABJRU5ErkJggg=="}}]`)
		var response openaiapi.ChatCompletionResponse
		response, err = b.Chat(ctx, ChatCall{RequestID: "capability-detection", ProviderModel: target.ProviderModel, Request: request})
		if err == nil && len(response.Choices) > 0 {
			result.Status, result.Evidence = domain.ProbeSupported, domain.EvidenceVerified
		}
	case "embedding":
		var response openaiapi.EmbeddingResponse
		response, err = b.Embed(ctx, EmbeddingCall{RequestID: "capability-detection", ProviderModel: target.ProviderModel,
			Request: openaiapi.EmbeddingRequest{Model: target.ProviderModel, Input: json.RawMessage(`"halro"`), EncodingFormat: "float"}})
		valid := err == nil && len(response.Data) > 0
		if valid {
			var vector []float64
			valid = json.Unmarshal(response.Data[0].Embedding, &vector) == nil && len(vector) > 0
			for _, value := range vector {
				valid = valid && !math.IsNaN(value) && !math.IsInf(value, 0)
			}
		}
		if valid {
			result.Status, result.Evidence = domain.ProbeSupported, domain.EvidenceVerified
		}
	case "moderation":
		var response ModerationResult
		response, err = b.Moderate(ctx, ModerationCall{RequestID: "capability-detection", ProviderModel: target.ProviderModel, Input: json.RawMessage(`"halro"`)})
		if err == nil && len(response.Results) > 0 && json.Valid(response.Results) {
			result.Status, result.Evidence = domain.ProbeSupported, domain.EvidenceVerified
		}
	case "rerank":
		var response RerankResult
		response, err = b.Rerank(ctx, RerankCall{RequestID: "capability-detection", ProviderModel: target.ProviderModel,
			Query: "halro", Documents: []string{"halro", "gateway"}, TopN: 1})
		if err == nil && len(response.Results) == 1 && response.Results[0].Index >= 0 && response.Results[0].Index < 2 &&
			!math.IsNaN(response.Results[0].RelevanceScore) && !math.IsInf(response.Results[0].RelevanceScore, 0) {
			result.Status, result.Evidence = domain.ProbeSupported, domain.EvidenceVerified
		}
	default:
		result.Status = domain.ProbeNotProbed
	}
	if err != nil {
		result.Status, result.Evidence, result.ErrorClass = classifyCapabilityProbeError(err), "", capabilityProbeErrorClass(err)
	}
	return result
}

func classifyCapabilityProbeError(err error) domain.CapabilityProbeStatus {
	if errors.Is(err, context.Canceled) {
		return domain.ProbeCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return domain.ProbeUnavailable
	}
	var providerError *Error
	if !errors.As(err, &providerError) {
		return domain.ProbeUnavailable
	}
	switch providerError.Class {
	case ErrorAuthentication:
		return domain.ProbeUnauthorized
	case ErrorRateLimit, ErrorTimeout, ErrorProvider5xx, ErrorConnect:
		return domain.ProbeUnavailable
	case ErrorBadRequest:
		// Only a structured, reviewed provider code is accepted as a stable
		// unsupported verdict. Free-form error text is never inspected.
		if strings.EqualFold(providerError.ProviderCode, "unsupported_parameter") || strings.EqualFold(providerError.ProviderCode, "unsupported_value") {
			return domain.ProbeUnsupported
		}
		return domain.ProbeInconclusive
	default:
		return domain.ProbeInconclusive
	}
}

func capabilityProbeErrorClass(err error) string {
	var providerError *Error
	if errors.As(err, &providerError) {
		return string(providerError.Class)
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "unknown"
}
