package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/akz142857/Halro/internal/anthropicapi"
	"github.com/akz142857/Halro/internal/compatibility"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/semantic"
)

func NewNativeSchemaRegistry() (*compatibility.NativeSchemaRegistry, error) {
	schemas := make([]compatibility.NativeSchema, 0, 2)
	for _, profileID := range []domain.ProviderProfileID{domain.ProfileAnthropicMessages, domain.ProfileBedrockMantleAnthropicMessages} {
		schemas = append(schemas, compatibility.NativeSchema{
			ProfileID: profileID, SchemaRevision: 1,
			AllowedHeaders: []string{anthropicapi.VersionHeader, anthropicapi.BetaHeader}, MaxPayloadBytes: anthropicapi.MaxRequestBytes, MaxEventBytes: semantic.MaxEncodedEventBytes,
			ValidatePayload: validateNativePayload, ExtractGovernance: extractNativeGovernance,
		})
	}
	return compatibility.NewNativeSchemaRegistry(schemas...)
}

func validateNativePayload(kind compatibility.NativePayloadKind, payload json.RawMessage) error {
	switch kind {
	case compatibility.NativeRequest:
		_, err := anthropicapi.DecodeMessageRequest(bytes.NewReader(payload))
		return err
	case compatibility.NativeResponse:
		_, err := anthropicapi.DecodeMessage(payload)
		return err
	case compatibility.NativeEvent:
		var event struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(payload, &event); err != nil {
			return err
		}
		switch event.Type {
		case "message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop", "ping", "error":
			return nil
		default:
			return errors.New("event type is not registered")
		}
	default:
		return errors.New("unknown native payload kind")
	}
}

func extractNativeGovernance(kind compatibility.NativePayloadKind, payload json.RawMessage) (compatibility.NativeDerivedGovernance, error) {
	result := compatibility.NativeDerivedGovernance{EstimatedInputTokens: int64((len(payload) + 3) / 4)}
	if kind != compatibility.NativeRequest {
		return result, nil
	}
	request, err := anthropicapi.DecodeMessageRequest(bytes.NewReader(payload))
	if err != nil {
		return result, err
	}
	result.EstimatedOutputTokens = request.MaxTokens
	result.Requirements = NativeRequirements(request)
	return result, nil
}

// NativeRequirements derives what a native request needs from a target. It is
// exported because routing has to apply it before a target is chosen, and the
// governance envelope is built for a target that is already selected — deriving
// requirements only inside the envelope left them unused by the native path,
// which is how a structured-output request could reach a target whose ceiling
// has no JSON mode.
func NativeRequirements(request anthropicapi.MessageRequest) semantic.Requirements {
	requirements := semantic.Requirements{
		Streaming: request.Stream, StreamUsage: request.Stream,
		Tools:                 len(request.Tools) > 0 || request.ToolChoice != nil,
		Reasoning:             len(request.Thinking) > 0,
		ProviderExecutedTools: request.UsesProviderExecutedTools(),
	}
	if request.OutputConfig != nil {
		requirements.Reasoning = requirements.Reasoning || request.OutputConfig.Effort != ""
		requirements.StructuredJSON = len(request.OutputConfig.Format) > 0
	}
	for _, message := range request.Messages {
		for _, block := range message.Content {
			switch block.Type {
			case "image", "document":
				// A PDF is decoded by the same multimodal pipeline as an image; a
				// target that cannot see one cannot read the other, so declaring
				// only images under-reported what the request needs.
				requirements.InputImage = true
			case "tool_use", "tool_result":
				requirements.Tools = true
			case "thinking", "redacted_thinking":
				requirements.Reasoning = true
			}
		}
	}
	return requirements
}

// NativeHeaders records the headers that will actually reach the provider. The
// beta tokens belong here with the version: they change what the upstream does
// with the request, and an envelope that proves "these bytes, under these
// headers" while omitting them proves the wrong thing.
func NativeHeaders(version string, betas []string) http.Header {
	headers := http.Header{http.CanonicalHeaderKey(anthropicapi.VersionHeader): []string{version}}
	if len(betas) > 0 {
		headers[http.CanonicalHeaderKey(anthropicapi.BetaHeader)] = []string{strings.Join(betas, ",")}
	}
	return headers
}
