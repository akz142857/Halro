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
	"github.com/akz142857/Halro/internal/semantic"
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

// The streaming path has its own request builder and its own error handling.
// An authentication failure there must be as terminal as on the non-stream
// path, and must not be marked ambiguous when nothing was emitted — an
// ambiguous failure is settled conservatively, as if the provider might have
// done the work.
func TestBedrockMantleChatStreamAuthenticationFailureIsTerminal(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"denied","type":"access_denied"}}`)),
			}, nil
		})}
		adapter := newMantleChatAdapter(t, client)
		emitted := 0
		_, err := adapter.ChatStream(context.Background(), provider.ChatCall{
			RequestID: "req_1", ProviderModel: "openai.gpt-test",
			Request: openaiapi.ChatCompletionRequest{Model: "route", Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hello")}}},
		}, func(semantic.Event) error {
			emitted++
			return nil
		})
		var classified *provider.Error
		if !errors.As(err, &classified) {
			t.Fatalf("status %d was not classified: %v", status, err)
		}
		if classified.Class != provider.ErrorAuthentication || classified.Retryable || classified.Ambiguous {
			t.Fatalf("status %d stream error: %#v", status, classified)
		}
		if emitted != 0 {
			t.Fatalf("status %d emitted %d events before failing", status, emitted)
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

// The OpenAI-shaped Mantle profiles select the same Bedrock Project resource
// with OpenAI-Project. An empty project sends no header, which is how the
// account default is addressed.
func TestBedrockMantleChatRendersTheProjectAsAnOpenAIProjectHeader(t *testing.T) {
	for _, test := range []struct{ projectID, expected string }{
		{"proj_abc123", "proj_abc123"},
		{"", ""},
	} {
		var seen *http.Request
		client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			seen = request
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"object":"list","data":[]}`)),
			}, nil
		})}
		endpoint, _ := url.Parse("https://bedrock-mantle.us-east-1.api.aws")
		authorizer, err := provider.NewStaticHeaderAuthorizer(domain.CredentialBedrockAPIKey, "Authorization", "Bearer ", []byte("bedrock-key"), "api-key", "x-api-key")
		if err != nil {
			t.Fatal(err)
		}
		adapter, err := NewWithOptions(Options{
			Endpoint: endpoint, Authorizer: authorizer, Client: client,
			ProviderType:     string(domain.ProviderBedrock),
			CredentialScheme: domain.CredentialBedrockAPIKey,
			Capabilities:     provider.Capabilities{Chat: true},
			BedrockProjectID: test.projectID,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := adapter.Probe(context.Background(), "openai.gpt-test"); err != nil {
			t.Fatalf("project %q: %v", test.projectID, err)
		}
		if got := seen.Header.Get("OpenAI-Project"); got != test.expected {
			t.Fatalf("project %q rendered OpenAI-Project=%q", test.projectID, got)
		}
		if seen.Header.Get("anthropic-workspace") != "" || seen.Header.Get("anthropic-workspace-id") != "" {
			t.Fatalf("project %q leaked a foreign resource header: %v", test.projectID, seen.Header)
		}
		if seen.Header.Get("Authorization") != "Bearer bedrock-key" {
			t.Fatalf("project %q disturbed the credential header: %v", test.projectID, seen.Header)
		}
		adapter.Close()
	}
}
