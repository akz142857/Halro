package compatibility

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

// VolcengineChatRequest is the Ark pay-as-you-go Chat API subset Halro can
// preserve. Source: https://docs.volcengine.com/docs/ark/chat-api?lang=zh
// (reviewed 2026-11-02). Keep it explicit: OpenAI adding a member does not
// establish that Ark reads it.
type VolcengineChatRequest struct {
	Model               string                   `json:"model"`
	Messages            []openaiapi.Message      `json:"messages"`
	Stream              bool                     `json:"stream,omitempty"`
	StreamOptions       *openaiapi.StreamOptions `json:"stream_options,omitempty"`
	Temperature         *float64                 `json:"temperature,omitempty"`
	TopP                *float64                 `json:"top_p,omitempty"`
	MaxTokens           *int64                   `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int64                   `json:"max_completion_tokens,omitempty"`
	Stop                json.RawMessage          `json:"stop,omitempty"`
	Seed                *int64                   `json:"seed,omitempty"`
	Tools               []openaiapi.Tool         `json:"tools,omitempty"`
	ToolChoice          json.RawMessage          `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool                    `json:"parallel_tool_calls,omitempty"`
	ResponseFormat      json.RawMessage          `json:"response_format,omitempty"`
	ReasoningEffort     string                   `json:"reasoning_effort,omitempty"`
	Thinking            *volcengineThinking      `json:"thinking,omitempty"`
}

type volcengineThinking struct {
	Type string `json:"type"`
}

func RenderVolcengineChatRequest(request openaiapi.ChatCompletionRequest) (VolcengineChatRequest, error) {
	return RenderVolcengineChatRequestForModel(request, request.Model)
}

func RenderVolcengineChatRequestForModel(request openaiapi.ChatCompletionRequest, model string) (VolcengineChatRequest, error) {
	if model == "" {
		model = request.Model
	}
	if request.N != nil && *request.N != 1 {
		return VolcengineChatRequest{}, errors.New("Ark Chat supports only one candidate")
	}
	if request.User != "" {
		return VolcengineChatRequest{}, errors.New("Ark Chat has no established user attribution member")
	}
	if !VolcengineReasoningSupported(model, request.ReasoningEffort) {
		return VolcengineChatRequest{}, fmt.Errorf("Ark model %q cannot preserve reasoning effort %q", request.Model, request.ReasoningEffort)
	}
	if request.MaxTokens != nil && request.MaxCompletionTokens != nil {
		return VolcengineChatRequest{}, errors.New("Ark Chat cannot preserve two simultaneous output limits")
	}
	if request.MaxTokens != nil && !volcengineAnswerLimitSupported(model, request.ReasoningEffort) {
		return VolcengineChatRequest{}, errors.New("this Ark model counts reasoning in max_tokens; use max_completion_tokens for a total budget")
	}
	for _, message := range request.Messages {
		if message.Role == "developer" {
			return VolcengineChatRequest{}, errors.New("Ark Chat developer messages have not been established")
		}
	}
	thinking := "disabled"
	effort := request.ReasoningEffort
	if (effort != "" && effort != "none") || VolcengineReasonsUnasked(model) {
		thinking = "enabled"
	}
	if effort == "none" {
		effort = ""
	}
	switchValue := &volcengineThinking{Type: thinking}
	if model == "kimi-k3-1-pro-261024" {
		// This model always reasons and does not use the thinking parameter.
		switchValue = nil
	}
	return VolcengineChatRequest{
		ReasoningEffort: effort,
		Model:           request.Model, Messages: request.Messages,
		Stream: request.Stream, StreamOptions: request.StreamOptions,
		Temperature: request.Temperature, TopP: request.TopP,
		MaxTokens: request.MaxTokens, MaxCompletionTokens: request.MaxCompletionTokens,
		Stop: request.Stop, Seed: request.Seed, Tools: request.Tools,
		ToolChoice: request.ToolChoice, ParallelToolCalls: request.ParallelToolCalls,
		ResponseFormat: request.ResponseFormat,
		Thinking:       switchValue,
	}, nil
}

// These are capability/parameter contracts, not a model inventory. Enumeration
// always comes from the upstream. Exact IDs prevent a future name from silently
// inheriting effort semantics. Source: Ark deep-thinking reference, 2026-11-02.
var VolcengineEffortLevels = []string{"low", "medium", "high", "xhigh"}

func VolcengineEffortSupported(effort string) bool {
	return effort == "" || effort == "none" || slices.Contains(VolcengineEffortLevels, effort)
}

func VolcengineReasonsUnasked(model string) bool {
	return model == "glm-5-3-flash-260828" || model == "kimi-k3-1-pro-261024"
}

func VolcengineReasoningSupported(model, effort string) bool {
	if !VolcengineEffortSupported(effort) {
		return false
	}
	if effort == "" {
		return true
	}
	if effort == "none" {
		return !VolcengineReasonsUnasked(model)
	}
	switch model {
	case "doubao-seed-evolving", "doubao-seed-2-1-pro-260915", "doubao-seed-2-1-lite-260915",
		"doubao-seed-2-1-pro-260628", "doubao-seed-2-1-turbo-260628", "doubao-seed-2-0-lite-260428", "doubao-seed-2-0-mini-260428",
		"doubao-seed-2-0-pro-260215", "doubao-seed-2-0-lite-260215", "doubao-seed-2-0-mini-260215", "doubao-seed-2-0-code-preview-260215", "doubao-seed-character-260628":
		return effort != "xhigh"
	case "deepseek-v4-pro-260425":
		return true
	case "deepseek-v4-pro-ga-260813", "deepseek-v4-flash-ga-260731", "deepseek-v4-1-flash-260910", "deepseek-v4-1-flash-261017", "deepseek-v4-1-flash-economy-261017", "glm-5-3-flash-260828", "kimi-k3-1-pro-261024":
		return effort == "low" || effort == "high"
	case "glm-5-2-260617":
		return effort == "high"
	default:
		return false
	}
}

func VolcengineTargetUnsupportedGenerateFields(model string, request semantic.GenerateRequest) []string {
	var fields []string
	if !VolcengineReasoningSupported(model, request.ReasoningEffort) {
		fields = append(fields, "reasoning_effort")
	}
	if request.VisibleOutputTokenLimit != nil && !volcengineAnswerLimitSupported(model, request.ReasoningEffort) {
		fields = append(fields, "max_tokens")
	}
	return fields
}

func volcengineAnswerLimitSupported(model, effort string) bool {
	if effort == "" || effort == "none" {
		return true
	}
	return !slices.Contains([]string{"deepseek-v4-1-flash-261017", "deepseek-v4-1-flash-260910", "deepseek-v4-1-flash-economy-261017"}, model)
}
