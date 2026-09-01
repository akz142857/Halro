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

// tool_choice=required is a per-model limit too, and it goes the other way from
// the reasoning switch: kimi-k3 accepts it and the K2.x line refuses it.
func TestKimiChatAllowsRequiredToolChoiceOnlyWhereItIsAccepted(t *testing.T) {
	for model, allowed := range map[string]bool{
		"kimi-k3":                  true,
		"kimi-k2.7-code":           false,
		"kimi-k2.7-code-highspeed": false,
		"kimi-k2.6":                false,
		"kimi-k9-unreleased":       false,
	} {
		request := kimiBaseRequest()
		request.Model = model
		request.ToolChoice = json.RawMessage(`"required"`)
		_, err := RenderKimiChatRequest(request)
		if allowed && err != nil {
			t.Errorf("%s refused tool_choice=required and it accepts it: %v", model, err)
		}
		if !allowed && err == nil {
			t.Errorf("%s accepted tool_choice=required and it does not", model)
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
