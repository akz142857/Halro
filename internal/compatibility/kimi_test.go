package compatibility

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

func kimiBaseRequest() openaiapi.ChatCompletionRequest {
	return openaiapi.ChatCompletionRequest{
		Model:    "kimi-k3",
		Messages: []openaiapi.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
}

// The members Kimi has no place for. Each of these is absent from its request
// schema and pinned by its parameter reference, so the renderer refuses rather
// than sending a value that would come back 400 after the budget is reserved.
func TestKimiChatRefusesTheMembersItCannotCarry(t *testing.T) {
	sampling := 0.5
	candidates := 2
	seed := int64(7)
	noParallel := false
	for _, test := range []struct {
		name   string
		mutate func(*openaiapi.ChatCompletionRequest)
	}{
		{"temperature", func(r *openaiapi.ChatCompletionRequest) { r.Temperature = &sampling }},
		{"top_p", func(r *openaiapi.ChatCompletionRequest) { r.TopP = &sampling }},
		{"n", func(r *openaiapi.ChatCompletionRequest) { r.N = &candidates }},
		{"seed", func(r *openaiapi.ChatCompletionRequest) { r.Seed = &seed }},
		{"user", func(r *openaiapi.ChatCompletionRequest) { r.User = "u" }},
		{"parallel_tool_calls", func(r *openaiapi.ChatCompletionRequest) { r.ParallelToolCalls = &noParallel }},
	} {
		request := kimiBaseRequest()
		test.mutate(&request)
		if _, err := RenderKimiChatRequest(request); err == nil {
			t.Errorf("%s was accepted; Kimi has no member for it", test.name)
		}
	}
}

// temperature is refused at every value, including the one the model is pinned
// to. Carrying the pinned value needs a per-model fact the field rules cannot
// reach, so the renderer must not quietly special-case it — otherwise the rule
// and the renderer disagree about which requests may reach this profile.
func TestKimiChatRefusesEvenThePinnedSamplingValue(t *testing.T) {
	pinned := 1.0
	request := kimiBaseRequest()
	request.Temperature = &pinned
	if _, err := RenderKimiChatRequest(request); err == nil {
		t.Fatal("temperature=1.0 was accepted; the field rule routes it away, so the renderer must refuse it too")
	}
}

// The reasoning switch, which is the one thing about Kimi that varies by model
// rather than by connection. Sending the wrong spelling is not a validation
// error upstream — the model ignores it and reasons at its default — so this is
// the table that has to be right.
func TestKimiChatWritesTheReasoningSpellingTheModelReads(t *testing.T) {
	for _, test := range []struct {
		model    string
		effort   string
		effortJS string
		thinking string
		wantErr  bool
	}{
		{model: "kimi-k3", effort: "high", effortJS: "high"},
		{model: "kimi-k3", effort: "max", effortJS: "max"},
		{model: "kimi-k3", effort: "medium", wantErr: true},
		// kimi-k3 takes the off switch, measured, against what its own
		// documentation and /v1/models metadata both claim.
		{model: "kimi-k3", effort: "none", thinking: `{"type":"disabled"}`},
		{model: "kimi-k2.7-code", effort: "high", thinking: `{"type":"enabled","keep":"all"}`},
		{model: "kimi-k2.7-code-highspeed", effort: "low", thinking: `{"type":"enabled","keep":"all"}`},
		// The one line with no off state. Measured: `invalid thinking: only
		// type=enabled is allowed for this model`.
		{model: "kimi-k2.7-code", effort: "none", wantErr: true},
		{model: "kimi-k2.6", effort: "high", thinking: `{"type":"enabled"}`},
		{model: "kimi-k2.6", effort: "none", thinking: `{"type":"disabled"}`},
		// Nothing asked means off wherever there is a switch — the house rule the
		// DeepSeek and MiniMax renderers follow, and the fix for a real failure
		// where Kimi reasoned on requests that never asked and the answer could
		// not be rendered back out.
		{model: "kimi-k3", effort: "", thinking: `{"type":"disabled"}`},
		{model: "kimi-k2.6", effort: "", thinking: `{"type":"disabled"}`},
		// No switch to send: this line keeps reasoning, and what that costs is
		// recorded against the model rather than papered over here.
		{model: "kimi-k2.7-code", effort: ""},
		// An identifier this build does not know. A reasoning request cannot be
		// rendered for it, because guessing the spelling is how a request that
		// asked for a depth gets billed for the default instead — and an unasked
		// request gets no member either, for the same reason.
		{model: "kimi-k9-unreleased", effort: "high", wantErr: true},
		{model: "kimi-k9-unreleased", effort: ""},
	} {
		request := kimiBaseRequest()
		request.Model = test.model
		request.ReasoningEffort = test.effort
		body, err := RenderKimiChatRequest(request)
		if test.wantErr {
			if err == nil {
				t.Errorf("%s with effort %q was accepted and should not be", test.model, test.effort)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s with effort %q: %v", test.model, test.effort, err)
			continue
		}
		if body.ReasoningEffort != test.effortJS {
			t.Errorf("%s with effort %q sent reasoning_effort %q, want %q", test.model, test.effort, body.ReasoningEffort, test.effortJS)
		}
		var thinking string
		if body.Thinking != nil {
			encoded, err := json.Marshal(body.Thinking)
			if err != nil {
				t.Fatal(err)
			}
			thinking = string(encoded)
		}
		if thinking != test.thinking {
			t.Errorf("%s with effort %q sent thinking %s, want %s", test.model, test.effort, thinking, test.thinking)
		}
	}
}

// A forced tool call is refused on the K2.x line while it is reasoning, and
// nowhere else. Both conditions have to hold, and each was arrived at by
// overturning a version that had only one of them: Kimi's parameter reference
// says the limit is the model, its own error message says the limit is thinking
// enabled, and the four measured points say it is the conjunction — kimi-k3
// answers 200 with a tool call and a reasoning span in the same response.
//
// Written as a grid for that reason. The model-keyed version passed a one-model
// test, and the thinking-keyed version passed a grid that had no kimi-k3 row
// with a depth on it.
func TestKimiChatRefusesAForcedToolCallOnlyOnTheK2LineWhileItReasons(t *testing.T) {
	for _, choice := range []struct {
		raw json.RawMessage
		// exemptsK3 is the difference the fourth version of this rule exists for.
		// kimi-k3 with a depth accepts `required` and refuses a named function,
		// which no reading of the model or of the reasoning switch alone predicts.
		exemptsK3 bool
	}{
		{raw: json.RawMessage(`"required"`), exemptsK3: true},
		{raw: json.RawMessage(`{"type":"function","function":{"name":"f"}}`)},
	} {
		for _, test := range []struct {
			model, effort string
			refused       bool
		}{
			// Nothing asked, and the model has an off switch: this renderer sends
			// it, so nothing is thinking and the choice stands. kimi-k2.6 is the
			// row the model-keyed version got wrong — it refused the ordinary
			// portable request.
			{model: "kimi-k3", refused: false},
			{model: "kimi-k2.6", refused: false},
			// A depth was asked for. On kimi-k3 `required` is fine and measured —
			// 200, tool_calls, reasoning_content non-empty, one response — while a
			// named function on the same model with the same depth answers
			// `tool_choice 'specified' is incompatible with thinking enabled`.
			// exemptsK3 carries that split; refused here is the `required` answer.
			{model: "kimi-k3", effort: "high", refused: false},
			{model: "kimi-k3", effort: "max", refused: false},
			// The K2.x line with thinking on is the pair that actually conflicts.
			{model: "kimi-k2.6", effort: "low", refused: true},
			// Explicitly no depth is the same as nothing asked.
			{model: "kimi-k3", effort: "none", refused: false},
			// No off switch at all, so these reason whatever the request says.
			{model: "kimi-k2.7-code", refused: true},
			{model: "kimi-k2.7-code-highspeed", refused: true},
			// An identifier this build does not know gets no off switch sent, so
			// Kimi's default stands and the conservative reading is that it thinks.
			{model: "kimi-k9-unreleased", refused: true},
		} {
			request := kimiBaseRequest()
			request.Model, request.ReasoningEffort = test.model, test.effort
			request.ToolChoice = choice.raw
			// A row marked not-refused is the `required` answer. A named function
			// is refused there too unless nothing is reasoning at all, which is
			// what exemptsK3 distinguishes.
			refused := test.refused
			if !refused && !choice.exemptsK3 && KimiEffortAsksForDepth(test.effort) {
				refused = true
			}
			_, err := RenderKimiChatRequest(request)
			if refused && err == nil {
				t.Errorf("%s with effort %q accepted tool_choice %s while reasoning", test.model, test.effort, choice.raw)
			}
			if !refused && err != nil {
				t.Errorf("%s with effort %q refused tool_choice %s and it is accepted there: %v", test.model, test.effort, choice.raw, err)
			}
		}
	}
}

// Kimi documents stop as honoured, so it is carried rather than refused — and
// its published bounds are checked here, because a 400 from Kimi arrives after
// the reservation is taken.
func TestKimiChatEnforcesTheStopBounds(t *testing.T) {
	carried := kimiBaseRequest()
	carried.Stop = json.RawMessage(`["END","STOP"]`)
	body, err := RenderKimiChatRequest(carried)
	if err != nil {
		t.Fatalf("a stop list within the bounds was refused: %v", err)
	}
	if string(body.Stop) != `["END","STOP"]` {
		t.Fatalf("stop was not carried through: %s", body.Stop)
	}
	tooMany := kimiBaseRequest()
	tooMany.Stop = json.RawMessage(`["1","2","3","4","5","6"]`)
	if _, err := RenderKimiChatRequest(tooMany); err == nil {
		t.Error("six stop sequences were accepted; Kimi documents five as the ceiling")
	}
	tooLong := kimiBaseRequest()
	tooLong.Stop = json.RawMessage(`["` + strings.Repeat("x", 33) + `"]`)
	if _, err := RenderKimiChatRequest(tooLong); err == nil {
		t.Error("a 33-byte stop sequence was accepted; Kimi documents 32 bytes as the ceiling")
	}
}

// Both JSON halves are documented on this face. text means the same thing as
// omitting the member, so it is dropped rather than sent.
func TestKimiChatCarriesBothJSONHalvesAndDropsText(t *testing.T) {
	for _, test := range []struct {
		body string
		want string
	}{
		{`{"type":"text"}`, ""},
		{`{"type":"json_object"}`, `{"type":"json_object"}`},
		{`{"type":"json_schema","json_schema":{"name":"r","schema":{"type":"object"}}}`, `{"type":"json_schema","json_schema":{"name":"r","schema":{"type":"object"}}}`},
	} {
		request := kimiBaseRequest()
		request.ResponseFormat = json.RawMessage(test.body)
		rendered, err := RenderKimiChatRequest(request)
		if err != nil {
			t.Fatalf("%s: %v", test.body, err)
		}
		if string(rendered.ResponseFormat) != test.want {
			t.Errorf("%s rendered as %q, want %q", test.body, rendered.ResponseFormat, test.want)
		}
	}
	unknown := kimiBaseRequest()
	unknown.ResponseFormat = json.RawMessage(`{"type":"grammar"}`)
	if _, err := RenderKimiChatRequest(unknown); err == nil {
		t.Error("an unknown response_format type was accepted")
	}
}

// Kimi's single output bound counts reasoning, measured: max_completion_tokens
// 48 on kimi-k3 produced 45 reasoning tokens, 3 content tokens and an empty
// answer. The answer-only bound is therefore the same quantity exactly while
// nothing is thinking — which, since this renderer switches reasoning off on an
// unasked request, is the ordinary case.
func TestKimiChatCarriesTheAnswerBoundOnlyWhileNothingThinks(t *testing.T) {
	answer, completion := int64(16), int64(32)
	carried := kimiBaseRequest()
	carried.MaxTokens = &answer
	body, err := RenderKimiChatRequest(carried)
	if err != nil {
		t.Fatalf("an answer bound was refused with reasoning off: %v", err)
	}
	if body.MaxCompletionTokens == nil || *body.MaxCompletionTokens != answer {
		t.Fatalf("the answer bound was not carried: %v", body.MaxCompletionTokens)
	}
	thinking := kimiBaseRequest()
	thinking.MaxTokens = &answer
	thinking.ReasoningEffort = "high"
	if _, err := RenderKimiChatRequest(thinking); err == nil {
		t.Error("an answer bound was carried while reasoning was on, where Kimi's member counts reasoning too")
	}
	both := kimiBaseRequest()
	both.MaxTokens = &answer
	both.MaxCompletionTokens = &completion
	if _, err := RenderKimiChatRequest(both); err == nil {
		t.Error("two different output bounds were accepted; Kimi has one member for them")
	}
}

// The field rule and the renderer are two statements of one fact, and the shape
// of a disagreement between them is a request the router admits and the renderer
// then refuses — after the budget is reserved. This drives both over the same
// requests and requires them to agree.
func TestKimiChatFieldRulesAgreeWithTheRenderer(t *testing.T) {
	sampling := 0.5
	seed := int64(1)
	candidates := 2
	noParallel := false
	base := func() semantic.GenerateRequest {
		return semantic.GenerateRequest{
			Messages: []semantic.Message{{Role: semantic.RoleUser, Content: []semantic.Content{{Kind: semantic.ContentText, Text: "hi"}}}},
		}
	}
	for _, test := range []struct {
		name     string
		semantic func(*semantic.GenerateRequest)
		wire     func(*openaiapi.ChatCompletionRequest)
	}{
		{"temperature",
			func(r *semantic.GenerateRequest) { r.Temperature = &sampling },
			func(r *openaiapi.ChatCompletionRequest) { r.Temperature = &sampling }},
		{"top_p",
			func(r *semantic.GenerateRequest) { r.TopP = &sampling },
			func(r *openaiapi.ChatCompletionRequest) { r.TopP = &sampling }},
		{"n",
			func(r *semantic.GenerateRequest) { r.Candidates = &candidates },
			func(r *openaiapi.ChatCompletionRequest) { r.N = &candidates }},
		{"seed",
			func(r *semantic.GenerateRequest) { r.Seed = &seed },
			func(r *openaiapi.ChatCompletionRequest) { r.Seed = &seed }},
		{"user",
			func(r *semantic.GenerateRequest) { r.EndUserRef = "u" },
			func(r *openaiapi.ChatCompletionRequest) { r.User = "u" }},
		{"parallel_tool_calls",
			func(r *semantic.GenerateRequest) { r.ParallelTools = &noParallel },
			func(r *openaiapi.ChatCompletionRequest) { r.ParallelToolCalls = &noParallel }},
		{"reasoning_effort=medium",
			func(r *semantic.GenerateRequest) { r.ReasoningEffort = "medium" },
			func(r *openaiapi.ChatCompletionRequest) { r.ReasoningEffort = "medium" }},
		{"reasoning_effort=none",
			func(r *semantic.GenerateRequest) { r.ReasoningEffort = "none" },
			func(r *openaiapi.ChatCompletionRequest) { r.ReasoningEffort = "none" }},
		{"max_tokens",
			func(r *semantic.GenerateRequest) { limit := int64(16); r.VisibleOutputTokenLimit = &limit },
			func(r *openaiapi.ChatCompletionRequest) { limit := int64(16); r.MaxTokens = &limit }},
	} {
		canonical := base()
		test.semantic(&canonical)
		routedAway := len(UnsupportedGenerateFields(domain.ProfileKimiChat, canonical)) > 0
		wire := kimiBaseRequest()
		test.wire(&wire)
		_, err := RenderKimiChatRequest(wire)
		if routedAway != (err != nil) {
			t.Errorf("%s: the field rules route away = %v but the renderer refuses = %v", test.name, routedAway, err != nil)
		}
	}
	// The other direction: a rung both ladders carry must survive both layers.
	for _, effort := range KimiPortableEfforts {
		canonical := base()
		canonical.ReasoningEffort = effort
		if fields := UnsupportedGenerateFields(domain.ProfileKimiChat, canonical); len(fields) > 0 {
			t.Errorf("effort %q is on both ladders and the field rules refuse it: %v", effort, fields)
		}
		wire := kimiBaseRequest()
		wire.ReasoningEffort = effort
		if _, err := RenderKimiChatRequest(wire); err != nil {
			t.Errorf("effort %q is on both ladders and the renderer refuses it: %v", effort, err)
		}
	}

	// The crossed axes, which is where every disagreement this test was written
	// to catch actually lived. The single-member cases above vary one thing at a
	// time on one model, and the two layers see different halves of a request:
	// the field rules hold the members and the renderer holds the model. A
	// disagreement is a request the router admits and the renderer then refuses,
	// after the budget is reserved.
	//
	// The known residue is stated rather than hidden: on a model that cannot be
	// switched off, or one this build does not know, the renderer refuses things
	// no profile-scoped rule can see. Those rows carry residue=true, and the
	// assertion is that the set of them does not grow.
	limit := int64(16)
	for _, test := range []struct {
		name    string
		model   string
		mutate  func(*semantic.GenerateRequest, *openaiapi.ChatCompletionRequest)
		residue bool
	}{
		{name: "answer bound with an explicit no-depth", model: "kimi-k3",
			mutate: func(c *semantic.GenerateRequest, w *openaiapi.ChatCompletionRequest) {
				c.ReasoningEffort, w.ReasoningEffort = "none", "none"
				c.VisibleOutputTokenLimit, w.MaxTokens = &limit, &limit
			}},
		{name: "answer bound with a depth", model: "kimi-k3",
			mutate: func(c *semantic.GenerateRequest, w *openaiapi.ChatCompletionRequest) {
				c.ReasoningEffort, w.ReasoningEffort = "high", "high"
				c.VisibleOutputTokenLimit, w.MaxTokens = &limit, &limit
			}},
		{name: "too many stop sequences", model: "kimi-k3",
			mutate: func(c *semantic.GenerateRequest, w *openaiapi.ChatCompletionRequest) {
				c.Stop = []string{"1", "2", "3", "4", "5", "6"}
				w.Stop = json.RawMessage(`["1","2","3","4","5","6"]`)
			}},
		{name: "an over-long stop sequence", model: "kimi-k3",
			mutate: func(c *semantic.GenerateRequest, w *openaiapi.ChatCompletionRequest) {
				long := strings.Repeat("x", 64)
				c.Stop = []string{long}
				w.Stop = json.RawMessage(`["` + long + `"]`)
			}},
		{name: "a named function with a depth, refused by every model and routed away", model: "kimi-k3",
			mutate: func(c *semantic.GenerateRequest, w *openaiapi.ChatCompletionRequest) {
				c.ReasoningEffort, w.ReasoningEffort = "high", "high"
				c.Tools = []semantic.Tool{{Name: "f"}}
				c.ToolChoice = &semantic.ToolChoice{Mode: semantic.ToolChoiceNamed, Name: "f"}
				w.ToolChoice = json.RawMessage(`{"type":"function","function":{"name":"f"}}`)
			}},
		{name: "required with a depth on the model that allows it", model: "kimi-k3",
			mutate: func(c *semantic.GenerateRequest, w *openaiapi.ChatCompletionRequest) {
				c.ReasoningEffort, w.ReasoningEffort = "high", "high"
				c.Tools = []semantic.Tool{{Name: "f"}}
				c.ToolChoice = &semantic.ToolChoice{Mode: semantic.ToolChoiceRequired}
				w.ToolChoice = json.RawMessage(`"required"`)
			}},
		// Residue, and the trade behind it is the point: a rule keyed by profile
		// would have to refuse the kimi-k3 row above to route this one away, and
		// kimi-k3 with a depth and a forced tool is measured working.
		{name: "required with a depth on the line that refuses it", model: "kimi-k2.6",
			mutate: func(c *semantic.GenerateRequest, w *openaiapi.ChatCompletionRequest) {
				c.ReasoningEffort, w.ReasoningEffort = "low", "low"
				c.Tools = []semantic.Tool{{Name: "f"}}
				c.ToolChoice = &semantic.ToolChoice{Mode: semantic.ToolChoiceRequired}
				w.ToolChoice = json.RawMessage(`"required"`)
			}, residue: true},
		{name: "a forced tool call with nothing asked", model: "kimi-k2.6",
			mutate: func(c *semantic.GenerateRequest, w *openaiapi.ChatCompletionRequest) {
				c.Tools = []semantic.Tool{{Name: "f"}}
				c.ToolChoice = &semantic.ToolChoice{Mode: semantic.ToolChoiceRequired}
				w.ToolChoice = json.RawMessage(`"required"`)
			}},
		// The residue. Both are per-model facts a rule keyed by profile cannot
		// reach, and both are declared in the endpoint manifest's transforms.
		{name: "an answer bound on a model that always reasons", model: "kimi-k2.7-code",
			mutate: func(c *semantic.GenerateRequest, w *openaiapi.ChatCompletionRequest) {
				c.VisibleOutputTokenLimit, w.MaxTokens = &limit, &limit
			}, residue: true},
		{name: "an answer bound on a model this build does not know", model: "kimi-k9-unreleased",
			mutate: func(c *semantic.GenerateRequest, w *openaiapi.ChatCompletionRequest) {
				c.VisibleOutputTokenLimit, w.MaxTokens = &limit, &limit
			}, residue: true},
		{name: "a depth on a model this build does not know", model: "kimi-k9-unreleased",
			mutate: func(c *semantic.GenerateRequest, w *openaiapi.ChatCompletionRequest) {
				c.ReasoningEffort, w.ReasoningEffort = "high", "high"
			}, residue: true},
	} {
		canonical := base()
		wire := kimiBaseRequest()
		wire.Model = test.model
		test.mutate(&canonical, &wire)
		routedAway := len(UnsupportedGenerateFields(domain.ProfileKimiChat, canonical)) > 0
		_, err := RenderKimiChatRequest(wire)
		switch {
		case test.residue:
			if routedAway || err == nil {
				t.Errorf("%s on %s: this was the known per-model residue and is no longer (routed away = %v, renderer refuses = %v)",
					test.name, test.model, routedAway, err != nil)
			}
		case routedAway != (err != nil):
			t.Errorf("%s on %s: the field rules route away = %v but the renderer refuses = %v",
				test.name, test.model, routedAway, err)
		}
	}
}

// The ladder itself, pinned so a later edit cannot widen it by accident. Kimi
// publishes low, high and max; the portable path reaches the first two, because
// every portable request passes through the OpenAI ladder and that one stops at
// xhigh.
func TestKimiPortableEffortsAreTheIntersection(t *testing.T) {
	if !slices.Equal(KimiEffortLevels, []string{"low", "high", "max"}) {
		t.Fatalf("Kimi's published ladder changed: %v", KimiEffortLevels)
	}
	// `none` is prepended rather than intersected: Kimi spells "off" as a
	// thinking type, not as a rung on the depth ladder.
	if !slices.Equal(KimiPortableEfforts, []string{"none", "low", "high"}) {
		t.Fatalf("the portable reach of that ladder changed: %v", KimiPortableEfforts)
	}
}
