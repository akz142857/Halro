package anthropic

import (
	"testing"

	"github.com/akz142857/Halro/internal/semantic"
)

func TestStreamRendererEmitsAnthropicLifecycle(t *testing.T) {
	renderer := NewStreamRenderer("public")
	events, err := renderer.Accept(semantic.Event{Kind: semantic.EventDelta, ID: "chatcmpl-1", Model: "provider", Outputs: []semantic.OutputDelta{{Index: 0, Role: semantic.RoleAssistant, Content: []semantic.ContentDelta{{Kind: semantic.ContentText, Text: "hi"}}}}, Translation: semantic.TranslationNone, MappingRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Type != "message_start" || events[1].Type != "content_block_start" || events[2].Type != "content_block_delta" {
		t.Fatalf("unexpected start events: %#v", events)
	}
	_, err = renderer.Accept(semantic.Event{Kind: semantic.EventDelta, ID: "chatcmpl-1", Model: "provider", Outputs: []semantic.OutputDelta{{Index: 0, Termination: "stop"}}, Translation: semantic.TranslationNone, MappingRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	final, err := renderer.Accept(semantic.Event{Kind: semantic.EventUsage, ID: "chatcmpl-1", Model: "provider", Usage: &semantic.Usage{InputTokens: 2, OutputTokens: 1, TotalTokens: 3, Source: semantic.UsageProviderReported}, Translation: semantic.TranslationNone, MappingRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(final) != 3 || final[0].Type != "content_block_stop" || final[1].Type != "message_delta" || final[2].Type != "message_stop" {
		t.Fatalf("unexpected final events: %#v", final)
	}
}

func TestStreamRendererMapsToolArgumentDeltas(t *testing.T) {
	renderer := NewStreamRenderer("public")
	toolIndex := 0
	events, err := renderer.Accept(semantic.Event{Kind: semantic.EventDelta, ID: "chatcmpl-2", Model: "provider", Outputs: []semantic.OutputDelta{{Index: 0, Role: semantic.RoleAssistant, Content: []semantic.ContentDelta{{Kind: semantic.ContentToolCall, ToolIndex: &toolIndex, CallID: "call_1", Name: "lookup", ArgumentsFragment: `{"q"`}}}}, Translation: semantic.TranslationNone, MappingRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[2].Type != "content_block_delta" {
		t.Fatalf("unexpected events: %#v", events)
	}
}
