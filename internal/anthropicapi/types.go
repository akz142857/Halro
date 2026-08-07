package anthropicapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	VersionHeader     = "anthropic-version"
	BetaHeader        = "anthropic-beta"
	SupportedVersion  = "2023-06-01"
	RouteModeHeader   = "Heimdall-Route-Mode"
	MaxRequestBytes   = 4 << 20
	MaxContentBlocks  = 4096
	MaxMessages       = 100000
	MaxToolInputBytes = 1 << 20
)

type ExecutionMode string

const (
	ModePortable ExecutionMode = "portable"
	ModeNative   ExecutionMode = "native"
)

func ParseExecutionMode(value string) (ExecutionMode, error) {
	switch ExecutionMode(strings.ToLower(strings.TrimSpace(value))) {
	case "", ModePortable:
		return ModePortable, nil
	case ModeNative:
		return ModeNative, nil
	default:
		return "", errors.New("Heimdall-Route-Mode must be portable or native")
	}
}

type MessageRequest struct {
	Model         string          `json:"model"`
	MaxTokens     int64           `json:"max_tokens"`
	Messages      []MessageParam  `json:"messages"`
	System        json.RawMessage `json:"system,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	TopK          *int64          `json:"top_k,omitempty"`
	Tools         []Tool          `json:"tools,omitempty"`
	ToolChoice    *ToolChoice     `json:"tool_choice,omitempty"`
	Thinking      json.RawMessage `json:"thinking,omitempty"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
	ServiceTier   string          `json:"service_tier,omitempty"`
	Raw           json.RawMessage `json:"-"`
}

type MessageParam struct {
	Role    string        `json:"role"`
	Content ContentBlocks `json:"content"`
}

type ContentBlocks []ContentBlock

func (blocks *ContentBlocks) UnmarshalJSON(data []byte) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return errors.New("message content is required")
	}
	var text string
	if json.Unmarshal(data, &text) == nil {
		*blocks = []ContentBlock{{Type: "text", Text: text, Raw: append(json.RawMessage(nil), data...)}}
		return nil
	}
	var rawBlocks []json.RawMessage
	if err := json.Unmarshal(data, &rawBlocks); err != nil {
		return errors.New("message content must be a string or array")
	}
	if len(rawBlocks) > MaxContentBlocks {
		return errors.New("message contains too many content blocks")
	}
	result := make([]ContentBlock, 0, len(rawBlocks))
	for _, raw := range rawBlocks {
		block, err := decodeContentBlock(raw)
		if err != nil {
			return err
		}
		result = append(result, block)
	}
	*blocks = result
	return nil
}

func (blocks ContentBlocks) MarshalJSON() ([]byte, error) {
	values := make([]json.RawMessage, 0, len(blocks))
	for _, block := range blocks {
		if len(block.Raw) > 0 {
			values = append(values, append(json.RawMessage(nil), block.Raw...))
			continue
		}
		encoded, err := json.Marshal(block.wireValue())
		if err != nil {
			return nil, err
		}
		values = append(values, encoded)
	}
	return json.Marshal(values)
}

type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Data      string          `json:"data,omitempty"`
	Source    json.RawMessage `json:"source,omitempty"`
	Raw       json.RawMessage `json:"-"`
}

func decodeContentBlock(raw json.RawMessage) (ContentBlock, error) {
	var block ContentBlock
	if err := json.Unmarshal(raw, &block); err != nil {
		return ContentBlock{}, errors.New("invalid content block")
	}
	block.Raw = append(json.RawMessage(nil), raw...)
	if strings.TrimSpace(block.Type) == "" {
		return ContentBlock{}, errors.New("content block type is required")
	}
	return block, nil
}

func (block ContentBlock) wireValue() any {
	switch block.Type {
	case "text":
		return struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{block.Type, block.Text}
	case "tool_use":
		return struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}{block.Type, block.ID, block.Name, block.Input}
	case "tool_result":
		return struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content,omitempty"`
			IsError   bool            `json:"is_error,omitempty"`
		}{block.Type, block.ToolUseID, block.Content, block.IsError}
	case "thinking":
		return struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
		}{block.Type, block.Thinking, block.Signature}
	case "redacted_thinking":
		return struct {
			Type string `json:"type"`
			Data string `json:"data"`
		}{block.Type, block.Data}
	default:
		return map[string]any{"type": block.Type}
	}
}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
	Type        string          `json:"type,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

type ToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
}

func DecodeMessageRequest(reader io.Reader) (MessageRequest, error) {
	payload, err := io.ReadAll(io.LimitReader(reader, MaxRequestBytes+1))
	if err != nil {
		return MessageRequest{}, err
	}
	if len(payload) > MaxRequestBytes {
		return MessageRequest{}, errors.New("request body exceeds limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var request MessageRequest
	if err := decoder.Decode(&request); err != nil {
		return MessageRequest{}, err
	}
	if err := ensureEOF(decoder); err != nil {
		return MessageRequest{}, err
	}
	request.Raw = append(json.RawMessage(nil), payload...)
	if err := request.Validate(); err != nil {
		return MessageRequest{}, err
	}
	return request, nil
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body contains multiple JSON values")
		}
		return err
	}
	return nil
}

func (request MessageRequest) Validate() error {
	if strings.TrimSpace(request.Model) == "" || request.MaxTokens < 0 || len(request.Messages) == 0 || len(request.Messages) > MaxMessages {
		return errors.New("model, max_tokens, and messages are required")
	}
	if request.Temperature != nil && (*request.Temperature < 0 || *request.Temperature > 1) {
		return errors.New("temperature must be between 0 and 1")
	}
	if request.TopP != nil && (*request.TopP < 0 || *request.TopP > 1) {
		return errors.New("top_p must be between 0 and 1")
	}
	for _, message := range request.Messages {
		if message.Role != "user" && message.Role != "assistant" {
			return errors.New("messages may only use user or assistant roles")
		}
		if len(message.Content) == 0 {
			return errors.New("message content must not be empty")
		}
		for _, block := range message.Content {
			if err := validateBlock(message.Role, block); err != nil {
				return err
			}
		}
	}
	for _, tool := range request.Tools {
		if tool.Type != "" && tool.Type != "custom" {
			return errors.New("hosted and server tools are not supported")
		}
		if tool.Name == "" || len(tool.InputSchema) == 0 || !json.Valid(tool.InputSchema) || bytes.TrimSpace(tool.InputSchema)[0] != '{' {
			return errors.New("invalid client tool")
		}
	}
	if request.ToolChoice != nil {
		switch request.ToolChoice.Type {
		case "auto", "any":
			if request.ToolChoice.Name != "" {
				return errors.New("tool_choice name is only valid for type tool")
			}
		case "tool":
			if request.ToolChoice.Name == "" {
				return errors.New("named tool_choice requires name")
			}
		case "none":
			if request.ToolChoice.Name != "" || request.ToolChoice.DisableParallelToolUse {
				return errors.New("none tool_choice has unrelated fields")
			}
		default:
			return errors.New("invalid tool_choice type")
		}
	}
	if len(request.Thinking) > 0 {
		var thinking struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(request.Thinking, &thinking); err != nil || strings.TrimSpace(thinking.Type) == "" {
			return errors.New("invalid thinking configuration")
		}
		if thinking.Type == "enabled" && request.ToolChoice != nil && (request.ToolChoice.Type == "any" || request.ToolChoice.Type == "tool") {
			return errors.New("enabled thinking is incompatible with forced tool_choice")
		}
	}
	return nil
}

func validateBlock(role string, block ContentBlock) error {
	switch block.Type {
	case "text":
		return nil
	case "image":
		if role != "user" || len(block.Source) == 0 || !json.Valid(block.Source) {
			return errors.New("invalid image block")
		}
	case "tool_use":
		if role != "assistant" || block.ID == "" || block.Name == "" || len(block.Input) == 0 || len(block.Input) > MaxToolInputBytes || !json.Valid(block.Input) {
			return errors.New("invalid tool_use block")
		}
	case "tool_result":
		if role != "user" || block.ToolUseID == "" || (len(block.Content) > 0 && !json.Valid(block.Content)) {
			return errors.New("invalid tool_result block")
		}
	case "thinking":
		if role != "assistant" || block.Signature == "" {
			return errors.New("thinking blocks require an unchanged signature")
		}
	case "redacted_thinking":
		if role != "assistant" || block.Data == "" {
			return errors.New("redacted thinking blocks require opaque data")
		}
	default:
		return fmt.Errorf("unsupported content block type %q", block.Type)
	}
	return nil
}

type Usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens,omitempty"`
	ThinkingTokens           int64 `json:"thinking_tokens,omitempty"`
}

// PromptTokens is every prompt token the request consumed. Anthropic reports
// input_tokens net of both cache tiers, so recovering the full prompt span means
// adding them back — reading input_tokens alone under-counts a cached request by
// whatever fraction the cache served, which on an agent workload is most of it.
func (u Usage) PromptTokens() int64 {
	return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}

type Message struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Role         string          `json:"role"`
	Content      ContentBlocks   `json:"content"`
	Model        string          `json:"model"`
	StopReason   *string         `json:"stop_reason"`
	StopSequence *string         `json:"stop_sequence"`
	Usage        Usage           `json:"usage"`
	Raw          json.RawMessage `json:"-"`
}

type ErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}
type ErrorResponse struct {
	Type      string      `json:"type"`
	Error     ErrorDetail `json:"error"`
	RequestID string      `json:"request_id,omitempty"`
}

func DecodeMessage(payload []byte) (Message, error) {
	if len(payload) == 0 || len(payload) > MaxRequestBytes {
		return Message{}, errors.New("Anthropic message response has invalid size")
	}
	var message Message
	if err := json.Unmarshal(payload, &message); err != nil {
		return Message{}, err
	}
	if message.ID == "" || message.Type != "message" || message.Role != "assistant" || message.Model == "" {
		return Message{}, errors.New("Anthropic message response is missing required fields")
	}
	for _, block := range message.Content {
		if err := validateBlock("assistant", block); err != nil {
			return Message{}, err
		}
	}
	if message.Usage.InputTokens < 0 || message.Usage.OutputTokens < 0 {
		return Message{}, errors.New("Anthropic usage is invalid")
	}
	message.Raw = append(json.RawMessage(nil), payload...)
	return message, nil
}

type StreamEvent struct {
	Type         string       `json:"type"`
	Index        *int         `json:"index,omitempty"`
	Message      *Message     `json:"message,omitempty"`
	ContentBlock any          `json:"content_block,omitempty"`
	Delta        any          `json:"delta,omitempty"`
	Usage        *Usage       `json:"usage,omitempty"`
	Error        *ErrorDetail `json:"error,omitempty"`
}

type RawStreamEvent struct {
	Type string
	Data json.RawMessage
}
