package semantic

import (
	"errors"
)

const MaxToolArgumentsBytes = 1 << 20

type Role string

const (
	RoleSystem    Role = "system"
	RoleDeveloper Role = "developer"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type ContentKind string

const (
	ContentText       ContentKind = "text"
	ContentInputImage ContentKind = "input_image"
	ContentToolCall   ContentKind = "tool_call"
	ContentToolResult ContentKind = "tool_result"
	ContentReasoning  ContentKind = "reasoning"
)

// Content is the smallest portable content vocabulary needed by the current
// Chat Completions profiles. Provider-native payloads are never stored here.
type Content struct {
	Kind      ContentKind `json:"kind"`
	Text      string      `json:"text,omitempty"`
	URL       string      `json:"url,omitempty"`
	Detail    string      `json:"detail,omitempty"`
	CallID    string      `json:"call_id,omitempty"`
	Name      string      `json:"name,omitempty"`
	Arguments string      `json:"arguments,omitempty"`
	// ToolError marks a tool result the caller reported as failed. It is only
	// meaningful on ContentToolResult. Anthropic models it (tool_result.is_error)
	// and the OpenAI wire does not, so a profile that cannot carry it declares the
	// loss in compatibility.UnsupportedGenerateFields rather than dropping it:
	// feeding a failed tool result to a model as if it had succeeded changes what
	// the model is answering.
	ToolError bool `json:"tool_error,omitempty"`
}

type Message struct {
	Role    Role      `json:"role"`
	Name    string    `json:"name,omitempty"`
	Content []Content `json:"content,omitempty"`
}

func (message Message) Validate() error {
	switch message.Role {
	case RoleSystem, RoleDeveloper, RoleUser, RoleAssistant, RoleTool:
	default:
		return errors.New("semantic message role is invalid")
	}
	reasoningParts := 0
	toolResultCallID := ""
	for _, part := range message.Content {
		switch part.Kind {
		case ContentText:
			if part.URL != "" || part.Detail != "" || part.CallID != "" || part.Name != "" || part.Arguments != "" {
				return errors.New("semantic text content has unrelated fields")
			}
		case ContentReasoning:
			reasoningParts++
			if message.Role != RoleAssistant || part.URL != "" || part.Detail != "" || part.CallID != "" || part.Name != "" || part.Arguments != "" {
				return errors.New("semantic reasoning content is invalid")
			}
		case ContentInputImage:
			if message.Role != RoleUser || part.URL == "" || part.Text != "" || part.CallID != "" || part.Name != "" || part.Arguments != "" {
				return errors.New("semantic image content is missing url")
			}
		case ContentToolCall:
			if message.Role != RoleAssistant || part.CallID == "" || part.Name == "" || len(part.Arguments) > MaxToolArgumentsBytes || part.Text != "" || part.URL != "" || part.Detail != "" {
				return errors.New("semantic tool call is invalid")
			}
		case ContentToolResult:
			if message.Role != RoleTool || part.CallID == "" || part.URL != "" || part.Detail != "" || part.Name != "" || part.Arguments != "" {
				return errors.New("semantic tool result is missing call id")
			}
			if toolResultCallID == "" {
				toolResultCallID = part.CallID
			}
			if toolResultCallID != part.CallID {
				return errors.New("semantic tool result message has multiple call ids")
			}
		default:
			return errors.New("semantic content kind is invalid")
		}
	}
	if reasoningParts > 1 {
		return errors.New("semantic message contains multiple reasoning parts")
	}
	if message.Role == RoleTool && (len(message.Content) == 0 || toolResultCallID == "") {
		return errors.New("semantic tool message has no result")
	}
	return nil
}
