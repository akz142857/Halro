package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/akz142857/Halro/internal/compatibility"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

func DecodeResponseGenerate(request openaiapi.ResponseRequest) (semantic.GenerateRequest, error) {
	if err := request.Validate(); err != nil {
		return semantic.GenerateRequest{}, err
	}
	result := semantic.GenerateRequest{
		Operation: semantic.OperationGenerate,
		Source:    compatibility.SourceOf(compatibility.ProfileOpenAIResponses),
		Mode:      semantic.ModePortable, RequestedModel: request.Model,
		Temperature: cloneFloat(request.Temperature), TopP: cloneFloat(request.TopP),
		CompletionTokenLimit: cloneInt64(request.MaxOutputTokens),
		ParallelTools:        cloneBool(request.ParallelToolCalls), Stream: request.Stream,
		IncludeUsage: request.Stream, EndUserRef: request.User,
	}
	if request.Reasoning != nil {
		result.ReasoningEffort = request.Reasoning.Effort
		result.ReasoningSummary = request.Reasoning.Summary
	}
	if request.Instructions != "" {
		result.Messages = append(result.Messages, semantic.Message{Role: semantic.RoleDeveloper, Content: []semantic.Content{{Kind: semantic.ContentText, Text: request.Instructions}}})
	}
	messages, err := decodeResponseInput(request.Input)
	if err != nil {
		return semantic.GenerateRequest{}, err
	}
	result.Messages = append(result.Messages, messages...)
	for _, tool := range request.Tools {
		if tool.Type == openaiapi.ProviderExecutedToolWebSearch {
			result.Tools = append(result.Tools, semantic.Tool{
				Name: semantic.ProviderToolWebSearch, Execution: semantic.ToolExecutionProvider,
			})
			continue
		}
		result.Tools = append(result.Tools, semantic.Tool{Name: tool.Name, Description: tool.Description, Schema: bytes.Clone(tool.Parameters), Strict: cloneBool(tool.Strict)})
	}
	result.ToolChoice, err = decodeResponseToolChoice(request.ToolChoice)
	if err != nil {
		return semantic.GenerateRequest{}, err
	}
	if err := validateResponseTools(result.Tools, result.ToolChoice); err != nil {
		return semantic.GenerateRequest{}, err
	}
	if request.Text != nil {
		format := request.Text.Format
		result.OutputFormat = &semantic.OutputFormat{Kind: semantic.OutputFormatKind(format.Type), Name: format.Name, Description: format.Description, Schema: bytes.Clone(format.Schema), Strict: format.Strict}
	}
	result.Requirements = result.DeriveRequirements()
	if err := result.Validate(); err != nil {
		return semantic.GenerateRequest{}, err
	}
	return result, nil
}

func validateResponseTools(tools []semantic.Tool, choice *semantic.ToolChoice) error {
	names := make(map[string]struct{}, len(tools))
	functionTools := 0
	for _, tool := range tools {
		if _, duplicate := names[tool.Name]; duplicate {
			return errors.New("tool names must be unique")
		}
		names[tool.Name] = struct{}{}
		if !tool.ProviderExecuted() {
			functionTools++
		}
	}
	if choice == nil || choice.Mode == semantic.ToolChoiceAuto || choice.Mode == semantic.ToolChoiceNone {
		return nil
	}
	// tool_choice selects among tools the caller will be called back about. A
	// provider-executed tool is not one of those, so it cannot satisfy a choice
	// that demands one — counting it would let `required` through on a request
	// with nothing for the model to call.
	if functionTools == 0 {
		return errors.New("tool_choice requires at least one function tool")
	}
	if choice.Mode == semantic.ToolChoiceNamed {
		named := false
		for _, tool := range tools {
			if !tool.ProviderExecuted() && tool.Name == choice.Name {
				named = true
			}
		}
		if !named {
			return errors.New("named tool_choice does not match a declared function tool")
		}
	}
	return nil
}

func decodeResponseInput(raw json.RawMessage) ([]semantic.Message, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []semantic.Message{{Role: semantic.RoleUser, Content: []semantic.Content{{Kind: semantic.ContentText, Text: text}}}}, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || len(items) == 0 {
		return nil, errors.New("input must be a string or a non-empty item array")
	}
	result := make([]semantic.Message, 0, len(items))
	for index, itemRaw := range items {
		var item openaiapi.ResponseInputItem
		if err := strictUnmarshal(itemRaw, &item); err != nil {
			return nil, fmt.Errorf("input[%d] is invalid: %w", index, err)
		}
		if item.Type != "reasoning" && (item.EncryptedContent != "" || item.Summary != nil) {
			return nil, fmt.Errorf("input[%d] has reasoning fields on another item type", index)
		}
		switch item.Type {
		case "", "message":
			if item.ID != "" && len(item.Content) == 0 {
				return nil, fmt.Errorf("input[%d].id requires stored item ownership", index)
			}
			message, err := decodeResponseMessage(item)
			if err != nil {
				return nil, fmt.Errorf("input[%d]: %w", index, err)
			}
			result = append(result, message)
		case "reasoning":
			if item.Role != "" || item.CallID != "" || item.Name != "" || item.Arguments != "" || len(item.Content) > 0 || len(item.Output) > 0 {
				return nil, fmt.Errorf("input[%d] is invalid reasoning", index)
			}
			part := semantic.Content{Kind: semantic.ContentReasoning, EncryptedContent: item.EncryptedContent}
			if strings.HasPrefix(item.EncryptedContent, responseReasoningPrefix) {
				if _, err := ResponseReasoningItems(part); err != nil {
					return nil, err
				}
				for _, summary := range item.Summary {
					if summary.Type != "summary_text" {
						return nil, errors.New("unsupported reasoning summary")
					}
					part.Text += summary.Text
				}
			} else {
				var err error
				part, err = ResponseReasoningContent([]openaiapi.ResponseOutputItem{{Type: "reasoning", ID: item.ID, EncryptedContent: item.EncryptedContent, Summary: item.Summary}})
				if err != nil {
					return nil, err
				}
			}
			result = append(result, semantic.Message{Role: semantic.RoleAssistant, Content: []semantic.Content{part}})
		case "function_call":
			if item.CallID == "" || item.Name == "" || item.Arguments == "" || len(item.Content) > 0 || len(item.Output) > 0 || item.Role != "" {
				return nil, fmt.Errorf("input[%d] is an invalid function_call", index)
			}
			result = append(result, semantic.Message{Role: semantic.RoleAssistant, Content: []semantic.Content{{Kind: semantic.ContentToolCall, CallID: item.CallID, Name: item.Name, Arguments: item.Arguments}}})
		case "function_call_output":
			if item.ID != "" || item.CallID == "" || len(item.Output) == 0 || len(item.Content) > 0 || item.Role != "" || item.Name != "" || item.Arguments != "" {
				return nil, fmt.Errorf("input[%d] is an invalid function_call_output", index)
			}
			output, err := responseOutputString(item.Output)
			if err != nil {
				return nil, fmt.Errorf("input[%d].output: %w", index, err)
			}
			result = append(result, semantic.Message{Role: semantic.RoleTool, Content: []semantic.Content{{Kind: semantic.ContentToolResult, CallID: item.CallID, Text: output}}})
		default:
			return nil, fmt.Errorf("input[%d].type is unsupported", index)
		}
	}
	return result, nil
}

func decodeResponseMessage(item openaiapi.ResponseInputItem) (semantic.Message, error) {
	role := semantic.Role(item.Role)
	switch role {
	case semantic.RoleSystem, semantic.RoleDeveloper, semantic.RoleUser, semantic.RoleAssistant:
	default:
		return semantic.Message{}, errors.New("message role is unsupported")
	}
	if item.CallID != "" || item.Name != "" || item.Arguments != "" || len(item.Output) > 0 || item.EncryptedContent != "" || len(item.Summary) > 0 {
		return semantic.Message{}, errors.New("message contains unrelated fields")
	}
	var text string
	if json.Unmarshal(item.Content, &text) == nil {
		return semantic.Message{Role: role, Content: []semantic.Content{{Kind: semantic.ContentText, Text: text}}}, nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(item.Content, &parts); err != nil || len(parts) == 0 {
		return semantic.Message{}, errors.New("message content must be text or a non-empty content array")
	}
	message := semantic.Message{Role: role}
	for index, partRaw := range parts {
		var part openaiapi.ResponseInputContent
		if err := strictUnmarshal(partRaw, &part); err != nil {
			return semantic.Message{}, fmt.Errorf("content[%d] is invalid", index)
		}
		if len(part.Annotations) > 0 || len(part.Logprobs) > 0 {
			return semantic.Message{}, errors.New("replay annotations and logprobs are unsupported")
		}
		switch part.Type {
		case "refusal":
			if role != semantic.RoleAssistant || part.Text != "" || part.ImageURL != "" || part.Detail != "" {
				return semantic.Message{}, errors.New("invalid refusal replay")
			}
			message.Content = append(message.Content, semantic.Content{Kind: semantic.ContentText, Text: part.Refusal})
		case "input_text", "output_text":
			if part.ImageURL != "" || part.Detail != "" || part.Refusal != "" {
				return semantic.Message{}, fmt.Errorf("content[%d] text contains image fields", index)
			}
			message.Content = append(message.Content, semantic.Content{Kind: semantic.ContentText, Text: part.Text})
		case "input_image":
			if role != semantic.RoleUser || part.ImageURL == "" || part.Text != "" || part.Refusal != "" {
				return semantic.Message{}, fmt.Errorf("content[%d] is an invalid input_image", index)
			}
			message.Content = append(message.Content, semantic.Content{Kind: semantic.ContentInputImage, URL: part.ImageURL, Detail: part.Detail})
		default:
			return semantic.Message{}, fmt.Errorf("content[%d].type is unsupported", index)
		}
	}
	return message, message.Validate()
}

func responseOutputString(raw json.RawMessage) (string, error) {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value, nil
	}
	return "", errors.New("only string tool outputs are portable")
}

func decodeResponseToolChoice(raw json.RawMessage) (*semantic.ToolChoice, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var mode string
	if json.Unmarshal(raw, &mode) == nil {
		switch mode {
		case "auto", "none", "required":
			return &semantic.ToolChoice{Mode: semantic.ToolChoiceMode(mode)}, nil
		default:
			return nil, errors.New("unsupported tool_choice")
		}
	}
	var named struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if strictUnmarshal(raw, &named) != nil || named.Type != "function" || named.Name == "" {
		return nil, errors.New("unsupported tool_choice")
	}
	return &semantic.ToolChoice{Mode: semantic.ToolChoiceNamed, Name: named.Name}, nil
}

func RenderResponseResult(result semantic.GenerateResult, request openaiapi.ResponseRequest) (openaiapi.Response, error) {
	if err := result.Validate(); err != nil {
		return openaiapi.Response{}, err
	}
	var err error
	if len(result.Choices) != 1 {
		return openaiapi.Response{}, errors.New("Responses requires exactly one generated candidate")
	}
	completedAt := result.Created
	response := baseResponse(responseID(result.ID), result.Created, result.Model, request)
	response.Status = responseStatus(result.Choices[0].Termination)
	if response.Status == "" {
		return openaiapi.Response{}, errors.New("provider termination cannot be represented by Responses")
	}
	if response.Status == "incomplete" {
		response.IncompleteDetails = map[string]string{"reason": "max_output_tokens"}
	}
	if response.Status == "completed" {
		response.CompletedAt = &completedAt
	}
	response.Output, err = renderResponseOutput(response.ID, result.Choices[0].Message, result.Choices[0].Termination)
	if err != nil {
		return openaiapi.Response{}, err
	}
	response.Usage = renderResponseUsage(result.Usage)
	return response, nil
}

func baseResponse(id string, created int64, model string, request openaiapi.ResponseRequest) openaiapi.Response {
	// The original Responses request has not passed through the translated Chat
	// request's redaction path. Do not echo payload/configuration that could
	// re-expose unredacted instructions, tool schemas, or schema descriptions.
	toolChoice := any("auto")
	format := openaiapi.ResponseTextFormat{Type: "text"}
	if request.Text != nil {
		format.Type = request.Text.Format.Type
	}
	parallel := true
	if request.ParallelToolCalls != nil {
		parallel = *request.ParallelToolCalls
	}
	temperature := cloneFloat(request.Temperature)
	if temperature == nil {
		value := 1.0
		temperature = &value
	}
	topP := cloneFloat(request.TopP)
	if topP == nil {
		value := 1.0
		topP = &value
	}
	return openaiapi.Response{
		ID: id, Object: "response", CreatedAt: created, Status: "in_progress", Background: false,
		Error: nil, IncompleteDetails: nil, Instructions: nil, MaxOutputTokens: cloneInt64(request.MaxOutputTokens),
		Model: model, Output: []openaiapi.ResponseOutputItem{}, ParallelToolCalls: parallel, PreviousResponseID: nil,
		Reasoning: openaiapi.ResponseReasoningOut{Effort: nil, Summary: nil}, Store: false,
		Temperature: temperature, Text: openaiapi.ResponseTextOut{Format: format}, ToolChoice: toolChoice,
		Tools: []openaiapi.ResponseTool{}, TopP: topP, Truncation: "disabled", Metadata: map[string]string{},
	}
}

func renderResponseOutput(responseIDValue string, message semantic.Message, termination string) ([]openaiapi.ResponseOutputItem, error) {
	var text strings.Builder
	items := make([]openaiapi.ResponseOutputItem, 0, len(message.Content)+1)
	annotations := []openaiapi.ResponseAnnotation{}
	toolNumber := 0
	for _, content := range message.Content {
		switch content.Kind {
		case semantic.ContentText:
			// Every text part lands in one output item, so each part's spans move
			// by however much text precedes it. Rebasing here is the only place
			// that knows both numbers; a citation left on its part's own offsets
			// would point at whatever happened to be at that index in the joined
			// answer.
			offset := text.Len()
			for _, citation := range content.Citations {
				annotations = append(annotations, openaiapi.ResponseAnnotation{
					Type: openaiapi.AnnotationURLCitation, URL: citation.URL, Title: citation.Title,
					StartIndex: citation.StartIndex + offset, EndIndex: citation.EndIndex + offset,
				})
			}
			text.WriteString(content.Text)
		case semantic.ContentReasoning:
			summary := []openaiapi.ResponseReasoningSummary{}
			if content.Text != "" {
				summary = append(summary, openaiapi.ResponseReasoningSummary{Type: "summary_text", Text: content.Text})
			}
			items = append(items, openaiapi.ResponseOutputItem{ID: responseItemID("rs", responseIDValue, 0), Type: "reasoning", Summary: summary, EncryptedContent: content.EncryptedContent})
		case semantic.ContentToolCall:
			items = append(items, openaiapi.ResponseOutputItem{ID: responseItemID("fc", responseIDValue, toolNumber), Type: "function_call", Status: "completed", CallID: content.CallID, Name: content.Name, Arguments: content.Arguments})
			toolNumber++
		case semantic.ContentProviderToolCall:
			if content.Name != semantic.ProviderToolWebSearch {
				return nil, errors.New("provider-executed tool cannot be represented by Responses")
			}
			item := openaiapi.ResponseOutputItem{
				ID: content.CallID, Type: openaiapi.OutputItemWebSearchCall, Status: content.Status,
			}
			if content.Text != "" {
				item.Action = &openaiapi.ResponseToolAction{Type: openaiapi.ToolActionSearch, Query: content.Text}
			}
			items = append(items, item)
		default:

			return nil, errors.New("content kind cannot be represented by Responses")
		}
	}
	if text.Len() > 0 || len(items) == 0 {
		outputContent := openaiapi.ResponseOutputContent{Type: "output_text", Text: text.String(), Annotations: annotations, Logprobs: []any{}}
		if termination == "refusal" {
			if len(annotations) > 0 {
				return nil, errors.New("a refusal cannot carry citations")
			}
			outputContent = openaiapi.ResponseOutputContent{Type: "refusal", Refusal: text.String()}
		}
		messageItem := openaiapi.ResponseOutputItem{ID: responseItemID("msg", responseIDValue, 0), Type: "message", Status: "completed", Role: "assistant", Content: []openaiapi.ResponseOutputContent{outputContent}}
		items = append([]openaiapi.ResponseOutputItem{messageItem}, items...)
	} else if len(annotations) > 0 {
		return nil, errors.New("citations have no text to annotate")
	}
	return items, nil
}

func renderResponseUsage(usage *semantic.Usage) *openaiapi.ResponseUsage {
	if usage == nil {
		return nil
	}
	return &openaiapi.ResponseUsage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, TotalTokens: usage.TotalTokens, InputTokensDetails: openaiapi.ResponseTokenDetails{CachedTokens: usage.CachedInputTokens, CacheWriteTokens: usage.CacheWriteInputTokens}, OutputTokensDetails: openaiapi.ResponseTokenDetails{ReasoningTokens: usage.ReasoningTokens}}
}

func responseStatus(termination string) string {
	switch termination {
	case "max_output":
		return "incomplete"
	case "", "complete", "tool_call", "refusal":
		return "completed"
	default:
		return ""
	}
}

func responseID(value string) string {
	if strings.HasPrefix(value, "resp_") {
		return value
	}
	value = strings.TrimPrefix(value, "chatcmpl-")
	value = strings.TrimPrefix(value, "chatcmpl_")
	return "resp_" + value
}

func responseItemID(prefix, id string, index int) string {
	return fmt.Sprintf("%s_%s_%d", prefix, strings.TrimPrefix(responseID(id), "resp_"), index)
}
