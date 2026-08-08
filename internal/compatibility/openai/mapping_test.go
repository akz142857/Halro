package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

func TestEventRoundTripPreservesSemanticChannels(t *testing.T) {
	finish := "tool_calls"
	toolZero, toolOne := 0, 1
	input := openaiapi.ChatCompletionResponse{
		ID: "chatcmpl-1", Object: "chat.completion.chunk", Created: 42, Model: "chat",
		Choices: []openaiapi.Choice{
			{Index: 1, Delta: &openaiapi.Message{Role: "assistant", Content: openaiapi.TextContent("hello"), ReasoningContent: "think", ToolCalls: []openaiapi.ToolCall{
				{Index: &toolZero, ID: "call_0", Type: "function", Function: openaiapi.ToolCallFunction{Name: "weather", Arguments: "{\"city\":"}},
				{Index: &toolOne, ID: "call_1", Type: "function", Function: openaiapi.ToolCallFunction{Name: "clock", Arguments: "{\"tz\":"}},
			}}, FinishReason: &finish},
			{Index: 0, Delta: &openaiapi.Message{Content: openaiapi.TextContent("parallel")}},
		},
		Usage: &openaiapi.Usage{PromptTokens: 3, CompletionTokens: 5, TotalTokens: 8},
	}

	event, err := DecodeEvent(input)
	if err != nil {
		t.Fatal(err)
	}
	if event.MappingRevision != MappingRevision || event.Translation != semantic.TranslationNone {
		t.Fatalf("missing event audit metadata: %#v", event)
	}
	event.MappingRevision = 7
	event.Translation = semantic.TranslationDeclared
	output, err := RenderEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	if output.Choices[0].Delta.ReasoningContent != "think" || string(output.Choices[0].Delta.Content) != `"hello"` || output.Choices[0].Delta.ToolCalls[1].Function.Arguments != `{"tz":` || output.Usage.TotalTokens != 8 {
		t.Fatalf("round trip changed the chunk: %#v", output)
	}
	roundTrip, err := DecodeEvent(output)
	if err != nil {
		t.Fatal(err)
	}
	if roundTrip.MappingRevision != 7 || roundTrip.Translation != semantic.TranslationDeclared {
		t.Fatalf("audit metadata was not preserved: %#v", roundTrip)
	}
	wire, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "Semantic") || strings.Contains(string(wire), "mapping_revision") || strings.Contains(string(wire), "translation") {
		t.Fatalf("internal audit metadata leaked onto the wire: %s", wire)
	}
}

func TestToolCallArgumentsRemainOpaqueIncludingEmpty(t *testing.T) {
	request := openaiapi.ChatCompletionRequest{
		Model: "public",
		Messages: []openaiapi.Message{{
			Role: "assistant",
			ToolCalls: []openaiapi.ToolCall{
				{ID: "call_1", Type: "function", Function: openaiapi.ToolCallFunction{Name: "lookup", Arguments: ""}},
				{ID: "call_2", Type: "function", Function: openaiapi.ToolCallFunction{Name: "raw", Arguments: "not-json"}},
			},
		}},
	}
	canonical, err := DecodeGenerate(request)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := RenderGenerateRequest(canonical, "provider")
	if err != nil {
		t.Fatal(err)
	}
	if rendered.Messages[0].ToolCalls[0].Function.Arguments != "" || rendered.Messages[0].ToolCalls[1].Function.Arguments != "not-json" {
		t.Fatalf("opaque arguments changed: %#v", rendered.Messages[0].ToolCalls)
	}
}

func TestDecodeEventRejectsInvalidBoundaries(t *testing.T) {
	index := 0
	base := func() openaiapi.ChatCompletionResponse {
		return openaiapi.ChatCompletionResponse{ID: "id", Object: "chat.completion.chunk", Choices: []openaiapi.Choice{{Index: 0, Delta: &openaiapi.Message{Content: openaiapi.TextContent("ok")}}}}
	}
	tests := []struct {
		name   string
		mutate func(*openaiapi.ChatCompletionResponse)
	}{
		{"missing id", func(v *openaiapi.ChatCompletionResponse) { v.ID = "" }},
		{"duplicate choice", func(v *openaiapi.ChatCompletionResponse) { v.Choices = append(v.Choices, v.Choices[0]) }},
		{"missing delta", func(v *openaiapi.ChatCompletionResponse) { v.Choices[0].Delta = nil }},
		{"negative choice", func(v *openaiapi.ChatCompletionResponse) { v.Choices[0].Index = -1 }},
		{"negative tool", func(v *openaiapi.ChatCompletionResponse) {
			negative := -1
			v.Choices[0].Delta.ToolCalls = []openaiapi.ToolCall{{Index: &negative}}
		}},
		{"oversized tool", func(v *openaiapi.ChatCompletionResponse) {
			v.Choices[0].Delta.ToolCalls = []openaiapi.ToolCall{{Index: &index, Function: openaiapi.ToolCallFunction{Arguments: strings.Repeat("x", semantic.MaxToolArgumentBytes+1)}}}
		}},
		{"invalid usage", func(v *openaiapi.ChatCompletionResponse) {
			v.Usage = &openaiapi.Usage{PromptTokens: 5, CompletionTokens: 5, TotalTokens: 9}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			v := base()
			test.mutate(&v)
			if _, err := DecodeEvent(v); err == nil {
				t.Fatal("invalid chunk accepted")
			}
		})
	}
}

func TestGenerateRequestRoundTripUsesCanonicalFields(t *testing.T) {
	parallel := true
	request := openaiapi.ChatCompletionRequest{Model: "public", Messages: []openaiapi.Message{{Role: "developer", Content: openaiapi.TextContent("policy")}, {Role: "user", Content: []byte(`[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"https://example.test/a.png","detail":"low"}}]`)}}, Tools: []openaiapi.Tool{{Type: "function", Function: openaiapi.ToolFunction{Name: "inspect", Parameters: []byte(`{"type":"object"}`)}}}, ParallelToolCalls: &parallel, ResponseFormat: []byte(`{"type":"json_object"}`)}
	canonical, err := DecodeGenerate(request)
	if err != nil {
		t.Fatal(err)
	}
	if !canonical.Requirements.DeveloperRole || !canonical.Requirements.InputImage || !canonical.Requirements.Tools || !canonical.Requirements.StructuredJSON {
		t.Fatalf("requirements=%#v", canonical.Requirements)
	}
	rendered, err := RenderGenerateRequest(canonical, "provider-model")
	if err != nil {
		t.Fatal(err)
	}
	if rendered.Model != "provider-model" || len(rendered.Messages) != 2 || len(rendered.Tools) != 1 {
		t.Fatalf("rendered=%#v", rendered)
	}
}

func TestTextOutputDoesNotRequireStructuredJSON(t *testing.T) {
	parallel := true
	request, err := DecodeGenerate(openaiapi.ChatCompletionRequest{Model: "public", Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hello")}}, ParallelToolCalls: &parallel, ResponseFormat: json.RawMessage(`{"type":"text"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if request.Requirements.StructuredJSON {
		t.Fatal("plain text output incorrectly requires structured JSON")
	}
	if !request.Requirements.Tools || !request.Requirements.ParallelTools {
		t.Fatal("parallel tool requirement did not imply tool capability")
	}
}

func TestToolResultArrayAndNilFinishReasonRoundTrip(t *testing.T) {
	request := openaiapi.ChatCompletionRequest{Model: "public", Messages: []openaiapi.Message{{Role: "tool", ToolCallID: "call_1", Content: json.RawMessage(`[{"type":"text","text":"first"},{"type":"text","text":"second"}]`)}}}
	canonical, err := DecodeGenerate(request)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := RenderGenerateRequest(canonical, "provider")
	if err != nil {
		t.Fatal(err)
	}
	if rendered.Messages[0].ToolCallID != "call_1" || string(rendered.Messages[0].Content) != `[{"text":"first","type":"text"},{"text":"second","type":"text"}]` {
		t.Fatalf("tool result changed: %#v", rendered.Messages[0])
	}
	result, err := DecodeGenerateResult(openaiapi.ChatCompletionResponse{ID: "id", Model: "model", Choices: []openaiapi.Choice{{Index: 0, Message: &openaiapi.Message{Role: "assistant"}}}})
	if err != nil {
		t.Fatal(err)
	}
	wire, err := RenderGenerateResult(result)
	if err != nil {
		t.Fatal(err)
	}
	if wire.Choices[0].FinishReason != nil {
		t.Fatalf("nil finish reason changed to %q", *wire.Choices[0].FinishReason)
	}
}

func TestTokenLimitAliasesAndJSONSchemaMetadataRoundTrip(t *testing.T) {
	legacy, completion := int64(12), int64(24)
	request := openaiapi.ChatCompletionRequest{Model: "public", Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hello")}}, MaxTokens: &legacy, MaxCompletionTokens: &completion, ResponseFormat: json.RawMessage(`{"type":"json_schema","json_schema":{"name":"answer","description":"structured answer","schema":{"type":"object"},"strict":true}}`)}
	canonical, err := DecodeGenerate(request)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := RenderGenerateRequest(canonical, "provider")
	if err != nil {
		t.Fatal(err)
	}
	if rendered.MaxTokens == nil || *rendered.MaxTokens != legacy || rendered.MaxCompletionTokens == nil || *rendered.MaxCompletionTokens != completion {
		t.Fatalf("token aliases changed: %#v", rendered)
	}
	if !strings.Contains(string(rendered.ResponseFormat), `"description":"structured answer"`) {
		t.Fatalf("json schema description lost: %s", rendered.ResponseFormat)
	}
	request.ResponseFormat = json.RawMessage(`{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object"},"unknown":true}}`)
	if _, err := DecodeGenerate(request); err == nil {
		t.Fatal("unknown response_format field was silently dropped")
	}
}
