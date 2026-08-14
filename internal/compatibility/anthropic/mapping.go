package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/akz142857/Halro/internal/anthropicapi"
	"github.com/akz142857/Halro/internal/compatibility"
	"github.com/akz142857/Halro/internal/semantic"
)

const MappingRevision uint64 = 1

// defaultMaxTokens is what a request that named no output ceiling is given.
// Anthropic requires the field, so there has to be a number; it applies only
// when the caller supplied neither max_completion_tokens nor max_tokens.
const defaultMaxTokens int64 = 1024

func DecodePortable(request anthropicapi.MessageRequest) (semantic.GenerateRequest, error) {
	if len(request.Thinking) > 0 || len(request.Metadata) > 0 || request.ServiceTier != "" || request.TopK != nil {
		return semantic.GenerateRequest{}, errors.New("request contains Anthropic-native fields")
	}
	result := semantic.GenerateRequest{
		Operation: semantic.OperationGenerate,
		Source:    semantic.Source{ProfileID: string(compatibility.ProfileAnthropicMessages), ProfileRevision: 1},
		Mode:      semantic.ModePortable, RequestedModel: request.Model, Stream: request.Stream,
		Temperature: request.Temperature, TopP: request.TopP, Stop: append([]string(nil), request.StopSequences...),
		VisibleOutputTokenLimit: cloneInt64(&request.MaxTokens), IncludeUsage: request.Stream,
	}
	system, err := decodeSystem(request.System)
	if err != nil {
		return semantic.GenerateRequest{}, err
	}
	if len(system) > 0 {
		result.Messages = append(result.Messages, semantic.Message{Role: semantic.RoleSystem, Content: system})
	}
	for _, message := range request.Messages {
		mapped, err := decodeMessage(message)
		if err != nil {
			return semantic.GenerateRequest{}, err
		}
		result.Messages = append(result.Messages, mapped...)
	}
	for _, tool := range request.Tools {
		if tool.Strict != nil && *tool.Strict {
			return semantic.GenerateRequest{}, errors.New("strict Anthropic tools are not portable")
		}
		if tool.IsAnthropicDefined() {
			return semantic.GenerateRequest{}, errors.New("Anthropic-defined tools are not portable")
		}
		// The portable projection rewrites the request body, so a member it does
		// not model is not passed through — it is dropped. Refusing is the only
		// honest answer: cache_control changes what the caller pays, defer_loading
		// changes when the tool is read, and silently discarding either produces a
		// request the caller did not make.
		if unknown := tool.UnknownMembers(); len(unknown) > 0 {
			return semantic.GenerateRequest{}, fmt.Errorf("tool %q declares %s, which the portable projection cannot carry", tool.Name, strings.Join(unknown, ", "))
		}
		result.Tools = append(result.Tools, semantic.Tool{Name: tool.Name, Description: tool.Description, Schema: append(json.RawMessage(nil), tool.InputSchema...)})
	}
	if request.OutputConfig != nil {
		if unknown := request.OutputConfig.UnknownMembers(); len(unknown) > 0 {
			return semantic.GenerateRequest{}, fmt.Errorf("output_config declares %s, which the portable projection cannot carry", strings.Join(unknown, ", "))
		}
		result.ReasoningEffort = request.OutputConfig.Effort
		format, err := decodeOutputFormat(request.OutputConfig.Format)
		if err != nil {
			return semantic.GenerateRequest{}, err
		}
		result.OutputFormat = format
	}
	if request.ToolChoice != nil {
		choice, parallel, err := compatibility.DecodeToolChoice(compatibility.ToolChoiceWire{
			Protocol: compatibility.ToolProtocolAnthropic, Mode: request.ToolChoice.Type,
			NamedTool: request.ToolChoice.Name, ParallelAllowed: !request.ToolChoice.DisableParallelToolUse,
		})
		if err != nil {
			return semantic.GenerateRequest{}, err
		}
		result.ToolChoice, result.ParallelTools = &choice, parallel
	}
	result.Requirements = result.DeriveRequirements()
	if err := result.Validate(); err != nil {
		return semantic.GenerateRequest{}, err
	}
	return result, nil
}

func decodeSystem(raw json.RawMessage) ([]semantic.Content, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []semantic.Content{{Kind: semantic.ContentText, Text: text}}, nil
	}
	var blocks anthropicapi.ContentBlocks
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, errors.New("system must be text or text blocks")
	}
	result := make([]semantic.Content, 0, len(blocks))
	for _, block := range blocks {
		if block.Type != "text" {
			return nil, errors.New("portable system supports text blocks only")
		}
		if unknown := block.UnknownMembers(); len(unknown) > 0 {
			return nil, fmt.Errorf("system block declares %s, which the portable projection cannot carry", strings.Join(unknown, ", "))
		}
		result = append(result, semantic.Content{Kind: semantic.ContentText, Text: block.Text})
	}
	return result, nil
}

func decodeMessage(message anthropicapi.MessageParam) ([]semantic.Message, error) {
	role := semantic.RoleUser
	if message.Role == "assistant" {
		role = semantic.RoleAssistant
	}
	current := semantic.Message{Role: role}
	result := make([]semantic.Message, 0, 2)
	flush := func() {
		if len(current.Content) > 0 {
			result = append(result, current)
			current = semantic.Message{Role: role}
		}
	}
	for _, block := range message.Content {
		// The same rule tools[] follows, applied where the caller writes far more
		// of them: a portable request is re-authored, so a member this projection
		// does not read is dropped rather than forwarded. Refusing cache_control
		// here and rejecting it on tools[] is one rule; refusing it there and
		// dropping it here was one request body answering two ways.
		if unknown := block.UnknownMembers(); len(unknown) > 0 {
			return nil, fmt.Errorf("content block %q declares %s, which the portable projection cannot carry", block.Type, strings.Join(unknown, ", "))
		}
		switch block.Type {
		case "text":
			current.Content = append(current.Content, semantic.Content{Kind: semantic.ContentText, Text: block.Text})
		case "image":
			url, detail, err := decodeURLSource(block.Source)
			if err != nil {
				return nil, err
			}
			current.Content = append(current.Content, semantic.Content{Kind: semantic.ContentInputImage, URL: url, Detail: detail})
		case "tool_use":
			current.Content = append(current.Content, semantic.Content{Kind: semantic.ContentToolCall, CallID: block.ID, Name: block.Name, Arguments: string(block.Input)})
		case "tool_result":
			flush()
			text, err := decodeToolResult(block.Content)
			if err != nil {
				return nil, err
			}
			result = append(result, semantic.Message{Role: semantic.RoleTool, Content: []semantic.Content{{Kind: semantic.ContentToolResult, CallID: block.ToolUseID, Text: text, ToolError: block.IsError}}})
		case "thinking", "redacted_thinking":
			return nil, errors.New("signed thinking blocks require native mode")
		default:
			return nil, fmt.Errorf("content block %q is not portable", block.Type)
		}
	}
	flush()
	if len(result) == 0 {
		return nil, errors.New("message has no portable content")
	}
	return result, nil
}

func decodeURLSource(raw json.RawMessage) (string, string, error) {
	var source struct{ Type, URL, Detail string }
	if err := json.Unmarshal(raw, &source); err != nil || source.Type != "url" || strings.TrimSpace(source.URL) == "" {
		return "", "", errors.New("portable image input requires an Anthropic URL source")
	}
	return source.URL, source.Detail, nil
}

func decodeToolResult(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	var blocks anthropicapi.ContentBlocks
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", errors.New("portable tool_result must be text")
	}
	var joined strings.Builder
	for _, block := range blocks {
		if block.Type != "text" {
			return "", errors.New("portable tool_result supports text blocks only")
		}
		if unknown := block.UnknownMembers(); len(unknown) > 0 {
			return "", fmt.Errorf("tool_result block declares %s, which the portable projection cannot carry", strings.Join(unknown, ", "))
		}
		joined.WriteString(block.Text)
	}
	return joined.String(), nil
}

func RenderResult(result semantic.GenerateResult, publicModel string) (anthropicapi.Message, error) {
	if err := result.Validate(); err != nil {
		return anthropicapi.Message{}, err
	}
	if len(result.Choices) != 1 {
		return anthropicapi.Message{}, errors.New("Anthropic Messages requires exactly one output")
	}
	output := result.Choices[0]
	content := make(anthropicapi.ContentBlocks, 0, len(output.Message.Content))
	for _, part := range output.Message.Content {
		switch part.Kind {
		case semantic.ContentText:
			content = append(content, anthropicapi.ContentBlock{Type: "text", Text: part.Text})
		case semantic.ContentToolCall:
			input := json.RawMessage(part.Arguments)
			if !json.Valid(input) {
				return anthropicapi.Message{}, errors.New("provider returned invalid tool input")
			}
			content = append(content, anthropicapi.ContentBlock{Type: "tool_use", ID: part.CallID, Name: part.Name, Input: input})
		default:
			return anthropicapi.Message{}, errors.New("provider result contains non-portable content")
		}
	}
	stop := renderStopReason(output.Termination)
	message := anthropicapi.Message{ID: anthropicID(result.ID), Type: "message", Role: "assistant", Content: content, Model: publicModel, StopReason: &stop}
	if result.Usage != nil {
		message.Usage = renderUsage(*result.Usage)
	}
	return message, nil
}

// renderStopReason is DecodeStopReason's inverse, and reads the same semantic
// vocabulary. It too used to expect OpenAI's words, so a semantic "max_output"
// or "tool_call" fell through to end_turn and told the caller the turn had
// finished normally when it had been cut short or had asked for a tool.
func renderStopReason(reason string) string {
	switch reason {
	case "max_output":
		return "max_tokens"
	case "tool_call":
		return "tool_use"
	case "refusal":
		return "refusal"
	default:
		return "end_turn"
	}
}

func renderUsage(usage semantic.Usage) anthropicapi.Usage {
	return anthropicapi.Usage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens}
}

func anthropicID(id string) string {
	if strings.HasPrefix(id, "msg_") {
		return id
	}
	return "msg_" + strings.TrimPrefix(id, "chatcmpl-")
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func RawEqual(left, right json.RawMessage) bool { return bytes.Equal(left, right) }

func RenderPortableRequest(request semantic.GenerateRequest, providerModel string) (anthropicapi.MessageRequest, error) {
	if err := request.Validate(); err != nil {
		return anthropicapi.MessageRequest{}, err
	}
	result := anthropicapi.MessageRequest{Model: providerModel, Stream: request.Stream, StopSequences: append([]string(nil), request.Stop...), Temperature: request.Temperature, TopP: request.TopP}
	// Anthropic's max_tokens is required, so a request that named no ceiling gets
	// the fallback below. A request that named one gets the one it named: reading
	// only the visible limit meant max_completion_tokens — the sole output ceiling
	// on the Responses surface — was replaced by a fallback the caller never
	// wrote, silently raising a 64-token ceiling to 1024. Bedrock reads the two in
	// this order for the same reason.
	if request.CompletionTokenLimit != nil && *request.CompletionTokenLimit > 0 {
		result.MaxTokens = *request.CompletionTokenLimit
	} else if request.VisibleOutputTokenLimit != nil {
		result.MaxTokens = *request.VisibleOutputTokenLimit
	}
	if result.MaxTokens == 0 {
		result.MaxTokens = defaultMaxTokens
	}
	for _, message := range request.Messages {
		if message.Role == semantic.RoleSystem || message.Role == semantic.RoleDeveloper {
			if len(result.Messages) > 0 {
				return anthropicapi.MessageRequest{}, errors.New("system instructions must precede messages")
			}
			var text strings.Builder
			for _, part := range message.Content {
				if part.Kind != semantic.ContentText {
					return anthropicapi.MessageRequest{}, errors.New("Anthropic system supports portable text only")
				}
				text.WriteString(part.Text)
			}
			encoded, _ := json.Marshal(text.String())
			if len(result.System) == 0 {
				result.System = encoded
			} else {
				var existing string
				_ = json.Unmarshal(result.System, &existing)
				encoded, _ = json.Marshal(existing + "\n" + text.String())
				result.System = encoded
			}
			continue
		}
		mapped, err := renderMessage(message)
		if err != nil {
			return anthropicapi.MessageRequest{}, err
		}
		result.Messages = append(result.Messages, mapped)
	}
	for _, tool := range request.Tools {
		result.Tools = append(result.Tools, anthropicapi.Tool{Name: tool.Name, Description: tool.Description, InputSchema: append(json.RawMessage(nil), tool.Schema...)})
	}
	parallel := true
	if request.ParallelTools != nil {
		parallel = *request.ParallelTools
	}
	// Anthropic keeps the parallel switch inside tool_choice, so a caller who
	// sent parallel_tool_calls: false and no tool_choice used to have the
	// constraint disappear on the way out — the only branch that rendered it was
	// the one this request did not take. "auto" is what Anthropic already does
	// when tools are present and no choice is named, so saying it here adds the
	// switch without deciding anything else on the caller's behalf. With no tools
	// there is nothing to run in parallel and nothing to say.
	if request.ToolChoice == nil && !parallel && len(request.Tools) > 0 {
		result.ToolChoice = &anthropicapi.ToolChoice{Type: "auto", DisableParallelToolUse: true}
	}
	if request.ToolChoice != nil {
		wire, err := compatibility.RenderToolChoice(*request.ToolChoice, parallel, compatibility.ToolProtocolAnthropic)
		if err != nil {
			return anthropicapi.MessageRequest{}, err
		}
		result.ToolChoice = &anthropicapi.ToolChoice{Type: wire.Mode, Name: wire.NamedTool, DisableParallelToolUse: !wire.ParallelAllowed}
	}
	// Adaptive-thinking models decide for themselves whether to think, and the
	// current generation does it by default: a request that says nothing about
	// thinking comes back carrying signed thinking blocks. The portable surface
	// cannot return those — a thinking block's signature has to be handed back
	// verbatim on the next turn, and there is nowhere in an OpenAI-shaped
	// response to keep it — so DecodeResult refuses the response and the caller
	// gets a 502 for a request the upstream executed and billed.
	//
	// Not asking is the only honest answer available here. Reasoning that was
	// explicitly requested is left alone: the caller asked for depth, and
	// disabling it to make the response decodable would quietly serve them
	// something other than what they asked for. Those requests still need the
	// native Messages surface on a model that thinks.
	if request.ReasoningEffort == "" {
		result.Thinking = json.RawMessage(`{"type":"disabled"}`)
	}
	if request.ReasoningEffort != "" || request.OutputFormat != nil {
		config := &anthropicapi.OutputConfig{Effort: request.ReasoningEffort}
		if request.OutputFormat != nil {
			format, err := renderOutputFormat(*request.OutputFormat)
			if err != nil {
				return anthropicapi.MessageRequest{}, err
			}
			config.Format = format
		}
		result.OutputConfig = config
	}
	return result, result.Validate()
}

// decodeOutputFormat maps Anthropic's output_config.format onto the portable
// output format. Anthropic expresses structured output as a JSON schema only —
// it has no counterpart to OpenAI's schema-less json_object mode — so json_schema
// and text are the whole portable surface here.
func decodeOutputFormat(raw json.RawMessage) (*semantic.OutputFormat, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var format struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Schema      json.RawMessage `json:"schema"`
	}
	if err := json.Unmarshal(raw, &format); err != nil {
		return nil, errors.New("invalid output_config format")
	}
	switch format.Type {
	case "text":
		return &semantic.OutputFormat{Kind: semantic.OutputText}, nil
	case "json_schema":
		if strings.TrimSpace(format.Name) == "" {
			// Anthropic treats name as optional; every portable target requires it.
			// Saying so here is the difference between an error that names the field
			// and the "request is not portable" the semantic layer would produce two
			// steps later, which points the caller at nothing they can act on.
			return nil, errors.New("portable output_config.format requires a name for the json_schema")
		}
		return &semantic.OutputFormat{
			Kind: semantic.OutputJSONSchema, Name: format.Name, Description: format.Description,
			// Anthropic has no relaxed schema mode: a json_schema format is enforced.
			// Strict is therefore a fact about this request, not a default — and
			// renderOutputFormat refuses to render the other value rather than
			// quietly promoting it.
			Schema: append(json.RawMessage(nil), format.Schema...), Strict: true,
		}, nil
	default:
		return nil, errors.New("output_config format type is not portable")
	}
}

func renderOutputFormat(format semantic.OutputFormat) (json.RawMessage, error) {
	switch format.Kind {
	case semantic.OutputText:
		return json.Marshal(map[string]string{"type": "text"})
	case semantic.OutputJSONSchema:
		if len(format.Schema) == 0 {
			return nil, errors.New("json_schema output requires a schema")
		}
		if !format.Strict {
			// Anthropic enforces the schema unconditionally. Rendering a
			// non-strict request here would hand the caller stricter behaviour
			// than they asked for — a response refused for a schema violation the
			// original request was willing to accept. UnsupportedGenerateFields
			// declares this so routing avoids the profile instead of arriving here.
			return nil, errors.New("Anthropic structured output is always schema-enforced and cannot express strict=false")
		}
		value := map[string]any{"type": "json_schema", "schema": format.Schema}
		if format.Name != "" {
			value["name"] = format.Name
		}
		if format.Description != "" {
			value["description"] = format.Description
		}
		return json.Marshal(value)
	default:
		// OpenAI's json_object asks for "some JSON" without a schema, and
		// Anthropic has no way to express that. Inventing a schema would change
		// what the caller asked for, so the profile declares it unsupported and
		// routing avoids this provider instead.
		return nil, errors.New("Anthropic structured output requires a schema")
	}
}

func renderMessage(message semantic.Message) (anthropicapi.MessageParam, error) {
	result := anthropicapi.MessageParam{Role: string(message.Role)}
	if message.Role == semantic.RoleTool {
		result.Role = "user"
	}
	for _, part := range message.Content {
		switch part.Kind {
		case semantic.ContentText:
			result.Content = append(result.Content, anthropicapi.ContentBlock{Type: "text", Text: part.Text})
		case semantic.ContentInputImage:
			source, _ := json.Marshal(map[string]any{"type": "url", "url": part.URL, "detail": part.Detail})
			result.Content = append(result.Content, anthropicapi.ContentBlock{Type: "image", Source: source})
		case semantic.ContentToolCall:
			input := json.RawMessage(part.Arguments)
			if !json.Valid(input) {
				return result, errors.New("tool call arguments are invalid JSON")
			}
			result.Content = append(result.Content, anthropicapi.ContentBlock{Type: "tool_use", ID: part.CallID, Name: part.Name, Input: input})
		case semantic.ContentToolResult:
			content, _ := json.Marshal(part.Text)
			result.Content = append(result.Content, anthropicapi.ContentBlock{Type: "tool_result", ToolUseID: part.CallID, Content: content, IsError: part.ToolError})
		default:
			return result, errors.New("content is not portable to Anthropic Messages")
		}
	}
	return result, nil
}

func DecodeResult(message anthropicapi.Message) (semantic.GenerateResult, error) {
	if message.ID == "" || message.Model == "" || message.Role != "assistant" {
		return semantic.GenerateResult{}, errors.New("Anthropic response identity is invalid")
	}
	canonical := semantic.Message{Role: semantic.RoleAssistant}
	for _, block := range message.Content {
		switch block.Type {
		case "text":
			canonical.Content = append(canonical.Content, semantic.Content{Kind: semantic.ContentText, Text: block.Text})
		case "tool_use":
			if !json.Valid(block.Input) {
				return semantic.GenerateResult{}, errors.New("Anthropic tool input is invalid")
			}
			canonical.Content = append(canonical.Content, semantic.Content{Kind: semantic.ContentToolCall, CallID: block.ID, Name: block.Name, Arguments: string(block.Input)})
		case "thinking", "redacted_thinking":
			return semantic.GenerateResult{}, errors.New("signed thinking response requires native mode")
		default:
			return semantic.GenerateResult{}, errors.New("Anthropic response contains non-portable content")
		}
	}
	termination := DecodeStopReason(message.StopReason)
	// input_tokens excludes both cache tiers on this API, so the full prompt span
	// has to be recovered before anything downstream prices it.
	promptTokens := message.Usage.PromptTokens()
	usage := &semantic.Usage{
		InputTokens:           promptTokens,
		CachedInputTokens:     message.Usage.CacheReadInputTokens,
		CacheWriteInputTokens: message.Usage.CacheCreationInputTokens,
		OutputTokens:          message.Usage.OutputTokens,
		ReasoningTokens:       message.Usage.ThinkingTokens,
		TotalTokens:           promptTokens + message.Usage.OutputTokens,
		Source:                semantic.UsageProviderReported,
	}
	result := semantic.GenerateResult{ID: message.ID, Model: message.Model, Choices: []semantic.GenerateChoice{{Index: 0, Message: canonical, Termination: termination, NativeTermination: stringValue(message.StopReason)}}, Usage: usage, Translation: semantic.TranslationNone, MappingRevision: MappingRevision}
	return result, result.Validate()
}

// DecodeStopReason maps Anthropic's stop_reason onto the semantic termination
// vocabulary — the same one the Gemini and Bedrock adapters produce. It used to
// answer in OpenAI's wire vocabulary ("stop", "length", "tool_calls"), which is
// a different set of words for the same field: every consumer downstream reads
// semantic terminations, so an Anthropic response arrived carrying a value none
// of them recognized. The provider's own word is not lost — it travels beside
// this one as NativeTermination.
//
// It is exported because the streaming path lives in the provider package and
// decodes the same field. That path used to carry its own copy, which kept the
// OpenAI vocabulary long after this one was corrected: one state with two decode
// paths, only one of them fixed. There is now a single function to fix.
func DecodeStopReason(reason *string) string {
	if reason == nil {
		return ""
	}
	switch *reason {
	case "max_tokens", "model_context_window_exceeded":
		return "max_output"
	case "tool_use", "pause_turn":
		return "tool_call"
	case "refusal":
		return "refusal"
	case "end_turn", "stop_sequence":
		return "complete"
	default:
		// An unrecognized stop_reason is reported as unknown rather than
		// flattened into "complete": a turn that ended for a reason this build
		// has never seen is not a turn that ended normally.
		return "unknown"
	}
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
