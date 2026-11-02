package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/openaiapi"
)

func TestEncryptedReasoningSurvivesRequestResponseAndStandaloneStreamChunk(t *testing.T) {
	opaque := "ciphertext+/=byte-exact"
	message := openaiapi.Message{Role: "assistant", ReasoningContent: "summary", EncryptedContent: opaque, ToolCalls: []openaiapi.ToolCall{{ID: "call", Type: "function", Function: openaiapi.ToolCallFunction{Name: "echo", Arguments: `{}`}}}}
	request := openaiapi.ChatCompletionRequest{Model: "alias", Messages: []openaiapi.Message{message, {Role: "tool", ToolCallID: "call", Content: json.RawMessage(`"pong"`)}}}
	canonical, err := DecodeGenerate(request)
	if err != nil {
		t.Fatal(err)
	}
	if !canonical.Requirements.Reasoning {
		t.Fatal("opaque history did not require a reasoning-capable route")
	}
	rendered, err := RenderGenerateRequest(canonical, "upstream")
	if err != nil || rendered.Messages[0].EncryptedContent != opaque {
		t.Fatalf("replay state lost: %v", err)
	}
	response := openaiapi.ChatCompletionResponse{ID: "response", Object: "chat.completion", Model: "upstream", Choices: []openaiapi.Choice{{Index: 0, Message: &message}}}
	result, err := DecodeGenerateResult(response)
	if err != nil {
		t.Fatal(err)
	}
	output, err := RenderGenerateResult(result)
	if err != nil || output.Choices[0].Message.EncryptedContent != opaque {
		t.Fatalf("response state lost: %v", err)
	}
	chunk := openaiapi.ChatCompletionResponse{ID: "stream", Model: "upstream", Choices: []openaiapi.Choice{{Index: 0, Delta: &openaiapi.Message{EncryptedContent: opaque}}}}
	event, err := DecodeEvent(chunk)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := RenderEvent(event)
	if err != nil || encoded.Choices[0].Delta.EncryptedContent != opaque {
		t.Fatalf("standalone encrypted chunk lost: %v", err)
	}
}

func TestEncryptedReasoningRejectsInvalidRolesAndOversizedState(t *testing.T) {
	for _, message := range []openaiapi.Message{
		{Role: "user", Content: openaiapi.TextContent("hi"), EncryptedContent: "opaque"},
		{Role: "assistant", EncryptedContent: strings.Repeat("x", (64<<10)+1)},
	} {
		request := openaiapi.ChatCompletionRequest{Model: "model", Messages: []openaiapi.Message{message}}
		if request.Validate() == nil {
			t.Fatal("wire accepted invalid opaque reasoning state")
		}
		if _, err := DecodeGenerate(request); err == nil {
			t.Fatal("semantic mapping accepted invalid opaque reasoning state")
		}
	}
}
