package app

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/buildinfo"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
)

// A subscription key reaching the ordinary API path may still return 200 and
// consume a different balance. These assertions therefore bind the product to
// its exact path and authentication shape, rather than treating reachability as
// proof that the wiring is correct.
func TestCodeSubscriptionProfilesUseTheirOwnPathsAndAuthentication(t *testing.T) {
	for _, test := range []struct {
		profile domain.ProviderProfileID
		surface domain.AccessSurface
		scheme  domain.CredentialScheme
		host    string
		path    string
		header  string
		value   string
		kimiUA  bool
	}{
		{domain.ProfileKimiCodeOpenAIChat, domain.SurfaceKimiCode, domain.CredentialKimiCodeKey,
			"https://api.kimi.com", "/coding/v1/chat/completions", "Authorization", "Bearer subscription-key", true},
		{domain.ProfileKimiCodeAnthropicMessages, domain.SurfaceKimiCode, domain.CredentialKimiCodeKey,
			"https://api.kimi.com", "/coding/v1/messages", "Authorization", "Bearer subscription-key", true},
		{domain.ProfileMiniMaxCNSubscriptionOpenAIChat, domain.SurfaceMiniMaxCNSubscription, domain.CredentialMiniMaxSubscriptionKey,
			"https://api.minimax.cn", "/v1/chat/completions", "Authorization", "Bearer subscription-key", false},
		{domain.ProfileMiniMaxGlobalSubscriptionOpenAIChat, domain.SurfaceMiniMaxGlobalSubscription, domain.CredentialMiniMaxSubscriptionKey,
			"https://api.minimax.io", "/v1/chat/completions", "Authorization", "Bearer subscription-key", false},
		{domain.ProfileMiniMaxCNSubscriptionAnthropicMessages, domain.SurfaceMiniMaxCNSubscription, domain.CredentialMiniMaxSubscriptionKey,
			"https://api.minimax.cn", "/anthropic/v1/messages", "x-api-key", "subscription-key", false},
		{domain.ProfileMiniMaxGlobalSubscriptionAnthropicMessages, domain.SurfaceMiniMaxGlobalSubscription, domain.CredentialMiniMaxSubscriptionKey,
			"https://api.minimax.io", "/anthropic/v1/messages", "x-api-key", "subscription-key", false},
	} {
		t.Run(string(test.profile), func(t *testing.T) {
			endpoint, _ := url.Parse(test.host)
			providerType, _, ok := domain.RegisteredProviderProfile(test.profile)
			if !ok {
				t.Fatal("profile is not registered")
			}
			var seen *http.Request
			client := &http.Client{Transport: recordingTransport(func(request *http.Request) (*http.Response, error) {
				seen = request
				body := `{"id":"chat_1","object":"chat.completion","created":1,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
				if strings.HasSuffix(test.path, "/messages") {
					body = `{"id":"msg_1","type":"message","role":"assistant","model":"model","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			instance := domain.ProviderInstance{
				ID: "prov_1", Name: "subscription", Type: providerType,
				BaseURL: test.host, CredentialID: "cred_1", AccessSurface: test.surface,
				ProfileID: test.profile, CredentialScheme: test.scheme,
			}
			binding := domain.ProviderProfileBinding{
				ID: domain.DefaultProviderProfileBindingID(instance.ID, test.profile), ProviderID: instance.ID,
				ProfileID: test.profile, AccessSurface: test.surface, CredentialScheme: test.scheme,
				Enabled: true, Capabilities: domain.DefaultProviderCapabilitiesForProfile(instance.Type, test.profile),
			}
			adapter, err := newProviderBindingAdapterWithClient(instance, binding, endpoint, []byte("subscription-key"), client)
			if err != nil {
				t.Fatal(err)
			}
			defer adapter.Close()
			_, err = adapter.Chat(context.Background(), provider.ChatCall{
				RequestID: "req_1", ProviderModel: "model",
				Request: openaiapi.ChatCompletionRequest{Model: "model", Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if seen == nil || seen.URL.Scheme+"://"+seen.URL.Host != test.host || seen.URL.Path != test.path {
				t.Fatalf("request=%v", seen)
			}
			if got := seen.Header.Get(test.header); got != test.value {
				t.Fatalf("%s=%q", test.header, got)
			}
			if test.kimiUA && seen.Header.Get("User-Agent") != "Halro/"+buildinfo.Version {
				t.Fatalf("User-Agent=%q", seen.Header.Get("User-Agent"))
			}
		})
	}
}

// The renderer reaching the wire, not just existing. Kimi Code's Chat face was
// built on the plain OpenAI marshaller, which sent the request struct as
// written: no off switch, and temperature straight through to an upstream that
// pins it. This drives the adapter the gateway builds and reads the bytes.
func TestKimiCodeChatSendsTheOffSwitchAndDropsThePinnedMembers(t *testing.T) {
	endpoint, _ := url.Parse("https://api.kimi.com")
	providerType, _, ok := domain.RegisteredProviderProfile(domain.ProfileKimiCodeOpenAIChat)
	if !ok {
		t.Fatal("profile is not registered")
	}
	var sent []byte
	client := &http.Client{Transport: recordingTransport(func(request *http.Request) (*http.Response, error) {
		sent, _ = io.ReadAll(request.Body)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"id":"chat_1","object":"chat.completion","created":1,"model":"k3","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))}, nil
	})}
	instance := domain.ProviderInstance{
		ID: "prov_1", Name: "subscription", Type: providerType,
		BaseURL: "https://api.kimi.com", CredentialID: "cred_1", AccessSurface: domain.SurfaceKimiCode,
		ProfileID: domain.ProfileKimiCodeOpenAIChat, CredentialScheme: domain.CredentialKimiCodeKey,
	}
	binding := domain.ProviderProfileBinding{
		ID:         domain.DefaultProviderProfileBindingID(instance.ID, domain.ProfileKimiCodeOpenAIChat),
		ProviderID: instance.ID, ProfileID: domain.ProfileKimiCodeOpenAIChat,
		AccessSurface: domain.SurfaceKimiCode, CredentialScheme: domain.CredentialKimiCodeKey, Enabled: true,
		Capabilities: domain.DefaultProviderCapabilitiesForProfile(instance.Type, domain.ProfileKimiCodeOpenAIChat),
	}
	adapter, err := newProviderBindingAdapterWithClient(instance, binding, endpoint, []byte("subscription-key"), client)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	temperature := 0.5
	if _, err := adapter.Chat(context.Background(), provider.ChatCall{
		RequestID: "req_1", ProviderModel: "k3",
		Request: openaiapi.ChatCompletionRequest{
			Model: "k3", Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sent), `"reasoning_effort":"none"`) {
		t.Fatalf("a request that asked for nothing left without the off switch: %s", sent)
	}
	// Reached through the adapter rather than the renderer alone, because the
	// refusal has to happen before the bytes exist.
	if _, err := adapter.Chat(context.Background(), provider.ChatCall{
		RequestID: "req_2", ProviderModel: "k3",
		Request: openaiapi.ChatCompletionRequest{
			Model: "k3", Temperature: &temperature,
			Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}},
		},
	}); err == nil {
		t.Fatal("temperature reached an upstream that pins it")
	}
}
