package semantic

import (
	"strings"
	"testing"

	"github.com/akz142857/Heimdall/internal/openaiapi"
)

func TestOpenAIChunkRoundTripPreservesSemanticChannels(t *testing.T) {
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

	event, err := FromOpenAIChunk(input)
	if err != nil {
		t.Fatal(err)
	}
	if event.Kind != KindDelta || event.Choices[0].Delta.ReasoningContent != "think" || len(event.Choices[0].Delta.ToolCalls) != 2 {
		t.Fatalf("semantic channels were not preserved: %#v", event)
	}
	output, err := event.OpenAIChunk()
	if err != nil {
		t.Fatal(err)
	}
	if output.Choices[0].Delta.ReasoningContent != "think" || string(output.Choices[0].Delta.Content) != `"hello"` ||
		output.Choices[0].Delta.ToolCalls[1].Function.Arguments != `{"tz":` || output.Usage.TotalTokens != 8 {
		t.Fatalf("round trip changed the chunk: %#v", output)
	}
}

func TestUsageOnlyChunk(t *testing.T) {
	event, err := FromOpenAIChunk(openaiapi.ChatCompletionResponse{
		ID: "chatcmpl-usage", Object: "chat.completion.chunk", Model: "chat",
		Usage: &openaiapi.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	if event.Kind != KindUsage || event.Usage == nil || len(event.Choices) != 0 {
		t.Fatalf("unexpected usage event: %#v", event)
	}
}

func TestFromOpenAIChunkRejectsInvalidBoundaries(t *testing.T) {
	index := 0
	base := func() openaiapi.ChatCompletionResponse {
		return openaiapi.ChatCompletionResponse{
			ID: "chatcmpl-1", Object: "chat.completion.chunk",
			Choices: []openaiapi.Choice{{Index: 0, Delta: &openaiapi.Message{Content: openaiapi.TextContent("ok")}}},
		}
	}
	tests := []struct {
		name   string
		mutate func(*openaiapi.ChatCompletionResponse)
	}{
		{"missing id", func(value *openaiapi.ChatCompletionResponse) { value.ID = "" }},
		{"duplicate choice", func(value *openaiapi.ChatCompletionResponse) { value.Choices = append(value.Choices, value.Choices[0]) }},
		{"missing delta", func(value *openaiapi.ChatCompletionResponse) { value.Choices[0].Delta = nil }},
		{"negative choice", func(value *openaiapi.ChatCompletionResponse) { value.Choices[0].Index = -1 }},
		{"negative tool", func(value *openaiapi.ChatCompletionResponse) {
			value.Choices[0].Delta.ToolCalls = []openaiapi.ToolCall{{Index: negativePointer()}}
		}},
		{"oversized tool", func(value *openaiapi.ChatCompletionResponse) {
			value.Choices[0].Delta.ToolCalls = []openaiapi.ToolCall{{Index: &index, Function: openaiapi.ToolCallFunction{Arguments: strings.Repeat("x", MaxToolArgumentBytes+1)}}}
		}},
		{"invalid usage", func(value *openaiapi.ChatCompletionResponse) {
			value.Usage = &openaiapi.Usage{PromptTokens: 5, CompletionTokens: 5, TotalTokens: 9}
		}},
		{"oversized event", func(value *openaiapi.ChatCompletionResponse) {
			value.Choices[0].Delta.ReasoningContent = strings.Repeat("x", MaxEncodedEventBytes)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := base()
			test.mutate(&value)
			if _, err := FromOpenAIChunk(value); err == nil {
				t.Fatal("invalid provider chunk was accepted")
			}
		})
	}
}

func TestValidateRejectsMalformedDirectEvent(t *testing.T) {
	event := Event{
		Kind: KindDelta, ID: "id", Object: "chat.completion.chunk",
		Choices: []Choice{{Index: 0, Delta: Delta{Content: []byte("not-json")}}},
	}
	if err := event.Validate(); err == nil {
		t.Fatal("invalid raw content was accepted")
	}
}

func negativePointer() *int {
	value := -1
	return &value
}
