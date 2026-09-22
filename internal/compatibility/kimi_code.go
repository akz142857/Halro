package compatibility

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/akz142857/Halro/internal/openaiapi"
)

// Kimi Code's OpenAI-compatible face is a different contract from the Open
// Platform dialect in kimi.go, and it needed its own renderer because the
// profile was reaching the upstream through the plain OpenAI marshaller — the
// one branch of encodeChatRequest that sends the request struct as written.
//
// Everything here was measured on 2026-09-22 against a real Kimi Code
// subscription key over `https://api.kimi.com/coding/v1`, under Halro's own
// `User-Agent`. The fixtures are in
// docs/verification/kimi-code-subscription-evidence.md.
//
// What the measurement changed:
//
//   - Four members are refused with HTTP 400 and a message naming the pinned
//     value: temperature (only 0.6), top_p (only 0.95), frequency_penalty and
//     presence_penalty (only 0). n>1 and logprobs are refused as invalid values.
//     The canonical request carries three of those, so without this renderer an
//     ordinary OpenAI client sending temperature paid a reservation for a
//     request the upstream then refused.
//   - `stop` carries the same published bounds as the metered face — at most
//     five sequences, each at most 32 bytes — and the upstream names both in its
//     error text.
//   - The single output bound counts reasoning here too: kimi-for-coding with
//     max_tokens=32 and nothing asked came back with 26 reasoning tokens, an
//     empty answer and finish_reason=length.
//   - `seed`, `user` and parallel_tool_calls=false answer 200. Nothing
//     establishes that any of them does anything, and an accepted member that
//     silently does nothing is the failure this package exists to prevent, so
//     they are declared as losses rather than forwarded.
//   - A named-function tool_choice together with a depth is refused:
//     `tool_choice 'specified' is incompatible with thinking enabled`.
//     `required` with a depth answers 200 with a tool call, so only the named
//     form is constrained.
//
// The reasoning switch is the part that unblocked the profile. Every model this
// face serves publishes `supports_thinking_type: "only"` and reasons on a
// request that says nothing — measured on k3, k3-256k and kimi-for-coding, each
// returning reasoning_content with the answer, and on the Messages face a
// `thinking` block. A top-level `reasoning_effort: "none"` switches it off on
// all three: reasoning_content absent, no reasoning_tokens, and the injected
// prompt scaffold gone (90 prompt tokens became 22). That is why this renderer
// always spells the off state rather than omitting the member, and why the
// profile does not have to be marked as reasoning unasked.
//
// One spelling, unlike the metered face: `reasoning:{"effort":"none"}` is
// accepted here and ignored — 200, and it reasoned anyway — where the metered
// face refuses it outright. An ignored member is the worse of the two, so the
// nested form is never sent.

// KimiCodeEffortLevels is the ladder the product's own catalogue publishes, in
// `think_efforts.valid_efforts` on every model that carries the member. There
// is no rung below `low` and no `medium`.
//
// The upstream does not enforce it: every string this build tried, including a
// deliberate nonsense value, answered 200 and reasoned. So the ladder is a bound
// Halro applies rather than one it can rely on being told about — a caller
// asking for `medium` would otherwise be served, and billed for, Kimi's default
// depth instead of the depth they named.
var KimiCodeEffortLevels = []string{"low", "high", "max"}

// KimiCodePortableEfforts is what a portable request can ask this face for:
// `none`, plus the rungs both ladders share. `max` is Kimi's alone and has no
// portable spelling, and `minimal`, `medium` and `xhigh` have no Kimi rung, so
// they are declared rather than rounded to a neighbour.
var KimiCodePortableEfforts = append([]string{kimiCodeThinkingOff},
	intersectSorted(KimiCodeEffortLevels, openaiapi.ReasoningEffortLevels)...)

// kimiCodeThinkingOff is the portable ladder's name for "do not think", and on
// this face it is also the wire value.
const kimiCodeThinkingOff = "none"

// KimiCodeEffortAsksForDepth reports whether a caller asked this request to
// reason. The field rules and this renderer both read it, because a
// disagreement between them is a request the router admits and the renderer then
// refuses, after the budget is reserved.
func KimiCodeEffortAsksForDepth(effort string) bool {
	return effort != "" && effort != kimiCodeThinkingOff
}

// KimiCodeChatRequest is the accepted subset, written out rather than derived
// from the OpenAI request by omission: a member added to
// openaiapi.ChatCompletionRequest must be considered here deliberately instead
// of reaching the upstream because nobody remembered to exclude it.
//
// Absent by construction: temperature, top_p, n, seed, user,
// parallel_tool_calls and response_format.
type KimiCodeChatRequest struct {
	Model    string              `json:"model,omitempty"`
	Messages []openaiapi.Message `json:"messages"`
	// MaxCompletionTokens is the bound this face applies, and it counts
	// reasoning. An answer-only bound is only carried into it while nothing is
	// thinking — see RenderKimiCodeChatRequest.
	MaxCompletionTokens *int64                   `json:"max_completion_tokens,omitempty"`
	Stop                json.RawMessage          `json:"stop,omitempty"`
	Stream              bool                     `json:"stream,omitempty"`
	StreamOptions       *openaiapi.StreamOptions `json:"stream_options,omitempty"`
	Tools               []openaiapi.Tool         `json:"tools,omitempty"`
	ToolChoice          json.RawMessage          `json:"tool_choice,omitempty"`
	// ReasoningEffort is always written: "none" is the off state and omitting the
	// member is not.
	ReasoningEffort string `json:"reasoning_effort"`
}

// RenderKimiCodeChatRequest converts an OpenAI-shaped request into the body the
// Kimi Code subscription face accepts.
//
// It refuses rather than drops, the same rule the Kimi, DeepSeek and MiniMax
// renderers follow. Routing already sheds a target whose profile declares one of
// these members lost, so in the running gateway most of these errors are
// unreachable; they are here so a caller who reaches the adapter another way
// fails closed instead of sending a request they did not make.
func RenderKimiCodeChatRequest(request openaiapi.ChatCompletionRequest) (KimiCodeChatRequest, error) {
	// The pinned sampling members. Substituting the pinned value would serve the
	// caller something other than what they asked for, so each is refused with
	// the value the upstream named.
	if request.Temperature != nil {
		return KimiCodeChatRequest{}, errors.New("Kimi Code fixes temperature at 0.6 and refuses any other value")
	}
	if request.TopP != nil {
		return KimiCodeChatRequest{}, errors.New("Kimi Code fixes top_p at 0.95 and refuses any other value")
	}
	if request.N != nil && *request.N > 1 {
		return KimiCodeChatRequest{}, errors.New("Kimi Code Chat Completions accepts only n=1")
	}
	// Accepted upstream, and that is the whole problem: nothing establishes that
	// either is read, so forwarding them would promise a reproducibility and an
	// attribution this face has not shown.
	if request.Seed != nil {
		return KimiCodeChatRequest{}, errors.New("Kimi Code Chat Completions accepts seed without establishing that it is honoured")
	}
	if request.User != "" {
		return KimiCodeChatRequest{}, errors.New("Kimi Code Chat Completions accepts user without establishing that it is honoured")
	}
	if request.ParallelToolCalls != nil && !*request.ParallelToolCalls {
		return KimiCodeChatRequest{}, errors.New("Kimi Code Chat Completions accepts parallel_tool_calls=false without establishing that it disables anything")
	}
	// Both JSON halves answer 200 on this face — json_object returned an object
	// and a json_schema request returned a body matching the schema — but the
	// profile does not declare either capability, and a renderer that carried a
	// member the capability set refuses would be the form of drift where the
	// adapter is wider than the contract. Declaring them is a deliberate change
	// to what this product offers, with the catalogue entries and the manifest
	// that go with it; the measurement is here so that change has its evidence.
	if len(request.ResponseFormat) > 0 {
		return KimiCodeChatRequest{}, errors.New("the Kimi Code profile does not declare structured output")
	}
	// One output bound, and it counts reasoning. Since this renderer switches
	// reasoning off on a request that asked for none, the answer-only bound and
	// the completion budget are the same tokens in the common case and an
	// ordinary Chat client sending max_tokens reaches the upstream. A request
	// that does ask for depth is the one where the two differ.
	limit := request.MaxCompletionTokens
	if request.MaxTokens != nil {
		switch {
		case request.MaxCompletionTokens != nil:
			return KimiCodeChatRequest{}, errors.New("Kimi Code has one output limit and the request carries two")
		case KimiCodeEffortAsksForDepth(request.ReasoningEffort):
			return KimiCodeChatRequest{}, errors.New("Kimi Code's output bound counts reasoning, so an answer-only max_tokens is not the same limit")
		default:
			limit = request.MaxTokens
		}
	}
	// The bounds the upstream states in its own error text — "stop array too
	// long. Expected an array with maximum length 5" and "stop sequence must not
	// be longer than 32" — are the same numbers the metered face publishes, so
	// the check is shared rather than restated.
	stop, err := renderKimiStop(request.Stop)
	if err != nil {
		return KimiCodeChatRequest{}, err
	}
	// Measured refused: `tool_choice 'specified' is incompatible with thinking
	// enabled`. `required` with a depth answers 200 with a tool call and a
	// reasoning span, so it is left alone.
	if KimiCodeEffortAsksForDepth(request.ReasoningEffort) &&
		kimiForcedToolCall(request.ToolChoice) == kimiForcedSpecified {
		return KimiCodeChatRequest{}, errors.New("Kimi Code will not force a named tool call while it is reasoning")
	}
	effort := request.ReasoningEffort
	switch {
	case !KimiCodeEffortAsksForDepth(effort):
		effort = kimiCodeThinkingOff
	case !slices.Contains(KimiCodeEffortLevels, effort):
		return KimiCodeChatRequest{}, fmt.Errorf("Kimi Code does not publish reasoning effort %q", effort)
	}
	return KimiCodeChatRequest{
		Model: request.Model, Messages: request.Messages,
		MaxCompletionTokens: limit,
		Stop:                stop,
		Stream:              request.Stream, StreamOptions: request.StreamOptions,
		Tools: request.Tools, ToolChoice: request.ToolChoice,
		ReasoningEffort: effort,
	}, nil
}
