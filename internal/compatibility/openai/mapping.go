package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/akz142857/Heimdall/internal/compatibility"
	"github.com/akz142857/Heimdall/internal/openaiapi"
	"github.com/akz142857/Heimdall/internal/semantic"
)

const MappingRevision uint64 = 1

func DecodeGenerate(request openaiapi.ChatCompletionRequest) (semantic.GenerateRequest, error) {
	result := semantic.GenerateRequest{
		Operation: semantic.OperationGenerate,
		Source:    semantic.Source{ProfileID: string(compatibility.ProfileOpenAIChatCompletions), ProfileRevision: 1},
		Mode:      semantic.ModePortable, RequestedModel: request.Model,
		Temperature: request.Temperature, TopP: request.TopP, Candidates: request.N,
		Seed: request.Seed, ReasoningEffort: request.ReasoningEffort, EndUserRef: request.User,
		Stream: request.Stream, IncludeUsage: request.StreamOptions != nil && request.StreamOptions.IncludeUsage,
		ParallelTools: cloneBool(request.ParallelToolCalls),
	}
	result.VisibleOutputTokenLimit = cloneInt64(request.MaxTokens)
	result.CompletionTokenLimit = cloneInt64(request.MaxCompletionTokens)
	stop, err := decodeStop(request.Stop)
	if err != nil {
		return semantic.GenerateRequest{}, err
	}
	result.Stop = stop
	for _, message := range request.Messages {
		converted, err := decodeMessage(message)
		if err != nil {
			return semantic.GenerateRequest{}, err
		}
		result.Messages = append(result.Messages, converted)
	}
	for _, tool := range request.Tools {
		if tool.Type != "function" {
			return semantic.GenerateRequest{}, errors.New("unsupported tool type")
		}
		result.Tools = append(result.Tools, semantic.Tool{Name: tool.Function.Name, Description: tool.Function.Description, Schema: bytes.Clone(tool.Function.Parameters)})
	}
	result.ToolChoice, err = decodeToolChoice(request.ToolChoice)
	if err != nil {
		return semantic.GenerateRequest{}, err
	}
	result.OutputFormat, err = decodeOutputFormat(request.ResponseFormat)
	if err != nil {
		return semantic.GenerateRequest{}, err
	}
	result.Requirements = result.DeriveRequirements()
	if err := result.Validate(); err != nil {
		return semantic.GenerateRequest{}, err
	}
	return result, nil
}

func RenderGenerateRequest(request semantic.GenerateRequest, providerModel string) (openaiapi.ChatCompletionRequest, error) {
	if err := request.Validate(); err != nil {
		return openaiapi.ChatCompletionRequest{}, err
	}
	result := openaiapi.ChatCompletionRequest{
		Model: providerModel, Stream: request.Stream, Temperature: cloneFloat(request.Temperature), TopP: cloneFloat(request.TopP),
		MaxTokens: cloneInt64(request.VisibleOutputTokenLimit), MaxCompletionTokens: cloneInt64(request.CompletionTokenLimit), N: cloneInt(request.Candidates), Seed: cloneInt64(request.Seed),
		ParallelToolCalls: cloneBool(request.ParallelTools), ReasoningEffort: request.ReasoningEffort, User: request.EndUserRef,
	}
	if request.Stream && request.IncludeUsage {
		result.StreamOptions = &openaiapi.StreamOptions{IncludeUsage: true}
	}
	for _, message := range request.Messages {
		converted, err := renderMessage(message)
		if err != nil {
			return openaiapi.ChatCompletionRequest{}, err
		}
		result.Messages = append(result.Messages, converted)
	}
	for _, tool := range request.Tools {
		result.Tools = append(result.Tools, openaiapi.Tool{Type: "function", Function: openaiapi.ToolFunction{Name: tool.Name, Description: tool.Description, Parameters: bytes.Clone(tool.Schema)}})
	}
	result.ToolChoice = renderToolChoice(request.ToolChoice)
	result.ResponseFormat = renderOutputFormat(request.OutputFormat)
	result.Stop = renderStop(request.Stop)
	if err := result.Validate(); err != nil {
		return openaiapi.ChatCompletionRequest{}, err
	}
	return result, nil
}

func DecodeGenerateResult(response openaiapi.ChatCompletionResponse) (semantic.GenerateResult, error) {
	result := semantic.GenerateResult{ID: response.ID, Created: response.Created, Model: response.Model, Translation: semantic.TranslationNone, MappingRevision: MappingRevision}
	for _, choice := range response.Choices {
		messageSource := openaiapi.Message{Role: "assistant"}
		if choice.Message != nil {
			messageSource = *choice.Message
		}
		message, err := decodeMessage(messageSource)
		if err != nil {
			return semantic.GenerateResult{}, err
		}
		native := ""
		if choice.FinishReason != nil {
			native = *choice.FinishReason
		}
		result.Choices = append(result.Choices, semantic.GenerateChoice{Index: choice.Index, Message: message, Termination: normalizeTermination(native), NativeTermination: native})
	}
	result.Usage = decodeUsage(response.Usage)
	if err := result.Validate(); err != nil {
		return semantic.GenerateResult{}, err
	}
	return result, nil
}

func RenderGenerateResult(result semantic.GenerateResult) (openaiapi.ChatCompletionResponse, error) {
	if err := result.Validate(); err != nil {
		return openaiapi.ChatCompletionResponse{}, err
	}
	response := openaiapi.ChatCompletionResponse{ID: result.ID, Object: "chat.completion", Created: result.Created, Model: result.Model, Usage: renderUsage(result.Usage)}
	for _, choice := range result.Choices {
		message, err := renderMessage(choice.Message)
		if err != nil {
			return openaiapi.ChatCompletionResponse{}, err
		}
		finish := choice.NativeTermination
		if finish == "" {
			finish = renderTermination(choice.Termination)
		}
		response.Choices = append(response.Choices, openaiapi.Choice{Index: choice.Index, Message: &message, FinishReason: stringPointer(finish)})
	}
	return response, nil
}

func DecodeEmbedding(request openaiapi.EmbeddingRequest) (semantic.EmbeddingRequest, error) {
	result := semantic.EmbeddingRequest{Operation: semantic.OperationEmbed, Source: semantic.Source{ProfileID: string(compatibility.ProfileOpenAIEmbeddings), ProfileRevision: 1}, Mode: semantic.ModePortable, RequestedModel: request.Model, Input: bytes.Clone(request.Input), Encoding: request.EncodingFormat, Dimensions: cloneInt64(request.Dimensions), EndUserRef: request.User}
	result.Requirements = result.DeriveRequirements()
	if err := result.Validate(); err != nil {
		return semantic.EmbeddingRequest{}, err
	}
	return result, nil
}

func RenderEmbeddingRequest(request semantic.EmbeddingRequest, providerModel string) (openaiapi.EmbeddingRequest, error) {
	if err := request.Validate(); err != nil {
		return openaiapi.EmbeddingRequest{}, err
	}
	result := openaiapi.EmbeddingRequest{Model: providerModel, Input: bytes.Clone(request.Input), EncodingFormat: request.Encoding, Dimensions: cloneInt64(request.Dimensions), User: request.EndUserRef}
	if err := result.Validate(); err != nil {
		return openaiapi.EmbeddingRequest{}, err
	}
	return result, nil
}

func DecodeEmbeddingResult(response openaiapi.EmbeddingResponse) (semantic.EmbeddingResult, error) {
	result := semantic.EmbeddingResult{Model: response.Model, Usage: decodeUsage(response.Usage), Translation: semantic.TranslationNone, MappingRevision: MappingRevision}
	for _, item := range response.Data {
		result.Data = append(result.Data, semantic.Embedding{Index: item.Index, Value: bytes.Clone(item.Embedding)})
	}
	if err := result.Validate(); err != nil {
		return semantic.EmbeddingResult{}, err
	}
	return result, nil
}

func DecodeUsage(usage *openaiapi.Usage) *semantic.Usage { return decodeUsage(usage) }

func RenderUsage(usage *semantic.Usage) *openaiapi.Usage { return renderUsage(usage) }

func RenderEmbeddingResult(result semantic.EmbeddingResult) (openaiapi.EmbeddingResponse, error) {
	if err := result.Validate(); err != nil {
		return openaiapi.EmbeddingResponse{}, err
	}
	response := openaiapi.EmbeddingResponse{Object: "list", Model: result.Model, Usage: renderUsage(result.Usage)}
	for _, item := range result.Data {
		response.Data = append(response.Data, openaiapi.EmbeddingData{Object: "embedding", Index: item.Index, Embedding: bytes.Clone(item.Value)})
	}
	return response, nil
}

func DecodeEvent(chunk openaiapi.ChatCompletionResponse) (semantic.Event, error) {
	revision := chunk.SemanticMappingRevision
	if revision == 0 {
		revision = MappingRevision
	}
	translation := semantic.TranslationLoss(chunk.SemanticTranslation)
	if translation == "" {
		translation = semantic.TranslationNone
	}
	event := semantic.Event{ID: chunk.ID, Created: chunk.Created, Model: chunk.Model, MappingRevision: revision, Translation: translation}
	if len(chunk.Choices) > 0 {
		event.Kind = semantic.EventDelta
		for _, source := range chunk.Choices {
			if source.Delta == nil {
				return semantic.Event{}, errors.New("streaming choice is missing delta")
			}
			output := semantic.OutputDelta{Index: source.Index, Role: semantic.Role(source.Delta.Role)}
			if len(source.Delta.Content) > 0 {
				text, ok := openaiapi.DecodeTextContent(source.Delta.Content)
				if !ok {
					return semantic.Event{}, errors.New("streaming content delta is not text")
				}
				output.Content = append(output.Content, semantic.ContentDelta{Kind: semantic.ContentText, Text: text})
			}
			if source.Delta.ReasoningContent != "" {
				output.Content = append(output.Content, semantic.ContentDelta{Kind: semantic.ContentReasoning, Text: source.Delta.ReasoningContent})
			}
			for _, call := range source.Delta.ToolCalls {
				output.Content = append(output.Content, semantic.ContentDelta{Kind: semantic.ContentToolCall, ToolIndex: cloneInt(call.Index), CallID: call.ID, Name: call.Function.Name, ArgumentsFragment: call.Function.Arguments})
			}
			if source.FinishReason != nil {
				output.NativeTermination = *source.FinishReason
				output.Termination = normalizeTermination(*source.FinishReason)
			}
			event.Outputs = append(event.Outputs, output)
		}
	} else if chunk.Usage != nil {
		event.Kind = semantic.EventUsage
	}
	event.Usage = decodeUsage(chunk.Usage)
	if err := event.Validate(); err != nil {
		return semantic.Event{}, err
	}
	return event, nil
}

func RenderEvent(event semantic.Event) (openaiapi.ChatCompletionResponse, error) {
	if err := event.Validate(); err != nil {
		return openaiapi.ChatCompletionResponse{}, err
	}
	chunk := openaiapi.ChatCompletionResponse{ID: event.ID, Object: "chat.completion.chunk", Created: event.Created, Model: event.Model, Usage: renderUsage(event.Usage), SemanticMappingRevision: event.MappingRevision, SemanticTranslation: string(event.Translation)}
	for _, source := range event.Outputs {
		delta := &openaiapi.Message{Role: string(source.Role)}
		for _, content := range source.Content {
			switch content.Kind {
			case semantic.ContentText:
				delta.Content = openaiapi.TextContent(content.Text)
			case semantic.ContentReasoning:
				delta.ReasoningContent = content.Text
			case semantic.ContentToolCall:
				delta.ToolCalls = append(delta.ToolCalls, openaiapi.ToolCall{Index: cloneInt(content.ToolIndex), ID: content.CallID, Type: "function", Function: openaiapi.ToolCallFunction{Name: content.Name, Arguments: content.ArgumentsFragment}})
			}
		}
		finish := source.NativeTermination
		if finish == "" {
			finish = renderTermination(source.Termination)
		}
		chunk.Choices = append(chunk.Choices, openaiapi.Choice{Index: source.Index, Delta: delta, FinishReason: stringPointer(finish)})
	}
	return chunk, nil
}

func decodeMessage(message openaiapi.Message) (semantic.Message, error) {
	result := semantic.Message{Role: semantic.Role(message.Role), Name: message.Name}
	if len(message.Content) > 0 && !bytes.Equal(bytes.TrimSpace(message.Content), []byte("null")) {
		if text, ok := openaiapi.DecodeTextContent(message.Content); ok {
			kind := semantic.ContentText
			if message.Role == "tool" {
				kind = semantic.ContentToolResult
			}
			result.Content = append(result.Content, semantic.Content{Kind: kind, Text: text, CallID: message.ToolCallID})
		} else {
			var parts []struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				ImageURL struct {
					URL    string `json:"url"`
					Detail string `json:"detail"`
				} `json:"image_url"`
				URL    string `json:"url"`
				Detail string `json:"detail"`
			}
			if err := json.Unmarshal(message.Content, &parts); err != nil {
				return semantic.Message{}, errors.New("unsupported message content")
			}
			for _, part := range parts {
				switch part.Type {
				case "text", "input_text":
					kind := semantic.ContentText
					if message.Role == "tool" {
						kind = semantic.ContentToolResult
					}
					result.Content = append(result.Content, semantic.Content{Kind: kind, Text: part.Text, CallID: message.ToolCallID})
				case "image", "image_url", "input_image":
					url, detail := part.ImageURL.URL, part.ImageURL.Detail
					if url == "" {
						url, detail = part.URL, part.Detail
					}
					result.Content = append(result.Content, semantic.Content{Kind: semantic.ContentInputImage, URL: url, Detail: detail})
				default:
					return semantic.Message{}, fmt.Errorf("unsupported message content type %q", part.Type)
				}
			}
		}
	}
	if message.ReasoningContent != "" {
		result.Content = append(result.Content, semantic.Content{Kind: semantic.ContentReasoning, Text: message.ReasoningContent})
	}
	for _, call := range message.ToolCalls {
		result.Content = append(result.Content, semantic.Content{Kind: semantic.ContentToolCall, CallID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments})
	}
	if err := result.Validate(); err != nil {
		return semantic.Message{}, err
	}
	return result, nil
}

func renderMessage(message semantic.Message) (openaiapi.Message, error) {
	if err := message.Validate(); err != nil {
		return openaiapi.Message{}, err
	}
	result := openaiapi.Message{Role: string(message.Role), Name: message.Name}
	var textParts []string
	var rich []map[string]any
	var toolResults []string
	for _, part := range message.Content {
		switch part.Kind {
		case semantic.ContentText:
			textParts = append(textParts, part.Text)
			rich = append(rich, map[string]any{"type": "text", "text": part.Text})
		case semantic.ContentInputImage:
			image := map[string]any{"url": part.URL}
			if part.Detail != "" {
				image["detail"] = part.Detail
			}
			rich = append(rich, map[string]any{"type": "image_url", "image_url": image})
		case semantic.ContentReasoning:
			result.ReasoningContent = part.Text
		case semantic.ContentToolCall:
			result.ToolCalls = append(result.ToolCalls, openaiapi.ToolCall{ID: part.CallID, Type: "function", Function: openaiapi.ToolCallFunction{Name: part.Name, Arguments: part.Arguments}})
		case semantic.ContentToolResult:
			result.ToolCallID = part.CallID
			toolResults = append(toolResults, part.Text)
		}
	}
	if len(toolResults) == 1 {
		result.Content = openaiapi.TextContent(toolResults[0])
	}
	if len(toolResults) > 1 {
		parts := make([]map[string]string, 0, len(toolResults))
		for _, text := range toolResults {
			parts = append(parts, map[string]string{"type": "text", "text": text})
		}
		result.Content, _ = json.Marshal(parts)
	}
	if len(result.Content) == 0 && len(rich) > 0 {
		if len(rich) == len(textParts) && len(textParts) == 1 {
			result.Content = openaiapi.TextContent(textParts[0])
		} else {
			result.Content, _ = json.Marshal(rich)
		}
	}
	return result, nil
}

func decodeStop(raw json.RawMessage) ([]string, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}, nil
	}
	var many []string
	if json.Unmarshal(raw, &many) != nil {
		return nil, errors.New("stop must be a string or string array")
	}
	return many, nil
}
func renderStop(values []string) json.RawMessage {
	if len(values) == 0 {
		return nil
	}
	if len(values) == 1 {
		v, _ := json.Marshal(values[0])
		return v
	}
	v, _ := json.Marshal(values)
	return v
}
func decodeToolChoice(raw json.RawMessage) (*semantic.ToolChoice, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var mode string
	if json.Unmarshal(raw, &mode) == nil {
		choice := &semantic.ToolChoice{Mode: semantic.ToolChoiceMode(mode)}
		if mode != "auto" && mode != "none" && mode != "required" {
			return nil, errors.New("unsupported tool choice")
		}
		return choice, nil
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &obj) != nil || obj.Type != "function" || obj.Function.Name == "" {
		return nil, errors.New("unsupported tool choice")
	}
	return &semantic.ToolChoice{Mode: semantic.ToolChoiceNamed, Name: obj.Function.Name}, nil
}
func renderToolChoice(choice *semantic.ToolChoice) json.RawMessage {
	if choice == nil {
		return nil
	}
	if choice.Mode != semantic.ToolChoiceNamed {
		v, _ := json.Marshal(string(choice.Mode))
		return v
	}
	v, _ := json.Marshal(map[string]any{"type": "function", "function": map[string]string{"name": choice.Name}})
	return v
}
func decodeOutputFormat(raw json.RawMessage) (*semantic.OutputFormat, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var obj struct {
		Type       string `json:"type"`
		JSONSchema struct {
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Schema      json.RawMessage `json:"schema"`
			Strict      bool            `json:"strict"`
		} `json:"json_schema"`
	}
	if strictUnmarshal(raw, &obj) != nil {
		return nil, errors.New("invalid response format")
	}
	f := &semantic.OutputFormat{Kind: semantic.OutputFormatKind(obj.Type)}
	if obj.Type == "json_schema" {
		f.Name = obj.JSONSchema.Name
		f.Description = obj.JSONSchema.Description
		f.Schema = bytes.Clone(obj.JSONSchema.Schema)
		f.Strict = obj.JSONSchema.Strict
	}
	return f, nil
}
func renderOutputFormat(format *semantic.OutputFormat) json.RawMessage {
	if format == nil {
		return nil
	}
	var value any = map[string]any{"type": format.Kind}
	if format.Kind == semantic.OutputJSONSchema {
		schema := map[string]any{"name": format.Name, "schema": json.RawMessage(format.Schema), "strict": format.Strict}
		if format.Description != "" {
			schema["description"] = format.Description
		}
		value = map[string]any{"type": "json_schema", "json_schema": schema}
	}
	raw, _ := json.Marshal(value)
	return raw
}

func strictUnmarshal(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("multiple JSON values")
	}
	return nil
}
func decodeUsage(usage *openaiapi.Usage) *semantic.Usage {
	if usage == nil {
		return nil
	}
	return &semantic.Usage{InputTokens: usage.PromptTokens, OutputTokens: usage.CompletionTokens, TotalTokens: usage.TotalTokens, Source: semantic.UsageProviderReported}
}
func renderUsage(usage *semantic.Usage) *openaiapi.Usage {
	if usage == nil {
		return nil
	}
	return &openaiapi.Usage{PromptTokens: usage.InputTokens, CompletionTokens: usage.OutputTokens, TotalTokens: usage.TotalTokens}
}
func normalizeTermination(value string) string {
	switch value {
	case "":
		return ""
	case "stop":
		return "complete"
	case "length":
		return "max_output"
	case "tool_calls":
		return "tool_call"
	case "content_filter":
		return "refusal"
	default:
		return "unknown"
	}
}
func renderTermination(value string) string {
	switch value {
	case "complete":
		return "stop"
	case "max_output":
		return "length"
	case "tool_call":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	case "unknown":
		return "unknown"
	default:
		return ""
	}
}
func cloneBool(v *bool) *bool {
	if v == nil {
		return nil
	}
	x := *v
	return &x
}
func cloneInt(v *int) *int {
	if v == nil {
		return nil
	}
	x := *v
	return &x
}
func cloneInt64(v *int64) *int64 {
	if v == nil {
		return nil
	}
	x := *v
	return &x
}
func cloneFloat(v *float64) *float64 {
	if v == nil {
		return nil
	}
	x := *v
	return &x
}
func cloneString(v *string) *string {
	if v == nil {
		return nil
	}
	x := *v
	return &x
}
func stringPointer(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}
