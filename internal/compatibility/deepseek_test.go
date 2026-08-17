package compatibility_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/akz142857/Halro/internal/anthropicapi"
	"github.com/akz142857/Halro/internal/compatibility"
	anthropicwire "github.com/akz142857/Halro/internal/compatibility/anthropic"
	openaiwire "github.com/akz142857/Halro/internal/compatibility/openai"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

func deepSeekBaseRequest() semantic.GenerateRequest {
	request := semantic.GenerateRequest{
		Operation: semantic.OperationGenerate, Mode: semantic.ModePortable,
		Source:         semantic.Source{ProfileID: string(compatibility.ProfileOpenAIChatCompletions), ProfileRevision: 1},
		RequestedModel: "deepseek",
		Messages:       []semantic.Message{{Role: semantic.RoleUser, Content: []semantic.Content{{Kind: semantic.ContentText, Text: "hi"}}}},
	}
	request.Requirements = request.DeriveRequirements()
	return request
}

// The rule this table exists to hold: a member outside DeepSeek's accepted list
// is either declared or rendered into a shape DeepSeek reads, and there is no
// third outcome. Checking only the declared half is what let `user` sit in
// neither column — accepted from the caller, sent under a name the upstream does
// not read, and reported as a success.
func TestDeepSeekDeclaresEveryFieldItCannotCarryAndNoneItCan(t *testing.T) {
	declared := func(name string, mutate func(*semantic.GenerateRequest)) []string {
		t.Helper()
		request := deepSeekBaseRequest()
		mutate(&request)
		request.Requirements = request.DeriveRequirements()
		return compatibility.UnsupportedGenerateFields(domain.ProfileDeepSeekChat, request)
	}
	seed, limit, candidates := int64(7), int64(64), 2
	serial, parallel := false, true
	for _, testCase := range []struct {
		field  string
		mutate func(*semantic.GenerateRequest)
	}{
		{"n", func(r *semantic.GenerateRequest) { r.Candidates = &candidates }},
		{"seed", func(r *semantic.GenerateRequest) { r.Seed = &seed }},
		{"parallel_tool_calls", func(r *semantic.GenerateRequest) { r.ParallelTools = &serial }},
		{"max_completion_tokens", func(r *semantic.GenerateRequest) { r.CompletionTokenLimit = &limit }},
		{"reasoning_effort", func(r *semantic.GenerateRequest) { r.ReasoningEffort = "medium" }},
		{"reasoning_effort", func(r *semantic.GenerateRequest) { r.ReasoningEffort = "minimal" }},
		{"reasoning_effort", func(r *semantic.GenerateRequest) { r.ReasoningEffort = "xhigh" }},
		// DeepSeek takes text and json_object and has no schema mode.
		{"response_format", func(r *semantic.GenerateRequest) {
			r.OutputFormat = &semantic.OutputFormat{
				Kind: semantic.OutputJSONSchema, Name: "reply",
				Schema: json.RawMessage(`{"type":"object"}`), Strict: true,
			}
		}},
	} {
		if fields := declared(testCase.field, testCase.mutate); !slices.Contains(fields, testCase.field) {
			t.Fatalf("DeepSeek dropped %s without declaring it: %v", testCase.field, fields)
		}
	}
	visible := int64(64)
	for _, testCase := range []struct {
		field  string
		mutate func(*semantic.GenerateRequest)
	}{
		{"user", func(r *semantic.GenerateRequest) { r.EndUserRef = "customer" }},
		// Parallel-allowed is the upstream default and the value every portable
		// Messages request with a tool_choice arrives carrying, so declaring it
		// would refuse requests that asked for nothing.
		{"parallel_tool_calls", func(r *semantic.GenerateRequest) {
			r.Tools = []semantic.Tool{{Name: "lookup", Schema: json.RawMessage(`{"type":"object"}`)}}
			r.ToolChoice = &semantic.ToolChoice{Mode: semantic.ToolChoiceAuto}
			r.ParallelTools = &parallel
		}},
		{"max_tokens", func(r *semantic.GenerateRequest) { r.VisibleOutputTokenLimit = &visible }},
		{"stop", func(r *semantic.GenerateRequest) { r.Stop = []string{"END"} }},
		{"tools", func(r *semantic.GenerateRequest) {
			r.Tools = []semantic.Tool{{Name: "lookup", Schema: json.RawMessage(`{"type":"object"}`)}}
		}},
		{"tool_choice", func(r *semantic.GenerateRequest) {
			r.Tools = []semantic.Tool{{Name: "lookup", Schema: json.RawMessage(`{"type":"object"}`)}}
			r.ToolChoice = &semantic.ToolChoice{Mode: semantic.ToolChoiceAuto}
		}},
		{"response_format", func(r *semantic.GenerateRequest) {
			r.OutputFormat = &semantic.OutputFormat{Kind: semantic.OutputJSONObject}
		}},
		// `none` is a rung DeepSeek can serve: thinking.type takes "disabled".
		{"reasoning_effort", func(r *semantic.GenerateRequest) { r.ReasoningEffort = "none" }},
		{"stream_options", func(r *semantic.GenerateRequest) { r.Stream, r.IncludeUsage = true, true }},
		{"messages[].name", func(r *semantic.GenerateRequest) { r.Messages[0].Name = "customer" }},
		{"reasoning_effort", func(r *semantic.GenerateRequest) { r.ReasoningEffort = "low" }},
		{"reasoning_effort", func(r *semantic.GenerateRequest) { r.ReasoningEffort = "high" }},
	} {
		if fields := declared(testCase.field, testCase.mutate); slices.Contains(fields, testCase.field) {
			t.Fatalf("DeepSeek declared %s, which it carries: %v", testCase.field, fields)
		}
	}
}

// The other direction of the same rule, checked against the renderer rather than
// the declaration: anything the declaration lets through has to survive being
// put on the wire, and anything the renderer refuses has to have been declared.
func TestDeepSeekRendersEveryRequestItsDeclarationAdmits(t *testing.T) {
	seed, completion, visible, candidates := int64(7), int64(64), int64(64), 2
	serial, parallel := false, true
	for _, mutate := range []func(*semantic.GenerateRequest){
		func(r *semantic.GenerateRequest) {},
		func(r *semantic.GenerateRequest) { r.Candidates = &candidates },
		func(r *semantic.GenerateRequest) { r.Seed = &seed },
		func(r *semantic.GenerateRequest) { r.ParallelTools = &serial },
		func(r *semantic.GenerateRequest) { r.ParallelTools = &parallel },
		func(r *semantic.GenerateRequest) { r.CompletionTokenLimit = &completion },
		func(r *semantic.GenerateRequest) { r.VisibleOutputTokenLimit = &visible },
		func(r *semantic.GenerateRequest) { r.EndUserRef = "customer" },
		func(r *semantic.GenerateRequest) { r.Stop = []string{"END"} },
		func(r *semantic.GenerateRequest) { r.ReasoningEffort = "none" },
		func(r *semantic.GenerateRequest) { r.ReasoningEffort = "minimal" },
		func(r *semantic.GenerateRequest) { r.ReasoningEffort = "low" },
		func(r *semantic.GenerateRequest) { r.ReasoningEffort = "medium" },
		func(r *semantic.GenerateRequest) { r.ReasoningEffort = "high" },
		func(r *semantic.GenerateRequest) { r.ReasoningEffort = "xhigh" },
		func(r *semantic.GenerateRequest) {
			r.OutputFormat = &semantic.OutputFormat{Kind: semantic.OutputJSONObject}
		},
		func(r *semantic.GenerateRequest) {
			r.OutputFormat = &semantic.OutputFormat{
				Kind: semantic.OutputJSONSchema, Name: "reply",
				Schema: json.RawMessage(`{"type":"object"}`), Strict: true,
			}
		},
		// Declared, but not by the renderer — the loss is in the shape of an
		// OpenAI tool message, which is why the reverse assertion below is scoped.
		func(r *semantic.GenerateRequest) {
			r.Messages = append(r.Messages,
				semantic.Message{Role: semantic.RoleAssistant, Content: []semantic.Content{
					{Kind: semantic.ContentToolCall, CallID: "call_1", Name: "lookup", Arguments: "{}"},
				}},
				semantic.Message{Role: semantic.RoleTool, Content: []semantic.Content{
					{Kind: semantic.ContentToolResult, CallID: "call_1", Text: "boom", ToolError: true},
				}})
		},
	} {
		request := deepSeekBaseRequest()
		mutate(&request)
		request.Requirements = request.DeriveRequirements()
		unsupported := compatibility.UnsupportedGenerateFields(domain.ProfileDeepSeekChat, request)
		wire, err := openaiwire.RenderGenerateRequest(request, "deepseek-v4-flash")
		if err != nil {
			t.Fatalf("portable render failed for %#v: %v", request, err)
		}
		_, renderErr := compatibility.RenderDeepSeekChatRequest(wire)
		if len(unsupported) == 0 && renderErr != nil {
			t.Fatalf("declaration admitted a request the wire cannot carry (%v): %#v", renderErr, wire)
		}
		// The reverse direction is asserted only for the members the renderer is
		// the judge of. messages[].content[].is_error is declared too, and the
		// renderer does not refuse it — it is lost in the shape of an OpenAI tool
		// message, not in a member the renderer inspects — so holding the whole
		// declared set to "the renderer must also refuse it" would be a rule this
		// code does not follow and should not.
		declaredByRenderer := slices.DeleteFunc(slices.Clone(unsupported), func(field string) bool {
			return !slices.Contains(deepSeekRendererFields, field)
		})
		if len(declaredByRenderer) > 0 && renderErr == nil {
			t.Fatalf("declaration rejected %v for a request the wire carries fine: %#v", declaredByRenderer, wire)
		}
	}
}

// deepSeekRendererFields are the members RenderDeepSeekChatRequest decides on.
var deepSeekRendererFields = []string{"n", "seed", "parallel_tool_calls", "max_completion_tokens", "reasoning_effort", "response_format"}

// The portable Messages path is where a presence-only parallel_tool_calls
// declaration does damage, and it is not visible from the OpenAI side: an
// Anthropic-shaped request that merely names a tool_choice comes out of
// DecodePortable with the flag already set, because DecodeToolChoice always
// returns one. Declaring on presence therefore routed DeepSeek away from every
// tool-using Messages request, including ones that said nothing about
// parallelism. Only the disable is a loss.
func TestDeepSeekStillServesPortableMessagesThatNameAToolChoice(t *testing.T) {
	request := anthropicapi.MessageRequest{
		Model: "deepseek", MaxTokens: 64,
		Messages: []anthropicapi.MessageParam{{Role: "user", Content: anthropicapi.ContentBlocks{
			{Type: "text", Text: "hi", Raw: json.RawMessage(`"hi"`)},
		}}},
		Tools: []anthropicapi.Tool{{
			Name: "lookup", Description: "look something up",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		}},
		ToolChoice: &anthropicapi.ToolChoice{Type: "auto"},
	}
	canonical, err := anthropicwire.DecodePortable(request)
	if err != nil {
		t.Fatal(err)
	}
	if canonical.ParallelTools == nil {
		t.Fatal("this test no longer exercises the path it was written for")
	}
	if fields := compatibility.UnsupportedGenerateFields(domain.ProfileDeepSeekChat, canonical); len(fields) != 0 {
		t.Fatalf("a Messages request that asked for nothing was routed away from DeepSeek: %v", fields)
	}
	body, err := compatibility.RenderDeepSeekChatRequest(mustRenderDeepSeek(t, canonical))
	if err != nil {
		t.Fatal(err)
	}
	if len(body.Tools) != 1 || len(body.ToolChoice) == 0 {
		t.Fatalf("the tool call did not survive: %#v", body)
	}

	// Asking for tools to be run one at a time is the thing DeepSeek has no
	// member for, and that one must still be declared.
	request.ToolChoice = &anthropicapi.ToolChoice{Type: "auto", DisableParallelToolUse: true}
	serial, err := anthropicwire.DecodePortable(request)
	if err != nil {
		t.Fatal(err)
	}
	if fields := compatibility.UnsupportedGenerateFields(domain.ProfileDeepSeekChat, serial); !slices.Contains(fields, "parallel_tool_calls") {
		t.Fatalf("DeepSeek dropped a disable it cannot express: %v", fields)
	}
}

// user is the rename, not a loss. It reaches DeepSeek as user_id and appears
// under no other key — sending `user` was accepted upstream and ignored.
func TestDeepSeekCarriesTheEndUserReferenceAsUserID(t *testing.T) {
	request := deepSeekBaseRequest()
	request.EndUserRef = "customer-7"
	request.Requirements = request.DeriveRequirements()
	body, err := compatibility.RenderDeepSeekChatRequest(mustRenderDeepSeek(t, request))
	if err != nil {
		t.Fatal(err)
	}
	if body.UserID != "customer-7" {
		t.Fatalf("user_id = %q", body.UserID)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &members); err != nil {
		t.Fatal(err)
	}
	if _, present := members["user"]; present {
		t.Fatalf("the OpenAI spelling reached DeepSeek: %s", encoded)
	}
	for _, absent := range []string{"n", "seed", "parallel_tool_calls", "max_completion_tokens", "reasoning_effort"} {
		if _, present := members[absent]; present {
			t.Fatalf("%s reached a surface that does not accept it: %s", absent, encoded)
		}
	}
}

// The reasoning path, which had no wire at all: the Reasoning capability was on
// and the only thing the gateway could emit was a top-level reasoning_effort
// DeepSeek does not read.
func TestDeepSeekReachesReasoningThroughTheThinkingSwitch(t *testing.T) {
	for _, effort := range []string{"low", "high"} {
		request := deepSeekBaseRequest()
		request.ReasoningEffort = effort
		request.Requirements = request.DeriveRequirements()
		if !request.Requirements.Reasoning {
			t.Fatalf("effort %q did not require reasoning", effort)
		}
		if fields := compatibility.UnsupportedGenerateFields(domain.ProfileDeepSeekChat, request); len(fields) != 0 {
			t.Fatalf("effort %q was routed away from DeepSeek: %v", effort, fields)
		}
		wire, err := openaiwire.RenderGenerateRequest(request, "deepseek-v4-flash")
		if err != nil {
			t.Fatal(err)
		}
		body, err := compatibility.RenderDeepSeekChatRequest(wire)
		if err != nil {
			t.Fatal(err)
		}
		if body.Thinking == nil || body.Thinking.Type != "enabled" || body.Thinking.ReasoningEffort != effort {
			t.Fatalf("effort %q did not reach the thinking switch: %#v", effort, body.Thinking)
		}
	}
	// "Do not think" is a rung DeepSeek can serve — thinking.type takes
	// "disabled" — and it has to be sent, because DeepSeek's own default is
	// thinking on. Declaring it unsupported, or dropping it, would bill the
	// caller for reasoning they explicitly declined.
	off := deepSeekBaseRequest()
	off.ReasoningEffort = "none"
	off.Requirements = off.DeriveRequirements()
	if fields := compatibility.UnsupportedGenerateFields(domain.ProfileDeepSeekChat, off); len(fields) != 0 {
		t.Fatalf("a request that declined reasoning was routed away from DeepSeek: %v", fields)
	}
	body, err := compatibility.RenderDeepSeekChatRequest(mustRenderDeepSeek(t, off))
	if err != nil {
		t.Fatal(err)
	}
	if body.Thinking == nil || body.Thinking.Type != "disabled" || body.Thinking.ReasoningEffort != "" {
		t.Fatalf("none did not reach the off switch: %#v", body.Thinking)
	}

	// A request that asked for nothing is switched off too, because DeepSeek's
	// own default is on. Leaving it to the provider is what broke /v1/responses
	// streaming: that endpoint rejects the reasoning request field, so the caller
	// could not ask, and the stream came back carrying reasoning content the
	// Phase 1A renderer cannot represent — after the upstream had billed it.
	body, err = compatibility.RenderDeepSeekChatRequest(mustRenderDeepSeek(t, deepSeekBaseRequest()))
	if err != nil {
		t.Fatal(err)
	}
	if body.Thinking == nil || body.Thinking.Type != "disabled" {
		t.Fatalf("an unasked request was left to DeepSeek's thinking-on default: %#v", body.Thinking)
	}
	if body.Thinking.ReasoningEffort != "" {
		t.Fatalf("a disabled switch carried a depth: %#v", body.Thinking)
	}
}

// DeepSeek's `max` rung is real upstream and unreachable through Halro, because
// every portable request passes through the OpenAI ladder, which stops at
// xhigh. Recording it keeps the ladder honest about which end each bound comes
// from.
func TestDeepSeekMaxEffortIsUpstreamOnlyAndNotPortable(t *testing.T) {
	if !slices.Contains(compatibility.DeepSeekEffortLevels, "max") {
		t.Fatal("DeepSeek's documented max rung went missing")
	}
	request := deepSeekBaseRequest()
	request.ReasoningEffort = "max"
	if _, err := openaiwire.RenderGenerateRequest(request, "deepseek-v4-flash"); err == nil {
		t.Fatal("the portable ladder accepted an effort it does not define")
	}
}

func mustRenderDeepSeek(t *testing.T, request semantic.GenerateRequest) openaiapi.ChatCompletionRequest {
	t.Helper()
	rendered, err := openaiwire.RenderGenerateRequest(request, "deepseek-v4-flash")
	if err != nil {
		t.Fatal(err)
	}
	return rendered
}
