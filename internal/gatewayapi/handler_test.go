package gatewayapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/anthropicapi"
	"github.com/akz142857/Halro/internal/gateway"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/requestmeta"
)

type fakeService struct {
	key       string
	request   openaiapi.ChatCompletionRequest
	response  openaiapi.ChatCompletionResponse
	err       error
	calls     int
	requestID string
}

func (s *fakeService) Messages(_ context.Context, key string, request anthropicapi.MessageRequest) (anthropicapi.Message, error) {
	s.calls++
	s.key = key
	stop := "end_turn"
	return anthropicapi.Message{ID: "msg_1", Type: "message", Role: "assistant", Model: request.Model, Content: anthropicapi.ContentBlocks{{Type: "text", Text: "hello"}}, StopReason: &stop, Usage: anthropicapi.Usage{InputTokens: 1, OutputTokens: 1}}, s.err
}

func (s *fakeService) MessagesStream(_ context.Context, key string, request anthropicapi.MessageRequest, emit func(anthropicapi.StreamEvent) error) error {
	s.calls++
	s.key = key
	if s.err != nil {
		return s.err
	}
	message := anthropicapi.Message{ID: "msg_1", Type: "message", Role: "assistant", Model: request.Model, Content: anthropicapi.ContentBlocks{}, Usage: anthropicapi.Usage{}}
	if err := emit(anthropicapi.StreamEvent{Type: "message_start", Message: &message}); err != nil {
		return err
	}
	return emit(anthropicapi.StreamEvent{Type: "message_stop"})
}

// The handler discovers Messages support with a comma-ok assertion, so a fake
// that drifts from the interface silently answers 501 instead of failing the
// build. Pin the fake the same way the production service is pinned.
var _ MessagesService = (*fakeService)(nil)

func (s *fakeService) MessagesNative(ctx context.Context, key, _ string, _ []string, request anthropicapi.MessageRequest) (anthropicapi.Message, error) {
	return s.Messages(ctx, key, request)
}

func (s *fakeService) MessagesCountTokens(_ context.Context, key, _ string, _ []string, _ anthropicapi.MessageRequest) (anthropicapi.TokenCount, error) {
	s.calls++
	s.key = key
	if s.err != nil {
		return anthropicapi.TokenCount{}, s.err
	}
	return anthropicapi.TokenCount{InputTokens: 11}, nil
}

func (s *fakeService) MessagesNativeStream(_ context.Context, key, _ string, _ []string, request anthropicapi.MessageRequest, emit func(anthropicapi.RawStreamEvent) error) error {
	s.calls++
	s.key = key
	if s.err != nil {
		return s.err
	}
	for _, event := range []anthropicapi.RawStreamEvent{{Type: "message_start", Data: json.RawMessage(`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"` + request.Model + `","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":0}}}`)}, {Type: "message_delta", Data: json.RawMessage(`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`)}, {Type: "message_stop", Data: json.RawMessage(`{"type":"message_stop"}`)}} {
		if err := emit(event); err != nil {
			return err
		}
	}
	return nil
}

func (s *fakeService) Responses(_ context.Context, key string, request openaiapi.ResponseRequest) (openaiapi.Response, error) {
	s.calls++
	s.key = key
	return openaiapi.Response{ID: "resp_1", Object: "response", Status: "completed", Model: request.Model, Output: []openaiapi.ResponseOutputItem{}, Tools: []openaiapi.ResponseTool{}, Metadata: map[string]string{}}, s.err
}

func (s *fakeService) ResponsesStream(_ context.Context, key string, request openaiapi.ResponseRequest, emit func(openaiapi.ResponseStreamEvent) error) error {
	s.calls++
	s.key = key
	if s.err != nil {
		return s.err
	}
	response := openaiapi.Response{ID: "resp_1", Object: "response", Status: "completed", Model: request.Model, Output: []openaiapi.ResponseOutputItem{}, Tools: []openaiapi.ResponseTool{}, Metadata: map[string]string{}}
	if err := emit(openaiapi.ResponseStreamEvent{Type: "response.created", SequenceNumber: 0, Response: &response}); err != nil {
		return err
	}
	return emit(openaiapi.ResponseStreamEvent{Type: "response.completed", SequenceNumber: 1, Response: &response})
}

func (s *fakeService) Chat(
	ctx context.Context,
	key string,
	request openaiapi.ChatCompletionRequest,
) (openaiapi.ChatCompletionResponse, error) {
	s.calls++
	s.key = key
	s.request = request
	s.requestID, _ = requestmeta.RequestID(ctx)
	return s.response, s.err
}

func (s *fakeService) ChatStream(
	_ context.Context,
	key string,
	request openaiapi.ChatCompletionRequest,
	emit func(openaiapi.ChatCompletionResponse) error,
) error {
	s.calls++
	s.key = key
	if s.err != nil {
		return s.err
	}
	return emit(openaiapi.ChatCompletionResponse{
		ID: "chunk_1", Object: "chat.completion.chunk", Model: request.Model,
		Choices: []openaiapi.Choice{{Index: 0, Delta: &openaiapi.Message{
			Role: "assistant", Content: openaiapi.TextContent("hello"),
		}}},
	})
}

func (s *fakeService) Embeddings(
	_ context.Context,
	key string,
	request openaiapi.EmbeddingRequest,
) (openaiapi.EmbeddingResponse, error) {
	s.calls++
	s.key = key
	return openaiapi.EmbeddingResponse{
		Object: "list",
		Model:  request.Model,
		Data:   []openaiapi.EmbeddingData{{Object: "embedding", Embedding: json.RawMessage(`[0.1]`)}},
	}, s.err
}

func TestChatCompletionsContract(t *testing.T) {
	service := &fakeService{response: openaiapi.ChatCompletionResponse{
		ID:      "chatcmpl_1",
		Object:  "chat.completion",
		Model:   "chat",
		Choices: []openaiapi.Choice{{Index: 0}},
	}}
	handler, err := New(service, 1024)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"hello"}]}`),
	)
	request.Header.Set("Authorization", "Bearer gw_test")
	response := httptest.NewRecorder()
	handler.ChatCompletions(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if service.key != "gw_test" || service.request.Model != "chat" {
		t.Fatalf("request was not forwarded: %#v", service)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("sensitive response was cacheable")
	}
	if response.Header().Get("X-Request-ID") == "" || response.Header().Get("X-Request-ID") != service.requestID {
		t.Fatalf("response request ID=%q service request ID=%q", response.Header().Get("X-Request-ID"), service.requestID)
	}
}

func TestResponsesContractAndTypedSSE(t *testing.T) {
	service := &fakeService{}
	handler, err := New(service, 1024)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"chat","input":"hello","store":false}`))
	request.Header.Set("Authorization", "Bearer gw_test")
	response := httptest.NewRecorder()
	handler.Responses(response, request)
	if response.Code != http.StatusOK || service.calls != 1 || service.key != "gw_test" {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, service.calls, response.Body.String())
	}

	streamRequest := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"chat","input":"hello","stream":true,"store":false}`))
	streamRequest.Header.Set("Authorization", "Bearer gw_test")
	streamResponse := httptest.NewRecorder()
	handler.Responses(streamResponse, streamRequest)
	if streamResponse.Code != http.StatusOK || !strings.Contains(streamResponse.Body.String(), "event: response.created") || !strings.Contains(streamResponse.Body.String(), "event: response.completed") || strings.Contains(streamResponse.Body.String(), "[DONE]") {
		t.Fatalf("unexpected Responses SSE: status=%d body=%s", streamResponse.Code, streamResponse.Body.String())
	}
}

func TestAnthropicMessagesHeadersErrorsAndSSE(t *testing.T) {
	service := &fakeService{}
	handler, err := New(service, 2048)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"chat","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`

	missingVersion := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	missingVersion.Header.Set("x-api-key", "gw_test")
	missingResponse := httptest.NewRecorder()
	handler.Messages(missingResponse, missingVersion)
	if missingResponse.Code != http.StatusBadRequest || !strings.Contains(missingResponse.Body.String(), `"type":"invalid_request_error"`) || missingResponse.Header().Get("request-id") == "" {
		t.Fatalf("unexpected missing-version response: %#v %s", missingResponse.Result().Header, missingResponse.Body.String())
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("x-api-key", "gw_test")
	request.Header.Set("anthropic-version", anthropicapi.SupportedVersion)
	response := httptest.NewRecorder()
	handler.Messages(response, request)
	if response.Code != http.StatusOK || service.key != "gw_test" || !strings.Contains(response.Body.String(), `"type":"message"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}

	streamRequest := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"chat","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	streamRequest.Header.Set("x-api-key", "gw_test")
	streamRequest.Header.Set("anthropic-version", anthropicapi.SupportedVersion)
	streamResponse := httptest.NewRecorder()
	handler.Messages(streamResponse, streamRequest)
	if streamResponse.Code != http.StatusOK || !strings.Contains(streamResponse.Body.String(), "event: message_start") || !strings.Contains(streamResponse.Body.String(), "event: message_stop") || strings.Contains(streamResponse.Body.String(), "[DONE]") {
		t.Fatalf("unexpected Messages SSE: %d %s", streamResponse.Code, streamResponse.Body.String())
	}
}

func TestAnthropicMessagesRejectsBetaAndAmbiguousCredentials(t *testing.T) {
	service := &fakeService{}
	handler, _ := New(service, 2048)
	body := `{"model":"chat","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`
	for name, mutate := range map[string]func(http.Header){
		"beta":          func(header http.Header) { header.Set("x-api-key", "gw_test"); header.Set("anthropic-beta", "feature") },
		"ambiguous key": func(header http.Header) { header.Set("x-api-key", "one"); header.Set("Authorization", "Bearer two") },
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
			request.Header.Set("anthropic-version", anthropicapi.SupportedVersion)
			mutate(request.Header)
			response := httptest.NewRecorder()
			handler.Messages(response, request)
			if response.Code < 400 {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

// The Anthropic body has no `param`, so the field a refusal named travels in
// the sentence — the shape upstream Anthropic itself writes, and the shape this
// surface's own decoder rejections already arrive in. Before this, a provider
// refusal that named a field reached an Anthropic-protocol caller with the
// field dropped, while the same refusal on the OpenAI surface named it.
func TestAnthropicRefusalNamesTheFieldInTheMessage(t *testing.T) {
	handler, _ := New(&fakeService{}, 2048)
	response := httptest.NewRecorder()
	handler.renderAnthropicGatewayError(response, &gateway.Error{
		Code: "invalid_request_error", Message: "provider rejected the request",
		HTTPStatus: http.StatusBadRequest, Param: "messages[0].content[1].image_url",
	}, "req_test")
	var envelope anthropicapi.ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.Code != http.StatusBadRequest ||
		envelope.Error.Message != "messages[0].content[1].image_url: provider rejected the request" {
		t.Fatalf("status=%d message=%q", response.Code, envelope.Error.Message)
	}
}

// A failure about the request as a whole is not prefixed with an empty field
// name — the sentence is the whole message.
func TestAnthropicErrorWithoutAFieldIsNotPrefixed(t *testing.T) {
	handler, _ := New(&fakeService{}, 2048)
	response := httptest.NewRecorder()
	handler.renderAnthropicGatewayError(response, &gateway.Error{
		Code: "budget_exceeded", Message: "daily budget exceeded", HTTPStatus: http.StatusForbidden,
	}, "req_test")
	var envelope anthropicapi.ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.Error.Message != "daily budget exceeded" {
		t.Fatalf("message=%q", envelope.Error.Message)
	}
}

func TestAnthropicOverloadedErrorPreservesProtocolStatus(t *testing.T) {
	handler, _ := New(&fakeService{}, 2048)
	response := httptest.NewRecorder()
	handler.renderAnthropicGatewayError(response, &gateway.Error{
		Code: "provider_error", Message: "provider request failed", HTTPStatus: http.StatusBadGateway,
		Cause: &provider.Error{Class: provider.ErrorProvider5xx, StatusCode: 529, ProviderCode: "overloaded_error"},
	}, "req_test")
	if response.Code != 529 || !strings.Contains(response.Body.String(), `"type":"overloaded_error"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

// The status a client is answered with is Halro's to decide, and the only
// evidence of an overload it accepts is the status line the upstream sent.
// An `error.code` is a string the upstream wrote into a body, and one path
// carries it with no status behind it at all: a Mantle Responses stream error
// event fills ProviderCode straight from the event. Read as an overload marker,
// it turned a 502 into a 529 that no upstream ever sent — and 529 is the status
// SDKs treat as "back off and retry", so the upstream would have been choosing
// the client's retry behaviour.
func TestAnthropicOverloadIsReadFromTheStatusNotTheUpstreamsWord(t *testing.T) {
	handler, _ := New(&fakeService{}, 2048)
	response := httptest.NewRecorder()
	handler.renderAnthropicGatewayError(response, &gateway.Error{
		Code: "provider_error", Message: "provider request failed", HTTPStatus: http.StatusBadGateway,
		Cause: &provider.Error{Class: provider.ErrorProvider5xx, ProviderCode: "overloaded_error"},
	}, "req_test")
	if response.Code != http.StatusBadGateway {
		t.Fatalf("an upstream-chosen code set the response status: status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"type":"api_error"`) {
		t.Fatalf("the 502 was not reported as an api_error: %s", response.Body.String())
	}
}

func TestAnthropicNativeModeUsesNativeServiceAndRawSSE(t *testing.T) {
	service := &fakeService{}
	handler, _ := New(service, 2048)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"chat","max_tokens":32,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":"sig"}]},{"role":"user","content":"continue"}]}`))
	request.Header.Set("x-api-key", "gw_test")
	request.Header.Set("anthropic-version", anthropicapi.SupportedVersion)
	request.Header.Set(anthropicapi.RouteModeHeader, "native")
	response := httptest.NewRecorder()
	handler.Messages(response, request)
	if response.Code != http.StatusOK || service.key != "gw_test" {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	stream := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"chat","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"continue"}]}`))
	stream.Header = request.Header.Clone()
	streamResponse := httptest.NewRecorder()
	handler.Messages(streamResponse, stream)
	if streamResponse.Code != http.StatusOK || !strings.Contains(streamResponse.Body.String(), `event: message_start`) || !strings.Contains(streamResponse.Body.String(), `"type":"message_stop"`) || strings.Contains(streamResponse.Body.String(), "[DONE]") {
		t.Fatalf("unexpected native SSE: %d %s", streamResponse.Code, streamResponse.Body.String())
	}
}

func TestResponsesRejectsStateBeforeServiceInvocation(t *testing.T) {
	service := &fakeService{}
	handler, _ := New(service, 1024)
	for _, body := range []string{
		`{"model":"chat","input":"hello","store":true}`,
		`{"model":"chat","input":"hello","previous_response_id":"resp_1"}`,
		`{"model":"chat","input":"hello","background":true}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer gw_test")
		response := httptest.NewRecorder()
		handler.Responses(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("body=%s status=%d response=%s", body, response.Code, response.Body.String())
		}
	}
	if service.calls != 0 {
		t.Fatalf("unsafe requests reached service: calls=%d", service.calls)
	}
}

func TestMissingAuthorizationUsesOpenAIErrorEnvelope(t *testing.T) {
	service := &fakeService{}
	handler, _ := New(service, 1024)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	response := httptest.NewRecorder()
	handler.ChatCompletions(response, request)
	if response.Code != http.StatusUnauthorized || service.calls != 0 {
		t.Fatalf("status=%d calls=%d", response.Code, service.calls)
	}
	var envelope openaiapi.ErrorEnvelope
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "invalid_api_key" || envelope.Error.Type != "authentication_error" {
		t.Fatalf("unexpected error: %#v", envelope)
	}
}

func TestStrictJSONAndBodyLimit(t *testing.T) {
	service := &fakeService{}
	handler, _ := New(service, 80)
	for name, body := range map[string]string{
		"unknown field": `{"model":"chat","messages":[{"role":"user","content":"hello"}],"unknown":true}`,
		"too large":     `{"model":"chat","messages":[{"role":"user","content":"` + strings.Repeat("x", 100) + `"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer gw_test")
			response := httptest.NewRecorder()
			handler.ChatCompletions(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	if service.calls != 0 {
		t.Fatal("invalid bodies reached the service")
	}
}

func TestGatewayErrorIsMappedWithoutLeakingCause(t *testing.T) {
	service := &fakeService{err: &gateway.Error{
		Code:       "budget_exceeded",
		Message:    "daily budget exceeded",
		HTTPStatus: http.StatusForbidden,
		Cause:      errors.New("sensitive internal detail"),
	}}
	handler, _ := New(service, 1024)
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"hello"}]}`),
	)
	request.Header.Set("Authorization", "Bearer gw_test")
	response := httptest.NewRecorder()
	handler.ChatCompletions(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status=%d", response.Code)
	}
	if strings.Contains(response.Body.String(), "sensitive") {
		t.Fatal("internal cause leaked to the client")
	}
}

// The field an upstream refusal named has to survive the HTTP boundary, in the
// same `param` slot the decoder rejections already use. Every gateway failure
// reached this boundary through a call that passed nil, so `param` was null
// even when the gateway knew exactly which field was refused.
func TestProviderRefusalRendersTheRefusedField(t *testing.T) {
	service := &fakeService{err: &gateway.Error{
		Code: "invalid_request_error", Message: "provider rejected the request",
		HTTPStatus: http.StatusBadRequest, Param: "messages[0].content[1].image_url",
	}}
	handler, _ := New(service, 1024)
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"hello"}]}`),
	)
	request.Header.Set("Authorization", "Bearer gw_test")
	response := httptest.NewRecorder()
	handler.ChatCompletions(response, request)
	var envelope openaiapi.ErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.Code != http.StatusBadRequest ||
		envelope.Error.Param == nil || *envelope.Error.Param != "messages[0].content[1].image_url" {
		t.Fatalf("status=%d param=%v", response.Code, envelope.Error.Param)
	}
}

// A failure that is about the request as a whole leaves `param` null rather than
// pointing the caller at an empty field name.
func TestGatewayErrorWithoutAFieldLeavesParamNull(t *testing.T) {
	service := &fakeService{err: &gateway.Error{
		Code: "budget_exceeded", Message: "daily budget exceeded", HTTPStatus: http.StatusForbidden,
	}}
	handler, _ := New(service, 1024)
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"hello"}]}`),
	)
	request.Header.Set("Authorization", "Bearer gw_test")
	response := httptest.NewRecorder()
	handler.ChatCompletions(response, request)
	var envelope openaiapi.ErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.Error.Param != nil {
		t.Fatalf("param=%q on a failure that named no field", *envelope.Error.Param)
	}
}

func TestRateLimitErrorSetsRoundedRetryAfter(t *testing.T) {
	service := &fakeService{err: &gateway.Error{
		Code: "rate_limit_exceeded", Message: "project RPM limit exceeded",
		HTTPStatus: http.StatusTooManyRequests, RetryAfter: 1500 * time.Millisecond,
	}}
	handler, _ := New(service, 1024)
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"hello"}]}`),
	)
	request.Header.Set("Authorization", "Bearer gw_test")
	response := httptest.NewRecorder()
	handler.ChatCompletions(response, request)
	if response.Code != http.StatusTooManyRequests ||
		response.Header().Get("Retry-After") != "2" {
		t.Fatalf("status=%d retry-after=%q", response.Code, response.Header().Get("Retry-After"))
	}
}

func TestEmbeddingsContract(t *testing.T) {
	service := &fakeService{}
	handler, _ := New(service, 1024)
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/embeddings",
		strings.NewReader(`{"model":"embedding","input":["hello"]}`),
	)
	request.Header.Set("Authorization", "Bearer gw_test")
	response := httptest.NewRecorder()
	handler.Embeddings(response, request)
	if response.Code != http.StatusOK || service.key != "gw_test" {
		t.Fatalf("status=%d key=%q body=%s", response.Code, service.key, response.Body.String())
	}
}

func TestStreamingContractEmitsSSEAndDone(t *testing.T) {
	service := &fakeService{}
	handler, _ := New(service, 1024)
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"chat","stream":true,"messages":[{"role":"user","content":"hello"}]}`),
	)
	request.Header.Set("Authorization", "Bearer gw_test")
	response := httptest.NewRecorder()
	handler.ChatCompletions(response, request)
	if response.Code != http.StatusOK ||
		!strings.HasPrefix(response.Header().Get("Content-Type"), "text/event-stream") ||
		!strings.Contains(response.Body.String(), `"object":"chat.completion.chunk"`) ||
		!strings.HasSuffix(response.Body.String(), "data: [DONE]\n\n") {
		t.Fatalf("unexpected stream: status=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
	}
}

func TestStreamingPrePayloadErrorRemainsJSON(t *testing.T) {
	service := &fakeService{err: &gateway.Error{
		Code: "budget_exceeded", Message: "daily budget exceeded", HTTPStatus: http.StatusForbidden,
	}}
	handler, _ := New(service, 1024)
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"chat","stream":true,"messages":[{"role":"user","content":"hello"}]}`),
	)
	request.Header.Set("Authorization", "Bearer gw_test")
	response := httptest.NewRecorder()
	handler.ChatCompletions(response, request)
	if response.Code != http.StatusForbidden ||
		!strings.HasPrefix(response.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("unexpected response: status=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
	}
}

func TestStreamingPostPayloadErrorHasNoDoneSentinel(t *testing.T) {
	service := &fakeService{}
	handler, _ := New(service, 1024)
	service.err = errors.New("failure after payload")
	service.response = openaiapi.ChatCompletionResponse{}
	// This service normally returns before emitting when err is set, so use a
	// wrapper that emits once and then fails.
	streaming := &postPayloadFailureService{fakeService: service}
	handler, _ = New(streaming, 1024)
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"chat","stream":true,"messages":[{"role":"user","content":"hello"}]}`),
	)
	request.Header.Set("Authorization", "Bearer gw_test")
	response := httptest.NewRecorder()
	handler.ChatCompletions(response, request)
	if !strings.Contains(response.Body.String(), "event: error") ||
		strings.Contains(response.Body.String(), "[DONE]") {
		t.Fatalf("unexpected abnormal stream: %q", response.Body.String())
	}
}

func TestSourceIPTrustsForwardedChainOnlyFromConfiguredProxy(t *testing.T) {
	service := &fakeService{}
	handler, err := NewWithOptions(service, Options{
		TrustedProxyCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		TrustProxyHeaders: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	request.RemoteAddr = "10.0.0.1:1234"
	request.Header.Set("X-Forwarded-For", "198.51.100.9, 10.0.0.2")
	if got, ok := handler.sourceIP(request); !ok || got != netip.MustParseAddr("198.51.100.9") {
		t.Fatalf("source=%s", got)
	}
	request.RemoteAddr = "203.0.113.7:1234"
	request.Header.Set("X-Forwarded-For", "198.51.100.10")
	if got, ok := handler.sourceIP(request); !ok || got != netip.MustParseAddr("203.0.113.7") {
		t.Fatalf("spoofed forwarded header was trusted: %s", got)
	}

	// The client sent its own header line; the proxy appended the address it
	// actually saw as a second line. Both lines are one chain, so the truth the
	// proxy added is what the rightmost-untrusted walk has to find.
	request.RemoteAddr = "10.0.0.1:1234"
	request.Header.Del("X-Forwarded-For")
	request.Header.Add("X-Forwarded-For", "203.0.113.9")
	request.Header.Add("X-Forwarded-For", "198.51.100.7")
	if got, ok := handler.sourceIP(request); !ok || got != netip.MustParseAddr("198.51.100.7") {
		t.Fatalf("a second forwarded header line was ignored: source=%s", got)
	}
}

func TestTrustedProxyRejectsMalformedForwardedFor(t *testing.T) {
	service := &fakeService{}
	handler, err := NewWithOptions(service, Options{
		TrustedProxyCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		TrustProxyHeaders: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	request.RemoteAddr = "10.0.0.1:1234"
	request.Header.Set("X-Forwarded-For", "not-an-ip")
	response := httptest.NewRecorder()
	handler.ChatCompletions(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_forwarded_for") {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

type postPayloadFailureService struct {
	*fakeService
}

func (s *postPayloadFailureService) ChatStream(
	_ context.Context,
	_ string,
	request openaiapi.ChatCompletionRequest,
	emit func(openaiapi.ChatCompletionResponse) error,
) error {
	if err := emit(openaiapi.ChatCompletionResponse{
		ID: "chunk_1", Object: "chat.completion.chunk", Model: request.Model,
		Choices: []openaiapi.Choice{{Index: 0, Delta: &openaiapi.Message{
			Role: "assistant", Content: openaiapi.TextContent("partial"),
		}}},
	}); err != nil {
		return err
	}
	return errors.New("upstream failed")
}

// deadlineRecorder is an httptest.ResponseRecorder that also answers the
// SetWriteDeadline half of http.ResponseController, which the recorder alone
// does not implement.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
	flushes   int
}

func (r *deadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	r.deadlines = append(r.deadlines, deadline)
	return nil
}

func (r *deadlineRecorder) Flush() { r.flushes++; r.ResponseRecorder.Flush() }

func TestNonStreamingResponsesArmAWriteDeadlineWhenTheyStart(t *testing.T) {
	service := &fakeService{}
	handler, _ := NewWithOptions(service, Options{MaxRequestBytes: 1024, WriteTimeout: 3 * time.Second})
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/embeddings",
		strings.NewReader(`{"model":"embedding","input":["hello"]}`),
	)
	request.Header.Set("Authorization", "Bearer gw_test")
	response := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	before := time.Now()
	handler.WithWriteDeadline(http.HandlerFunc(handler.Embeddings)).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if len(response.deadlines) != 1 {
		t.Fatalf("expected exactly one armed deadline, got %d", len(response.deadlines))
	}
	// Armed at the response, not at the request: a slow provider must not consume
	// the client's read budget before a single byte has been sent.
	if elapsed := response.deadlines[0].Sub(before); elapsed < 3*time.Second {
		t.Fatalf("deadline %s is shorter than the configured write timeout", elapsed)
	}
}

func TestRejectedRequestsAlsoArmAWriteDeadline(t *testing.T) {
	service := &fakeService{}
	handler, _ := NewWithOptions(service, Options{MaxRequestBytes: 1024, WriteTimeout: 3 * time.Second})
	request := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{`))
	request.Header.Set("Authorization", "Bearer gw_test")
	response := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	handler.WithWriteDeadline(http.HandlerFunc(handler.Embeddings)).ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if len(response.deadlines) != 1 {
		t.Fatalf("an error envelope is a response too: deadlines=%d", len(response.deadlines))
	}
}

func TestStreamingKeepsFlushingAndExtendsThroughTheWrapper(t *testing.T) {
	service := &fakeService{}
	handler, _ := NewWithOptions(service, Options{MaxRequestBytes: 1024, WriteTimeout: 3 * time.Second})
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"chat","stream":true,"messages":[{"role":"user","content":"hello"}]}`),
	)
	request.Header.Set("Authorization", "Bearer gw_test")
	response := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	handler.WithWriteDeadline(http.HandlerFunc(handler.ChatCompletions)).ServeHTTP(response, request)

	if !strings.HasSuffix(response.Body.String(), "data: [DONE]\n\n") {
		t.Fatalf("stream did not complete through the wrapper: %q", response.Body.String())
	}
	if response.flushes == 0 {
		t.Fatal("the wrapper swallowed Flush, so events would sit in the buffer")
	}
	// One per event, so a stalled reader costs a single write rather than the stream.
	if len(response.deadlines) < 2 {
		t.Fatalf("expected the stream to keep extending the deadline, got %d", len(response.deadlines))
	}
}
