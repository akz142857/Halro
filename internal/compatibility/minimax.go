package compatibility

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/akz142857/Halro/internal/openaiapi"
)

// MiniMax speaks three wire shapes on one host, and only one of them needs a
// body of its own.
//
// Fields MiniMax documents on Chat Completions (platform.minimax.io and
// platform.minimax.cn, read 2026-08-31):
//
//	model, messages, service_tier, thinking, reasoning_split, stream,
//	stream_options, max_completion_tokens, temperature, top_p, tools, tool_choice
//
// presence_penalty, frequency_penalty and logit_bias are documented as
// "会被忽略" — accepted and silently dropped. Halro's OpenAI request type has no
// such members, so there is nothing here to declare for them.
//
// What is not on the accepted list matters more than usual on this upstream,
// because MiniMax ignores rather than refuses: n above 1, seed, stop,
// response_format, user, and — the expensive one — the top-level
// reasoning_effort every other OpenAI-shaped profile sends. A request carrying
// reasoning_effort: none would come back 200 having thought and billed for it,
// with nothing anywhere saying the switch was not read. That is the whole reason
// this file exists; the OpenAI adapter cannot be shared unmodified the way it
// looked like it could.
//
// The Anthropic face needs no counterpart. The portable Messages mapper already
// emits {"type":"disabled"} for a request that asked for no depth, which is
// exactly MiniMax's spelling, and a request that does ask reaches
// output_config.effort — a member MiniMax does not accept, so it is declared
// unsupported in provider_fields.go rather than rendered into something else.
//
// The Responses face needs none either: MiniMax accepts reasoning.effort under
// its OpenAI name.
//
// Everything here is read from published documentation and has not been
// confirmed against a live account. See docs/prd/minimax-adaptation-plan.zh-CN.md.

// MiniMaxThinkingOn is MiniMax's single "think" state. The switch has two
// values and no depth ladder, so every portable effort that is not "none"
// reaches this one — which is a real loss of resolution and is declared as a
// transform on the endpoint manifests rather than hidden here.
const MiniMaxThinkingOn = "adaptive"

// MiniMaxThinkingOff is the portable ladder's "do not think", which MiniMax
// spells as a type rather than a depth.
const MiniMaxThinkingOff = "disabled"

// minimaxThinkingOffEffort is the portable name for the same request.
const minimaxThinkingOffEffort = "none"

// MiniMaxThinking is MiniMax's reasoning switch.
//
// Its documented default is "adaptive" on MiniMax-M3 — thinking is on unless
// the request says otherwise — which is why this member is always present in a
// rendered body rather than omitted when nobody asked. See the switch at the
// bottom of RenderMiniMaxChatRequest for why that default cannot be left alone.
type MiniMaxThinking struct {
	Type string `json:"type"`
}

// MiniMaxChatRequest is the accepted subset, written out rather than derived
// from the OpenAI request by omission: a member added to
// openaiapi.ChatCompletionRequest must be considered here deliberately instead
// of reaching MiniMax because nobody remembered to exclude it.
type MiniMaxChatRequest struct {
	Model    string              `json:"model,omitempty"`
	Messages []openaiapi.Message `json:"messages"`
	Thinking *MiniMaxThinking    `json:"thinking,omitempty"`
	// ReasoningSplit asks MiniMax to return reasoning in its own member instead
	// of inline in the answer. It is sent only while thinking is on: with the
	// switch off there is nothing to split, and sending it anyway would be a
	// member the caller never asked for on every request.
	ReasoningSplit bool `json:"reasoning_split,omitempty"`
	// MaxCompletionTokens is MiniMax's only output bound. It counts everything a
	// completion generates, reasoning included.
	MaxCompletionTokens *int64                   `json:"max_completion_tokens,omitempty"`
	Stream              bool                     `json:"stream,omitempty"`
	StreamOptions       *openaiapi.StreamOptions `json:"stream_options,omitempty"`
	Temperature         *float64                 `json:"temperature,omitempty"`
	TopP                *float64                 `json:"top_p,omitempty"`
	Tools               []openaiapi.Tool         `json:"tools,omitempty"`
	ToolChoice          json.RawMessage          `json:"tool_choice,omitempty"`
}

// RenderMiniMaxChatRequest converts an OpenAI-shaped request into the body
// MiniMax accepts.
//
// It refuses rather than drops. Routing already removes a MiniMax target from
// the candidate set when the request carries one of these members, so in the
// running gateway these errors are unreachable; they are here so that a caller
// who reaches the adapter another way fails closed at the last moment instead of
// sending a request the caller did not make.
func RenderMiniMaxChatRequest(request openaiapi.ChatCompletionRequest) (MiniMaxChatRequest, error) {
	if request.N != nil && *request.N > 1 {
		return MiniMaxChatRequest{}, errors.New("MiniMax Chat Completions accepts only n=1")
	}
	if request.Seed != nil {
		return MiniMaxChatRequest{}, errors.New("MiniMax Chat Completions does not accept seed")
	}
	if len(request.Stop) > 0 {
		return MiniMaxChatRequest{}, errors.New("MiniMax Chat Completions does not accept stop")
	}
	if len(request.ResponseFormat) > 0 {
		return MiniMaxChatRequest{}, errors.New("MiniMax Chat Completions does not accept response_format")
	}
	if request.User != "" {
		return MiniMaxChatRequest{}, errors.New("MiniMax Chat Completions has no end-user attribution member")
	}
	// Parallel-allowed is what omitting the member already gets, so only the
	// disable has nowhere to go. Refusing on presence would refuse every portable
	// Messages request that names a tool_choice, because that path always
	// produces the flag.
	if request.ParallelToolCalls != nil && !*request.ParallelToolCalls {
		return MiniMaxChatRequest{}, errors.New("MiniMax Chat Completions cannot disable parallel tool calls")
	}
	thinkingOn := request.ReasoningEffort != "" && request.ReasoningEffort != minimaxThinkingOffEffort
	// MiniMax has one output bound and it counts reasoning. max_completion_tokens
	// means the same thing and is carried as itself; max_tokens bounds the answer
	// alone, so it is the same quantity only while nothing is thinking. Renaming
	// it unconditionally would silently turn a caller's answer budget into a
	// budget over answer-plus-reasoning.
	limit := request.MaxCompletionTokens
	if request.MaxTokens != nil {
		switch {
		case request.MaxCompletionTokens != nil:
			return MiniMaxChatRequest{}, errors.New("MiniMax Chat Completions has one output limit and the request carries two")
		case thinkingOn:
			return MiniMaxChatRequest{}, errors.New("MiniMax Chat Completions cannot bound reasoning and answer with one limit")
		default:
			limit = request.MaxTokens
		}
	}
	result := MiniMaxChatRequest{
		Model: request.Model, Messages: request.Messages,
		MaxCompletionTokens: limit,
		Stream:              request.Stream, StreamOptions: request.StreamOptions,
		Temperature: request.Temperature, TopP: request.TopP,
		Tools: request.Tools, ToolChoice: request.ToolChoice,
	}
	switch {
	// Nothing asked for, so thinking is switched off rather than left to
	// MiniMax's own default, which is on.
	//
	// Leaving it to the provider looks like the neutral choice and is not: on
	// MiniMax-M3 the default is "adaptive", so a caller who never mentioned
	// reasoning pays for it on every request. DeepSeek reached this same
	// conclusion the expensive way, and the Anthropic portable mapper already
	// disables thinking on an unasked request, so matching them also removes a
	// difference that had nothing to do with the caller: the same portable
	// request would otherwise cost differently depending on which provider
	// routing happened to pick.
	//
	// What is deliberately loud here: MiniMax's M2.x line cannot switch thinking
	// off at all. Whether it answers a disabled switch with a refusal or by
	// ignoring it is not established, and this renderer cannot tell the families
	// apart — the model identifier is a string, and deriving behaviour from its
	// prefix is exactly what the model catalogue exists to avoid. If M2.x
	// refuses, every M2.x request fails visibly rather than quietly costing more
	// than it should, which is the side of that trade this repository takes.
	case request.ReasoningEffort == "", request.ReasoningEffort == minimaxThinkingOffEffort:
		result.Thinking = &MiniMaxThinking{Type: MiniMaxThinkingOff}
	// Every remaining rung reaches the one "on" state. The portable ladder has
	// six values and MiniMax's switch has two, so depth is not preserved — the
	// endpoint manifests declare that as a transform. Rounding is acceptable
	// here and was not for DeepSeek because MiniMax has no neighbouring rung to
	// round to: there is a single on state, not a coarser ladder.
	case slices.Contains(openaiapi.ReasoningEffortLevels, request.ReasoningEffort):
		result.Thinking = &MiniMaxThinking{Type: MiniMaxThinkingOn}
		// Without this, reasoning comes back inline in the answer and the caller
		// reads the model's thinking as part of its reply. With it, MiniMax puts
		// reasoning in reasoning_content, which the canonical mapper already
		// understands as a reasoning content part.
		result.ReasoningSplit = true
	default:
		return MiniMaxChatRequest{}, fmt.Errorf("MiniMax thinking does not accept reasoning effort %q", request.ReasoningEffort)
	}
	return result, nil
}
