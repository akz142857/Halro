package semantic

import (
	"errors"
)

const MaxToolArgumentsBytes = 1 << 20

// ImageInputTokenCeiling is what one image input contributes to a pre-flight
// token estimate.
//
// An image arrives either as a URL or as base64, and neither is prose: charging
// the encoded bytes at the text ratio estimates six figures of tokens for a
// picture every provider bills in the hundreds. That estimate is not advisory —
// it is what the project input limit, the deployment context window, the TPM
// lease and the budget reservation are all measured against, so an image that
// is counted as text is refused before it reaches a provider that would have
// accepted it happily.
//
// The value is a ceiling over the published image accounting of the providers
// this gateway routes to — OpenAI's high-detail tiling tops out at 1,445 tokens
// on gpt-4o and 2,500 patches on the 5-series, Anthropic's (w×h)/750 caps near
// 1,600 — so the guards stay conservative. It is deliberately not a per-provider
// table: a pre-flight estimate that tried to model each provider's tiling would
// claim a precision it cannot have before the image is decoded, and settlement
// uses provider-reported usage regardless.
const ImageInputTokenCeiling = 2500

// Inline reports whether this content carries its bytes rather than an address
// somebody has to go and get. Only an image has the distinction today, and it is
// answered here because Content owns the URL: the routing filter, the token
// estimate and every wire renderer were each deciding it separately, and a data
// URL that one of them read as an address is how an inline picture reached a
// provider as a link to nowhere.
func (c Content) Inline() bool {
	// The scheme is case-insensitive (RFC 3986 §3.1), and a sender that spells it
	// `DATA:` is still carrying its bytes. Compared by hand rather than with
	// strings.EqualFold because this package's import allowlist is an executable
	// architecture rule, and five ASCII bytes are not worth widening it for.
	const scheme = "data:"
	if len(c.URL) < len(scheme) {
		return false
	}
	for index := 0; index < len(scheme); index++ {
		actual := c.URL[index]
		if actual >= 'A' && actual <= 'Z' {
			actual += 'a' - 'A'
		}
		if actual != scheme[index] {
			return false
		}
	}
	return true
}

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
