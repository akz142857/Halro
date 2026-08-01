package semantic

import (
	"encoding/json"
	"errors"
	"fmt"
)

const (
	MaxEncodedEventBytes = 64 << 10
	MaxToolArgumentBytes = 32 << 10
)

type EventKind string

const (
	EventDelta EventKind = "output_delta"
	EventUsage EventKind = "usage"
)

type Event struct {
	Kind            EventKind       `json:"kind"`
	ID              string          `json:"id"`
	Created         int64           `json:"created,omitempty"`
	Model           string          `json:"model"`
	Outputs         []OutputDelta   `json:"outputs,omitempty"`
	Usage           *Usage          `json:"usage,omitempty"`
	Translation     TranslationLoss `json:"translation,omitempty"`
	MappingRevision uint64          `json:"mapping_revision,omitempty"`
}

type OutputDelta struct {
	Index             int            `json:"index"`
	Role              Role           `json:"role,omitempty"`
	Content           []ContentDelta `json:"content,omitempty"`
	Termination       string         `json:"termination,omitempty"`
	NativeTermination string         `json:"native_termination,omitempty"`
}

type ContentDelta struct {
	Kind              ContentKind `json:"kind"`
	Text              string      `json:"text,omitempty"`
	ToolIndex         *int        `json:"tool_index,omitempty"`
	CallID            string      `json:"call_id,omitempty"`
	Name              string      `json:"name,omitempty"`
	ArgumentsFragment string      `json:"arguments_fragment,omitempty"`
}

func (event Event) Validate() error {
	if event.ID == "" || event.Model == "" {
		return errors.New("semantic event identity is incomplete")
	}
	switch event.Kind {
	case EventDelta:
		if len(event.Outputs) == 0 {
			return errors.New("semantic delta event has no outputs")
		}
	case EventUsage:
		if len(event.Outputs) != 0 || event.Usage == nil {
			return errors.New("semantic usage event is invalid")
		}
	default:
		return errors.New("semantic event kind is invalid")
	}
	seen := map[int]struct{}{}
	for _, output := range event.Outputs {
		if output.Index < 0 {
			return errors.New("semantic output index cannot be negative")
		}
		if _, ok := seen[output.Index]; ok {
			return errors.New("semantic event contains duplicate output index")
		}
		seen[output.Index] = struct{}{}
		if output.Role != "" && output.Role != RoleAssistant {
			return errors.New("semantic stream output role is invalid")
		}
		toolIndexes := map[int]struct{}{}
		for _, delta := range output.Content {
			switch delta.Kind {
			case ContentText, ContentReasoning:
				if delta.ToolIndex != nil || delta.CallID != "" || delta.Name != "" || delta.ArgumentsFragment != "" {
					return errors.New("semantic text delta has tool fields")
				}
			case ContentToolCall:
				if len(delta.ArgumentsFragment) > MaxToolArgumentBytes {
					return errors.New("semantic tool argument fragment exceeds limit")
				}
				if delta.ToolIndex != nil {
					if *delta.ToolIndex < 0 {
						return errors.New("semantic tool index cannot be negative")
					}
					if _, ok := toolIndexes[*delta.ToolIndex]; ok {
						return errors.New("semantic delta contains duplicate tool index")
					}
					toolIndexes[*delta.ToolIndex] = struct{}{}
				}
			default:
				return errors.New("semantic stream content kind is invalid")
			}
		}
	}
	if event.Usage != nil {
		if err := event.Usage.Validate(); err != nil {
			return err
		}
	}
	if event.MappingRevision == 0 && event.Translation != "" || event.MappingRevision != 0 && !validTranslationLoss(event.Translation) {
		return errors.New("semantic event audit metadata is invalid")
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

type StreamValidator struct {
	id        string
	model     string
	seen      map[int]struct{}
	terminal  map[int]struct{}
	tools     map[string]*streamToolState
	usageSeen bool
}

type streamToolState struct {
	callID string
	name   string
	bytes  int
}

func NewStreamValidator() *StreamValidator {
	return &StreamValidator{seen: map[int]struct{}{}, terminal: map[int]struct{}{}, tools: map[string]*streamToolState{}}
}

func (validator *StreamValidator) Accept(event Event) error {
	if validator == nil {
		return errors.New("semantic stream validator is nil")
	}
	if err := event.Validate(); err != nil {
		return err
	}
	if event.MappingRevision == 0 || !validTranslationLoss(event.Translation) {
		return errors.New("semantic stream event is missing audit metadata")
	}
	if validator.id == "" {
		validator.id, validator.model = event.ID, event.Model
	} else if validator.id != event.ID || validator.model != event.Model {
		return errors.New("semantic stream identity changed")
	}
	if validator.usageSeen {
		return errors.New("semantic stream emitted data after final usage")
	}
	if event.Kind == EventUsage {
		validator.usageSeen = true
		return nil
	}
	for _, output := range event.Outputs {
		if _, done := validator.terminal[output.Index]; done {
			return errors.New("semantic stream emitted output after termination")
		}
		validator.seen[output.Index] = struct{}{}
		for _, delta := range output.Content {
			if delta.Kind == ContentToolCall {
				if delta.ToolIndex == nil {
					return errors.New("semantic streamed tool call is missing its index")
				}
				key := fmt.Sprintf("%d:%d", output.Index, *delta.ToolIndex)
				state := validator.tools[key]
				if state == nil {
					state = &streamToolState{}
					validator.tools[key] = state
				}
				if delta.CallID != "" {
					if state.callID != "" && state.callID != delta.CallID {
						return errors.New("semantic streamed tool call changed call id")
					}
					state.callID = delta.CallID
				}
				if delta.Name != "" {
					if state.name != "" && state.name != delta.Name {
						return errors.New("semantic streamed tool call changed name")
					}
					state.name = delta.Name
				}
				state.bytes += len(delta.ArgumentsFragment)
				if state.bytes > MaxToolArgumentBytes {
					return errors.New("semantic streamed tool arguments exceed limit")
				}
			}
		}
		if output.Termination != "" || output.NativeTermination != "" {
			validator.terminal[output.Index] = struct{}{}
		}
	}
	return nil
}

func (validator *StreamValidator) Finalize(requireUsage bool) error {
	if validator == nil || len(validator.seen) == 0 {
		return errors.New("semantic stream emitted no output")
	}
	for index := range validator.seen {
		if _, ok := validator.terminal[index]; !ok {
			return errors.New("semantic stream output did not terminate")
		}
	}
	for _, tool := range validator.tools {
		if tool.callID == "" || tool.name == "" {
			return errors.New("semantic streamed tool call identity is incomplete")
		}
	}
	if requireUsage && !validator.usageSeen {
		return errors.New("semantic stream omitted required usage")
	}
	return nil
}

func validTranslationLoss(loss TranslationLoss) bool {
	return loss == TranslationNone || loss == TranslationDeclared || loss == TranslationUnsupported
}
