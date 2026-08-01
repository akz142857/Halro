package openai

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

	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/openaiapi"
	"github.com/akz142857/Heimdall/internal/provider"
	"github.com/akz142857/Heimdall/internal/semantic"
	"github.com/akz142857/Heimdall/internal/sse"
)

const maxResponseBytes = 16 << 20

type Adapter struct {
	endpoint     *url.URL
	authorizer   provider.Authorizer
	client       *http.Client
	providerType string
	apiVersion   string
	azure        bool
	capabilities provider.Capabilities
}

func New(endpoint *url.URL, apiKey []byte, client *http.Client) (*Adapter, error) {
	return NewWithOptions(Options{
		Endpoint: endpoint, APIKey: apiKey, Client: client, ProviderType: "openai",
		Capabilities: provider.Capabilities{
			Chat: true, Streaming: true, Embeddings: true, Tools: true,
			Vision: true, JSONMode: true, StreamUsage: true,
		},
	})
}

type Options struct {
	Endpoint     *url.URL
	APIKey       []byte
	Client       *http.Client
	ProviderType string
	APIVersion   string
	Azure        bool
	Capabilities provider.Capabilities
	Authorizer   provider.Authorizer
}

func NewWithOptions(options Options) (*Adapter, error) {
	endpoint, apiKey, client := options.Endpoint, options.APIKey, options.Client
	if endpoint == nil || client == nil {
		return nil, errors.New("endpoint and client are required")
	}
	if len(apiKey) == 0 && options.Authorizer == nil {
		return nil, errors.New("api key is required")
	}
	if options.ProviderType == "" {
		options.ProviderType = "openai_compatible"
	}
	if options.Azure && strings.TrimSpace(options.APIVersion) == "" {
		return nil, errors.New("azure api version is required")
	}
	authorizer := options.Authorizer
	if authorizer == nil {
		var err error
		if options.Azure {
			authorizer, err = provider.NewStaticHeaderAuthorizer(domain.CredentialAzureAPIKey, "api-key", "", apiKey, "Authorization")
		} else {
			authorizer, err = provider.NewStaticHeaderAuthorizer(domain.CredentialBearerStatic, "Authorization", "Bearer ", apiKey, "api-key")
		}
		if err != nil {
			return nil, err
		}
	}
	expectedScheme := domain.CredentialBearerStatic
	if options.Azure {
		expectedScheme = domain.CredentialAzureAPIKey
	}
	if authorizer.Scheme() != expectedScheme {
		authorizer.Close()
		return nil, errors.New("credential scheme does not match OpenAI adapter profile")
	}
	return &Adapter{
		endpoint: endpoint, authorizer: authorizer, client: client,
		providerType: options.ProviderType, apiVersion: options.APIVersion,
		azure: options.Azure, capabilities: options.Capabilities,
	}, nil
}

func (a *Adapter) Type() string {
	return a.providerType
}

func (a *Adapter) Capabilities() provider.Capabilities {
	return a.capabilities
}

func (a *Adapter) Probe(ctx context.Context, providerModel string) error {
	var endpoint url.URL
	if a.azure {
		if !validAzureDeployment(providerModel) {
			return &provider.Error{Class: provider.ErrorBadRequest, Message: "azure connection test requires a deployment route"}
		}
		endpoint = a.operationURL(providerModel, "chat/completions")
	} else {
		endpoint = *a.endpoint
		basePath := strings.TrimRight(endpoint.Path, "/")
		if strings.HasSuffix(basePath, "/v1") {
			endpoint.Path = basePath + "/models"
		} else {
			endpoint.Path = basePath + "/v1/models"
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return &provider.Error{Class: provider.ErrorBadRequest, Message: "create provider probe", Cause: err}
	}
	if err := a.authorize(request); err != nil {
		return &provider.Error{Class: provider.ErrorAuthentication, Message: "authorize provider probe", Cause: err}
	}
	request.Header.Set("Accept", "application/json")
	response, err := a.client.Do(request)
	if err != nil {
		class := provider.ErrorConnect
		if errors.Is(err, context.DeadlineExceeded) {
			class = provider.ErrorTimeout
		}
		return &provider.Error{Class: class, Retryable: true, Message: "provider probe failed", Cause: err}
	}
	defer response.Body.Close()
	// Azure has no universal, non-billable data-plane discovery endpoint. A
	// deployment URL returning Method Not Allowed proves DNS/TLS/routing and
	// authentication without generating tokens.
	if a.azure && response.StatusCode == http.StatusMethodNotAllowed {
		return nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return classifyHTTPError(response.StatusCode, limitedErrorMessage(response.Body))
	}
	_, err = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return &provider.Error{Class: provider.ErrorMalformed, Message: "read provider probe response", Cause: err}
	}
	return nil
}

func (a *Adapter) Close() {
	a.authorizer.Close()
	a.client.CloseIdleConnections()
}

func (a *Adapter) Chat(ctx context.Context, call provider.ChatCall) (openaiapi.ChatCompletionResponse, error) {
	if a.azure && !validAzureDeployment(call.ProviderModel) {
		return openaiapi.ChatCompletionResponse{}, &provider.Error{Class: provider.ErrorBadRequest, Message: "invalid azure deployment name"}
	}
	requestBody := call.Request
	if a.azure {
		requestBody.Model = ""
	} else {
		requestBody.Model = call.ProviderModel
	}
	requestBody.Stream = false
	encoded, err := json.Marshal(requestBody)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, &provider.Error{Class: provider.ErrorBadRequest, Message: "encode provider request", Cause: err}
	}
	endpoint := a.operationURL(call.ProviderModel, "chat/completions")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(encoded))
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, &provider.Error{Class: provider.ErrorBadRequest, Message: "create provider request", Cause: err}
	}
	if err := a.authorize(request); err != nil {
		return openaiapi.ChatCompletionResponse{}, &provider.Error{Class: provider.ErrorAuthentication, Message: "authorize provider request", Cause: err}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Request-ID", call.RequestID)

	response, err := a.client.Do(request)
	if err != nil {
		class := provider.ErrorConnect
		if errors.Is(err, context.DeadlineExceeded) {
			class = provider.ErrorTimeout
		}
		return openaiapi.ChatCompletionResponse{}, &provider.Error{
			Class:     class,
			Retryable: class == provider.ErrorConnect || class == provider.ErrorTimeout,
			Ambiguous: true,
			Message:   "provider request failed",
			Cause:     err,
		}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := limitedErrorMessage(response.Body)
		return openaiapi.ChatCompletionResponse{}, classifyHTTPError(response.StatusCode, message)
	}
	limited := io.LimitReader(response.Body, maxResponseBytes+1)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, &provider.Error{Class: provider.ErrorMalformed, Message: "read provider response", Cause: err}
	}
	if len(payload) > maxResponseBytes {
		return openaiapi.ChatCompletionResponse{}, &provider.Error{Class: provider.ErrorMalformed, Message: "provider response exceeded size limit"}
	}
	var result openaiapi.ChatCompletionResponse
	if err := json.Unmarshal(payload, &result); err != nil {
		return openaiapi.ChatCompletionResponse{}, &provider.Error{Class: provider.ErrorMalformed, Message: "decode provider response", Cause: err}
	}
	if result.ID == "" || result.Object == "" || len(result.Choices) == 0 {
		return openaiapi.ChatCompletionResponse{}, &provider.Error{Class: provider.ErrorMalformed, Message: "provider response is missing required fields"}
	}
	return result, nil
}

func (a *Adapter) Embed(ctx context.Context, call provider.EmbeddingCall) (openaiapi.EmbeddingResponse, error) {
	if a.azure && !validAzureDeployment(call.ProviderModel) {
		return openaiapi.EmbeddingResponse{}, &provider.Error{Class: provider.ErrorBadRequest, Message: "invalid azure deployment name"}
	}
	requestBody := call.Request
	if a.azure {
		requestBody.Model = ""
	} else {
		requestBody.Model = call.ProviderModel
	}
	encoded, err := json.Marshal(requestBody)
	if err != nil {
		return openaiapi.EmbeddingResponse{}, &provider.Error{Class: provider.ErrorBadRequest, Message: "encode provider request", Cause: err}
	}
	endpoint := a.operationURL(call.ProviderModel, "embeddings")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(encoded))
	if err != nil {
		return openaiapi.EmbeddingResponse{}, &provider.Error{Class: provider.ErrorBadRequest, Message: "create provider request", Cause: err}
	}
	if err := a.authorize(request); err != nil {
		return openaiapi.EmbeddingResponse{}, &provider.Error{Class: provider.ErrorAuthentication, Message: "authorize provider embedding request", Cause: err}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Request-ID", call.RequestID)
	response, err := a.client.Do(request)
	if err != nil {
		class := provider.ErrorConnect
		if errors.Is(err, context.DeadlineExceeded) {
			class = provider.ErrorTimeout
		}
		return openaiapi.EmbeddingResponse{}, &provider.Error{
			Class:     class,
			Retryable: class == provider.ErrorConnect || class == provider.ErrorTimeout,
			Ambiguous: true,
			Message:   "provider request failed",
			Cause:     err,
		}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return openaiapi.EmbeddingResponse{}, classifyHTTPError(response.StatusCode, limitedErrorMessage(response.Body))
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return openaiapi.EmbeddingResponse{}, &provider.Error{Class: provider.ErrorMalformed, Message: "read provider response", Cause: err}
	}
	if len(payload) > maxResponseBytes {
		return openaiapi.EmbeddingResponse{}, &provider.Error{Class: provider.ErrorMalformed, Message: "provider response exceeded size limit"}
	}
	var result openaiapi.EmbeddingResponse
	if err := json.Unmarshal(payload, &result); err != nil {
		return openaiapi.EmbeddingResponse{}, &provider.Error{Class: provider.ErrorMalformed, Message: "decode provider response", Cause: err}
	}
	if result.Object == "" || len(result.Data) == 0 {
		return openaiapi.EmbeddingResponse{}, &provider.Error{Class: provider.ErrorMalformed, Message: "provider response is missing required fields"}
	}
	return result, nil
}

func (a *Adapter) ChatStream(
	ctx context.Context,
	call provider.ChatCall,
	emit func(semantic.Event) error,
) (*openaiapi.Usage, error) {
	if emit == nil {
		return nil, &provider.Error{Class: provider.ErrorBadRequest, Message: "stream callback is required"}
	}
	if a.azure && !validAzureDeployment(call.ProviderModel) {
		return nil, &provider.Error{Class: provider.ErrorBadRequest, Message: "invalid azure deployment name"}
	}
	requestBody := call.Request
	if a.azure {
		requestBody.Model = ""
	} else {
		requestBody.Model = call.ProviderModel
	}
	requestBody.Stream = true
	if a.capabilities.StreamUsage {
		requestBody.StreamOptions = &openaiapi.StreamOptions{IncludeUsage: true}
	} else {
		requestBody.StreamOptions = nil
	}
	encoded, err := json.Marshal(requestBody)
	if err != nil {
		return nil, &provider.Error{Class: provider.ErrorBadRequest, Message: "encode provider request", Cause: err}
	}
	endpoint := a.operationURL(call.ProviderModel, "chat/completions")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(encoded))
	if err != nil {
		return nil, &provider.Error{Class: provider.ErrorBadRequest, Message: "create provider request", Cause: err}
	}
	if err := a.authorize(request); err != nil {
		return nil, &provider.Error{Class: provider.ErrorAuthentication, Message: "authorize provider stream request", Cause: err}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("X-Request-ID", call.RequestID)
	response, err := a.client.Do(request)
	if err != nil {
		class := provider.ErrorConnect
		if errors.Is(err, context.DeadlineExceeded) {
			class = provider.ErrorTimeout
		}
		return nil, &provider.Error{
			Class: class, Retryable: class == provider.ErrorConnect || class == provider.ErrorTimeout,
			Ambiguous: true, Message: "provider stream request failed", Cause: err,
		}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, classifyHTTPError(response.StatusCode, limitedErrorMessage(response.Body))
	}
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(contentType), "text/event-stream") {
		return nil, &provider.Error{Class: provider.ErrorMalformed, Ambiguous: true, Message: "provider did not return an SSE stream"}
	}
	decoder := sse.NewDecoder(response.Body, semantic.MaxEncodedEventBytes)
	var usage *openaiapi.Usage
	for {
		event, err := decoder.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return usage, &provider.Error{
					Class: provider.ErrorMalformed, Ambiguous: true,
					Message: "provider stream ended before [DONE]",
				}
			}
			return usage, &provider.Error{
				Class: provider.ErrorMalformed, Ambiguous: true,
				Message: "decode provider stream", Cause: err,
			}
		}
		if string(event.Data) == "[DONE]" {
			return usage, nil
		}
		if len(event.Data) == 0 {
			continue
		}
		var chunk openaiapi.ChatCompletionResponse
		if err := json.Unmarshal(event.Data, &chunk); err != nil {
			return usage, &provider.Error{
				Class: provider.ErrorMalformed, Ambiguous: true,
				Message: "decode provider stream chunk", Cause: err,
			}
		}
		semanticEvent, err := semantic.FromOpenAIChunk(chunk)
		if err != nil {
			return usage, &provider.Error{
				Class: provider.ErrorMalformed, Ambiguous: true,
				Message: "provider stream chunk is semantically invalid", Cause: err,
			}
		}
		if chunk.Usage != nil {
			copyUsage := *chunk.Usage
			usage = &copyUsage
		}
		if err := emit(semanticEvent); err != nil {
			return usage, err
		}
	}
}

func (a *Adapter) operationURL(providerModel, operation string) url.URL {
	endpoint := *a.endpoint
	basePath := strings.TrimRight(endpoint.Path, "/")
	if a.azure {
		endpoint.Path = basePath + "/openai/deployments/" + providerModel + "/" + operation
		escapedBase := strings.TrimRight((&url.URL{Path: basePath}).EscapedPath(), "/")
		endpoint.RawPath = escapedBase + "/openai/deployments/" + url.PathEscape(providerModel) + "/" + operation
		query := endpoint.Query()
		query.Set("api-version", a.apiVersion)
		endpoint.RawQuery = query.Encode()
		return endpoint
	}
	if strings.HasSuffix(basePath, "/v1") {
		endpoint.Path = basePath + "/" + operation
	} else {
		endpoint.Path = basePath + "/v1/" + operation
	}
	return endpoint
}

func (a *Adapter) authorize(request *http.Request) error {
	return a.authorizer.Authorize(request, nil)
}

func validAzureDeployment(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' {
			continue
		}
		return false
	}
	return true
}

func classifyHTTPError(status int, message string) *provider.Error {
	result := &provider.Error{StatusCode: status, Message: "provider rejected request"}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		result.Class = provider.ErrorAuthentication
	case status == http.StatusRequestTimeout:
		result.Class = provider.ErrorTimeout
		result.Retryable = true
	case status == http.StatusTooManyRequests:
		result.Class = provider.ErrorRateLimit
		result.Retryable = true
	case status >= 500:
		result.Class = provider.ErrorProvider5xx
		result.Retryable = true
	default:
		result.Class = provider.ErrorBadRequest
	}
	if message != "" {
		result.Message = fmt.Sprintf("provider error (%d): %s", status, message)
	}
	return result
}

func limitedErrorMessage(reader io.Reader) string {
	payload, err := io.ReadAll(io.LimitReader(reader, 4096))
	if err != nil {
		return ""
	}
	var envelope openaiapi.ErrorEnvelope
	if json.Unmarshal(payload, &envelope) == nil && envelope.Error.Message != "" {
		return envelope.Error.Message
	}
	return http.StatusText(http.StatusBadGateway)
}
