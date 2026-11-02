package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	openaiwire "github.com/akz142857/Halro/internal/compatibility/openai"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
)

func TestVolcengineWiringUsesPayAsYouGoRouteAndBearerKey(t *testing.T) {
	endpoint, _ := url.Parse(domain.ResolveBaseURL(domain.ProfileVolcengineChat, ""))
	called := false
	client := &http.Client{Transport: recordingTransport(func(request *http.Request) (*http.Response, error) {
		called = true
		if request.Method != "POST" || request.URL.String() != "https://ark.cn-beijing.volces.com/api/v3/chat/completions" {
			t.Fatalf("unexpected Ark route: %s %s", request.Method, request.URL)
		}
		if request.Header.Get("Authorization") != "Bearer ark-key" || request.Header.Get("api-key") != "" || request.Header.Get("x-api-key") != "" {
			t.Fatal("incorrect credential headers")
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if string(body["thinking"]) != `{"type":"disabled"}` {
			t.Fatalf("missing Ark renderer: %v", body)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"id":"chat_1","object":"chat.completion","created":1,"model":"doubao-test","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))}, nil
	})}
	instance := domain.ProviderInstance{ID: "ark", Name: "Ark", Type: domain.ProviderVolcengine, BaseURL: endpoint.String(), CredentialID: "credential"}
	binding := domain.ProviderProfileBinding{ID: "binding", ProviderID: instance.ID, ProfileID: domain.ProfileVolcengineChat, AccessSurface: domain.SurfaceVolcengine, CredentialScheme: domain.CredentialBearerStatic, Enabled: true, Capabilities: domain.DefaultProviderCapabilities(domain.ProviderVolcengine)}
	adapter, err := newProviderBindingAdapterWithClient(instance, binding, endpoint, []byte("ark-key"), client)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	_, err = adapter.Chat(context.Background(), provider.ChatCall{ProviderModel: "doubao-test", Request: openaiapi.ChatCompletionRequest{Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}}}})
	if err != nil || !called {
		t.Fatalf("called=%t err=%v", called, err)
	}
}

func TestVolcengineCredentialAndConnectionSaveThroughAdminAPI(t *testing.T) {
	runtime, _ := openRuntimeWithPolicyForTest(t, testConfig(t))
	cookie, csrf := loginAdminForTest(t, runtime)
	endpoint := domain.ResolveBaseURL(domain.ProfileVolcengineChat, "")
	response := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
		"name": "Ark", "type": "volcengine", "base_url": endpoint, "secret": "ark-key",
		"access_surface": domain.SurfaceVolcengine, "scheme": domain.CredentialBearerStatic,
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("credential: %d %s", response.Code, response.Body.String())
	}
	var credential struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &credential); err != nil {
		t.Fatal(err)
	}
	response = performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
		"name": "Ark", "type": "volcengine", "base_url": endpoint, "credential_id": credential.ID, "enabled": true,
		"access_surface": domain.SurfaceVolcengine, "profile_id": domain.ProfileVolcengineChat, "credential_scheme": domain.CredentialBearerStatic,
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("connection: %d %s", response.Code, response.Body.String())
	}
}

func TestVolcengineReviewedEndpointMappingReachesTheWire(t *testing.T) {
	endpoint, _ := url.Parse(domain.ResolveBaseURL(domain.ProfileVolcengineChat, ""))
	called := false
	client := &http.Client{Transport: recordingTransport(func(request *http.Request) (*http.Response, error) {
		called = true
		var body struct {
			Model           string `json:"model"`
			ReasoningEffort string `json:"reasoning_effort"`
			Thinking        struct {
				Type string `json:"type"`
			} `json:"thinking"`
		}
		if json.NewDecoder(request.Body).Decode(&body) != nil || body.Model != "ep-reviewed" || body.ReasoningEffort != "high" || body.Thinking.Type != "enabled" {
			t.Fatalf("reviewed model mapping did not preserve wire identity/effort: %+v", body)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"id":"reply","object":"chat.completion","created":1,"model":"actual-model","choices":[{"index":0,"message":{"role":"assistant","content":"OK","reasoning_content":"summary","encrypted_content":"opaque"},"finish_reason":"stop"}]}`))}, nil
	})}
	instance := domain.ProviderInstance{ID: "ark", Type: domain.ProviderVolcengine, BaseURL: endpoint.String()}
	binding := domain.ProviderProfileBinding{ID: "binding", ProviderID: instance.ID, ProfileID: domain.ProfileVolcengineChat, AccessSurface: domain.SurfaceVolcengine, CredentialScheme: domain.CredentialBearerStatic, Enabled: true, Capabilities: domain.DefaultProviderCapabilities(domain.ProviderVolcengine)}
	adapter, err := newProviderBindingAdapterWithClient(instance, binding, endpoint, []byte("test-key"), client)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	manifest, ok := provider.BuiltinProfile(binding.ProfileID)
	if !ok {
		t.Fatal("missing Ark profile")
	}
	bridge, err := provider.NewLegacyAdapterBridge(adapter, manifest, nil)
	if err != nil {
		t.Fatal(err)
	}
	target := provider.Target{Adapter: bridge, ProfileID: binding.ProfileID, ProviderModel: "ep-reviewed", CanonicalModelRef: "doubao-seed-2-0-lite-260428"}
	operation, ok := bridge.Operations().Resolve(provider.OperationChat)
	if !ok {
		t.Fatal("missing chat primitive")
	}
	generation := operation.(provider.GenerationAdapter)
	request, err := openaiwire.DecodeGenerate(openaiapi.ChatCompletionRequest{Model: "public", ReasoningEffort: "high", Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := generation.Generate(context.Background(), provider.GenerateCall{Request: request, ProviderModel: target.ProviderModel, CanonicalModelRef: target.CapabilityModel()})
	if err != nil || !called {
		t.Fatalf("called=%v err=%v", called, err)
	}
	response, err := openaiwire.RenderGenerateResult(result)
	if err != nil || response.Choices[0].Message.EncryptedContent != "opaque" {
		t.Fatal("primitive lost opaque state")
	}
}
