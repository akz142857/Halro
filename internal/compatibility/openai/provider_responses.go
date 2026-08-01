package openai

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/akz142857/Heimdall/internal/openaiapi"
	"github.com/akz142857/Heimdall/internal/semantic"
)

// RenderProviderResponseRequest converts the canonical Generate subset to the
// stateless Responses wire used by Provider primitives such as Bedrock Mantle.
// Store is always explicit false so an upstream default can never create a
// provider-owned resource behind Heimdall's stateless facade.
func RenderProviderResponseRequest(request semantic.GenerateRequest, providerModel string) (openaiapi.ResponseRequest, error) {
	if err := request.Validate(); err != nil {
		return openaiapi.ResponseRequest{}, err
	}
	if request.Candidates != nil && *request.Candidates > 1 {
		return openaiapi.ResponseRequest{}, errors.New("multiple candidates are not portable to stateless Responses")
	}
	if len(request.Stop) > 0 {
		return openaiapi.ResponseRequest{}, errors.New("stop sequences are not portable to stateless Responses")
	}
	if request.Seed != nil {
		return openaiapi.ResponseRequest{}, errors.New("seed is not portable to stateless Responses")
	}
	store := false
	result := openaiapi.ResponseRequest{
		Model: providerModel, Stream: request.Stream, Store: &store,
		Temperature: cloneFloat(request.Temperature), TopP: cloneFloat(request.TopP),
		ParallelToolCalls: cloneBool(request.ParallelTools), User: request.EndUserRef,
	}
	if request.CompletionTokenLimit != nil {
		result.MaxOutputTokens = cloneInt64(request.CompletionTokenLimit)
	} else {
		result.MaxOutputTokens = cloneInt64(request.VisibleOutputTokenLimit)
	}
	items, err := renderProviderResponseItems(request.Messages)
	if err != nil {
		return openaiapi.ResponseRequest{}, err
	}
	result.Input, err = json.Marshal(items)
	if err != nil {
		return openaiapi.ResponseRequest{}, err
	}
	for _, tool := range request.Tools {
		result.Tools = append(result.Tools, openaiapi.ResponseTool{Type: "function", Name: tool.Name, Description: tool.Description, Parameters: bytes.Clone(tool.Schema)})
	}
	result.ToolChoice, err = renderProviderResponseToolChoice(request.ToolChoice)
	if err != nil {
		return openaiapi.ResponseRequest{}, err
	}
	if request.OutputFormat != nil {
		result.Text = &openaiapi.ResponseTextConfig{Format: openaiapi.ResponseTextFormat{Type: string(request.OutputFormat.Kind), Name: request.OutputFormat.Name, Description: request.OutputFormat.Description, Schema: bytes.Clone(request.OutputFormat.Schema), Strict: request.OutputFormat.Strict}}
	}
	if request.ReasoningEffort != "" {
		result.Reasoning = &openaiapi.ResponseReasoning{Effort: request.ReasoningEffort}
	}
	return result, result.Validate()
}

func renderProviderResponseItems(messages []semantic.Message) ([]openaiapi.ResponseInputItem, error) {
	items := make([]openaiapi.ResponseInputItem, 0, len(messages))
	for _, message := range messages {
		parts := make([]openaiapi.ResponseInputContent, 0, len(message.Content))
		flush := func() error {
			if len(parts) == 0 {
				return nil
			}
			content, err := json.Marshal(parts)
			if err != nil {
				return err
			}
			items = append(items, openaiapi.ResponseInputItem{Type: "message", Role: string(message.Role), Content: content})
			parts = nil
			return nil
		}
		for _, content := range message.Content {
			switch content.Kind {
			case semantic.ContentText:
				parts = append(parts, openaiapi.ResponseInputContent{Type: "input_text", Text: content.Text})
			case semantic.ContentInputImage:
				parts = append(parts, openaiapi.ResponseInputContent{Type: "input_image", ImageURL: content.URL, Detail: content.Detail})
			case semantic.ContentToolCall:
				if err := flush(); err != nil {
					return nil, err
				}
				items = append(items, openaiapi.ResponseInputItem{Type: "function_call", CallID: content.CallID, Name: content.Name, Arguments: content.Arguments})
			case semantic.ContentToolResult:
				if err := flush(); err != nil {
					return nil, err
				}
				output, _ := json.Marshal(content.Text)
				items = append(items, openaiapi.ResponseInputItem{Type: "function_call_output", CallID: content.CallID, Output: output})
			default:
				return nil, errors.New("content is not portable to Responses")
			}
		}
		if err := flush(); err != nil {
			return nil, err
		}
	}
	return items, nil
}

func renderProviderResponseToolChoice(choice *semantic.ToolChoice) (json.RawMessage, error) {
	if choice == nil {
		return nil, nil
	}
	if choice.Mode == semantic.ToolChoiceNamed {
		return json.Marshal(map[string]string{"type": "function", "name": choice.Name})
	}
	return json.Marshal(string(choice.Mode))
}

func DecodeProviderResponse(response openaiapi.Response) (semantic.GenerateResult, error) {
	if response.ID == "" || response.Model == "" || (response.Status != "completed" && response.Status != "incomplete") {
		return semantic.GenerateResult{}, errors.New("provider Responses result identity is invalid")
	}
	message := semantic.Message{Role: semantic.RoleAssistant}
	for _, item := range response.Output {
		switch item.Type {
		case "message":
			for _, content := range item.Content {
				switch content.Type {
				case "output_text":
					message.Content = append(message.Content, semantic.Content{Kind: semantic.ContentText, Text: content.Text})
				case "refusal":
					message.Content = append(message.Content, semantic.Content{Kind: semantic.ContentText, Text: content.Refusal})
				default:
					return semantic.GenerateResult{}, errors.New("provider Responses output content is unsupported")
				}
			}
		case "function_call":
			if item.CallID == "" || item.Name == "" || !json.Valid([]byte(item.Arguments)) {
				return semantic.GenerateResult{}, errors.New("provider Responses function call is invalid")
			}
			message.Content = append(message.Content, semantic.Content{Kind: semantic.ContentToolCall, CallID: item.CallID, Name: item.Name, Arguments: item.Arguments})
		default:
			return semantic.GenerateResult{}, errors.New("provider Responses output item is unsupported")
		}
	}
	if len(message.Content) == 0 {
		return semantic.GenerateResult{}, errors.New("provider Responses result has no portable output")
	}
	termination := "complete"
	if response.Status == "incomplete" {
		termination = "max_output"
	}
	usage := decodeProviderResponseUsage(response.Usage)
	result := semantic.GenerateResult{ID: response.ID, Created: response.CreatedAt, Model: response.Model, Choices: []semantic.GenerateChoice{{Index: 0, Message: message, Termination: termination, NativeTermination: response.Status}}, Usage: usage, Translation: semantic.TranslationNone, MappingRevision: MappingRevision}
	return result, result.Validate()
}

func decodeProviderResponseUsage(usage *openaiapi.ResponseUsage) *semantic.Usage {
	if usage == nil {
		return nil
	}
	return &semantic.Usage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, ReasoningTokens: usage.OutputTokensDetails.ReasoningTokens, TotalTokens: usage.TotalTokens, Source: semantic.UsageProviderReported}
}
