package compatibility

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

// BigModelEffortLevels is the depth ladder exposed by the current GLM thinking
// contract. Portable callers cannot name max because the OpenAI northbound
// ladder stops at xhigh; the exact per-model subset is enforced below.
var BigModelEffortLevels = []string{"low", "high", "max"}

type BigModelThinking struct {
	Type string `json:"type"`
}

// BigModelChatRequest is the documented common subset of the mainland
// BigModel and international Z.AI Chat Completions requests. It is intentionally
// not embedded from openaiapi.ChatCompletionRequest: doing that would put
// OpenAI-only members on the wire whenever that type grows.
type BigModelChatRequest struct {
	Model           string              `json:"model"`
	Messages        []openaiapi.Message `json:"messages"`
	Stream          bool                `json:"stream,omitempty"`
	Thinking        *BigModelThinking   `json:"thinking,omitempty"`
	ReasoningEffort string              `json:"reasoning_effort,omitempty"`
	Temperature     *float64            `json:"temperature,omitempty"`
	TopP            *float64            `json:"top_p,omitempty"`
	MaxTokens       *int64              `json:"max_tokens,omitempty"`
	Tools           []openaiapi.Tool    `json:"tools,omitempty"`
	ToolChoice      json.RawMessage     `json:"tool_choice,omitempty"`
	Stop            json.RawMessage     `json:"stop,omitempty"`
	ResponseFormat  json.RawMessage     `json:"response_format,omitempty"`
	RequestID       string              `json:"request_id,omitempty"`
	UserID          string              `json:"user_id,omitempty"`
}

type bigModelReasoningMode int

const (
	bigModelNoReasoning bigModelReasoningMode = iota
	bigModelOptionalReasoning
	bigModelAlwaysReasoning
)

func bigModelReasoningFor(model string) bigModelReasoningMode {
	switch model {
	case "glm-5.3", "glm-5.3-flash":
		return bigModelAlwaysReasoning
	case "glm-5.2":
		return bigModelOptionalReasoning
	default:
		return bigModelNoReasoning
	}
}

func bigModelReasoningSupported(model, effort string) bool {
	if effort == "" {
		return true
	}
	switch bigModelReasoningFor(model) {
	case bigModelAlwaysReasoning:
		return effort == "low" || effort == "high"
	case bigModelOptionalReasoning:
		// GLM-5.2 maps low/medium to high, xhigh to max, and minimal to
		// disabled. Halro does not silently round a caller's requested rung.
		return effort == "none" || effort == "high"
	default:
		return effort == "none"
	}
}

func bigModelThinkingWillBeOn(model, effort string) bool {
	if bigModelReasoningFor(model) == bigModelAlwaysReasoning {
		return true
	}
	return effort != "" && effort != "none"
}

// BigModelTargetUnsupportedGenerateFields contains constraints that require the
// invocation target's exact model identifier. Profile-only field rules cannot
// distinguish GLM-5.2's optional thinking from GLM-5.3's always-on contract.
func BigModelTargetUnsupportedGenerateFields(model string, request semantic.GenerateRequest) []string {
	var fields []string
	if !bigModelReasoningSupported(model, request.ReasoningEffort) {
		fields = append(fields, "reasoning_effort")
	}
	if request.VisibleOutputTokenLimit != nil &&
		(bigModelThinkingWillBeOn(model, request.ReasoningEffort) || request.CompletionTokenLimit != nil) {
		fields = append(fields, "max_tokens")
	}
	return fields
}

// RenderBigModelChatRequest converts the OpenAI-shaped request into the exact
// subset both regional APIs document. Every unsupported member is refused or
// deliberately omitted only when omission is semantically identical.
func RenderBigModelChatRequest(request openaiapi.ChatCompletionRequest, requestID string) (BigModelChatRequest, error) {
	if request.Temperature != nil && (*request.Temperature < 0 || *request.Temperature > 1) {
		return BigModelChatRequest{}, errors.New("BigModel temperature must be between 0 and 1")
	}
	if request.TopP != nil && (*request.TopP < 0.01 || *request.TopP > 1) {
		return BigModelChatRequest{}, errors.New("BigModel top_p must be between 0.01 and 1")
	}
	if request.N != nil && *request.N != 1 {
		return BigModelChatRequest{}, errors.New("BigModel Chat Completions accepts only n=1")
	}
	if request.Seed != nil {
		return BigModelChatRequest{}, errors.New("BigModel Chat Completions does not accept seed")
	}
	if request.ParallelToolCalls != nil && !*request.ParallelToolCalls {
		return BigModelChatRequest{}, errors.New("BigModel Chat Completions cannot disable parallel tool calls")
	}
	if len(request.Tools) > 128 {
		return BigModelChatRequest{}, errors.New("BigModel Chat Completions accepts at most 128 function tools")
	}
	if request.User != "" {
		length := utf8.RuneCountInString(request.User)
		if length < 6 || length > 128 {
			return BigModelChatRequest{}, errors.New("BigModel user_id must contain 6 to 128 characters")
		}
	}
	messages, err := renderBigModelMessages(request.Messages)
	if err != nil {
		return BigModelChatRequest{}, err
	}
	responseFormat, err := renderBigModelResponseFormat(request.ResponseFormat)
	if err != nil {
		return BigModelChatRequest{}, err
	}
	stop, err := renderBigModelStop(request.Stop)
	if err != nil {
		return BigModelChatRequest{}, err
	}
	toolChoice, err := renderBigModelToolChoice(request.ToolChoice)
	if err != nil {
		return BigModelChatRequest{}, err
	}
	if !bigModelReasoningSupported(request.Model, request.ReasoningEffort) {
		return BigModelChatRequest{}, fmt.Errorf("BigModel model %q cannot represent reasoning effort %q", request.Model, request.ReasoningEffort)
	}
	limit := request.MaxCompletionTokens
	if request.MaxTokens != nil {
		switch {
		case request.MaxCompletionTokens != nil:
			return BigModelChatRequest{}, errors.New("BigModel has one output limit and the request carries two")
		case bigModelThinkingWillBeOn(request.Model, request.ReasoningEffort):
			return BigModelChatRequest{}, errors.New("BigModel max_tokens cannot preserve an answer-only limit while this model reasons")
		default:
			limit = request.MaxTokens
		}
	}
	if limit != nil && (*limit < 1 || *limit > 131_072) {
		return BigModelChatRequest{}, errors.New("BigModel max_tokens must be between 1 and 131072")
	}
	result := BigModelChatRequest{
		Model: request.Model, Messages: messages, Stream: request.Stream,
		Temperature: request.Temperature, TopP: request.TopP, MaxTokens: limit,
		Tools: request.Tools, ToolChoice: toolChoice, Stop: stop,
		ResponseFormat: responseFormat, RequestID: requestID, UserID: request.User,
	}
	applyBigModelReasoning(&result, request.Model, request.ReasoningEffort)
	return result, nil
}

func applyBigModelReasoning(result *BigModelChatRequest, model, effort string) {
	switch bigModelReasoningFor(model) {
	case bigModelAlwaysReasoning:
		if effort != "" {
			result.Thinking = &BigModelThinking{Type: "enabled"}
			result.ReasoningEffort = effort
		}
	case bigModelOptionalReasoning:
		if effort == "high" {
			result.Thinking = &BigModelThinking{Type: "enabled"}
			result.ReasoningEffort = effort
		} else {
			result.Thinking = &BigModelThinking{Type: "disabled"}
		}
	}
}

func renderBigModelMessages(messages []openaiapi.Message) ([]openaiapi.Message, error) {
	result := make([]openaiapi.Message, len(messages))
	for index, message := range messages {
		if message.Role == "developer" {
			return nil, errors.New("BigModel Chat Completions does not accept developer messages")
		}
		if message.Name != "" {
			return nil, errors.New("BigModel Chat Completions does not accept message names")
		}
		result[index] = message
		content := bytes.TrimSpace(message.Content)
		if len(content) == 0 || content[0] != '[' {
			continue
		}
		var parts []map[string]json.RawMessage
		if err := json.Unmarshal(content, &parts); err != nil {
			return nil, fmt.Errorf("BigModel cannot read message content: %w", err)
		}
		changed := false
		for _, part := range parts {
			rawImage, ok := part["image_url"]
			if !ok {
				continue
			}
			var image map[string]json.RawMessage
			if err := json.Unmarshal(rawImage, &image); err != nil {
				return nil, fmt.Errorf("BigModel cannot read image_url content: %w", err)
			}
			raw, ok := image["detail"]
			if !ok {
				continue
			}
			var detail string
			if json.Unmarshal(raw, &detail) != nil || (detail != "" && detail != "auto") {
				return nil, errors.New("BigModel image content cannot represent a non-auto detail")
			}
			delete(image, "detail")
			encodedImage, err := json.Marshal(image)
			if err != nil {
				return nil, err
			}
			part["image_url"] = encodedImage
			changed = true
		}
		if changed {
			content, err := json.Marshal(parts)
			if err != nil {
				return nil, err
			}
			result[index].Content = content
		}
	}
	return result, nil
}

func renderBigModelResponseFormat(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var format struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &format); err != nil {
		return nil, fmt.Errorf("BigModel cannot read response_format: %w", err)
	}
	switch format.Type {
	case "text":
		return nil, nil
	case "json_object":
		return json.RawMessage(`{"type":"json_object"}`), nil
	default:
		return nil, fmt.Errorf("BigModel does not accept response_format type %q", format.Type)
	}
}

func renderBigModelToolChoice(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var mode string
	if err := json.Unmarshal(raw, &mode); err != nil || mode != "auto" {
		return nil, errors.New("BigModel tool_choice accepts only auto")
	}
	return json.RawMessage(`"auto"`), nil
}

func renderBigModelStop(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var single string
	if json.Unmarshal(raw, &single) == nil {
		encoded, err := json.Marshal([]string{single})
		return encoded, err
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, fmt.Errorf("BigModel cannot read stop: %w", err)
	}
	if len(many) > 4 {
		return nil, errors.New("BigModel accepts at most four stop sequences")
	}
	return raw, nil
}
