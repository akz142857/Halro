package anthropicapi

import (
	"encoding/json"
	"errors"
)

type StreamValidator struct {
	started   bool
	deltaSeen bool
	stopped   bool
	blocks    map[int]string
}

func NewStreamValidator() *StreamValidator { return &StreamValidator{blocks: map[int]string{}} }

func (validator *StreamValidator) Accept(event RawStreamEvent) error {
	if validator == nil || validator.stopped {
		return errors.New("Anthropic stream is already closed")
	}
	if event.Type == "" || len(event.Data) == 0 || !json.Valid(event.Data) {
		return errors.New("invalid Anthropic SSE event")
	}
	var envelope struct {
		Type         string `json:"type"`
		Index        *int   `json:"index,omitempty"`
		ContentBlock struct {
			Type string `json:"type"`
		} `json:"content_block,omitempty"`
		Delta struct {
			Type string `json:"type"`
		} `json:"delta,omitempty"`
	}
	if err := json.Unmarshal(event.Data, &envelope); err != nil {
		return err
	}
	if envelope.Type != event.Type {
		return errors.New("Anthropic SSE event name and type differ")
	}
	switch event.Type {
	case "message_start":
		if validator.started {
			return errors.New("duplicate message_start")
		}
		validator.started = true
	case "ping":
		if !validator.started {
			return errors.New("ping before message_start")
		}
	case "content_block_start":
		if !validator.started || validator.deltaSeen || envelope.Index == nil || envelope.ContentBlock.Type == "" {
			return errors.New("invalid content_block_start ordering")
		}
		if _, exists := validator.blocks[*envelope.Index]; exists {
			return errors.New("duplicate content block index")
		}
		validator.blocks[*envelope.Index] = envelope.ContentBlock.Type
	case "content_block_delta":
		if !validator.started || validator.deltaSeen || envelope.Index == nil {
			return errors.New("invalid content_block_delta ordering")
		}
		kind, exists := validator.blocks[*envelope.Index]
		if !exists {
			return errors.New("delta for unopened content block")
		}
		switch envelope.Delta.Type {
		case "text_delta":
			if kind != "text" {
				return errors.New("text delta for non-text block")
			}
		case "input_json_delta":
			if kind != "tool_use" {
				return errors.New("tool delta for non-tool block")
			}
		case "thinking_delta", "signature_delta":
			if kind != "thinking" {
				return errors.New("thinking delta for non-thinking block")
			}
		default:
			return errors.New("unknown Anthropic content delta")
		}
	case "content_block_stop":
		if !validator.started || validator.deltaSeen || envelope.Index == nil {
			return errors.New("invalid content_block_stop ordering")
		}
		if _, exists := validator.blocks[*envelope.Index]; !exists {
			return errors.New("stop for unopened content block")
		}
		delete(validator.blocks, *envelope.Index)
	case "message_delta":
		if !validator.started || validator.deltaSeen || len(validator.blocks) != 0 {
			return errors.New("invalid message_delta ordering")
		}
		validator.deltaSeen = true
	case "message_stop":
		if !validator.deltaSeen || len(validator.blocks) != 0 {
			return errors.New("message_stop before message_delta")
		}
		validator.stopped = true
	case "error":
		validator.stopped = true
	default:
		return errors.New("unknown Anthropic SSE event")
	}
	return nil
}

func (validator *StreamValidator) Finalize() error {
	if validator == nil || !validator.started || !validator.stopped || len(validator.blocks) != 0 {
		return errors.New("Anthropic stream did not complete")
	}
	return nil
}
