package anthropic

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/anthropicapi"
	"github.com/akz142857/Halro/internal/compatibility"
	"github.com/akz142857/Halro/internal/domain"
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

// output_config is the Anthropic spelling of two portable concepts, so it has to
// survive a round trip through the semantic model rather than being carried as
// an opaque native-only field.
func TestOutputConfigRoundTripsThroughTheSemanticModel(t *testing.T) {
	body := `{"model":"claude","max_tokens":64,` +
		`"output_config":{"effort":"xhigh","format":{"type":"json_schema","name":"invoice","schema":{"type":"object"}}},` +
		`"messages":[{"role":"user","content":"hi"}]}`
	request, err := anthropicapi.DecodeMessageRequest(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := DecodePortable(request)
	if err != nil {
		t.Fatal(err)
	}
	if canonical.ReasoningEffort != "xhigh" {
		t.Fatalf("effort lost: %q", canonical.ReasoningEffort)
	}
	if canonical.OutputFormat == nil || canonical.OutputFormat.Kind != semantic.OutputJSONSchema || canonical.OutputFormat.Name != "invoice" {
		t.Fatalf("output format lost: %#v", canonical.OutputFormat)
	}
	// The derived requirements are what routing filters on; without them a
	// structured-output request could land on a provider that cannot honour it.
	if !canonical.Requirements.StructuredJSON || !canonical.Requirements.Reasoning {
		t.Fatalf("requirements not derived: %#v", canonical.Requirements)
	}
	rendered, err := RenderPortableRequest(canonical, "claude-provider")
	if err != nil {
		t.Fatal(err)
	}
	if rendered.OutputConfig == nil || rendered.OutputConfig.Effort != "xhigh" {
		t.Fatalf("effort not rendered back: %#v", rendered.OutputConfig)
	}
	if !strings.Contains(string(rendered.OutputConfig.Format), `"type":"json_schema"`) {
		t.Fatalf("format not rendered back: %s", rendered.OutputConfig.Format)
	}
}

// json_object asks for unschema'd JSON, which Anthropic cannot express. Routing
// must be told so it picks another provider instead of the render failing late.
func TestJSONObjectOutputIsDeclaredUnsupportedForAnthropic(t *testing.T) {
	request := semantic.GenerateRequest{OutputFormat: &semantic.OutputFormat{Kind: semantic.OutputJSONObject}}
	unsupported := compatibility.UnsupportedGenerateFields(domain.ProfileAnthropicMessages, request)
	if !slices.Contains(unsupported, "response_format") {
		t.Fatalf("json_object should be declared unsupported, got %v", unsupported)
	}
	schema := semantic.GenerateRequest{OutputFormat: &semantic.OutputFormat{Kind: semantic.OutputJSONSchema}}
	if got := compatibility.UnsupportedGenerateFields(domain.ProfileAnthropicMessages, schema); slices.Contains(got, "response_format") {
		t.Fatalf("json_schema should route to Anthropic, got %v", got)
	}
}
