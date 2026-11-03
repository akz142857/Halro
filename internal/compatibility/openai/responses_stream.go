package openai

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

type responseStreamItem struct {
	item      openaiapi.ResponseOutputItem
	text      strings.Builder
	arguments strings.Builder
}

type ResponseStreamRenderer struct {
	request                   openaiapi.ResponseRequest
	sequence                  int64
	id, model                 string
	created                   int64
	items                     []*responseStreamItem
	textIndex, reasoningIndex int
	tools                     map[int]int
	usage                     *semantic.Usage
	termination               string
	complete                  bool
}

func NewResponseStreamRenderer(request openaiapi.ResponseRequest) *ResponseStreamRenderer {
	return &ResponseStreamRenderer{request: request, textIndex: -1, reasoningIndex: -1, tools: map[int]int{}}
}

func (r *ResponseStreamRenderer) event(kind string, apply func(*openaiapi.ResponseStreamEvent)) openaiapi.ResponseStreamEvent {
	e := openaiapi.ResponseStreamEvent{Type: kind, SequenceNumber: r.sequence}
	r.sequence++
	apply(&e)
	return e
}
func (r *ResponseStreamRenderer) addItem(kind string) (int, []openaiapi.ResponseStreamEvent) {
	index := len(r.items)
	prefix := "msg"
	if kind == "reasoning" {
		prefix = "rs"
	} else if kind == "function_call" {
		prefix = "fc"
	}
	item := openaiapi.ResponseOutputItem{ID: responseItemID(prefix, r.id, index), Type: kind, Status: "in_progress"}
	if kind == "message" {
		item.Role = "assistant"
		item.Content = []openaiapi.ResponseOutputContent{{Type: "output_text", Annotations: []openaiapi.ResponseAnnotation{}, Logprobs: []any{}}}
	}
	if kind == "message" && r.termination == "refusal" {
		item.Content = []openaiapi.ResponseOutputContent{{Type: "refusal"}}
	}
	if kind == "reasoning" {
		item.Summary = []openaiapi.ResponseReasoningSummary{}
	}
	r.items = append(r.items, &responseStreamItem{item: item})
	events := []openaiapi.ResponseStreamEvent{r.event("response.output_item.added", func(e *openaiapi.ResponseStreamEvent) { e.OutputIndex = &index; e.Item = &item })}
	contentIndex := 0
	if kind == "message" {
		part := item.Content[0]
		events = append(events, r.event("response.content_part.added", func(e *openaiapi.ResponseStreamEvent) {
			e.OutputIndex = &index
			e.ContentIndex = &contentIndex
			e.ItemID = item.ID
			e.Part = &part
		}))
	}
	return index, events
}

func (r *ResponseStreamRenderer) Accept(event semantic.Event) ([]openaiapi.ResponseStreamEvent, error) {
	if r == nil || r.complete {
		return nil, errors.New("Responses stream is not active")
	}
	if err := event.Validate(); err != nil {
		return nil, err
	}
	if r.id == "" {
		r.id, r.model, r.created = responseID(event.ID), event.Model, event.Created
	}
	if r.id != responseID(event.ID) || r.model != event.Model {
		return nil, errors.New("Responses stream identity changed")
	}
	var events []openaiapi.ResponseStreamEvent
	if r.sequence == 0 {
		response := baseResponse(r.id, r.created, r.model, r.request)
		events = append(events, r.event("response.created", func(e *openaiapi.ResponseStreamEvent) { e.Response = &response }), r.event("response.in_progress", func(e *openaiapi.ResponseStreamEvent) { e.Response = &response }))
	}
	if event.Kind == semantic.EventUsage {
		r.usage = event.Usage
		return events, nil
	}
	for _, out := range event.Outputs {
		if out.Index != 0 {
			return nil, errors.New("Responses supports one candidate")
		}
		if out.Termination != "" {
			if responseStatus(out.Termination) == "" {
				return nil, errors.New("unsupported Responses termination")
			}
			r.termination = out.Termination
		}
		for _, part := range out.Content {
			var index int
			var started []openaiapi.ResponseStreamEvent
			switch part.Kind {
			case semantic.ContentText:
				if r.textIndex < 0 {
					r.textIndex, started = r.addItem("message")
					events = append(events, started...)
				}
				index = r.textIndex
				r.items[index].text.WriteString(part.Text)
				ci := 0
				events = append(events, r.event("response.output_text.delta", func(e *openaiapi.ResponseStreamEvent) {
					e.OutputIndex = &index
					e.ContentIndex = &ci
					e.ItemID = r.items[index].item.ID
					e.Delta = part.Text
					if r.termination == "refusal" {
						e.Type = "response.refusal.delta"
						return
					}
					empty := []any{}
					e.Logprobs = &empty
				}))
			case semantic.ContentReasoning:
				if r.reasoningIndex < 0 {
					r.reasoningIndex, started = r.addItem("reasoning")
					events = append(events, started...)
				}
				index = r.reasoningIndex
				state := r.items[index]
				if part.EncryptedContent != "" {
					state.item.EncryptedContent = part.EncryptedContent
				}
				if part.Text != "" {
					si := 0
					if state.text.Len() == 0 {
						events = append(events, r.event("response.reasoning_summary_part.added", func(e *openaiapi.ResponseStreamEvent) {
							e.OutputIndex = &index
							e.SummaryIndex = &si
							e.ItemID = state.item.ID
							e.Part = &openaiapi.ResponseOutputContent{Type: "summary_text"}
						}))
					}
					state.text.WriteString(part.Text)
					events = append(events, r.event("response.reasoning_summary_text.delta", func(e *openaiapi.ResponseStreamEvent) {
						e.OutputIndex = &index
						e.SummaryIndex = &si
						e.ItemID = state.item.ID
						e.Delta = part.Text
					}))
				}
			case semantic.ContentToolCall:
				if part.ToolIndex == nil {
					return nil, errors.New("Responses tool delta lacks index")
				}
				var exists bool
				index, exists = r.tools[*part.ToolIndex]
				if !exists {
					if part.CallID == "" || part.Name == "" {
						return nil, errors.New("Responses tool delta lacks identity")
					}
					index = len(r.items)
					r.tools[*part.ToolIndex] = index
					item := openaiapi.ResponseOutputItem{ID: responseItemID("fc", r.id, index), Type: "function_call", Status: "in_progress", CallID: part.CallID, Name: part.Name}
					r.items = append(r.items, &responseStreamItem{item: item})
					events = append(events, r.event("response.output_item.added", func(e *openaiapi.ResponseStreamEvent) { e.OutputIndex = &index; e.Item = &item }))
				}
				state := r.items[index]
				if (part.CallID != "" && part.CallID != state.item.CallID) || (part.Name != "" && part.Name != state.item.Name) {
					return nil, errors.New("Responses tool identity changed")
				}
				if state.arguments.Len()+len(part.ArgumentsFragment) > semantic.MaxToolArgumentBytes {
					return nil, errors.New("Responses tool arguments exceed limit")
				}
				state.arguments.WriteString(part.ArgumentsFragment)
				if part.ArgumentsFragment != "" {
					events = append(events, r.event("response.function_call_arguments.delta", func(e *openaiapi.ResponseStreamEvent) {
						e.OutputIndex = &index
						e.ItemID = state.item.ID
						e.Delta = part.ArgumentsFragment
					}))
				}
			default:
				return nil, errors.New("unsupported Responses stream content")
			}
		}
	}
	return events, nil
}

func (r *ResponseStreamRenderer) Complete() ([]openaiapi.ResponseStreamEvent, error) {
	if r == nil || r.id == "" || r.complete || r.termination == "" {
		return nil, errors.New("Responses stream did not terminate")
	}
	r.complete = true
	var events []openaiapi.ResponseStreamEvent
	if len(r.items) == 0 {
		_, events = r.addItem("message")
	}
	output := make([]openaiapi.ResponseOutputItem, 0, len(r.items))
	for index, state := range r.items {
		index := index
		item := state.item
		item.Status = "completed"
		switch item.Type {
		case "message":
			ci := 0
			content := item.Content[0]
			content.Text = state.text.String()
			if r.termination == "refusal" {
				content.Type = "refusal"
				content.Refusal = content.Text
				content.Text = ""
			}
			item.Content = []openaiapi.ResponseOutputContent{content}
			events = append(events, r.event("response.output_text.done", func(e *openaiapi.ResponseStreamEvent) {
				e.OutputIndex = &index
				e.ContentIndex = &ci
				e.ItemID = item.ID
				e.Text = content.Text
				if content.Type == "refusal" {
					e.Type = "response.refusal.done"
					e.Refusal = content.Refusal
					return
				}
				empty := []any{}
				e.Logprobs = &empty
			}), r.event("response.content_part.done", func(e *openaiapi.ResponseStreamEvent) {
				e.OutputIndex = &index
				e.ContentIndex = &ci
				e.ItemID = item.ID
				e.Part = &content
			}))
		case "reasoning":
			if state.text.Len() > 0 {
				si := 0
				summary := openaiapi.ResponseReasoningSummary{Type: "summary_text", Text: state.text.String()}
				item.Summary = []openaiapi.ResponseReasoningSummary{summary}
				events = append(events, r.event("response.reasoning_summary_text.done", func(e *openaiapi.ResponseStreamEvent) {
					e.OutputIndex = &index
					e.SummaryIndex = &si
					e.ItemID = item.ID
					e.Text = summary.Text
				}), r.event("response.reasoning_summary_part.done", func(e *openaiapi.ResponseStreamEvent) {
					e.OutputIndex = &index
					e.SummaryIndex = &si
					e.ItemID = item.ID
					e.Part = &openaiapi.ResponseOutputContent{Type: "summary_text", Text: summary.Text}
				}))
			}
		case "function_call":
			item.Arguments = state.arguments.String()
			if r.termination != "max_output" && !json.Valid([]byte(item.Arguments)) {
				return nil, errors.New("Responses function arguments are invalid JSON")
			}
			events = append(events, r.event("response.function_call_arguments.done", func(e *openaiapi.ResponseStreamEvent) {
				e.OutputIndex = &index
				e.ItemID = item.ID
				e.Arguments = item.Arguments
				e.Name = item.Name
			}))
		}
		output = append(output, item)
		events = append(events, r.event("response.output_item.done", func(e *openaiapi.ResponseStreamEvent) { e.OutputIndex = &index; e.Item = &item }))
	}
	response := baseResponse(r.id, r.created, r.model, r.request)
	response.Status = responseStatus(r.termination)
	response.Output = output
	response.Usage = renderResponseUsage(r.usage)
	if response.Status == "incomplete" {
		response.IncompleteDetails = map[string]string{"reason": "max_output_tokens"}
	} else {
		completed := r.created
		response.CompletedAt = &completed
	}
	kind := "response." + response.Status
	events = append(events, r.event(kind, func(e *openaiapi.ResponseStreamEvent) { e.Response = &response }))
	return events, nil
}
