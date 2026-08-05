package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/akz142857/Heimdall/internal/anthropicapi"
	anthropicwire "github.com/akz142857/Heimdall/internal/compatibility/anthropic"
	openaiwire "github.com/akz142857/Heimdall/internal/compatibility/openai"
	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/openaiapi"
	"github.com/akz142857/Heimdall/internal/provider"
	"github.com/akz142857/Heimdall/internal/semantic"
	"github.com/akz142857/Heimdall/internal/sse"
)

const maxResponseBytes = 16 << 20

type Options struct {
	Endpoint         *url.URL
	Authorizer       provider.Authorizer
	Client           *http.Client
	Capabilities     provider.Capabilities
	ProviderType     string
	CredentialScheme domain.CredentialScheme
	MessagesPath     string
}

type Adapter struct {
	endpoint     *url.URL
	authorizer   provider.Authorizer
	client       *http.Client
	capabilities provider.Capabilities
	providerType string
	messagesPath string
}

func New(options Options) (*Adapter, error) {
	if options.Endpoint == nil || options.Authorizer == nil || options.Client == nil {
		return nil, errors.New("endpoint, authorizer, and client are required")
	}
	expectedScheme := options.CredentialScheme
	if expectedScheme == "" {
		expectedScheme = domain.CredentialAnthropicAPIKey
	}
	if options.Authorizer.Scheme() != expectedScheme {
		return nil, errors.New("credential scheme does not match Anthropic profile")
	}
	providerType := options.ProviderType
	if providerType == "" {
		providerType = string(domain.ProviderAnthropic)
	}
	return &Adapter{endpoint: options.Endpoint, authorizer: options.Authorizer, client: options.Client, capabilities: options.Capabilities, providerType: providerType, messagesPath: options.MessagesPath}, nil
}

func (adapter *Adapter) Type() string                        { return adapter.providerType }
func (adapter *Adapter) Capabilities() provider.Capabilities { return adapter.capabilities }
func (adapter *Adapter) Close()                              { adapter.authorizer.Close(); adapter.client.CloseIdleConnections() }

func (adapter *Adapter) Probe(ctx context.Context, model string) error {
	request := anthropicapi.MessageRequest{Model: model, MaxTokens: 1, Messages: []anthropicapi.MessageParam{{Role: "user", Content: anthropicapi.ContentBlocks{{Type: "text", Text: "ping"}}}}}
	payload, _ := json.Marshal(request)
	_, err := adapter.MessagesNative(ctx, provider.NativeMessageCall{RequestID: "probe", ProviderModel: model, Version: anthropicapi.SupportedVersion, Payload: payload})
	return err
}

func (adapter *Adapter) Chat(ctx context.Context, call provider.ChatCall) (openaiapi.ChatCompletionResponse, error) {
	canonical, err := openaiwire.DecodeGenerate(call.Request)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, badRequest("decode portable request", err)
	}
	request, err := anthropicwire.RenderPortableRequest(canonical, call.ProviderModel)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, badRequest("render Anthropic request", err)
	}
	request.Stream = false
	payload, err := json.Marshal(request)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, badRequest("encode Anthropic request", err)
	}
	result, err := adapter.MessagesNative(ctx, provider.NativeMessageCall{RequestID: call.RequestID, ProviderModel: call.ProviderModel, Version: anthropicapi.SupportedVersion, Payload: payload})
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, err
	}
	message, err := anthropicapi.DecodeMessage(result.Payload)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, malformed("decode Anthropic response", err)
	}
	semanticResult, err := anthropicwire.DecodeResult(message)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, malformed("normalize Anthropic response", err)
	}
	semanticResult.ProviderRequestID = result.ProviderRequestID
	return openaiwire.RenderGenerateResult(semanticResult)
}

func (adapter *Adapter) ChatStream(ctx context.Context, call provider.ChatCall, emit func(semantic.Event) error) (*openaiapi.Usage, error) {
	canonical, err := openaiwire.DecodeGenerate(call.Request)
	if err != nil {
		return nil, badRequest("decode portable stream request", err)
	}
	request, err := anthropicwire.RenderPortableRequest(canonical, call.ProviderModel)
	if err != nil {
		return nil, badRequest("render Anthropic stream request", err)
	}
	request.Stream = true
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, badRequest("encode Anthropic stream request", err)
	}
	bridge := newPortableStreamBridge(emit)
	usage, err := adapter.MessagesNativeStream(ctx, provider.NativeMessageCall{RequestID: call.RequestID, ProviderModel: call.ProviderModel, Version: anthropicapi.SupportedVersion, Payload: payload}, bridge.Accept)
	if err == nil {
		err = bridge.Finalize()
	}
	if usage == nil {
		usage = bridge.usage
	}
	if usage == nil {
		return nil, err
	}
	return &openaiapi.Usage{PromptTokens: usage.InputTokens, CompletionTokens: usage.OutputTokens, TotalTokens: usage.InputTokens + usage.OutputTokens}, err
}

func (adapter *Adapter) Embed(context.Context, provider.EmbeddingCall) (openaiapi.EmbeddingResponse, error) {
	return openaiapi.EmbeddingResponse{}, &provider.Error{Class: provider.ErrorBadRequest, Message: "Anthropic profile does not support embeddings"}
}

func (adapter *Adapter) MessagesNative(ctx context.Context, call provider.NativeMessageCall) (provider.NativeMessageResult, error) {
	payload, err := preparePayload(call, false)
	if err != nil {
		return provider.NativeMessageResult{}, badRequest("prepare Anthropic request", err)
	}
	request, err := adapter.request(ctx, call, payload, false)
	if err != nil {
		return provider.NativeMessageResult{}, err
	}
	response, err := adapter.client.Do(request)
	if err != nil {
		return provider.NativeMessageResult{}, transportError(err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return provider.NativeMessageResult{}, decodeHTTPError(response)
	}
	body, err := readLimited(response.Body, maxResponseBytes)
	if err != nil {
		return provider.NativeMessageResult{}, malformed("read Anthropic response", err)
	}
	if _, err := anthropicapi.DecodeMessage(body); err != nil {
		return provider.NativeMessageResult{}, malformed("validate Anthropic response", err)
	}
	return provider.NativeMessageResult{Payload: body, ProviderRequestID: upstreamRequestID(response.Header), RetryAfter: parseRetryAfter(response.Header)}, nil
}

func (adapter *Adapter) MessagesNativeStream(ctx context.Context, call provider.NativeMessageCall, emit func(anthropicapi.RawStreamEvent) error) (*anthropicapi.Usage, error) {
	if emit == nil {
		return nil, badRequest("Anthropic stream callback is required", nil)
	}
	payload, err := preparePayload(call, true)
	if err != nil {
		return nil, badRequest("prepare Anthropic stream request", err)
	}
	request, err := adapter.request(ctx, call, payload, true)
	if err != nil {
		return nil, err
	}
	response, err := adapter.client.Do(request)
	if err != nil {
		return nil, transportError(err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, decodeHTTPError(response)
	}
	validator := anthropicapi.NewStreamValidator()
	decoder := sse.NewDecoder(response.Body, semantic.MaxEncodedEventBytes)
	var usage *anthropicapi.Usage
	emitted := false
	for {
		event, nextErr := decoder.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return usage, &provider.Error{Class: provider.ErrorMalformed, Ambiguous: emitted, Message: "read Anthropic stream", Cause: nextErr}
		}
		raw := anthropicapi.RawStreamEvent{Type: event.Event, Data: append(json.RawMessage(nil), event.Data...)}
		if err := validator.Accept(raw); err != nil {
			return usage, &provider.Error{Class: provider.ErrorMalformed, Ambiguous: emitted, Message: "validate Anthropic stream event " + raw.Type, Cause: err}
		}
		usage = updateUsage(usage, raw)
		if err := emit(raw); err != nil {
			return usage, err
		}
		emitted = true
		if raw.Type == "error" {
			return usage, &provider.Error{Class: provider.ErrorProvider5xx, Retryable: false, Ambiguous: true, ProviderCode: "stream_error_event", Message: "Anthropic stream returned an error event"}
		}
	}
	if err := validator.Finalize(); err != nil {
		return usage, &provider.Error{Class: provider.ErrorMalformed, Ambiguous: emitted, Message: "Anthropic stream ended early", Cause: err}
	}
	return usage, nil
}

func (adapter *Adapter) request(ctx context.Context, call provider.NativeMessageCall, payload []byte, stream bool) (*http.Request, error) {
	endpoint := *adapter.endpoint
	endpoint.Path = strings.TrimRight(endpoint.Path, "/")
	if adapter.messagesPath != "" {
		endpoint.Path += "/" + strings.Trim(adapter.messagesPath, "/")
	} else {
		if !strings.HasSuffix(endpoint.Path, "/v1") {
			endpoint.Path += "/v1"
		}
		endpoint.Path += "/messages"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, badRequest("create Anthropic request", err)
	}
	if err := adapter.authorizer.Authorize(request, nil); err != nil {
		return nil, &provider.Error{Class: provider.ErrorAuthentication, Message: "authorize Anthropic request", Cause: err}
	}
	request.Header.Set("anthropic-version", call.Version)
	request.Header.Set("content-type", "application/json")
	if stream {
		request.Header.Set("accept", "text/event-stream")
	} else {
		request.Header.Set("accept", "application/json")
	}
	request.Header.Set("x-request-id", call.RequestID)
	return request, nil
}

func preparePayload(call provider.NativeMessageCall, stream bool) ([]byte, error) {
	request, err := anthropicapi.DecodeMessageRequest(bytes.NewReader(call.Payload))
	if err != nil {
		return nil, err
	}
	request.Model, request.Stream, request.Raw = call.ProviderModel, stream, nil
	return json.Marshal(request)
}

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	payload, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) > limit {
		return nil, errors.New("response exceeds size limit")
	}
	return payload, nil
}

func decodeHTTPError(response *http.Response) error {
	payload, _ := readLimited(response.Body, 1<<20)
	var envelope anthropicapi.ErrorResponse
	_ = json.Unmarshal(payload, &envelope)
	class, retryable := provider.ErrorBadRequest, false
	switch response.StatusCode {
	case 401, 403:
		class = provider.ErrorAuthentication
	case 429:
		class, retryable = provider.ErrorRateLimit, true
	case 500, 502, 503, 504, 529:
		class, retryable = provider.ErrorProvider5xx, true
	}
	message := envelope.Error.Message
	if message == "" {
		message = http.StatusText(response.StatusCode)
	}
	return &provider.Error{Class: class, StatusCode: response.StatusCode, Retryable: retryable, Message: message, ProviderRequestID: upstreamRequestID(response.Header), ProviderCode: envelope.Error.Type, RetryAfter: parseRetryAfter(response.Header)}
}

func upstreamRequestID(header http.Header) string {
	for _, name := range []string{"request-id", "x-amzn-requestid", "x-amzn-request-id", "x-request-id"} {
		if value := strings.TrimSpace(header.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func parseRetryAfter(header http.Header) time.Duration {
	value := strings.TrimSpace(header.Get("retry-after"))
	if value == "" {
		return 0
	}
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(0, time.Until(at))
	}
	return 0
}

func transportError(err error) error {
	class := provider.ErrorConnect
	if errors.Is(err, context.DeadlineExceeded) {
		class = provider.ErrorTimeout
	}
	return &provider.Error{Class: class, Retryable: true, Ambiguous: !provider.Unsent(err), Message: "Anthropic request failed", Cause: err}
}
func badRequest(message string, cause error) error {
	return &provider.Error{Class: provider.ErrorBadRequest, Message: message, Cause: cause}
}
func malformed(message string, cause error) error {
	return &provider.Error{Class: provider.ErrorMalformed, Ambiguous: true, Message: message, Cause: cause}
}

func updateUsage(current *anthropicapi.Usage, event anthropicapi.RawStreamEvent) *anthropicapi.Usage {
	if current == nil {
		current = &anthropicapi.Usage{}
	}
	if event.Type == "message_start" {
		var value struct {
			Message struct {
				Usage anthropicapi.Usage `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(event.Data, &value) == nil {
			*current = value.Message.Usage
		}
	}
	if event.Type == "message_delta" {
		var value struct {
			Usage anthropicapi.Usage `json:"usage"`
		}
		if json.Unmarshal(event.Data, &value) == nil {
			if value.Usage.InputTokens != 0 {
				current.InputTokens = value.Usage.InputTokens
			}
			current.OutputTokens = value.Usage.OutputTokens
			current.ThinkingTokens = value.Usage.ThinkingTokens
		}
	}
	return current
}

type portableStreamBridge struct {
	emit       func(semantic.Event) error
	id, model  string
	created    int64
	blocks     map[int]portableBlock
	usage      *anthropicapi.Usage
	terminated bool
}

type portableBlock struct {
	kind, callID, name string
	toolIndex          int
}

func newPortableStreamBridge(emit func(semantic.Event) error) *portableStreamBridge {
	return &portableStreamBridge{emit: emit, blocks: map[int]portableBlock{}}
}

func (bridge *portableStreamBridge) Accept(event anthropicapi.RawStreamEvent) error {
	bridge.usage = updateUsage(bridge.usage, event)
	switch event.Type {
	case "message_start":
		var value struct {
			Message anthropicapi.Message `json:"message"`
		}
		if err := json.Unmarshal(event.Data, &value); err != nil {
			return err
		}
		bridge.id, bridge.model = value.Message.ID, value.Message.Model
	case "content_block_start":
		var value struct {
			Index        int                       `json:"index"`
			ContentBlock anthropicapi.ContentBlock `json:"content_block"`
		}
		if err := json.Unmarshal(event.Data, &value); err != nil {
			return err
		}
		if value.ContentBlock.Type == "thinking" || value.ContentBlock.Type == "redacted_thinking" {
			return errors.New("signed thinking requires native mode")
		}
		block := portableBlock{kind: value.ContentBlock.Type, callID: value.ContentBlock.ID, name: value.ContentBlock.Name, toolIndex: value.Index}
		bridge.blocks[value.Index] = block
		if block.kind == "tool_use" {
			index := block.toolIndex
			return bridge.emitEvent(semantic.OutputDelta{Index: 0, Role: semantic.RoleAssistant, Content: []semantic.ContentDelta{{Kind: semantic.ContentToolCall, ToolIndex: &index, CallID: block.callID, Name: block.name}}})
		}
	case "content_block_delta":
		var value struct {
			Index int `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(event.Data, &value); err != nil {
			return err
		}
		block := bridge.blocks[value.Index]
		if value.Delta.Type == "text_delta" {
			return bridge.emitEvent(semantic.OutputDelta{Index: 0, Role: semantic.RoleAssistant, Content: []semantic.ContentDelta{{Kind: semantic.ContentText, Text: value.Delta.Text}}})
		}
		if value.Delta.Type == "input_json_delta" {
			index := block.toolIndex
			return bridge.emitEvent(semantic.OutputDelta{Index: 0, Content: []semantic.ContentDelta{{Kind: semantic.ContentToolCall, ToolIndex: &index, ArgumentsFragment: value.Delta.PartialJSON}}})
		}
	case "message_delta":
		var value struct {
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(event.Data, &value); err != nil {
			return err
		}
		bridge.terminated = true
		if err := bridge.emitEvent(semantic.OutputDelta{Index: 0, Termination: anthropicwireDecodeStop(value.Delta.StopReason), NativeTermination: value.Delta.StopReason}); err != nil {
			return err
		}
		if bridge.usage != nil {
			return bridge.emit(semantic.Event{Kind: semantic.EventUsage, ID: bridge.id, Model: bridge.model, Usage: &semantic.Usage{InputTokens: bridge.usage.InputTokens, OutputTokens: bridge.usage.OutputTokens, TotalTokens: bridge.usage.InputTokens + bridge.usage.OutputTokens, Source: semantic.UsageProviderReported}, Translation: semantic.TranslationNone, MappingRevision: anthropicwire.MappingRevision})
		}
	}
	return nil
}

func (bridge *portableStreamBridge) emitEvent(output semantic.OutputDelta) error {
	return bridge.emit(semantic.Event{Kind: semantic.EventDelta, ID: bridge.id, Model: bridge.model, Outputs: []semantic.OutputDelta{output}, Translation: semantic.TranslationNone, MappingRevision: anthropicwire.MappingRevision})
}
func (bridge *portableStreamBridge) Finalize() error {
	if bridge.id == "" || !bridge.terminated {
		return errors.New("Anthropic portable stream did not terminate")
	}
	return nil
}
func anthropicwireDecodeStop(reason string) string {
	switch reason {
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "tool_use", "pause_turn":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}
