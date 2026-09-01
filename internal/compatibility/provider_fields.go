package compatibility

import (
	"encoding/json"
	"slices"

	"github.com/akz142857/Halro/internal/anthropicapi"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

// portableEffortLevels is what a portable request can actually ask an Anthropic
// target for: the values Anthropic accepts, minus the ones that cannot survive
// the OpenAI intermediate representation every portable request passes through.
// Anthropic's `max` is the whole difference, and it is why this is an
// intersection rather than either list on its own.
var portableEffortLevels = intersectSorted(anthropicapi.EffortLevels, openaiapi.ReasoningEffortLevels)

func intersectSorted(left, right []string) []string {
	result := make([]string, 0, len(left))
	for _, value := range left {
		if slices.Contains(right, value) {
			result = append(result, value)
		}
	}
	return result
}

// generateFieldRules is what each profile declares it cannot carry, registered
// by profile rather than matched in a switch.
//
// A switch answers the question and hides whether it was asked: a profile added
// to the domain table and forgotten here fell to the legacy branch, which is
// fail-closed and therefore silent — the platform would run, serve plain text,
// and refuse tools, images and structured output with nothing to say why. A
// registry makes the omission enumerable, and TestEveryProfileIsRegistered
// names the profile and this file when one is missing.
var generateFieldRules = func() map[domain.ProviderProfileID]func(add fieldSink, request semantic.GenerateRequest) {
	rules := map[domain.ProviderProfileID]func(add fieldSink, request semantic.GenerateRequest){}
	register := func(rule func(add fieldSink, request semantic.GenerateRequest), profiles ...domain.ProviderProfileID) {
		for _, profileID := range profiles {
			rules[profileID] = rule
		}
	}
	register(func(add fieldSink, request semantic.GenerateRequest) {
		add(hasNamedMessage(request), "messages[].name")
		add(hasFailedToolResult(request), "messages[].content[].is_error")
		add(request.Seed != nil, "seed")
		add(len(request.Tools) > 0, "tools")
		add(request.ToolChoice != nil, "tool_choice")
		add(request.ParallelTools != nil, "parallel_tool_calls")
		add(request.OutputFormat != nil, "response_format")
		add(request.ReasoningEffort != "", "reasoning_effort")
		add(request.EndUserRef != "", "user")
	}, domain.ProfileGeminiText)
	register(func(add fieldSink, request semantic.GenerateRequest) {
		add(hasNamedMessage(request), "messages[].name")
		add(hasFailedToolResult(request), "messages[].content[].is_error")
		add(request.Candidates != nil && *request.Candidates > 1, "n")
		add(request.Seed != nil, "seed")
		add(len(request.Tools) > 0, "tools")
		add(request.ToolChoice != nil, "tool_choice")
		add(request.ParallelTools != nil, "parallel_tool_calls")
		add(request.OutputFormat != nil, "response_format")
		add(request.ReasoningEffort != "", "reasoning_effort")
		add(request.EndUserRef != "", "user")
	}, domain.ProfileBedrockConverseText)
	register(func(add fieldSink, request semantic.GenerateRequest) {
		add(hasNamedMessage(request), "messages[].name")
		// Anthropic's image source is base64, url, or file — there is no member
		// for OpenAI's fidelity hint. Writing it in anyway made the whole request
		// invalid; dropping it silently would change what the caller asked for and
		// what it costs them.
		add(hasImageDetail(request), "messages[].content[].detail")
		add(hasDeveloperMessage(request), "messages[].role=developer")
		add(request.Candidates != nil && *request.Candidates > 1, "n")
		add(request.Seed != nil, "seed")
		// Structured output is supported, but only as a schema: Anthropic has no
		// counterpart to the schema-less json_object mode, and it has no relaxed
		// mode either — a schema it is given is enforced.
		//
		// The first of those is also the json_object capability, absent from this
		// profile's ceiling, so routing has already dropped the target before this
		// runs. It is still declared here because this file is the renderer's
		// contract as much as it is routing's input: the manifest coverage tests
		// hold every field the wire cannot carry to a declaration, and a fact
		// stated in only one of the two layers is a fact the other can contradict.
		add(request.OutputFormat != nil && request.OutputFormat.Kind == semantic.OutputJSONObject, "response_format")
		add(request.OutputFormat != nil && request.OutputFormat.Kind == semantic.OutputJSONSchema && !request.OutputFormat.Strict, "response_format")
		// The ladder is bounded by the portable representation, not by Anthropic.
		// Every portable request reaches this profile through the OpenAI wire
		// form, whose reasoning_effort stops at xhigh, so declaring Anthropic's
		// own `max` as routable produced a request that passed capability
		// filtering and then failed while being rendered — after the budget
		// reservation, naming a field the caller never sent.
		add(request.ReasoningEffort != "" && !slices.Contains(portableEffortLevels, request.ReasoningEffort), "reasoning_effort")
		add(request.EndUserRef != "", "user")
	}, domain.ProfileAnthropicMessages)
	register(func(add fieldSink, request semantic.GenerateRequest) {
		add(hasImageDetail(request), "messages[].content[].detail")
		// The Mantle Beta profile shares this wire representation and could carry
		// output_config, but its capability ceiling is fixed by the build and
		// widening it is a separate contract review. Until that happens the
		// declared surface stays where it was.
		add(hasNamedMessage(request), "messages[].name")
		add(hasDeveloperMessage(request), "messages[].role=developer")
		add(request.Candidates != nil && *request.Candidates > 1, "n")
		add(request.Seed != nil, "seed")
		add(request.OutputFormat != nil, "response_format")
		add(request.ReasoningEffort != "", "reasoning_effort")
		add(request.EndUserRef != "", "user")
	}, domain.ProfileBedrockMantleAnthropicMessages)
	register(func(add fieldSink, request semantic.GenerateRequest) {
		add(hasFailedToolResult(request), "messages[].content[].is_error")
		// A Responses message item has no author name to put one in — the Name
		// field on the item carries a function's name, not a speaker's — so the
		// renderer drops it. Every other profile that cannot carry it says so;
		// this one did not, which made it the one branch that neither carried the
		// field nor declared the loss, and a multi-speaker conversation routed
		// here came back 200 with the speakers made indistinguishable.
		add(hasNamedMessage(request), "messages[].name")
		add(request.Candidates != nil && *request.Candidates > 1, "n")
		add(len(request.Stop) > 0, "stop")
		add(request.Seed != nil, "seed")
		add(request.Stream && len(request.Tools) > 0, "tools")
		add(request.ReasoningEffort != "", "reasoning_effort")
	}, domain.ProfileBedrockMantleResponses, domain.ProfileBedrockMantleOpenAIResponses)
	register(func(add fieldSink, request semantic.GenerateRequest) {
		// The direct OpenAI Responses surface. Its losses are the stateless
		// Responses form's, not this account's: an input item has no speaker name,
		// no stop list and no seed, and one request produces one response.
		add(hasFailedToolResult(request), "messages[].content[].is_error")
		add(hasNamedMessage(request), "messages[].name")
		add(request.Candidates != nil && *request.Candidates > 1, "n")
		add(len(request.Stop) > 0, "stop")
		add(request.Seed != nil, "seed")
		// Reasoning is declared rather than left to the capability filter alone.
		// The filter does drop this target for a request that asks to think — the
		// ceiling carries no reasoning — but a caller told "no route supports
		// this" learns less than one told which field cost them the route.
		add(request.ReasoningEffort != "", "reasoning_effort")
	}, domain.ProfileOpenAIResponses)
	register(func(add fieldSink, request semantic.GenerateRequest) {
		// DeepSeek speaks this wire format but accepts a smaller set of members
		// than OpenAI does; deepseek.go holds the list and the renderer that has
		// to agree with it. Sharing OpenAI's branch declared none of the gap, so
		// n, seed and parallel_tool_calls were sent to a surface that ignores
		// them and the caller got a 200 for a request that never happened as
		// written.
		add(hasFailedToolResult(request), "messages[].content[].is_error")
		add(request.Candidates != nil && *request.Candidates > 1, "n")
		add(request.Seed != nil, "seed")
		// Only the disable is a loss, and the guard is not cosmetic. Every
		// portable Messages request that names a tool_choice comes out of
		// DecodeToolChoice with this flag set — it always returns one, defaulting
		// to parallel-allowed — so declaring on presence routed DeepSeek away from
		// requests that expressed no preference at all. Parallel-allowed is what
		// omitting the member already gets, the way n=1 is; asking for tools to be
		// run one at a time is the thing DeepSeek has no member for.
		add(request.ParallelTools != nil && !*request.ParallelTools, "parallel_tool_calls")
		// Value-dependent, and the value is another field: a completion budget
		// counts reasoning tokens and DeepSeek's max_tokens does not, so the two
		// are the same bound exactly while thinking is off. Declaring it
		// unconditionally routed away every /v1/responses request that budgeted
		// its output — that endpoint rejects the `reasoning` field outright, so
		// its requests never think, and the limit it decodes is this one.
		// Carrying it when thinking is on would be the opposite mistake: a bound
		// the caller set over answer-plus-reasoning would silently become a bound
		// over the answer alone.
		//
		// Two output limits in one request stay declared, because DeepSeek has
		// one member for them and choosing between them is not this layer's call.
		add(request.CompletionTokenLimit != nil &&
			(deepSeekThinkingIsOn(request.ReasoningEffort) || request.VisibleOutputTokenLimit != nil),
			"max_completion_tokens")
		// DeepSeek has json_object and no schema mode. The structured_outputs
		// capability is what routes a schema request elsewhere; this is the same
		// fact stated to the renderer, which refuses the format on the wire and
		// must not be the only place that knows.
		add(request.OutputFormat != nil && request.OutputFormat.Kind == semantic.OutputJSONSchema, "response_format")
		// Value-dependent too. `none` reaches thinking.type=disabled and low/high
		// reach thinking.reasoning_effort; minimal, medium and xhigh have no
		// DeepSeek rung and are not rounded to a neighbouring one.
		add(request.ReasoningEffort != "" && !slices.Contains(deepSeekPortableEfforts, request.ReasoningEffort), "reasoning_effort")
		// `user` is absent on purpose. DeepSeek carries the same concept as
		// user_id, so it is renamed by the renderer rather than declared lost.
	}, domain.ProfileDeepSeekChat)
	register(func(add fieldSink, request semantic.GenerateRequest) {
		// These profiles use the OpenAI-compatible wire representation directly.
		// The one thing that representation has no place for is a tool result the
		// caller marked as failed: an OpenAI tool message is its text and nothing
		// else, so is_error would be dropped and the model would read a failure as
		// a successful answer.
		add(hasFailedToolResult(request), "messages[].content[].is_error")
	}, domain.ProfileOpenAIChatEmbeddings, domain.ProfileAzureChatEmbeddings, domain.ProfileOpenAICompatible)
	register(func(add fieldSink, request semantic.GenerateRequest) {
		// MiniMax's Anthropic face. It starts from the direct Anthropic profile's
		// losses because it is the same wire form, then adds what MiniMax alone
		// cannot carry.
		add(hasNamedMessage(request), "messages[].name")
		add(hasImageDetail(request), "messages[].content[].detail")
		add(hasDeveloperMessage(request), "messages[].role=developer")
		add(request.Candidates != nil && *request.Candidates > 1, "n")
		add(request.Seed != nil, "seed")
		// No JSON mode of either kind is documented on any MiniMax face. That is
		// documentation being silent rather than refusing, so it is treated as
		// absent — the fail-closed direction — until a real request says
		// otherwise. Both capability halves are off, so routing has already
		// dropped this target before the rule runs; it is still declared because
		// the manifest coverage tests hold every unrepresentable member to a
		// declaration, and a fact stated in only one layer is a fact the other
		// can contradict.
		add(request.OutputFormat != nil, "response_format")
		// Any depth at all. A portable request that asks for one reaches
		// output_config.effort, and MiniMax accepts no output_config — its
		// reasoning switch is the older thinking member. A request that asks for
		// nothing still gets thinking disabled, because the portable mapper emits
		// {"type":"disabled"} on its own and that is MiniMax's exact spelling, so
		// the capability is real and only the depth is unreachable.
		add(request.ReasoningEffort != "", "reasoning_effort")
		add(request.EndUserRef != "", "user")
		// MiniMax documents stop_sequences as ignored rather than refused. An
		// ignored member is worse than a refused one: the request comes back 200,
		// the caller pays for a completion that ran past the boundary they set,
		// and nothing in the chain says the boundary was dropped.
		add(len(request.Stop) > 0, "stop")
	}, domain.ProfileMiniMaxAnthropicMessages)
	register(func(add fieldSink, request semantic.GenerateRequest) {
		// MiniMax's two OpenAI-shaped faces. They share this rule because they
		// share the accepted set; where they differ is reasoning, which is
		// reachable on Responses under its OpenAI name and reachable on Chat only
		// through the dialect renderer in minimax.go.
		add(hasFailedToolResult(request), "messages[].content[].is_error")
		add(request.Candidates != nil && *request.Candidates > 1, "n")
		add(request.Seed != nil, "seed")
		add(len(request.Stop) > 0, "stop")
		// Value-dependent now that it has been measured: json_object is served
		// and a schema has not been established. The schema-less half was declared
		// unsupported on both because no MiniMax document names the member; a real
		// request answered one half of that and left the other where it was.
		add(request.OutputFormat != nil && request.OutputFormat.Kind == semantic.OutputJSONSchema, "response_format")
		add(request.EndUserRef != "", "user")
		add(request.ParallelTools != nil && !*request.ParallelTools, "parallel_tool_calls")
	}, domain.ProfileMiniMaxChat)
	register(func(add fieldSink, request semantic.GenerateRequest) {
		// The Responses face carries the Chat face's losses and two of its own.
		// It is written out rather than layered on top of the rule above because
		// register keys by profile, so a second registration for the same profile
		// would replace the first rather than add to it — the losses would
		// silently halve.
		add(hasFailedToolResult(request), "messages[].content[].is_error")
		add(request.Candidates != nil && *request.Candidates > 1, "n")
		add(request.Seed != nil, "seed")
		add(len(request.Stop) > 0, "stop")
		add(request.OutputFormat != nil, "response_format")
		add(request.EndUserRef != "", "user")
		add(request.ParallelTools != nil && !*request.ParallelTools, "parallel_tool_calls")
		// No reasoning at all. The capability ceiling already routes a thinking
		// request away — the canonical response mapper cannot carry reasoning
		// items — but a caller told "no route supports this" learns less than one
		// told which field cost them the route.
		add(request.ReasoningEffort != "", "reasoning_effort")
		// A Responses message item has no author name to put one in.
		add(hasNamedMessage(request), "messages[].name")
	}, domain.ProfileMiniMaxResponses)
	register(func(add fieldSink, request semantic.GenerateRequest) {
		// Bedrock's inability to fetch an image used to be declared here, once per
		// northbound endpoint, in each endpoint's own name for the same member.
		// Three spellings of one fact was the evidence that it did not belong in
		// this layer: it is not a property of a request field, it is a property of
		// the target. It is a capability now — fetched_image, absent from every
		// Mantle ceiling — so those requests are refused by the same filter that
		// refuses a target with no vision at all, and an operator sees the rule as
		// a checkbox instead of meeting it as a refusal.
		add(hasFailedToolResult(request), "messages[].content[].is_error")
	}, domain.ProfileBedrockMantleChat, domain.ProfileBedrockMantleOpenAIChat)
	return rules
}()

// legacyFieldRules is what an unregistered profile falls back to: permit the
// portable text core and refuse everything whose semantics an adapter could
// otherwise silently discard. It is a plain function rather than a registry
// entry so that "registered" and "fell back" stay distinguishable — the
// completeness test reads exactly that difference.
func legacyFieldRules(add fieldSink, request semantic.GenerateRequest) {
	// Legacy or extension adapters do not have profile-level proof for optional
	// fields. Permit only the portable text/model core and fail closed for
	// everything whose semantics an adapter could otherwise silently discard.
	add(hasNamedMessage(request), "messages[].name")
	add(hasDeveloperMessage(request), "messages[].role")
	add(hasNonTextContent(request), "messages[].content")
	add(hasFailedToolResult(request), "messages[].content[].is_error")
	// Scalar Chat Completions controls remain part of the legacy OpenAI-wire
	// adapter contract. Their conversion is declared (never lossless), while
	// richer optional semantics stay fail-closed until a profile is supplied.
	add(request.Seed != nil, "seed")
	add(len(request.Tools) > 0, "tools")
	add(request.ToolChoice != nil, "tool_choice")
	add(request.ParallelTools != nil, "parallel_tool_calls")
	add(request.OutputFormat != nil, "response_format")
	add(request.ReasoningEffort != "", "reasoning_effort")
	add(request.EndUserRef != "", "user")
	add(request.IncludeUsage, "stream_options")
}

// fieldSink records a field the profile cannot carry, once.
type fieldSink func(condition bool, field string)

// RegisteredGenerateProfiles lists the profiles that declare their own field
// rules, for the completeness test that holds the table and this file together.
func RegisteredGenerateProfiles() []domain.ProviderProfileID {
	profiles := make([]domain.ProviderProfileID, 0, len(generateFieldRules))
	for profileID := range generateFieldRules {
		profiles = append(profiles, profileID)
	}
	return profiles
}

// UnsupportedGenerateFields returns northbound fields that the selected
// provider profile cannot represent without silent semantic loss.
func UnsupportedGenerateFields(profileID domain.ProviderProfileID, request semantic.GenerateRequest) []string {
	var unsupported []string
	seen := map[string]struct{}{}
	add := func(condition bool, field string) {
		if _, exists := seen[field]; condition && !exists {
			unsupported = append(unsupported, field)
			seen[field] = struct{}{}
		}
	}
	if rule, registered := generateFieldRules[profileID]; registered {
		rule(add, request)
	} else {
		legacyFieldRules(add, request)
	}
	// Declared once for every profile rather than repeated in each rule set,
	// because the answer is the same everywhere but one: a tool the upstream runs
	// itself only exists on a surface that has such tools, and writing the same
	// negative into a dozen rule sets is a dozen places for the next profile to
	// be forgotten. The allowlist inverts that — a new profile carries it only by
	// being named here.
	add(hasProviderExecutedTool(request) && !slices.Contains(providerExecutedToolProfiles, profileID), "tools[].type")
	return unsupported
}

// providerExecutedToolProfiles are the profiles whose wire form can carry a tool
// the upstream runs itself.
//
// The Anthropic Messages profile is deliberately absent even though its ceiling
// permits the capability: Anthropic's own provider-executed tools reach it
// through native mode, where the request is forwarded as the caller wrote it.
// The portable path has no way to express them, so a portable request naming one
// is refused here rather than translated into a function declaration the model
// would try to call back about.
var providerExecutedToolProfiles = []domain.ProviderProfileID{domain.ProfileOpenAIResponses}

func hasProviderExecutedTool(request semantic.GenerateRequest) bool {
	for _, tool := range request.Tools {
		if tool.ProviderExecuted() {
			return true
		}
	}
	return false
}

// hasFailedToolResult reports a tool result the caller marked as failed. Only
// the Anthropic-wire profiles can carry it; everywhere else the flag would be
// dropped and the model would be told a failed tool call succeeded.
func hasFailedToolResult(request semantic.GenerateRequest) bool {
	for _, message := range request.Messages {
		for _, part := range message.Content {
			if part.Kind == semantic.ContentToolResult && part.ToolError {
				return true
			}
		}
	}
	return false
}

func hasNamedMessage(request semantic.GenerateRequest) bool {
	for _, message := range request.Messages {
		if message.Name != "" {
			return true
		}
	}
	return false
}

func hasDeveloperMessage(request semantic.GenerateRequest) bool {
	for _, message := range request.Messages {
		if message.Role == semantic.RoleDeveloper {
			return true
		}
	}
	return false
}

// hasImageDetail reports a fidelity hint that would be lost. OpenAI's default is
// auto, which is what omitting the member already means, so only a request that
// asked for something else is refused — the same rule the value-dependent fields
// on the DeepSeek profile follow.
func hasImageDetail(request semantic.GenerateRequest) bool {
	for _, message := range request.Messages {
		for _, part := range message.Content {
			if part.Kind == semantic.ContentInputImage && part.Detail != "" && part.Detail != "auto" {
				return true
			}
		}
	}
	return false
}

func hasNonTextContent(request semantic.GenerateRequest) bool {
	for _, message := range request.Messages {
		for _, part := range message.Content {
			if part.Kind != semantic.ContentText {
				return true
			}
		}
	}
	return false
}

// UnsupportedEmbeddingFields returns northbound embedding fields which would
// otherwise be ignored by the selected provider profile.
func UnsupportedEmbeddingFields(profileID domain.ProviderProfileID, request semantic.EmbeddingRequest) []string {
	switch profileID {
	case domain.ProfileOpenAIChatEmbeddings, domain.ProfileAzureChatEmbeddings, domain.ProfileOpenAICompatible:
		return nil
	case domain.ProfileGeminiText:
		var unsupported []string
		if request.Encoding != "" && request.Encoding != "float" {
			unsupported = append(unsupported, "encoding_format")
		}
		if request.EndUserRef != "" {
			unsupported = append(unsupported, "user")
		}
		return unsupported
	case domain.ProfileBedrockInvokeTitanEmbedV2:
		var unsupported []string
		var input string
		if json.Unmarshal(request.Input, &input) != nil {
			unsupported = append(unsupported, "input")
		}
		if request.Encoding != "" && request.Encoding != "float" {
			unsupported = append(unsupported, "encoding_format")
		}
		if request.Dimensions != nil && *request.Dimensions != 256 && *request.Dimensions != 512 && *request.Dimensions != 1024 {
			unsupported = append(unsupported, "dimensions")
		}
		if request.EndUserRef != "" {
			unsupported = append(unsupported, "user")
		}
		return unsupported
	default:
		var unsupported []string
		// Encoding and dimensions are part of the legacy embedding wire contract;
		// the unknown conversion is declared by the legacy primitive.
		if request.EndUserRef != "" {
			unsupported = append(unsupported, "user")
		}
		return unsupported
	}
}
