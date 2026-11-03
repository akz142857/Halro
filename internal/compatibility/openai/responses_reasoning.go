package openai

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

// Chat has one opaque reasoning field. Preserve the original reasoning items
// and their positions relative to tool calls inside that field. Only the
// upstream can authenticate the ciphertext; Halro never interprets it.
const responseReasoningPrefix = "halro.responses.v1."

type responseReasoningState struct {
	Items       []openaiapi.ResponseOutputItem `json:"items"`
	BeforeCalls []string                       `json:"before_calls,omitempty"`
}

func ResponseReasoningContent(items []openaiapi.ResponseOutputItem) (semantic.Content, error) {
	return responseReasoningContent(responseReasoningState{Items: items})
}

func responseReasoningContent(state responseReasoningState) (semantic.Content, error) {
	if len(state.Items) == 0 || (len(state.BeforeCalls) != 0 && len(state.BeforeCalls) != len(state.Items)) {
		return semantic.Content{}, errors.New("invalid Responses reasoning state")
	}
	var text strings.Builder
	for _, item := range state.Items {
		if item.Type != "reasoning" || item.Role != "" || len(item.Content) != 0 || item.CallID != "" || item.Name != "" || item.Arguments != "" || item.Action != nil {
			return semantic.Content{}, errors.New("invalid Responses reasoning item")
		}
		for _, summary := range item.Summary {
			if summary.Type != "summary_text" {
				return semantic.Content{}, errors.New("unsupported Responses reasoning summary")
			}
			text.WriteString(summary.Text)
		}
	}
	// Readable summaries must stay in Content.Text, where the redactor sees
	// them. Encoding a second plaintext copy in this opaque field would bypass
	// outbound redaction. The upstream ciphertext and IDs suffice for replay;
	// the required summary member is sent as an empty array.
	replayItems := make([]openaiapi.ResponseOutputItem, len(state.Items))
	for index, item := range state.Items {
		item.Summary = nil
		replayItems[index] = item
	}
	state.Items = replayItems
	body, err := json.Marshal(state)
	if err != nil {
		return semantic.Content{}, err
	}
	opaque := responseReasoningPrefix + base64.RawURLEncoding.EncodeToString(body)
	if len(opaque) > 64<<10 {
		return semantic.Content{}, errors.New("Responses reasoning state exceeds limit")
	}
	return semantic.Content{Kind: semantic.ContentReasoning, Text: text.String(), EncryptedContent: opaque}, nil
}

func decodeResponseReasoningState(part semantic.Content) (responseReasoningState, error) {
	if part.EncryptedContent == "" {
		return responseReasoningState{}, errors.New("OpenAI Responses replay requires the original opaque reasoning state")
	}
	if !strings.HasPrefix(part.EncryptedContent, responseReasoningPrefix) || len(part.EncryptedContent) > 64<<10 {
		return responseReasoningState{}, errors.New("reasoning state does not belong to the OpenAI Responses profile")
	}
	body, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(part.EncryptedContent, responseReasoningPrefix))
	if err != nil {
		return responseReasoningState{}, errors.New("invalid Responses reasoning state")
	}
	var state responseReasoningState
	if err := strictUnmarshal(body, &state); err != nil {
		return responseReasoningState{}, err
	}
	for _, item := range state.Items {
		if len(item.Summary) > 0 {
			return responseReasoningState{}, errors.New("readable summaries cannot be hidden in opaque reasoning state")
		}
	}
	if _, err := responseReasoningContent(state); err != nil {
		return responseReasoningState{}, err
	}
	return state, nil
}

func ResponseReasoningItems(part semantic.Content) ([]openaiapi.ResponseOutputItem, error) {
	state, err := decodeResponseReasoningState(part)
	return state.Items, err
}
