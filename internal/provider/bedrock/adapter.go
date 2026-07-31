package bedrock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/akz142857/Heimdall/internal/openaiapi"
	"github.com/akz142857/Heimdall/internal/provider"
	"github.com/akz142857/Heimdall/internal/semantic"
)

const maxResponseBytes = 16 << 20

type Options struct {
	Endpoint       *url.URL
	CredentialJSON []byte
	Client         *http.Client
	Now            func() time.Time
}

type Adapter struct {
	endpoint *url.URL
	client   *http.Client
	signer   *signer
}

type textBlock struct {
	Text string `json:"text"`
}

type message struct {
	Role    string      `json:"role"`
	Content []textBlock `json:"content"`
}

type inferenceConfig struct {
	MaxTokens     *int64   `json:"maxTokens,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
	TopP          *float64 `json:"topP,omitempty"`
	StopSequences []string `json:"stopSequences,omitempty"`
}

type converseRequest struct {
	Messages        []message       `json:"messages"`
	System          []textBlock     `json:"system,omitempty"`
	InferenceConfig inferenceConfig `json:"inferenceConfig,omitempty"`
}

type tokenUsage struct {
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
	TotalTokens  int64 `json:"totalTokens"`
}

type converseResponse struct {
	Output struct {
		Message struct {
			Role    string            `json:"role"`
			Content []json.RawMessage `json:"content"`
		} `json:"message"`
	} `json:"output"`
	StopReason string     `json:"stopReason"`
	Usage      tokenUsage `json:"usage"`
}

func New(options Options) (*Adapter, error) {
	if options.Endpoint == nil || options.Client == nil {
		return nil, errors.New("endpoint and client are required")
	}
	decoder := json.NewDecoder(bytes.NewReader(options.CredentialJSON))
	decoder.DisallowUnknownFields()
	var value credentials
	if err := decoder.Decode(&value); err != nil {
		return nil, errors.New("AWS credential must be valid JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("AWS credential contains trailing data")
	}
	signed, err := newSigner(value)
	value.SecretAccessKey = ""
	value.SessionToken = ""
	if err != nil {
		return nil, err
	}
	if !strings.Contains(strings.ToLower(options.Endpoint.Hostname()), "."+signed.region+".") {
		signed.close()
		return nil, errors.New("AWS endpoint host does not match credential region")
	}
	if options.Now != nil {
		signed.now = options.Now
	}
	endpoint := *options.Endpoint
	return &Adapter{endpoint: &endpoint, client: options.Client, signer: signed}, nil
}

func (a *Adapter) Type() string { return "bedrock" }

func (a *Adapter) Capabilities() provider.Capabilities {
	return provider.Capabilities{Chat: true, Streaming: true, DeveloperRole: true, StreamUsage: true}
}

func (a *Adapter) Close() {
	a.signer.close()
	a.client.CloseIdleConnections()
}

func (a *Adapter) Probe(ctx context.Context, model string) error {
	endpoint, err := a.operationURL(model, "converse")
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, endpoint.String(), nil)
	if err != nil {
		return badRequest("create Bedrock probe", err)
	}
	if err := a.signer.sign(request, nil); err != nil {
		return badRequest("sign Bedrock probe", err)
	}
	response, err := a.client.Do(request)
	if err != nil {
		return transportError("Bedrock probe failed", err)
	}
	defer response.Body.Close()
	if response.StatusCode >= 200 && response.StatusCode < 300 ||
		response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusMethodNotAllowed {
		return nil
	}
	return classifyHTTP(response.StatusCode, response.Body)
}

func (a *Adapter) Chat(ctx context.Context, call provider.ChatCall) (openaiapi.ChatCompletionResponse, error) {
	payload, err := translateRequest(call.Request)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, err
	}
	var result converseResponse
	if err := a.postJSON(ctx, call.ProviderModel, "converse", call.RequestID, payload, &result); err != nil {
		return openaiapi.ChatCompletionResponse{}, err
	}
	if result.Output.Message.Role != "assistant" {
		return openaiapi.ChatCompletionResponse{}, malformed("Bedrock response is missing output", nil)
	}
	text, err := responseBlocksText(result.Output.Message.Content)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, err
	}
	finish, err := mapFinishReason(result.StopReason)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, err
	}
	usage, err := openAIUsage(result.Usage)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, err
	}
	return openaiapi.ChatCompletionResponse{
		ID: "bedrock-" + call.RequestID, Object: "chat.completion", Created: time.Now().Unix(), Model: call.ProviderModel,
		Choices: []openaiapi.Choice{{Index: 0, Message: &openaiapi.Message{Role: "assistant", Content: openaiapi.TextContent(text)}, FinishReason: finish}},
		Usage:   usage,
	}, nil
}

func (a *Adapter) ChatStream(ctx context.Context, call provider.ChatCall, emit func(semantic.Event) error) (*openaiapi.Usage, error) {
	if emit == nil {
		return nil, badRequest("stream callback is required", nil)
	}
	payload, err := translateRequest(call.Request)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, badRequest("encode Bedrock request", err)
	}
	endpoint, err := a.operationURL(call.ProviderModel, "converse-stream")
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(encoded))
	if err != nil {
		return nil, badRequest("create Bedrock stream request", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/vnd.amazon.eventstream")
	request.Header.Set("X-Request-ID", call.RequestID)
	if err := a.signer.sign(request, encoded); err != nil {
		return nil, badRequest("sign Bedrock stream request", err)
	}
	response, err := a.client.Do(request)
	if err != nil {
		return nil, transportError("Bedrock stream request failed", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, classifyHTTP(response.StatusCode, response.Body)
	}
	if !strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "application/vnd.amazon.eventstream") {
		return nil, malformed("Bedrock did not return an AWS event stream", nil)
	}
	id := "bedrock-" + call.RequestID
	created := time.Now().Unix()
	var usage *openaiapi.Usage
	sawMessage := false
	sawStop := false
	for {
		frame, err := readStreamMessage(response.Body)
		if errors.Is(err, io.EOF) {
			if !sawMessage {
				return usage, malformed("Bedrock stream ended before messageStart", nil)
			}
			if !sawStop {
				return usage, malformed("Bedrock stream ended before messageStop", nil)
			}
			return usage, nil
		}
		if err != nil {
			return usage, malformed("decode Bedrock event stream", err)
		}
		eventType := frame.Headers[":event-type"]
		if frame.Headers[":message-type"] == "exception" || strings.HasSuffix(strings.ToLower(eventType), "exception") {
			return usage, streamException(eventType)
		}
		event, found, eventUsage, err := translateStreamEvent(eventType, frame.Payload, id, call.ProviderModel, created)
		if err != nil {
			return usage, err
		}
		if eventUsage != nil {
			usage = eventUsage
		}
		if eventType == "messageStop" {
			sawStop = true
		}
		if !found {
			continue
		}
		if event.Choices[0].Delta.Role == "assistant" {
			sawMessage = true
		}
		if err := emit(event); err != nil {
			return usage, err
		}
	}
}

func (a *Adapter) Embed(context.Context, provider.EmbeddingCall) (openaiapi.EmbeddingResponse, error) {
	return openaiapi.EmbeddingResponse{}, badRequest("Bedrock Beta Converse profile does not declare embeddings", nil)
}

func translateRequest(request openaiapi.ChatCompletionRequest) (converseRequest, error) {
	result := converseRequest{InferenceConfig: inferenceConfig{
		Temperature: request.Temperature, TopP: request.TopP,
	}}
	if request.MaxCompletionTokens != nil {
		result.InferenceConfig.MaxTokens = request.MaxCompletionTokens
	} else {
		result.InferenceConfig.MaxTokens = request.MaxTokens
	}
	if len(request.Stop) != 0 {
		if err := json.Unmarshal(request.Stop, &result.InferenceConfig.StopSequences); err != nil {
			var single string
			if json.Unmarshal(request.Stop, &single) != nil {
				return converseRequest{}, badRequest("Bedrock stop must be a string or string array", err)
			}
			result.InferenceConfig.StopSequences = []string{single}
		}
	}
	for _, source := range request.Messages {
		text, ok := openaiapi.DecodeTextContent(source.Content)
		if !ok {
			return converseRequest{}, badRequest("Bedrock Beta supports text message content only", nil)
		}
		switch source.Role {
		case "system", "developer":
			result.System = append(result.System, textBlock{Text: text})
		case "user":
			result.Messages = append(result.Messages, message{Role: "user", Content: []textBlock{{Text: text}}})
		case "assistant":
			if len(source.ToolCalls) != 0 {
				return converseRequest{}, badRequest("Bedrock Beta does not support tool calls", nil)
			}
			result.Messages = append(result.Messages, message{Role: "assistant", Content: []textBlock{{Text: text}}})
		default:
			return converseRequest{}, badRequest("Bedrock Beta does not support tool messages", nil)
		}
	}
	if len(result.Messages) == 0 {
		return converseRequest{}, badRequest("Bedrock request has no conversation content", nil)
	}
	return result, nil
}

func translateStreamEvent(eventType string, payload []byte, id, model string, created int64) (semantic.Event, bool, *openaiapi.Usage, error) {
	base := semantic.Event{Kind: semantic.KindDelta, ID: id, Object: "chat.completion.chunk", Created: created, Model: model}
	switch eventType {
	case "messageStart":
		var body struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal(payload, &body); err != nil || body.Role != "assistant" {
			return semantic.Event{}, false, nil, malformed("decode Bedrock message start", err)
		}
		base.Choices = []semantic.Choice{{Index: 0, Delta: semantic.Delta{Role: "assistant"}}}
	case "contentBlockDelta":
		var body struct {
			Delta map[string]json.RawMessage `json:"delta"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			return semantic.Event{}, false, nil, malformed("decode Bedrock content delta", err)
		}
		if len(body.Delta) != 1 || body.Delta["text"] == nil {
			return semantic.Event{}, false, nil, badRequest("Bedrock Beta received unsupported non-text stream delta", nil)
		}
		var text string
		if err := json.Unmarshal(body.Delta["text"], &text); err != nil {
			return semantic.Event{}, false, nil, malformed("decode Bedrock text delta", err)
		}
		base.Choices = []semantic.Choice{{Index: 0, Delta: semantic.Delta{Content: openaiapi.TextContent(text)}}}
	case "messageStop":
		var body struct {
			StopReason string `json:"stopReason"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			return semantic.Event{}, false, nil, malformed("decode Bedrock message stop", err)
		}
		finish, err := mapFinishReason(body.StopReason)
		if err != nil {
			return semantic.Event{}, false, nil, err
		}
		base.Choices = []semantic.Choice{{Index: 0, Delta: semantic.Delta{}, FinishReason: finish}}
	case "metadata":
		var body struct {
			Usage tokenUsage `json:"usage"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			return semantic.Event{}, false, nil, malformed("decode Bedrock stream metadata", err)
		}
		usage, err := openAIUsage(body.Usage)
		return semantic.Event{}, false, usage, err
	case "contentBlockStart":
		var body struct {
			Start map[string]json.RawMessage `json:"start"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			return semantic.Event{}, false, nil, malformed("decode Bedrock content block start", err)
		}
		if len(body.Start) != 0 {
			return semantic.Event{}, false, nil, badRequest("Bedrock Beta received unsupported non-text stream content", nil)
		}
		return semantic.Event{}, false, nil, nil
	case "contentBlockStop":
		return semantic.Event{}, false, nil, nil
	default:
		return semantic.Event{}, false, nil, malformed("unsupported Bedrock stream event", nil)
	}
	if err := base.Validate(); err != nil {
		return semantic.Event{}, false, nil, malformed("Bedrock stream event is semantically invalid", err)
	}
	return base, true, nil, nil
}

func (a *Adapter) postJSON(ctx context.Context, model, operation, requestID string, input, output any) error {
	encoded, err := json.Marshal(input)
	if err != nil {
		return badRequest("encode Bedrock request", err)
	}
	endpoint, err := a.operationURL(model, operation)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(encoded))
	if err != nil {
		return badRequest("create Bedrock request", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Request-ID", requestID)
	if err := a.signer.sign(request, encoded); err != nil {
		return badRequest("sign Bedrock request", err)
	}
	response, err := a.client.Do(request)
	if err != nil {
		return transportError("Bedrock request failed", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return classifyHTTP(response.StatusCode, response.Body)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(payload) > maxResponseBytes {
		return malformed("read Bedrock response", err)
	}
	if err := json.Unmarshal(payload, output); err != nil {
		return malformed("decode Bedrock response", err)
	}
	return nil
}

func (a *Adapter) operationURL(model, operation string) (url.URL, error) {
	model = strings.TrimSpace(model)
	if model == "" || strings.ContainsAny(model, "?#\\") {
		return url.URL{}, badRequest("invalid Bedrock model id", nil)
	}
	endpoint := *a.endpoint
	basePath := strings.TrimRight(endpoint.Path, "/")
	endpoint.Path = basePath + "/model/" + model + "/" + operation
	endpoint.RawPath = strings.TrimRight((&url.URL{Path: basePath}).EscapedPath(), "/") + "/model/" + url.PathEscape(model) + "/" + operation
	return endpoint, nil
}

func blocksText(blocks []textBlock) string {
	var result strings.Builder
	for _, block := range blocks {
		result.WriteString(block.Text)
	}
	return result.String()
}

func responseBlocksText(blocks []json.RawMessage) (string, error) {
	var result strings.Builder
	for _, encoded := range blocks {
		var block map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &block); err != nil {
			return "", malformed("decode Bedrock output block", err)
		}
		if len(block) != 1 || block["text"] == nil {
			return "", badRequest("Bedrock Beta received unsupported non-text output", nil)
		}
		var text string
		if err := json.Unmarshal(block["text"], &text); err != nil {
			return "", malformed("decode Bedrock output text", err)
		}
		result.WriteString(text)
	}
	return result.String(), nil
}

func openAIUsage(value tokenUsage) (*openaiapi.Usage, error) {
	if value.InputTokens < 0 || value.OutputTokens < 0 || value.TotalTokens < 0 {
		return nil, malformed("Bedrock usage contains negative tokens", nil)
	}
	if value.InputTokens == 0 && value.OutputTokens == 0 && value.TotalTokens == 0 {
		return nil, nil
	}
	return &openaiapi.Usage{PromptTokens: value.InputTokens, CompletionTokens: value.OutputTokens, TotalTokens: max(value.TotalTokens, value.InputTokens+value.OutputTokens)}, nil
}

func mapFinishReason(value string) (*string, error) {
	if value == "" {
		return nil, nil
	}
	mapped := "stop"
	switch value {
	case "end_turn", "stop_sequence":
	case "max_tokens":
		mapped = "length"
	case "guardrail_intervened", "content_filtered":
		mapped = "content_filter"
	default:
		return nil, badRequest("Bedrock Beta received unsupported stop reason", nil)
	}
	return &mapped, nil
}

func classifyHTTP(status int, body io.Reader) *provider.Error {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 4096))
	result := &provider.Error{StatusCode: status, Message: fmt.Sprintf("Bedrock error (%d)", status)}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		result.Class = provider.ErrorAuthentication
	case status == http.StatusRequestTimeout:
		result.Class, result.Retryable = provider.ErrorTimeout, true
	case status == http.StatusTooManyRequests:
		result.Class, result.Retryable = provider.ErrorRateLimit, true
	case status >= 500:
		result.Class, result.Retryable = provider.ErrorProvider5xx, true
	default:
		result.Class = provider.ErrorBadRequest
	}
	return result
}

func streamException(eventType string) *provider.Error {
	if strings.Contains(strings.ToLower(eventType), "throttl") {
		return &provider.Error{Class: provider.ErrorRateLimit, Retryable: true, Message: "Bedrock stream throttled"}
	}
	return &provider.Error{Class: provider.ErrorProvider5xx, Retryable: true, Ambiguous: true, Message: "Bedrock stream exception"}
}

func badRequest(message string, cause error) *provider.Error {
	return &provider.Error{Class: provider.ErrorBadRequest, Message: message, Cause: cause}
}

func malformed(message string, cause error) *provider.Error {
	return &provider.Error{Class: provider.ErrorMalformed, Ambiguous: true, Message: message, Cause: cause}
}

func transportError(message string, err error) *provider.Error {
	class := provider.ErrorConnect
	if errors.Is(err, context.DeadlineExceeded) {
		class = provider.ErrorTimeout
	}
	return &provider.Error{Class: class, Retryable: true, Ambiguous: true, Message: message, Cause: err}
}
