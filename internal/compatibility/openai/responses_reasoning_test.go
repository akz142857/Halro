package openai

import (
	"encoding/json"
	"testing"

	"github.com/akz142857/Halro/internal/openaiapi"
)

func TestResponsesStreamCarriesReasoningContent(t *testing.T) {
	var chunk openaiapi.ChatCompletionResponse
	if err := json.Unmarshal([]byte(`{"id":"chat_1","model":"model","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"summary"}}]}`), &chunk); err != nil {
		t.Fatal(err)
	}
	event, err := DecodeEvent(chunk)
	if err != nil {
		t.Fatal(err)
	}
	events, err := NewResponseStreamRenderer(openaiapi.ResponseRequest{}).Accept(event)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == "response.reasoning_summary_text.delta" && event.Delta == "summary" {
			return
		}
	}
	t.Fatal("reasoning summary did not reach caller")
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
