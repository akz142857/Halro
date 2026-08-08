package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"

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
			AllowedHeaders: []string{anthropicapi.VersionHeader}, MaxPayloadBytes: anthropicapi.MaxRequestBytes, MaxEventBytes: semantic.MaxEncodedEventBytes,
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
	result.Requirements = semantic.Requirements{Streaming: request.Stream, StreamUsage: request.Stream, Tools: len(request.Tools) > 0 || request.ToolChoice != nil, Reasoning: len(request.Thinking) > 0}
	for _, message := range request.Messages {
		for _, block := range message.Content {
			switch block.Type {
			case "image":
				result.Requirements.InputImage = true
			case "tool_use", "tool_result":
				result.Requirements.Tools = true
			case "thinking", "redacted_thinking":
				result.Requirements.Reasoning = true
			}
		}
	}
	return result, nil
}

func NativeHeaders(version string) http.Header {
	return http.Header{http.CanonicalHeaderKey(anthropicapi.VersionHeader): []string{version}}
}
