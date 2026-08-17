package openai

import (
	"context"
	"encoding/json"
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

func newDeepSeekAdapter(t *testing.T, handle func(*http.Request) (*http.Response, error)) *Adapter {
	t.Helper()
	endpoint, err := url.Parse("https://api.deepseek.com")
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := NewWithOptions(Options{
		Endpoint: endpoint, APIKey: []byte("deepseek-key"),
		Client:       &http.Client{Transport: roundTripFunc(handle)},
		ProviderType: string(domain.ProviderDeepSeek),
		Capabilities: provider.Capabilities{Chat: true, Streaming: true, Reasoning: true, StreamUsage: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adapter.Close)
	return adapter
}

// DeepSeek shares this adapter because it shares the wire format, not the member
// list. Marshalling the OpenAI struct straight out put five members on the wire
// that DeepSeek accepts and ignores, and sent the end-user reference under a key
// it does not read — a 200 for a request nobody made.
func TestDeepSeekChatSendsOnlyTheMembersDeepSeekAccepts(t *testing.T) {
	var body map[string]json.RawMessage
	adapter := newDeepSeekAdapter(t, func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://api.deepseek.com/v1/chat/completions" {
			t.Fatalf("unexpected URL: %s", request.URL)
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"id":"chat_1","object":"chat.completion","created":1,"model":"deepseek-v4-flash",
				"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,
				"prompt_cache_hit_tokens":8,"prompt_cache_miss_tokens":2}}`)),
			Request: request,
		}, nil
	})
	temperature := 0.5
	_, err := adapter.Chat(context.Background(), provider.ChatCall{
		RequestID: "req_1", ProviderModel: "deepseek-v4-flash",
		Request: openaiapi.ChatCompletionRequest{
			Model: "public", Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}},
			Temperature: &temperature, User: "customer-7", ReasoningEffort: "high",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(body["user_id"]) != `"customer-7"` {
		t.Fatalf("user_id = %s", body["user_id"])
	}
	if string(body["thinking"]) != `{"type":"enabled","reasoning_effort":"high"}` {
		t.Fatalf("thinking = %s", body["thinking"])
	}
	if string(body["model"]) != `"deepseek-v4-flash"` || string(body["temperature"]) != "0.5" {
		t.Fatalf("unexpected body: %v", body)
	}
	for _, absent := range []string{"user", "reasoning_effort", "n", "seed", "parallel_tool_calls", "max_completion_tokens"} {
		if _, present := body[absent]; present {
			t.Fatalf("%s reached a surface that does not accept it: %v", absent, body)
		}
	}
}

// Streaming shares the shaping, and it is the path that also carries
// stream_options — one of the members DeepSeek does accept, so it has to survive
// the narrowing rather than be lost with the rest.
func TestDeepSeekChatStreamKeepsStreamOptionsAndDropsTheRest(t *testing.T) {
	var body map[string]json.RawMessage
	adapter := newDeepSeekAdapter(t, func(request *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(
				"data: {\"id\":\"chat_1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"deepseek-v4-flash\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n" +
					"data: {\"id\":\"chat_1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"deepseek-v4-flash\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15,\"prompt_cache_hit_tokens\":8,\"prompt_cache_miss_tokens\":2}}\n\n" +
					"data: [DONE]\n\n")),
			Request: request,
		}, nil
	})
	usage, err := adapter.ChatStream(context.Background(), provider.ChatCall{
		RequestID: "req_1", ProviderModel: "deepseek-v4-flash",
		Request: openaiapi.ChatCompletionRequest{
			Model: "public", Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}},
			User: "customer-7",
		},
	}, func(semantic.Event) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if string(body["stream"]) != "true" || string(body["stream_options"]) != `{"include_usage":true}` {
		t.Fatalf("stream members did not survive: %v", body)
	}
	if _, present := body["user"]; present {
		t.Fatalf("the OpenAI spelling reached DeepSeek: %v", body)
	}
	// The stream reports the cache split under DeepSeek's own two keys, which is
	// the tier the cache-read rate is charged against.
	if usage == nil || usage.CachedPromptTokens() != 8 || usage.PromptCacheMissTokens != 2 {
		t.Fatalf("stream usage lost the cache split: %#v", usage)
	}
}

// The DeepSeek cache split has to survive the response decode as well, or the
// hit span settles at the miss rate.
func TestDeepSeekChatDecodesThePromptCacheSplit(t *testing.T) {
	adapter := newDeepSeekAdapter(t, func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"id":"chat_1","object":"chat.completion","created":1,"model":"deepseek-v4-flash",
				"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,
				"prompt_cache_hit_tokens":8,"prompt_cache_miss_tokens":2}}`)),
			Request: request,
		}, nil
	})
	response, err := adapter.Chat(context.Background(), provider.ChatCall{
		RequestID: "req_1", ProviderModel: "deepseek-v4-flash",
		Request: openaiapi.ChatCompletionRequest{
			Model: "public", Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Usage == nil || response.Usage.CachedPromptTokens() != 8 || response.Usage.PromptCacheMissTokens != 2 {
		t.Fatalf("cache split was not decoded: %#v", response.Usage)
	}
}

// Capability detection sends its own chat probes through this adapter, and the
// output limit it bounds them with is max_completion_tokens — a member DeepSeek
// has no place for. Once the adapter stopped sending members DeepSeek does not
// accept, every DeepSeek probe would have failed inside the process without ever
// reaching the upstream, and a connection would have come back with nothing
// established.
func TestDeepSeekCapabilityProbesReachTheUpstream(t *testing.T) {
	var body map[string]json.RawMessage
	adapter := newDeepSeekAdapter(t, func(request *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"id":"chat_1","object":"chat.completion","created":1,"model":"deepseek-v4-flash",
				"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)),
			Request: request,
		}, nil
	})
	manifest, ok := provider.BuiltinProfile(domain.ProfileDeepSeekChat)
	if !ok {
		t.Fatal("the DeepSeek profile is not registered")
	}
	bridge, err := provider.NewLegacyAdapterBridge(adapter, manifest, domain.EvidenceForCapabilities(
		domain.DefaultProviderCapabilitiesForProfile(domain.ProviderDeepSeek, domain.ProfileDeepSeekChat), domain.EvidenceDeclared))
	if err != nil {
		t.Fatal(err)
	}
	target := provider.ModelCapabilityDetectionTarget{
		ProviderModel: "deepseek-v4-flash", BindingID: "binding", ProfileID: domain.ProfileDeepSeekChat, RiskTier: "safe_automatic",
	}
	plan, err := bridge.CapabilityDetectionPlan(target)
	if err != nil {
		t.Fatal(err)
	}
	var chatProbe provider.CapabilityProbe
	for _, probe := range plan.Probes {
		if probe.Kind == "minimal_chat" {
			chatProbe = probe
		}
	}
	if chatProbe.Kind == "" {
		t.Fatal("the DeepSeek plan has no minimal chat probe")
	}
	result := bridge.DetectCapability(context.Background(), target, chatProbe)
	if result.Status != domain.ProbeSupported {
		t.Fatalf("the chat probe did not establish chat: %#v", result)
	}
	if _, present := body["max_tokens"]; !present {
		t.Fatalf("the probe sent no output bound DeepSeek accepts: %v", body)
	}
	if _, present := body["max_completion_tokens"]; present {
		t.Fatalf("the probe sent a member DeepSeek does not accept: %v", body)
	}
}

// Every other OpenAI-wire provider keeps its own member list. Narrowing them to
// DeepSeek's would silently drop fields those upstreams do serve.
func TestOnlyDeepSeekGetsTheNarrowedBody(t *testing.T) {
	var body map[string]json.RawMessage
	endpoint, err := url.Parse("https://provider.example")
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := NewWithOptions(Options{
		Endpoint: endpoint, APIKey: []byte("key"), ProviderType: string(domain.ProviderOpenAI),
		Capabilities: provider.Capabilities{Chat: true},
		Client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"id":"chat_1","object":"chat.completion","created":1,"model":"gpt-test",
					"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)),
				Request: request,
			}, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	seed := int64(7)
	if _, err := adapter.Chat(context.Background(), provider.ChatCall{
		RequestID: "req_1", ProviderModel: "gpt-test",
		Request: openaiapi.ChatCompletionRequest{
			Model: "public", Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}},
			Seed: &seed, User: "customer-7", ReasoningEffort: "high",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if string(body["seed"]) != "7" || string(body["user"]) != `"customer-7"` || string(body["reasoning_effort"]) != `"high"` {
		t.Fatalf("OpenAI lost members it serves: %v", body)
	}
	if _, present := body["user_id"]; present {
		t.Fatalf("DeepSeek's spelling reached OpenAI: %v", body)
	}
}
