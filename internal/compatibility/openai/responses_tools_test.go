package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

func TestNativeResponsesReasoningAndFunctionOutputCanBeReplayed(t *testing.T) {
	original := []openaiapi.ResponseOutputItem{{ID: "rs_upstream", Type: "reasoning", EncryptedContent: "cipher", Summary: []openaiapi.ResponseReasoningSummary{}}}
	part, err := ResponseReasoningContent(original)
	if err != nil {
		t.Fatal(err)
	}
	result := semantic.GenerateResult{ID: "response", Model: "alias", Translation: semantic.TranslationNone, MappingRevision: MappingRevision, Choices: []semantic.GenerateChoice{{Index: 0, Termination: "tool_call", Message: semantic.Message{Role: semantic.RoleAssistant, Content: []semantic.Content{part, {Kind: semantic.ContentToolCall, CallID: "call", Name: "ping", Arguments: `{}`}}}}}}
	response, err := RenderResponseResult(result, openaiapi.ResponseRequest{})
	if err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(response.Output)
	if err != nil {
		t.Fatal(err)
	}
	request, err := DecodeResponseGenerate(openaiapi.ResponseRequest{Model: "alias", Input: input, Reasoning: &openaiapi.ResponseReasoning{Effort: "max", Summary: "auto"}})
	if err != nil {
		t.Fatal(err)
	}
	wire, err := RenderProviderResponseRequest(request, "gpt-6.1-sol")
	if err != nil {
		t.Fatal(err)
	}
	var items []openaiapi.ResponseInputItem
	if err := json.Unmarshal(wire.Input, &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].ID != "rs_upstream" || items[0].EncryptedContent != "cipher" || wire.Reasoning.Summary != "auto" {
		t.Fatalf("native replay lost state: %s", wire.Input)
	}
}

func TestResponsesRendererStreamsParallelToolsAndReasoning(t *testing.T) {
	reasoning, err := ResponseReasoningContent([]openaiapi.ResponseOutputItem{{Type: "reasoning", ID: "rs", EncryptedContent: "cipher"}})
	if err != nil {
		t.Fatal(err)
	}
	renderer := NewResponseStreamRenderer(openaiapi.ResponseRequest{})
	zero, two := 0, 2
	input := []semantic.Event{
		{Kind: semantic.EventDelta, Outputs: []semantic.OutputDelta{{Index: 0, Content: []semantic.ContentDelta{{Kind: semantic.ContentReasoning, Text: "plan"}}}}},
		{Kind: semantic.EventDelta, Outputs: []semantic.OutputDelta{{Index: 0, Content: []semantic.ContentDelta{{Kind: semantic.ContentToolCall, ToolIndex: &two, CallID: "c2", Name: "second", ArgumentsFragment: `{"a":`}, {Kind: semantic.ContentToolCall, ToolIndex: &zero, CallID: "c0", Name: "first", ArgumentsFragment: `{}`}}}}},
		{Kind: semantic.EventDelta, Outputs: []semantic.OutputDelta{{Index: 0, Content: []semantic.ContentDelta{{Kind: semantic.ContentToolCall, ToolIndex: &two, ArgumentsFragment: `1}`}, {Kind: semantic.ContentReasoning, EncryptedContent: reasoning.EncryptedContent}}, Termination: "tool_call"}}},
		{Kind: semantic.EventUsage, Usage: &semantic.Usage{InputTokens: 100, CachedInputTokens: 20, CacheWriteInputTokens: 30, OutputTokens: 10, TotalTokens: 110, Source: semantic.UsageProviderReported}},
	}
	var events []openaiapi.ResponseStreamEvent
	for _, event := range input {
		event.ID, event.Model = "resp", "alias"
		output, err := renderer.Accept(event)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, output...)
	}
	output, err := renderer.Complete()
	if err != nil {
		t.Fatal(err)
	}
	events = append(events, output...)
	for index, event := range events {
		if event.SequenceNumber != int64(index) {
			t.Fatal("non-monotonic sequence")
		}
	}
	final := events[len(events)-1].Response
	if len(final.Output) != 3 || final.Output[0].EncryptedContent != reasoning.EncryptedContent || final.Output[1].CallID != "c2" || final.Output[1].Arguments != `{"a":1}` || final.Usage.InputTokensDetails.CacheWriteTokens != 30 {
		t.Fatalf("final Responses output lost content: %+v", final)
	}
}

func TestReasoningReplayRejectsOtherProvidersAndInvalidEnvelope(t *testing.T) {
	for _, opaque := range []string{"ark-cipher", responseReasoningPrefix + "invalid", responseReasoningPrefix + strings.Repeat("a", 64<<10)} {
		if _, err := ResponseReasoningItems(semantic.Content{Kind: semantic.ContentReasoning, EncryptedContent: opaque}); err == nil {
			t.Fatal("invalid reasoning replay accepted")
		}
	}
}

func TestInterleavedReasoningItemsKeepTheirToolCallOrder(t *testing.T) {
	response := openaiapi.Response{ID: "resp", Model: "model", Status: "completed", Output: []openaiapi.ResponseOutputItem{
		{ID: "rs1", Type: "reasoning", EncryptedContent: "cipher1"},
		{ID: "fc1", Type: "function_call", CallID: "c1", Name: "one", Arguments: `{}`},
		{ID: "rs2", Type: "reasoning", EncryptedContent: "cipher2"},
		{ID: "fc2", Type: "function_call", CallID: "c2", Name: "two", Arguments: `{}`},
	}}
	result, err := DecodeOpenAIProviderResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	chat, err := RenderGenerateResult(result)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := DecodeGenerate(openaiapi.ChatCompletionRequest{Model: "model", Messages: []openaiapi.Message{*chat.Choices[0].Message}})
	if err != nil {
		t.Fatal(err)
	}
	wire, err := RenderProviderResponseRequest(canonical, "model")
	if err != nil {
		t.Fatal(err)
	}
	var items []openaiapi.ResponseInputItem
	if err := json.Unmarshal(wire.Input, &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 || items[0].ID != "rs1" || items[1].CallID != "c1" || items[2].ID != "rs2" || items[3].CallID != "c2" {
		t.Fatalf("interleaved reasoning moved: %s", wire.Input)
	}
	// Native Responses clients replay the same state as separate output items.
	native, err := RenderResponseResult(result, openaiapi.ResponseRequest{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(native.Output)
	canonical, err = DecodeResponseGenerate(openaiapi.ResponseRequest{Model: "model", Input: raw})
	if err != nil {
		t.Fatal(err)
	}
	wire, err = RenderProviderResponseRequest(canonical, "model")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(wire.Input, &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 || items[0].ID != "rs1" || items[2].ID != "rs2" {
		t.Fatalf("native interleaving lost: %s", wire.Input)
	}
}

func TestStrictToolSettingsSurviveChatAndResponsesMapping(t *testing.T) {
	for _, strict := range []*bool{nil, new(bool), func() *bool { v := true; return &v }()} {
		native, err := DecodeResponseGenerate(openaiapi.ResponseRequest{Model: "m", Input: json.RawMessage(`"hello"`), Tools: []openaiapi.ResponseTool{{Type: "function", Name: "f", Parameters: json.RawMessage(`{"type":"object"}`), Strict: strict}}})
		if err != nil {
			t.Fatal(err)
		}
		wire, err := RenderProviderResponseRequest(native, "m")
		if err != nil {
			t.Fatal(err)
		}
		got := wire.Tools[0].Strict
		if (got == nil) != (strict == nil) || (got != nil && *got != *strict) {
			t.Fatal("strict setting changed")
		}
	}
}

func TestResponsesRefusalKeepsItsWireType(t *testing.T) {
	result, err := DecodeOpenAIProviderResponse(openaiapi.Response{ID: "resp", Model: "m", Status: "completed", Output: []openaiapi.ResponseOutputItem{{ID: "msg", Type: "message", Role: "assistant", Content: []openaiapi.ResponseOutputContent{{Type: "refusal", Refusal: "Cannot help."}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Choices[0].Termination != "refusal" {
		t.Fatal("refusal lost its semantic type")
	}
	renderer := NewResponseStreamRenderer(openaiapi.ResponseRequest{})
	events, err := renderer.Accept(semantic.Event{Kind: semantic.EventDelta, ID: "resp", Model: "m", Outputs: []semantic.OutputDelta{{Index: 0, Termination: "refusal", Content: []semantic.ContentDelta{{Kind: semantic.ContentText, Text: "Cannot help."}}}}})
	if err != nil {
		t.Fatal(err)
	}
	sawDelta := false
	for _, event := range events {
		if event.Type == "response.refusal.delta" {
			sawDelta = true
		}
	}
	end, err := renderer.Complete()
	if err != nil {
		t.Fatal(err)
	}
	if !sawDelta || end[len(end)-1].Response.Output[0].Content[0].Refusal != "Cannot help." {
		t.Fatal("refusal became output_text")
	}
}

func TestOpaqueReplayCannotBypassReadableSummaryRedaction(t *testing.T) {
	part, err := ResponseReasoningContent([]openaiapi.ResponseOutputItem{{Type: "reasoning", ID: "rs_original", EncryptedContent: "cipher", Summary: []openaiapi.ResponseReasoningSummary{{Type: "summary_text", Text: "sensitive-summary-marker"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if part.Text != "sensitive-summary-marker" {
		t.Fatal("readable summary never reached the redaction path")
	}
	// Simulate the normal outbound redactor replacing the readable content.
	part.Text = "[REDACTED]"
	replay, err := ResponseReasoningItems(part)
	if err != nil {
		t.Fatal(err)
	}
	if len(replay[0].Summary) != 0 || replay[0].EncryptedContent != "cipher" {
		t.Fatal("opaque envelope retained a readable summary or lost authenticated state")
	}
	output, err := renderResponseOutput("resp", semantic.Message{Role: semantic.RoleAssistant, Content: []semantic.Content{part}}, "complete")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(output)
	if strings.Contains(string(encoded), "sensitive-summary-marker") || output[0].Summary[0].Text != "[REDACTED]" {
		t.Fatal("summary escaped redaction")
	}
}

func TestResponsesRejectsMisplacedReasoningFields(t *testing.T) {
	for _, input := range []string{
		`[{"type":"function_call","call_id":"call","name":"f","arguments":"{}","encrypted_content":"opaque"}]`,
		`[{"type":"message","role":"user","content":"hi","summary":[]}]`,
	} {
		if _, err := DecodeResponseGenerate(openaiapi.ResponseRequest{Model: "m", Input: json.RawMessage(input)}); err == nil {
			t.Fatal("misplaced reasoning fields were dropped")
		}
	}
}
