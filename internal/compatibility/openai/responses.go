package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/akz142857/Heimdall/internal/compatibility"
	"github.com/akz142857/Heimdall/internal/openaiapi"
	"github.com/akz142857/Heimdall/internal/semantic"
)

func DecodeResponseGenerate(request openaiapi.ResponseRequest) (semantic.GenerateRequest, error) {
	if err := request.Validate(); err != nil {
		return semantic.GenerateRequest{}, err
	}
	result := semantic.GenerateRequest{
		Operation: semantic.OperationGenerate,
		Source:    semantic.Source{ProfileID: string(compatibility.ProfileOpenAIResponses), ProfileRevision: 1},
		Mode:      semantic.ModePortable, RequestedModel: request.Model,
		Temperature: cloneFloat(request.Temperature), TopP: cloneFloat(request.TopP),
		CompletionTokenLimit: cloneInt64(request.MaxOutputTokens),
		ParallelTools:        cloneBool(request.ParallelToolCalls), Stream: request.Stream,
		IncludeUsage: request.Stream, EndUserRef: request.User,
	}
	if request.Reasoning != nil {
		result.ReasoningEffort = request.Reasoning.Effort
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
		result.Tools = append(result.Tools, semantic.Tool{Name: tool.Name, Description: tool.Description, Schema: bytes.Clone(tool.Parameters)})
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
	for _, tool := range tools {
		if _, duplicate := names[tool.Name]; duplicate {
			return errors.New("function tool names must be unique")
		}
		names[tool.Name] = struct{}{}
	}
	if choice == nil || choice.Mode == semantic.ToolChoiceAuto || choice.Mode == semantic.ToolChoiceNone {
		return nil
	}
	if len(tools) == 0 {
		return errors.New("tool_choice requires at least one function tool")
	}
	if choice.Mode == semantic.ToolChoiceNamed {
		if _, ok := names[choice.Name]; !ok {
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
		switch item.Type {
		case "", "message":
			if item.ID != "" {
				return nil, fmt.Errorf("input[%d].id requires stored item ownership", index)
			}
			message, err := decodeResponseMessage(item)
			if err != nil {
				return nil, fmt.Errorf("input[%d]: %w", index, err)
			}
			result = append(result, message)
		case "function_call":
			if item.ID != "" || item.CallID == "" || item.Name == "" || item.Arguments == "" || len(item.Content) > 0 || len(item.Output) > 0 || item.Role != "" {
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
	if item.CallID != "" || item.Name != "" || item.Arguments != "" || len(item.Output) > 0 {
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
		switch part.Type {
		case "input_text", "output_text":
			if part.ImageURL != "" || part.Detail != "" {
				return semantic.Message{}, fmt.Errorf("content[%d] text contains image fields", index)
			}
			message.Content = append(message.Content, semantic.Content{Kind: semantic.ContentText, Text: part.Text})
		case "input_image":
			if role != semantic.RoleUser || part.ImageURL == "" || part.Text != "" {
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
	response.Output = renderResponseOutput(response.ID, result.Choices[0].Message, result.Choices[0].Termination)
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

func renderResponseOutput(responseIDValue string, message semantic.Message, termination string) []openaiapi.ResponseOutputItem {
	var text strings.Builder
	items := make([]openaiapi.ResponseOutputItem, 0, len(message.Content)+1)
	toolNumber := 0
	for _, content := range message.Content {
		switch content.Kind {
		case semantic.ContentText:
			text.WriteString(content.Text)
		case semantic.ContentToolCall:
			items = append(items, openaiapi.ResponseOutputItem{ID: responseItemID("fc", responseIDValue, toolNumber), Type: "function_call", Status: "completed", CallID: content.CallID, Name: content.Name, Arguments: content.Arguments})
			toolNumber++
		}
	}
	if text.Len() > 0 || len(items) == 0 {
		outputContent := openaiapi.ResponseOutputContent{Type: "output_text", Text: text.String(), Annotations: []any{}, Logprobs: []any{}}
		if termination == "refusal" {
			outputContent = openaiapi.ResponseOutputContent{Type: "refusal", Refusal: text.String()}
		}
		messageItem := openaiapi.ResponseOutputItem{ID: responseItemID("msg", responseIDValue, 0), Type: "message", Status: "completed", Role: "assistant", Content: []openaiapi.ResponseOutputContent{outputContent}}
		items = append([]openaiapi.ResponseOutputItem{messageItem}, items...)
	}
	return items
}

func renderResponseUsage(usage *semantic.Usage) *openaiapi.ResponseUsage {
	if usage == nil {
		return nil
	}
	return &openaiapi.ResponseUsage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, TotalTokens: usage.TotalTokens, OutputTokensDetails: openaiapi.ResponseTokenDetails{ReasoningTokens: usage.ReasoningTokens}}
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

type ResponseStreamRenderer struct {
	request     openaiapi.ResponseRequest
	sequence    int64
	id          string
	created     int64
	model       string
	itemStarted bool
	text        strings.Builder
	usage       *semantic.Usage
	termination string
}

func NewResponseStreamRenderer(request openaiapi.ResponseRequest) *ResponseStreamRenderer {
	return &ResponseStreamRenderer{request: request}
}

func (renderer *ResponseStreamRenderer) Accept(event semantic.Event) ([]openaiapi.ResponseStreamEvent, error) {
	if renderer == nil {
		return nil, errors.New("response stream renderer is nil")
	}
	if renderer.id == "" {
		renderer.id, renderer.created, renderer.model = responseID(event.ID), event.Created, event.Model
	}
	if renderer.id != responseID(event.ID) || renderer.model != event.Model {
		return nil, errors.New("response stream identity changed")
	}
	events := make([]openaiapi.ResponseStreamEvent, 0, 4)
	if renderer.sequence == 0 {
		created := baseResponse(renderer.id, renderer.created, renderer.model, renderer.request)
		events = append(events, renderer.event("response.created", func(output *openaiapi.ResponseStreamEvent) { output.Response = &created }))
		inProgress := created
		events = append(events, renderer.event("response.in_progress", func(output *openaiapi.ResponseStreamEvent) { output.Response = &inProgress }))
	}
	if event.Kind == semantic.EventUsage {
		renderer.usage = event.Usage
		return events, nil
	}
	for _, output := range event.Outputs {
		if output.Index != 0 {
			return nil, errors.New("Responses streaming supports exactly one candidate")
		}
		if output.Termination != "" {
			if output.Termination != "complete" && output.Termination != "max_output" {
				return nil, errors.New("provider stream termination is unsupported by the Phase 1A text contract")
			}
			renderer.termination = output.Termination
		}
		for _, content := range output.Content {
			if content.Kind != semantic.ContentText {
				return nil, errors.New("Phase 1A Responses streaming only supports output text")
			}
			if !renderer.itemStarted {
				events = append(events, renderer.startItemEvents()...)
			}
			renderer.text.WriteString(content.Text)
			outputIndex, contentIndex := 0, 0
			events = append(events, renderer.event("response.output_text.delta", func(event *openaiapi.ResponseStreamEvent) {
				event.OutputIndex, event.ContentIndex = &outputIndex, &contentIndex
				event.ItemID = renderer.itemID()
				event.Delta = content.Text
				empty := []any{}
				event.Logprobs = &empty
			}))
		}
	}
	return events, nil
}

func (renderer *ResponseStreamRenderer) Complete() ([]openaiapi.ResponseStreamEvent, error) {
	if renderer == nil || renderer.id == "" {
		return nil, errors.New("response stream completed before any provider event")
	}
	events := make([]openaiapi.ResponseStreamEvent, 0, 6)
	if !renderer.itemStarted {
		events = append(events, renderer.startItemEvents()...)
	}
	outputIndex, contentIndex := 0, 0
	content := openaiapi.ResponseOutputContent{Type: "output_text", Text: renderer.text.String(), Annotations: []any{}, Logprobs: []any{}}
	item := renderer.item("completed")
	events = append(events,
		renderer.event("response.output_text.done", func(event *openaiapi.ResponseStreamEvent) {
			event.OutputIndex, event.ContentIndex, event.ItemID, event.Text = &outputIndex, &contentIndex, renderer.itemID(), renderer.text.String()
			empty := []any{}
			event.Logprobs = &empty
		}),
		renderer.event("response.content_part.done", func(event *openaiapi.ResponseStreamEvent) {
			event.OutputIndex, event.ContentIndex, event.ItemID, event.Part = &outputIndex, &contentIndex, renderer.itemID(), &content
		}),
		renderer.event("response.output_item.done", func(event *openaiapi.ResponseStreamEvent) {
			event.OutputIndex, event.Item = &outputIndex, &item
		}),
	)
	response := baseResponse(renderer.id, renderer.created, renderer.model, renderer.request)
	response.Status = responseStatus(renderer.termination)
	if response.Status == "" {
		return nil, errors.New("provider stream termination cannot be represented by Responses")
	}
	if response.Status == "incomplete" {
		response.IncompleteDetails = map[string]string{"reason": "max_output_tokens"}
	}
	completedAt := renderer.created
	if response.Status == "completed" {
		response.CompletedAt = &completedAt
	}
	response.Output = []openaiapi.ResponseOutputItem{item}
	response.Usage = renderResponseUsage(renderer.usage)
	eventType := "response.completed"
	if response.Status == "incomplete" {
		eventType = "response.incomplete"
	}
	events = append(events, renderer.event(eventType, func(event *openaiapi.ResponseStreamEvent) { event.Response = &response }))
	return events, nil
}

func (renderer *ResponseStreamRenderer) startItemEvents() []openaiapi.ResponseStreamEvent {
	renderer.itemStarted = true
	outputIndex, contentIndex := 0, 0
	item := renderer.item("in_progress")
	part := openaiapi.ResponseOutputContent{Type: "output_text", Text: "", Annotations: []any{}, Logprobs: []any{}}
	return []openaiapi.ResponseStreamEvent{
		renderer.event("response.output_item.added", func(event *openaiapi.ResponseStreamEvent) { event.OutputIndex, event.Item = &outputIndex, &item }),
		renderer.event("response.content_part.added", func(event *openaiapi.ResponseStreamEvent) {
			event.OutputIndex, event.ContentIndex, event.ItemID, event.Part = &outputIndex, &contentIndex, renderer.itemID(), &part
		}),
	}
}

func (renderer *ResponseStreamRenderer) item(status string) openaiapi.ResponseOutputItem {
	return openaiapi.ResponseOutputItem{ID: renderer.itemID(), Type: "message", Status: status, Role: "assistant", Content: []openaiapi.ResponseOutputContent{{Type: "output_text", Text: renderer.text.String(), Annotations: []any{}, Logprobs: []any{}}}}
}

func (renderer *ResponseStreamRenderer) itemID() string {
	return responseItemID("msg", renderer.id, 0)
}

func (renderer *ResponseStreamRenderer) event(kind string, apply func(*openaiapi.ResponseStreamEvent)) openaiapi.ResponseStreamEvent {
	event := openaiapi.ResponseStreamEvent{Type: kind, SequenceNumber: renderer.sequence}
	renderer.sequence++
	apply(&event)
	return event
}
