package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	openaiwire "github.com/akz142857/Halro/internal/compatibility/openai"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/semantic"
	"github.com/akz142857/Halro/internal/sse"
)

// GenerateSemanticStream consumes the upstream Responses SSE protocol directly.
// A completed/incomplete event is mandatory: an EOF is never a successful turn.
func (a *Adapter) GenerateSemanticStream(ctx context.Context, call provider.GenerateCall, emit func(semantic.Event) error) (*semantic.Usage, error) {
	if !a.responses || a.providerType != string(domain.ProviderOpenAI) || emit == nil {
		return nil, &provider.Error{Class: provider.ErrorBadRequest, Message: "this profile does not serve Responses streaming"}
	}
	wire, err := openaiwire.RenderProviderResponseRequest(call.Request, call.ProviderModel)
	if err != nil {
		return nil, &provider.Error{Class: provider.ErrorBadRequest, Message: "render Responses stream request", Cause: err}
	}
	wire.Stream = true
	wire.Include = []string{"reasoning.encrypted_content"}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	endpoint := a.operationURL(call.ProviderModel, "responses")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	if err = a.prepareRequest(request); err != nil {
		return nil, &provider.Error{Class: provider.ErrorAuthentication, Message: "authorize Responses stream", Cause: err}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("X-Request-ID", call.RequestID)
	response, err := a.client.Do(request)
	if err != nil {
		return nil, provider.NewTransportError("Responses stream transport", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, a.classifyHTTPError(response.StatusCode, limitedErrorMessage(response.Body))
	}
	if !strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		return nil, acceptedResponseMalformed("Responses upstream did not return SSE", nil)
	}
	state := responsesStreamState{items: map[int]*responsesStreamItem{}}
	decoder := sse.NewDecoder(response.Body, maxResponseBytes)
	for {
		frame, readErr := decoder.Next()
		if readErr != nil {
			if ctx.Err() != nil {
				return state.usage, provider.NewTransportError("read Responses stream", ctx.Err())
			}
			if errors.Is(readErr, io.EOF) {
				readErr = errors.New("Responses stream ended without a terminal event")
			}
			return state.usage, acceptedResponseMalformed("read Responses stream", readErr)
		}
		var event openaiapi.ResponseStreamEvent
		if err = json.Unmarshal(frame.Data, &event); err != nil {
			return state.usage, acceptedResponseMalformed("decode Responses stream event", err)
		}
		if frame.Event != "" && frame.Event != event.Type {
			return state.usage, acceptedResponseMalformed("Responses SSE event type mismatch", nil)
		}
		done, err := state.accept(event, call.Request.IncludeUsage, emit)
		if err != nil {
			return state.usage, acceptedResponseMalformed("consume Responses stream", err)
		}
		if done {
			return state.usage, nil
		}
	}
}

type responsesStreamItem struct {
	item      openaiapi.ResponseOutputItem
	toolIndex int
	deferred  bool
	text      strings.Builder
	arguments strings.Builder
	refusal   bool
}
type responsesStreamState struct {
	id, model string
	created   int64
	items     map[int]*responsesStreamItem
	summary   strings.Builder
	usage     *semantic.Usage
	bytes     int
	toolCount int
	roleSent  bool
	// Opaque reasoning is only complete at the terminal snapshot. Hold tool
	// fragments behind it so the northbound stream can replay that snapshot's
	// reasoning before its associated calls, even without summary deltas.
	hasReasoning bool
}

func (s *responsesStreamState) emit(parts []semantic.ContentDelta, termination string, emit func(semantic.Event) error) error {
	var role semantic.Role
	if !s.roleSent {
		role = semantic.RoleAssistant
		s.roleSent = true
	}
	return emit(semantic.Event{Kind: semantic.EventDelta, ID: s.id, Model: s.model, Created: s.created, Translation: semantic.TranslationNone, MappingRevision: openaiwire.MappingRevision,
		Outputs: []semantic.OutputDelta{{Index: 0, Role: role, Content: parts, Termination: termination}}})
}

func (s *responsesStreamState) emitTool(item *responsesStreamItem, part semantic.ContentDelta, emit func(semantic.Event) error) error {
	if item.deferred {
		// Arguments already live in the bounded per-item builder. Do not retain
		// another allocation for every upstream fragment (including empty ones).
		return nil
	}
	return s.emit([]semantic.ContentDelta{part}, "", emit)
}

func (s *responsesStreamState) accept(e openaiapi.ResponseStreamEvent, includeUsage bool, emit func(semantic.Event) error) (bool, error) {
	if e.Response != nil {
		r := e.Response
		if r.ID == "" || r.Model == "" {
			return false, errors.New("Responses stream identity is incomplete")
		}
		if s.id == "" {
			s.id, s.model, s.created = r.ID, r.Model, r.CreatedAt
		} else if s.id != r.ID || s.model != r.Model {
			return false, errors.New("Responses stream identity changed")
		}
	}
	if e.Response != nil {
		s.usage = openaiwire.DecodeResponseUsage(e.Response.Usage)
	}
	if e.Type == "error" || e.Type == "response.failed" {
		return false, errors.New("upstream Responses stream failed")
	}
	if s.id == "" {
		return false, errors.New("Responses stream omitted response identity")
	}
	switch e.Type {
	case "response.created", "response.in_progress":
		return false, nil
	case "response.output_item.added":
		if e.OutputIndex == nil || *e.OutputIndex < 0 || e.Item == nil || s.items[*e.OutputIndex] != nil || len(s.items) >= 1024 {
			return false, errors.New("invalid Responses stream item")
		}
		item := e.Item
		if item.Type != "message" && item.Type != "reasoning" && item.Type != "function_call" {
			return false, errors.New("unsupported Responses stream item type")
		}
		// Initial snapshots are retained too, including reasoning metadata and
		// any arguments already present. They share the stream's cumulative
		// budget rather than multiplying the per-frame decoder limit by 1024.
		encoded, err := json.Marshal(item)
		if err != nil {
			return false, errors.New("invalid Responses stream item")
		}
		s.bytes += len(encoded)
		if s.bytes > maxResponseBytes {
			return false, errors.New("Responses stream exceeds size limit")
		}
		if item.Type == "function_call" && len(item.Arguments) > semantic.MaxToolArgumentBytes {
			return false, errors.New("invalid Responses tool arguments snapshot")
		}
		s.items[*e.OutputIndex] = &responsesStreamItem{item: *item}
		if item.Type == "reasoning" {
			s.hasReasoning = true
		}
		if item.Type == "function_call" {
			if item.CallID == "" || item.Name == "" {
				return false, errors.New("Responses tool identity missing")
			}
			state := s.items[*e.OutputIndex]
			state.toolIndex = s.toolCount
			state.deferred = s.hasReasoning
			s.toolCount++
			state.arguments.WriteString(item.Arguments)
			return false, s.emitTool(state, semantic.ContentDelta{Kind: semantic.ContentToolCall, ToolIndex: &state.toolIndex, CallID: item.CallID, Name: item.Name, ArgumentsFragment: item.Arguments}, emit)
		}
	case "response.output_text.delta", "response.refusal.delta", "response.function_call_arguments.delta", "response.reasoning_summary_text.delta":
		if e.OutputIndex == nil || s.items[*e.OutputIndex] == nil {
			return false, errors.New("Responses delta has no item")
		}
		item := s.items[*e.OutputIndex]
		if e.ItemID != "" && e.ItemID != item.item.ID {
			return false, errors.New("Responses delta changed item identity")
		}
		s.bytes += len(e.Delta)
		if s.bytes > maxResponseBytes {
			return false, errors.New("Responses stream exceeds size limit")
		}
		part := semantic.ContentDelta{Text: e.Delta, Kind: semantic.ContentText}
		switch e.Type {
		case "response.function_call_arguments.delta":
			if item.item.Type != "function_call" || item.arguments.Len()+len(e.Delta) > semantic.MaxToolArgumentBytes {
				return false, errors.New("invalid Responses tool arguments delta")
			}
			item.arguments.WriteString(e.Delta)
			return false, s.emitTool(item, semantic.ContentDelta{Kind: semantic.ContentToolCall, ToolIndex: &item.toolIndex, ArgumentsFragment: e.Delta}, emit)
		case "response.refusal.delta":
			if item.item.Type != "message" {
				return false, errors.New("refusal delta has wrong item type")
			}
			item.refusal = true
			item.text.WriteString(e.Delta)
			return false, nil
		case "response.reasoning_summary_text.delta":
			if item.item.Type != "reasoning" {
				return false, errors.New("reasoning delta has wrong item type")
			}
			s.summary.WriteString(e.Delta)
			part.Kind = semantic.ContentReasoning
		default:
			if item.item.Type != "message" {
				return false, errors.New("text delta has wrong item type")
			}
			item.text.WriteString(e.Delta)
		}
		return false, s.emit([]semantic.ContentDelta{part}, "", emit)
	case "response.completed", "response.incomplete":
		if e.Response == nil || e.Response.Status != strings.TrimPrefix(e.Type, "response.") {
			return false, errors.New("invalid Responses terminal event")
		}
		result, err := openaiwire.DecodeOpenAIProviderResponse(*e.Response)
		if err != nil {
			return false, err
		}
		s.usage = result.Usage
		for index, item := range e.Response.Output {
			seen := s.items[index]
			if seen == nil || seen.item.ID != item.ID || seen.item.Type != item.Type {
				return false, errors.New("Responses terminal items differ from stream")
			}
			switch item.Type {
			case "message":
				var text strings.Builder
				for _, part := range item.Content {
					if len(part.Annotations) > 0 {
						return false, errors.New("Responses streaming annotations are unsupported")
					}
					if part.Type == "refusal" {
						text.WriteString(part.Refusal)
					} else {
						text.WriteString(part.Text)
					}
				}
				if text.String() != seen.text.String() {
					return false, errors.New("Responses terminal text differs from deltas")
				}
			case "function_call":
				if item.CallID != seen.item.CallID || item.Name != seen.item.Name || item.Arguments != seen.arguments.String() {
					return false, errors.New("Responses terminal tool differs from deltas")
				}
			}
		}
		if len(s.items) != len(e.Response.Output) {
			return false, errors.New("Responses terminal output is incomplete")
		}
		for _, part := range result.Choices[0].Message.Content {
			if part.Kind != semantic.ContentReasoning {
				continue
			}
			summary := part.Text
			if s.summary.Len() > 0 {
				if s.summary.String() != summary {
					return false, errors.New("Responses terminal summary differs from deltas")
				}
				summary = ""
			}
			if err = s.emit([]semantic.ContentDelta{{Kind: semantic.ContentReasoning, Text: summary, EncryptedContent: part.EncryptedContent}}, "", emit); err != nil {
				return false, err
			}
		}
		for index := range e.Response.Output {
			item := s.items[index]
			if !item.deferred {
				continue
			}
			if err = s.emit([]semantic.ContentDelta{{Kind: semantic.ContentToolCall, ToolIndex: &item.toolIndex, CallID: item.item.CallID, Name: item.item.Name}}, "", emit); err != nil {
				return false, err
			}
			// Keep re-emitted fragments small enough for the semantic event bound,
			// and never split a UTF-8 code point when changing fragment boundaries.
			for arguments := item.arguments.String(); arguments != ""; {
				end := min(len(arguments), 1024)
				for end < len(arguments) && !utf8.RuneStart(arguments[end]) {
					end--
				}
				if err = s.emit([]semantic.ContentDelta{{Kind: semantic.ContentToolCall, ToolIndex: &item.toolIndex, ArgumentsFragment: arguments[:end]}}, "", emit); err != nil {
					return false, err
				}
				arguments = arguments[end:]
			}
		}
		var finalParts []semantic.ContentDelta
		for index := range e.Response.Output {
			item := s.items[index]
			if item.refusal {
				finalParts = append(finalParts, semantic.ContentDelta{Kind: semantic.ContentText, Text: item.text.String()})
			}
		}
		if err = s.emit(finalParts, result.Choices[0].Termination, emit); err != nil {
			return false, err
		}
		if includeUsage && s.usage != nil {
			err = emit(semantic.Event{Kind: semantic.EventUsage, ID: s.id, Model: s.model, Created: s.created, Usage: s.usage, Translation: semantic.TranslationNone, MappingRevision: openaiwire.MappingRevision})
		}
		return true, err
	case "response.output_item.done", "response.content_part.added", "response.content_part.done", "response.output_text.done", "response.refusal.done", "response.function_call_arguments.done", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done", "response.reasoning_summary_text.done":
		// The terminal response is checked against the deltas; these snapshots must
		// not be emitted again as fragments or the caller would receive duplicates.
	default:
		return false, errors.New("unsupported Responses stream event")
	}
	return false, nil
}
