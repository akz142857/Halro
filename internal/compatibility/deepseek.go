package compatibility

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

// DeepSeek speaks the OpenAI Chat Completions wire format, but it accepts a
// smaller set of members than OpenAI does and spells two of them differently.
// This file is the one place that says which is which, so the declaration in
// UnsupportedGenerateFields and the body actually put on the wire cannot drift
// apart: every member the declaration lets through has to be one
// RenderDeepSeekChatRequest can carry, and every member it cannot carry has to
// be one the declaration stops.
//
// Fields DeepSeek documents on Chat Completions (api-docs.deepseek.com,
// reviewed again 2026-11-03):
//
//	messages, model, thinking, reasoning_effort, max_tokens, response_format, stop, stream,
//	stream_options, temperature, top_p, tools, tool_choice, logprobs,
//	top_logprobs, user_id
//
// frequency_penalty and presence_penalty are still accepted and documented as
// having no effect. Halro never sends them — its OpenAI request type has no
// such members — so there is nothing here to declare.
//
// response_format takes text and json_object. There is no schema mode, which
// is why the declaration below is value-dependent rather than a plain "yes".
//
// Sharing OpenAI's adapter is why the gap went unnoticed: n, seed,
// parallel_tool_calls and max_completion_tokens were rendered
// onto a surface that accepts none of them, and `user` was sent under a name
// DeepSeek does not read. The first group is declared unsupported so routing
// moves the request rather than letting it come back 200 with the field
// silently ignored; the second is a rename, which is carried rather than
// declared.
//
// max_completion_tokens belongs to both groups depending on the request:
// carried as max_tokens while thinking is off, because the two then bound the
// same tokens, and declared unsupported while thinking is on, because they do
// not.
//
// Everything here is read from DeepSeek's published API documentation and has
// not been confirmed against a live account. See
// docs/prd/deepseek-adaptation-plan.zh-CN.md.

// DeepSeekEffortLevels is the depth ladder DeepSeek accepts once thinking is
// on, as a top-level member. Off is handled through thinking.type, so `none`
// is handled separately below rather than being looked up here.
var DeepSeekEffortLevels = []string{"low", "high", "max"}

// deepSeekPortableEfforts is what a portable request can actually ask a DeepSeek
// target for. Each end of it comes from a different side:
//
//   - `none` is reachable, because thinking.type takes "disabled".
//   - `low` and `high` are the rungs both ladders have.
//   - `max` is portable now that the OpenAI request ladder carries it.
//   - `minimal`, `medium` and `xhigh` are the portable ladder's and have no
//     DeepSeek rung, so they are declared rather than rounded to a neighbour —
//     rounding would serve a depth, and a bill, the caller did not ask for.
var deepSeekPortableEfforts = append([]string{deepSeekThinkingOff},
	intersectSorted(DeepSeekEffortLevels, openaiapi.ReasoningEffortLevels)...)

// deepSeekThinkingOff is the portable ladder's name for "do not think", which
// DeepSeek spells as a type rather than a depth.
const deepSeekThinkingOff = "none"

// deepSeekThinkingIsOn reports whether the rendered body will have thinking
// switched on. It is what tells the two output limits apart: with nothing
// thinking, a budget for the whole completion and a budget for the answer bound
// the same tokens.
func deepSeekThinkingIsOn(effort string) bool {
	return effort != "" && effort != deepSeekThinkingOff
}

// DeepSeekThinking controls the mode. As of the 2026-11-03 Chat contract,
// reasoning_effort is a sibling of thinking, not a member inside it.
type DeepSeekThinking struct {
	Type string `json:"type"`
}

// DeepSeekChatRequest is the accepted subset, written out rather than derived
// from the OpenAI request by omission: a new member added to
// openaiapi.ChatCompletionRequest must be considered here deliberately instead
// of reaching DeepSeek because nobody remembered to exclude it.
type DeepSeekChatRequest struct {
	Model           string                   `json:"model,omitempty"`
	Messages        []openaiapi.Message      `json:"messages"`
	Thinking        *DeepSeekThinking        `json:"thinking,omitempty"`
	ReasoningEffort string                   `json:"reasoning_effort,omitempty"`
	MaxTokens       *int64                   `json:"max_tokens,omitempty"`
	ResponseFormat  json.RawMessage          `json:"response_format,omitempty"`
	Stop            json.RawMessage          `json:"stop,omitempty"`
	Stream          bool                     `json:"stream,omitempty"`
	StreamOptions   *openaiapi.StreamOptions `json:"stream_options,omitempty"`
	Temperature     *float64                 `json:"temperature,omitempty"`
	TopP            *float64                 `json:"top_p,omitempty"`
	Tools           []openaiapi.Tool         `json:"tools,omitempty"`
	ToolChoice      json.RawMessage          `json:"tool_choice,omitempty"`
	UserID          string                   `json:"user_id,omitempty"`
}

// RenderDeepSeekChatRequest converts an OpenAI-shaped request into the body
// DeepSeek accepts.
//
// It refuses rather than drops. Routing already removes a DeepSeek target from
// the candidate set when the request carries one of these members, so in the
// running gateway this error is unreachable; it is here so that a caller who
// reaches the adapter another way fails closed at the last moment instead of
// sending a request the caller did not make.
func RenderDeepSeekChatRequest(request openaiapi.ChatCompletionRequest) (DeepSeekChatRequest, error) {
	toolCalling := len(request.Tools) > 0
	var choice string
	if json.Unmarshal(request.ToolChoice, &choice) == nil && choice == "none" {
		toolCalling = false
	}
	if fields := deepSeekUnsupportedSampling(request.Model, request.ReasoningEffort, request.Temperature, request.TopP, toolCalling); len(fields) > 0 {
		return DeepSeekChatRequest{}, fmt.Errorf("DeepSeek cannot preserve sampling controls %v for this model and mode", fields)
	}

	if request.N != nil && *request.N > 1 {
		return DeepSeekChatRequest{}, errors.New("DeepSeek Chat Completions does not accept n")
	}
	if request.Seed != nil {
		return DeepSeekChatRequest{}, errors.New("DeepSeek Chat Completions does not accept seed")
	}
	// Parallel-allowed is what omitting the member already gets, so only the
	// disable has nowhere to go. Refusing on presence would have refused every
	// portable Messages request carrying a tool_choice, because that path always
	// produces the flag.
	if request.ParallelToolCalls != nil && !*request.ParallelToolCalls {
		return DeepSeekChatRequest{}, errors.New("DeepSeek Chat Completions cannot disable parallel tool calls")
	}
	// max_completion_tokens is a budget for everything a completion generates,
	// reasoning included; DeepSeek's max_tokens bounds the answer. Those are two
	// different quantities while thinking is on, and one quantity while it is
	// off — so the limit is carried in the second case and refused in the first,
	// rather than renamed unconditionally into a limit that means something else.
	//
	// The off case is not a corner: /v1/responses rejects the `reasoning` request
	// field outright, so every request arriving from there has thinking switched
	// off, and refusing the limit there took DeepSeek out of the candidate set
	// for any Responses request that budgeted its output at all.
	//
	// Two limits at once has no single member to land in, and picking one would
	// be the silent rewrite this file exists to prevent.
	maxTokens := request.MaxTokens
	if request.MaxCompletionTokens != nil {
		switch {
		case deepSeekThinkingIsOn(request.ReasoningEffort):
			return DeepSeekChatRequest{}, errors.New("DeepSeek Chat Completions cannot bound reasoning and answer with one limit")
		case request.MaxTokens != nil:
			return DeepSeekChatRequest{}, errors.New("DeepSeek Chat Completions has one output limit and the request carries two")
		default:
			maxTokens = request.MaxCompletionTokens
		}
	}
	result := DeepSeekChatRequest{
		Model: request.Model, Messages: request.Messages,
		MaxTokens: maxTokens, ResponseFormat: request.ResponseFormat,
		Stop: request.Stop, Stream: request.Stream, StreamOptions: request.StreamOptions,
		Temperature: request.Temperature, TopP: request.TopP,
		Tools: request.Tools, ToolChoice: request.ToolChoice,
		// DeepSeek has the end-user concept, under a different name. Sending
		// `user` was the worst of the three outcomes: neither declared nor
		// carried, so the caller's attribution was accepted and then discarded.
		UserID: request.User,
	}
	if len(request.ResponseFormat) > 0 {
		var format struct {
			Type string `json:"type"`
		}
		// DeepSeek takes text and json_object and has no schema mode. Sending a
		// schema would be refused after the budget was reserved, naming a field
		// the caller did in fact write — the case the declaration exists to stop.
		if json.Unmarshal(request.ResponseFormat, &format) != nil || (format.Type != "text" && format.Type != "json_object") {
			return DeepSeekChatRequest{}, fmt.Errorf("DeepSeek Chat Completions does not accept response_format type %q", format.Type)
		}
	}
	switch {
	// Nothing asked for, so thinking is switched off rather than left to
	// DeepSeek's own default, which is on at effort "high".
	//
	// Earlier versions of the Responses facade could not render reasoning.
	// Keep the established default-off contract even though that facade now
	// carries reasoning, so upgrading Halro does not change an unasked request's
	// provider cost or latency.
	// This is the same reasoning as the Anthropic profile's, which disables
	// thinking on an unasked request for its own version of the same failure
	// (compatibility/anthropic/mapping.go). Matching it also removes a difference
	// that had nothing to do with the caller: the same portable request cost and
	// behaved differently depending on which provider routing happened to pick.
	//
	// Reasoning stays available on DeepSeek by asking for it — reasoning_effort
	// on Chat Completions, output_config.effort on Messages — which is what the
	// low and high rungs below are.
	case request.ReasoningEffort == "":
		result.Thinking = &DeepSeekThinking{Type: "disabled"}
	case request.ReasoningEffort == deepSeekThinkingOff:
		result.Thinking = &DeepSeekThinking{Type: "disabled"}
	case slices.Contains(DeepSeekEffortLevels, request.ReasoningEffort):
		result.Thinking = &DeepSeekThinking{Type: "enabled"}
		result.ReasoningEffort = request.ReasoningEffort
	default:
		return DeepSeekChatRequest{}, fmt.Errorf("DeepSeek thinking does not accept reasoning effort %q", request.ReasoningEffort)
	}
	return result, nil
}

// DeepSeekTargetUnsupportedGenerateFields refuses controls that the documented
// model/mode would ignore or clamp, using the same rules as the wire renderer.
func DeepSeekTargetUnsupportedGenerateFields(model string, request semantic.GenerateRequest) []string {
	toolCalling := len(request.Tools) > 0 && (request.ToolChoice == nil || request.ToolChoice.Mode != semantic.ToolChoiceNone)
	return deepSeekUnsupportedSampling(model, request.ReasoningEffort, request.Temperature, request.TopP, toolCalling)
}

func deepSeekUnsupportedSampling(model, effort string, temperature, topP *float64, toolCalling bool) []string {
	var fields []string
	invalidTopP := topP != nil && (*topP <= 0 || *topP > 1)
	if invalidTopP {
		fields = append(fields, "top_p")
	}
	flash := false
	switch model {
	case "deepseek-flash", "deepseek-v4-flash", "deepseek-v4-flash-vision-exp":
		flash = true
	case "deepseek-v4-pro":
	default:
		return fields
	}
	thinking := deepSeekThinkingIsOn(effort)
	if thinking && temperature != nil {
		fields = append(fields, "temperature")
	}
	if topP != nil && !invalidTopP {
		unsupported := false
		if flash {
			if thinking {
				unsupported = *topP < 0.95
			} else {
				unsupported = *topP != 1
			}
		} else if thinking {
			fixed := 1.0
			if toolCalling {
				fixed = 0.95
			}
			unsupported = *topP != fixed
		}
		if unsupported {
			fields = append(fields, "top_p")
		}
	}
	return fields
}
