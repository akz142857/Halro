package openai

import (
	"context"
	"encoding/json"
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

func TestBedrockMantleChatUsesBearerAPIKeyAndOpenAIPath(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://bedrock-mantle.us-east-1.api.aws/v1/chat/completions" || request.Header.Get("Authorization") != "Bearer bedrock-key" || request.Header.Get("x-api-key") != "" {
			t.Fatalf("unexpected Mantle request: %s %#v", request.URL, request.Header)
		}
		// Halro addresses the account's default Bedrock project, so OpenAI-Project
		// is never sent. anthropic-workspace-id belongs to a different AWS service.
		assertNoBedrockResourceHeaders(t, request.Header)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"amazon.nova-pro","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)), Request: request}, nil
	})}
	endpoint, _ := url.Parse("https://bedrock-mantle.us-east-1.api.aws")
	authorizer, err := provider.NewStaticHeaderAuthorizer(domain.CredentialBedrockAPIKey, "Authorization", "Bearer ", []byte("bedrock-key"), "api-key", "x-api-key")
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := NewWithOptions(Options{Endpoint: endpoint, Authorizer: authorizer, Client: client, ProviderType: string(domain.ProviderBedrock), CredentialScheme: domain.CredentialBedrockAPIKey, Capabilities: provider.Capabilities{Chat: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	if adapter.Type() != string(domain.ProviderBedrock) {
		t.Fatalf("adapter type=%q", adapter.Type())
	}
	_, err = adapter.Chat(context.Background(), provider.ChatCall{RequestID: "req", ProviderModel: "amazon.nova-pro", Request: openaiapi.ChatCompletionRequest{Model: "public", Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}}}})
	if err != nil {
		t.Fatal(err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestChatTransformsPublicModelAndAuthorization(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://provider.example/v1/chat/completions" {
			t.Fatalf("unexpected URL: %s", request.URL)
		}
		if request.Header.Get("Authorization") != "Bearer provider-key" {
			t.Fatal("provider authorization was not set")
		}
		if request.Header.Get("X-Request-ID") != "req_1" {
			t.Fatal("request ID was not propagated")
		}
		var got openaiapi.ChatCompletionRequest
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got.Model != "gpt-test" || got.Stream {
			t.Fatalf("unexpected provider request: %#v", got)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
				"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"gpt-test",
				"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}
			}`)),
			Request: request,
		}, nil
	})}
	endpoint, _ := url.Parse("https://provider.example")
	adapter, err := New(endpoint, []byte("provider-key"), client)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()

	response, err := adapter.Chat(context.Background(), provider.ChatCall{
		RequestID:     "req_1",
		ProviderModel: "gpt-test",
		Request: openaiapi.ChatCompletionRequest{
			Model:    "chat",
			Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hello")}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.ID != "chatcmpl_1" || response.Usage == nil || response.Usage.TotalTokens != 3 {
		t.Fatalf("unexpected response: %#v", response)
	}
}

func TestAzureChatUsesDeploymentPathAPIKeyAndOmitsModel(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		want := "https://resource.openai.azure.com/openai/deployments/chat-prod/chat/completions?api-version=2025-01-01"
		if request.URL.String() != want {
			t.Fatalf("unexpected URL: %s", request.URL)
		}
		if request.Header.Get("api-key") != "azure-key" || request.Header.Get("Authorization") != "" {
			t.Fatalf("unexpected auth headers: %#v", request.Header)
		}
		var raw map[string]json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&raw); err != nil {
			t.Fatal(err)
		}
		if _, exists := raw["model"]; exists {
			t.Fatal("azure deployment request must omit model")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
				"id":"chatcmpl_azure","object":"chat.completion","created":1,"model":"deployment",
				"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]
			}`)),
			Request: request,
		}, nil
	})}
	endpoint, _ := url.Parse("https://resource.openai.azure.com")
	adapter, err := NewWithOptions(Options{
		Endpoint: endpoint, APIKey: []byte("azure-key"), Client: client,
		ProviderType: "azure_openai", APIVersion: "2025-01-01", Azure: true,
		Capabilities: provider.Capabilities{Chat: true, Streaming: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	if adapter.Type() != "azure_openai" {
		t.Fatalf("type=%q", adapter.Type())
	}
	_, err = adapter.Chat(context.Background(), provider.ChatCall{
		RequestID: "req_azure", ProviderModel: "chat-prod",
		Request: openaiapi.ChatCompletionRequest{
			Model: "public-chat",
			Messages: []openaiapi.Message{{
				Role: "user", Content: openaiapi.TextContent("hello"),
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCompatibleBaseV1IsNotDuplicatedAndOptionalStreamUsageIsOmitted(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://compatible.example/v1/chat/completions" {
			t.Fatalf("unexpected URL: %s", request.URL)
		}
		var raw map[string]json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&raw); err != nil {
			t.Fatal(err)
		}
		if _, exists := raw["stream_options"]; exists {
			t.Fatal("generic compatible profile must not force stream_options")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
			Request:    request,
		}, nil
	})}
	endpoint, _ := url.Parse("https://compatible.example/v1")
	adapter, err := NewWithOptions(Options{
		Endpoint: endpoint, APIKey: []byte("compatible-key"), Client: client,
		ProviderType: "openai_compatible",
		Capabilities: provider.Capabilities{Chat: true, Streaming: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	_, err = adapter.ChatStream(context.Background(), provider.ChatCall{
		RequestID: "req_compatible", ProviderModel: "local-model",
		Request: openaiapi.ChatCompletionRequest{
			Model: "chat", Stream: true,
			Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hello")}},
		},
	}, func(semantic.Event) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
}

func TestProfileOperationURLs(t *testing.T) {
	tests := []struct {
		name       string
		base       string
		model      string
		operation  string
		azure      bool
		apiVersion string
		prefix     string
		want       string
	}{
		{
			name: "deepseek base", base: "https://api.deepseek.com",
			model: "deepseek-v4-flash", operation: "chat/completions",
			want: "https://api.deepseek.com/v1/chat/completions",
		},
		{
			name: "compatible v1 base", base: "https://llm.internal/v1",
			model: "model", operation: "embeddings",
			want: "https://llm.internal/v1/embeddings",
		},
		{
			// A path-bearing base is literal: no /v1 is inserted, so endpoints
			// versioned as /v4 (Z.AI) or anything else are configurable.
			name: "compatible non-v1 versioned base", base: "https://api.z.ai/api/paas/v4",
			model: "glm-4-flash", operation: "chat/completions",
			want: "https://api.z.ai/api/paas/v4/chat/completions",
		},
		{
			name: "compatible path base wanting v1 spells it out", base: "https://llm.internal/openai/v1",
			model: "model", operation: "chat/completions",
			want: "https://llm.internal/openai/v1/chat/completions",
		},
		{
			// Bedrock Mantle serves each model from exactly one of two routes on
			// one host, so the route is fixed by the profile rather than joined
			// from the base. The base stays the bare origin because that is what
			// the credential is bound to.
			name: "bedrock mantle openai route", base: "https://bedrock-mantle.us-east-2.api.aws",
			model: "openai.gpt-5.6-sol", operation: "chat/completions", prefix: "openai/v1",
			want: "https://bedrock-mantle.us-east-2.api.aws/openai/v1/chat/completions",
		},
		{
			name: "bedrock mantle default route", base: "https://bedrock-mantle.us-east-2.api.aws",
			model: "qwen.qwen3-32b", operation: "chat/completions", prefix: "v1",
			want: "https://bedrock-mantle.us-east-2.api.aws/v1/chat/completions",
		},
		{
			name: "azure deployment", base: "https://resource.openai.azure.com",
			model: "embedding/prod", operation: "embeddings", azure: true,
			apiVersion: "2025-01-01",
			want:       "https://resource.openai.azure.com/openai/deployments/embedding%2Fprod/embeddings?api-version=2025-01-01",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			endpoint, _ := url.Parse(test.base)
			adapter := &Adapter{
				endpoint: endpoint, azure: test.azure, apiVersion: test.apiVersion,
				operationPathPrefix: test.prefix,
			}
			operationURL := adapter.operationURL(test.model, test.operation)
			got := operationURL.String()
			if got != test.want {
				t.Fatalf("URL=%q want=%q", got, test.want)
			}
		})
	}
}

func TestConnectionProbeUsesNonBillableEndpoint(t *testing.T) {
	wantURL := "https://provider.example/v1/models/org%2Fgpt-test"
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.String() != wantURL {
			t.Fatalf("unexpected probe: %s %s want=%s", request.Method, request.URL, wantURL)
		}
		if request.Header.Get("Authorization") != "Bearer provider-key" {
			t.Fatal("probe authorization was not set")
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header),
			Body:    io.NopCloser(strings.NewReader(`{"object":"list","data":[]}`)),
			Request: request,
		}, nil
	})}
	endpoint, _ := url.Parse("https://provider.example")
	adapter, err := New(endpoint, []byte("provider-key"), client)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	if err := adapter.Probe(context.Background(), "org/gpt-test"); err != nil {
		t.Fatal(err)
	}
	wantURL = "https://provider.example/v1/models"
	if err := adapter.Probe(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
}

func TestListInvocationTargetsUsesBoundCatalogEndpointAndParsesIDs(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.String() != "https://provider.example/v1/models" {
			t.Fatalf("unexpected catalog request: %s %s", request.Method, request.URL)
		}
		if request.Header.Get("Authorization") != "Bearer provider-key" {
			t.Fatal("catalog authorization was not set")
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header),
			Body:    io.NopCloser(strings.NewReader(`{"object":"list","data":[{"id":"gpt-b","owned_by":"openai"},{"id":" gpt-a ","owned_by":" owner "},{"id":""}]}`)),
			Request: request,
		}, nil
	})}
	endpoint, _ := url.Parse("https://provider.example")
	adapter, err := New(endpoint, []byte("provider-key"), client)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	models, err := adapter.ListInvocationTargets(context.Background(), domain.TargetQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].TargetID != "gpt-b" || models[1].TargetID != "gpt-a" || models[1].OwnedBy != "owner" {
		t.Fatalf("models=%#v", models)
	}
}

func TestCompatibleInvocationTargetDoesNotInferCanonicalModelFromSameName(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{
				"data":[{"id":"gpt-5","owned_by":"system","capabilities":{"vision":true}}]
			}`)), Request: request,
		}, nil
	})}
	endpoint, _ := url.Parse("https://compatible.example")
	adapter, err := NewWithOptions(Options{
		Endpoint: endpoint, APIKey: []byte("provider-key"), Client: client,
		ProviderType: string(domain.ProviderOpenAICompatible),
		Capabilities: provider.Capabilities{Chat: true, Streaming: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()

	targets, err := adapter.ListInvocationTargets(context.Background(), domain.TargetQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].TargetKind != domain.TargetCustomEndpointModel {
		t.Fatalf("targets=%#v", targets)
	}
	if targets[0].CanonicalModelRef != "" || targets[0].MetadataSource != domain.MetadataSourceNone || len(targets[0].Metadata.SupportedOperations) != 0 {
		t.Fatalf("compatible identity inferred canonical capabilities: %#v", targets[0])
	}
}

func TestHTTPErrorClassification(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"busy"}}`)),
			Header:     make(http.Header),
			Request:    request,
		}, nil
	})}
	endpoint, _ := url.Parse("https://provider.example")
	adapter, err := New(endpoint, []byte("provider-key"), client)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	_, err = adapter.Chat(context.Background(), provider.ChatCall{
		RequestID:     "req_1",
		ProviderModel: "gpt-test",
		Request: openaiapi.ChatCompletionRequest{
			Model:    "chat",
			Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hello")}},
		},
	})
	var providerError *provider.Error
	if !errors.As(err, &providerError) || providerError.Class != provider.ErrorRateLimit || !providerError.Retryable {
		t.Fatalf("unexpected error: %#v", err)
	}
}

func TestProviderAuthorizationCannotBeOverriddenByExistingHeaders(t *testing.T) {
	endpoint, _ := url.Parse("https://provider.example")
	openAI, err := New(endpoint, []byte("provider-key"), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer openAI.Close()
	request, _ := http.NewRequest(http.MethodPost, endpoint.String(), nil)
	request.Header.Set("Authorization", "Bearer client-controlled")
	request.Header.Set("api-key", "client-controlled")
	openAI.prepareRequest(request)
	if request.Header.Get("Authorization") != "Bearer provider-key" || request.Header.Get("api-key") != "" {
		t.Fatalf("OpenAI provider credentials were not authoritative: %#v", request.Header)
	}

	azure, err := NewWithOptions(Options{
		Endpoint: endpoint, APIKey: []byte("azure-key"), Client: http.DefaultClient,
		ProviderType: "azure_openai", APIVersion: "2025-01-01", Azure: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer azure.Close()
	request.Header.Set("Authorization", "Bearer client-controlled")
	request.Header.Set("api-key", "client-controlled")
	azure.prepareRequest(request)
	if request.Header.Get("api-key") != "azure-key" || request.Header.Get("Authorization") != "" {
		t.Fatalf("Azure provider credentials were not authoritative: %#v", request.Header)
	}
}

func TestFakeProviderHTTPErrorMatrix(t *testing.T) {
	tests := []struct {
		status    int
		class     provider.ErrorClass
		retryable bool
	}{
		{http.StatusBadRequest, provider.ErrorBadRequest, false},
		{http.StatusUnauthorized, provider.ErrorAuthentication, false},
		{http.StatusForbidden, provider.ErrorAuthentication, false},
		{http.StatusRequestTimeout, provider.ErrorTimeout, true},
		{http.StatusTooManyRequests, provider.ErrorRateLimit, true},
		{http.StatusInternalServerError, provider.ErrorProvider5xx, true},
		{http.StatusBadGateway, provider.ErrorProvider5xx, true},
		{http.StatusServiceUnavailable, provider.ErrorProvider5xx, true},
	}
	for _, test := range tests {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			got := classifyHTTPError(test.status, upstreamRefusal{Message: "synthetic provider failure"})
			if got.Class != test.class || got.Retryable != test.retryable || got.StatusCode != test.status {
				t.Fatalf("status=%d error=%#v", test.status, got)
			}
		})
	}
}

func TestChatPropagatesCancellation(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	endpoint, _ := url.Parse("https://provider.example")
	adapter, err := New(endpoint, []byte("provider-key"), client)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = adapter.Chat(ctx, provider.ChatCall{
		RequestID:     "req_1",
		ProviderModel: "gpt-test",
		Request: openaiapi.ChatCompletionRequest{
			Model:    "chat",
			Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hello")}},
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
	// A cancel is the caller abandoning the request, not a connection failure.
	// Classified as "connect" it read as an upstream network problem and
	// counted against the deployment's availability breaker; it stays
	// ambiguous because the upstream may have served the request anyway.
	var classified *provider.Error
	if !errors.As(err, &classified) {
		t.Fatalf("expected *provider.Error, got %T", err)
	}
	if classified.Class != provider.ErrorCanceled {
		t.Fatalf("class=%q, want %q", classified.Class, provider.ErrorCanceled)
	}
	if classified.Retryable {
		t.Fatal("a canceled request must not be marked retryable")
	}
	if !classified.Ambiguous {
		t.Fatal("a cancel after the request was sent must stay ambiguous")
	}
}

func TestEmbeddingsMapsModelAndDecodesUsage(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://provider.example/v1/embeddings" {
			t.Fatalf("unexpected URL: %s", request.URL)
		}
		var got openaiapi.EmbeddingRequest
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got.Model != "text-embedding-provider" {
			t.Fatalf("model=%q", got.Model)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
				"object":"list","model":"text-embedding-provider",
				"data":[{"object":"embedding","embedding":[0.1,0.2],"index":0}],
				"usage":{"prompt_tokens":3,"completion_tokens":0,"total_tokens":3}
			}`)),
			Request: request,
		}, nil
	})}
	endpoint, _ := url.Parse("https://provider.example")
	adapter, err := New(endpoint, []byte("provider-key"), client)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	response, err := adapter.Embed(context.Background(), provider.EmbeddingCall{
		RequestID:     "req_1",
		ProviderModel: "text-embedding-provider",
		Request: openaiapi.EmbeddingRequest{
			Model: "embedding",
			Input: json.RawMessage(`["hello"]`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Usage == nil || response.Usage.PromptTokens != 3 || len(response.Data) != 1 {
		t.Fatalf("unexpected response: %#v", response)
	}
}

func TestChatStreamParsesSemanticChunksAndUsage(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Accept") != "text/event-stream" {
			t.Fatal("stream accept header was not set")
		}
		var got openaiapi.ChatCompletionRequest
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if !got.Stream || got.StreamOptions == nil || !got.StreamOptions.IncludeUsage {
			t.Fatalf("usage stream options were not forced: %#v", got)
		}
		body := strings.Join([]string{
			`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-test","choices":[{"index":0,"delta":{"role":"assistant","content":"hel"},"finish_reason":null}]}`,
			"",
			`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-test","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
			"",
			`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-test","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`,
			"",
			"data: [DONE]",
			"",
		}, "\n")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream; charset=utf-8"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})}
	endpoint, _ := url.Parse("https://provider.example")
	adapter, err := New(endpoint, []byte("provider-key"), client)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	var chunks []semantic.Event
	usage, err := adapter.ChatStream(context.Background(), provider.ChatCall{
		RequestID:     "req_1",
		ProviderModel: "gpt-test",
		Request: openaiapi.ChatCompletionRequest{
			Model:    "chat",
			Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hello")}},
			Stream:   true,
		},
	}, func(chunk semantic.Event) error {
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 3 || usage == nil || usage.TotalTokens != 3 {
		t.Fatalf("chunks=%d usage=%#v", len(chunks), usage)
	}
}

func TestChatStreamRequiresDoneSentinel(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(
				"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n",
			)),
			Request: request,
		}, nil
	})}
	endpoint, _ := url.Parse("https://provider.example")
	adapter, _ := New(endpoint, []byte("provider-key"), client)
	defer adapter.Close()
	_, err := adapter.ChatStream(context.Background(), provider.ChatCall{
		RequestID:     "req_1",
		ProviderModel: "gpt-test",
		Request: openaiapi.ChatCompletionRequest{
			Model: "chat", Stream: true,
			Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hello")}},
		},
	}, func(semantic.Event) error { return nil })
	var classified *provider.Error
	if !errors.As(err, &classified) || !classified.Ambiguous {
		t.Fatalf("unexpected error: %#v", err)
	}
}
