package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
)

// The MiniMax smoke under provider/openai builds the OpenAI adapter for every
// one of its subtests, so it never addresses /anthropic/v1/messages — the face
// MiniMax itself recommends, and the one a connection anchors on by default.
// The assertion it was supposed to make therefore had no test behind it.
//
// The claim at stake: MiniMax accepts the key as `Authorization: Bearer` on the
// Anthropic route. Two documentation sites say so and say it takes precedence
// over x-api-key; no request has confirmed it, and the adapter sends bearer on
// all three faces. If it is wrong, the credential scheme splits and the three
// profiles stop sharing one connection group.
func TestRealMiniMaxAnthropicRouteSmoke(t *testing.T) {
	if os.Getenv("HALRO_REAL_PROVIDER_SMOKE") != "1" || os.Getenv("HALRO_SMOKE_PROFILE") != "minimax_anthropic" {
		t.Skip("set HALRO_REAL_PROVIDER_SMOKE=1 and HALRO_SMOKE_PROFILE=minimax_anthropic to run the billable MiniMax Anthropic-route smoke")
	}
	apiKey := strings.TrimSpace(os.Getenv("HALRO_SMOKE_API_KEY"))
	if apiKey == "" {
		t.Skip("HALRO_SMOKE_API_KEY is required")
	}
	rawEndpoint := strings.TrimSpace(os.Getenv("HALRO_SMOKE_BASE_URL"))
	if rawEndpoint == "" {
		rawEndpoint = "https://api.minimax.io"
	}
	endpoint, err := url.Parse(rawEndpoint)
	if err != nil {
		t.Fatalf("HALRO_SMOKE_BASE_URL is not a URL: %v", err)
	}
	model := strings.TrimSpace(os.Getenv("HALRO_SMOKE_MODEL"))
	if model == "" {
		model = "MiniMax-M3"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	authorizer, err := provider.NewStaticHeaderAuthorizer(domain.CredentialBearerStatic, "Authorization", "Bearer ", []byte(apiKey), "x-api-key")
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := New(Options{
		Endpoint: endpoint, Authorizer: authorizer, Client: &http.Client{Timeout: 90 * time.Second},
		Capabilities: domain.ProviderCapabilities{Chat: true, Streaming: true, Tools: true, StreamUsage: true},
		ProviderType: string(domain.ProviderMiniMax), CredentialScheme: domain.CredentialBearerStatic,
		MessagesPath: "anthropic/v1/messages", ProfileID: domain.ProfileMiniMaxAnthropicMessages,
		CatalogShape: CatalogOpenAI,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()

	// Portable Chat through the Anthropic wire, which is how routing reaches this
	// profile for an OpenAI-shaped request.
	t.Run("bearer is accepted on the Anthropic route", func(t *testing.T) {
		response, err := adapter.Chat(ctx, provider.ChatCall{
			RequestID: "smoke_anthropic", ProviderModel: model,
			Request: openaiapi.ChatCompletionRequest{
				Model:     model,
				MaxTokens: func() *int64 { v := int64(16); return &v }(),
				Messages:  []openaiapi.Message{{Role: "user", Content: json.RawMessage(`"Reply with the single word: ok"`)}},
			},
		})
		if err != nil {
			providerErr, ok := err.(*provider.Error)
			if ok && providerErr.Class == provider.ErrorAuthentication {
				t.Fatalf("MiniMax refused the bearer token on /anthropic/v1/messages: %v\n"+
					"the credential scheme has to split and the three MiniMax profiles stop sharing one connection group", err)
			}
			t.Fatalf("Anthropic-route chat: %v", err)
		}
		if response.Usage == nil {
			t.Fatal("no usage on the Anthropic route; a settled attempt would carry no cost")
		}
		t.Logf("usage: prompt=%d completion=%d total=%d",
			response.Usage.PromptTokens, response.Usage.CompletionTokens, response.Usage.TotalTokens)
		// Anthropic reports both cache tiers; MiniMax documents no prompt caching,
		// so this records what it actually sends rather than asserting either way.
		var cacheRead int64
		if response.Usage.PromptTokensDetails != nil {
			cacheRead = response.Usage.PromptTokensDetails.CachedTokens
		}
		t.Logf("cache tiers: write=%d read=%d", response.Usage.CacheWriteTokens, cacheRead)
	})
}
