package openaiapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
)

// ReasoningEffortLevels is the ladder this wire format accepts. It is exported
// because it bounds every portable request: a request that reaches any provider
// through the OpenAI intermediate representation cannot carry an effort outside
// it, whatever the destination provider supports on its own surface.
var ReasoningEffortLevels = []string{"none", "minimal", "low", "medium", "high", "xhigh"}

type ChatCompletionRequest struct {
	Model               string          `json:"model,omitempty"`
	Messages            []Message       `json:"messages"`
	Stream              bool            `json:"stream,omitempty"`
	StreamOptions       *StreamOptions  `json:"stream_options,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	MaxTokens           *int64          `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int64          `json:"max_completion_tokens,omitempty"`
	N                   *int            `json:"n,omitempty"`
	Stop                json.RawMessage `json:"stop,omitempty"`
	Seed                *int64          `json:"seed,omitempty"`
	Tools               []Tool          `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls,omitempty"`
	ResponseFormat      json.RawMessage `json:"response_format,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	User                string          `json:"user,omitempty"`
}

type Message struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	Name             string          `json:"name,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	ToolCalls        []ToolCall      `json:"tool_calls,omitempty"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type ToolCall struct {
	Index    *int             `json:"index,omitempty"`
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

func (r ChatCompletionRequest) Validate() error {
	var problems []error
	if r.Model == "" {
		problems = append(problems, errors.New("model is required"))
	}
	if len(r.Messages) == 0 {
		problems = append(problems, errors.New("messages must not be empty"))
	}
	for index, message := range r.Messages {
		switch message.Role {
		case "system", "developer", "user", "assistant", "tool":
		default:
			problems = append(problems, fmt.Errorf("messages[%d].role is invalid", index))
		}
		if len(message.Content) > 0 && !json.Valid(message.Content) {
			problems = append(problems, fmt.Errorf("messages[%d].content is invalid JSON", index))
		}
	}
	if r.MaxTokens != nil && *r.MaxTokens < 0 {
		problems = append(problems, errors.New("max_tokens cannot be negative"))
	}
	if r.MaxCompletionTokens != nil && *r.MaxCompletionTokens < 0 {
		problems = append(problems, errors.New("max_completion_tokens cannot be negative"))
	}
	if r.N != nil && (*r.N < 1 || *r.N > 16) {
		problems = append(problems, errors.New("n must be between 1 and 16"))
	}
	if r.Temperature != nil && (*r.Temperature < 0 || *r.Temperature > 2) {
		problems = append(problems, errors.New("temperature must be between 0 and 2"))
	}
	if r.TopP != nil && (*r.TopP < 0 || *r.TopP > 1) {
		problems = append(problems, errors.New("top_p must be between 0 and 1"))
	}
	if r.ReasoningEffort != "" && !slices.Contains(ReasoningEffortLevels, r.ReasoningEffort) {
		problems = append(problems, errors.New("reasoning_effort is invalid"))
	}
	if len(r.ResponseFormat) > 0 {
		var format struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(r.ResponseFormat, &format); err != nil {
			problems = append(problems, errors.New("response_format must be a JSON object"))
		} else {
			switch format.Type {
			case "text", "json_object", "json_schema":
			default:
				problems = append(problems, errors.New("response_format.type is invalid"))
			}
		}
	}
	return errors.Join(problems...)
}

func (r ChatCompletionRequest) EstimatedInputBytes() int64 {
	var total int64
	for _, message := range r.Messages {
		total += int64(len(message.Role) + len(message.Content) + len(message.ReasoningContent) + len(message.Name) + len(message.ToolCallID))
		for _, call := range message.ToolCalls {
			total += int64(len(call.ID) + len(call.Type) + len(call.Function.Name) + len(call.Function.Arguments))
		}
	}
	for _, tool := range r.Tools {
		total += int64(len(tool.Type) + len(tool.Function.Name) + len(tool.Function.Description) + len(tool.Function.Parameters))
	}
	return total
}

func DecodeChatCompletionRequest(decoder *json.Decoder) (ChatCompletionRequest, error) {
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var request ChatCompletionRequest
	if err := decoder.Decode(&request); err != nil {
		return ChatCompletionRequest{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return ChatCompletionRequest{}, errors.New("multiple JSON values are not allowed")
		}
		return ChatCompletionRequest{}, err
	}
	if err := request.Validate(); err != nil {
		return ChatCompletionRequest{}, err
	}
	return request, nil
}

func TextContent(value string) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

func DecodeTextContent(raw json.RawMessage) (string, bool) {
	var value string
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&value); err != nil {
		return "", false
	}
	return value, true
}

type ChatCompletionResponse struct {
	ID                      string   `json:"id"`
	Object                  string   `json:"object"`
	Created                 int64    `json:"created"`
	Model                   string   `json:"model"`
	Choices                 []Choice `json:"choices"`
	Usage                   *Usage   `json:"usage,omitempty"`
	SemanticMappingRevision uint64   `json:"-"`
	SemanticTranslation     string   `json:"-"`
}

type Choice struct {
	Index        int      `json:"index"`
	Message      *Message `json:"message,omitempty"`
	Delta        *Message `json:"delta,omitempty"`
	FinishReason *string  `json:"finish_reason"`
}

type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	// The details objects mirror OpenAI's own schema, so parsing an upstream
	// response fills them and rendering one back to a client stays wire-faithful.
	// Both are omitted when empty, which keeps today's three-field response
	// byte-identical for providers that report no breakdown.
	PromptTokensDetails     *PromptTokenDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *CompletionTokenDetails `json:"completion_tokens_details,omitempty"`
	// CacheWriteTokens carries the cache-creation tier that Anthropic and Bedrock
	// bill at a premium. OpenAI has no equivalent field, so it stays off the wire
	// rather than inventing a non-standard key in an OpenAI-shaped response; the
	// adapters that populate it construct this struct in process.
	CacheWriteTokens int64 `json:"-"`
}

type PromptTokenDetails struct {
	CachedTokens int64 `json:"cached_tokens,omitempty"`
	AudioTokens  int64 `json:"audio_tokens,omitempty"`
}

type CompletionTokenDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
	AudioTokens     int64 `json:"audio_tokens,omitempty"`
}

// CachedPromptTokens reports the cache-read tier without the nil dance.
func (u Usage) CachedPromptTokens() int64 {
	if u.PromptTokensDetails == nil {
		return 0
	}
	return u.PromptTokensDetails.CachedTokens
}

// ReasoningTokens reports the reasoning span of the completion, which providers
// bill at the output rate and therefore already count inside CompletionTokens.
func (u Usage) ReasoningTokens() int64 {
	if u.CompletionTokensDetails == nil {
		return 0
	}
	return u.CompletionTokensDetails.ReasoningTokens
}

// AudioTokens sums the audio spans reported on either side of the exchange.
func (u Usage) AudioTokens() int64 {
	var total int64
	if u.PromptTokensDetails != nil {
		total += u.PromptTokensDetails.AudioTokens
	}
	if u.CompletionTokensDetails != nil {
		total += u.CompletionTokensDetails.AudioTokens
	}
	return total
}

// SetCachedPromptTokens records the cache-read tier, allocating the details
// object only when there is something to report.
func (u *Usage) SetCachedPromptTokens(tokens int64) {
	if tokens == 0 {
		return
	}
	if u.PromptTokensDetails == nil {
		u.PromptTokensDetails = &PromptTokenDetails{}
	}
	u.PromptTokensDetails.CachedTokens = tokens
}

// SetReasoningTokens records the reasoning span of the completion.
func (u *Usage) SetReasoningTokens(tokens int64) {
	if tokens == 0 {
		return
	}
	if u.CompletionTokensDetails == nil {
		u.CompletionTokensDetails = &CompletionTokenDetails{}
	}
	u.CompletionTokensDetails.ReasoningTokens = tokens
}

type ErrorEnvelope struct {
	Error ErrorBody `json:"error"`
}

type ErrorBody struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}

type EmbeddingRequest struct {
	Model          string          `json:"model,omitempty"`
	Input          json.RawMessage `json:"input"`
	EncodingFormat string          `json:"encoding_format,omitempty"`
	Dimensions     *int64          `json:"dimensions,omitempty"`
	User           string          `json:"user,omitempty"`
}

func (r EmbeddingRequest) Validate() error {
	var problems []error
	if r.Model == "" {
		problems = append(problems, errors.New("model is required"))
	}
	if len(r.Input) == 0 || !json.Valid(r.Input) || string(r.Input) == "null" {
		problems = append(problems, errors.New("input must be valid, non-null JSON"))
	}
	if r.EncodingFormat != "" && r.EncodingFormat != "float" && r.EncodingFormat != "base64" {
		problems = append(problems, errors.New("encoding_format must be float or base64"))
	}
	if r.Dimensions != nil && *r.Dimensions <= 0 {
		problems = append(problems, errors.New("dimensions must be positive"))
	}
	return errors.Join(problems...)
}

func DecodeEmbeddingRequest(decoder *json.Decoder) (EmbeddingRequest, error) {
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var request EmbeddingRequest
	if err := decoder.Decode(&request); err != nil {
		return EmbeddingRequest{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return EmbeddingRequest{}, errors.New("multiple JSON values are not allowed")
		}
		return EmbeddingRequest{}, err
	}
	if err := request.Validate(); err != nil {
		return EmbeddingRequest{}, err
	}
	return request, nil
}

func (r EmbeddingRequest) EstimatedInputTokens() int64 {
	if len(r.Input) == 0 {
		return 1
	}
	return (int64(len(r.Input)) + 3) / 4
}

type EmbeddingResponse struct {
	Object string          `json:"object"`
	Data   []EmbeddingData `json:"data"`
	Model  string          `json:"model"`
	Usage  *Usage          `json:"usage,omitempty"`
}

type EmbeddingData struct {
	Object    string          `json:"object"`
	Embedding json.RawMessage `json:"embedding"`
	Index     int             `json:"index"`
}
