package compatibility

import (
	"encoding/json"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

// The failure this pins reached an operator's screen, so it is written from that
// end rather than from the rule.
//
// A Kimi deployment answered `POST /v1/responses` with
// `502 provider response cannot be rendered safely`, and then, once enough of
// them had accumulated, `503 no healthy deployment is available for this model`.
// The 503 is the 502's consequence: repeated provider errors mark the deployment
// unhealthy and candidate resolution comes back empty.
//
// The cause was that this renderer left Kimi's reasoning switch alone on a
// request that asked for no reasoning. Kimi's default is on, so reasoning came
// back on every call, Halro decoded it into a reasoning content part, and
// neither the Responses nor the Messages northbound endpoint can render one —
// the upstream call had already succeeded and been billed.
//
// The fix is the house rule the DeepSeek and MiniMax renderers already follow:
// unspecified means off. This test pins that rule at the place it has to hold,
// which is the rendered body — not at routing, because taking Kimi off two of
// the three northbound endpoints was the first attempt at this and it broke the
// promise the gateway exists to keep, that an application does not change when
// the operator changes which model an alias points at.
func TestKimiSwitchesReasoningOffWhenNobodyAskedForIt(t *testing.T) {
	for _, test := range []struct {
		model string
		// want is the rendered thinking member, or "" for none at all.
		want string
	}{
		// Measured 2026-09-01: both accept the off switch and stop reasoning.
		{"kimi-k3", `{"type":"disabled"}`},
		{"kimi-k2.6", `{"type":"disabled"}`},
		// Measured on the same run: `invalid thinking: only type=enabled is
		// allowed for this model`. There is no off state to send, so nothing is
		// sent and the model keeps reasoning.
		{"kimi-k2.7-code", ""},
		{"kimi-k2.7-code-highspeed", ""},
		// An identifier this build does not know. Guessing a spelling for it is
		// how a request that asked for nothing gets billed for reasoning.
		{"kimi-k9-unreleased", ""},
	} {
		request := openaiapi.ChatCompletionRequest{
			Model:    test.model,
			Messages: []openaiapi.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		}
		body, err := RenderKimiChatRequest(request)
		if err != nil {
			t.Errorf("%s: %v", test.model, err)
			continue
		}
		var thinking string
		if body.Thinking != nil {
			encoded, err := json.Marshal(body.Thinking)
			if err != nil {
				t.Fatal(err)
			}
			thinking = string(encoded)
		}
		if thinking != test.want {
			t.Errorf("%s with nothing asked sent thinking %q, want %q", test.model, thinking, test.want)
		}
	}
}

// The other half of the promise: having made reasoning not come back, the Kimi
// targets must stay routable on every northbound endpoint. Refusing them was the
// first fix and it is the one this replaces — an application that integrated
// against `/v1/responses` would have stopped working the day its operator
// pointed the alias at Kimi.
func TestKimiTargetsStayRoutableOnEveryNorthboundEndpoint(t *testing.T) {
	request := func(northbound NorthboundProfileID) semantic.GenerateRequest {
		return semantic.GenerateRequest{
			Source:   semantic.Source{ProfileID: string(northbound), ProfileRevision: 1},
			Messages: []semantic.Message{{Role: semantic.RoleUser, Content: []semantic.Content{{Kind: semantic.ContentText, Text: "hi"}}}},
		}
	}
	for _, profile := range []domain.ProviderProfileID{
		domain.ProfileKimiChat, domain.ProfileKimiAnthropicMessages, domain.ProfileKimiResponses,
	} {
		for _, northbound := range []NorthboundProfileID{
			ProfileOpenAIChatCompletions, ProfileOpenAIResponses, ProfileAnthropicMessages,
		} {
			if fields := UnsupportedGenerateFields(profile, request(northbound)); len(fields) != 0 {
				t.Errorf("%s is refused on %s over %v, for a request that asked for nothing", profile, northbound, fields)
			}
		}
	}
}

// An ordinary Chat client sends max_tokens. With reasoning switched off it bounds
// the same tokens Kimi's own member does, so such a request has to reach Kimi
// rather than being routed away from it — the first version of the output-bound
// rule refused it unconditionally, which took Kimi off a large share of real
// traffic for a reason that only applies while something is thinking.
func TestKimiCarriesAnAnswerBoundWhileNothingIsThinking(t *testing.T) {
	limit := int64(64)
	base := semantic.GenerateRequest{
		Messages:                []semantic.Message{{Role: semantic.RoleUser, Content: []semantic.Content{{Kind: semantic.ContentText, Text: "hi"}}}},
		VisibleOutputTokenLimit: &limit,
	}
	for _, profile := range []domain.ProviderProfileID{domain.ProfileKimiChat, domain.ProfileKimiResponses} {
		if fields := UnsupportedGenerateFields(profile, base); len(fields) != 0 {
			t.Errorf("%s refuses an answer bound on a request with nothing thinking: %v", profile, fields)
		}
		thinking := base
		thinking.ReasoningEffort = "high"
		if fields := UnsupportedGenerateFields(profile, thinking); len(fields) == 0 {
			t.Errorf("%s carries an answer bound while reasoning is on, where Kimi's bound counts reasoning too", profile)
		}
	}
	// And the renderer has to agree, including on the one model that cannot be
	// switched off — there the two bounds differ even with nothing asked.
	answerOnly := openaiapi.ChatCompletionRequest{
		Model:     "kimi-k3",
		Messages:  []openaiapi.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		MaxTokens: &limit,
	}
	body, err := RenderKimiChatRequest(answerOnly)
	if err != nil {
		t.Fatalf("kimi-k3 refused an answer bound with reasoning off: %v", err)
	}
	if body.MaxCompletionTokens == nil || *body.MaxCompletionTokens != limit {
		t.Fatalf("the answer bound was not carried: %v", body.MaxCompletionTokens)
	}
	alwaysReasons := answerOnly
	alwaysReasons.Model = "kimi-k2.7-code"
	if _, err := RenderKimiChatRequest(alwaysReasons); err == nil {
		t.Error("kimi-k2.7-code carried an answer-only bound into a member that counts the reasoning it cannot stop")
	}
}
