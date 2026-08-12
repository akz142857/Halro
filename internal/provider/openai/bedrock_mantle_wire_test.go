package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
)

// The OpenAI-shaped profiles probe by reading one model's metadata. The
// Anthropic
// Messages profile probes with a one-token inference call instead. The two are
// not interchangeable for an operator deciding whether a connection test costs
// money, so both are pinned in their own package.
func TestBedrockMantleChatProbeReadsModelMetadata(t *testing.T) {
	var seen *http.Request
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		seen = request
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"object":"list","data":[{"id":"openai.gpt-test","object":"model"}]}`)),
		}, nil
	})}
	adapter := newMantleChatAdapter(t, client)

	if err := adapter.Probe(context.Background(), "openai.gpt-test"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if seen == nil || seen.Method != http.MethodGet || seen.URL.String() != "https://bedrock-mantle.us-east-1.api.aws/v1/models/openai.gpt-test" {
		t.Fatalf("probe was not a model metadata read: %+v", seen)
	}
	assertNoBedrockResourceHeaders(t, seen.Header)
}

// 401 and 403 are authentication failures, not availability failures. The
// gateway does not retry them and does not fall back to another deployment, so
// an expired or wrong-project Bedrock API key surfaces as one terminal error
// rather than silently draining budget on a standby route.
func TestBedrockMantleChatAuthenticationFailureIsNotRetryable(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		calls := 0
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"denied","type":"access_denied"}}`)),
			}, nil
		})}
		adapter := newMantleChatAdapter(t, client)

		_, err := adapter.Chat(context.Background(), provider.ChatCall{
			RequestID: "req_1", ProviderModel: "openai.gpt-test",
			Request: openaiapi.ChatCompletionRequest{Model: "route", Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hello")}}},
		})
		var classified *provider.Error
		if !errors.As(err, &classified) {
			t.Fatalf("status %d was not classified: %v", status, err)
		}
		if classified.Class != provider.ErrorAuthentication || classified.Retryable {
			t.Fatalf("status %d classified as %s retryable=%v", status, classified.Class, classified.Retryable)
		}
		if calls != 1 {
			t.Fatalf("status %d was attempted %d times", status, calls)
		}
	}
}

func newMantleChatAdapter(t *testing.T, client *http.Client) *Adapter {
	t.Helper()
	endpoint, _ := url.Parse("https://bedrock-mantle.us-east-1.api.aws")
	authorizer, err := provider.NewStaticHeaderAuthorizer(domain.CredentialBedrockAPIKey, "Authorization", "Bearer ", []byte("bedrock-key"), "api-key", "x-api-key")
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := NewWithOptions(Options{
		Endpoint: endpoint, Authorizer: authorizer, Client: client,
		ProviderType:     string(domain.ProviderBedrock),
		CredentialScheme: domain.CredentialBedrockAPIKey,
		Capabilities:     provider.Capabilities{Chat: true, Streaming: true, Tools: true, Vision: true, JSONMode: true, DeveloperRole: true, Reasoning: true, StreamUsage: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adapter.Close)
	return adapter
}

func assertNoBedrockResourceHeaders(t *testing.T, header http.Header) {
	t.Helper()
	for _, name := range []string{"OpenAI-Project", "OpenAI-Organization", "anthropic-workspace", "anthropic-workspace-id"} {
		if value := header.Get(name); value != "" {
			t.Fatalf("%s was sent while Halro only supports the account default project: %q", name, value)
		}
	}
}
