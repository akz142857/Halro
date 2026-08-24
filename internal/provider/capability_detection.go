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

// CapabilityDetectorContractVersion is part of the detection selection
// fingerprint, so bumping it stops a stored result from being reused under
// semantics that did not produce it. v2 is the first version whose bad-request
// classifier reads the code half of a joined provider identifier — a v1 result
// recorded "inconclusive" where v2 would record "unsupported".
// maxDetectionProbes bounds how many possibly billable calls one plan may ask
// for. It is a cost ceiling, not a capability ceiling: a profile that serves
// more than this many probeable capabilities gets the ones the plan lists and
// defers the rest by name, so raising the ceiling stays a decision somebody
// makes rather than a silent truncation nobody sees.
const maxDetectionProbes = 8

const CapabilityDetectorContractVersion = "capability-detector-v2"

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
	// Deferred names capabilities the profile can serve and the call budget
	// could not fit. The budget used to drop them by returning early, which is
	// indistinguishable from a capability the plan deliberately never reaches —
	// and the two are recorded differently, because one is a policy and the
	// other is a ceiling somebody may want raised.
	Deferred []string `json:"deferred,omitempty"`
}

type CapabilityDetector interface {
	CapabilityDetectionPlan(ModelCapabilityDetectionTarget) (CapabilityDetectionPlan, error)
	DetectCapability(context.Context, ModelCapabilityDetectionTarget, CapabilityProbe) domain.CapabilityProbeResult
}

// CapabilityDetectionPlan is deliberately derived from the immutable profile
// wrapper. It can only offer operations the registered adapter/profile ceiling
// already permits, and excludes every persistent or high-cost primitive.
//
// Which ones that excludes, so the omission is not read as an oversight and
// re-raised as a gap: images, speech, transcription and async generation are
// high-cost — one probe is a real generation, billed at generation prices, on
// every model an operator asks about. Files and batches are persistent: a probe
// would create an object on the operator's account that nothing here deletes.
//
// The consequence is worth stating plainly, because it is a real limit rather
// than a free choice. Those capabilities can only ever hold declared evidence.
// They are still filtered before Provider I/O and their unsupported fields are
// still rejected, but nothing will automatically discover that a declaration
// and the upstream disagree. Verifying them needs an operator-initiated action
// that accepts the cost, which is a different mechanism from this one.
func (b *LegacyAdapterBridge) CapabilityDetectionPlan(target ModelCapabilityDetectionTarget) (CapabilityDetectionPlan, error) {
	if target.RiskTier != "safe_automatic" || target.ProviderModel == "" || target.BindingID == "" || target.ProfileID != b.manifest.ID {
		return CapabilityDetectionPlan{}, errors.New("capability detection target does not match adapter profile")
	}
	c := b.Capabilities()
	probes := make([]CapabilityProbe, 0, maxDetectionProbes)
	var deferred []string
	add := func(capability, kind string, dependencies ...string) {
		if len(probes) == maxDetectionProbes {
			deferred = append(deferred, capability)
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
	// Reasoning is added last on purpose. It is the probe whose criterion is the
	// softest — an upstream that ignores the parameter answers exactly like one
	// that has no reasoning to report — so when the budget cannot fit every
	// capability the profile serves, this is the one to give up. Ordering is the
	// whole mechanism: add() fills until the ceiling and defers the rest.
	if c.Reasoning {
		add("reasoning", "reasoning_effort", "chat")
	}
	if len(probes) == 0 {
		return CapabilityDetectionPlan{}, errors.New("adapter profile has no safe automatic capability probes")
	}
	return CapabilityDetectionPlan{ContractVersion: CapabilityDetectorContractVersion, Probes: probes, MaxCalls: len(probes), Deferred: deferred}, nil
}

func (b *LegacyAdapterBridge) DetectCapability(ctx context.Context, target ModelCapabilityDetectionTarget, probe CapabilityProbe) domain.CapabilityProbeResult {
	result := domain.CapabilityProbeResult{Status: domain.ProbeInconclusive, BindingID: target.BindingID, ProbeKind: probe.Kind}
	maxTokens := probe.MaxOutputTokens
	request := openaiapi.ChatCompletionRequest{Model: target.ProviderModel,
		Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("Reply briefly.")}}}
	// Which output-limit parameter a probe may send is a property of the
	// upstream. OpenAI's current models reject max_tokens outright and take
	// max_completion_tokens; DeepSeek accepts only max_tokens, and its adapter
	// now refuses to put the other one on the wire, which would have failed
	// every DeepSeek probe before it left the process.
	//
	// Choosing here is not the silent rewrite the request path forbids. This
	// bound is Halro's own — it exists to keep a probe cheap — so there is no
	// caller intent to preserve, only a parameter name to get right.
	if target.ProfileID == domain.ProfileDeepSeekChat {
		request.MaxTokens = &maxTokens
	} else {
		request.MaxCompletionTokens = &maxTokens
	}
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
	case "reasoning_effort":
		// The evidence has to be something only a reasoning model produces. An
		// upstream that does not reason answers this request exactly like any
		// other — same shape, same fields — so "it did not refuse the parameter"
		// proves nothing and is not accepted here. What is accepted is the
		// upstream reporting a reasoning span it billed for, or returning
		// reasoning content alongside the answer. Both are things the model did,
		// not things it declined to complain about.
		//
		// The effort level is the lowest the ladder offers above "none": the
		// probe is asking whether reasoning happens at all, and paying for a
		// deep one to learn that would be paying for the wrong answer.
		request.ReasoningEffort = "minimal"
		var response openaiapi.ChatCompletionResponse
		response, err = b.Chat(ctx, ChatCall{RequestID: "capability-detection", ProviderModel: target.ProviderModel, Request: request})
		if err == nil && len(response.Choices) > 0 && response.Choices[0].Message != nil {
			reasoned := response.Usage != nil && response.Usage.ReasoningTokens() > 0
			reasoned = reasoned || strings.TrimSpace(response.Choices[0].Message.ReasoningContent) != ""
			if reasoned {
				result.Status, result.Evidence = domain.ProbeSupported, domain.EvidenceVerified
			}
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
	// The upstream answered and the answer did not carry the evidence. Every
	// case above only ever upgrades the status, and the zero value it starts
	// from is "could not tell" — which is what a refused request Halro could
	// not read looks like too. Those are opposite situations with opposite next
	// steps, and folding them together left the second one with no record at
	// all: ErrorClass is written only on the error path, so a tool probe whose
	// reply simply carried no tool call left nothing behind but the word
	// "inconclusive".
	if err == nil && result.Status == domain.ProbeInconclusive {
		result.Status = domain.ProbeAssertionFailed
	}
	if err != nil {
		result.Status, result.Evidence, result.ErrorClass = classifyCapabilityProbeError(err), "", capabilityProbeErrorClass(err)
		// What the upstream said about the request, in identifiers only. The
		// class says what kind of failure it was; these two say which request
		// and which field, which is the part an operator can act on. The
		// sentence beside them is a provider response body and stays inside the
		// error — this record is durable and is served to the console.
		var classified *Error
		if errors.As(err, &classified) {
			if classified.StatusCode >= 100 && classified.StatusCode <= 599 {
				result.ProviderStatus = classified.StatusCode
			}
			result.ProviderCode = SafeProviderIdentifier(classified.ProviderCode)
		}
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
		//
		// The identifier arrives joined to the parameter it names —
		// "unsupported_parameter:image_url" — because the refused field is the
		// half an operator can act on and the error contract has one identifier
		// field to carry both. Comparing the whole string therefore matched only
		// when the upstream named no parameter, and an OpenAI-shaped body always
		// names one: every refused probe on that family (OpenAI, OpenAI-compatible
		// endpoints, both Bedrock Mantle routes) landed as inconclusive, and this
		// branch was unreachable. The verdict belongs to the code, so compare the
		// code and let the parameter travel with it.
		code, _, _ := strings.Cut(providerError.ProviderCode, ":")
		if strings.EqualFold(code, "unsupported_parameter") || strings.EqualFold(code, "unsupported_value") {
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
