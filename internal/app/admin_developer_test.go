package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akz142857/Heimdall/internal/config"
	"github.com/akz142857/Heimdall/internal/gatewayapi"
	"github.com/akz142857/Heimdall/internal/openaiapi"
	"github.com/akz142857/Heimdall/internal/requestmeta"
	"github.com/go-chi/chi/v5"
)

type developerGatewayService struct {
	key              string
	requestID        string
	inheritedAdmin   bool
	streamingInvoked bool
}

type developerAdminContextKey struct{}

func (s *developerGatewayService) capture(ctx context.Context, key string) {
	s.key = key
	s.requestID, _ = requestmeta.RequestID(ctx)
	s.inheritedAdmin = ctx.Value(developerAdminContextKey{}) != nil
}

func (s *developerGatewayService) Chat(ctx context.Context, key string, request openaiapi.ChatCompletionRequest) (openaiapi.ChatCompletionResponse, error) {
	s.capture(ctx, key)
	return openaiapi.ChatCompletionResponse{ID: "chatcmpl_debug", Object: "chat.completion", Model: request.Model}, nil
}

func (s *developerGatewayService) ChatStream(ctx context.Context, key string, request openaiapi.ChatCompletionRequest, emit func(openaiapi.ChatCompletionResponse) error) error {
	s.capture(ctx, key)
	s.streamingInvoked = true
	return emit(openaiapi.ChatCompletionResponse{ID: "chatcmpl_debug", Object: "chat.completion.chunk", Model: request.Model})
}

func (s *developerGatewayService) Embeddings(ctx context.Context, key string, request openaiapi.EmbeddingRequest) (openaiapi.EmbeddingResponse, error) {
	s.capture(ctx, key)
	return openaiapi.EmbeddingResponse{Object: "list", Model: request.Model, Data: []openaiapi.EmbeddingData{}}, nil
}

func TestDeveloperGatewayBaseURLUsesGatewayListenerNotAdminOrigin(t *testing.T) {
	runtime := &Runtime{config: config.Config{Server: config.Server{GatewayListen: "127.0.0.1:8080", AdminListen: "127.0.0.1:8081"}}}
	request := httptest.NewRequest("GET", "http://gateway.example:8081/admin/api/v1/developer/config", nil)
	if got := runtime.developerGatewayBaseURL(request); got != "http://gateway.example:8080" {
		t.Fatalf("gateway base URL=%q", got)
	}
	runtime.config.TLS.Enabled = true
	if got := runtime.developerGatewayBaseURL(request); got != "https://gateway.example:8080" {
		t.Fatalf("TLS gateway base URL=%q", got)
	}
}

func TestAdminDeveloperExecutionUsesGatewayHandlerAndCorrelatedRequestID(t *testing.T) {
	service := &developerGatewayService{}
	handler, err := gatewayapi.New(service, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{gateway: handler}
	request := developerExecutionRequest(t, "chat-completions", `{"model":"chat","messages":[{"role":"user","content":"hello"}]}`)
	request.Header.Set("Authorization", "Bearer gw_debug")
	response := httptest.NewRecorder()
	runtime.executeAdminDeveloperRequest(response, request)
	if response.Code != http.StatusOK || service.key != "gw_debug" || service.requestID == "" || service.inheritedAdmin {
		t.Fatalf("status=%d service=%#v body=%s", response.Code, service, response.Body.String())
	}
	if got := response.Header().Get("X-Request-ID"); got != service.requestID {
		t.Fatalf("response request ID=%q accounting request ID=%q", got, service.requestID)
	}
}

func TestAdminDeveloperExecutionStreamsAndRejectsUnsupportedEndpoint(t *testing.T) {
	service := &developerGatewayService{}
	handler, err := gatewayapi.New(service, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{gateway: handler}
	request := developerExecutionRequest(t, "chat-completions", `{"model":"chat","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	request.Header.Set("Authorization", "Bearer gw_debug")
	request.Header.Set("Accept", "text/event-stream")
	response := httptest.NewRecorder()
	runtime.executeAdminDeveloperRequest(response, request)
	if response.Code != http.StatusOK || !service.streamingInvoked || !strings.Contains(response.Body.String(), "data:") ||
		!strings.HasPrefix(response.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("unexpected stream response: status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}

	unsupported := developerExecutionRequest(t, "arbitrary-url", `{}`)
	unsupportedResponse := httptest.NewRecorder()
	runtime.executeAdminDeveloperRequest(unsupportedResponse, unsupported)
	if unsupportedResponse.Code != http.StatusNotFound {
		t.Fatalf("unsupported endpoint status=%d", unsupportedResponse.Code)
	}
}

func TestAdminDeveloperExecutionRequiresAdminCSRF(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte("correct horse battery staple")); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	cookie, csrf := loginAdminForTest(t, runtime)

	withoutCSRF := adminRequest(t, http.MethodPost, "/admin/api/v1/developer/execute/chat-completions", map[string]any{})
	withoutCSRF.AddCookie(cookie)
	rejected := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(rejected, withoutCSRF)
	if rejected.Code != http.StatusForbidden {
		t.Fatalf("developer execution without CSRF status=%d", rejected.Code)
	}

	withCSRF := adminRequest(t, http.MethodPost, "/admin/api/v1/developer/execute/chat-completions", map[string]any{})
	withCSRF.AddCookie(cookie)
	withCSRF.Header.Set("X-CSRF-Token", csrf)
	forwarded := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(forwarded, withCSRF)
	if forwarded.Code != http.StatusUnauthorized {
		t.Fatalf("CSRF-authorized request did not reach Gateway: status=%d body=%s", forwarded.Code, forwarded.Body.String())
	}
}

func developerExecutionRequest(t *testing.T, endpoint, body string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/admin/api/v1/developer/execute/"+endpoint, strings.NewReader(body))
	request = request.WithContext(context.WithValue(request.Context(), developerAdminContextKey{}, "must not cross boundary"))
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("endpoint", endpoint)
	return request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
}
