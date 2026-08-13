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
	result := semantic.GenerateResult{ID: "chatcmpl-1", Model: "upstream", Choices: []semantic.GenerateChoice{{Index: 0, Message: semantic.Message{Role: semantic.RoleAssistant, Content: []semantic.Content{{Kind: semantic.ContentToolCall, CallID: "call_1", Name: "lookup", Arguments: `{"q":"x"}`}}}, Termination: "tool_call"}}, Usage: &semantic.Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5, Source: semantic.UsageProviderReported}, Translation: semantic.TranslationNone, MappingRevision: 1}
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
	schema := semantic.GenerateRequest{OutputFormat: &semantic.OutputFormat{Kind: semantic.OutputJSONSchema, Name: "result", Strict: true}}
	if got := compatibility.UnsupportedGenerateFields(domain.ProfileAnthropicMessages, schema); slices.Contains(got, "response_format") {
		t.Fatalf("json_schema should route to Anthropic, got %v", got)
	}
}

// Anthropic enforces any schema it is given, so a caller who asked for a relaxed
// one must be routed elsewhere rather than silently upgraded to stricter
// behaviour than they requested — and that has to be declared, or the request
// fails at render time after its budget is already reserved.
func TestNonStrictSchemaIsDeclaredUnsupportedForAnthropic(t *testing.T) {
	request := semantic.GenerateRequest{OutputFormat: &semantic.OutputFormat{Kind: semantic.OutputJSONSchema, Name: "result", Strict: false}}
	if got := compatibility.UnsupportedGenerateFields(domain.ProfileAnthropicMessages, request); !slices.Contains(got, "response_format") {
		t.Fatalf("want response_format declared unsupported, got %v", got)
	}
}

// Anthropic treats the schema name as optional; every portable target requires
// it. Saying so at the wire layer is the difference between an error that names
// the field and the "request is not portable" the semantic layer produces two
// steps later, which points the caller at nothing they can act on.
func TestNamelessSchemaIsRefusedWhereTheFieldCanBeNamed(t *testing.T) {
	request, err := anthropicapi.DecodeMessageRequest(strings.NewReader(
		`{"model":"m","max_tokens":10,"output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}},"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = DecodePortable(request)
	if err == nil || !strings.Contains(err.Error(), "output_config.format requires a name") {
		t.Fatalf("want an error naming the field, got %v", err)
	}
}

// Anthropic's own ladder reaches `max`, but every portable request crosses the
// OpenAI intermediate representation, which stops at xhigh. Declaring `max` as
// routable made the request fail while being rendered — after the reservation,
// citing a field the caller never sent.
func TestEffortBeyondThePortableLadderIsDeclaredUnsupported(t *testing.T) {
	for effort, wantUnsupported := range map[string]bool{"xhigh": false, "max": true} {
		request := semantic.GenerateRequest{ReasoningEffort: effort}
		got := compatibility.UnsupportedGenerateFields(domain.ProfileAnthropicMessages, request)
		if slices.Contains(got, "reasoning_effort") != wantUnsupported {
			t.Fatalf("effort %q: unsupported=%v, want %v", effort, got, wantUnsupported)
		}
	}
}

// The current Claude models decide for themselves whether to think, and they do
// it by default: a request that says nothing about thinking comes back carrying
// signed thinking blocks. The portable surface has nowhere to keep a block
// signature that the next turn must hand back verbatim, so DecodeResult refuses
// such a response — and the caller is charged for a request that produced a 502.
// Not asking for thinking is what makes the OpenAI-compatible surface usable
// against a model that thinks by default.
func TestPortableRequestsDoNotAskForThinking(t *testing.T) {
	request := semantic.GenerateRequest{
		Operation: semantic.OperationGenerate, Mode: semantic.ModePortable, RequestedModel: "public",
		Source:   semantic.Source{ProfileID: string(compatibility.ProfileAnthropicMessages), ProfileRevision: 1},
		Messages: []semantic.Message{{Role: semantic.RoleUser, Content: []semantic.Content{{Kind: semantic.ContentText, Text: "hi"}}}},
	}
	rendered, err := RenderPortableRequest(request, "claude-provider")
	if err != nil {
		t.Fatal(err)
	}
	if string(rendered.Thinking) != `{"type":"disabled"}` {
		t.Fatalf("thinking configuration=%s", rendered.Thinking)
	}
	if rendered.OutputConfig != nil {
		t.Fatalf("a request that asked for nothing carried an output config: %#v", rendered.OutputConfig)
	}
}

// Reasoning the caller explicitly asked for is not quietly switched off. Serving
// them a shallower answer than they requested would be a worse failure than the
// one it avoids — it is invisible. Those requests belong on the native Messages
// surface, which can carry the thinking blocks back.
func TestExplicitlyRequestedReasoningIsNotDisabled(t *testing.T) {
	request := semantic.GenerateRequest{
		Operation: semantic.OperationGenerate, Mode: semantic.ModePortable, RequestedModel: "public",
		Source:          semantic.Source{ProfileID: string(compatibility.ProfileAnthropicMessages), ProfileRevision: 1},
		ReasoningEffort: "high",
		Requirements:    semantic.Requirements{Reasoning: true},
		Messages:        []semantic.Message{{Role: semantic.RoleUser, Content: []semantic.Content{{Kind: semantic.ContentText, Text: "hi"}}}},
	}
	rendered, err := RenderPortableRequest(request, "claude-provider")
	if err != nil {
		t.Fatal(err)
	}
	if len(rendered.Thinking) != 0 {
		t.Fatalf("requested reasoning was overridden: %s", rendered.Thinking)
	}
	if rendered.OutputConfig == nil || rendered.OutputConfig.Effort != "high" {
		t.Fatalf("effort not rendered: %#v", rendered.OutputConfig)
	}
}

// The semantic termination field has one vocabulary, and every adapter has to
// speak it: the Gemini and Bedrock adapters answer complete/max_output/refusal,
// and this mapping answered in OpenAI's words instead. Every consumer downstream
// reads the semantic set, so an Anthropic response arrived carrying a value none
// of them recognized — and /v1/responses turned that into a 502 for a turn that
// had ended normally.
func TestStopReasonsUseTheSemanticVocabularyInBothDirections(t *testing.T) {
	for _, test := range []struct {
		reason      string
		termination string
		rendered    string
	}{
		{"end_turn", "complete", "end_turn"},
		{"stop_sequence", "complete", "end_turn"},
		{"max_tokens", "max_output", "max_tokens"},
		{"model_context_window_exceeded", "max_output", "max_tokens"},
		{"tool_use", "tool_call", "tool_use"},
		{"pause_turn", "tool_call", "tool_use"},
		{"refusal", "refusal", "refusal"},
	} {
		t.Run(test.reason, func(t *testing.T) {
			reason := test.reason
			if got := decodeStopReason(&reason); got != test.termination {
				t.Fatalf("decoded %q as %q, want %q", test.reason, got, test.termination)
			}
			// The pairs are round trips: what this mapping decodes, it renders
			// back. A vocabulary mismatch between the two halves is invisible in
			// either half alone.
			if got := renderStopReason(test.termination); got != test.rendered {
				t.Fatalf("rendered %q as %q, want %q", test.termination, got, test.rendered)
			}
		})
	}
}

// A stop_reason this build has never seen ended the turn for a reason it cannot
// name. Reporting that as a normal completion would tell the caller the answer
// is whole when nothing checked whether it is.
func TestAnUnknownStopReasonIsNotReportedAsCompletion(t *testing.T) {
	reason := "some_future_stop_reason"
	if got := decodeStopReason(&reason); got != "unknown" {
		t.Fatalf("decoded as %q, want unknown", got)
	}
	var absent *string
	if got := decodeStopReason(absent); got != "" {
		t.Fatalf("a missing stop_reason decoded as %q", got)
	}
}
