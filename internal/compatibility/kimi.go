package compatibility

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

// Kimi speaks the OpenAI Chat Completions wire format and accepts a smaller
// member list than OpenAI's — smaller in a direction no upstream Halro speaks
// to had taken before.
//
// Fields Kimi documents on /v1/chat/completions (platform.kimi.com and
// platform.kimi.ai, read 2026-09-01, from the published OpenAPI document rather
// than from prose):
//
//	model, messages, max_tokens (deprecated), max_completion_tokens,
//	response_format, stop, stream, stream_options, tools, tool_choice,
//	logprobs, top_logprobs, prediction, prompt_cache_key, safety_identifier,
//	plus reasoning_effort on kimi-k3 and thinking on the K2.x line
//
// What is absent from that list is the reason this file exists:
// temperature, top_p, n, presence_penalty and frequency_penalty are not
// members of Kimi's request schema at all, and its parameter reference says
// each is fixed and that sending any other value is an error. Halro's OpenAI
// request type carries temperature, top_p and n, so without a renderer they go
// on the wire and the request fails upstream — after the budget is reserved,
// on a request that cannot fall back because a bad request is not retryable.
//
// The other half is reasoning, and it is the part that has no counterpart on
// any other platform here: Kimi spells the switch two different ways depending
// on which model the request names, and its own OpenAPI document discriminates
// on the exact `model` string to decide which spelling applies. Matching exact
// identifiers is therefore following the upstream's own discriminator rather
// than inferring behaviour from a name — the distinction the model catalogue
// exists to protect, and the reason kimiReasoningSpelling holds exact strings
// and never a prefix.
//
// Measured against a real mainland account on 2026-09-01, and two of the
// documented facts did not survive it:
//
//   - The K2.x line accepts a top-level reasoning_effort (200), and kimi-k3
//     accepts a `thinking` member (200). Kimi's parameter reference says each is
//     unsupported on the other family. Neither was observed to *do* anything, so
//     this renderer still writes the spelling the model reads — an ignored
//     member is worse than a refused one, and picking the documented spelling is
//     the only way to know the switch was honoured.
//   - The single output bound counts reasoning. See RenderKimiChatRequest.
//
// What the measurement confirmed: temperature, top_p and n are refused at any
// value but the pinned one, with messages naming the pinned value; the stop
// bounds are enforced; and a retired model answers 404
// resource_not_found_error. See docs/prd/kimi-adaptation-plan.zh-CN.md §10.

// KimiEffortLevels is the depth ladder kimi-k3 accepts, and the whole of it.
// Kimi's default is "max"; a request that names no depth sends no member and
// gets that default.
var KimiEffortLevels = []string{"low", "high", "max"}

// KimiPortableEfforts is what a portable request can actually ask a Kimi target
// for. Each end comes from a different side:
//
//   - `low` and `high` are the rungs both ladders have.
//   - `max` is Kimi's and not portable: every portable request passes through
//     the OpenAI ladder, which stops at xhigh.
//   - `minimal`, `medium` and `xhigh` are the portable ladder's and have no Kimi
//     rung, so they are declared rather than rounded to a neighbour — rounding
//     would serve a depth, and a bill, the caller did not ask for.
//   - `none` is reachable and is prepended, because kimi-k3 and kimi-k2.6 both
//     take {"type":"disabled"} — measured 2026-09-01, after the documentation
//     had said kimi-k3 could not. It is not part of Kimi's own depth ladder,
//     which is why it is added rather than intersected: Kimi spells "off" as a
//     type, not as a rung.
//
// It is not reachable on every model. The kimi-k2.7-code pair answers `invalid
// thinking: only type=enabled is allowed for this model`, and an identifier this
// build does not know is refused as well, because there is no spelling to send
// it in. A field rule is keyed by profile and cannot tell the models apart, so
// those requests are admitted here and refused by the renderer — after the
// budget is reserved, which is the shape this repository normally refuses to
// accept. It is taken
// here because the alternative was worse in a way an operator feels: declaring
// `none` unsupported for the whole profile meant a caller explicitly asking for
// no reasoning was routed away, while a caller who said nothing got exactly
// that. Two of four models are served, one pays a clear error, and the gap is
// the same per-model one recorded against the output bound.
var KimiPortableEfforts = append([]string{kimiThinkingOff},
	intersectSorted(KimiEffortLevels, openaiapi.ReasoningEffortLevels)...)

// kimiThinkingOff is the portable ladder's name for "do not think", which the
// K2.x line spells as a thinking type and kimi-k3 cannot express at all.
const kimiThinkingOff = "none"

// kimiReasoning says how one exact model identifier spells its reasoning
// switch, and whether that model can be told not to reason.
//
// The identifiers are exact and complete as published on 2026-09-01. An
// identifier absent from this map is not assumed to behave like any of them: a
// request naming it carries no reasoning member at all, and one that asks for a
// depth is refused, because sending the wrong spelling to an unknown model is
// how a request that asked for no thinking gets billed for thinking.
type kimiReasoning struct {
	// TopLevelEffort is kimi-k3's spelling: a top-level reasoning_effort taking
	// this file's ladder.
	TopLevelEffort bool
	// ThinkingKeepAll is the K2.x coding line's constraint: `thinking` is
	// accepted only as {"type":"enabled","keep":"all"}, and any other
	// configuration is refused by the upstream.
	ThinkingKeepAll bool
	// CanDisable is false for every model that always reasons, which on
	// 2026-09-01 was the kimi-k2.7-code pair alone. kimi-k3 and kimi-k2.6 both
	// take an off switch — for kimi-k3 that was measured against its own
	// documentation and against its /v1/models metadata, both of which say it
	// always reasons and are both wrong. See the map below.
	CanDisable bool
}

var kimiReasoningSpelling = map[string]kimiReasoning{
	// kimi-k3 can be switched off, measured 2026-09-01. Both its documentation
	// ("K3 始终进行推理") and its own /v1/models metadata
	// (`supports_thinking_type: "only"`) say otherwise, and both are wrong:
	//
	//	{"model":"kimi-k3", ..., "thinking":{"type":"disabled"}}
	//	  -> 200, content "OK", reasoning_content "", no reasoning_tokens
	//	{"model":"kimi-k3", ...}                      (the same request without it)
	//	  -> 200, content "", 59 reasoning tokens, finish_reason "length"
	//
	// The second row is a caller paying a 64-token budget for no answer, which is
	// how this was found.
	"kimi-k3":   {TopLevelEffort: true, CanDisable: true},
	"kimi-k2.6": {CanDisable: true},
	// The one line that really cannot be switched off. Measured on the same run:
	// `invalid thinking: only type=enabled is allowed for this model`. Here the
	// documentation is right.
	"kimi-k2.7-code":           {ThinkingKeepAll: true},
	"kimi-k2.7-code-highspeed": {ThinkingKeepAll: true},
}

// KimiEffortAsksForDepth reports whether a caller asked this request to reason.
//
// It is the half of kimiThinkingWillBeOn that does not need a model, and it
// exists because the field rules are keyed by profile and have no target in
// hand while the renderer does. Both layers reading one definition is the point:
// the first version of the max_tokens rule spelled it `ReasoningEffort != ""`,
// which counts "none" as a request to think and routed away the one request that
// is cheapest to serve — an explicit ask for no reasoning, with a budget for the
// answer. MiniMax's equivalent rule already excluded "none"; this is the same
// predicate, named once.
func KimiEffortAsksForDepth(effort string) bool {
	return effort != "" && effort != kimiThinkingOff
}

// kimiThinkingWillBeOn reports whether the rendered body leaves the model
// reasoning. It is what tells the two output limits apart — with nothing
// thinking, a budget for the whole completion and a budget for the answer bound
// the same tokens — and the field rules and this renderer both have to agree
// about it, because a disagreement is a request the router admits and the
// renderer then refuses, after the budget is reserved.
//
// A model this build does not know is treated as reasoning, which is the
// fail-closed direction: an unknown identifier gets no off switch sent, so
// whatever Kimi's default is stands.
func kimiThinkingWillBeOn(model, effort string) bool {
	if KimiEffortAsksForDepth(effort) {
		return true
	}
	spelling, known := kimiReasoningSpelling[model]
	return !known || !spelling.CanDisable
}

// KimiThinking is the K2.x reasoning switch.
//
// Keep is a pointer-free string because both of its meaningful values are
// spelled: "all" preserves reasoning_content across turns, and the empty string
// omits the member, which is kimi-k2.6's default of not preserving it.
type KimiThinking struct {
	Type string `json:"type"`
	Keep string `json:"keep,omitempty"`
}

// KimiChatRequest is the accepted subset, written out rather than derived from
// the OpenAI request by omission: a member added to
// openaiapi.ChatCompletionRequest must be considered here deliberately instead
// of reaching Kimi because nobody remembered to exclude it.
//
// Absent by construction, and each absence is the point: temperature, top_p, n,
// seed, user and parallel_tool_calls.
type KimiChatRequest struct {
	Model    string              `json:"model,omitempty"`
	Messages []openaiapi.Message `json:"messages"`
	// MaxCompletionTokens is the member Kimi asks for, and the only output bound
	// it has. It counts reasoning, so an answer-only bound is never carried into
	// it — see RenderKimiChatRequest.
	MaxCompletionTokens *int64                   `json:"max_completion_tokens,omitempty"`
	ResponseFormat      json.RawMessage          `json:"response_format,omitempty"`
	Stop                json.RawMessage          `json:"stop,omitempty"`
	Stream              bool                     `json:"stream,omitempty"`
	StreamOptions       *openaiapi.StreamOptions `json:"stream_options,omitempty"`
	Tools               []openaiapi.Tool         `json:"tools,omitempty"`
	ToolChoice          json.RawMessage          `json:"tool_choice,omitempty"`
	ReasoningEffort     string                   `json:"reasoning_effort,omitempty"`
	Thinking            *KimiThinking            `json:"thinking,omitempty"`
}

// kimiStopLimits are Kimi's published bounds on the stop member: at most five
// strings, each at most 32 bytes. They are checked here rather than left to the
// upstream because a 400 from Kimi arrives after the reservation is taken.
const (
	kimiMaxStopSequences = 5
	kimiMaxStopBytes     = 32
)

// RenderKimiChatRequest converts an OpenAI-shaped request into the body Kimi
// accepts.
//
// It refuses rather than drops, the same rule the DeepSeek and MiniMax
// renderers follow. Routing already removes a Kimi target from the candidate set
// when the request carries one of these members, so in the running gateway most
// of these errors are unreachable; they are here so that a caller who reaches
// the adapter another way fails closed at the last moment instead of sending a
// request the caller did not make.
//
// Two of them are reachable, and deliberately so, because they depend on the
// model rather than on the profile and the field rules are keyed by profile:
// the reasoning spelling and tool_choice=required.
func RenderKimiChatRequest(request openaiapi.ChatCompletionRequest) (KimiChatRequest, error) {
	// The sampling parameters. Kimi pins each of these to one value per model and
	// answers any other with an error, so Halro refuses them here rather than
	// sending a value it cannot know is the pinned one — the alternative, quietly
	// substituting the pinned value, would serve the caller something other than
	// what they asked for.
	if request.Temperature != nil {
		return KimiChatRequest{}, errors.New("Kimi fixes temperature per model and does not accept it as a request member")
	}
	if request.TopP != nil {
		return KimiChatRequest{}, errors.New("Kimi fixes top_p at 0.95 and does not accept it as a request member")
	}
	if request.N != nil && *request.N > 1 {
		return KimiChatRequest{}, errors.New("Kimi Chat Completions accepts only n=1")
	}
	if request.Seed != nil {
		return KimiChatRequest{}, errors.New("Kimi Chat Completions does not accept seed")
	}
	if request.User != "" {
		return KimiChatRequest{}, errors.New("Kimi Chat Completions has no end-user attribution member")
	}
	// Parallel-allowed is what omitting the member already gets, so only the
	// disable has nowhere to go. Refusing on presence would refuse every portable
	// Messages request that names a tool_choice, because that path always
	// produces the flag.
	if request.ParallelToolCalls != nil && !*request.ParallelToolCalls {
		return KimiChatRequest{}, errors.New("Kimi Chat Completions cannot disable parallel tool calls")
	}
	// Kimi has one output bound and it counts reasoning — measured, not read:
	// max_completion_tokens=48 on kimi-k3 came back with 48 completion tokens, 45
	// of them reasoning, an empty answer and finish_reason=length. max_tokens
	// bounds the answer alone, so the two are the same quantity exactly while
	// nothing is thinking. Renaming it while something is would silently turn a
	// caller's answer budget into a budget over answer-plus-reasoning.
	//
	// Since this renderer switches reasoning off on an unasked request, that
	// condition is usually met, and an ordinary Chat client sending max_tokens
	// reaches Kimi rather than being routed away from it. The exception is
	// kimi-k2.7-code, which cannot be switched off: there the two bounds really
	// are different quantities and the request is refused.
	limit := request.MaxCompletionTokens
	if request.MaxTokens != nil {
		switch {
		case request.MaxCompletionTokens != nil:
			return KimiChatRequest{}, errors.New("Kimi has one output limit and the request carries two")
		case kimiThinkingWillBeOn(request.Model, request.ReasoningEffort):
			return KimiChatRequest{}, errors.New("Kimi's output bound counts reasoning on this model, so an answer-only max_tokens is not the same limit")
		default:
			limit = request.MaxTokens
		}
	}
	stop, err := renderKimiStop(request.Stop)
	if err != nil {
		return KimiChatRequest{}, err
	}
	responseFormat, err := renderKimiResponseFormat(request.ResponseFormat)
	if err != nil {
		return KimiChatRequest{}, err
	}
	result := KimiChatRequest{
		Model: request.Model, Messages: request.Messages,
		MaxCompletionTokens: limit,
		ResponseFormat:      responseFormat,
		Stop:                stop,
		Stream:              request.Stream, StreamOptions: request.StreamOptions,
		Tools: request.Tools, ToolChoice: request.ToolChoice,
	}
	if err := applyKimiToolChoice(request); err != nil {
		return KimiChatRequest{}, err
	}
	if err := applyKimiReasoning(&result, request.Model, request.ReasoningEffort); err != nil {
		return KimiChatRequest{}, err
	}
	return result, nil
}

// renderKimiStop checks the published bounds and passes the member through. Kimi
// documents stop as honoured — matched text stops the completion and is not
// emitted — which is why it is carried rather than refused the way MiniMax's is;
// MiniMax documents the same member as accepted and ignored, and an ignored
// boundary is billed as if it had been honoured.
func renderKimiStop(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var single string
	if json.Unmarshal(raw, &single) == nil {
		if err := checkKimiStopSequence(single); err != nil {
			return nil, err
		}
		return raw, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, fmt.Errorf("Kimi Chat Completions cannot read stop: %w", err)
	}
	if len(many) > kimiMaxStopSequences {
		return nil, fmt.Errorf("Kimi Chat Completions accepts at most %d stop sequences", kimiMaxStopSequences)
	}
	for _, value := range many {
		if err := checkKimiStopSequence(value); err != nil {
			return nil, err
		}
	}
	return raw, nil
}

func checkKimiStopSequence(value string) error {
	if len(value) > kimiMaxStopBytes {
		return fmt.Errorf("Kimi Chat Completions accepts stop sequences of at most %d bytes", kimiMaxStopBytes)
	}
	if !utf8.ValidString(value) {
		return errors.New("Kimi Chat Completions accepts only valid UTF-8 stop sequences")
	}
	return nil
}

// renderKimiResponseFormat carries what Kimi documents and refuses the rest.
//
// Both JSON halves are documented on this face — json_object and json_schema —
// which is why both capability bits are declared for the profile. `text` is the
// default and means the same thing as omitting the member, so it is dropped
// rather than sent.
func renderKimiResponseFormat(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var format struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &format); err != nil {
		return nil, fmt.Errorf("Kimi Chat Completions cannot read response_format: %w", err)
	}
	switch format.Type {
	case "text":
		return nil, nil
	case "json_object":
		// Re-emitted rather than forwarded, because the facade checks only the
		// type member and a caller's sibling members would otherwise ride along
		// into an object Kimi's schema does not describe.
		return json.RawMessage(`{"type":"json_object"}`), nil
	case "json_schema":
		// Forwarded whole: the schema itself is the payload, and re-authoring it
		// here would mean this layer deciding what a caller's schema means.
		return raw, nil
	default:
		return nil, fmt.Errorf("Kimi Chat Completions does not accept response_format type %q", format.Type)
	}
}

// applyKimiToolChoice enforces the one tool_choice constraint Kimi has, and it
// is keyed on whether the model will be reasoning rather than on which model was
// named.
//
// The first version of this function keyed on the model, because Kimi's
// parameter reference says the K2.x line does not support `required`. The
// measurement on 2026-09-01 refuted that in the upstream's own words:
//
//	tool_choice 'required' is incompatible with thinking enabled
//
// and the Anthropic face says the same thing about a named function
// (`tool_choice 'specified' is incompatible with thinking enabled`), while with
// thinking switched off a named tool call answered normally with a tool_use
// block. The conflict belongs to the reasoning switch — which this renderer sets
// — and not to the identifier.
//
// Keying it on the model was wrong in both directions, which is why this is a
// fix rather than a tidy-up:
//
//   - {kimi-k3, reasoning_effort: high, tool_choice: required} was rendered and
//     sent, and Kimi answered 400 after the budget was reserved.
//   - {kimi-k2.6, tool_choice: required} was refused here, though it is the
//     ordinary portable request: nothing asked for depth, so this renderer sends
//     thinking disabled and Kimi accepts the choice.
//
// The named-function half is the inferred one and is marked as such: it was
// measured on the Anthropic face, not on this one. It is refused while reasoning
// because the two faces are one upstream answering with one error family, and
// because the cost of being wrong is asymmetric — a caller who both asks for a
// depth and forces one specific tool gets a clear refusal instead of a 400
// charged against a reservation.
//
// Still reachable in the running gateway: whether reasoning ends up on depends
// on the model, and the field rules are keyed by profile with no target in hand.
// What the field rules can see — a request that names a depth and forces a tool,
// which fails on every Kimi model — is declared there and routed away first.
func applyKimiToolChoice(request openaiapi.ChatCompletionRequest) error {
	if len(request.ToolChoice) == 0 || !kimiForcesAToolCall(request.ToolChoice) {
		return nil
	}
	if !kimiThinkingWillBeOn(request.Model, request.ReasoningEffort) {
		return nil
	}
	return fmt.Errorf("Kimi refuses a forced tool call while model %q is reasoning", request.Model)
}

// kimiToolChoiceForces is the same question as kimiForcesAToolCall asked one
// layer earlier, where the request is still semantic and the field rules live.
// The two spellings exist because the two layers hold different representations
// of the same choice, and they are kept adjacent so neither can be changed
// alone: a disagreement between them is a request the router admits and the
// renderer then refuses, after the budget is reserved.
func kimiToolChoiceForces(choice *semantic.ToolChoice) bool {
	return choice != nil &&
		(choice.Mode == semantic.ToolChoiceRequired || choice.Mode == semantic.ToolChoiceNamed)
}

// kimiForcesAToolCall reports whether this tool_choice obliges the model to call
// a tool. `auto` and `none` do not and are never in question.
//
// The two shapes are the only two the facade admits: decodeToolChoice accepts
// the strings auto, none and required, or a {"type":"function"} object naming
// one, and renderToolChoice re-emits exactly those. A shape neither branch
// recognises is treated as forcing nothing, because it cannot have come from a
// caller.
func kimiForcesAToolCall(raw json.RawMessage) bool {
	var mode string
	if json.Unmarshal(raw, &mode) == nil {
		return mode == "required"
	}
	var named struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(raw, &named) == nil && named.Type == "function"
}

// applyKimiReasoning writes whichever reasoning member the named model reads.
//
// The table below is the whole contract, and each row is a measured fact rather
// than a choice made here:
//
//	model              effort ""          effort none        effort low/high/max
//	kimi-k3            thinking disabled  thinking disabled  reasoning_effort
//	kimi-k2.6          thinking disabled  thinking disabled  thinking enabled
//	kimi-k2.7-code     omit member        refuse             thinking enabled+keep
//	unknown            omit member        refuse             refuse
//
// **Unspecified means off**, which is the house rule the DeepSeek and MiniMax
// renderers already follow and which this file departed from in its first
// version. The departure was argued from Kimi's documentation — kimi-k3 was said
// to reason unconditionally, so there looked to be no off switch to send — and
// it produced a real failure on an operator's screen: Kimi reasoned on requests
// that never asked, the reasoning came back as a content part, and the two
// northbound endpoints that cannot render one answered 502 with the upstream
// already billed.
//
// The rule earns its place twice over. It removes that failure, and it removes a
// difference that has nothing to do with the caller: before it, the same
// portable request cost less on Anthropic than on Kimi because one of them
// thought and the other did not. Removing that difference is a large part of
// what a gateway is for. See docs/prd/deepseek-adaptation-plan.zh-CN.md §9.2,
// where the same conclusion was reached the same way.
//
// kimi-k2.7-code is the one line with no off state at all. It gets no member on
// an unasked request — there is nothing honest to send — so it keeps reasoning,
// and what that costs is recorded against the model rather than hidden here.
func applyKimiReasoning(result *KimiChatRequest, model, effort string) error {
	spelling, known := kimiReasoningSpelling[model]
	if effort == "" {
		// Nothing asked. Switch reasoning off where the model has a switch, and
		// send nothing where it does not — guessing a spelling for an unknown
		// identifier is how a request that asked for nothing gets billed for
		// reasoning it never wanted.
		if known && spelling.CanDisable {
			result.Thinking = &KimiThinking{Type: "disabled"}
		}
		return nil
	}
	if !known {
		return fmt.Errorf("Kimi model %q has no known reasoning spelling, so a reasoning request cannot be rendered for it", model)
	}
	if effort == kimiThinkingOff {
		if !spelling.CanDisable {
			return fmt.Errorf("Kimi model %q always reasons and cannot be asked not to", model)
		}
		result.Thinking = &KimiThinking{Type: "disabled"}
		return nil
	}
	if !slices.Contains(KimiEffortLevels, effort) {
		return fmt.Errorf("Kimi does not accept reasoning effort %q", effort)
	}
	switch {
	case spelling.TopLevelEffort:
		result.ReasoningEffort = effort
	case spelling.ThinkingKeepAll:
		// The only configuration this line accepts. Depth is not preserved — the
		// K2.x switch has one on state and no ladder to round to — and the
		// endpoint manifest declares that as a transform.
		result.Thinking = &KimiThinking{Type: "enabled", Keep: "all"}
	default:
		result.Thinking = &KimiThinking{Type: "enabled"}
	}
	return nil
}
