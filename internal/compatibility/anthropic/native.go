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
	profiles := []domain.ProviderProfileID{domain.ProfileAnthropicMessages, domain.ProfileBedrockMantleAnthropicMessages, domain.ProfileMiniMaxAnthropicMessages}
	schemas := make([]compatibility.NativeSchema, 0, len(profiles))
	for _, profileID := range profiles {
		validate := validateNativePayload
		if profileID == domain.ProfileMiniMaxAnthropicMessages {
			validate = validateMiniMaxNativePayload
		}
		schemas = append(schemas, compatibility.NativeSchema{
			ProfileID: profileID, SchemaRevision: 1,
			AllowedHeaders: []string{anthropicapi.VersionHeader, anthropicapi.BetaHeader}, MaxPayloadBytes: anthropicapi.MaxRequestBytes, MaxEventBytes: semantic.MaxEncodedEventBytes,
			ValidatePayload: validate, ExtractGovernance: extractNativeGovernance,
		})
	}
	return compatibility.NewNativeSchemaRegistry(schemas...)
}

// validateMiniMaxNativePayload is the Anthropic native validator plus the three
// members MiniMax accepts and then ignores.
//
// Every other native profile is narrower on the portable path than on this one:
// native forwards the caller's bytes, and what the upstream cannot represent it
// refuses. MiniMax breaks that assumption. Its documentation says top_k and
// stop_sequences "会被忽略" — accepted and dropped — and it has no prompt caching
// at all, so a cache_control marker is read as nothing. Forwarding any of the
// three returns 200 for a request that did not happen as written: a completion
// that ran past the caller's stop boundary, sampled differently than they asked,
// and billed at the uncached rate they thought they had avoided.
//
// A refusal here costs the caller one clear error. Forwarding costs them a
// wrong answer they have no way to detect, which is the trade this repository
// resolves the same way everywhere else.
func validateMiniMaxNativePayload(kind compatibility.NativePayloadKind, payload json.RawMessage) error {
	if err := validateNativePayload(kind, payload); err != nil {
		return err
	}
	if kind != compatibility.NativeRequest {
		return nil
	}
	request, err := anthropicapi.DecodeMessageRequest(bytes.NewReader(payload))
	if err != nil {
		return err
	}
	if request.TopK != nil {
		return errors.New("MiniMax ignores top_k rather than honouring it, so it is refused instead of forwarded")
	}
	if len(request.StopSequences) > 0 {
		return errors.New("MiniMax ignores stop_sequences rather than honouring them, so they are refused instead of forwarded")
	}
	// cache_control is not a member of any struct here — it rides inside content
	// blocks, system blocks and tool definitions, which are forwarded as raw
	// bytes. Searching the payload is therefore the only way to see it, and a
	// false positive costs a caller one error on a request that mentions the
	// member name in text they were sending anyway.
	//
	// The reason is not that MiniMax has no cache. It was written that way from
	// the documentation, and a real account contradicted it on 2026-08-31: both
	// the Chat and the Anthropic routes reported 128 cache-read tokens on a
	// repeated prefix. What MiniMax has is caching it manages itself, with no
	// documented member for a caller to steer it. So cache_control is refused
	// because it is a directive this upstream never agreed to read — forwarding
	// it would let a caller believe they had placed a cache breakpoint that
	// nothing acts on.
	if bytes.Contains(payload, []byte(`"cache_control"`)) {
		return errors.New("MiniMax does not document cache_control, so it is refused rather than forwarded and ignored")
	}
	return nil
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
	result := compatibility.NativeDerivedGovernance{EstimatedInputTokens: estimateNativeTokens(int64(len(payload)))}
	if kind != compatibility.NativeRequest {
		return result, nil
	}
	request, err := anthropicapi.DecodeMessageRequest(bytes.NewReader(payload))
	if err != nil {
		return result, err
	}
	result.EstimatedInputTokens = estimateNativeInputTokens(int64(len(payload)), request)
	result.EstimatedOutputTokens = request.MaxTokens
	result.Requirements = NativeRequirements(request)
	return result, nil
}

func estimateNativeTokens(payloadBytes int64) int64 {
	if payloadBytes <= 0 {
		return 1
	}
	return (payloadBytes + 3) / 4
}

// estimateNativeInputTokens keeps a picture from being priced as prose. Native is
// the mode that carries an image as base64 inside the payload, so charging the
// whole payload at the text ratio put a 400 KB photograph six figures of tokens
// over a project limit it would never have reached at Anthropic's own accounting.
// Take each image's encoded source back out and charge it the ceiling instead.
func estimateNativeInputTokens(payloadBytes int64, request anthropicapi.MessageRequest) int64 {
	images := int64(0)
	for _, message := range request.Messages {
		for _, block := range message.Content {
			// A PDF rides the same multimodal pipeline as an image and is billed
			// the same way, which is why NativeRequirements treats them alike.
			if block.Type != "image" && block.Type != "document" {
				continue
			}
			images++
			payloadBytes -= int64(len(nativeSourcePayload(block.Source)))
		}
	}
	if images == 0 {
		return estimateNativeTokens(payloadBytes)
	}
	return estimateNativeTokens(payloadBytes) + images*semantic.ImageInputTokenCeiling
}

// nativeSourcePayload reports the encoded bytes a source carries — the base64 for
// an inline source, the address for a fetched one. An unreadable source counts as
// nothing, which leaves the estimate where it already was rather than crediting a
// request for bytes it did not explain.
func nativeSourcePayload(raw json.RawMessage) string {
	var source struct {
		Data string `json:"data"`
		URL  string `json:"url"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &source) != nil {
		return ""
	}
	if source.Data != "" {
		return source.Data
	}
	return source.URL
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
		// output_config.format is a schema Anthropic enforces, which is
		// structured_outputs. There is no schema-less half of it to raise.
		requirements.StructuredOutputs = len(request.OutputConfig.Format) > 0
	}
	for _, message := range request.Messages {
		for _, block := range message.Content {
			switch block.Type {
			case "image", "document":
				// A PDF is decoded by the same multimodal pipeline as an image; a
				// target that cannot see one cannot read the other, so declaring
				// only images under-reported what the request needs.
				requirements.Vision = true
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
