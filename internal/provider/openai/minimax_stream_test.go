package openai

import (
	"context"
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

// Transcribed from a real MiniMax stream, 2026-08-31. The shape that matters is
// the ending: a chunk carrying finish_reason, then a chunk with no choices and
// the usage totals, then the connection closes. There is no `data: [DONE]`
// anywhere, which is why this had to be captured rather than imagined.
const minimaxRealStream = `data: {"id":"06e4","choices":[{"index":0,"delta":{"content":"OK","role":"assistant"}}],"created":1788167855,"model":"MiniMax-M3","object":"chat.completion.chunk","usage":null,"service_tier":"standard"}

data: {"id":"06e4","choices":[{"finish_reason":"stop","index":0,"delta":{"role":"assistant"}}],"created":1788167855,"model":"MiniMax-M3","object":"chat.completion.chunk","usage":null,"service_tier":"standard"}

data: {"id":"06e4","choices":[],"created":1788167855,"model":"MiniMax-M3","object":"chat.completion.chunk","usage":{"total_tokens":167,"total_characters":0,"prompt_tokens":165,"completion_tokens":2},"service_tier":"standard","base_resp":{"status_code":0,"status_msg":""}}

`

func minimaxStreamAdapter(t *testing.T, body string) *Adapter {
	t.Helper()
	endpoint, _ := url.Parse("https://api.minimax.io")
	authorizer, err := provider.NewStaticHeaderAuthorizer(domain.CredentialBearerStatic, "Authorization", "Bearer ", []byte("k"), "x-api-key")
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream; charset=utf-8"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}
	adapter, err := NewWithOptions(Options{
		Endpoint: endpoint, Authorizer: authorizer, Client: client,
		ProviderType: string(domain.ProviderMiniMax), CredentialScheme: domain.CredentialBearerStatic,
		Capabilities: domain.ProviderCapabilities{Chat: true, Streaming: true, StreamUsage: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adapter.Close)
	return adapter
}

func minimaxStreamCall() provider.ChatCall {
	return provider.ChatCall{
		RequestID: "req", ProviderModel: "MiniMax-M3",
		Request: openaiapi.ChatCompletionRequest{
			Model: "MiniMax-M3", Stream: true,
			Messages: []openaiapi.Message{{Role: "user", Content: []byte(`"hi"`)}},
		},
	}
}

// The defect this closes: the caller paid for a generation the upstream ran and
// received malformed_response, because the stream loop requires a sentinel this
// upstream does not send.
func TestMiniMaxStreamEndingWithoutDoneIsComplete(t *testing.T) {
	var events int
	usage, err := minimaxStreamAdapter(t, minimaxRealStream).ChatStream(context.Background(), minimaxStreamCall(), func(semantic.Event) error {
		events++
		return nil
	})
	if err != nil {
		t.Fatalf("a complete MiniMax stream was reported as a failure: %v", err)
	}
	if events == 0 {
		t.Fatal("no events were emitted")
	}
	if usage == nil {
		t.Fatal("the final chunk's usage was not carried out; a streamed attempt would settle with nothing measured")
	}
	if usage.PromptTokens != 165 || usage.CompletionTokens != 2 {
		t.Fatalf("usage is prompt=%d completion=%d, want 165 and 2", usage.PromptTokens, usage.CompletionTokens)
	}
}

// The exemption must not swallow a real truncation. Cutting the stream before
// the model says it stopped is exactly what the sentinel check exists to catch,
// and it stays caught here.
func TestMiniMaxStreamTruncatedBeforeFinishReasonStillFails(t *testing.T) {
	truncated := minimaxRealStream[:strings.Index(minimaxRealStream, `"finish_reason"`)]
	_, err := minimaxStreamAdapter(t, truncated).ChatStream(context.Background(), minimaxStreamCall(), func(semantic.Event) error { return nil })
	if err == nil {
		t.Fatal("a stream that stopped before the model finished was accepted as complete")
	}
	providerErr, ok := err.(*provider.Error)
	if !ok || !providerErr.Ambiguous {
		t.Fatalf("a truncated stream produced %v; it has to stay ambiguous so the attempt is conservatively accounted", err)
	}
}

// The exemption is scoped to MiniMax. For every other OpenAI-family upstream a
// stream without the sentinel is a partial response, and reading it as complete
// would settle a truncated generation as a finished one.
func TestStreamWithoutDoneStillFailsForOtherProviders(t *testing.T) {
	endpoint, _ := url.Parse("https://api.openai.com")
	authorizer, err := provider.NewStaticHeaderAuthorizer(domain.CredentialBearerStatic, "Authorization", "Bearer ", []byte("k"), "api-key")
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(minimaxRealStream)),
		}, nil
	})}
	adapter, err := NewWithOptions(Options{
		Endpoint: endpoint, Authorizer: authorizer, Client: client,
		ProviderType: string(domain.ProviderOpenAI), CredentialScheme: domain.CredentialBearerStatic,
		Capabilities: domain.ProviderCapabilities{Chat: true, Streaming: true, StreamUsage: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	if _, err := adapter.ChatStream(context.Background(), minimaxStreamCall(), func(semantic.Event) error { return nil }); err == nil {
		t.Fatal("the MiniMax exemption leaked to another provider")
	}
}
