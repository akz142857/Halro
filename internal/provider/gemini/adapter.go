package gemini

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
	"github.com/akz142857/Heimdall/internal/sse"
)

const maxResponseBytes = 16 << 20

type Options struct {
	Endpoint *url.URL
	APIKey   []byte
	Client   *http.Client
}

type Adapter struct {
	endpoint *url.URL
	apiKey   []byte
	client   *http.Client
}

type content struct {
	Role  string `json:"role,omitempty"`
	Parts []part `json:"parts"`
}

type part struct {
	Text string `json:"text,omitempty"`
}

type generationConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	MaxOutputTokens *int64   `json:"maxOutputTokens,omitempty"`
	CandidateCount  *int     `json:"candidateCount,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
}

type generateRequest struct {
	Contents          []content        `json:"contents"`
	SystemInstruction *content         `json:"systemInstruction,omitempty"`
	GenerationConfig  generationConfig `json:"generationConfig,omitempty"`
}

type generateResponse struct {
	Candidates    []candidate   `json:"candidates"`
	UsageMetadata usageMetadata `json:"usageMetadata"`
	ModelVersion  string        `json:"modelVersion"`
	ResponseID    string        `json:"responseId"`
}

type candidate struct {
	Content      content `json:"content"`
	FinishReason string  `json:"finishReason"`
	Index        int     `json:"index"`
}

type usageMetadata struct {
	PromptTokenCount     int64 `json:"promptTokenCount"`
	CandidatesTokenCount int64 `json:"candidatesTokenCount"`
	TotalTokenCount      int64 `json:"totalTokenCount"`
}

type embeddingResponse struct {
	Embedding struct {
		Values []float64 `json:"values"`
	} `json:"embedding"`
}

func New(options Options) (*Adapter, error) {
	if options.Endpoint == nil || options.Client == nil {
		return nil, errors.New("endpoint and client are required")
	}
	if len(options.APIKey) == 0 {
		return nil, errors.New("api key is required")
	}
	key := bytes.Clone(options.APIKey)
	endpoint := *options.Endpoint
	return &Adapter{endpoint: &endpoint, apiKey: key, client: options.Client}, nil
}

func (a *Adapter) Type() string { return "gemini" }

func (a *Adapter) Capabilities() provider.Capabilities {
	return provider.Capabilities{Chat: true, Streaming: true, Embeddings: true, DeveloperRole: true}
}

func (a *Adapter) Close() {
	clear(a.apiKey)
	a.apiKey = nil
	a.client.CloseIdleConnections()
}

func (a *Adapter) Probe(ctx context.Context, model string) error {
	var endpoint url.URL
	var err error
	if strings.TrimSpace(model) == "" {
		endpoint = a.collectionURL()
	} else {
		endpoint, err = a.modelURL(model, "")
		if err != nil {
			return err
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return badRequest("create Gemini probe", err)
	}
	a.authorize(request)
	response, err := a.client.Do(request)
	if err != nil {
		return transportError("Gemini probe failed", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return httpError(response.StatusCode, response.Body)
	}
	_, err = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes+1))
	return err
}

func (a *Adapter) Chat(ctx context.Context, call provider.ChatCall) (openaiapi.ChatCompletionResponse, error) {
	payload, err := translateChat(call.Request)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, err
	}
	var response generateResponse
	if err := a.postJSON(ctx, call.ProviderModel, "generateContent", call.RequestID, payload, &response); err != nil {
		return openaiapi.ChatCompletionResponse{}, err
	}
	return translateResponse(response, call.ProviderModel, call.RequestID)
}

func (a *Adapter) ChatStream(ctx context.Context, call provider.ChatCall, emit func(semantic.Event) error) (*openaiapi.Usage, error) {
	if emit == nil {
		return nil, badRequest("stream callback is required", nil)
	}
	payload, err := translateChat(call.Request)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, badRequest("encode Gemini request", err)
	}
	endpoint, err := a.modelURL(call.ProviderModel, "streamGenerateContent")
	if err != nil {
		return nil, err
	}
	query := endpoint.Query()
	query.Set("alt", "sse")
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(encoded))
	if err != nil {
		return nil, badRequest("create Gemini stream request", err)
	}
	a.authorize(request)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("X-Request-ID", call.RequestID)
	response, err := a.client.Do(request)
	if err != nil {
		return nil, transportError("Gemini stream request failed", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, httpError(response.StatusCode, response.Body)
	}
	if !strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		return nil, malformed("Gemini did not return an SSE stream", nil)
	}
	decoder := sse.NewDecoder(response.Body, semantic.MaxEncodedEventBytes)
	var usage *openaiapi.Usage
	emitted := false
	for {
		event, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			if !emitted {
				return usage, malformed("Gemini stream returned no events", nil)
			}
			return usage, nil
		}
		if err != nil {
			return usage, malformed("decode Gemini stream", err)
		}
		if len(event.Data) == 0 {
			continue
		}
		var chunk generateResponse
		if err := json.Unmarshal(event.Data, &chunk); err != nil {
			return usage, malformed("decode Gemini stream chunk", err)
		}
		semanticEvent, chunkUsage, err := translateStreamChunk(chunk, call.ProviderModel, call.RequestID, !emitted)
		if err != nil {
			return usage, err
		}
		if chunkUsage != nil {
			usage = chunkUsage
		}
		if len(semanticEvent.Choices) == 0 {
			continue
		}
		if err := emit(semanticEvent); err != nil {
			return usage, err
		}
		emitted = true
	}
}

func (a *Adapter) Embed(ctx context.Context, call provider.EmbeddingCall) (openaiapi.EmbeddingResponse, error) {
	if call.Request.EncodingFormat == "base64" {
		return openaiapi.EmbeddingResponse{}, badRequest("Gemini Beta supports float embeddings only", nil)
	}
	inputs, err := embeddingInputs(call.Request.Input)
	if err != nil {
		return openaiapi.EmbeddingResponse{}, err
	}
	result := openaiapi.EmbeddingResponse{Object: "list", Model: call.ProviderModel}
	for index, input := range inputs {
		body := struct {
			Content              content `json:"content"`
			OutputDimensionality *int64  `json:"outputDimensionality,omitempty"`
		}{Content: content{Parts: []part{{Text: input}}}, OutputDimensionality: call.Request.Dimensions}
		var response embeddingResponse
		if err := a.postJSON(ctx, call.ProviderModel, "embedContent", call.RequestID, body, &response); err != nil {
			return openaiapi.EmbeddingResponse{}, err
		}
		if len(response.Embedding.Values) == 0 {
			return openaiapi.EmbeddingResponse{}, malformed("Gemini embedding response is empty", nil)
		}
		encoded, _ := json.Marshal(response.Embedding.Values)
		result.Data = append(result.Data, openaiapi.EmbeddingData{Object: "embedding", Embedding: encoded, Index: index})
	}
	inputTokens := call.Request.EstimatedInputTokens()
	result.Usage = &openaiapi.Usage{PromptTokens: inputTokens, TotalTokens: inputTokens}
	return result, nil
}

func translateChat(request openaiapi.ChatCompletionRequest) (generateRequest, error) {
	result := generateRequest{GenerationConfig: generationConfig{
		Temperature: request.Temperature, TopP: request.TopP, CandidateCount: request.N,
	}}
	if request.MaxCompletionTokens != nil {
		result.GenerationConfig.MaxOutputTokens = request.MaxCompletionTokens
	} else {
		result.GenerationConfig.MaxOutputTokens = request.MaxTokens
	}
	if len(request.Stop) > 0 {
		if err := json.Unmarshal(request.Stop, &result.GenerationConfig.StopSequences); err != nil {
			var single string
			if singleErr := json.Unmarshal(request.Stop, &single); singleErr != nil {
				return generateRequest{}, badRequest("Gemini stop must be a string or string array", err)
			}
			result.GenerationConfig.StopSequences = []string{single}
		}
	}
	var system []string
	for _, message := range request.Messages {
		text, ok := openaiapi.DecodeTextContent(message.Content)
		if !ok {
			return generateRequest{}, badRequest("Gemini Beta supports text message content only", nil)
		}
		switch message.Role {
		case "system", "developer":
			system = append(system, text)
		case "user":
			result.Contents = append(result.Contents, content{Role: "user", Parts: []part{{Text: text}}})
		case "assistant":
			if len(message.ToolCalls) != 0 {
				return generateRequest{}, badRequest("Gemini Beta does not support tool calls", nil)
			}
			result.Contents = append(result.Contents, content{Role: "model", Parts: []part{{Text: text}}})
		default:
			return generateRequest{}, badRequest("Gemini Beta does not support tool messages", nil)
		}
	}
	if len(system) != 0 {
		result.SystemInstruction = &content{Parts: []part{{Text: strings.Join(system, "\n")}}}
	}
	if len(result.Contents) == 0 {
		return generateRequest{}, badRequest("Gemini request has no conversation content", nil)
	}
	return result, nil
}

func translateResponse(source generateResponse, model, requestID string) (openaiapi.ChatCompletionResponse, error) {
	if len(source.Candidates) == 0 {
		return openaiapi.ChatCompletionResponse{}, malformed("Gemini response has no candidates", nil)
	}
	result := openaiapi.ChatCompletionResponse{
		ID: responseID(source.ResponseID, requestID), Object: "chat.completion",
		Created: time.Now().Unix(), Model: firstNonEmpty(source.ModelVersion, model), Usage: openAIUsage(source.UsageMetadata),
	}
	for _, candidate := range source.Candidates {
		text := partsText(candidate.Content.Parts)
		finish := finishReason(candidate.FinishReason)
		result.Choices = append(result.Choices, openaiapi.Choice{
			Index: candidate.Index, Message: &openaiapi.Message{Role: "assistant", Content: openaiapi.TextContent(text)},
			FinishReason: finish,
		})
	}
	return result, nil
}

func translateStreamChunk(source generateResponse, model, requestID string, first bool) (semantic.Event, *openaiapi.Usage, error) {
	usage := openAIUsage(source.UsageMetadata)
	event := semantic.Event{
		Kind: semantic.KindDelta, ID: responseID(source.ResponseID, requestID), Object: "chat.completion.chunk",
		Created: time.Now().Unix(), Model: firstNonEmpty(source.ModelVersion, model),
	}
	for _, candidate := range source.Candidates {
		delta := semantic.Delta{Content: openaiapi.TextContent(partsText(candidate.Content.Parts))}
		if first {
			delta.Role = "assistant"
		}
		event.Choices = append(event.Choices, semantic.Choice{
			Index: candidate.Index, Delta: delta, FinishReason: finishReason(candidate.FinishReason),
		})
	}
	if usage != nil {
		event.Usage = &semantic.Usage{InputTokens: usage.PromptTokens, OutputTokens: usage.CompletionTokens, TotalTokens: usage.TotalTokens}
	}
	if len(event.Choices) != 0 {
		if err := event.Validate(); err != nil {
			return semantic.Event{}, usage, malformed("Gemini stream chunk is semantically invalid", err)
		}
	}
	return event, usage, nil
}

func (a *Adapter) postJSON(ctx context.Context, model, operation, requestID string, input, output any) error {
	encoded, err := json.Marshal(input)
	if err != nil {
		return badRequest("encode Gemini request", err)
	}
	endpoint, err := a.modelURL(model, operation)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(encoded))
	if err != nil {
		return badRequest("create Gemini request", err)
	}
	a.authorize(request)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Request-ID", requestID)
	response, err := a.client.Do(request)
	if err != nil {
		return transportError("Gemini request failed", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return httpError(response.StatusCode, response.Body)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return malformed("read Gemini response", err)
	}
	if len(payload) > maxResponseBytes {
		return malformed("Gemini response exceeded size limit", nil)
	}
	if err := json.Unmarshal(payload, output); err != nil {
		return malformed("decode Gemini response", err)
	}
	return nil
}

func (a *Adapter) modelURL(model, operation string) (url.URL, error) {
	model = strings.TrimPrefix(strings.TrimSpace(model), "models/")
	if model == "" || strings.ContainsAny(model, "/\\?#") {
		return url.URL{}, badRequest("invalid Gemini model name", nil)
	}
	endpoint := *a.endpoint
	base := strings.TrimRight(endpoint.Path, "/")
	if !strings.HasSuffix(base, "/v1beta") {
		base += "/v1beta"
	}
	endpoint.Path = base + "/models/" + model
	if operation != "" {
		endpoint.Path += ":" + operation
	}
	endpoint.RawPath = ""
	return endpoint, nil
}

func (a *Adapter) collectionURL() url.URL {
	endpoint := *a.endpoint
	base := strings.TrimRight(endpoint.Path, "/")
	if !strings.HasSuffix(base, "/v1beta") {
		base += "/v1beta"
	}
	endpoint.Path = base + "/models"
	endpoint.RawPath = ""
	return endpoint
}

func (a *Adapter) authorize(request *http.Request) {
	request.Header.Del("Authorization")
	request.Header.Set("x-goog-api-key", string(a.apiKey))
}

func embeddingInputs(raw json.RawMessage) ([]string, error) {
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return []string{single}, nil
	}
	var multiple []string
	if err := json.Unmarshal(raw, &multiple); err != nil || len(multiple) == 0 {
		return nil, badRequest("Gemini embedding input must be a string or string array", err)
	}
	return multiple, nil
}

func openAIUsage(source usageMetadata) *openaiapi.Usage {
	if source.PromptTokenCount == 0 && source.CandidatesTokenCount == 0 && source.TotalTokenCount == 0 {
		return nil
	}
	total := max(source.TotalTokenCount, source.PromptTokenCount+source.CandidatesTokenCount)
	return &openaiapi.Usage{PromptTokens: source.PromptTokenCount, CompletionTokens: source.CandidatesTokenCount, TotalTokens: total}
}

func partsText(parts []part) string {
	var builder strings.Builder
	for _, part := range parts {
		builder.WriteString(part.Text)
	}
	return builder.String()
}

func finishReason(value string) *string {
	if value == "" {
		return nil
	}
	mapped := "stop"
	switch strings.ToUpper(value) {
	case "MAX_TOKENS":
		mapped = "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT":
		mapped = "content_filter"
	}
	return &mapped
}

func responseID(value, requestID string) string {
	if value != "" {
		return value
	}
	return "gemini-" + requestID
}

func firstNonEmpty(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
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

func httpError(status int, body io.Reader) *provider.Error {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 4096))
	result := &provider.Error{StatusCode: status, Message: fmt.Sprintf("Gemini error (%d)", status)}
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
