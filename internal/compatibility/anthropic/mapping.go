package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/akz142857/Heimdall/internal/anthropicapi"
	"github.com/akz142857/Heimdall/internal/compatibility"
	"github.com/akz142857/Heimdall/internal/semantic"
)

const MappingRevision uint64 = 1

func DecodePortable(request anthropicapi.MessageRequest) (semantic.GenerateRequest, error) {
	if len(request.Thinking) > 0 || len(request.Metadata) > 0 || request.ServiceTier != "" || request.TopK != nil {
		return semantic.GenerateRequest{}, errors.New("request contains Anthropic-native fields")
	}
	result := semantic.GenerateRequest{
		Operation: semantic.OperationGenerate,
		Source:    semantic.Source{ProfileID: string(compatibility.ProfileAnthropicMessages), ProfileRevision: 1},
		Mode:      semantic.ModePortable, RequestedModel: request.Model, Stream: request.Stream,
		Temperature: request.Temperature, TopP: request.TopP, Stop: append([]string(nil), request.StopSequences...),
		VisibleOutputTokenLimit: cloneInt64(&request.MaxTokens), IncludeUsage: request.Stream,
	}
	system, err := decodeSystem(request.System)
	if err != nil {
		return semantic.GenerateRequest{}, err
	}
	if len(system) > 0 {
		result.Messages = append(result.Messages, semantic.Message{Role: semantic.RoleSystem, Content: system})
	}
	for _, message := range request.Messages {
		mapped, err := decodeMessage(message)
		if err != nil {
			return semantic.GenerateRequest{}, err
		}
		result.Messages = append(result.Messages, mapped...)
	}
	for _, tool := range request.Tools {
		if tool.Strict != nil && *tool.Strict {
			return semantic.GenerateRequest{}, errors.New("strict Anthropic tools are not portable")
		}
		result.Tools = append(result.Tools, semantic.Tool{Name: tool.Name, Description: tool.Description, Schema: append(json.RawMessage(nil), tool.InputSchema...)})
	}
	if request.ToolChoice != nil {
		choice, parallel, err := compatibility.DecodeToolChoice(compatibility.ToolChoiceWire{
			Protocol: compatibility.ToolProtocolAnthropic, Mode: request.ToolChoice.Type,
			NamedTool: request.ToolChoice.Name, ParallelAllowed: !request.ToolChoice.DisableParallelToolUse,
		})
		if err != nil {
			return semantic.GenerateRequest{}, err
		}
		result.ToolChoice, result.ParallelTools = &choice, parallel
	}
	result.Requirements = result.DeriveRequirements()
	if err := result.Validate(); err != nil {
		return semantic.GenerateRequest{}, err
	}
	return result, nil
}

func decodeSystem(raw json.RawMessage) ([]semantic.Content, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []semantic.Content{{Kind: semantic.ContentText, Text: text}}, nil
	}
	var blocks anthropicapi.ContentBlocks
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, errors.New("system must be text or text blocks")
	}
	result := make([]semantic.Content, 0, len(blocks))
	for _, block := range blocks {
		if block.Type != "text" {
			return nil, errors.New("portable system supports text blocks only")
		}
		result = append(result, semantic.Content{Kind: semantic.ContentText, Text: block.Text})
	}
	return result, nil
}

func decodeMessage(message anthropicapi.MessageParam) ([]semantic.Message, error) {
	role := semantic.RoleUser
	if message.Role == "assistant" {
		role = semantic.RoleAssistant
	}
	current := semantic.Message{Role: role}
	result := make([]semantic.Message, 0, 2)
	flush := func() {
		if len(current.Content) > 0 {
			result = append(result, current)
			current = semantic.Message{Role: role}
		}
	}
	for _, block := range message.Content {
		switch block.Type {
		case "text":
			current.Content = append(current.Content, semantic.Content{Kind: semantic.ContentText, Text: block.Text})
		case "image":
			url, detail, err := decodeURLSource(block.Source)
			if err != nil {
				return nil, err
			}
			current.Content = append(current.Content, semantic.Content{Kind: semantic.ContentInputImage, URL: url, Detail: detail})
		case "tool_use":
			current.Content = append(current.Content, semantic.Content{Kind: semantic.ContentToolCall, CallID: block.ID, Name: block.Name, Arguments: string(block.Input)})
		case "tool_result":
			flush()
			text, err := decodeToolResult(block.Content)
			if err != nil {
				return nil, err
			}
			result = append(result, semantic.Message{Role: semantic.RoleTool, Content: []semantic.Content{{Kind: semantic.ContentToolResult, CallID: block.ToolUseID, Text: text}}})
		case "thinking", "redacted_thinking":
			return nil, errors.New("signed thinking blocks require native mode")
		default:
			return nil, fmt.Errorf("content block %q is not portable", block.Type)
		}
	}
	flush()
	if len(result) == 0 {
		return nil, errors.New("message has no portable content")
	}
	return result, nil
}

func decodeURLSource(raw json.RawMessage) (string, string, error) {
	var source struct{ Type, URL, Detail string }
	if err := json.Unmarshal(raw, &source); err != nil || source.Type != "url" || strings.TrimSpace(source.URL) == "" {
		return "", "", errors.New("portable image input requires an Anthropic URL source")
	}
	return source.URL, source.Detail, nil
}

func decodeToolResult(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	var blocks anthropicapi.ContentBlocks
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", errors.New("portable tool_result must be text")
	}
	var joined strings.Builder
	for _, block := range blocks {
		if block.Type != "text" {
			return "", errors.New("portable tool_result supports text blocks only")
		}
		joined.WriteString(block.Text)
	}
	return joined.String(), nil
}

func RenderResult(result semantic.GenerateResult, publicModel string) (anthropicapi.Message, error) {
	if err := result.Validate(); err != nil {
		return anthropicapi.Message{}, err
	}
	if len(result.Choices) != 1 {
		return anthropicapi.Message{}, errors.New("Anthropic Messages requires exactly one output")
	}
	output := result.Choices[0]
	content := make(anthropicapi.ContentBlocks, 0, len(output.Message.Content))
	for _, part := range output.Message.Content {
		switch part.Kind {
		case semantic.ContentText:
			content = append(content, anthropicapi.ContentBlock{Type: "text", Text: part.Text})
		case semantic.ContentToolCall:
			input := json.RawMessage(part.Arguments)
			if !json.Valid(input) {
				return anthropicapi.Message{}, errors.New("provider returned invalid tool input")
			}
			content = append(content, anthropicapi.ContentBlock{Type: "tool_use", ID: part.CallID, Name: part.Name, Input: input})
		default:
			return anthropicapi.Message{}, errors.New("provider result contains non-portable content")
		}
	}
	stop := renderStopReason(output.Termination)
	message := anthropicapi.Message{ID: anthropicID(result.ID), Type: "message", Role: "assistant", Content: content, Model: publicModel, StopReason: &stop}
	if result.Usage != nil {
		message.Usage = renderUsage(*result.Usage)
	}
	return message, nil
}

func renderStopReason(reason string) string {
	switch reason {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}

func renderUsage(usage semantic.Usage) anthropicapi.Usage {
	return anthropicapi.Usage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens}
}

func anthropicID(id string) string {
	if strings.HasPrefix(id, "msg_") {
		return id
	}
	return "msg_" + strings.TrimPrefix(id, "chatcmpl-")
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func RawEqual(left, right json.RawMessage) bool { return bytes.Equal(left, right) }

func RenderPortableRequest(request semantic.GenerateRequest, providerModel string) (anthropicapi.MessageRequest, error) {
	if err := request.Validate(); err != nil {
		return anthropicapi.MessageRequest{}, err
	}
	result := anthropicapi.MessageRequest{Model: providerModel, Stream: request.Stream, StopSequences: append([]string(nil), request.Stop...), Temperature: request.Temperature, TopP: request.TopP}
	if request.VisibleOutputTokenLimit != nil {
		result.MaxTokens = *request.VisibleOutputTokenLimit
	}
	if result.MaxTokens == 0 {
		result.MaxTokens = 1024
	}
	for _, message := range request.Messages {
		if message.Role == semantic.RoleSystem || message.Role == semantic.RoleDeveloper {
			if len(result.Messages) > 0 {
				return anthropicapi.MessageRequest{}, errors.New("system instructions must precede messages")
			}
			var text strings.Builder
			for _, part := range message.Content {
				if part.Kind != semantic.ContentText {
					return anthropicapi.MessageRequest{}, errors.New("Anthropic system supports portable text only")
				}
				text.WriteString(part.Text)
			}
			encoded, _ := json.Marshal(text.String())
			if len(result.System) == 0 {
				result.System = encoded
			} else {
				var existing string
				_ = json.Unmarshal(result.System, &existing)
				encoded, _ = json.Marshal(existing + "\n" + text.String())
				result.System = encoded
			}
			continue
		}
		mapped, err := renderMessage(message)
		if err != nil {
			return anthropicapi.MessageRequest{}, err
		}
		result.Messages = append(result.Messages, mapped)
	}
	for _, tool := range request.Tools {
		result.Tools = append(result.Tools, anthropicapi.Tool{Name: tool.Name, Description: tool.Description, InputSchema: append(json.RawMessage(nil), tool.Schema...)})
	}
	if request.ToolChoice != nil {
		parallel := true
		if request.ParallelTools != nil {
			parallel = *request.ParallelTools
		}
		wire, err := compatibility.RenderToolChoice(*request.ToolChoice, parallel, compatibility.ToolProtocolAnthropic)
		if err != nil {
			return anthropicapi.MessageRequest{}, err
		}
		result.ToolChoice = &anthropicapi.ToolChoice{Type: wire.Mode, Name: wire.NamedTool, DisableParallelToolUse: !wire.ParallelAllowed}
	}
	return result, result.Validate()
}

func renderMessage(message semantic.Message) (anthropicapi.MessageParam, error) {
	result := anthropicapi.MessageParam{Role: string(message.Role)}
	if message.Role == semantic.RoleTool {
		result.Role = "user"
	}
	for _, part := range message.Content {
		switch part.Kind {
		case semantic.ContentText:
			result.Content = append(result.Content, anthropicapi.ContentBlock{Type: "text", Text: part.Text})
		case semantic.ContentInputImage:
			source, _ := json.Marshal(map[string]any{"type": "url", "url": part.URL, "detail": part.Detail})
			result.Content = append(result.Content, anthropicapi.ContentBlock{Type: "image", Source: source})
		case semantic.ContentToolCall:
			input := json.RawMessage(part.Arguments)
			if !json.Valid(input) {
				return result, errors.New("tool call arguments are invalid JSON")
			}
			result.Content = append(result.Content, anthropicapi.ContentBlock{Type: "tool_use", ID: part.CallID, Name: part.Name, Input: input})
		case semantic.ContentToolResult:
			content, _ := json.Marshal(part.Text)
			result.Content = append(result.Content, anthropicapi.ContentBlock{Type: "tool_result", ToolUseID: part.CallID, Content: content})
		default:
			return result, errors.New("content is not portable to Anthropic Messages")
		}
	}
	return result, nil
}

func DecodeResult(message anthropicapi.Message) (semantic.GenerateResult, error) {
	if message.ID == "" || message.Model == "" || message.Role != "assistant" {
		return semantic.GenerateResult{}, errors.New("Anthropic response identity is invalid")
	}
	canonical := semantic.Message{Role: semantic.RoleAssistant}
	for _, block := range message.Content {
		switch block.Type {
		case "text":
			canonical.Content = append(canonical.Content, semantic.Content{Kind: semantic.ContentText, Text: block.Text})
		case "tool_use":
			if !json.Valid(block.Input) {
				return semantic.GenerateResult{}, errors.New("Anthropic tool input is invalid")
			}
			canonical.Content = append(canonical.Content, semantic.Content{Kind: semantic.ContentToolCall, CallID: block.ID, Name: block.Name, Arguments: string(block.Input)})
		case "thinking", "redacted_thinking":
			return semantic.GenerateResult{}, errors.New("signed thinking response requires native mode")
		default:
			return semantic.GenerateResult{}, errors.New("Anthropic response contains non-portable content")
		}
	}
	termination := decodeStopReason(message.StopReason)
	usage := &semantic.Usage{InputTokens: message.Usage.InputTokens, OutputTokens: message.Usage.OutputTokens, ReasoningTokens: message.Usage.ThinkingTokens, TotalTokens: message.Usage.InputTokens + message.Usage.OutputTokens, Source: semantic.UsageProviderReported}
	result := semantic.GenerateResult{ID: message.ID, Model: message.Model, Choices: []semantic.GenerateChoice{{Index: 0, Message: canonical, Termination: termination, NativeTermination: stringValue(message.StopReason)}}, Usage: usage, Translation: semantic.TranslationNone, MappingRevision: MappingRevision}
	return result, result.Validate()
}

func decodeStopReason(reason *string) string {
	if reason == nil {
		return ""
	}
	switch *reason {
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "tool_use", "pause_turn":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
