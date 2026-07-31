package semantic

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/akz142857/Heimdall/internal/openaiapi"
)

const (
	MaxEncodedEventBytes = 64 << 10
	MaxToolArgumentBytes = 32 << 10
)

type Kind string

const (
	KindDelta Kind = "delta"
	KindUsage Kind = "usage"
)

// Event is the Provider-independent streaming boundary. Provider wire frames
// are normalized here before redaction, routing delivery, or SDK encoding.
type Event struct {
	Kind    Kind     `json:"kind"`
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices,omitempty"`
	Usage   *Usage   `json:"usage,omitempty"`
}

type Choice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type Delta struct {
	Role             string          `json:"role,omitempty"`
	Content          json.RawMessage `json:"content,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	Name             string          `json:"name,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	ToolCalls        []ToolCall      `json:"tool_calls,omitempty"`
}

type ToolCall struct {
	Index     *int   `json:"index,omitempty"`
	ID        string `json:"id,omitempty"`
	Type      string `json:"type,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

func FromOpenAIChunk(chunk openaiapi.ChatCompletionResponse) (Event, error) {
	event := Event{
		ID: chunk.ID, Object: chunk.Object, Created: chunk.Created, Model: chunk.Model,
	}
	if len(chunk.Choices) > 0 {
		event.Kind = KindDelta
		event.Choices = make([]Choice, 0, len(chunk.Choices))
		seenChoices := make(map[int]struct{}, len(chunk.Choices))
		for _, source := range chunk.Choices {
			if source.Index < 0 {
				return Event{}, errors.New("semantic choice index cannot be negative")
			}
			if _, exists := seenChoices[source.Index]; exists {
				return Event{}, errors.New("semantic event contains duplicate choice index")
			}
			seenChoices[source.Index] = struct{}{}
			if source.Delta == nil {
				return Event{}, errors.New("streaming choice is missing delta")
			}
			delta := Delta{
				Role: source.Delta.Role, Content: bytes.Clone(source.Delta.Content),
				ReasoningContent: source.Delta.ReasoningContent,
				Name:             source.Delta.Name, ToolCallID: source.Delta.ToolCallID,
			}
			for _, call := range source.Delta.ToolCalls {
				if len(call.Function.Arguments) > MaxToolArgumentBytes {
					return Event{}, errors.New("semantic tool argument fragment exceeds limit")
				}
				var index *int
				if call.Index != nil {
					copyIndex := *call.Index
					if copyIndex < 0 {
						return Event{}, errors.New("semantic tool call index cannot be negative")
					}
					index = &copyIndex
				}
				delta.ToolCalls = append(delta.ToolCalls, ToolCall{
					Index: index, ID: call.ID, Type: call.Type,
					Name: call.Function.Name, Arguments: call.Function.Arguments,
				})
			}
			event.Choices = append(event.Choices, Choice{
				Index: source.Index, Delta: delta, FinishReason: cloneString(source.FinishReason),
			})
		}
	} else if chunk.Usage != nil {
		event.Kind = KindUsage
	}
	if chunk.Usage != nil {
		event.Usage = &Usage{
			InputTokens:  chunk.Usage.PromptTokens,
			OutputTokens: chunk.Usage.CompletionTokens,
			TotalTokens:  chunk.Usage.TotalTokens,
		}
	}
	if err := event.Validate(); err != nil {
		return Event{}, err
	}
	return event, nil
}

func (event Event) Validate() error {
	if event.ID == "" || event.Object == "" {
		return errors.New("semantic event is missing id or object")
	}
	switch event.Kind {
	case KindDelta:
		if len(event.Choices) == 0 {
			return errors.New("semantic delta event has no choices")
		}
	case KindUsage:
		if len(event.Choices) != 0 || event.Usage == nil {
			return errors.New("semantic usage event is invalid")
		}
	default:
		return errors.New("semantic event kind is invalid")
	}
	seenChoices := make(map[int]struct{}, len(event.Choices))
	for _, choice := range event.Choices {
		if choice.Index < 0 {
			return errors.New("semantic choice index cannot be negative")
		}
		if _, exists := seenChoices[choice.Index]; exists {
			return errors.New("semantic event contains duplicate choice index")
		}
		seenChoices[choice.Index] = struct{}{}
		if len(choice.Delta.Content) > 0 && !json.Valid(choice.Delta.Content) {
			return errors.New("semantic content is invalid JSON")
		}
		seenTools := make(map[int]struct{}, len(choice.Delta.ToolCalls))
		for _, call := range choice.Delta.ToolCalls {
			if len(call.Arguments) > MaxToolArgumentBytes {
				return errors.New("semantic tool argument fragment exceeds limit")
			}
			if call.Index == nil {
				continue
			}
			if *call.Index < 0 {
				return errors.New("semantic tool call index cannot be negative")
			}
			if _, exists := seenTools[*call.Index]; exists {
				return errors.New("semantic delta contains duplicate tool call index")
			}
			seenTools[*call.Index] = struct{}{}
		}
	}
	if event.Usage != nil && (event.Usage.InputTokens < 0 ||
		event.Usage.OutputTokens < 0 || event.Usage.TotalTokens < 0 ||
		event.Usage.TotalTokens < event.Usage.InputTokens+event.Usage.OutputTokens) {
		return errors.New("semantic usage is invalid")
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode semantic event: %w", err)
	}
	if len(encoded) > MaxEncodedEventBytes {
		return fmt.Errorf("semantic event exceeds %d bytes", MaxEncodedEventBytes)
	}
	return nil
}

func (event Event) OpenAIChunk() (openaiapi.ChatCompletionResponse, error) {
	if err := event.Validate(); err != nil {
		return openaiapi.ChatCompletionResponse{}, err
	}
	chunk := openaiapi.ChatCompletionResponse{
		ID: event.ID, Object: event.Object, Created: event.Created, Model: event.Model,
	}
	for _, source := range event.Choices {
		delta := &openaiapi.Message{
			Role: source.Delta.Role, Content: bytes.Clone(source.Delta.Content),
			ReasoningContent: source.Delta.ReasoningContent,
			Name:             source.Delta.Name, ToolCallID: source.Delta.ToolCallID,
		}
		for _, call := range source.Delta.ToolCalls {
			var index *int
			if call.Index != nil {
				copyIndex := *call.Index
				index = &copyIndex
			}
			delta.ToolCalls = append(delta.ToolCalls, openaiapi.ToolCall{
				Index: index, ID: call.ID, Type: call.Type,
				Function: openaiapi.ToolCallFunction{Name: call.Name, Arguments: call.Arguments},
			})
		}
		chunk.Choices = append(chunk.Choices, openaiapi.Choice{
			Index: source.Index, Delta: delta, FinishReason: cloneString(source.FinishReason),
		})
	}
	if event.Usage != nil {
		chunk.Usage = &openaiapi.Usage{
			PromptTokens:     event.Usage.InputTokens,
			CompletionTokens: event.Usage.OutputTokens,
			TotalTokens:      event.Usage.TotalTokens,
		}
	}
	return chunk, nil
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}
