package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/gatewayapi"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/requestmeta"
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

// The console builds request bodies that can carry a base64 image, so it has to know
// the size the Gateway will actually measure them against rather than guess one.
func TestDeveloperConfigReportsTheServerRequestLimit(t *testing.T) {
	runtime := &Runtime{config: config.Config{Server: config.Server{
		GatewayListen: "127.0.0.1:8080", AdminListen: "127.0.0.1:8081", MaxRequestBytes: 10 << 20,
	}}}
	response := httptest.NewRecorder()
	runtime.getAdminDeveloperConfig(response, httptest.NewRequest("GET", "/admin/api/v1/developer/config", nil))
	if !strings.Contains(response.Body.String(), `"max_request_bytes":10485760`) {
		t.Fatalf("config did not report the request limit: %s", response.Body.String())
	}
}

func TestDeveloperGatewayBaseURLUsesGatewayListenerNotAdminOrigin(t *testing.T) {
	// A loopback listener is unreachable under any other host, so the admin origin must not
	// replace it: the integrating application runs alongside the Gateway by definition.
	runtime := &Runtime{config: config.Config{Server: config.Server{GatewayListen: "127.0.0.1:8080", AdminListen: "127.0.0.1:8081"}}}
	request := httptest.NewRequest("GET", "http://gateway.example:8081/admin/api/v1/developer/config", nil)
	if got := runtime.developerGatewayBaseURL(request); got != "http://127.0.0.1:8080" {
		t.Fatalf("gateway base URL=%q", got)
	}
	runtime.config.TLS.Enabled = true
	if got := runtime.developerGatewayBaseURL(request); got != "https://127.0.0.1:8080" {
		t.Fatalf("TLS gateway base URL=%q", got)
	}

	// An unspecified listener answers on every interface, so the admin host is the best
	// address to hand to the caller.
	runtime.config.TLS.Enabled = false
	runtime.config.Server.GatewayListen = "0.0.0.0:8080"
	if got := runtime.developerGatewayBaseURL(request); got != "http://gateway.example:8080" {
		t.Fatalf("unspecified gateway base URL=%q", got)
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

// Executing through the workbench spends real money against a real project. It has to be
// attributable to the admin who triggered it, like every other admin mutation.
func TestAdminDeveloperExecutionIsAudited(t *testing.T) {
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

	execute := adminRequest(t, http.MethodPost, "/admin/api/v1/developer/execute/chat-completions", map[string]any{
		"model": "support-chat", "messages": []map[string]string{{"role": "user", "content": "ping"}},
	})
	execute.AddCookie(cookie)
	execute.Header.Set("X-CSRF-Token", csrf)
	executed := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(executed, execute)

	audits := adminRequest(t, http.MethodGet, "/admin/api/v1/audit", nil)
	audits.AddCookie(cookie)
	auditResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(auditResponse, audits)
	if auditResponse.Code != http.StatusOK {
		t.Fatalf("audit list status=%d body=%s", auditResponse.Code, auditResponse.Body.String())
	}
	// The audit list is served flat: the console reads these fields directly.
	var page struct {
		Items []struct {
			ActorType string         `json:"actor_type"`
			ActorID   string         `json:"actor_id"`
			Action    string         `json:"action"`
			TargetID  string         `json:"target_id"`
			Metadata  map[string]any `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(auditResponse.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	for _, event := range page.Items {
		if event.Action != "developer.execute" {
			continue
		}
		if event.ActorType != "admin_user" || event.ActorID != "admin" {
			t.Fatalf("developer.execute actor=%s/%s", event.ActorType, event.ActorID)
		}
		if event.TargetID != "chat-completions" {
			t.Fatalf("developer.execute target=%s", event.TargetID)
		}
		if _, recorded := event.Metadata["http_status"]; !recorded {
			t.Fatalf("developer.execute metadata missing http_status: %v", event.Metadata)
		}
		return
	}
	t.Fatalf("no developer.execute audit event recorded in %d events", len(page.Items))
}

// Enabling the workbench makes the admin listener carry data-plane traffic. Deployments
// that isolate the Gateway listener at the network layer must be able to refuse it.
func TestAdminDeveloperExecutionRespectsDisabledWorkbench(t *testing.T) {
	service := &developerGatewayService{}
	handler, err := gatewayapi.New(service, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{gateway: handler, config: config.Config{Admin: config.Admin{DeveloperWorkbench: "disabled"}}}
	request := developerExecutionRequest(t, "chat-completions", `{"model":"chat","messages":[]}`)
	request.Header.Set("Authorization", "Bearer gw_debug")
	response := httptest.NewRecorder()

	runtime.executeAdminDeveloperRequest(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("disabled workbench status=%d body=%s", response.Code, response.Body.String())
	}
	if service.key != "" {
		t.Fatalf("disabled workbench still reached the Gateway with key=%q", service.key)
	}

	configResponse := httptest.NewRecorder()
	runtime.getAdminDeveloperConfig(configResponse, httptest.NewRequest("GET", "/admin/api/v1/developer/config", nil))
	if !strings.Contains(configResponse.Body.String(), `"enabled":false`) {
		t.Fatalf("config did not advertise the disabled workbench: %s", configResponse.Body.String())
	}
}

// A workbench on a loopback-only listener is the quickstart, and the first-run
// guidance sends people to it. One bound to a routable address is a data-plane
// entrance that the Gateway listener's network controls do not cover, and the
// operator has to be told so at startup rather than discover it in a review.
func TestWorkbenchWarnsOnlyWhenTheAdminListenerIsReachable(t *testing.T) {
	warned := func(listen, workbench string) bool {
		var buffer strings.Builder
		runtime := &Runtime{
			logger: slog.New(slog.NewTextHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelWarn})),
			config: config.Config{
				Server: config.Server{AdminListen: listen},
				Admin:  config.Admin{DeveloperWorkbench: workbench},
			},
		}
		runtime.warnAboutReachableWorkbench()
		return strings.Contains(buffer.String(), "developer workbench")
	}
	if warned("127.0.0.1:8081", "enabled") {
		t.Fatal("a loopback admin listener does not expose the workbench to anyone else")
	}
	if warned("0.0.0.0:8081", "disabled") {
		t.Fatal("warned about a workbench that is switched off")
	}
	if !warned("0.0.0.0:8081", "enabled") {
		t.Fatal("a workbench on a routable admin listener was not reported")
	}
	if !warned("[::]:8081", "enabled") {
		t.Fatal("an unspecified IPv6 admin listener was not reported")
	}
}
