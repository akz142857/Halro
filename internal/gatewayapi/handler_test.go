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

	"github.com/akz142857/Heimdall/internal/gateway"
	"github.com/akz142857/Heimdall/internal/openaiapi"
)

type fakeService struct {
	key      string
	request  openaiapi.ChatCompletionRequest
	response openaiapi.ChatCompletionResponse
	err      error
	calls    int
}

func (s *fakeService) Chat(
	_ context.Context,
	key string,
	request openaiapi.ChatCompletionRequest,
) (openaiapi.ChatCompletionResponse, error) {
	s.calls++
	s.key = key
	s.request = request
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
