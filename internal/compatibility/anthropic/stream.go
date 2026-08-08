package anthropic

import (
	"errors"
	"fmt"
	"sort"

	"github.com/akz142857/Halro/internal/anthropicapi"
	"github.com/akz142857/Halro/internal/semantic"
)

type StreamRenderer struct {
	publicModel string
	started     bool
	finished    bool
	id          string
	created     int64
	blocks      map[string]*streamBlock
	nextIndex   int
	termination string
}

type streamBlock struct {
	index int
	kind  semantic.ContentKind
	open  bool
}

func NewStreamRenderer(publicModel string) *StreamRenderer {
	return &StreamRenderer{publicModel: publicModel, blocks: map[string]*streamBlock{}}
}

func (renderer *StreamRenderer) Accept(event semantic.Event) ([]anthropicapi.StreamEvent, error) {
	if renderer == nil || renderer.finished {
		return nil, errors.New("Anthropic stream renderer is closed")
	}
	if err := event.Validate(); err != nil {
		return nil, err
	}
	var events []anthropicapi.StreamEvent
	if !renderer.started {
		if event.Kind == semantic.EventUsage {
			return nil, errors.New("usage arrived before message content")
		}
		renderer.started, renderer.id, renderer.created = true, anthropicID(event.ID), event.Created
		message := anthropicapi.Message{ID: renderer.id, Type: "message", Role: "assistant", Content: anthropicapi.ContentBlocks{}, Model: renderer.publicModel, Usage: anthropicapi.Usage{}}
		events = append(events, anthropicapi.StreamEvent{Type: "message_start", Message: &message})
	}
	if event.ID != "" && anthropicID(event.ID) != renderer.id {
		return nil, errors.New("provider stream message id changed")
	}
	if event.Kind == semantic.EventUsage {
		if renderer.termination == "" {
			return nil, errors.New("usage arrived before message termination")
		}
		closed, err := renderer.closeBlocks()
		if err != nil {
			return nil, err
		}
		events = append(events, closed...)
		stop := renderStopReason(renderer.termination)
		usage := renderUsage(*event.Usage)
		events = append(events,
			anthropicapi.StreamEvent{Type: "message_delta", Delta: map[string]any{"stop_reason": stop, "stop_sequence": nil}, Usage: &usage},
			anthropicapi.StreamEvent{Type: "message_stop"},
		)
		renderer.finished = true
		return events, nil
	}
	for _, output := range event.Outputs {
		if output.Index != 0 {
			return nil, errors.New("Anthropic Messages stream requires one output")
		}
		for _, delta := range output.Content {
			switch delta.Kind {
			case semantic.ContentText:
				block, created := renderer.block("text", semantic.ContentText)
				if created {
					index := block.index
					events = append(events, anthropicapi.StreamEvent{Type: "content_block_start", Index: &index, ContentBlock: map[string]any{"type": "text", "text": ""}})
				}
				index := block.index
				events = append(events, anthropicapi.StreamEvent{Type: "content_block_delta", Index: &index, Delta: map[string]any{"type": "text_delta", "text": delta.Text}})
			case semantic.ContentToolCall:
				if delta.ToolIndex == nil {
					return nil, errors.New("streamed tool call is missing index")
				}
				key := fmt.Sprintf("tool:%d", *delta.ToolIndex)
				block, created := renderer.block(key, semantic.ContentToolCall)
				if created {
					if delta.CallID == "" || delta.Name == "" {
						return nil, errors.New("first tool delta is missing identity")
					}
					index := block.index
					events = append(events, anthropicapi.StreamEvent{Type: "content_block_start", Index: &index, ContentBlock: map[string]any{"type": "tool_use", "id": delta.CallID, "name": delta.Name, "input": map[string]any{}}})
				}
				if delta.ArgumentsFragment != "" {
					index := block.index
					events = append(events, anthropicapi.StreamEvent{Type: "content_block_delta", Index: &index, Delta: map[string]any{"type": "input_json_delta", "partial_json": delta.ArgumentsFragment}})
				}
			default:
				return nil, errors.New("provider stream contains non-portable content")
			}
		}
		if output.Termination != "" {
			renderer.termination = output.Termination
		}
	}
	return events, nil
}

func (renderer *StreamRenderer) Complete() ([]anthropicapi.StreamEvent, error) {
	if renderer == nil || !renderer.started {
		return nil, errors.New("provider stream emitted no message")
	}
	if renderer.finished {
		return nil, nil
	}
	if renderer.termination == "" {
		return nil, errors.New("provider stream did not terminate")
	}
	closed, err := renderer.closeBlocks()
	if err != nil {
		return nil, err
	}
	stop := renderStopReason(renderer.termination)
	renderer.finished = true
	return append(closed,
		anthropicapi.StreamEvent{Type: "message_delta", Delta: map[string]any{"stop_reason": stop, "stop_sequence": nil}, Usage: &anthropicapi.Usage{}},
		anthropicapi.StreamEvent{Type: "message_stop"},
	), nil
}

func (renderer *StreamRenderer) block(key string, kind semantic.ContentKind) (*streamBlock, bool) {
	if block := renderer.blocks[key]; block != nil {
		return block, false
	}
	block := &streamBlock{index: renderer.nextIndex, kind: kind, open: true}
	renderer.nextIndex++
	renderer.blocks[key] = block
	return block, true
}

func (renderer *StreamRenderer) closeBlocks() ([]anthropicapi.StreamEvent, error) {
	blocks := make([]*streamBlock, 0, len(renderer.blocks))
	for _, block := range renderer.blocks {
		if block.open {
			blocks = append(blocks, block)
		}
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].index < blocks[j].index })
	events := make([]anthropicapi.StreamEvent, 0, len(blocks))
	for _, block := range blocks {
		block.open = false
		index := block.index
		events = append(events, anthropicapi.StreamEvent{Type: "content_block_stop", Index: &index})
	}
	return events, nil
}
