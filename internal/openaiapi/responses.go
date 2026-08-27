package openaiapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ResponseRequest is the deliberately small, stateless Responses API surface
// implemented by Halro. Unknown fields are rejected by DecodeResponseRequest.
type ResponseRequest struct {
	Model             string              `json:"model"`
	Input             json.RawMessage     `json:"input"`
	Instructions      string              `json:"instructions,omitempty"`
	MaxOutputTokens   *int64              `json:"max_output_tokens,omitempty"`
	ParallelToolCalls *bool               `json:"parallel_tool_calls,omitempty"`
	Stream            bool                `json:"stream,omitempty"`
	Store             *bool               `json:"store,omitempty"`
	Temperature       *float64            `json:"temperature,omitempty"`
	TopP              *float64            `json:"top_p,omitempty"`
	Tools             []ResponseTool      `json:"tools,omitempty"`
	ToolChoice        json.RawMessage     `json:"tool_choice,omitempty"`
	Text              *ResponseTextConfig `json:"text,omitempty"`
	Reasoning         *ResponseReasoning  `json:"reasoning,omitempty"`
	User              string              `json:"user,omitempty"`
}

// ProviderExecutedToolWebSearch is the wire type of the one tool this endpoint
// accepts that the upstream runs itself. Routing gates it on a capability an
// operator turns on per connection, because running it originates network calls
// from the provider that never pass through SafeTransport.
const ProviderExecutedToolWebSearch = "web_search"

type ResponseTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

type ResponseTextConfig struct {
	Format ResponseTextFormat `json:"format"`
}

type ResponseTextFormat struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
	Strict      bool            `json:"strict,omitempty"`
}

type ResponseReasoning struct {
	Effort string `json:"effort,omitempty"`
}

type ResponseInputItem struct {
	Type      string          `json:"type,omitempty"`
	Role      string          `json:"role,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	ID        string          `json:"id,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
}

type ResponseInputContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

func DecodeResponseRequest(decoder *json.Decoder) (ResponseRequest, error) {
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var request ResponseRequest
	if err := decoder.Decode(&request); err != nil {
		return ResponseRequest{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return ResponseRequest{}, errors.New("multiple JSON values are not allowed")
		}
		return ResponseRequest{}, err
	}
	if err := request.Validate(); err != nil {
		return ResponseRequest{}, err
	}
	return request, nil
}

func (r ResponseRequest) Validate() error {
	var problems []error
	if r.Model == "" {
		problems = append(problems, errors.New("model is required"))
	}
	if len(r.Input) == 0 || !json.Valid(r.Input) || bytes.Equal(bytes.TrimSpace(r.Input), []byte("null")) {
		problems = append(problems, errors.New("input must be valid, non-null JSON"))
	}
	if r.Store != nil && *r.Store {
		problems = append(problems, errors.New("store=true is unavailable on the stateless endpoint"))
	}
	if r.MaxOutputTokens != nil && *r.MaxOutputTokens < 0 {
		problems = append(problems, errors.New("max_output_tokens cannot be negative"))
	}
	if r.Temperature != nil && (*r.Temperature < 0 || *r.Temperature > 2) {
		problems = append(problems, errors.New("temperature must be between 0 and 2"))
	}
	if r.TopP != nil && (*r.TopP < 0 || *r.TopP > 1) {
		problems = append(problems, errors.New("top_p must be between 0 and 1"))
	}
	if r.Reasoning != nil && r.Reasoning.Effort != "" {
		switch r.Reasoning.Effort {
		case "none", "minimal", "low", "medium", "high", "xhigh":
		default:
			problems = append(problems, errors.New("reasoning.effort is invalid"))
		}
	}
	for index, tool := range r.Tools {
		// web_search is the one tool the caller names rather than describes: it
		// is run by the upstream, so it carries no schema of the caller's and
		// nothing for the caller to answer. Everything else on this endpoint is a
		// function the caller will be called back about.
		if tool.Type == ProviderExecutedToolWebSearch {
			if tool.Name != "" || tool.Description != "" || len(tool.Parameters) > 0 || tool.Strict != nil {
				problems = append(problems, fmt.Errorf("tools[%d] is provider-executed and takes no function fields", index))
			}
			continue
		}
		if tool.Type != "function" || tool.Name == "" {
			problems = append(problems, fmt.Errorf("tools[%d] must be a named function or a supported provider-executed tool", index))
		}
		if len(tool.Parameters) > 0 && (!json.Valid(tool.Parameters) || bytes.TrimSpace(tool.Parameters)[0] != '{') {
			problems = append(problems, fmt.Errorf("tools[%d].parameters must be a JSON object", index))
		}
		if tool.Strict != nil && *tool.Strict {
			problems = append(problems, fmt.Errorf("tools[%d].strict=true cannot be represented losslessly", index))
		}
	}
	if r.Stream && len(r.Tools) > 0 {
		problems = append(problems, errors.New("streaming tools are not available yet"))
	}
	if r.Reasoning != nil {
		problems = append(problems, errors.New("reasoning output cannot be represented losslessly in Phase 1A"))
	}
	if r.Text != nil {
		switch r.Text.Format.Type {
		case "text", "json_object":
			if r.Text.Format.Name != "" || r.Text.Format.Description != "" || len(r.Text.Format.Schema) > 0 || r.Text.Format.Strict {
				problems = append(problems, errors.New("text.format contains fields unrelated to its type"))
			}
		case "json_schema":
			if r.Text.Format.Name == "" || len(r.Text.Format.Schema) == 0 || !json.Valid(r.Text.Format.Schema) {
				problems = append(problems, errors.New("text.format json_schema requires name and schema"))
			}
		default:
			problems = append(problems, errors.New("text.format.type is invalid"))
		}
	}
	return errors.Join(problems...)
}

func (r ResponseRequest) EstimatedInputBytes() int64 {
	return int64(len(r.Input) + len(r.Instructions) + len(r.ToolChoice) + len(r.User))
}

type Response struct {
	ID                 string               `json:"id"`
	Object             string               `json:"object"`
	CreatedAt          int64                `json:"created_at"`
	Status             string               `json:"status"`
	Background         bool                 `json:"background"`
	CompletedAt        *int64               `json:"completed_at"`
	Error              any                  `json:"error"`
	IncompleteDetails  any                  `json:"incomplete_details"`
	Instructions       any                  `json:"instructions"`
	MaxOutputTokens    *int64               `json:"max_output_tokens"`
	Model              string               `json:"model"`
	Output             []ResponseOutputItem `json:"output"`
	ParallelToolCalls  bool                 `json:"parallel_tool_calls"`
	PreviousResponseID any                  `json:"previous_response_id"`
	Reasoning          ResponseReasoningOut `json:"reasoning"`
	Store              bool                 `json:"store"`
	Temperature        *float64             `json:"temperature"`
	Text               ResponseTextOut      `json:"text"`
	ToolChoice         any                  `json:"tool_choice"`
	Tools              []ResponseTool       `json:"tools"`
	TopP               *float64             `json:"top_p"`
	Truncation         string               `json:"truncation"`
	Usage              *ResponseUsage       `json:"usage"`
	Metadata           map[string]string    `json:"metadata"`
}

type ResponseReasoningOut struct {
	Effort  any `json:"effort"`
	Summary any `json:"summary"`
}

type ResponseTextOut struct {
	Format ResponseTextFormat `json:"format"`
}

// OutputItemWebSearchCall is the item an upstream emits to report a search it
// ran during the turn.
const OutputItemWebSearchCall = "web_search_call"

type ResponseOutputItem struct {
	ID        string                  `json:"id"`
	Type      string                  `json:"type"`
	Status    string                  `json:"status"`
	Role      string                  `json:"role,omitempty"`
	Content   []ResponseOutputContent `json:"content,omitempty"`
	CallID    string                  `json:"call_id,omitempty"`
	Name      string                  `json:"name,omitempty"`
	Arguments string                  `json:"arguments,omitempty"`
	Action    *ResponseToolAction     `json:"action,omitempty"`
}

type ResponseOutputContent struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal,omitempty"`
	// Annotations were `[]any` while nothing produced them. A provider-executed
	// search does, and untyped is the one shape that cannot be checked: an
	// annotation kind nobody has modelled would have been copied through as
	// whatever JSON arrived, or re-emitted as an empty list, and neither is a
	// statement about where the answer came from that a reader can trust.
	Annotations []ResponseAnnotation `json:"annotations"`
	Logprobs    []any                `json:"logprobs"`
}

// AnnotationURLCitation is the only annotation type carried. A web search
// attributes a span of the answer to a page it read.
const AnnotationURLCitation = "url_citation"

type ResponseAnnotation struct {
	Type       string `json:"type"`
	URL        string `json:"url,omitempty"`
	Title      string `json:"title,omitempty"`
	StartIndex int    `json:"start_index"`
	EndIndex   int    `json:"end_index"`
}

// ResponseToolAction is what a provider-executed tool was asked to do. For a
// web search that is the query the model wrote, which is the only part of the
// call a reader can check against the answer.
type ResponseToolAction struct {
	Type  string `json:"type"`
	Query string `json:"query,omitempty"`
}

// ToolActionSearch is the one action kind carried.
const ToolActionSearch = "search"

func (content ResponseOutputContent) MarshalJSON() ([]byte, error) {
	if content.Type == "refusal" {
		return json.Marshal(struct {
			Type    string `json:"type"`
			Refusal string `json:"refusal"`
		}{Type: content.Type, Refusal: content.Refusal})
	}
	type alias ResponseOutputContent
	return json.Marshal(alias(content))
}

type ResponseUsage struct {
	InputTokens         int64                `json:"input_tokens"`
	InputTokensDetails  ResponseTokenDetails `json:"input_tokens_details"`
	OutputTokens        int64                `json:"output_tokens"`
	OutputTokensDetails ResponseTokenDetails `json:"output_tokens_details"`
	TotalTokens         int64                `json:"total_tokens"`
}

type ResponseTokenDetails struct {
	CachedTokens    int64 `json:"cached_tokens,omitempty"`
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
	AudioTokens     int64 `json:"audio_tokens,omitempty"`
}

type ResponseStreamEvent struct {
	Type           string                 `json:"type"`
	SequenceNumber int64                  `json:"sequence_number"`
	Response       *Response              `json:"response,omitempty"`
	OutputIndex    *int                   `json:"output_index,omitempty"`
	ContentIndex   *int                   `json:"content_index,omitempty"`
	ItemID         string                 `json:"item_id,omitempty"`
	Item           *ResponseOutputItem    `json:"item,omitempty"`
	Part           *ResponseOutputContent `json:"part,omitempty"`
	Delta          string                 `json:"delta,omitempty"`
	Text           string                 `json:"text,omitempty"`
	Arguments      string                 `json:"arguments,omitempty"`
	Name           string                 `json:"name,omitempty"`
	Code           string                 `json:"code,omitempty"`
	Message        string                 `json:"message,omitempty"`
	Param          *string                `json:"param,omitempty"`
	Logprobs       *[]any                 `json:"logprobs,omitempty"`
}
