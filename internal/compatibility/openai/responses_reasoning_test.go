package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/openaiapi"
)

// This is the constraint that makes the DeepSeek render rule necessary, kept
// where the constraint lives.
//
// Phase 1A Responses models output text and nothing else. A provider stream
// carrying reasoning content therefore kills the request — and the caller could
// not have avoided it, because /v1/responses rejects the reasoning request field
// outright. DeepSeek reached this by thinking on an unasked request, which is
// its documented default; the fix is on the request side
// (compatibility.RenderDeepSeekChatRequest switches thinking off when nobody
// asked), not by teaching this renderer to drop content the upstream produced
// and billed.
func TestResponsesStreamRefusesReasoningContentAndSaysWhat(t *testing.T) {
	var chunk openaiapi.ChatCompletionResponse
	if err := json.Unmarshal([]byte(`{
		"id":"chat_1","object":"chat.completion.chunk","created":1,"model":"deepseek-v4-flash",
		"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"let me think"},"finish_reason":null}]
	}`), &chunk); err != nil {
		t.Fatal(err)
	}
	event, err := DecodeEvent(chunk)
	if err != nil {
		t.Fatal(err)
	}
	renderer := NewResponseStreamRenderer(openaiapi.ResponseRequest{Model: "chat", Input: json.RawMessage(`"hi"`), Stream: true})
	_, err = renderer.Accept(event)
	if err == nil {
		t.Fatal("the Phase 1A renderer accepted content it cannot represent")
	}
	// The kind belongs in the message: without it the operator cannot tell a
	// thinking model from a tool call or a malformed chunk, and this failure
	// arrives as a bare provider_error on the client side.
	if !strings.Contains(err.Error(), "reasoning") {
		t.Fatalf("the refusal does not name what it saw: %v", err)
	}
}

// Output text still streams, so the guard above is not simply refusing
// everything.
func TestResponsesStreamAcceptsOutputText(t *testing.T) {
	var chunk openaiapi.ChatCompletionResponse
	if err := json.Unmarshal([]byte(`{
		"id":"chat_1","object":"chat.completion.chunk","created":1,"model":"deepseek-v4-flash",
		"choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]
	}`), &chunk); err != nil {
		t.Fatal(err)
	}
	event, err := DecodeEvent(chunk)
	if err != nil {
		t.Fatal(err)
	}
	renderer := NewResponseStreamRenderer(openaiapi.ResponseRequest{Model: "chat", Input: json.RawMessage(`"hi"`), Stream: true})
	events, err := renderer.Accept(event)
	if err != nil {
		t.Fatal(err)
	}
	var sawDelta bool
	for _, emitted := range events {
		if emitted.Type == "response.output_text.delta" && emitted.Delta == "ok" {
			sawDelta = true
		}
	}
	if !sawDelta {
		t.Fatalf("output text did not reach the client: %#v", events)
	}
}
