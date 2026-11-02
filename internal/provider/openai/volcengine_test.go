package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/semantic"
)

func newVolcengineTestAdapter(t *testing.T, handle roundTripFunc) *Adapter {
	t.Helper()
	endpoint, _ := url.Parse("https://ark.cn-beijing.volces.com/api/v3/")
	adapter, err := NewWithOptions(Options{
		Endpoint: endpoint, APIKey: []byte("ark-test-key"), Client: &http.Client{Transport: handle},
		ProviderType: string(domain.ProviderVolcengine), ProfileID: domain.ProfileVolcengineChat,
		Capabilities: domain.DefaultProviderCapabilities(domain.ProviderVolcengine), DisableTargetDescribe: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adapter.Close)
	return adapter
}

// Synthetic OpenAI-compatible responses exercise Halro's mapping, not an
// assertion that a live Ark account has returned these fixtures.
func TestVolcengineChatAndStreamUseArkPathAndDisableThinking(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "chat", true: "stream"}[streaming], func(t *testing.T) {
			adapter := newVolcengineTestAdapter(t, func(request *http.Request) (*http.Response, error) {
				if request.Method != http.MethodPost || request.URL.Path != "/api/v3/chat/completions" {
					t.Fatalf("unexpected route: %s %s", request.Method, request.URL)
				}
				if request.Header.Get("Authorization") != "Bearer ark-test-key" {
					t.Fatal("missing bearer credential")
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if string(body["model"]) != `"ep-20261102000000-test"` || string(body["thinking"]) != `{"type":"disabled"}` {
					t.Fatalf("incorrect target or reasoning state: %v", body)
				}
				if string(body["max_completion_tokens"]) != "128" || len(body["tools"]) == 0 {
					t.Fatalf("lost output limit or tools: %v", body)
				}
				if _, found := body["n"]; found {
					t.Fatal("n=1 must use the single-candidate default")
				}
				response := `{"id":"chat_1","object":"chat.completion","created":1,"model":"doubao-seed-2-1-pro-260915","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13,"prompt_tokens_details":{"cached_tokens":6}}}`
				contentType := "application/json"
				if streaming {
					if string(body["stream"]) != "true" || string(body["stream_options"]) != `{"include_usage":true}` {
						t.Fatalf("missing streaming usage request: %v", body)
					}
					contentType = "text/event-stream"
					response = "data: {\"id\":\"chat_1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"doubao-seed-2-1-pro-260915\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"weather\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
						"data: {\"id\":\"chat_1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"doubao-seed-2-1-pro-260915\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":3,\"total_tokens\":13,\"prompt_tokens_details\":{\"cached_tokens\":6}}}\n\ndata: [DONE]\n\n"
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(response))}, nil
			})
			limit, n := int64(128), 1
			call := provider.ChatCall{ProviderModel: "ep-20261102000000-test", Request: openaiapi.ChatCompletionRequest{
				Model: "public", N: &n, MaxCompletionTokens: &limit,
				Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("weather?")}},
				Tools:    []openaiapi.Tool{{Type: "function", Function: openaiapi.ToolFunction{Name: "weather", Parameters: json.RawMessage(`{"type":"object"}`)}}},
			}}
			if streaming {
				var toolSeen bool
				usage, err := adapter.ChatStream(context.Background(), call, func(event semantic.Event) error {
					for _, candidate := range event.Outputs {
						for _, part := range candidate.Content {
							toolSeen = toolSeen || part.Kind == semantic.ContentToolCall
						}
					}
					return nil
				})
				if err != nil || usage == nil || usage.PromptTokensDetails == nil || usage.PromptTokensDetails.CachedTokens != 6 || usage.TotalTokens != 13 || !toolSeen {
					t.Fatalf("usage=%+v tool=%t err=%v", usage, toolSeen, err)
				}
			} else {
				result, err := adapter.Chat(context.Background(), call)
				if err != nil || result.Model != "doubao-seed-2-1-pro-260915" || len(result.Choices) != 1 || len(result.Choices[0].Message.ToolCalls) != 1 || result.Usage == nil || result.Usage.TotalTokens != 13 {
					t.Fatalf("result=%+v err=%v", result, err)
				}
			}
		})
	}
}

func TestVolcengineDiscoveryAndProbeDoNotInventModelCapabilities(t *testing.T) {
	calls := 0
	adapter := newVolcengineTestAdapter(t, func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Method != "GET" || request.URL.Path != "/api/v3/models" {
			t.Fatalf("discovery must never infer a describe or inference route: %s %s", request.Method, request.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"object":"list","data":[{"id":"ep-custom","object":"model","owned_by":"test"}]}`))}, nil
	})
	discovery := adapter.InvocationTargetDiscovery()
	if !discovery.CanEnumerate || discovery.CanDescribe {
		t.Fatalf("discovery=%+v", discovery)
	}
	items, err := adapter.ListInvocationTargets(context.Background(), domain.TargetQuery{})
	if err != nil || len(items) != 1 || items[0].MetadataSource != domain.MetadataSourceNone || items[0].CanonicalModelRef != "ep-custom" {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	if err := adapter.Probe(context.Background(), "ep-custom"); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Probe(context.Background(), "invented"); err == nil {
		t.Fatal("an unlisted target passed its probe")
	}
	if calls != 3 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestVolcengineDiscoveryRefusesNonListsAndUpstreamErrors(t *testing.T) {
	for _, fixture := range []struct {
		body   string
		status int
		class  provider.ErrorClass
	}{
		{`{}`, 200, provider.ErrorMalformed},
		{`{"data":null}`, 200, provider.ErrorMalformed},
		{`{"message":"login required"}`, 200, provider.ErrorMalformed},
		{`{"error":{"code":"AuthenticationError","message":"bad key"}}`, 401, provider.ErrorAuthentication},
		{`{"error":{"code":"NotFound","message":"no models route"}}`, 404, provider.ErrorBadRequest},
	} {
		t.Run(fixture.body, func(t *testing.T) {
			adapter := newVolcengineTestAdapter(t, func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: fixture.status, Body: io.NopCloser(strings.NewReader(fixture.body))}, nil
			})
			err := adapter.Probe(context.Background(), "")
			var providerErr *provider.Error
			if !errors.As(err, &providerErr) || providerErr.Class != fixture.class {
				t.Fatalf("error=%v want class=%s", err, fixture.class)
			}
		})
	}
}

func TestVolcengineRealCatalogPreservesLifecycleWithoutInventingEntitlements(t *testing.T) {
	payload, err := os.ReadFile("testdata/volcengine-models.json")
	if err != nil {
		t.Fatal(err)
	}
	adapter := newVolcengineTestAdapter(t, func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(payload)))}, nil
	})
	items, err := adapter.ListInvocationTargets(context.Background(), domain.TargetQuery{})
	if err != nil || len(items) != 4 {
		t.Fatalf("items=%d err=%v", len(items), err)
	}
	byID := map[string]domain.InvocationTargetDescriptor{}
	for _, item := range items {
		byID[item.TargetID] = item
		if item.MetadataSource != domain.MetadataSourceProvider {
			t.Fatal("real structured metadata was lost")
		}
	}
	shutdown := byID["doubao-seed-1-6-251015"]
	if shutdown.Availability != domain.AvailabilityUnavailable || shutdown.Lifecycle != domain.TargetLifecycleDeprecated {
		t.Fatalf("shutdown=%+v", shutdown)
	}
	retiring := byID["doubao-embedding-large-text-250515"]
	if retiring.Availability != domain.AvailabilityUnverified || retiring.Lifecycle != domain.TargetLifecycleDeprecated {
		t.Fatalf("retiring=%+v", retiring)
	}
	for _, id := range []string{"doubao-seed-2-0-mini-260428", "doubao-seedance-2-5-260628"} {
		if item := byID[id]; item.Availability != domain.AvailabilityUnverified || item.Lifecycle != domain.TargetLifecycleUnknown {
			t.Fatalf("missing status treated as active entitlement: %+v", item)
		}
	}
	if err := adapter.Probe(context.Background(), shutdown.TargetID); err == nil {
		t.Fatal("shut-down target passed connection test")
	}
}
