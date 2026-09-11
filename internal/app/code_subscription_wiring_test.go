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
