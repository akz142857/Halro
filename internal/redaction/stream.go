package redaction

import (
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/akz142857/Halro/internal/openaiapi"
)

const (
	maxStreamChunkBytes   = 64 << 10
	maxStreamPendingBytes = 128 << 10
)

// Stream incrementally redacts semantic Chat Completions chunks. It keeps a
// bounded suffix per content/tool-argument channel so a finite-width match
// cannot be split across provider chunks and partially delivered.
type Stream struct {
	engine   *Engine
	policyID string
	rules    []compiledRule
	width    int
	choices  map[int]*streamChoice
}

type streamChoice struct {
	content     *rollingString
	reasoning   *rollingString
	tools       map[int]*rollingString
	toolMeta    map[int]openaiapi.ToolCall
	messageMeta openaiapi.Message
	base        openaiapi.ChatCompletionResponse
}

type rollingString struct {
	engine   *Engine
	policyID string
	rules    []compiledRule
	width    int
	pending  string
	tool     bool
	// unescaper and unescaped mirror a tool-argument channel with its JSON
	// string escapes decoded. Fragments are matched in their raw form so they
	// can be transformed without corrupting partial JSON syntax, but a client
	// reconstitutes the arguments by decoding them, so a secret carrying a
	// single escape inside it passes every raw pattern and still arrives whole.
	// The mirror is matched as well, and can only reject: rewriting a raw
	// fragment from a decoded offset is exactly what the raw pass avoids.
	unescaper jsonUnescaper
	unescaped string
}

func (e *Engine) NewStream(policyID string) (*Stream, error) {
	policy, exists := e.policy(policyID)
	if exists && policy.policy.Mode == "strict" {
		return nil, errors.New("strict redaction policy does not allow streaming")
	}
	rules := make([]compiledRule, 0, len(e.mandatory)+len(policy.rules))
	width := 1
	for _, rule := range e.mandatory {
		rules = append(rules, rule)
		width = max(width, rule.rule.ComputedMaxMatchBytes)
	}
	if exists {
		for _, rule := range policy.rules {
			if !hasScope(rule.rule, "outbound") {
				continue
			}
			if rule.rule.ComputedMaxMatchBytes > 0 {
				rules = append(rules, rule)
				width = max(width, rule.rule.ComputedMaxMatchBytes)
			}
		}
	}
	if width > maxBoundedMatchBytes {
		return nil, errors.New("stream redaction width exceeds the hard limit")
	}
	return &Stream{
		engine: e, policyID: policyID, rules: rules, width: width,
		choices: make(map[int]*streamChoice),
	}, nil
}

// Process returns zero or more safe chunks. A terminal chunk produces a
// synthetic delta that flushes the protected suffix, before it when the chunk
// carries no text of its own and after it when it does.
func (s *Stream) Process(chunk openaiapi.ChatCompletionResponse) ([]openaiapi.ChatCompletionResponse, error) {
	chunk = cloneResponse(chunk)
	var leading, trailing []openaiapi.ChatCompletionResponse
	for choiceIndex := range chunk.Choices {
		choice := &chunk.Choices[choiceIndex]
		state := s.choice(choice.Index)
		state.base = responseBase(chunk)
		message := choice.Delta
		if message == nil {
			message = choice.Message
		}
		if message != nil {
			if err := s.processMessage(state, message); err != nil {
				return nil, err
			}
		}
		if choice.FinishReason != nil {
			flushed, err := s.flushChoice(choice.Index)
			if err != nil {
				return nil, err
			}
			switch {
			case flushed == nil:
			case deltaCarriesOutput(message):
				// The flush holds the newest bytes of this choice, so once the
				// terminal chunk carries text of its own the flush has to
				// follow it — emitting it first delivers the tail of the
				// message ahead of its head, and the client concatenates deltas
				// in arrival order. The terminal marker rides along with the
				// flush so it still arrives last.
				flushed.Choices[0].FinishReason = choice.FinishReason
				choice.FinishReason = nil
				trailing = append(trailing, *flushed)
			default:
				leading = append(leading, *flushed)
			}
		}
	}
	result := leading
	if meaningfulStreamChunk(chunk) {
		result = append(result, chunk)
	}
	return append(result, trailing...), nil
}

// Flush emits all remaining protected suffixes after a successful provider
// stream that did not include a terminal finish_reason chunk.
func (s *Stream) Flush() ([]openaiapi.ChatCompletionResponse, error) {
	result := make([]openaiapi.ChatCompletionResponse, 0, len(s.choices))
	for index := range s.choices {
		chunk, err := s.flushChoice(index)
		if err != nil {
			return nil, err
		}
		if chunk != nil {
			result = append(result, *chunk)
		}
	}
	return result, nil
}

func (s *Stream) processMessage(state *streamChoice, message *openaiapi.Message) error {
	if message.Role != "" {
		state.messageMeta.Role = message.Role
	}
	if message.Name != "" {
		state.messageMeta.Name = message.Name
	}
	if message.ToolCallID != "" {
		state.messageMeta.ToolCallID = message.ToolCallID
	}
	hasSafeContent := false
	if text, ok := openaiapi.DecodeTextContent(message.Content); ok {
		if state.content == nil {
			state.content = s.newRolling(false)
		}
		safe, err := state.content.Push(text, false)
		if err != nil {
			return err
		}
		if safe == "" {
			message.Content = nil
		} else {
			message.Content = openaiapi.TextContent(safe)
			hasSafeContent = true
		}
	} else if len(message.Content) > 0 {
		// Structured content parts are complete JSON values within one semantic
		// chunk; use the regular recursive transformer.
		value, err := s.engine.processRaw(s.policyID, "outbound", message.Content)
		if err != nil {
			return err
		}
		message.Content = value
		hasSafeContent = true
	}
	if message.ReasoningContent != "" {
		if state.reasoning == nil {
			state.reasoning = s.newRolling(false)
		}
		safe, err := state.reasoning.Push(message.ReasoningContent, false)
		if err != nil {
			return err
		}
		message.ReasoningContent = safe
		if safe != "" {
			hasSafeContent = true
		}
	}
	outputCalls := make([]openaiapi.ToolCall, 0, len(message.ToolCalls))
	for position := range message.ToolCalls {
		call := &message.ToolCalls[position]
		key := position
		if call.Index != nil {
			key = *call.Index
		}
		state.toolMeta[key] = mergeToolMeta(state.toolMeta[key], *call)
		if call.Function.Arguments == "" {
			continue
		}
		rolling := state.tools[key]
		if rolling == nil {
			rolling = s.newRolling(true)
			state.tools[key] = rolling
		}
		safe, err := rolling.Push(call.Function.Arguments, false)
		if err != nil {
			return err
		}
		if safe != "" {
			meta := state.toolMeta[key]
			meta.Function.Arguments = safe
			outputCalls = append(outputCalls, meta)
		}
	}
	message.ToolCalls = outputCalls
	if hasSafeContent || len(outputCalls) > 0 {
		applyMessageMeta(message, &state.messageMeta)
	} else {
		message.Role = ""
		message.Name = ""
		message.ToolCallID = ""
	}
	return nil
}

func (s *Stream) choice(index int) *streamChoice {
	state := s.choices[index]
	if state == nil {
		state = &streamChoice{
			tools: make(map[int]*rollingString), toolMeta: make(map[int]openaiapi.ToolCall),
		}
		s.choices[index] = state
	}
	return state
}

func (s *Stream) newRolling(tool bool) *rollingString {
	return &rollingString{
		engine: s.engine, policyID: s.policyID, rules: s.rules, width: s.width, tool: tool,
	}
}

func (s *Stream) flushChoice(index int) (*openaiapi.ChatCompletionResponse, error) {
	state := s.choices[index]
	if state == nil {
		return nil, nil
	}
	delta := state.messageMeta
	hasOutput := delta.Role != "" || delta.Name != "" || delta.ToolCallID != ""
	if state.content != nil {
		value, err := state.content.Push("", true)
		if err != nil {
			return nil, err
		}
		if value != "" {
			delta.Content = openaiapi.TextContent(value)
			hasOutput = true
		}
	}
	if state.reasoning != nil {
		value, err := state.reasoning.Push("", true)
		if err != nil {
			return nil, err
		}
		if value != "" {
			delta.ReasoningContent = value
			hasOutput = true
		}
	}
	for toolIndex, rolling := range state.tools {
		value, err := rolling.Push("", true)
		if err != nil {
			return nil, err
		}
		if value == "" {
			continue
		}
		meta := state.toolMeta[toolIndex]
		meta.Function.Arguments = value
		delta.ToolCalls = append(delta.ToolCalls, meta)
		hasOutput = true
	}
	delete(s.choices, index)
	if !hasOutput {
		return nil, nil
	}
	result := state.base
	result.Choices = []openaiapi.Choice{{Index: index, Delta: &delta}}
	return &result, nil
}

func (r *rollingString) Push(piece string, final bool) (string, error) {
	if len(piece) > maxStreamChunkBytes {
		return "", fmt.Errorf("stream redaction chunk exceeds %d bytes", maxStreamChunkBytes)
	}
	if len(r.pending)+len(piece) > maxStreamPendingBytes {
		return "", fmt.Errorf("stream redaction buffer exceeds %d bytes", maxStreamPendingBytes)
	}
	if r.tool {
		if err := r.scanUnescaped(piece, final); err != nil {
			return "", err
		}
	}
	r.pending += piece
	cut := len(r.pending)
	if !final {
		cut -= max(r.width-1, 0)
		if cut <= 0 {
			return "", nil
		}
		cut = utf8Boundary(r.pending, cut)
		cut = r.safeCut(cut)
		if cut <= 0 {
			return "", nil
		}
	}
	prefix := r.pending[:cut]
	r.pending = r.pending[cut:]
	if r.tool {
		return r.engine.processToolFragment(r.policyID, "outbound", prefix)
	}
	return r.engine.processString(r.policyID, "outbound", prefix)
}

// scanUnescaped matches the decoded mirror of a tool-argument channel and fails
// closed on a hit. It keeps the trailing width-1 bytes of the mirror so a value
// that only becomes a match once decoded is still seen whole when it spans two
// fragments, and it runs before any of the fragment is released so the match
// cannot be delivered first and refused afterwards.
func (r *rollingString) scanUnescaped(piece string, final bool) error {
	decoded := r.unescaper.push(piece)
	if final {
		decoded += r.unescaper.flush()
	}
	if decoded == "" {
		return nil
	}
	window := r.unescaped + decoded
	for _, rule := range r.rules {
		if rule.rule.Action == "detect_only" || !rule.matches(window) {
			continue
		}
		return &MatchError{
			RuleID: rule.rule.ID, Category: category(rule.rule), Scope: "outbound",
		}
	}
	if keep := max(r.width-1, 0); len(window) > keep {
		window = window[len(window)-keep:]
	}
	r.unescaped = window
	return nil
}

func (r *rollingString) safeCut(cut int) int {
	for {
		next := cut
		for _, rule := range r.rules {
			for _, location := range rule.validLocations(r.pending) {
				if location[0] < cut && location[1] > cut {
					next = min(next, location[0])
				}
			}
		}
		next = utf8Boundary(r.pending, next)
		if next == cut {
			return cut
		}
		cut = next
	}
}

func utf8Boundary(value string, cut int) int {
	if cut >= len(value) {
		return len(value)
	}
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return cut
}

func responseBase(value openaiapi.ChatCompletionResponse) openaiapi.ChatCompletionResponse {
	return openaiapi.ChatCompletionResponse{
		ID: value.ID, Object: value.Object, Created: value.Created, Model: value.Model,
	}
}

func cloneResponse(value openaiapi.ChatCompletionResponse) openaiapi.ChatCompletionResponse {
	value.Choices = append([]openaiapi.Choice(nil), value.Choices...)
	for index := range value.Choices {
		choice := &value.Choices[index]
		if choice.Delta != nil {
			copyMessage := *choice.Delta
			copyMessage.Content = append([]byte(nil), copyMessage.Content...)
			copyMessage.ToolCalls = append([]openaiapi.ToolCall(nil), copyMessage.ToolCalls...)
			choice.Delta = &copyMessage
		}
		if choice.Message != nil {
			copyMessage := *choice.Message
			copyMessage.Content = append([]byte(nil), copyMessage.Content...)
			copyMessage.ToolCalls = append([]openaiapi.ToolCall(nil), copyMessage.ToolCalls...)
			choice.Message = &copyMessage
		}
	}
	return value
}

func mergeToolMeta(current, next openaiapi.ToolCall) openaiapi.ToolCall {
	if next.Index != nil {
		current.Index = next.Index
	}
	if next.ID != "" {
		current.ID = next.ID
	}
	if next.Type != "" {
		current.Type = next.Type
	}
	if next.Function.Name != "" {
		current.Function.Name = next.Function.Name
	}
	return current
}

func applyMessageMeta(message, stored *openaiapi.Message) {
	if message.Role == "" {
		message.Role = stored.Role
	}
	if message.Name == "" {
		message.Name = stored.Name
	}
	if message.ToolCallID == "" {
		message.ToolCallID = stored.ToolCallID
	}
	stored.Role = ""
	stored.Name = ""
	stored.ToolCallID = ""
}

// deltaCarriesOutput reports whether a redacted delta still holds text the
// client will concatenate, which is what decides where the flush belongs.
func deltaCarriesOutput(message *openaiapi.Message) bool {
	if message == nil {
		return false
	}
	return len(message.Content) > 0 || message.ReasoningContent != "" || len(message.ToolCalls) > 0
}

func meaningfulStreamChunk(chunk openaiapi.ChatCompletionResponse) bool {
	if chunk.Usage != nil {
		return true
	}
	for _, choice := range chunk.Choices {
		if choice.FinishReason != nil {
			return true
		}
		for _, message := range []*openaiapi.Message{choice.Delta, choice.Message} {
			if message == nil {
				continue
			}
			if len(message.Content) > 0 || message.ReasoningContent != "" || message.Role != "" || message.Name != "" ||
				message.ToolCallID != "" || len(message.ToolCalls) > 0 {
				return true
			}
		}
	}
	return false
}
