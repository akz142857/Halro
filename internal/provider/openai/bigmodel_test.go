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

func newBigModelTestAdapter(t *testing.T, endpoint string, transport roundTripFunc) *Adapter {
	t.Helper()
	baseURL, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	authorizer, err := provider.NewStaticHeaderAuthorizer(
		domain.CredentialBigModelAPIKey, "Authorization", "Bearer ", []byte("bigmodel-key"), "api-key", "x-api-key",
	)
	if err != nil {
		t.Fatal(err)
	}
	prefix := "api/paas/v4"
	adapter, err := NewWithOptions(Options{
		Endpoint: baseURL, Authorizer: authorizer, Client: &http.Client{Transport: transport},
		ProviderType: string(domain.ProviderBigModel), CredentialScheme: domain.CredentialBigModelAPIKey,
		Capabilities:        provider.Capabilities{Chat: true, Streaming: true, StreamUsage: true, Embeddings: true},
		OperationPathPrefix: prefix, CatalogPathPrefix: &prefix, DisableTargetDescribe: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adapter.Close)
	return adapter
}

func TestBigModelUsesTheRegionalGeneralAPIPathAndDialect(t *testing.T) {
	for _, endpoint := range []string{"https://open.bigmodel.cn", "https://api.z.ai"} {
		t.Run(endpoint, func(t *testing.T) {
			adapter := newBigModelTestAdapter(t, endpoint, func(request *http.Request) (*http.Response, error) {
				if got, want := request.URL.Path, "/api/paas/v4/chat/completions"; got != want {
					t.Fatalf("path=%q want=%q", got, want)
				}
				if got := request.Header.Get("Authorization"); got != "Bearer bigmodel-key" {
					t.Fatalf("authorization=%q", got)
				}
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(body, &fields); err != nil {
					t.Fatalf("request body: %v: %s", err, body)
				}
				for _, name := range []string{"request_id", "user_id", "max_tokens"} {
					if _, ok := fields[name]; !ok {
						t.Errorf("%s is absent from %s", name, body)
					}
				}
				for _, name := range []string{"user", "max_completion_tokens", "stream_options"} {
					if _, ok := fields[name]; ok {
						t.Errorf("%s leaked onto the BigModel wire: %s", name, body)
					}
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"chat_1","object":"chat.completion","created":1,"model":"glm-5.2","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)), Request: request}, nil
			})
			limit := int64(16)
			response, err := adapter.Chat(context.Background(), provider.ChatCall{
				RequestID: "req-123", ProviderModel: "glm-5.2",
				Request: openaiapi.ChatCompletionRequest{
					Model: "public", Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}},
					MaxCompletionTokens: &limit, User: "halro-user",
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(response.Choices) != 1 {
				t.Fatalf("choices=%d", len(response.Choices))
			}
		})
	}
}

func TestBigModelCatalogUsesItsSiblingPathAndDoesNotAdvertiseDescribe(t *testing.T) {
	adapter := newBigModelTestAdapter(t, "https://api.z.ai", func(request *http.Request) (*http.Response, error) {
		if got, want := request.URL.Path, "/api/paas/v4/models"; got != want {
			t.Fatalf("path=%q want=%q", got, want)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"object":"list","data":[{"id":"glm-5.3","owned_by":"zai"}]}`)), Request: request}, nil
	})
	discovery := adapter.InvocationTargetDiscovery()
	if !discovery.CanEnumerate || discovery.CanDescribe || !discovery.CanVerify {
		t.Fatalf("discovery=%#v", discovery)
	}
	targets, err := adapter.ListInvocationTargets(context.Background(), domain.TargetQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].TargetID != "glm-5.3" || targets[0].OwnedBy != "zai" {
		t.Fatalf("targets=%#v", targets)
	}
}

func TestBigModelStreamCarriesFinalUsageWithoutStreamOptions(t *testing.T) {
	const stream = "data: {\"id\":\"chat_1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"think\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chat_1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5,\"prompt_tokens_details\":{\"cached_tokens\":2}}}\n\n" +
		"data: [DONE]\n\n"
	adapter := newBigModelTestAdapter(t, "https://open.bigmodel.cn", func(request *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(request.Body)
		if strings.Contains(string(body), "stream_options") {
			t.Fatalf("stream_options leaked onto the BigModel wire: %s", body)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream)), Request: request}, nil
	})
	var events []semantic.Event
	usage, err := adapter.ChatStream(context.Background(), provider.ChatCall{
		RequestID: "stream-1", ProviderModel: "glm-5.3",
		Request: openaiapi.ChatCompletionRequest{Model: "public", Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}}},
	}, func(event semantic.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || usage == nil || usage.PromptTokens != 3 || usage.CompletionTokens != 2 || usage.CachedPromptTokens() != 2 {
		t.Fatalf("events=%d usage=%#v", len(events), usage)
	}
}

func TestBigModelJSONVisionAndToolFixturesUseTheSharedChatContract(t *testing.T) {
	call := 0
	adapter := newBigModelTestAdapter(t, "https://open.bigmodel.cn", func(request *http.Request) (*http.Response, error) {
		call++
		body, _ := io.ReadAll(request.Body)
		if call == 1 {
			if strings.Contains(string(body), `"detail"`) || !strings.Contains(string(body), `"response_format":{"type":"json_object"}`) {
				t.Fatalf("vision/json request was not rendered as BigModel documents it: %s", body)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"chat_1","object":"chat.completion","created":1,"model":"glm-4.6v","choices":[{"index":0,"message":{"role":"assistant","content":"{\"ok\":true}"},"finish_reason":"stop"}]}`)), Request: request}, nil
		}
		if !strings.Contains(string(body), `"tools"`) || !strings.Contains(string(body), `"tool_choice":"auto"`) {
			t.Fatalf("tool request was not carried: %s", body)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"chat_2","object":"chat.completion","created":1,"model":"glm-5.2","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},"finish_reason":"tool_calls"}]}`)), Request: request}, nil
	})
	jsonResponse, err := adapter.Chat(context.Background(), provider.ChatCall{RequestID: "json", ProviderModel: "glm-4.6v", Request: openaiapi.ChatCompletionRequest{
		Messages:       []openaiapi.Message{{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"inspect"},{"type":"image_url","image_url":{"url":"https://example.test/image.png","detail":"auto"}}]`)}},
		ResponseFormat: json.RawMessage(`{"type":"json_object"}`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	content, ok := openaiapi.DecodeTextContent(jsonResponse.Choices[0].Message.Content)
	if !ok || !json.Valid([]byte(content)) {
		t.Fatalf("JSON response=%q", content)
	}
	toolResponse, err := adapter.Chat(context.Background(), provider.ChatCall{RequestID: "tool", ProviderModel: "glm-5.2", Request: openaiapi.ChatCompletionRequest{
		Messages:   []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("use lookup")}},
		Tools:      []openaiapi.Tool{{Type: "function", Function: openaiapi.ToolFunction{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}},
		ToolChoice: json.RawMessage(`"auto"`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(toolResponse.Choices[0].Message.ToolCalls) != 1 || toolResponse.Choices[0].Message.ToolCalls[0].Function.Name != "lookup" {
		t.Fatalf("tool response=%#v", toolResponse)
	}
}

func TestBigModelStreamFinishErrorDoesNotSettleAsSuccess(t *testing.T) {
	const stream = "data: {\"id\":\"chat_1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"network_error\"}]}\n\n" +
		"data: [DONE]\n\n"
	adapter := newBigModelTestAdapter(t, "https://api.z.ai", func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream)), Request: request}, nil
	})
	_, err := adapter.ChatStream(context.Background(), provider.ChatCall{RequestID: "stream-error", ProviderModel: "glm-5.3", Request: openaiapi.ChatCompletionRequest{
		Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}},
	}}, func(semantic.Event) error { return nil })
	classified, ok := err.(*provider.Error)
	if !ok || classified.Class != provider.ErrorProvider5xx || !classified.Ambiguous {
		t.Fatalf("error=%#v", err)
	}
}

func TestBigModelFinishErrorsAreAmbiguousAndClassified(t *testing.T) {
	for finish, class := range map[string]provider.ErrorClass{
		"sensitive":                     provider.ErrorBadRequest,
		"network_error":                 provider.ErrorProvider5xx,
		"model_context_window_exceeded": provider.ErrorBadRequest,
	} {
		t.Run(finish, func(t *testing.T) {
			adapter := newBigModelTestAdapter(t, "https://api.z.ai", func(request *http.Request) (*http.Response, error) {
				body := `{"id":"chat_1","object":"chat.completion","created":1,"model":"glm-5.2","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"` + finish + `"}]}`
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
			})
			_, err := adapter.Chat(context.Background(), provider.ChatCall{RequestID: "req", ProviderModel: "glm-5.2", Request: openaiapi.ChatCompletionRequest{Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}}}})
			var classified *provider.Error
			if err == nil || !strings.Contains(err.Error(), "BigModel") || !asProviderError(err, &classified) || classified.Class != class || !classified.Ambiguous {
				t.Fatalf("error=%#v", err)
			}
		})
	}
}

func asProviderError(err error, target **provider.Error) bool {
	classified, ok := err.(*provider.Error)
	if ok {
		*target = classified
	}
	return ok
}
