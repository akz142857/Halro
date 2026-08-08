package openai

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

func TestDecodeResponseGenerateMapsPortableItems(t *testing.T) {
	request := openaiapi.ResponseRequest{
		Model: "route", Instructions: "be concise",
		Input: json.RawMessage(`[
			{"role":"user","content":[{"type":"input_text","text":"hello"}]},
			{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"id\":1}"},
			{"type":"function_call_output","call_id":"call_1","output":"ok"}
		]`),
		Tools:      []openaiapi.ResponseTool{{Type: "function", Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}},
		ToolChoice: json.RawMessage(`{"type":"function","name":"lookup"}`),
	}
	canonical, err := DecodeResponseGenerate(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(canonical.Messages) != 4 || canonical.Messages[0].Role != semantic.RoleDeveloper || canonical.Messages[2].Content[0].Kind != semantic.ContentToolCall || canonical.Messages[3].Content[0].Kind != semantic.ContentToolResult {
		t.Fatalf("unexpected canonical request: %#v", canonical)
	}
	if canonical.ToolChoice == nil || canonical.ToolChoice.Mode != semantic.ToolChoiceNamed || canonical.ToolChoice.Name != "lookup" {
		t.Fatalf("unexpected tool choice: %#v", canonical.ToolChoice)
	}
}

func TestRenderResponseResultProducesSDKOutputTextAndFunctionCall(t *testing.T) {
	result := semantic.GenerateResult{
		ID: "chatcmpl-abc", Created: 42, Model: "route", Translation: semantic.TranslationNone, MappingRevision: 1,
		Choices: []semantic.GenerateChoice{{Index: 0, Termination: "tool_call", Message: semantic.Message{Role: semantic.RoleAssistant, Content: []semantic.Content{
			{Kind: semantic.ContentText, Text: "hello"},
			{Kind: semantic.ContentToolCall, CallID: "call_1", Name: "lookup", Arguments: `{"id":1}`},
		}}}},
		Usage: &semantic.Usage{InputTokens: 3, OutputTokens: 2, TotalTokens: 5, Source: semantic.UsageProviderReported},
	}
	response, err := RenderResponseResult(result, openaiapi.ResponseRequest{
		Model: "route", Input: json.RawMessage(`"hello"`), Instructions: "secret instructions",
		Tools:      []openaiapi.ResponseTool{{Type: "function", Name: "secret_tool", Description: "secret description"}},
		ToolChoice: json.RawMessage(`{"type":"function","name":"secret_tool"}`),
		Text:       &openaiapi.ResponseTextConfig{Format: openaiapi.ResponseTextFormat{Type: "json_schema", Name: "secret_schema", Schema: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.ID != "resp_abc" || response.Status != "completed" || len(response.Output) != 2 || response.Output[0].Content[0].Text != "hello" || response.Output[1].Type != "function_call" || response.Usage.TotalTokens != 5 {
		t.Fatalf("unexpected response: %#v", response)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret instructions", "secret_tool", "secret description", "secret_schema"} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("response metadata echoed unredacted request material %q: %s", secret, encoded)
		}
	}
}

func TestResponseStreamRendererEmitsOrderedLifecycle(t *testing.T) {
	request := openaiapi.ResponseRequest{Model: "route", Input: json.RawMessage(`"hello"`), Stream: true}
	renderer := NewResponseStreamRenderer(request)
	events, err := renderer.Accept(semantic.Event{Kind: semantic.EventDelta, ID: "chunk_1", Created: 42, Model: "route", Translation: semantic.TranslationNone, MappingRevision: 1, Outputs: []semantic.OutputDelta{{Index: 0, Role: semantic.RoleAssistant, Content: []semantic.ContentDelta{{Kind: semantic.ContentText, Text: "hello"}}, Termination: "complete"}}})
	if err != nil {
		t.Fatal(err)
	}
	usageEvents, err := renderer.Accept(semantic.Event{Kind: semantic.EventUsage, ID: "chunk_1", Created: 42, Model: "route", Translation: semantic.TranslationNone, MappingRevision: 1, Usage: &semantic.Usage{InputTokens: 3, OutputTokens: 2, TotalTokens: 5, Source: semantic.UsageProviderReported}})
	if err != nil {
		t.Fatal(err)
	}
	events = append(events, usageEvents...)
	completed, err := renderer.Complete()
	if err != nil {
		t.Fatal(err)
	}
	events = append(events, completed...)
	want := []string{"response.created", "response.in_progress", "response.output_item.added", "response.content_part.added", "response.output_text.delta", "response.output_text.done", "response.content_part.done", "response.output_item.done", "response.completed"}
	if len(events) != len(want) {
		t.Fatalf("events=%d want=%d: %#v", len(events), len(want), events)
	}
	for index, event := range events {
		if event.Type != want[index] || event.SequenceNumber != int64(index) {
			t.Fatalf("event[%d]=%#v want=%q", index, event, want[index])
		}
	}
	if events[len(events)-1].Response == nil || events[len(events)-1].Response.Usage.TotalTokens != 5 {
		t.Fatal("completed event omitted final usage")
	}
}

func TestRenderProviderResponseRequestIsStatelessAndRejectsDroppedControls(t *testing.T) {
	request := semantic.GenerateRequest{Operation: semantic.OperationGenerate, Source: semantic.Source{ProfileID: "test", ProfileRevision: 1}, Mode: semantic.ModePortable, RequestedModel: "public", Messages: []semantic.Message{{Role: semantic.RoleUser, Content: []semantic.Content{{Kind: semantic.ContentText, Text: "hi"}}}}}
	rendered, err := RenderProviderResponseRequest(request, "amazon.nova-pro")
	if err != nil {
		t.Fatal(err)
	}
	if rendered.Store == nil || *rendered.Store || rendered.Model != "amazon.nova-pro" || !bytes.Contains(rendered.Input, []byte(`"hi"`)) {
		t.Fatalf("unsafe provider request: %#v", rendered)
	}
	seed := int64(1)
	request.Seed = &seed
	if _, err := RenderProviderResponseRequest(request, "amazon.nova-pro"); err == nil {
		t.Fatal("seed would have been silently dropped")
	}
	request.Seed = nil
	request.Stop = []string{"stop"}
	if _, err := RenderProviderResponseRequest(request, "amazon.nova-pro"); err == nil {
		t.Fatal("stop sequences would have been silently dropped")
	}
}
