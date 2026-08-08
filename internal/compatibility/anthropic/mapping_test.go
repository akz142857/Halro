package anthropic

import (
	"bytes"
	"testing"

	"github.com/akz142857/Halro/internal/anthropicapi"
	"github.com/akz142857/Halro/internal/semantic"
)

func TestDecodePortableMapsClientTools(t *testing.T) {
	request, err := anthropicapi.DecodeMessageRequest(bytes.NewBufferString(`{"model":"portable","max_tokens":128,"system":"be concise","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":"x"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"lookup","disable_parallel_tool_use":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := DecodePortable(request)
	if err != nil {
		t.Fatal(err)
	}
	if canonical.ToolChoice == nil || canonical.ToolChoice.Mode != semantic.ToolChoiceNamed || canonical.ToolChoice.Name != "lookup" {
		t.Fatalf("unexpected tool choice: %#v", canonical.ToolChoice)
	}
	if canonical.ParallelTools == nil || *canonical.ParallelTools {
		t.Fatal("parallel disable was not preserved")
	}
	if len(canonical.Messages) != 3 || canonical.Messages[2].Role != semantic.RoleTool {
		t.Fatalf("unexpected messages: %#v", canonical.Messages)
	}
}

func TestDecodePortableRejectsSignedThinking(t *testing.T) {
	request, err := anthropicapi.DecodeMessageRequest(bytes.NewBufferString(`{"model":"portable","max_tokens":128,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":"sig"}]},{"role":"user","content":"continue"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePortable(request); err == nil {
		t.Fatal("expected native-mode requirement")
	}
}

func TestRenderResultMapsToolUseAndStopReason(t *testing.T) {
	result := semantic.GenerateResult{ID: "chatcmpl-1", Model: "upstream", Choices: []semantic.GenerateChoice{{Index: 0, Message: semantic.Message{Role: semantic.RoleAssistant, Content: []semantic.Content{{Kind: semantic.ContentToolCall, CallID: "call_1", Name: "lookup", Arguments: `{"q":"x"}`}}}, Termination: "tool_calls"}}, Usage: &semantic.Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5, Source: semantic.UsageProviderReported}, Translation: semantic.TranslationNone, MappingRevision: 1}
	message, err := RenderResult(result, "public")
	if err != nil {
		t.Fatal(err)
	}
	if message.ID != "msg_1" || message.StopReason == nil || *message.StopReason != "tool_use" || message.Content[0].Type != "tool_use" {
		t.Fatalf("unexpected message: %#v", message)
	}
}
